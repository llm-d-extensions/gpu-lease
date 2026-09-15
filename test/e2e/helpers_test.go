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

// Shared helpers for the two scenario tests in e2e_test.go. Everything here shells
// out to kubectl (matching the kubebuilder scaffold's own convention in
// test/utils/utils.go) rather than opening a client-go connection, so this suite
// needs nothing beyond the same kubeconfig context an operator running
// docs/demo.md's commands by hand would already have. Where the plain string
// output of a jsonpath query isn't enough -- or, empirically, where the jsonpath
// query targets a map key containing both a "." and a "/" (e.g. the
// gpu-lease.llm-d.ai/* label/annotation keys used throughout this PoC): a live
// side-by-side comparison found `-o jsonpath={.metadata.labels['gpu-lease.
// llm-d.ai/state']}` returning a stale/empty string for many seconds after the
// same field was already correct and stable via `-o json`, so every read of one
// of those keys in this file uses `-o json` decode instead, never bracket-key
// jsonpath -- `-o json` is decoded into either this project's own api/v1alpha1
// types (for GPULease) or corev1/appsv1 types (for Pod/Deployment/ConfigMap)
// rather than a jq dependency -- this repo does not import the
// kueue.x-k8s.io/kueue Go module (the controller talks to Kueue only via the
// dynamic/typed core client against the kueue.x-k8s.io/v1beta2 Workload GVK, per
// design.md §4.7.1), so Workload/ResourceFlavor are decoded into small local
// structs carrying only the fields these tests check.

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gpuleasev1alpha1 "github.com/llm-d-extensions/gpu-lease/api/v1alpha1"
	"github.com/llm-d-extensions/gpu-lease/test/utils"
)

const (
	appA = "app-a"
	appB = "app-b"

	// baselineDemand matches hack/demo.sh's BASELINE_DEMAND and docs/demo.md step
	// 1: with pod-capacity-rps=10 and warm-replicas=2, ceil(20/10)+2 = 4 replicas,
	// 2 hot + 2 warm.
	baselineDemand = "20"

	// demandConfigMap is deploy/apps/*'s shared demand source (design.md §5).
	demandConfigMap = "gpu-lease-demand"
)

func runKubectl(args ...string) (string, error) {
	return utils.Run(exec.Command("kubectl", args...))
}

// ---------------------------------------------------------------------------
// demand ConfigMap
// ---------------------------------------------------------------------------

func setDemand(app, rps string) error {
	_, err := runKubectl("-n", pocNamespace, "patch", "configmap", demandConfigMap,
		"--type", "merge", "-p", fmt.Sprintf(`{"data":{%q:%q}}`, app, rps))
	return err
}

func getDemand(app string) (string, error) {
	return runKubectl("get", "configmap", demandConfigMap, "-n", pocNamespace,
		"-o", fmt.Sprintf("jsonpath={.data.%s}", app))
}

// ---------------------------------------------------------------------------
// pods
// ---------------------------------------------------------------------------

func getPods(app string) (*corev1.PodList, error) {
	out, err := runKubectl("get", "pods", "-n", pocNamespace, "-l", "app="+app, "-o", "json")
	if err != nil {
		return nil, err
	}
	var list corev1.PodList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("decoding pod list for app=%s: %w", app, err)
	}
	return &list, nil
}

const stateLabel = "gpu-lease.llm-d.ai/state"

// hotWarmCounts returns [hot, warm] pod counts for app, as an array so it can be
// compared directly with gomega's Equal matcher from an Eventually callback.
func hotWarmCounts(app string) [2]int {
	list, err := getPods(app)
	if err != nil {
		return [2]int{-1, -1}
	}
	var hot, warm int
	for _, p := range list.Items {
		switch p.Labels[stateLabel] {
		case "hot":
			hot++
		case "warm":
			warm++
		}
	}
	return [2]int{hot, warm}
}

// podState reads a single pod's stateLabel via a full -o json decode rather than
// `-o jsonpath={.metadata.labels['...']}`. That bracket-key jsonpath form was
// tried first and found empirically unreliable for this specific key: against a
// live cluster, `kubectl get pod ... -o jsonpath="{.metadata.labels['gpu-lease.
// llm-d.ai/state']}"` repeatedly returned an empty string for many consecutive
// seconds (both a 30s and a 90s Eventually window observed) for a label the
// controller's own logs and a same-moment `-o json` decode both showed was
// already set correctly -- i.e. the delay was in this jsonpath invocation, not
// in the controller or the API server. `-o json` decode never showed that
// staleness in the same side-by-side comparison, so it's used here instead,
// matching getPods' existing pattern in this file.
func podState(name string) (string, error) {
	out, err := runKubectl("get", "pod", name, "-n", pocNamespace, "-o", "json")
	if err != nil {
		return "", err
	}
	var pod corev1.Pod
	if err := json.Unmarshal([]byte(out), &pod); err != nil {
		return "", fmt.Errorf("decoding pod %s: %w", name, err)
	}
	return pod.Labels[stateLabel], nil
}

func podNodeName(name string) (string, error) {
	return runKubectl("get", "pod", name, "-n", pocNamespace, "-o", "jsonpath={.spec.nodeName}")
}

func noPodCrashLooping(app string) bool {
	list, err := getPods(app)
	if err != nil {
		return false
	}
	for _, p := range list.Items {
		var restarts int32
		for _, cs := range p.Status.ContainerStatuses {
			restarts += cs.RestartCount
		}
		if restarts > 2 {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// GPULeases (cluster-scoped)
// ---------------------------------------------------------------------------

func getGPULeases() (*gpuleasev1alpha1.GPULeaseList, error) {
	out, err := runKubectl("get", "gpuleases", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list gpuleasev1alpha1.GPULeaseList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("decoding GPULease list: %w", err)
	}
	return &list, nil
}

func getGPULease(name string) (*gpuleasev1alpha1.GPULease, error) {
	out, err := runKubectl("get", "gpulease", name, "-o", "json")
	if err != nil {
		return nil, err
	}
	var lease gpuleasev1alpha1.GPULease
	if err := json.Unmarshal([]byte(out), &lease); err != nil {
		return nil, fmt.Errorf("decoding GPULease %s: %w", name, err)
	}
	return &lease, nil
}

// boundLeaseNamesForApp returns the names of every Bound GPULease currently
// belonging to app, as a set, for diffing before/after a demand bump.
func boundLeaseNamesForApp(app string) (map[string]bool, error) {
	list, err := getGPULeases()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, l := range list.Items {
		if l.Status.Phase == gpuleasev1alpha1.GPULeasePhaseBound && l.Spec.DeploymentRef.Name == app {
			out[l.Name] = true
		}
	}
	return out, nil
}

func gpuLeaseFailedWithReason(reason string) bool {
	list, err := getGPULeases()
	if err != nil {
		return false
	}
	for _, l := range list.Items {
		if l.Status.Phase != gpuleasev1alpha1.GPULeasePhaseFailed {
			continue
		}
		for _, c := range l.Status.Conditions {
			if c.Type == gpuleasev1alpha1.ConditionReady && c.Reason == reason {
				return true
			}
		}
	}
	return false
}

func eventWithReason(reason string) bool {
	out, err := runKubectl("get", "events", "-n", pocNamespace,
		"--field-selector", "reason="+reason, "-o", "name")
	if err != nil {
		return false
	}
	return len(utils.GetNonEmptyLines(out)) > 0
}

// ---------------------------------------------------------------------------
// Kueue Workload / ResourceFlavor -- minimal local decode, see file header.
// ---------------------------------------------------------------------------

type workloadPodSetAssignment struct {
	Name    string            `json:"name"`
	Flavors map[string]string `json:"flavors"`
}

type workloadView struct {
	Status struct {
		Conditions []metav1.Condition `json:"conditions"`
		Admission  *struct {
			ClusterQueue      string                     `json:"clusterQueue"`
			PodSetAssignments []workloadPodSetAssignment `json:"podSetAssignments"`
		} `json:"admission,omitempty"`
	} `json:"status"`
}

func getWorkload(name string) (*workloadView, error) {
	out, err := runKubectl("get", "workload", name, "-n", pocNamespace, "-o", "json")
	if err != nil {
		return nil, err
	}
	var w workloadView
	if err := json.Unmarshal([]byte(out), &w); err != nil {
		return nil, fmt.Errorf("decoding Workload %s: %w", name, err)
	}
	return &w, nil
}

func workloadCondition(w *workloadView, condType string) *metav1.Condition {
	for i := range w.Status.Conditions {
		if w.Status.Conditions[i].Type == condType {
			return &w.Status.Conditions[i]
		}
	}
	return nil
}

type resourceFlavorView struct {
	Spec struct {
		NodeLabels map[string]string `json:"nodeLabels"`
	} `json:"spec"`
}

func getResourceFlavor(name string) (*resourceFlavorView, error) {
	out, err := runKubectl("get", "resourceflavor", name, "-o", "json")
	if err != nil {
		return nil, err
	}
	var rf resourceFlavorView
	if err := json.Unmarshal([]byte(out), &rf); err != nil {
		return nil, fmt.Errorf("decoding ResourceFlavor %s: %w", name, err)
	}
	return &rf, nil
}

// ---------------------------------------------------------------------------
// Deployment nodeSelector / annotation mutation (mirrors hack/demo.sh's step6/
// step7 helpers, restricted to what these tests need).
// ---------------------------------------------------------------------------

func getDeployment(dep string) (*appsv1.Deployment, error) {
	out, err := runKubectl("get", "deployment", dep, "-n", pocNamespace, "-o", "json")
	if err != nil {
		return nil, err
	}
	var d appsv1.Deployment
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		return nil, fmt.Errorf("decoding deployment %s: %w", dep, err)
	}
	return &d, nil
}

// getNodeSelectorJSON decodes the Deployment via -o json (see podState's comment
// on why bracket-key jsonpath reads are avoided in this file) and re-marshals
// just the nodeSelector map, which is what restoreNodeSelector needs to embed
// back into a merge patch.
func getNodeSelectorJSON(dep string) (string, error) {
	d, err := getDeployment(dep)
	if err != nil {
		return "null", err
	}
	sel := d.Spec.Template.Spec.NodeSelector
	if len(sel) == 0 {
		return "null", nil
	}
	b, err := json.Marshal(sel)
	if err != nil {
		return "null", fmt.Errorf("marshaling nodeSelector for %s: %w", dep, err)
	}
	return string(b), nil
}

func setNodeSelectorHostname(dep, hostname string) error {
	_, err := runKubectl("patch", "deployment", dep, "-n", pocNamespace, "--type=merge",
		"-p", fmt.Sprintf(`{"spec":{"template":{"spec":{"nodeSelector":{"kubernetes.io/hostname":%q}}}}}`, hostname))
	return err
}

// restoreNodeSelector applies origJSON (as previously returned by
// getNodeSelectorJSON) back via a JSON merge patch. "null" removes the field
// entirely (RFC 7396), matching these Deployments' default of no nodeSelector
// (Phase 3 note: the app Deployments deliberately ship without one).
func restoreNodeSelector(dep, origJSON string) error {
	_, err := runKubectl("patch", "deployment", dep, "-n", pocNamespace, "--type=merge",
		"-p", fmt.Sprintf(`{"spec":{"template":{"spec":{"nodeSelector":%s}}}}`, origJSON))
	return err
}

// getAnnotation decodes the Deployment via -o json rather than
// `-o jsonpath={.metadata.annotations['...']}` -- see podState's comment; the
// same dotted/slashed bracket-key jsonpath form is used here for a key
// ("gpu-lease.llm-d.ai/admission-timeout") shaped just like the one proven
// unreliable, so this is fixed proactively rather than waiting to prove it fails
// the same way.
func getAnnotation(dep, key string) (string, error) {
	d, err := getDeployment(dep)
	if err != nil {
		return "", err
	}
	return d.Annotations[key], nil
}

func setOrClearAnnotation(dep, key, val string) error {
	if val == "" {
		_, err := runKubectl("annotate", "deployment", dep, "-n", pocNamespace, key+"-", "--overwrite")
		return err
	}
	_, err := runKubectl("annotate", "deployment", dep, "-n", pocNamespace,
		fmt.Sprintf("%s=%s", key, val), "--overwrite")
	return err
}

// ---------------------------------------------------------------------------
// node capacity label (hack/setup-nodes.sh)
// ---------------------------------------------------------------------------

func setupNodesScript() (string, error) {
	dir, err := utils.GetProjectDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hack", "setup-nodes.sh"), nil
}

func setNodeCapacity(node string, capacity int) error {
	script, err := setupNodesScript()
	if err != nil {
		return err
	}
	_, err = utils.Run(exec.Command(script, "--node", node, "--capacity", fmt.Sprintf("%d", capacity)))
	return err
}
