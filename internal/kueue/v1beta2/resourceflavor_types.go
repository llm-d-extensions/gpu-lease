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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ResourceFlavorSpec is the subset of the real kueue.x-k8s.io/v1beta2 ResourceFlavorSpec
// this PoC needs: which node labels select the flavor (design.md §4.1,
// deploy/kueue/resource-flavors.yaml keys every flavor on
// kubernetes.io/hostname). The GPULease reconciler uses this, read-only, to compute
// "which ResourceFlavor is expected for a given node" so it can cross-check the flavor
// Kueue actually assigned (status.admission.podSetAssignments[0].flavors, design.md §4.7)
// without hardcoding deploy/kueue's flavor-naming convention.
type ResourceFlavorSpec struct {
	// NodeLabels are the labels a node must carry for this flavor to be eligible.
	// +optional
	NodeLabels map[string]string `json:"nodeLabels,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster

// ResourceFlavor is a minimal stand-in for kueue.x-k8s.io/v1beta2 ResourceFlavor -- see
// workload_types.go's package doc for why a hand-maintained subset is used instead of the
// real dependency. It carries the real GroupVersionKind (kueue.x-k8s.io/v1beta2, Kind
// ResourceFlavor, cluster-scoped) so a controller-runtime client using this Go type reads
// the real ResourceFlavor objects installed by deploy/kueue/resource-flavors.yaml.
type ResourceFlavor struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ResourceFlavorSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// ResourceFlavorList contains a list of ResourceFlavor.
type ResourceFlavorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourceFlavor `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ResourceFlavor{}, &ResourceFlavorList{})
}
