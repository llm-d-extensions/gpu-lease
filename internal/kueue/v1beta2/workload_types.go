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

package v1beta2

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionAdmitted is the Workload condition type the GPULease reconciler polls for
// (design.md §4.7: "wait for Workload condition Admitted=True"). This mirrors the real
// Kueue API's kueue.WorkloadCondition type name; confirm against the phase 0 spike's
// captured JSON (docs/kueue-spike.md) before relying on it, per design.md's note that
// "docs were inconclusive on condition names; the cluster is the source of truth."
const ConditionAdmitted = "Admitted"

// ConditionQuotaReserved mirrors Kueue's own "QuotaReserved" Workload condition, set
// before Admitted in newer Kueue versions. Not required by the design's state machine
// (which only waits on Admitted) but kept for diagnostics.
const ConditionQuotaReserved = "QuotaReserved"

// PodSetName is the name the GPULease reconciler must use for its single podSet, so
// status.admission.podSetAssignments[0] unambiguously corresponds to it.
const PodSetName = "main"

// WorkloadSpec is the subset of the real kueue.x-k8s.io/v1beta2 WorkloadSpec this PoC
// needs: which LocalQueue to charge, and the single podSet describing the GPU request
// and node pinning (design.md §4.1).
type WorkloadSpec struct {
	// QueueName is the LocalQueue this Workload is submitted to.
	QueueName string `json:"queueName,omitempty"`

	// PodSets has exactly one entry for this PoC: a single logical pod requesting
	// poc.llm-d.ai/gpu: 1 with a nodeSelector pinning it to the lease's node.
	PodSets []PodSet `json:"podSets,omitempty"`

	// PriorityClassName is unused by this PoC; kept for wire-compatibility with the
	// real CRD's defaulting/validation webhooks, which may expect the field to exist.
	PriorityClassName string `json:"priorityClassName,omitempty"`
}

// PodSet mirrors kueue.x-k8s.io/v1beta2 PodSet, trimmed to the fields this PoC sets.
type PodSet struct {
	// Name identifies the podSet within the Workload; use PodSetName.
	Name string `json:"name"`
	// Count is the number of pods described by Template; always 1 for a lease.
	Count int32 `json:"count"`
	// Template describes the resource request (poc.llm-d.ai/gpu: 1) and nodeSelector
	// (kubernetes.io/hostname: <node>) that select the eligible ResourceFlavor
	// (design.md §4.1). No pod is ever actually created from it.
	Template corev1.PodTemplateSpec `json:"template"`
}

// WorkloadStatus is the subset of the real WorkloadStatus this PoC reads: the
// Admitted/QuotaReserved conditions and, once admitted, which flavor/node was assigned.
type WorkloadStatus struct {
	// Conditions includes ConditionAdmitted, set True once Kueue has reserved quota.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Admission is set once Kueue has admitted the Workload.
	// +optional
	Admission *Admission `json:"admission,omitempty"`
}

// Admission mirrors kueue.x-k8s.io/v1beta2 Admission, trimmed to what design.md §4.7
// needs recorded back onto the GPULease's status message: the assigned flavor per
// resource, read from podSetAssignments[0].flavors.
type Admission struct {
	// ClusterQueue is the ClusterQueue that admitted the Workload.
	ClusterQueue string `json:"clusterQueue,omitempty"`
	// PodSetAssignments has one entry per WorkloadSpec.PodSets entry, in the same
	// order; for this PoC, exactly one (PodSetName).
	PodSetAssignments []PodSetAssignment `json:"podSetAssignments,omitempty"`
}

// PodSetAssignment mirrors kueue.x-k8s.io/v1beta2 PodSetAssignment, trimmed to Flavors:
// the map from requested resource name (poc.llm-d.ai/gpu) to the ResourceFlavor name
// Kueue assigned. Reading this back proves the lease landed on the intended node
// (design.md §4.7).
type PodSetAssignment struct {
	Name    string                         `json:"name,omitempty"`
	Flavors map[corev1.ResourceName]string `json:"flavors,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Workload is a minimal stand-in for kueue.x-k8s.io/v1beta2 Workload — see this
// package's doc comment for why. It carries the real GroupVersionKind
// (kueue.x-k8s.io/v1beta2, Kind Workload) so a controller-runtime client using this Go
// type operates against the real CRD on the cluster.
type Workload struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkloadSpec   `json:"spec,omitempty"`
	Status WorkloadStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkloadList contains a list of Workload.
type WorkloadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workload `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Workload{}, &WorkloadList{})
}
