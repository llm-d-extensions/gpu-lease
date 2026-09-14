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

package inventory

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	poc "github.com/llm-d-extensions/gpu-lease/api/v1alpha1"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	if err := poc.AddToScheme(scheme); err != nil {
		t.Fatalf("add poc/v1alpha1 to scheme: %v", err)
	}
	return scheme
}

func node(name, count, model string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				poc.NodeLabelGPUCount: count,
				poc.NodeLabelGPUModel: model,
			},
		},
	}
}

func lease(name, node string, gpuID int32) *poc.GPULease {
	return &poc.GPULease{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: poc.GPULeaseSpec{
			NodeName: node,
			GPUID:    gpuID,
			ClaimRef: poc.ObjectRef{Namespace: "ns", Name: "pod"},
		},
	}
}

func TestNodeCapacity(t *testing.T) {
	scheme := newScheme(t)
	c := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		node("worker", "4", "A100"),
	).Build()
	inv := New(c)

	count, model, err := inv.NodeCapacity(context.Background(), "worker")
	if err != nil {
		t.Fatalf("NodeCapacity: %v", err)
	}
	if count != 4 || model != "A100" {
		t.Fatalf("got (%d, %q), want (4, A100)", count, model)
	}
}

func TestNodeCapacityNotFound(t *testing.T) {
	scheme := newScheme(t)
	c := clientfake.NewClientBuilder().WithScheme(scheme).Build()
	inv := New(c)

	if _, _, err := inv.NodeCapacity(context.Background(), "nope"); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("got err=%v, want ErrNodeNotFound", err)
	}
}

func TestNodeCapacityNoLabel(t *testing.T) {
	scheme := newScheme(t)
	c := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker2"}},
	).Build()
	inv := New(c)

	if _, _, err := inv.NodeCapacity(context.Background(), "worker2"); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("got err=%v, want ErrNodeNotFound", err)
	}
}

func TestFreeGPUIDsAndAllocateIDLowestFree(t *testing.T) {
	scheme := newScheme(t)
	c := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		node("worker", "4", "A100"),
		lease("worker-gpu-0", "worker", 0),
		lease("worker-gpu-2", "worker", 2),
	).Build()
	inv := New(c)
	ctx := context.Background()

	free, err := inv.FreeGPUIDs(ctx, "worker")
	if err != nil {
		t.Fatalf("FreeGPUIDs: %v", err)
	}
	if len(free) != 2 || free[0] != 1 || free[1] != 3 {
		t.Fatalf("got %v, want [1 3]", free)
	}

	id, err := inv.AllocateID(ctx, "worker")
	if err != nil {
		t.Fatalf("AllocateID: %v", err)
	}
	if id != 1 {
		t.Fatalf("AllocateID got %d, want 1 (lowest free index, gap-filling)", id)
	}
}

func TestAllocateIDExhausted(t *testing.T) {
	scheme := newScheme(t)
	c := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		node("worker", "2", "A100"),
		lease("worker-gpu-0", "worker", 0),
		lease("worker-gpu-1", "worker", 1),
	).Build()
	inv := New(c)

	if _, err := inv.AllocateID(context.Background(), "worker"); !errors.Is(err, ErrExhausted) {
		t.Fatalf("got err=%v, want ErrExhausted", err)
	}
}

func TestAllocateIDIgnoresOtherNodes(t *testing.T) {
	scheme := newScheme(t)
	c := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		node("worker", "1", "A100"),
		node("worker2", "1", "MI300X"),
		lease("worker2-gpu-0", "worker2", 0),
	).Build()
	inv := New(c)

	id, err := inv.AllocateID(context.Background(), "worker")
	if err != nil {
		t.Fatalf("AllocateID: %v", err)
	}
	if id != 0 {
		t.Fatalf("got %d, want 0 (worker's own gpu-0 is free even though worker2's is taken)", id)
	}
}

func TestNodeFreeCounts(t *testing.T) {
	scheme := newScheme(t)
	c := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		node("cp", "4", "H100"),
		node("worker", "4", "A100"),
		node("worker2", "4", "MI300X"),
		lease("cp-gpu-0", "cp", 0),
		lease("cp-gpu-1", "cp", 1),
		lease("worker-gpu-0", "worker", 0),
	).Build()
	inv := New(c)

	counts, err := inv.NodeFreeCounts(context.Background())
	if err != nil {
		t.Fatalf("NodeFreeCounts: %v", err)
	}
	want := map[string]int32{"cp": 2, "worker": 3, "worker2": 4}
	for k, v := range want {
		if counts[k] != v {
			t.Errorf("counts[%q] = %d, want %d", k, counts[k], v)
		}
	}
}
