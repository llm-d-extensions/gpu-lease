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

// Package inventory answers "how many GPUs does this node have, which are free, and
// which index should the next lease use" (design.md §4.5). There is a single source of
// truth: node labels poc.llm-d.ai/gpu-count and poc.llm-d.ai/gpu-model, applied by
// hack/setup-nodes.sh, plus the set of GPULease objects that currently exist for a node
// (a GPULease's existence, regardless of its phase, holds its GPUID reserved). This
// package derives everything else from those two sources; it does not cache or maintain
// any additional state of its own.
package inventory

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	poc "github.com/llm-d-extensions/gpu-lease/api/v1alpha1"
)

// ErrNodeNotFound is returned when the requested node does not exist or carries no
// poc.llm-d.ai/gpu-count label (i.e. it has no leasable GPUs at all).
var ErrNodeNotFound = fmt.Errorf("inventory: node not found or has no %s label", poc.NodeLabelGPUCount)

// ErrExhausted is returned by AllocateID when every GPU index on the node is already
// held by an existing GPULease.
var ErrExhausted = fmt.Errorf("inventory: no free GPU id on node")

// Inventory answers questions about per-node GPU capacity, freedom and allocation,
// derived from node labels and existing GPULease objects. Implementations must be safe
// for concurrent use; Inventory itself does not provide mutual exclusion for allocation
// (etcd's AlreadyExists on the derived GPULease name is the actual mutex — see
// poc.LeaseName) so callers must be prepared to retry AllocateID -> create -> retry on a
// race.
type Inventory interface {
	// NodeCapacity returns the leasable GPU count and model for node, read from its
	// poc.llm-d.ai/gpu-count / poc.llm-d.ai/gpu-model labels. Returns ErrNodeNotFound if
	// the node does not exist or has no gpu-count label.
	NodeCapacity(ctx context.Context, node string) (count int32, model string, err error)

	// FreeGPUIDs returns the GPU indices on node, in [0, count), that are not currently
	// held by any GPULease, in ascending order.
	FreeGPUIDs(ctx context.Context, node string) ([]int32, error)

	// AllocateID returns the lowest free GPU index on node. Returns ErrExhausted if
	// none is free. It does not reserve the index; the caller must create the GPULease
	// (named via poc.LeaseName(node, id)) to do that, and must handle AlreadyExists by
	// retrying allocation.
	AllocateID(ctx context.Context, node string) (int32, error)

	// NodeFreeCounts returns, for every node carrying a poc.llm-d.ai/gpu-count label,
	// the number of currently-free GPU indices on it. Used by the WarmPool reconciler
	// to rank promotion candidates by (node free count desc, pod age asc)
	// (design.md §4.6).
	NodeFreeCounts(ctx context.Context) (map[string]int32, error)
}

// clientInventory is the controller-runtime-backed implementation of Inventory.
type clientInventory struct {
	client client.Client
}

// New returns an Inventory backed by c. c must be able to List corev1.Node and
// poc.GPULease (cluster-scoped; no namespace filtering is needed or applied).
func New(c client.Client) Inventory {
	return &clientInventory{client: c}
}

// NodeCapacity implements Inventory.
func (i *clientInventory) NodeCapacity(ctx context.Context, node string) (int32, string, error) {
	var n corev1.Node
	if err := i.client.Get(ctx, client.ObjectKey{Name: node}, &n); err != nil {
		return 0, "", fmt.Errorf("%w: %v", ErrNodeNotFound, err)
	}
	countStr, ok := n.Labels[poc.NodeLabelGPUCount]
	if !ok {
		return 0, "", ErrNodeNotFound
	}
	count, err := strconv.ParseInt(countStr, 10, 32)
	if err != nil || count < 0 {
		return 0, "", fmt.Errorf("inventory: node %s has malformed %s label %q: %v", node, poc.NodeLabelGPUCount, countStr, err)
	}
	model := n.Labels[poc.NodeLabelGPUModel]
	return int32(count), model, nil
}

// usedGPUIDs returns the set of GPU indices currently held by a GPULease on node,
// regardless of the GPULease's phase: existence alone reserves the index. GPULease is
// cluster-scoped and this PoC's lease count is small, so a full list + client-side
// filter is simpler and cheap; callers holding a cached client (the manager's) pay no
// extra API cost since the cache is already warm.
func (i *clientInventory) usedGPUIDs(ctx context.Context, node string) (map[int32]bool, error) {
	var all poc.GPULeaseList
	if err := i.client.List(ctx, &all); err != nil {
		return nil, fmt.Errorf("inventory: list GPULeases: %w", err)
	}
	used := map[int32]bool{}
	for _, l := range all.Items {
		if l.Spec.NodeName == node {
			used[l.Spec.GPUID] = true
		}
	}
	return used, nil
}

// FreeGPUIDs implements Inventory.
func (i *clientInventory) FreeGPUIDs(ctx context.Context, node string) ([]int32, error) {
	count, _, err := i.NodeCapacity(ctx, node)
	if err != nil {
		return nil, err
	}
	used, err := i.usedGPUIDs(ctx, node)
	if err != nil {
		return nil, err
	}
	free := make([]int32, 0, count)
	for id := range count {
		if !used[id] {
			free = append(free, id)
		}
	}
	return free, nil
}

// AllocateID implements Inventory.
func (i *clientInventory) AllocateID(ctx context.Context, node string) (int32, error) {
	free, err := i.FreeGPUIDs(ctx, node)
	if err != nil {
		return 0, err
	}
	if len(free) == 0 {
		return 0, ErrExhausted
	}
	return free[0], nil
}

// NodeFreeCounts implements Inventory.
func (i *clientInventory) NodeFreeCounts(ctx context.Context) (map[string]int32, error) {
	var nodes corev1.NodeList
	if err := i.client.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("inventory: list nodes: %w", err)
	}

	var leases poc.GPULeaseList
	if err := i.client.List(ctx, &leases); err != nil {
		return nil, fmt.Errorf("inventory: list GPULeases: %w", err)
	}
	usedByNode := map[string]map[int32]bool{}
	for _, l := range leases.Items {
		if usedByNode[l.Spec.NodeName] == nil {
			usedByNode[l.Spec.NodeName] = map[int32]bool{}
		}
		usedByNode[l.Spec.NodeName][l.Spec.GPUID] = true
	}

	out := map[string]int32{}
	for _, n := range nodes.Items {
		countStr, ok := n.Labels[poc.NodeLabelGPUCount]
		if !ok {
			continue
		}
		count64, err := strconv.ParseInt(countStr, 10, 32)
		if err != nil || count64 < 0 {
			continue
		}
		used := usedByNode[n.Name]
		free := int32(0)
		for id := range int32(count64) {
			if !used[id] {
				free++
			}
		}
		out[n.Name] = free
	}
	return out, nil
}

var _ Inventory = (*clientInventory)(nil)
