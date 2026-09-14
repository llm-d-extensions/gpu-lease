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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	poc "github.com/llm-d-extensions/gpu-lease/api/v1alpha1"
	"github.com/llm-d-extensions/gpu-lease/internal/controller/podclient"
	kueuev1beta2 "github.com/llm-d-extensions/gpu-lease/internal/kueue/v1beta2"
)

// These specs drive the GPULeaseReconciler's state machine (design.md §4.7) directly,
// calling Reconcile repeatedly against envtest exactly as the controller-runtime manager
// would, but under test control: no real Kueue controller runs against envtest, so tests
// stand in for it by patching the Workload's status conditions themselves, and a
// podclient.FakeClient stands in for the workload simulator pod's REST API.
//
// newManagedDeployment, newDeploymentPod, markPodRunningReady, forceDeleteLease and
// drainEvents are shared with warmpool_controller_test.go (same package); only the
// GPULease-specific helpers below (markPodReadyWithIP, newGPULease, admitWorkload,
// getWorkload) are new.

// markPodReadyWithIP patches pod's status (via the status subresource) to Running/Ready
// with podIP, the minimum a GPULease reconciler requires before it will call
// PodClient.Activate. Unlike the shared markPodRunningReady helper, this also sets PodIP.
func markPodReadyWithIP(ctx context.Context, pod *corev1.Pod, podIP string) {
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodRunning,
		PodIP: podIP,
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		},
	}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

// newGPULease returns a GPULease already carrying the release finalizer, matching how
// the WarmPool reconciler creates one in production (acquireLeaseOnNode) so tests do not
// need an extra Reconcile call just to observe finalizer-add-if-missing.
func newGPULease(node string, gpuID int32, pod *corev1.Pod, dep *appsv1.Deployment, queueName string) *poc.GPULease {
	return &poc.GPULease{
		ObjectMeta: metav1.ObjectMeta{
			Name:       poc.LeaseName(node, gpuID),
			Finalizers: []string{poc.Finalizer},
		},
		Spec: poc.GPULeaseSpec{
			NodeName: node,
			GPUID:    gpuID,
			GPUModel: "H100",
			ClaimRef: poc.ObjectRef{Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID},
			DeploymentRef: poc.ObjectRef{
				Namespace: dep.Namespace, Name: dep.Name,
			},
			LocalQueueName: queueName,
		},
	}
}

// admitWorkload patches wl's status to Kueue's own admitted shape (design.md §4.7.1):
// QuotaReserved=True, Admitted=True, and a podSetAssignment for the gpu resource naming
// flavor.
func admitWorkload(ctx context.Context, wl *kueuev1beta2.Workload, flavor string) {
	wl.Status.Conditions = []metav1.Condition{
		{Type: kueuev1beta2.ConditionQuotaReserved, Status: metav1.ConditionTrue, Reason: "QuotaReserved", Message: "quota reserved", LastTransitionTime: metav1.Now()},
		{Type: kueuev1beta2.ConditionAdmitted, Status: metav1.ConditionTrue, Reason: "Admitted", Message: "admitted", LastTransitionTime: metav1.Now()},
	}
	wl.Status.Admission = &kueuev1beta2.Admission{
		ClusterQueue: "cq",
		PodSetAssignments: []kueuev1beta2.PodSetAssignment{
			{Name: kueuev1beta2.PodSetName, Flavors: map[corev1.ResourceName]string{corev1.ResourceName(poc.KueueResourceName): flavor}},
		},
	}
	Expect(k8sClient.Status().Update(ctx, wl)).To(Succeed())
}

// getWorkload fetches the Workload backing lease, failing the test if it does not exist.
func getWorkload(ctx context.Context, lease *poc.GPULease) *kueuev1beta2.Workload {
	var wl kueuev1beta2.Workload
	Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: lease.Spec.ClaimRef.Namespace, Name: lease.Name}, &wl)).To(Succeed())
	return &wl
}

var _ = Describe("GPULease Controller", func() {
	ctx := context.Background()

	var (
		reconciler *GPULeaseReconciler
		fakePods   *podclient.FakeClient
		fakeRec    *record.FakeRecorder
	)

	BeforeEach(func() {
		fakePods = podclient.NewFakeClient()
		fakeRec = record.NewFakeRecorder(100)
		reconciler = &GPULeaseReconciler{
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			Recorder:  fakeRec,
			PodClient: fakePods,
		}
	})

	// reconcileN calls Reconcile n times in a row against lease's request, propagating
	// any error immediately.
	reconcileN := func(name string, n int) {
		req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}
		for i := 0; i < n; i++ {
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
		}
	}

	getLease := func(name string) *poc.GPULease {
		var lease poc.GPULease
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, &lease)).To(Succeed())
		return &lease
	}

	Context("happy path", func() {
		It("advances Pending -> Admitted -> Activating -> Bound and marks the pod hot", func() {
			dep := newManagedDeployment("happy-dep", nil)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, dep) })

			pod := newDeploymentPod("happy-pod", dep.Name, "node-happy")
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })
			markPodReadyWithIP(ctx, pod, "10.0.0.1")

			lease := newGPULease("node-happy", 0, pod, dep, "happy-queue")
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())
			DeferCleanup(func() { forceDeleteLease(lease.Name) })

			By("creating the backing Workload while Pending")
			reconcileN(lease.Name, 1)
			wl := getWorkload(ctx, lease)
			Expect(wl.Spec.QueueName).To(Equal("happy-queue"))
			Expect(wl.OwnerReferences).To(HaveLen(1))
			Expect(wl.OwnerReferences[0].Kind).To(Equal("GPULease"))
			Expect(wl.OwnerReferences[0].Name).To(Equal(lease.Name))
			Expect(getLease(lease.Name).Status.WorkloadName).To(Equal(lease.Name))

			By("admitting the Workload and observing the lease move to Admitted")
			admitWorkload(ctx, wl, "h100-flavor")
			reconcileN(lease.Name, 1)
			Expect(getLease(lease.Name).Status.Phase).To(Equal(poc.GPULeasePhaseAdmitted))
			Expect(getLease(lease.Name).Status.AdmittedAt).NotTo(BeNil())

			By("activating the pod and observing the lease move to Activating")
			reconcileN(lease.Name, 1)
			Expect(getLease(lease.Name).Status.Phase).To(Equal(poc.GPULeasePhaseActivating))
			Expect(fakePods.ActivateCalls).To(HaveLen(1))
			Expect(fakePods.ActivateCalls[0].Request.LeaseName).To(Equal(lease.Name))
			Expect(fakePods.ActivateCalls[0].Request.Node).To(Equal("node-happy"))

			By("observing the pod report hot and the lease move to Bound")
			reconcileN(lease.Name, 1)
			bound := getLease(lease.Name)
			Expect(bound.Status.Phase).To(Equal(poc.GPULeasePhaseBound))
			Expect(bound.Status.BoundAt).NotTo(BeNil())

			var hotPod corev1.Pod
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "happy-pod"}, &hotPod)).To(Succeed())
			Expect(hotPod.Labels[poc.StateLabelKey]).To(Equal(poc.StateHot))
			Expect(hotPod.Annotations[poc.AnnotationLease]).To(Equal(lease.Name))
			Expect(hotPod.Annotations[poc.AnnotationGPUID]).To(Equal("0"))
			Expect(hotPod.Annotations[poc.AnnotationPodDeletionCost]).To(Equal(poc.PodDeletionCostHot))

			Expect(fakeRec.Events).NotTo(BeEmpty())
		})
	})

	Context("admission failures", func() {
		It("fails with ReasonQuotaExhaustedOnNode once admission-timeout elapses", func() {
			dep := newManagedDeployment("timeout-dep", nil)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, dep) })

			pod := newDeploymentPod("timeout-pod", dep.Name, "node-timeout")
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })
			markPodReadyWithIP(ctx, pod, "10.0.0.2")

			lease := newGPULease("node-timeout", 0, pod, dep, "timeout-queue")
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())
			DeferCleanup(func() { forceDeleteLease(lease.Name) })

			reconcileN(lease.Name, 1)
			wl := getWorkload(ctx, lease)

			// No real Kueue controller runs in envtest, so wl.Status is never set; drive
			// the reconciler's clock past the default 60s admission-timeout instead of
			// sleeping for it.
			reconciler.Clock = func() time.Time {
				return wl.CreationTimestamp.Time.Add(poc.DefaultAdmissionTimeout + time.Second)
			}
			reconcileN(lease.Name, 1)

			failed := getLease(lease.Name)
			Expect(failed.Status.Phase).To(Equal(poc.GPULeasePhaseFailed))
			readyCond := readyConditionOf(failed)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Reason).To(Equal(poc.ReasonQuotaExhaustedOnNode))

			By("having deleted the backing Workload")
			var gone kueuev1beta2.Workload
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: wl.Namespace, Name: wl.Name}, &gone)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("fails immediately with ReasonQueueMisconfigured when Kueue reports Inadmissible, without waiting for admission-timeout", func() {
			dep := newManagedDeployment("inadmissible-dep", nil)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, dep) })

			pod := newDeploymentPod("inadmissible-pod", dep.Name, "node-inadmissible")
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })
			markPodReadyWithIP(ctx, pod, "10.0.0.3")

			lease := newGPULease("node-inadmissible", 0, pod, dep, "no-such-queue")
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())
			DeferCleanup(func() { forceDeleteLease(lease.Name) })

			reconcileN(lease.Name, 1)
			wl := getWorkload(ctx, lease)
			wl.Status.Conditions = []metav1.Condition{
				{Type: kueuev1beta2.ConditionQuotaReserved, Status: metav1.ConditionFalse, Reason: kueueReasonInadmissible,
					Message: "LocalQueue no-such-queue doesn't exist", LastTransitionTime: metav1.Now()},
			}
			Expect(k8sClient.Status().Update(ctx, wl)).To(Succeed())

			// Clock stays at "now": if this reason waited for admission-timeout like a
			// normal quota shortfall, this single immediate reconcile would still see
			// Pending, not Failed.
			reconcileN(lease.Name, 1)

			failed := getLease(lease.Name)
			Expect(failed.Status.Phase).To(Equal(poc.GPULeasePhaseFailed))
			Expect(failed.Status.Message).To(ContainSubstring("Inadmissible"))
			readyCond := readyConditionOf(failed)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Reason).To(Equal(poc.ReasonQueueMisconfigured))
		})
	})

	Context("activation failures", func() {
		It("fails with ReasonNodeMismatch when the pod rejects activation", func() {
			dep := newManagedDeployment("nodemismatch-dep", nil)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, dep) })

			pod := newDeploymentPod("nodemismatch-pod", dep.Name, "node-mismatch")
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })
			markPodReadyWithIP(ctx, pod, "10.0.0.4")

			lease := newGPULease("node-mismatch", 0, pod, dep, "mismatch-queue")
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())
			DeferCleanup(func() { forceDeleteLease(lease.Name) })

			reconcileN(lease.Name, 1)
			wl := getWorkload(ctx, lease)
			admitWorkload(ctx, wl, "h100-flavor")
			reconcileN(lease.Name, 1) // -> Admitted

			fakePods.ActivateErr["10.0.0.4"] = podclient.ErrNodeMismatch
			reconcileN(lease.Name, 1) // -> should fail, not Activating

			failed := getLease(lease.Name)
			Expect(failed.Status.Phase).To(Equal(poc.GPULeasePhaseFailed))
			readyCond := readyConditionOf(failed)
			Expect(readyCond.Reason).To(Equal(poc.ReasonNodeMismatch))
		})

		It("fails with ReasonPodGone when the claim pod disappears before activation completes", func() {
			dep := newManagedDeployment("podgone-dep", nil)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, dep) })

			pod := newDeploymentPod("podgone-pod", dep.Name, "node-podgone")
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			markPodReadyWithIP(ctx, pod, "10.0.0.5")

			lease := newGPULease("node-podgone", 0, pod, dep, "podgone-queue")
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())
			DeferCleanup(func() { forceDeleteLease(lease.Name) })

			reconcileN(lease.Name, 1)
			wl := getWorkload(ctx, lease)
			admitWorkload(ctx, wl, "h100-flavor")
			reconcileN(lease.Name, 1) // -> Admitted

			Expect(k8sClient.Delete(ctx, pod)).To(Succeed())

			reconcileN(lease.Name, 1) // -> should fail, not Activating

			failed := getLease(lease.Name)
			Expect(failed.Status.Phase).To(Equal(poc.GPULeasePhaseFailed))
			readyCond := readyConditionOf(failed)
			Expect(readyCond.Reason).To(Equal(poc.ReasonPodGone))
		})
	})

	Context("release", func() {
		It("removes the finalizer even though the claim pod is already gone", func() {
			dep := newManagedDeployment("release-dep", nil)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, dep) })

			pod := newDeploymentPod("release-pod", dep.Name, "node-release")
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			markPodReadyWithIP(ctx, pod, "10.0.0.6")

			lease := newGPULease("node-release", 0, pod, dep, "release-queue")
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())

			reconcileN(lease.Name, 1)
			wl := getWorkload(ctx, lease)
			admitWorkload(ctx, wl, "h100-flavor")
			reconcileN(lease.Name, 1) // -> Admitted
			reconcileN(lease.Name, 1) // -> Activating (POST /lease succeeds against the fake)
			Expect(getLease(lease.Name).Status.Phase).To(Equal(poc.GPULeasePhaseActivating))

			By("deleting the pod out from under the lease before it is ever released")
			Expect(k8sClient.Delete(ctx, pod)).To(Succeed())

			By("deleting the GPULease and confirming the finalizer does not block removal")
			Expect(k8sClient.Delete(ctx, lease)).To(Succeed())
			reconcileN(lease.Name, 1)

			var gone poc.GPULease
			err := k8sClient.Get(ctx, types.NamespacedName{Name: lease.Name}, &gone)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())

			var wlGone kueuev1beta2.Workload
			err = k8sClient.Get(ctx, client.ObjectKey{Namespace: wl.Namespace, Name: wl.Name}, &wlGone)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("Workload shape mismatch", func() {
		It("deletes and recreates the Workload rather than patching it", func() {
			dep := newManagedDeployment("recreate-dep", nil)
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, dep) })

			pod := newDeploymentPod("recreate-pod", dep.Name, "node-recreate")
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })
			markPodReadyWithIP(ctx, pod, "10.0.0.7")

			lease := newGPULease("node-recreate", 0, pod, dep, "recreate-queue")
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())
			DeferCleanup(func() { forceDeleteLease(lease.Name) })

			reconcileN(lease.Name, 1)
			wl := getWorkload(ctx, lease)
			original := wl.UID

			By("mutating the Workload's spec to simulate a stale/mismatched shape")
			wl.Spec.PodSets[0].Count = 2
			Expect(k8sClient.Update(ctx, wl)).To(Succeed())

			By("reconciling once: the mismatched Workload must be deleted, not patched")
			reconcileN(lease.Name, 1)
			var deleted kueuev1beta2.Workload
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: wl.Namespace, Name: wl.Name}, &deleted)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())

			By("reconciling again: a fresh, correctly-shaped Workload is created")
			reconcileN(lease.Name, 1)
			recreated := getWorkload(ctx, lease)
			Expect(recreated.UID).NotTo(Equal(original))
			Expect(recreated.Spec.PodSets[0].Count).To(Equal(int32(1)))
		})
	})
})

// readyConditionOf returns lease's Ready condition, or nil if not yet set.
func readyConditionOf(lease *poc.GPULease) *metav1.Condition {
	for i := range lease.Status.Conditions {
		if lease.Status.Conditions[i].Type == poc.ConditionReady {
			return &lease.Status.Conditions[i]
		}
	}
	return nil
}
