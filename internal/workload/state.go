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
	"errors"
	"sync"
	"time"

	pocv1alpha1 "github.com/llm-d-extensions/gpu-lease/api/v1alpha1"
	"github.com/llm-d-extensions/gpu-lease/internal/controller/podclient"
)

// The pod's lifecycle state machine (design.md §5): warm -> activating -> hot ->
// (releasing) -> warm. State values reuse the shared vocabulary in
// api/v1alpha1/names.go rather than redeclaring the strings; they are identical to
// podclient's StateWarm/StateActivating/StateHot wire values by construction (both
// packages describe the same lifecycle, design.md §4.4 and §5).
const (
	StateWarm       = pocv1alpha1.StateWarm
	StateActivating = pocv1alpha1.StateActivating
	StateHot        = pocv1alpha1.StateHot
	// StateReleasing is a transient, internal-only state entered while tearing down a
	// hot pod (DELETE /lease -> TEARDOWN_SECONDS -> warm). design.md §5's GET /state
	// table enumerates warm|activating|hot as the pod's steady/settling states; this
	// value may be observed too, briefly, via GET /state during teardown. This is a
	// superset of the documented three values, not a contradiction of them: the
	// GPULease reconciler (design.md §4.7) only ever polls for state=="hot" during
	// Activating, and never polls GET /state during its own Releasing phase, so the
	// extra value is harmless to the documented contract.
	StateReleasing = pocv1alpha1.StateReleasing
)

// ErrNodeMismatch is returned by Activate when the request's Node does not match this
// pod's own NODE_NAME (design.md §5). The HTTP layer (server.go) maps this to 409,
// matching podclient.ErrNodeMismatch's expectation on the controller side.
var ErrNodeMismatch = errors.New("workload: lease request node does not match this pod's NODE_NAME")

// ErrLeaseConflict is returned by Activate when the pod already holds (or is
// activating/releasing) a *different* lease than the one requested. The HTTP layer
// maps this to 409 as well: podclient.HTTPClient.Activate only distinguishes 409 from
// "any other error", so both failure modes are wire-compatible.
var ErrLeaseConflict = errors.New("workload: pod is already committed to a different lease")

// Snapshot is a point-in-time, immutable copy of the Machine's state, safe to read
// without holding any lock.
type Snapshot struct {
	State     string
	LeaseName string
	Node      string
	GPUID     int32
	GPUModel  string
}

// Event describes one state transition, delivered to a Hook after the Machine's
// internal lock has been released (so hooks may safely call back into the Machine).
type Event struct {
	Old, New string
	Snapshot Snapshot
	// ActivationDuration is the warm-up-request-to-hot latency, set only on the
	// activating->hot transition reached via a completed warmup timer (zero
	// otherwise). Feeds gpulease_activation_seconds (design.md §5).
	ActivationDuration time.Duration
}

// Hook is called by the Machine after every state transition. Implementations must
// not block: the Machine calls hooks synchronously from its own timer goroutines.
type Hook func(Event)

// MachineConfig configures timer durations. Zero/negative values fall back to
// design.md §5's documented defaults.
type MachineConfig struct {
	WarmupDuration   time.Duration // default 10s (WARMUP_SECONDS)
	TeardownDuration time.Duration // default 2s (TEARDOWN_SECONDS)
}

// DefaultWarmupDuration and DefaultTeardownDuration are design.md §5's documented
// defaults for WARMUP_SECONDS and TEARDOWN_SECONDS.
const (
	DefaultWarmupDuration   = 10 * time.Second
	DefaultTeardownDuration = 2 * time.Second
)

// Machine is the concurrency-safe pod lifecycle state machine described in
// design.md §5. Zero value is not usable; construct with NewMachine.
//
// Activation and teardown run asynchronously on timers, guarded by a monotonically
// increasing generation counter: every timer captures the generation current at the
// moment it was armed, and only acts if that generation is still current when it
// fires. This makes cancellation (e.g. a release arriving mid-warmup) race-free
// without needing the timer goroutine to synchronize with whoever cancels it beyond
// the shared mutex.
type Machine struct {
	mu sync.Mutex

	state     string
	leaseName string
	node      string
	gpuID     int32
	gpuModel  string

	activationStart time.Time
	gen             uint64

	warmup   time.Duration
	teardown time.Duration

	hook Hook
}

// NewMachine returns a Machine starting in StateWarm, per design.md §4.4 ("The
// Deployment's pod template must ship with gpu-lease.llm-d.ai/state: warm so every
// new replica is born warm").
func NewMachine(cfg MachineConfig, hook Hook) *Machine {
	warmup := cfg.WarmupDuration
	if warmup <= 0 {
		warmup = DefaultWarmupDuration
	}
	teardown := cfg.TeardownDuration
	if teardown <= 0 {
		teardown = DefaultTeardownDuration
	}
	if hook == nil {
		hook = func(Event) {}
	}
	return &Machine{
		state:    StateWarm,
		gpuID:    -1, // no GPU held while warm; -1 is the "unassigned" sentinel.
		warmup:   warmup,
		teardown: teardown,
		hook:     hook,
	}
}

// Snapshot returns a copy of the Machine's current state.
func (m *Machine) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

func (m *Machine) snapshotLocked() Snapshot {
	return Snapshot{
		State:     m.state,
		LeaseName: m.leaseName,
		Node:      m.node,
		GPUID:     m.gpuID,
		GPUModel:  m.gpuModel,
	}
}

// IsHot reports whether the Machine is currently in StateHot. Convenience for the
// /infer handler (design.md §5).
func (m *Machine) IsHot() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state == StateHot
}

// Activate implements POST /lease (design.md §5):
//
//   - node != own NODE_NAME (req.Node)               -> ErrNodeMismatch (409)
//   - same leaseName while activating or hot         -> nil, no-op (idempotent)
//   - different leaseName while activating/hot/releasing -> ErrLeaseConflict (409)
//   - otherwise (warm)                                -> start async warmup -> nil (202)
//
// req.Node is checked against ownNode *before* any lease-state logic: a lease meant
// for a different node is rejected outright regardless of what this pod is doing.
func (m *Machine) Activate(req podclient.ActivateRequest, ownNode string) error {
	if req.Node != ownNode {
		return ErrNodeMismatch
	}

	m.mu.Lock()

	switch m.state {
	case StateActivating, StateHot:
		if req.LeaseName == m.leaseName {
			m.mu.Unlock()
			return nil // idempotent no-op: same lease, already in flight or bound.
		}
		m.mu.Unlock()
		return ErrLeaseConflict
	case StateReleasing:
		// Busy tearing down a previous lease; refuse any new lease (same name or
		// not) until we're back to warm. The caller (GPULease reconciler) will
		// simply retry.
		m.mu.Unlock()
		return ErrLeaseConflict
	}

	// state == StateWarm: begin async warmup.
	m.gen++
	gen := m.gen
	m.leaseName = req.LeaseName
	m.node = req.Node
	m.gpuID = req.GPUID
	m.gpuModel = req.GPUModel
	m.activationStart = time.Now()
	old := m.state
	m.state = StateActivating
	snap := m.snapshotLocked()
	warmup := m.warmup
	m.mu.Unlock()

	m.hook(Event{Old: old, New: StateActivating, Snapshot: snap})

	time.AfterFunc(warmup, func() { m.completeWarmup(gen) })
	return nil
}

// completeWarmup fires when a warmup timer expires. It is a no-op if gen is stale
// (the activation was cancelled by a Release, or superseded) -- see Machine's doc
// comment on the generation counter.
func (m *Machine) completeWarmup(gen uint64) {
	m.mu.Lock()
	if m.gen != gen || m.state != StateActivating {
		m.mu.Unlock()
		return
	}
	old := m.state
	m.state = StateHot
	duration := time.Since(m.activationStart)
	snap := m.snapshotLocked()
	m.mu.Unlock()

	m.hook(Event{Old: old, New: StateHot, Snapshot: snap, ActivationDuration: duration})
}

// Release implements DELETE /lease (design.md §5). It is always idempotent and
// always succeeds:
//
//   - warm      -> no-op (already released)
//   - activating -> cancel the in-flight warmup cleanly, go straight back to warm
//     (nothing was ever actually activated, so there is nothing to tear down)
//   - hot       -> start async teardown -> warm
//   - releasing -> no-op (a teardown is already in flight)
func (m *Machine) Release() error {
	m.mu.Lock()

	switch m.state {
	case StateWarm, StateReleasing:
		m.mu.Unlock()
		return nil
	case StateActivating:
		// Cancel cleanly: bump gen so the pending completeWarmup no-ops, and go
		// straight to warm since nothing was ever actually activated.
		m.gen++
		old := m.state
		m.clearLeaseLocked()
		m.state = StateWarm
		snap := m.snapshotLocked()
		m.mu.Unlock()
		m.hook(Event{Old: old, New: StateWarm, Snapshot: snap})
		return nil
	}

	// state == StateHot: start async teardown.
	m.gen++
	gen := m.gen
	old := m.state
	m.state = StateReleasing
	snap := m.snapshotLocked()
	teardown := m.teardown
	m.mu.Unlock()

	m.hook(Event{Old: old, New: StateReleasing, Snapshot: snap})

	time.AfterFunc(teardown, func() { m.completeTeardown(gen) })
	return nil
}

// completeTeardown fires when a teardown timer expires. No-op if gen is stale.
func (m *Machine) completeTeardown(gen uint64) {
	m.mu.Lock()
	if m.gen != gen || m.state != StateReleasing {
		m.mu.Unlock()
		return
	}
	old := m.state
	m.clearLeaseLocked()
	m.state = StateWarm
	snap := m.snapshotLocked()
	m.mu.Unlock()

	m.hook(Event{Old: old, New: StateWarm, Snapshot: snap})
}

// clearLeaseLocked resets lease identity fields to their "no lease held" values.
// Callers must hold m.mu.
func (m *Machine) clearLeaseLocked() {
	m.leaseName = ""
	m.node = ""
	m.gpuID = -1
	m.gpuModel = ""
}
