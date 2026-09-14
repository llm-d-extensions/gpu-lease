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

// Package podclient is the GPULease reconciler's REST client for the workload simulator
// (cmd/workload), whose HTTP contract is defined in design.md §5:
//
//	POST   /lease  {leaseName,node,gpuID,gpuModel} -> 202 Accepted (409 on node mismatch)
//	DELETE /lease  (no body)                        -> 200/204 (idempotent)
//	GET    /state  ->                                {state,leaseName,node,gpuID}
//
// The pod listens on :8000. This package defines the Client interface that the GPULease
// reconciler (phase 3) depends on, a real HTTP implementation, and a FakeClient for
// tests (phase 3/4 unit tests, and this package's own).
package podclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// DefaultPort is the port the workload simulator listens on (design.md §5).
const DefaultPort = 8000

// DefaultTimeout is the per-request timeout used by the HTTP Client implementation.
// Requests are to in-cluster pods over the pod network, so this is deliberately short:
// the reconciler must not block its worker on a hung pod.
const DefaultTimeout = 5 * time.Second

// ActivateRequest is the body of POST /lease. Field names and JSON tags match
// design.md §5 exactly.
type ActivateRequest struct {
	LeaseName string `json:"leaseName"`
	Node      string `json:"node"`
	GPUID     int32  `json:"gpuID"`
	GPUModel  string `json:"gpuModel"`
}

// StateResponse is the body of GET /state. Field names and JSON tags match
// design.md §5 exactly.
type StateResponse struct {
	State     string `json:"state"`
	LeaseName string `json:"leaseName"`
	Node      string `json:"node"`
	GPUID     int32  `json:"gpuID"`
}

// Pod states reported in StateResponse.State (design.md §5).
const (
	StateWarm       = "warm"
	StateActivating = "activating"
	StateHot        = "hot"
)

// ErrNodeMismatch is returned by Activate when the pod replies 409 Conflict, i.e. the
// pod's own NODE_NAME does not match ActivateRequest.Node. Callers should treat this as
// the ReasonNodeMismatch failure (design.md §4.7).
var ErrNodeMismatch = fmt.Errorf("podclient: node mismatch (409)")

// Client is the GPULease reconciler's view of a workload pod's lease API. Implementations
// must be safe for concurrent use and must respect ctx cancellation/deadlines.
type Client interface {
	// Activate calls POST /lease on the pod at podIP. It returns nil on 202 Accepted,
	// ErrNodeMismatch on 409, and a non-nil error for any other failure (including
	// network errors, which the reconciler maps to ReasonPodUnreachable).
	Activate(ctx context.Context, podIP string, req ActivateRequest) error

	// Release calls DELETE /lease on the pod at podIP. It is idempotent: calling it on
	// an already-warm pod must not error.
	Release(ctx context.Context, podIP string) error

	// State calls GET /state on the pod at podIP and returns the decoded response.
	State(ctx context.Context, podIP string) (StateResponse, error)
}

// HTTPClient is the real implementation of Client, talking to the workload simulator's
// REST API over plain HTTP on DefaultPort.
type HTTPClient struct {
	// HTTPClient is the underlying client. If nil, a client with DefaultTimeout is used.
	HTTPClient *http.Client
	// Port is the pod port to call. Defaults to DefaultPort if zero.
	Port int
}

// NewHTTPClient returns an HTTPClient configured with DefaultTimeout and DefaultPort.
func NewHTTPClient() *HTTPClient {
	return &HTTPClient{
		HTTPClient: &http.Client{Timeout: DefaultTimeout},
		Port:       DefaultPort,
	}
}

func (c *HTTPClient) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: DefaultTimeout}
}

func (c *HTTPClient) port() int {
	if c.Port != 0 {
		return c.Port
	}
	return DefaultPort
}

func (c *HTTPClient) baseURL(podIP string) string {
	return fmt.Sprintf("http://%s:%d", podIP, c.port())
}

// Activate implements Client.
func (c *HTTPClient) Activate(ctx context.Context, podIP string, req ActivateRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("podclient: marshal ActivateRequest: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL(podIP)+"/lease", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("podclient: build POST /lease request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.client().Do(httpReq)
	if err != nil {
		return fmt.Errorf("podclient: POST /lease to %s: %w", podIP, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusOK:
		return nil
	case http.StatusConflict:
		return ErrNodeMismatch
	default:
		return fmt.Errorf("podclient: POST /lease to %s: unexpected status %d", podIP, resp.StatusCode)
	}
}

// Release implements Client.
func (c *HTTPClient) Release(ctx context.Context, podIP string) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL(podIP)+"/lease", nil)
	if err != nil {
		return fmt.Errorf("podclient: build DELETE /lease request: %w", err)
	}

	resp, err := c.client().Do(httpReq)
	if err != nil {
		return fmt.Errorf("podclient: DELETE /lease to %s: %w", podIP, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("podclient: DELETE /lease to %s: unexpected status %d", podIP, resp.StatusCode)
	}
	return nil
}

// State implements Client.
func (c *HTTPClient) State(ctx context.Context, podIP string) (StateResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL(podIP)+"/state", nil)
	if err != nil {
		return StateResponse{}, fmt.Errorf("podclient: build GET /state request: %w", err)
	}

	resp, err := c.client().Do(httpReq)
	if err != nil {
		return StateResponse{}, fmt.Errorf("podclient: GET /state to %s: %w", podIP, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return StateResponse{}, fmt.Errorf("podclient: GET /state to %s: unexpected status %d", podIP, resp.StatusCode)
	}

	var out StateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return StateResponse{}, fmt.Errorf("podclient: decode /state response from %s: %w", podIP, err)
	}
	return out, nil
}

// FakeClient is an in-memory Client for unit tests. It tracks state per podIP so tests
// can simulate multiple pods, and lets tests inject errors/behaviour per pod.
type FakeClient struct {
	mu sync.Mutex

	// pods maps podIP -> current fake state ("" is treated as StateWarm).
	pods map[string]string

	// ActivateErr, when non-nil, is returned by Activate for the given podIP (and the
	// state is left unchanged). Populate to simulate ErrNodeMismatch or a network error.
	ActivateErr map[string]error
	// ReleaseErr, when non-nil, is returned by Release for the given podIP.
	ReleaseErr map[string]error
	// StateErr, when non-nil, is returned by State for the given podIP.
	StateErr map[string]error

	// ActivateCalls and ReleaseCalls record calls made, for test assertions.
	ActivateCalls []ActivateRequestCall
	ReleaseCalls  []string
}

// ActivateRequestCall records one Activate call for test assertions.
type ActivateRequestCall struct {
	PodIP   string
	Request ActivateRequest
}

// NewFakeClient returns an empty FakeClient; every pod starts "warm" implicitly.
func NewFakeClient() *FakeClient {
	return &FakeClient{
		pods:        map[string]string{},
		ActivateErr: map[string]error{},
		ReleaseErr:  map[string]error{},
		StateErr:    map[string]error{},
	}
}

// SetState forces the fake state reported for podIP, for test setup.
func (f *FakeClient) SetState(podIP, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pods[podIP] = state
}

// Activate implements Client. On success it immediately transitions the pod to
// StateHot (tests that need to observe the intermediate "activating" state should call
// SetState themselves between Activate and State).
func (f *FakeClient) Activate(_ context.Context, podIP string, req ActivateRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ActivateCalls = append(f.ActivateCalls, ActivateRequestCall{PodIP: podIP, Request: req})

	if err, ok := f.ActivateErr[podIP]; ok && err != nil {
		return err
	}
	f.pods[podIP] = StateHot
	return nil
}

// Release implements Client.
func (f *FakeClient) Release(_ context.Context, podIP string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ReleaseCalls = append(f.ReleaseCalls, podIP)

	if err, ok := f.ReleaseErr[podIP]; ok && err != nil {
		return err
	}
	f.pods[podIP] = StateWarm
	return nil
}

// State implements Client.
func (f *FakeClient) State(_ context.Context, podIP string) (StateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err, ok := f.StateErr[podIP]; ok && err != nil {
		return StateResponse{}, err
	}
	state := f.pods[podIP]
	if state == "" {
		state = StateWarm
	}
	return StateResponse{State: state}, nil
}

var _ Client = (*HTTPClient)(nil)
var _ Client = (*FakeClient)(nil)
