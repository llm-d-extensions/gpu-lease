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

package e2e

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2" //nolint:golint,revive
	. "github.com/onsi/gomega"    //nolint:golint,revive

	gpuleasev1alpha1 "github.com/llm-d-extensions/gpu-lease/api/v1alpha1"
)

// These two specs cover the minimum design.md §9 asks for end-to-end, against the
// live cluster BeforeSuite already validated is up and at baseline:
//   - a promotion that reaches GPULease phase Bound with its pod reporting "hot",
//     and the full causal chain behind it (Kueue Workload Admitted, the admitted
//     ResourceFlavor's node label matching the lease's node) -- design.md §9
//     criterion 1/2, the same claim verified by hand for this Phase 6 report.
//   - one of the two documented failure modes, NoFreeGPUOnAnyPodNode -- design.md
//     §9 criterion 4. QuotaExhaustedOnNode (the other failure mode) is
//     deliberately left to hack/demo.sh --step 6: reaching it requires inflating a
//     node's poc.llm-d.ai/gpu-count label *above* the ClusterQueue's real
//     nominalQuota (see the comment above step6() in hack/demo.sh), which is a
//     property of this specific deployment's deploy/kueue/cluster-queue.yaml, not
//     something this suite should assume or hard-code. NoFreeGPUOnAnyPodNode needs
//     no such assumption -- zeroing a node's label is self-contained regardless of
//     quota -- so it is the one exercised here.
var _ = Describe("GPULease lifecycle", func() {

	Context("promotion", func() {
		It("promotes a warm pod to a Bound lease reporting hot, admitted by Kueue on the matching ResourceFlavor", func() {
			By("recording app-b's Bound leases before the demand bump")
			before, err := boundLeaseNamesForApp(appB)
			Expect(err).NotTo(HaveOccurred())

			origDemand, err := getDemand(appB)
			Expect(err).NotTo(HaveOccurred())
			if origDemand == "" {
				origDemand = baselineDemand
			}
			DeferCleanup(func() {
				By("restoring app-b's demand to " + origDemand)
				Expect(setDemand(appB, origDemand)).To(Succeed())
				// Best-effort re-convergence, not asserted: hack/demo.sh's own
				// restore path doesn't wait for this either, and the controller's
				// warm-pool resync (design.md §4.6) will get there on its own.
			})

			By("raising app-b's demand so KEDA scales up and the controller must promote the surplus warm pod")
			Expect(setDemand(appB, "25")).To(Succeed())

			By("waiting for a new Bound lease to appear for app-b")
			var newLeaseName string
			Eventually(func() (string, error) {
				after, err := boundLeaseNamesForApp(appB)
				if err != nil {
					return "", err
				}
				for name := range after {
					if !before[name] {
						newLeaseName = name
						return name, nil
					}
				}
				return "", nil
			}, "120s", "3s").ShouldNot(BeEmpty(), "no new Bound GPULease appeared for app-b")

			By("fetching the new lease and asserting phase Bound / Ready=True / reason Hot-or-Bound")
			lease, err := getGPULease(newLeaseName)
			Expect(err).NotTo(HaveOccurred())
			Expect(lease.Status.Phase).To(Equal(gpuleasev1alpha1.GPULeasePhaseBound))
			var readyCond *struct{ status, reason string }
			for _, c := range lease.Status.Conditions {
				if c.Type == gpuleasev1alpha1.ConditionReady {
					readyCond = &struct{ status, reason string }{string(c.Status), c.Reason}
				}
			}
			Expect(readyCond).NotTo(BeNil(), "lease has no Ready condition")
			Expect(readyCond.status).To(Equal("True"))

			By("asserting the claimed pod is labeled hot and actually runs on the lease's node")
			podName := lease.Spec.ClaimRef.Name
			// bindLease (internal/controller/gpulease_controller.go) patches the pod's
			// state label to hot strictly before it writes the lease's Bound/Ready
			// status in the same reconcile call, and the controller's own logs confirm
			// that write lands before (not after) the lease's Bound status is visible --
			// there's no real propagation delay to wait out here. An earlier version of
			// this assertion used a bare Expect, then a 30s/90s Eventually, and still saw
			// the label read back empty well past both windows; that turned out to be a
			// bug in podState() itself (see its comment in helpers_test.go), which used a
			// `-o jsonpath={.metadata.labels['...']}` bracket-key read that is unreliable
			// for this key -- fixed now to decode `-o json` instead. The short Eventually
			// below is kept only as ordinary defensive polling against the informer-cache
			// staleness that's normal for *any* live-cluster read, not because this
			// specific transition is known to be slow.
			Eventually(func() (string, error) { return podState(podName) }, "15s", "1s").
				Should(Equal("hot"), "pod %s should be labeled hot", podName)
			nodeName, err := podNodeName(podName)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodeName).To(Equal(lease.Spec.NodeName),
				"GPULease.spec.nodeName must match the claimed pod's actual node (design.md §9 criterion 2)")

			By("fetching the backing Kueue Workload and asserting QuotaReserved/Admitted are True")
			Expect(lease.Status.WorkloadName).NotTo(BeEmpty())
			wl, err := getWorkload(lease.Status.WorkloadName)
			Expect(err).NotTo(HaveOccurred())
			qr := workloadCondition(wl, "QuotaReserved")
			Expect(qr).NotTo(BeNil())
			Expect(string(qr.Status)).To(Equal("True"))
			adm := workloadCondition(wl, "Admitted")
			Expect(adm).NotTo(BeNil())
			Expect(string(adm.Status)).To(Equal("True"))
			Expect(wl.Status.Admission).NotTo(BeNil(), "admitted Workload must carry a podSetAssignments admission")

			By("asserting the admitted ResourceFlavor's nodeLabels match the lease's actual node")
			Expect(wl.Status.Admission.PodSetAssignments).NotTo(BeEmpty())
			var flavorName string
			for _, f := range wl.Status.Admission.PodSetAssignments[0].Flavors {
				flavorName = f
			}
			Expect(flavorName).NotTo(BeEmpty(), "podSetAssignments should name exactly one ResourceFlavor")
			rf, err := getResourceFlavor(flavorName)
			Expect(err).NotTo(HaveOccurred())
			Expect(rf.Spec.NodeLabels["kubernetes.io/hostname"]).To(Equal(lease.Spec.NodeName),
				"the admitted ResourceFlavor %q must be keyed to the same node the lease is bound to", flavorName)

			_, _ = fmt.Fprintf(GinkgoWriter,
				"promotion causal chain verified: pod=%s node=%s lease=%s workload=%s flavor=%s\n",
				podName, nodeName, newLeaseName, lease.Status.WorkloadName, flavorName)
		})
	})

	Context("failure mode: NoFreeGPUOnAnyPodNode", func() {
		It("declines to create a Workload when no warm pod sits on a node with a free GPU", func() {
			const node = "kind-wva-gpu-cluster-worker2"
			const admissionTimeoutKey = "gpu-lease.llm-d.ai/admission-timeout"

			origSelector, err := getNodeSelectorJSON(appA)
			Expect(err).NotTo(HaveOccurred())
			origTimeout, err := getAnnotation(appA, admissionTimeoutKey)
			Expect(err).NotTo(HaveOccurred())
			origDemand, err := getDemand(appA)
			Expect(err).NotTo(HaveOccurred())
			if origDemand == "" {
				origDemand = baselineDemand
			}

			DeferCleanup(func() {
				By("restoring app-a's nodeSelector, admission-timeout, demand, and " + node + "'s capacity")
				Expect(restoreNodeSelector(appA, origSelector)).To(Succeed())
				Expect(setOrClearAnnotation(appA, admissionTimeoutKey, origTimeout)).To(Succeed())
				Expect(setDemand(appA, origDemand)).To(Succeed())
				Expect(setNodeCapacity(node, 4)).To(Succeed())
			})

			By(fmt.Sprintf("zeroing %s's leasable GPU count", node))
			Expect(setNodeCapacity(node, 0)).To(Succeed())

			By("pinning app-a to " + node + " and shortening its admission-timeout")
			Expect(setNodeSelectorHostname(appA, node)).To(Succeed())
			Expect(setOrClearAnnotation(appA, admissionTimeoutKey, "20s")).To(Succeed())

			By("raising app-a's demand so the WarmPool reconciler must attempt a promotion")
			Expect(setDemand(appA, "80")).To(Succeed())

			By("waiting for a GPULease or event with reason NoFreeGPUOnAnyPodNode")
			Eventually(func() bool {
				return gpuLeaseFailedWithReason(gpuleasev1alpha1.ReasonNoFreeGPUOnAnyPodNode) ||
					eventWithReason(gpuleasev1alpha1.ReasonNoFreeGPUOnAnyPodNode)
			}, "180s", "3s").Should(BeTrue(),
				"expected a Failed GPULease or Event with reason NoFreeGPUOnAnyPodNode for app-a")

			By("asserting no app-a pod crash-looped while pinned to a GPU-less node")
			Expect(noPodCrashLooping(appA)).To(BeTrue())
		})
	})
})
