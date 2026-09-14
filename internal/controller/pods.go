/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	poc "github.com/llm-d-extensions/gpu-lease/api/v1alpha1"
)

// This file holds the WarmPool reconciler's pod-selection and pod-classification
// helpers: resolving a Deployment's pods, deciding which of them count toward R
// (design.md §4.6), reading/writing the gpu-lease.llm-d.ai/state label, and ordering
// promotion/demotion candidates. It intentionally contains no cluster-mutating logic
// beyond simple label/annotation patches on pods the WarmPool reconciler itself owns;
// it never touches the pod REST API or Kueue.

// deploymentSelector returns the label selector matching dep's pods.
func deploymentSelector(dep *appsv1.Deployment) (labels.Selector, error) {
	if dep.Spec.Selector == nil {
		return nil, fmt.Errorf("deployment %s/%s has no spec.selector", dep.Namespace, dep.Name)
	}
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("deployment %s/%s: invalid spec.selector: %w", dep.Namespace, dep.Name, err)
	}
	return sel, nil
}

// listDeploymentPods returns every pod in dep's namespace matching its selector. This is
// a live list (not dep.Status.ReadyReplicas), so it always reflects the pods that
// actually exist right now, which the promotion/demotion decision depends on.
func listDeploymentPods(ctx context.Context, c client.Client, dep *appsv1.Deployment) ([]corev1.Pod, error) {
	sel, err := deploymentSelector(dep)
	if err != nil {
		return nil, err
	}
	var list corev1.PodList
	if err := c.List(ctx, &list, client.InNamespace(dep.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, fmt.Errorf("list pods for deployment %s/%s: %w", dep.Namespace, dep.Name, err)
	}
	return list.Items, nil
}

// isPodReady reports whether pod's PodReady condition is True.
func isPodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// isPodConsidered reports whether pod counts toward R, the "ready pods of the
// Deployment" in design.md §4.6: Running, Ready, scheduled to a node, and not
// terminating. This is deliberately the minimum defensible filter (task instructions,
// design.md §4.6): it does not additionally verify the pod belongs to the Deployment's
// current ReplicaSet generation, since this PoC's Deployments are not exercised through
// rolling updates.
func isPodConsidered(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	if pod.Spec.NodeName == "" {
		return false
	}
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	return isPodReady(pod)
}

// podState returns pod's gpu-lease.llm-d.ai/state label, defaulting to StateWarm when
// absent or empty (design.md §4.4: "a pod with no state label yet is treated as warm").
func podState(pod *corev1.Pod) string {
	if pod.Labels == nil {
		return poc.StateWarm
	}
	if s, ok := pod.Labels[poc.StateLabelKey]; ok && s != "" {
		return s
	}
	return poc.StateWarm
}

// isHeld reports whether state counts toward "held" (hot capacity) in the targetHot
// math (design.md §4.6): hot and activating pods are held; warm and releasing are not.
// A pod mid-warmup (activating) must count as held, or the reconciler would over-promote
// while a warmup is already in flight.
func isHeld(state string) bool {
	return state == poc.StateHot || state == poc.StateActivating
}

// targetHot implements the core reconcile-algorithm math of design.md §4.6:
//
//	targetHot = clamp(len(R) - desiredWarm, minHot, len(R))
//
// The clamp is applied floor-then-ceiling (minHot first, then capped at readyCount) so
// that minHot can never push the target above the number of pods that actually exist
// (e.g. readyCount == 0 always yields targetHot == 0, even if minHot > 0).
func targetHot(readyCount int, desiredWarm, minHot int32) int {
	t := readyCount - int(desiredWarm)
	if t < int(minHot) {
		t = int(minHot)
	}
	if t > readyCount {
		t = readyCount
	}
	return t
}

// sortPromotionCandidates returns the subset of pods sitting on a node with at least one
// free GPU (per freeByNode, e.g. inventory.Inventory.NodeFreeCounts), ordered by
// (node free count desc, pod age asc) as specified in design.md §4.6. "Age asc" ranks the
// longest-warm (earliest-created) pod first, for fair, deterministic promotion order.
func sortPromotionCandidates(pods []corev1.Pod, freeByNode map[string]int32) []corev1.Pod {
	eligible := make([]corev1.Pod, 0, len(pods))
	for _, p := range pods {
		if freeByNode[p.Spec.NodeName] > 0 {
			eligible = append(eligible, p)
		}
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		fi, fj := freeByNode[eligible[i].Spec.NodeName], freeByNode[eligible[j].Spec.NodeName]
		if fi != fj {
			return fi > fj
		}
		return eligible[i].CreationTimestamp.Time.Before(eligible[j].CreationTimestamp.Time)
	})
	return eligible
}

// sortDemotionCandidates returns hot pods ordered newest-first (LIFO), the least
// disruptive demotion order per design.md §4.6.
func sortDemotionCandidates(pods []corev1.Pod) []corev1.Pod {
	out := make([]corev1.Pod, len(pods))
	copy(out, pods)
	sort.SliceStable(out, func(i, j int) bool {
		return out[j].CreationTimestamp.Time.Before(out[i].CreationTimestamp.Time)
	})
	return out
}

// patchPodStateLabel patches only pod's gpu-lease.llm-d.ai/state label to state, via a
// merge patch, so a concurrent patch by the GPULease reconciler to other
// controller-owned keys (lease/gpu-id/gpu-model annotations, pod-deletion-cost=100 at
// Bound) is never clobbered.
func patchPodStateLabel(ctx context.Context, c client.Client, pod *corev1.Pod, state string) error {
	orig := pod.DeepCopy()
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[poc.StateLabelKey] = state
	if err := c.Patch(ctx, pod, client.MergeFrom(orig)); err != nil {
		return fmt.Errorf("patch pod %s/%s state=%s: %w", pod.Namespace, pod.Name, state, err)
	}
	return nil
}

// initializePodWarmState patches a pod born without the state label to
// gpu-lease.llm-d.ai/state=warm and controller.kubernetes.io/pod-deletion-cost=-100
// (design.md §4.4). Only these two keys are touched, via a merge patch.
func initializePodWarmState(ctx context.Context, c client.Client, pod *corev1.Pod) error {
	orig := pod.DeepCopy()
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[poc.StateLabelKey] = poc.StateWarm
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[poc.AnnotationPodDeletionCost] = poc.PodDeletionCostWarm
	if err := c.Patch(ctx, pod, client.MergeFrom(orig)); err != nil {
		return fmt.Errorf("initialize pod %s/%s warm state: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}

// leaseOrphanReason returns a non-empty GPULease failure reason (design.md §4.2) if
// lease's claimed pod no longer justifies the lease continuing to exist, or "" if the
// claim is still valid. pod is nil when the claimed pod could not be found at all. sel is
// the owning Deployment's pod selector; pass nil to skip the "still selected" check.
func leaseOrphanReason(pod *corev1.Pod, lease *poc.GPULease, sel labels.Selector) string {
	if pod == nil {
		return poc.ReasonPodGone
	}
	if lease.Spec.ClaimRef.UID != "" && pod.UID != lease.Spec.ClaimRef.UID {
		// The name was reused by a different pod; the original claimant is gone.
		return poc.ReasonPodGone
	}
	if pod.DeletionTimestamp != nil || !isPodReady(pod) {
		return poc.ReasonPodGone
	}
	if sel != nil && !sel.Matches(labels.Set(pod.Labels)) {
		return poc.ReasonPodGone
	}
	if pod.Spec.NodeName != lease.Spec.NodeName {
		return poc.ReasonNodeMismatch
	}
	return ""
}
