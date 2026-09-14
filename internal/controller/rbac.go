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

// This file exists only to hold +kubebuilder:rbac markers that are not naturally
// attached to a single reconciler's Reconcile method: RBAC needed across both the
// WarmPool and GPULease reconcilers (design.md §4, §5). controller-gen scans every Go
// file under the configured paths for these markers when generating
// config/rbac/role.yaml ("make manifests"), so their physical location doesn't matter;
// keeping them here (rather than editing gpulease_controller.go / warmpool_controller.go,
// which phases 3 and 4 own) avoids merge collisions between phases.
//
// The gpuleases / gpuleases/status / gpuleases/finalizers markers already live on
// GPULeaseReconciler in gpulease_controller.go and are not repeated here.

// Deployments: the WarmPool reconciler watches annotated Deployments but never writes
// them (it writes their pods).
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

// Pods: the WarmPool reconciler labels (state) and annotates (lease, gpu-id, gpu-model,
// pod-deletion-cost) pods, so it needs patch in addition to read access.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch

// Nodes: both reconcilers read node labels (poc.llm-d.ai/gpu-count, gpu-model) for
// inventory; nothing ever writes a node.
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Kueue Workloads: the GPULease reconciler creates a standalone Workload per lease and
// deletes it on release/failure (design.md §4.1, §4.7).
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=workloads,verbs=create;get;list;watch;delete

// Kueue LocalQueues/ClusterQueues/ResourceFlavors: read-only, used to resolve the
// LocalQueue named by gpu-lease.llm-d.ai/local-queue and, for docs/diagnostics, to read
// back quota (design.md §4.5).
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=localqueues;clusterqueues;resourceflavors,verbs=get;list;watch

// Events: both reconcilers emit Events (e.g. NoFreeGPUOnAnyPodNode,
// QuotaExhaustedOnNode) via the manager's EventRecorder.
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
