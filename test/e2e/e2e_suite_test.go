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
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2" //nolint:golint,revive
	. "github.com/onsi/gomega"    //nolint:golint,revive

	"github.com/llm-d-extensions/gpu-lease/test/utils"
)

// This suite deliberately does NOT follow the kubebuilder scaffold's default e2e
// pattern (build a throwaway image, kind-load it, install/uninstall CertManager,
// create/delete the controller's own namespace). Those steps assume a disposable,
// self-provisioned test environment. This repo's actual e2e story (design.md §2/§9,
// docs/demo.md) is the opposite: a single shared kind cluster
// (kind-wva-gpu-cluster) that already has Kueue, KEDA, kube-prometheus-stack, and
// this PoC's own controller/CRD/Deployments installed and running in their
// steady-state configuration (see hack/demo.sh step 1) -- reinstalling any of that
// here would either collide with what's already there or be actively wrong (a
// throwaway image tagged "example.com/gpu-lease-scaffold:v0.0.1" is not
// gpu-lease-controller:dev, and CertManager is not part of this PoC's stack at all;
// see docs/design.md §3/§4.5).
//
// So instead of provisioning anything, BeforeSuite only verifies the live
// environment already looks like the one docs/demo.md's install order produces,
// and Skip()s the whole suite with an actionable message if not. That keeps this
// runnable in two situations without any code path that could reinstall over, or
// tear down, someone else's running demo:
//   - the intended one: `make test-e2e` (or `go test ./test/e2e/`) against the
//     already-deployed kind-wva-gpu-cluster, after following docs/demo.md's
//     install order;
//   - a CI/sandbox run with no such cluster reachable: the suite Skips cleanly
//     instead of failing or trying to build/deploy anything.
//
// `make test` (envtest) never reaches this file at all: its test target filters
// `go list ./... | grep -v /e2e` (Makefile), so this package is excluded
// structurally, not just by a runtime guard -- the BeforeSuite guard below is a
// second, independent line of defense for anyone who runs `go test ./...`
// directly instead of via `make test`.
const (
	// pocNamespace is where the app-a/app-b Deployments, GPULeases' claimed pods,
	// the demand ConfigMap, and Kueue's LocalQueues/Workloads for this PoC live
	// (deploy/apps/, deploy/kueue/local-queues.yaml).
	pocNamespace = "gpu-lease-poc"

	// controllerNamespace is where `make deploy` puts the GPULease
	// controller-manager (config/default's kustomize namespace prefix).
	controllerNamespace = "gpu-lease-scaffold-system"

	// gpuLeaseCRDName is the cluster-scoped CRD this suite requires to already be
	// installed (`make install`, config/crd).
	gpuLeaseCRDName = "gpuleases.poc.llm-d.ai"
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting gpu-lease PoC e2e suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	skipUnless(clusterReachable, "no Kubernetes cluster reachable via the current kubeconfig context")
	skipUnless(crdInstalled(gpuLeaseCRDName),
		fmt.Sprintf("CRD %s is not installed -- run `make install` per docs/demo.md's install order", gpuLeaseCRDName))
	skipUnless(namespaceExists(controllerNamespace),
		fmt.Sprintf("namespace %s does not exist -- run `make deploy` per docs/demo.md", controllerNamespace))
	skipUnless(deploymentAvailable(controllerNamespace, "gpu-lease-scaffold-controller-manager"),
		"the GPULease controller-manager Deployment is not Available -- check `make deploy` succeeded and "+
			"`kubectl -n "+controllerNamespace+" get pods`")
	skipUnless(namespaceExists(pocNamespace),
		fmt.Sprintf("namespace %s does not exist -- apply deploy/apps/, deploy/kueue/, deploy/keda/ per docs/demo.md", pocNamespace))
	for _, dep := range []string{appA, appB} {
		skipUnless(deploymentAvailable(pocNamespace, dep),
			fmt.Sprintf("Deployment %s/%s is not Available -- apply deploy/apps/ per docs/demo.md", pocNamespace, dep))
	}
	skipUnless(configMapExists(pocNamespace, demandConfigMap),
		fmt.Sprintf("ConfigMap %s/%s (demand) does not exist -- apply deploy/apps/ per docs/demo.md", pocNamespace, demandConfigMap))

	// Baseline steady state (design.md §9 criterion 1 / hack/demo.sh step1): both
	// tests below mutate one deployment's demand/nodeSelector/labels temporarily
	// and restore them via DeferCleanup, but they still need to start from
	// something converged, or their own polling assertions have no stable
	// baseline to diff against.
	Expect(setDemand(appA, baselineDemand)).To(Succeed())
	Expect(setDemand(appB, baselineDemand)).To(Succeed())
	Eventually(func() [2]int { return hotWarmCounts(appA) }, "90s", "3s").Should(Equal([2]int{2, 2}),
		"app-a did not reach the 2 hot + 2 warm baseline before the suite started")
	Eventually(func() [2]int { return hotWarmCounts(appB) }, "90s", "3s").Should(Equal([2]int{2, 2}),
		"app-b did not reach the 2 hot + 2 warm baseline before the suite started")
})

func skipUnless(cond bool, reason string) {
	if !cond {
		Skip(reason)
	}
}

var clusterReachable = func() bool {
	cmd := exec.Command("kubectl", "cluster-info")
	_, err := utils.Run(cmd)
	return err == nil
}()

func crdInstalled(name string) bool {
	cmd := exec.Command("kubectl", "get", "crd", name)
	_, err := utils.Run(cmd)
	return err == nil
}

func namespaceExists(ns string) bool {
	cmd := exec.Command("kubectl", "get", "namespace", ns)
	_, err := utils.Run(cmd)
	return err == nil
}

func configMapExists(ns, name string) bool {
	cmd := exec.Command("kubectl", "get", "configmap", name, "-n", ns)
	_, err := utils.Run(cmd)
	return err == nil
}

func deploymentAvailable(ns, name string) bool {
	cmd := exec.Command("kubectl", "get", "deployment", name, "-n", ns,
		"-o", "jsonpath={.status.conditions[?(@.type==\"Available\")].status}")
	out, err := utils.Run(cmd)
	return err == nil && out == "True"
}
