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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	poc "github.com/llm-d-extensions/gpu-lease/api/v1alpha1"
	kueuev1beta2 "github.com/llm-d-extensions/gpu-lease/internal/kueue/v1beta2"
)

// This file holds the GPULease reconciler's Kueue Workload construction/inspection
// helpers (design.md §4.1, §4.7, §4.7.1; docs/kueue-spike.md). It never talks to a pod
// and never mutates a GPULease directly; gpulease_controller.go owns the state machine
// that calls into these helpers.

// placeholderContainerImage is the image used in every Workload's single podSet
// container. No pod is ever created from a standalone Workload, so any image reference
// is safe; this is the exact image docs/kueue-spike.md validated works against the live
// cluster (gotcha #7).
const placeholderContainerImage = "registry.k8s.io/pause:3.9"

// placeholderContainerName is the container name used in every Workload's podSet
// template. Arbitrary, but held constant so two Workloads built for the same lease are
// always identical (workloadMatchesSpec relies on this).
const placeholderContainerName = "main"

// kueueReasonInadmissible is Kueue's own QuotaReserved condition reason for a Workload
// whose spec.queueName names a LocalQueue that does not exist (docs/kueue-spike.md,
// design.md §4.7.1: message "LocalQueue <name> doesn't exist"). This is the ONE place
// this controller reads Kueue's own reason/message text for control flow -- design.md
// §4.7.1 explicitly calls this out as "a distinct, non-retryable-by-waiting error" that
// must not wait out admission-timeout. Every other Kueue reason/message is diagnostic
// only (see docs/kueue-spike.md gotcha #6).
const kueueReasonInadmissible = "Inadmissible"

// workloadName returns the name of the Kueue Workload backing lease. It is always
// lease.Name: a GPULease and its backing Workload are in a strict 1:1 relationship, and
// lease.Name (poc.LeaseName) is already a unique, deterministic, DNS-1123-safe string.
func workloadName(lease *poc.GPULease) string {
	return lease.Name
}

// buildWorkload returns the standalone Workload this PoC creates per GPULease
// (design.md §4.1): a single podSet requesting 1 unit of poc.llm-d.ai/gpu, nodeSelector
// pinned to the lease's node (so exactly one ResourceFlavor is eligible), submitted to
// the LocalQueue named by the GPULease's own spec (resolved once, at lease-creation time,
// by the WarmPool reconciler -- see api/v1alpha1.GPULeaseSpec.LocalQueueName). No pod is
// ever created from this template; restartPolicy Never + the pause image are cosmetic.
//
// buildWorkload is deterministic: calling it twice for the same lease.Spec always
// produces byte-for-byte the same Spec, which is what lets workloadMatchesSpec detect a
// stale Workload (e.g. after a lease's node changed, which cannot actually happen since
// GPULease is immutable-by-convention, but also after a controller bug) and is what
// makes buildWorkload safe to call on every reconcile of the Pending phase.
func buildWorkload(lease *poc.GPULease) *kueuev1beta2.Workload {
	return &kueuev1beta2.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workloadName(lease),
			Namespace: lease.Spec.ClaimRef.Namespace,
		},
		Spec: kueuev1beta2.WorkloadSpec{
			QueueName: lease.Spec.LocalQueueName,
			PodSets: []kueuev1beta2.PodSet{
				{
					Name:  kueuev1beta2.PodSetName,
					Count: 1,
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							NodeSelector: map[string]string{
								corev1.LabelHostname: lease.Spec.NodeName,
							},
							Containers: []corev1.Container{
								{
									Name:  placeholderContainerName,
									Image: placeholderContainerImage,
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceName(poc.KueueResourceName): resource.MustParse("1"),
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// workloadMatchesSpec reports whether existing's spec already matches want's, i.e.
// whether the Workload can be left alone. Both spec.podSets (as a whole) and
// spec.queueName are immutable in real Kueue once set (docs/kueue-spike.md gotchas #1/#2:
// even a no-op patch to podSets.count or nodeSelector is rejected with "field is
// immutable"), so any mismatch here means the existing Workload must be deleted and
// recreated -- there is no patch path. equality.Semantic.DeepEqual (rather than
// reflect.DeepEqual) is used because it compares resource.Quantity by value (Cmp), not by
// its private internal representation.
func workloadMatchesSpec(existing, want *kueuev1beta2.Workload) bool {
	return equality.Semantic.DeepEqual(existing.Spec, want.Spec)
}

// isWorkloadAdmitted reports whether wl's Admitted condition is True (design.md §4.7:
// "wait for Workload condition Admitted=True"; §4.7.1 confirms Admitted=True always
// accompanies QuotaReserved=True on the real cluster).
func isWorkloadAdmitted(wl *kueuev1beta2.Workload) bool {
	c := meta.FindStatusCondition(wl.Status.Conditions, kueuev1beta2.ConditionAdmitted)
	return c != nil && c.Status == metav1.ConditionTrue
}

// workloadInadmissible reports whether wl's QuotaReserved condition carries Kueue's own
// Inadmissible reason (a misconfigured LocalQueue; design.md §4.7.1), and if so returns
// Kueue's message for use in the GPULease's own status.message (diagnostics only).
func workloadInadmissible(wl *kueuev1beta2.Workload) (bool, string) {
	c := meta.FindStatusCondition(wl.Status.Conditions, kueuev1beta2.ConditionQuotaReserved)
	if c != nil && c.Status == metav1.ConditionFalse && c.Reason == kueueReasonInadmissible {
		return true, c.Message
	}
	return false, ""
}

// quotaReservedMessage returns wl's QuotaReserved condition message, for a Failed
// GPULease's diagnostic status.message, or a generic placeholder if Kueue has not yet
// observed the Workload at all.
func quotaReservedMessage(wl *kueuev1beta2.Workload) string {
	if c := meta.FindStatusCondition(wl.Status.Conditions, kueuev1beta2.ConditionQuotaReserved); c != nil {
		return c.Message
	}
	return "Workload not yet observed by Kueue"
}

// admissionAnchor returns the timestamp the GPULease reconciler measures
// admission-timeout against: the QuotaReserved condition's own LastTransitionTime (which
// meta.SetStatusCondition -- and Kueue's own controller -- only bump when .Status flips),
// or the Workload's creation time if Kueue has not yet reported any condition at all.
// Using the condition's transition time (rather than e.g. lease creation time) means a
// brief QuotaReserved=True -> False flap (quota reclaimed then re-exhausted) restarts the
// timeout window, matching "still trying" rather than "still stuck since the beginning".
func admissionAnchor(wl *kueuev1beta2.Workload) time.Time {
	if c := meta.FindStatusCondition(wl.Status.Conditions, kueuev1beta2.ConditionQuotaReserved); c != nil {
		return c.LastTransitionTime.Time
	}
	return wl.CreationTimestamp.Time
}

// assignedFlavor returns the ResourceFlavor name Kueue assigned to the lease's single
// podSet for the poc.llm-d.ai/gpu resource, read from
// status.admission.podSetAssignments[0].flavors (design.md §4.7.1's confirmed path). ok
// is false if the Workload has no admission recorded yet.
func assignedFlavor(wl *kueuev1beta2.Workload) (flavor string, ok bool) {
	if wl.Status.Admission == nil || len(wl.Status.Admission.PodSetAssignments) == 0 {
		return "", false
	}
	flavor, ok = wl.Status.Admission.PodSetAssignments[0].Flavors[corev1.ResourceName(poc.KueueResourceName)]
	return flavor, ok
}

// expectedFlavorForNode looks up which ResourceFlavor's nodeLabels select node (matching
// on kubernetes.io/hostname, the key deploy/kueue/resource-flavors.yaml uses for every
// flavor). It returns an error if no ResourceFlavor is found, or if the ResourceFlavor
// API is unavailable (missing CRD/RBAC); callers must treat both as "skip the check",
// per this repo's decision that the cross-check is best-effort/non-fatal (design.md §4.1
// guarantees nodeSelector pinning makes exactly one flavor eligible, so a mismatch here
// "should never happen").
func expectedFlavorForNode(ctx context.Context, c client.Client, node string) (string, error) {
	var list kueuev1beta2.ResourceFlavorList
	if err := c.List(ctx, &list); err != nil {
		return "", fmt.Errorf("list ResourceFlavors: %w", err)
	}
	for _, rf := range list.Items {
		if rf.Spec.NodeLabels[corev1.LabelHostname] == node {
			return rf.Name, nil
		}
	}
	return "", fmt.Errorf("no ResourceFlavor selects node %s", node)
}
