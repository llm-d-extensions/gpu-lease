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

package workload

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const demandTestNamespace = "gpu-lease-poc"

// updateRecorder collects every value pushed via DemandWatcher's onUpdate callback,
// safe for concurrent use by the informer's goroutine.
type updateRecorder struct {
	mu     sync.Mutex
	values []float64
}

func (r *updateRecorder) record(v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values = append(r.values, v)
}

func (r *updateRecorder) last() (float64, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.values) == 0 {
		return 0, 0
	}
	return r.values[len(r.values)-1], len(r.values)
}

// waitForCount blocks until r has recorded at least n values, or fails the test after
// a short timeout.
func waitForCount(t *testing.T, r *updateRecorder, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, got := r.last(); got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, got := r.last()
	t.Fatalf("timed out waiting for %d recorded updates, got %d", n, got)
}

func TestDemandWatcher_ConfigMapAbsentAtStartup(t *testing.T) {
	client := fake.NewClientset()
	rec := &updateRecorder{}
	w := NewDemandWatcher(client, demandTestNamespace, DefaultDemandConfigMapName, "app-a", rec.record, nil)

	if v := w.Value(); v != 0 {
		t.Fatalf("Value() before Run() = %v, want 0", v)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// Give the informer's initial list a moment to complete; there is nothing to
	// list, so no update is ever pushed, and Value() must stay at the 0 fallback.
	time.Sleep(100 * time.Millisecond)
	if v := w.Value(); v != 0 {
		t.Fatalf("Value() with no ConfigMap ever created = %v, want 0", v)
	}
}

func TestDemandWatcher_KeyPresentAndUpdated(t *testing.T) {
	client := fake.NewClientset()
	rec := &updateRecorder{}
	w := NewDemandWatcher(client, demandTestNamespace, DefaultDemandConfigMapName, "app-a", rec.record, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultDemandConfigMapName, Namespace: demandTestNamespace},
		Data:       map[string]string{"app-a": "20", "app-b": "5"},
	}
	if _, err := client.CoreV1().ConfigMaps(demandTestNamespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create() err = %v", err)
	}

	waitForCount(t, rec, 1)
	if v := w.Value(); v != 20 {
		t.Fatalf("Value() = %v, want 20", v)
	}

	cm.Data["app-a"] = "35"
	if _, err := client.CoreV1().ConfigMaps(demandTestNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() err = %v", err)
	}

	waitForCount(t, rec, 2)
	if v := w.Value(); v != 35 {
		t.Fatalf("Value() after update = %v, want 35", v)
	}
}

func TestDemandWatcher_KeyAbsentFallsBack(t *testing.T) {
	client := fake.NewClientset()
	rec := &updateRecorder{}
	// Watching deployment "app-a", but the ConfigMap only carries "app-b".
	w := NewDemandWatcher(client, demandTestNamespace, DefaultDemandConfigMapName, "app-a", rec.record, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultDemandConfigMapName, Namespace: demandTestNamespace},
		Data:       map[string]string{"app-b": "5"},
	}
	if _, err := client.CoreV1().ConfigMaps(demandTestNamespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create() err = %v", err)
	}

	waitForCount(t, rec, 1)
	if v := w.Value(); v != 0 {
		t.Fatalf("Value() with missing key = %v, want 0 (fallback, never had a good value)", v)
	}
}

func TestDemandWatcher_UnparseableValueFallsBackToLastGood(t *testing.T) {
	client := fake.NewClientset()
	rec := &updateRecorder{}
	w := NewDemandWatcher(client, demandTestNamespace, DefaultDemandConfigMapName, "app-a", rec.record, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultDemandConfigMapName, Namespace: demandTestNamespace},
		Data:       map[string]string{"app-a": "20"},
	}
	if _, err := client.CoreV1().ConfigMaps(demandTestNamespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create() err = %v", err)
	}
	waitForCount(t, rec, 1)
	if v := w.Value(); v != 20 {
		t.Fatalf("Value() = %v, want 20", v)
	}

	cm.Data["app-a"] = "not-a-number"
	if _, err := client.CoreV1().ConfigMaps(demandTestNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() err = %v", err)
	}

	waitForCount(t, rec, 2)
	if v := w.Value(); v != 20 {
		t.Fatalf("Value() after unparseable update = %v, want 20 (fallback to last good)", v)
	}
}

func TestDemandWatcher_DefaultConfigMapName(t *testing.T) {
	client := fake.NewClientset()
	w := NewDemandWatcher(client, demandTestNamespace, "", "app-a", nil, nil)
	if w.cmName != DefaultDemandConfigMapName {
		t.Fatalf("cmName = %q, want default %q", w.cmName, DefaultDemandConfigMapName)
	}
}
