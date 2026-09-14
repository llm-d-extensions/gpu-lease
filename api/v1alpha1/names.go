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

package v1alpha1

import (
	"fmt"
	"strconv"
	"time"
)

// This file is the shared vocabulary of the GPU Lease PoC: finalizer, label and
// annotation keys/values, node label keys, the Kueue resource name, and the annotation
// defaults. Every phase (2-4) must reference these constants rather than hardcoding the
// strings, so that a rename only ever touches this file. See design.md §4.3-§4.5.

// Finalizer added to every GPULease to guarantee the backing Kueue Workload is deleted
// (quota freed) before the GPULease is removed from etcd (design.md §4.2).
const Finalizer = "poc.llm-d.ai/release-lease"

// StateLabelKey is the pod label (not annotation, so Services and Prometheus can select
// on it) that records a pod's warm/hot lifecycle position (design.md §4.4).
const StateLabelKey = "gpu-lease.llm-d.ai/state"

// Values of StateLabelKey.
const (
	// StateWarm: running, no GPU lease, receives no traffic from the app's main Service.
	StateWarm = "warm"
	// StateActivating: a GPULease has been created for this pod and POST /lease issued;
	// waiting for the pod to report "hot".
	StateActivating = "activating"
	// StateHot: the pod holds a GPU lease and is ready to serve.
	StateHot = "hot"
	// StateReleasing: DELETE /lease has been issued; the pod is tearing down its lease.
	StateReleasing = "releasing"
)

// Deployment opt-in annotations (design.md §4.3). The controller reconciles a
// Deployment only if AnnotationManaged is "true".
const (
	// AnnotationManaged: "true" to opt a Deployment into WarmPool management. Required;
	// there is no default.
	AnnotationManaged = "gpu-lease.llm-d.ai/managed"
	// AnnotationWarmReplicas: desired warm pod count (the invariant). Default
	// DefaultWarmReplicas.
	AnnotationWarmReplicas = "gpu-lease.llm-d.ai/warm-replicas"
	// AnnotationMinHotReplicas: floor on hot pod count; wins over the warm target.
	// Default DefaultMinHotReplicas.
	AnnotationMinHotReplicas = "gpu-lease.llm-d.ai/min-hot-replicas"
	// AnnotationLocalQueue: the Kueue LocalQueue to charge leases against. Default is
	// "<deployment>-gpu" (computed, not a constant; see DefaultLocalQueueName).
	AnnotationLocalQueue = "gpu-lease.llm-d.ai/local-queue"
	// AnnotationPodCapacityRPS: per-hot-pod capacity, docs/metrics only. Default
	// DefaultPodCapacityRPS.
	AnnotationPodCapacityRPS = "gpu-lease.llm-d.ai/pod-capacity-rps"
	// AnnotationActivationTimeout: pod warmup budget. Default DefaultActivationTimeout.
	AnnotationActivationTimeout = "gpu-lease.llm-d.ai/activation-timeout"
	// AnnotationAdmissionTimeout: Kueue admission budget before declaring failure.
	// Default DefaultAdmissionTimeout.
	AnnotationAdmissionTimeout = "gpu-lease.llm-d.ai/admission-timeout"
)

// Pod annotations, controller-owned (design.md §4.4).
const (
	// AnnotationLease: name of the GPULease this pod currently holds/is acquiring.
	AnnotationLease = "gpu-lease.llm-d.ai/lease"
	// AnnotationGPUID: the GPU index the pod's lease refers to.
	AnnotationGPUID = "gpu-lease.llm-d.ai/gpu-id"
	// AnnotationGPUModel: the GPU model of the pod's lease, informational.
	AnnotationGPUModel = "gpu-lease.llm-d.ai/gpu-model"
	// AnnotationPodDeletionCost is the well-known Kubernetes annotation the WarmPool
	// reconciler sets to bias ReplicaSet scale-down away from hot pods (design.md §4.4).
	AnnotationPodDeletionCost = "controller.kubernetes.io/pod-deletion-cost"
)

// Values of AnnotationPodDeletionCost: hot pods must survive a scale-down before warm
// pods do.
const (
	PodDeletionCostHot  = "100"
	PodDeletionCostWarm = "-100"
)

// Node label keys, applied by hack/setup-nodes.sh, that form the single source of truth
// for GPU inventory (design.md §4.5).
const (
	// NodeLabelGPUCount: leasable GPU count on the node; valid IDs are [0, count).
	NodeLabelGPUCount = "poc.llm-d.ai/gpu-count"
	// NodeLabelGPUModel: informational GPU model string for the node.
	NodeLabelGPUModel = "poc.llm-d.ai/gpu-model"
)

// KueueResourceName is the extended resource name a lease's Workload podSet requests
// (1 unit per lease). It is never requested by a real pod (design.md §4.1, §4.5).
const KueueResourceName = "poc.llm-d.ai/gpu"

// Annotation defaults (design.md §4.3).
const (
	DefaultWarmReplicas      int32         = 2
	DefaultMinHotReplicas    int32         = 1
	DefaultPodCapacityRPS    int32         = 10
	DefaultActivationTimeout time.Duration = 120 * time.Second
	DefaultAdmissionTimeout  time.Duration = 60 * time.Second
)

// LeaseName returns the derived, deterministic GPULease name for the (node, gpuID) pair:
// "<node>-gpu-<id>". Because GPULease names must be unique in etcd, this name doubles as
// the mutual-exclusion mechanism for the pair (design.md §4.2).
func LeaseName(node string, gpuID int32) string {
	return fmt.Sprintf("%s-gpu-%d", node, gpuID)
}

// DefaultLocalQueueName returns the default LocalQueue name for a Deployment that does
// not set AnnotationLocalQueue: "<deployment>-gpu".
func DefaultLocalQueueName(deployment string) string {
	return fmt.Sprintf("%s-gpu", deployment)
}

// DeploymentSettings holds the parsed, defaulted values of the Deployment opt-in
// annotations (design.md §4.3). Use ParseDeploymentSettings to build one from an
// annotation map.
type DeploymentSettings struct {
	// WarmReplicas is the desired warm pod count invariant.
	WarmReplicas int32
	// MinHotReplicas is the floor on hot pod count; wins over WarmReplicas.
	MinHotReplicas int32
	// LocalQueueName is the Kueue LocalQueue to charge leases against.
	LocalQueueName string
	// PodCapacityRPS is the per-hot-pod capacity, docs/metrics only.
	PodCapacityRPS int32
	// ActivationTimeout is the pod warmup budget.
	ActivationTimeout time.Duration
	// AdmissionTimeout is the Kueue admission budget before declaring failure.
	AdmissionTimeout time.Duration
}

// ParseDeploymentSettings reads the gpu-lease.llm-d.ai/* annotations off a Deployment
// (annotations map and deployment name, used to compute the default LocalQueue name) and
// returns the effective, defaulted settings. Malformed values (non-integer counts,
// non-duration timeouts) fall back to their documented default rather than erroring,
// since a typo'd annotation must not wedge the controller.
func ParseDeploymentSettings(annotations map[string]string, deploymentName string) DeploymentSettings {
	s := DeploymentSettings{
		WarmReplicas:      DefaultWarmReplicas,
		MinHotReplicas:    DefaultMinHotReplicas,
		LocalQueueName:    DefaultLocalQueueName(deploymentName),
		PodCapacityRPS:    DefaultPodCapacityRPS,
		ActivationTimeout: DefaultActivationTimeout,
		AdmissionTimeout:  DefaultAdmissionTimeout,
	}
	if annotations == nil {
		return s
	}
	if v, ok := annotations[AnnotationWarmReplicas]; ok {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n >= 0 {
			s.WarmReplicas = int32(n)
		}
	}
	if v, ok := annotations[AnnotationMinHotReplicas]; ok {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n >= 0 {
			s.MinHotReplicas = int32(n)
		}
	}
	if v, ok := annotations[AnnotationLocalQueue]; ok && v != "" {
		s.LocalQueueName = v
	}
	if v, ok := annotations[AnnotationPodCapacityRPS]; ok {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n > 0 {
			s.PodCapacityRPS = int32(n)
		}
	}
	if v, ok := annotations[AnnotationActivationTimeout]; ok {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			s.ActivationTimeout = d
		}
	}
	if v, ok := annotations[AnnotationAdmissionTimeout]; ok {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			s.AdmissionTimeout = d
		}
	}
	return s
}

// IsManaged reports whether a Deployment's annotations opt it into WarmPool management.
func IsManaged(annotations map[string]string) bool {
	return annotations != nil && annotations[AnnotationManaged] == "true"
}
