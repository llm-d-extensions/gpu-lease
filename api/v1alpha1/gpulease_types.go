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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// GPULeasePhase is the lifecycle phase of a GPULease, as driven by the GPULease
// reconciler's state machine (design.md §4.7).
type GPULeasePhase string

const (
	// GPULeasePhasePending means the Kueue Workload has not yet been created, or has
	// been created but not yet admitted.
	GPULeasePhasePending GPULeasePhase = "Pending"
	// GPULeasePhaseAdmitted means Kueue admitted the Workload (quota reserved) but the
	// pod has not yet been asked to activate.
	GPULeasePhaseAdmitted GPULeasePhase = "Admitted"
	// GPULeasePhaseActivating means POST /lease succeeded and the reconciler is polling
	// GET /state waiting for "hot".
	GPULeasePhaseActivating GPULeasePhase = "Activating"
	// GPULeasePhaseBound is the steady state: the pod reported "hot" and holds the lease.
	GPULeasePhaseBound GPULeasePhase = "Bound"
	// GPULeasePhaseFailed means the lease could not be acquired or maintained; see
	// status.message and the Ready condition's reason for one of the FailureReason
	// constants below.
	GPULeasePhaseFailed GPULeasePhase = "Failed"
	// GPULeasePhaseReleasing means the lease is being torn down: DELETE /lease, delete
	// the Workload, remove the finalizer.
	GPULeasePhaseReleasing GPULeasePhase = "Releasing"
)

// Condition types set on GPULease.status.conditions (design.md §4.2).
const (
	// ConditionQuotaReserved is True once the backing Kueue Workload is Admitted.
	ConditionQuotaReserved = "QuotaReserved"
	// ConditionPodActivated is True once the pod has acknowledged POST /lease.
	ConditionPodActivated = "PodActivated"
	// ConditionReady is True once the pod reports state "hot" (phase Bound).
	ConditionReady = "Ready"
)

// Failure reasons (exact strings, used by metrics and tests; design.md §4.2).
const (
	// ReasonQuotaExhaustedOnNode: the Kueue Workload was not admitted before
	// admission-timeout because the node's ResourceFlavor quota is exhausted.
	ReasonQuotaExhaustedOnNode = "QuotaExhaustedOnNode"
	// ReasonNoFreeGPUOnAnyPodNode: the WarmPool reconciler found no warm pod sitting on
	// a node with a free GPU to promote.
	ReasonNoFreeGPUOnAnyPodNode = "NoFreeGPUOnAnyPodNode"
	// ReasonPodUnreachable: POST /lease to the pod failed (network/HTTP error).
	ReasonPodUnreachable = "PodUnreachable"
	// ReasonActivationTimeout: the pod never reported state "hot" within
	// activation-timeout.
	ReasonActivationTimeout = "ActivationTimeout"
	// ReasonPodGone: the claimed pod no longer exists, is no longer Ready, or is no
	// longer part of the Deployment.
	ReasonPodGone = "PodGone"
	// ReasonNodeMismatch: the claimed pod has moved off spec.nodeName.
	ReasonNodeMismatch = "NodeMismatch"
	// ReasonQueueMisconfigured: Kueue rejected the Workload as Inadmissible — e.g. the
	// referenced LocalQueue or ClusterQueue does not exist, or the ClusterQueue has no
	// flavor matching the pinned node. This is a *configuration* error, not a capacity
	// one: waiting cannot help and no amount of scale-down frees anything. It is kept
	// distinct from ReasonQuotaExhaustedOnNode because the operator response differs
	// entirely (fix the manifests vs. wait for / add capacity), and because these
	// strings surface as the `reason` label on gpulease_lease_acquire_failures_total.
	ReasonQueueMisconfigured = "QueueMisconfigured"
)

// DefaultFailedLeaseRetention is how long a GPULease in phase Failed is kept before the
// WarmPool reconciler reclaims it.
//
// A Failed lease must not be deleted instantly: it is the primary evidence of the
// failure for `kubectl get gpuleases`, for demo assertions, and for a human debugging a
// stuck warm pod. But it must not be kept forever either, because
// inventory.usedGPUIDs() counts *every* GPULease on a node regardless of phase, and the
// lease's name (<node>-gpu-<id>) occupies that (node, gpuID) pair in etcd. A Failed
// lease whose pod is alive and healthy is not an orphan by pod-identity rules, so
// without an explicit retention sweep it would pin its GPU id forever — permanently
// shrinking usable capacity every time a promotion fails, and eventually making
// promotions on that node report NoFreeGPUOnAnyPodNode instead of the real cause.
//
// Retention (rather than skipping Failed leases in the inventory) is the deliberate
// choice: it keeps exactly one writer of the (node, gpuID) namespace and avoids a
// window where inventory believes an id is free while an etcd object still holds its
// name, which would surface as AlreadyExists churn during promotion.
const DefaultFailedLeaseRetention = 60 * time.Second

// ObjectRef is a minimal reference to a namespaced Kubernetes object, used to point a
// GPULease at the pod (ClaimRef) and Deployment (DeploymentRef) it was created for.
type ObjectRef struct {
	// Namespace of the referenced object.
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`
	// Name of the referenced object.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// UID of the referenced object, when known. Used to detect the object having been
	// replaced (e.g. pod recreated with the same name).
	// +optional
	UID types.UID `json:"uid,omitempty"`
}

// GPULeaseSpec defines the desired state of GPULease.
//
// A GPULease represents the exclusive right for the pod in ClaimRef, which must already
// be running on NodeName, to use GPU index GPUID on that node. The pair (NodeName, GPUID)
// is encoded in the GPULease's name (see LeaseName in names.go), which makes etcd the
// mutual-exclusion mechanism for the pair.
type GPULeaseSpec struct {
	// NodeName is the node that owns GPUID and that ClaimRef's pod must be running on.
	// +kubebuilder:validation:Required
	NodeName string `json:"nodeName"`

	// GPUID is the index of the GPU on NodeName, in [0, node's poc.llm-d.ai/gpu-count).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=0
	GPUID int32 `json:"gpuID"`

	// GPUModel is informational, copied from the node's poc.llm-d.ai/gpu-model label at
	// creation time.
	// +optional
	GPUModel string `json:"gpuModel,omitempty"`

	// ClaimRef is the pod that must run on NodeName and receives the lease.
	// +kubebuilder:validation:Required
	ClaimRef ObjectRef `json:"claimRef"`

	// DeploymentRef is the Deployment that owns the claiming pod.
	// +kubebuilder:validation:Required
	DeploymentRef ObjectRef `json:"deploymentRef"`

	// LocalQueueName is the Kueue LocalQueue that the backing Workload is submitted to.
	// +kubebuilder:validation:Required
	LocalQueueName string `json:"localQueueName"`
}

// GPULeaseStatus defines the observed state of GPULease.
type GPULeaseStatus struct {
	// Phase is the current state-machine phase. See design.md §4.7.
	// +optional
	Phase GPULeasePhase `json:"phase,omitempty"`

	// WorkloadName is the name of the backing kueue.x-k8s.io Workload.
	// +optional
	WorkloadName string `json:"workloadName,omitempty"`

	// Message is a human-readable detail, e.g. the failure reason or the admitted
	// ResourceFlavor, for display in `kubectl get gpuleases` / `describe`.
	// +optional
	Message string `json:"message,omitempty"`

	// AdmittedAt is when the backing Workload's Admitted condition became True.
	// +optional
	AdmittedAt *metav1.Time `json:"admittedAt,omitempty"`

	// BoundAt is when the pod first reported state "hot" for this lease.
	// +optional
	BoundAt *metav1.Time `json:"boundAt,omitempty"`

	// Conditions holds the QuotaReserved, PodActivated and Ready conditions.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="GPU",type=integer,JSONPath=`.spec.gpuID`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.spec.claimRef.name`
// +kubebuilder:printcolumn:name="Deployment",type=string,JSONPath=`.spec.deploymentRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GPULease is the Schema for the gpuleases API. It is cluster-scoped: a GPU is a
// cluster-level resource, and the GPULease's name (<nodeName>-gpu-<gpuID>) is the unique
// key for the (node, gpuID) pair it represents.
type GPULease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GPULeaseSpec   `json:"spec,omitempty"`
	Status GPULeaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GPULeaseList contains a list of GPULease.
type GPULeaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPULease `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GPULease{}, &GPULeaseList{})
}
