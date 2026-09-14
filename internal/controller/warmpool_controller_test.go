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
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pocv1alpha1 "github.com/llm-d-extensions/gpu-lease/api/v1alpha1"
	"github.com/llm-d-extensions/gpu-lease/internal/controller/inventory"
)

// -----------------------------------------------------------------------------
// Plain table-driven test for the pure targetHot math (design.md §4.6):
//
//	targetHot = clamp(readyCount - desiredWarm, minHot, readyCount)
//
// This needs no envtest and runs under `go test` on its own.
// -----------------------------------------------------------------------------

func TestTargetHot(t *testing.T) {
	cases := []struct {
		name        string
		readyCount  int
		desiredWarm int32
		minHot      int32
		want        int
	}{
		{"scale up: plenty of ready pods", 10, 2, 1, 8},
		{"scale down: desiredWarm exceeds readyCount, minHot floors at 1", 2, 5, 1, 1},
		{"R=0: minHot must not push target above readyCount", 0, 2, 1, 0},
		{"desiredWarm=0 promotes everything ready", 5, 0, 0, 5},
		{"minHot satisfied without flooring", 4, 1, 2, 3},
		{"minHot floor kicks in", 4, 3, 2, 2},
		{"steady state: no floor, no cap needed", 3, 1, 0, 2},
		{"R below minHot+desiredWarm: minHot still wins over the raw deficit", 1, 2, 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := targetHot(tc.readyCount, tc.desiredWarm, tc.minHot)
			if got != tc.want {
				t.Errorf("targetHot(%d, %d, %d) = %d, want %d", tc.readyCount, tc.desiredWarm, tc.minHot, got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Envtest-backed Ginkgo specs for WarmPoolReconciler.Reconcile.
// -----------------------------------------------------------------------------

// newManagedDeployment returns a minimal, schema-valid Deployment opted into WarmPool
// management, selecting pods labeled app=name.
func newManagedDeployment(name string, extraAnnotations map[string]string) *appsv1.Deployment {
	annotations := map[string]string{pocv1alpha1.AnnotationManaged: "true"}
	for k, v := range extraAnnotations {
		annotations[k] = v
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "busybox"}},
				},
			},
		},
	}
}

// newDeploymentPod returns an unstarted Pod matching depName's selector, scheduled onto
// node. Labels beyond "app" (e.g. the state label) may be added by the caller before
// Create.
func newDeploymentPod(name, depName, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{"app": depName},
		},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Name: "c", Image: "busybox"}},
		},
	}
}

// markPodRunningReady patches pod's status to Running+Ready via the status subresource
// (Pod's create strategy always resets status, so this must be a separate call).
func markPodRunningReady(pod *corev1.Pod) {
	pod.Status = corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

// newGPUNode returns a cluster-scoped Node carrying the poc.llm-d.ai/gpu-count/model
// labels the inventory package reads.
func newGPUNode(name string, count int32) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				pocv1alpha1.NodeLabelGPUCount: fmt.Sprintf("%d", count),
				pocv1alpha1.NodeLabelGPUModel: "test-gpu",
			},
		},
	}
}

// forceDeleteLease deletes a GPULease that may carry poc.Finalizer, stripping the
// finalizer so the object actually disappears (no GPULease reconciler runs in this test
// binary to do it for us).
func forceDeleteLease(name string) {
	var l pocv1alpha1.GPULease
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &l); err != nil {
		return
	}
	_ = k8sClient.Delete(ctx, &l)
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &l); err == nil {
		l.Finalizers = nil
		_ = k8sClient.Update(ctx, &l)
	}
}

// drainEvents non-blockingly collects every event currently buffered on rec.
func drainEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

var _ = Describe("WarmPool Controller", func() {
	var (
		recorder      *record.FakeRecorder
		reconciler    *WarmPoolReconciler
		createdDeps   []string
		createdPods   []types.NamespacedName
		createdLeases []string
		createdNodes  []string
	)

	BeforeEach(func() {
		recorder = record.NewFakeRecorder(20)
		reconciler = &WarmPoolReconciler{
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  recorder,
			Inventory: inventory.New(k8sClient),
		}
		createdDeps = nil
		createdPods = nil
		createdLeases = nil
		createdNodes = nil
	})

	AfterEach(func() {
		for _, n := range createdLeases {
			forceDeleteLease(n)
		}
		for _, key := range createdPods {
			_ = k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}})
		}
		for _, name := range createdDeps {
			_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})
		}
		for _, name := range createdNodes {
			_ = k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
		}
	})

	reconcileDeployment := func(name string) (reconcile.Result, error) {
		return reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: "default", Name: name},
		})
	}

	Context("promotion", func() {
		It("creates a correctly-named GPULease claiming the promoted pod", func() {
			const depName = "wp-promote"
			const nodeName = "wp-promote-node"

			node := newGPUNode(nodeName, 1)
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			createdNodes = append(createdNodes, nodeName)

			dep := newManagedDeployment(depName, map[string]string{
				pocv1alpha1.AnnotationWarmReplicas:   "0",
				pocv1alpha1.AnnotationMinHotReplicas: "1",
			})
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			createdDeps = append(createdDeps, depName)

			pod := newDeploymentPod(depName+"-1", depName, nodeName)
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			createdPods = append(createdPods, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name})
			markPodRunningReady(pod)

			promotionsBefore := testutil.ToFloat64(PromotionsTotal)

			_, err := reconcileDeployment(depName)
			Expect(err).NotTo(HaveOccurred())

			wantLeaseName := pocv1alpha1.LeaseName(nodeName, 0)
			createdLeases = append(createdLeases, wantLeaseName)

			var lease pocv1alpha1.GPULease
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: wantLeaseName}, &lease)).To(Succeed())
			Expect(lease.Spec.NodeName).To(Equal(nodeName))
			Expect(lease.Spec.GPUID).To(Equal(int32(0)))
			Expect(lease.Spec.ClaimRef.Namespace).To(Equal(pod.Namespace))
			Expect(lease.Spec.ClaimRef.Name).To(Equal(pod.Name))
			Expect(lease.Spec.ClaimRef.UID).To(Equal(pod.UID))
			Expect(lease.Spec.DeploymentRef.Name).To(Equal(depName))

			var gotPod corev1.Pod
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, &gotPod)).To(Succeed())
			Expect(gotPod.Labels[pocv1alpha1.StateLabelKey]).To(Equal(pocv1alpha1.StateActivating))

			Expect(testutil.ToFloat64(PromotionsTotal) - promotionsBefore).To(Equal(1.0))
			Expect(drainEvents(recorder)).To(ContainElement(ContainSubstring(EventReasonPromoted)))
		})
	})

	Context("blocked promotion", func() {
		It("emits an event and the failure metric, and creates no GPULease, when no node has a free GPU", func() {
			const depName = "wp-blocked"
			const nodeName = "wp-blocked-node"

			node := newGPUNode(nodeName, 0) // no leasable GPUs at all
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			createdNodes = append(createdNodes, nodeName)

			dep := newManagedDeployment(depName, map[string]string{
				pocv1alpha1.AnnotationWarmReplicas:   "0",
				pocv1alpha1.AnnotationMinHotReplicas: "1",
			})
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			createdDeps = append(createdDeps, depName)

			pod := newDeploymentPod(depName+"-1", depName, nodeName)
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			createdPods = append(createdPods, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name})
			markPodRunningReady(pod)

			failuresBefore := testutil.ToFloat64(LeaseAcquireFailuresTotal.WithLabelValues(depName, pocv1alpha1.ReasonNoFreeGPUOnAnyPodNode))

			res, err := reconcileDeployment(depName)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(blockedPromotionRequeueAfter))

			var leases pocv1alpha1.GPULeaseList
			Expect(k8sClient.List(ctx, &leases)).To(Succeed())
			for _, l := range leases.Items {
				Expect(l.Spec.DeploymentRef.Name).NotTo(Equal(depName))
			}

			Expect(testutil.ToFloat64(LeaseAcquireFailuresTotal.WithLabelValues(depName, pocv1alpha1.ReasonNoFreeGPUOnAnyPodNode)) - failuresBefore).To(Equal(1.0))
			Expect(drainEvents(recorder)).To(ContainElement(ContainSubstring(EventReasonNoFreeGPUOnAnyNode)))

			var gotPod corev1.Pod
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, &gotPod)).To(Succeed())
			Expect(gotPod.Labels[pocv1alpha1.StateLabelKey]).To(Equal(pocv1alpha1.StateWarm))
		})
	})

	Context("demotion", func() {
		It("deletes the newest hot pod's GPULease and marks that pod releasing", func() {
			const depName = "wp-demote"
			const nodeName = "wp-demote-node"

			dep := newManagedDeployment(depName, map[string]string{
				pocv1alpha1.AnnotationWarmReplicas:   "1",
				pocv1alpha1.AnnotationMinHotReplicas: "0",
			})
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			createdDeps = append(createdDeps, depName)

			pod := newDeploymentPod(depName+"-1", depName, nodeName)
			pod.Labels[pocv1alpha1.StateLabelKey] = pocv1alpha1.StateHot
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			createdPods = append(createdPods, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name})
			markPodRunningReady(pod)

			leaseName := pocv1alpha1.LeaseName(nodeName, 0)
			lease := &pocv1alpha1.GPULease{
				ObjectMeta: metav1.ObjectMeta{Name: leaseName, Finalizers: []string{pocv1alpha1.Finalizer}},
				Spec: pocv1alpha1.GPULeaseSpec{
					NodeName:       nodeName,
					GPUID:          0,
					ClaimRef:       pocv1alpha1.ObjectRef{Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID},
					DeploymentRef:  pocv1alpha1.ObjectRef{Namespace: dep.Namespace, Name: dep.Name},
					LocalQueueName: "irrelevant",
				},
			}
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())
			createdLeases = append(createdLeases, leaseName)

			demotionsBefore := testutil.ToFloat64(DemotionsTotal)

			_, err := reconcileDeployment(depName)
			Expect(err).NotTo(HaveOccurred())

			Expect(testutil.ToFloat64(DemotionsTotal) - demotionsBefore).To(Equal(1.0))
			Expect(drainEvents(recorder)).To(ContainElement(ContainSubstring(EventReasonDemoted)))

			var gotLease pocv1alpha1.GPULease
			err = k8sClient.Get(ctx, types.NamespacedName{Name: leaseName}, &gotLease)
			if err == nil {
				Expect(gotLease.DeletionTimestamp).NotTo(BeNil())
			} else {
				Expect(errors.IsNotFound(err)).To(BeTrue())
			}

			var gotPod corev1.Pod
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, &gotPod)).To(Succeed())
			Expect(gotPod.Labels[pocv1alpha1.StateLabelKey]).To(Equal(pocv1alpha1.StateReleasing))
		})
	})

	Context("orphan garbage collection", func() {
		It("reclaims a GPULease whose claimed pod no longer exists", func() {
			const depName = "wp-orphan"
			const nodeName = "wp-orphan-node"

			dep := newManagedDeployment(depName, nil)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			createdDeps = append(createdDeps, depName)

			leaseName := pocv1alpha1.LeaseName(nodeName, 0)
			lease := &pocv1alpha1.GPULease{
				ObjectMeta: metav1.ObjectMeta{Name: leaseName, Finalizers: []string{pocv1alpha1.Finalizer}},
				Spec: pocv1alpha1.GPULeaseSpec{
					NodeName:       nodeName,
					GPUID:          0,
					ClaimRef:       pocv1alpha1.ObjectRef{Namespace: "default", Name: depName + "-ghost"},
					DeploymentRef:  pocv1alpha1.ObjectRef{Namespace: dep.Namespace, Name: dep.Name},
					LocalQueueName: "irrelevant",
				},
			}
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())
			createdLeases = append(createdLeases, leaseName)

			_, err := reconcileDeployment(depName)
			Expect(err).NotTo(HaveOccurred())

			var gotLease pocv1alpha1.GPULease
			err = k8sClient.Get(ctx, types.NamespacedName{Name: leaseName}, &gotLease)
			if err == nil {
				Expect(gotLease.DeletionTimestamp).NotTo(BeNil())
			} else {
				Expect(errors.IsNotFound(err)).To(BeTrue())
			}
			Expect(drainEvents(recorder)).To(ContainElement(ContainSubstring(EventReasonOrphanLeaseReclaimed)))
		})
	})

	Context("new pod initialization", func() {
		It("labels a freshly-observed pod warm with deletion-cost -100", func() {
			const depName = "wp-newpod"
			const nodeName = "wp-newpod-node"

			dep := newManagedDeployment(depName, nil)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			createdDeps = append(createdDeps, depName)

			pod := newDeploymentPod(depName+"-1", depName, nodeName)
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			createdPods = append(createdPods, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name})
			markPodRunningReady(pod)

			_, err := reconcileDeployment(depName)
			Expect(err).NotTo(HaveOccurred())

			var gotPod corev1.Pod
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, &gotPod)).To(Succeed())
			Expect(gotPod.Labels[pocv1alpha1.StateLabelKey]).To(Equal(pocv1alpha1.StateWarm))
			Expect(gotPod.Annotations[pocv1alpha1.AnnotationPodDeletionCost]).To(Equal(pocv1alpha1.PodDeletionCostWarm))
		})
	})

	Context("failed lease retention", func() {
		// api/v1alpha1.DefaultFailedLeaseRetention's doc comment explains why this sweep
		// exists at all: usedGPUIDs() counts every GPULease regardless of phase, and a
		// Failed lease's claim pod is normally still alive/Ready/on the right node (the
		// GPULease reconciler reverts it to warm on failure), so it is never an orphan by
		// pod-identity rules alone.
		// setup creates a fresh node/Deployment/pod trio named after suffix (each It uses
		// its own suffix: envtest has no kubelet/GC to finish deleting a directly-created
		// Pod between specs, so sharing one fixture across Its via BeforeEach would race
		// the previous It's still-terminating pod). The claim pod stays alive/Ready/on
		// the right node, mirroring the post-failure revert-to-warm state (design.md
		// §4.7): the lease is Failed only by its own status, not because its claim pod is
		// an identity-based orphan, which is exactly the gap this retention sweep closes.
		// It returns (depName, nodeName, leaseName). MinHotReplicas=0/WarmReplicas=1 with
		// a single ready pod makes targetHot 0, so the promotion logic later in the same
		// Reconcile never claims the GPU this sweep just freed -- keeping this Context's
		// assertions about the sweep itself uncontaminated by unrelated promotion.
		setup := func(suffix string) (string, string, string) {
			depName := "wp-failed-retention-" + suffix
			nodeName := depName + "-node"
			leaseName := pocv1alpha1.LeaseName(nodeName, 0)

			node := newGPUNode(nodeName, 1)
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			createdNodes = append(createdNodes, nodeName)

			dep := newManagedDeployment(depName, map[string]string{
				pocv1alpha1.AnnotationWarmReplicas:   "1",
				pocv1alpha1.AnnotationMinHotReplicas: "0",
			})
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			createdDeps = append(createdDeps, depName)

			pod := newDeploymentPod(depName+"-1", depName, nodeName)
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			createdPods = append(createdPods, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name})
			markPodRunningReady(pod)

			return depName, nodeName, leaseName
		}

		// newLease creates a GPULease named leaseName claiming pod depName+"-1" on
		// nodeName, in phase with a Ready condition backdated by age. GPULease has a
		// status subresource, so Status must be set via a separate Status().Update after
		// Create.
		newLease := func(depName, nodeName, leaseName string, age time.Duration, phase pocv1alpha1.GPULeasePhase) *pocv1alpha1.GPULease {
			lease := &pocv1alpha1.GPULease{
				ObjectMeta: metav1.ObjectMeta{Name: leaseName, Finalizers: []string{pocv1alpha1.Finalizer}},
				Spec: pocv1alpha1.GPULeaseSpec{
					NodeName:       nodeName,
					GPUID:          0,
					ClaimRef:       pocv1alpha1.ObjectRef{Namespace: "default", Name: depName + "-1"},
					DeploymentRef:  pocv1alpha1.ObjectRef{Namespace: "default", Name: depName},
					LocalQueueName: "irrelevant",
				},
			}
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())
			createdLeases = append(createdLeases, leaseName)

			lease.Status.Phase = phase
			lease.Status.Message = "original failure detail"
			lease.Status.Conditions = []metav1.Condition{
				{
					Type:               pocv1alpha1.ConditionReady,
					Status:             metav1.ConditionFalse,
					Reason:             pocv1alpha1.ReasonPodGone,
					Message:            "original failure detail",
					LastTransitionTime: metav1.NewTime(time.Now().Add(-age)),
				},
			}
			Expect(k8sClient.Status().Update(ctx, lease)).To(Succeed())
			return lease
		}

		It("does not delete a Failed lease younger than the retention window", func() {
			depName, nodeName, leaseName := setup("young")
			reconciler.FailedLeaseRetention = time.Minute
			newLease(depName, nodeName, leaseName, time.Second, pocv1alpha1.GPULeasePhaseFailed)

			res, err := reconcileDeployment(depName)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(res.RequeueAfter).To(BeNumerically("<=", time.Minute))

			var gotLease pocv1alpha1.GPULease
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: leaseName}, &gotLease)).To(Succeed())
			Expect(gotLease.DeletionTimestamp).To(BeNil())
		})

		It("deletes a Failed lease older than the retention window, frees its GPU id, and emits FailedLeaseReclaimed", func() {
			depName, nodeName, leaseName := setup("old")
			reconciler.FailedLeaseRetention = 10 * time.Millisecond
			newLease(depName, nodeName, leaseName, time.Hour, pocv1alpha1.GPULeasePhaseFailed)

			_, err := reconcileDeployment(depName)
			Expect(err).NotTo(HaveOccurred())

			var gotLease pocv1alpha1.GPULease
			err = k8sClient.Get(ctx, types.NamespacedName{Name: leaseName}, &gotLease)
			Expect(errors.IsNotFound(err)).To(BeTrue())

			free, err := reconciler.Inventory.FreeGPUIDs(ctx, nodeName)
			Expect(err).NotTo(HaveOccurred())
			Expect(free).To(ContainElement(int32(0)))

			Expect(drainEvents(recorder)).To(ContainElement(ContainSubstring(EventReasonFailedLeaseReclaimed)))
		})

		It("schedules a requeue at/after the moment a not-yet-reclaimable Failed lease becomes reclaimable", func() {
			depName, nodeName, leaseName := setup("pending")
			reconciler.FailedLeaseRetention = 5 * time.Second
			newLease(depName, nodeName, leaseName, 4*time.Second, pocv1alpha1.GPULeasePhaseFailed)

			res, err := reconcileDeployment(depName)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(res.RequeueAfter).To(BeNumerically("<=", 2*time.Second))

			var gotLease pocv1alpha1.GPULease
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: leaseName}, &gotLease)).To(Succeed())
			Expect(gotLease.DeletionTimestamp).To(BeNil())
		})

		It("never reclaims a Bound lease of the same age (guards against over-broad deletion)", func() {
			depName, nodeName, leaseName := setup("bound")
			reconciler.FailedLeaseRetention = 10 * time.Millisecond
			newLease(depName, nodeName, leaseName, time.Hour, pocv1alpha1.GPULeasePhaseBound)

			_, err := reconcileDeployment(depName)
			Expect(err).NotTo(HaveOccurred())

			var gotLease pocv1alpha1.GPULease
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: leaseName}, &gotLease)).To(Succeed())
			Expect(gotLease.DeletionTimestamp).To(BeNil())

			Expect(drainEvents(recorder)).NotTo(ContainElement(ContainSubstring(EventReasonFailedLeaseReclaimed)))
		})
	})
})
