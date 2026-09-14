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
	"sync"
	"testing"
	"time"

	"github.com/llm-d-extensions/gpu-lease/internal/controller/podclient"
)

const (
	testWarmup   = 30 * time.Millisecond
	testTeardown = 20 * time.Millisecond
	// testWait must comfortably exceed testWarmup/testTeardown so timers have fired.
	testWait = 300 * time.Millisecond
)

// eventRecorder is a Hook that records every Event it receives, safe for concurrent
// use by the Machine's timer goroutines.
type eventRecorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *eventRecorder) hook(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *eventRecorder) snapshot() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}

func newTestMachine(rec *eventRecorder) *Machine {
	return NewMachine(MachineConfig{WarmupDuration: testWarmup, TeardownDuration: testTeardown}, rec.hook)
}

func TestNewMachine_StartsWarm(t *testing.T) {
	m := NewMachine(MachineConfig{}, nil)
	snap := m.Snapshot()
	if snap.State != StateWarm {
		t.Fatalf("state = %q, want %q", snap.State, StateWarm)
	}
	if snap.GPUID != -1 {
		t.Fatalf("GPUID = %d, want -1", snap.GPUID)
	}
	if m.IsHot() {
		t.Fatal("IsHot() = true for a freshly constructed machine")
	}
}

func TestActivate_NodeMismatch(t *testing.T) {
	rec := &eventRecorder{}
	m := newTestMachine(rec)

	err := m.Activate(podclient.ActivateRequest{LeaseName: "l1", Node: "other-node"}, "own-node")
	if err != ErrNodeMismatch {
		t.Fatalf("err = %v, want ErrNodeMismatch", err)
	}
	if snap := m.Snapshot(); snap.State != StateWarm {
		t.Fatalf("state = %q, want %q (mismatch must not mutate state)", snap.State, StateWarm)
	}
}

func TestActivate_FullCycleToHot(t *testing.T) {
	rec := &eventRecorder{}
	m := newTestMachine(rec)

	if err := m.Activate(podclient.ActivateRequest{LeaseName: "l1", Node: "own-node", GPUID: 3, GPUModel: "H100"}, "own-node"); err != nil {
		t.Fatalf("Activate() err = %v", err)
	}
	if snap := m.Snapshot(); snap.State != StateActivating || snap.LeaseName != "l1" || snap.GPUID != 3 {
		t.Fatalf("snapshot after Activate = %+v", snap)
	}

	time.Sleep(testWait)

	snap := m.Snapshot()
	if snap.State != StateHot {
		t.Fatalf("state after warmup = %q, want %q", snap.State, StateHot)
	}
	if !m.IsHot() {
		t.Fatal("IsHot() = false after warmup completed")
	}

	events := rec.snapshot()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (warm->activating, activating->hot): %+v", len(events), events)
	}
	if events[0].Old != StateWarm || events[0].New != StateActivating {
		t.Fatalf("event[0] = %+v, want warm->activating", events[0])
	}
	if events[1].Old != StateActivating || events[1].New != StateHot {
		t.Fatalf("event[1] = %+v, want activating->hot", events[1])
	}
	if events[1].ActivationDuration <= 0 {
		t.Fatalf("ActivationDuration = %v, want > 0 on the activating->hot transition", events[1].ActivationDuration)
	}
}

func TestActivate_IdempotentSameLease(t *testing.T) {
	rec := &eventRecorder{}
	m := newTestMachine(rec)
	req := podclient.ActivateRequest{LeaseName: "l1", Node: "own-node", GPUID: 1}

	if err := m.Activate(req, "own-node"); err != nil {
		t.Fatalf("first Activate() err = %v", err)
	}
	// Repeat while activating: must be a no-op, not an error, and must not disturb
	// the in-flight warmup timer.
	if err := m.Activate(req, "own-node"); err != nil {
		t.Fatalf("repeat Activate() while activating, err = %v, want nil (idempotent)", err)
	}

	time.Sleep(testWait)
	if snap := m.Snapshot(); snap.State != StateHot {
		t.Fatalf("state = %q, want %q", snap.State, StateHot)
	}

	// Repeat again while hot: still a no-op.
	if err := m.Activate(req, "own-node"); err != nil {
		t.Fatalf("repeat Activate() while hot, err = %v, want nil (idempotent)", err)
	}
	if snap := m.Snapshot(); snap.State != StateHot {
		t.Fatalf("state after repeat while hot = %q, want %q", snap.State, StateHot)
	}
}

func TestActivate_ConflictDifferentLease(t *testing.T) {
	rec := &eventRecorder{}
	m := newTestMachine(rec)

	if err := m.Activate(podclient.ActivateRequest{LeaseName: "l1", Node: "own-node"}, "own-node"); err != nil {
		t.Fatalf("first Activate() err = %v", err)
	}

	err := m.Activate(podclient.ActivateRequest{LeaseName: "l2", Node: "own-node"}, "own-node")
	if err != ErrLeaseConflict {
		t.Fatalf("err = %v, want ErrLeaseConflict", err)
	}

	time.Sleep(testWait)
	// The original lease l1 should have proceeded to hot, unaffected by the
	// rejected l2 request.
	if snap := m.Snapshot(); snap.State != StateHot || snap.LeaseName != "l1" {
		t.Fatalf("snapshot = %+v, want hot/l1", snap)
	}

	// Conflict also applies while hot.
	err = m.Activate(podclient.ActivateRequest{LeaseName: "l2", Node: "own-node"}, "own-node")
	if err != ErrLeaseConflict {
		t.Fatalf("err while hot = %v, want ErrLeaseConflict", err)
	}
}

func TestRelease_WhileWarmIsNoop(t *testing.T) {
	rec := &eventRecorder{}
	m := newTestMachine(rec)

	if err := m.Release(); err != nil {
		t.Fatalf("Release() while warm, err = %v, want nil", err)
	}
	if snap := m.Snapshot(); snap.State != StateWarm {
		t.Fatalf("state = %q, want %q", snap.State, StateWarm)
	}
	if len(rec.snapshot()) != 0 {
		t.Fatalf("got %d events, want 0 (no-op must not fire a transition hook)", len(rec.snapshot()))
	}
}

func TestRelease_DuringWarmupCancelsCleanly(t *testing.T) {
	rec := &eventRecorder{}
	m := newTestMachine(rec)

	if err := m.Activate(podclient.ActivateRequest{LeaseName: "l1", Node: "own-node", GPUID: 5}, "own-node"); err != nil {
		t.Fatalf("Activate() err = %v", err)
	}
	if err := m.Release(); err != nil {
		t.Fatalf("Release() during warmup, err = %v, want nil", err)
	}

	snap := m.Snapshot()
	if snap.State != StateWarm {
		t.Fatalf("state right after Release() during warmup = %q, want %q (immediate, not async)", snap.State, StateWarm)
	}
	if snap.LeaseName != "" || snap.GPUID != -1 {
		t.Fatalf("lease identity not cleared: %+v", snap)
	}

	// Wait past when the (cancelled) warmup timer would have fired, and confirm it
	// did not resurrect the lease (the generation counter must have suppressed it).
	time.Sleep(testWait)
	if snap := m.Snapshot(); snap.State != StateWarm {
		t.Fatalf("state after waiting past the cancelled warmup = %q, want %q (stale timer must no-op)", snap.State, StateWarm)
	}

	events := rec.snapshot()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (warm->activating, activating->warm): %+v", len(events), events)
	}
	if events[1].Old != StateActivating || events[1].New != StateWarm {
		t.Fatalf("event[1] = %+v, want activating->warm", events[1])
	}
	if events[1].ActivationDuration != 0 {
		t.Fatalf("ActivationDuration = %v, want 0 (cancelled activation never reached hot)", events[1].ActivationDuration)
	}
}

func TestRelease_FromHotGoesThroughReleasing(t *testing.T) {
	rec := &eventRecorder{}
	m := newTestMachine(rec)

	if err := m.Activate(podclient.ActivateRequest{LeaseName: "l1", Node: "own-node", GPUID: 2}, "own-node"); err != nil {
		t.Fatalf("Activate() err = %v", err)
	}
	time.Sleep(testWait)
	if snap := m.Snapshot(); snap.State != StateHot {
		t.Fatalf("precondition: state = %q, want %q", snap.State, StateHot)
	}

	if err := m.Release(); err != nil {
		t.Fatalf("Release() while hot, err = %v, want nil", err)
	}
	if snap := m.Snapshot(); snap.State != StateReleasing {
		t.Fatalf("state right after Release() while hot = %q, want %q", snap.State, StateReleasing)
	}

	// A second Release while releasing is a no-op, not an error.
	if err := m.Release(); err != nil {
		t.Fatalf("repeat Release() while releasing, err = %v, want nil", err)
	}

	time.Sleep(testWait)
	snap := m.Snapshot()
	if snap.State != StateWarm {
		t.Fatalf("state after teardown = %q, want %q", snap.State, StateWarm)
	}
	if snap.LeaseName != "" || snap.GPUID != -1 {
		t.Fatalf("lease identity not cleared after teardown: %+v", snap)
	}
}

func TestActivate_RejectedWhileReleasing(t *testing.T) {
	rec := &eventRecorder{}
	m := newTestMachine(rec)

	if err := m.Activate(podclient.ActivateRequest{LeaseName: "l1", Node: "own-node"}, "own-node"); err != nil {
		t.Fatalf("Activate() err = %v", err)
	}
	time.Sleep(testWait)
	if err := m.Release(); err != nil {
		t.Fatalf("Release() err = %v", err)
	}
	if snap := m.Snapshot(); snap.State != StateReleasing {
		t.Fatalf("precondition: state = %q, want %q", snap.State, StateReleasing)
	}

	// Even the *same* leaseName is rejected while tearing down: the pod must settle
	// back to warm first.
	if err := m.Activate(podclient.ActivateRequest{LeaseName: "l1", Node: "own-node"}, "own-node"); err != ErrLeaseConflict {
		t.Fatalf("Activate() while releasing, err = %v, want ErrLeaseConflict", err)
	}

	time.Sleep(testWait)
	if snap := m.Snapshot(); snap.State != StateWarm {
		t.Fatalf("state after teardown completes = %q, want %q", snap.State, StateWarm)
	}
}
