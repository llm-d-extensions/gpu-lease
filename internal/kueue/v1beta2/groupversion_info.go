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

// Package v1beta2 is a minimal, hand-maintained stand-in for the real
// sigs.k8s.io/kueue/apis/kueue/v1beta2 package.
//
// # Why this exists instead of the real dependency
//
// design.md §5/§7 asks for a real dependency on sigs.k8s.io/kueue @ v0.19.4. That
// dependency was tried (`go get sigs.k8s.io/kueue@v0.19.4`) and works in isolation, but
// its module graph requires controller-runtime v0.24.x, k8s.io/{api,apimachinery,client-go}
// v0.36.x, and — transitively — a **Go 1.26 toolchain**, none of which match this
// repository's pinned versions (controller-runtime v0.20.2, k8s.io/* v0.32.1, Go 1.25.1
// per the task brief). Taking it would force every other package in this module (and
// every phase 2-4 agent) onto that newer toolchain/dependency set just to get one CRD's
// types. That is exactly the "awkward dependency" case design.md's implementers were
// told to route around: define a minimal local type instead.
//
// This package defines only the subset of the real Workload API's wire shape that the
// GPULease reconciler (phase 3) needs: spec.queueName, spec.podSets (name/count/template
// with resource requests and a nodeSelector), status.conditions, and
// status.admission.podSetAssignments[].flavors — i.e. exactly the fields named in
// design.md §4.1, §4.7. It uses the real Group/Version/Kind
// ("kueue.x-k8s.io/v1beta2", Kind "Workload"), so a controller-runtime client using this
// type talks to the *real* Workload CRD installed on the cluster (by the phase 0 spike /
// phase 5 manifests) exactly as if the real Go package had been vendored — JSON
// (de)serialization only cares about the wire shape, not which package defines the Go
// struct. Fields present on the real CRD but not modeled here are simply dropped on
// decode and omitted on encode; this reconciler never needs to round-trip them.
//
// If a future phase needs more of the real API (e.g. workload priority, admission
// checks), either extend this package or revisit taking the real dependency once its
// toolchain requirement lines up with this repo's.
//
// +kubebuilder:object:generate=true
// +groupName=kueue.x-k8s.io
package v1beta2

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is group kueue.x-k8s.io, version v1beta2 — the real Kueue API group and
// version (design.md §3, §4.1). It matches the CRD Kueue itself installs; nothing in
// this package registers or owns that CRD.
var GroupVersion = schema.GroupVersion{Group: "kueue.x-k8s.io", Version: "v1beta2"}

// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme adds the types in this group-version to the given scheme.
var AddToScheme = SchemeBuilder.AddToScheme
