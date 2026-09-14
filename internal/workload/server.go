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

// Package workload implements the GPU workload simulator's HTTP server and
// supporting state machine, described end-to-end in design.md §5. It is consumed by
// cmd/workload (the binary) and satisfies, byte-for-byte, the wire contract that
// internal/controller/podclient's Client interface expects of a pod at
// podclient.DefaultPort.
package workload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/llm-d-extensions/gpu-lease/internal/controller/podclient"
)

// Config is the effective, defaulted configuration for one workload simulator pod,
// read from the environment by cmd/workload/main.go (design.md §5).
type Config struct {
	// NodeName is this pod's spec.nodeName, via the downward API (env NODE_NAME).
	// Required: a lease request's node is checked against this.
	NodeName string
	// PodName is this pod's metadata.name, via the downward API (env POD_NAME).
	PodName string
	// PodNamespace is this pod's metadata.namespace, via the downward API (env
	// POD_NAMESPACE). Required: it scopes the demand ConfigMap watch.
	PodNamespace string
	// DeploymentName is this pod's owning Deployment name (env DEPLOYMENT_NAME,
	// literal in the pod spec -- there is no downward-API field for it).
	DeploymentName string
	// PodCapacityRPS is this pod's serving capacity while hot (env
	// POD_CAPACITY_RPS, default 10 -- api/v1alpha1.DefaultPodCapacityRPS).
	PodCapacityRPS int32
	// WarmupDuration is how long POST /lease takes to reach state hot (env
	// WARMUP_SECONDS, default DefaultWarmupDuration).
	WarmupDuration time.Duration
	// TeardownDuration is how long DELETE /lease takes to reach state warm (env
	// TEARDOWN_SECONDS, default DefaultTeardownDuration).
	TeardownDuration time.Duration
	// DemandConfigMapName is the ConfigMap watched for demand (env
	// DEMAND_CONFIGMAP, default DefaultDemandConfigMapName).
	DemandConfigMapName string
}

// Server is the workload simulator's HTTP server (design.md §5): POST/DELETE /lease,
// GET /state, POST /infer, GET /healthz, GET /readyz, GET /metrics, listening on
// podclient.DefaultPort.
type Server struct {
	cfg     Config
	logger  *slog.Logger
	machine *Machine
	metrics *Metrics
	demand  *DemandWatcher // nil if no k8s client was available (e.g. local dev)

	httpServer *http.Server
}

// NewServer wires up the Machine, Metrics and (optional) DemandWatcher and builds the
// HTTP handler. k8sClient may be nil (e.g. running outside a cluster for local dev):
// the server still runs, just never observes demand changes and
// gpulease_demand_rps stays at its initial 0 (main.go logs why, if it happens).
func NewServer(cfg Config, logger *slog.Logger, k8sClient kubernetes.Interface) *Server {
	if logger == nil {
		logger = slog.Default()
	}

	metrics := NewMetrics(cfg.DeploymentName, cfg.PodName, cfg.NodeName)

	s := &Server{
		cfg:     cfg,
		logger:  logger,
		metrics: metrics,
	}

	if k8sClient != nil {
		s.demand = NewDemandWatcher(k8sClient, cfg.PodNamespace, cfg.DemandConfigMapName, cfg.DeploymentName, metrics.SetDemandRPS, logger)
	}

	s.machine = NewMachine(MachineConfig{
		WarmupDuration:   cfg.WarmupDuration,
		TeardownDuration: cfg.TeardownDuration,
	}, s.onTransition)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /lease", s.handleActivate)
	mux.HandleFunc("DELETE /lease", s.handleRelease)
	mux.HandleFunc("GET /state", s.handleState)
	mux.HandleFunc("POST /infer", s.handleInfer)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleHealthz)
	mux.Handle("GET /metrics", metrics.Handler())

	s.httpServer = &http.Server{
		Addr:              fmt.Sprintf(":%d", podclient.DefaultPort),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	return s
}

// onTransition is the Machine's Hook: it feeds every state change into the metrics
// gauges/histogram (design.md §5).
func (s *Server) onTransition(ev Event) {
	s.metrics.ApplyEvent(ev, s.cfg.PodCapacityRPS)
	s.logger.Info("pod state transition", "old", ev.Old, "new", ev.New, "leaseName", ev.Snapshot.LeaseName, "gpuID", ev.Snapshot.GPUID)
}

// ListenAndServe starts the HTTP server and, if a DemandWatcher is configured, its
// watch loop. It blocks until ctx is cancelled or the server errors, then shuts both
// down gracefully. This is the method cmd/workload/main.go calls.
func (s *Server) ListenAndServe(ctx context.Context) error {
	demandCtx, cancelDemand := context.WithCancel(ctx)
	defer cancelDemand()

	if s.demand != nil {
		go s.demand.Run(demandCtx)
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("workload simulator listening", "addr", s.httpServer.Addr)
		if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) handleActivate(w http.ResponseWriter, r *http.Request) {
	var req podclient.ActivateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	err := s.machine.Activate(req, s.cfg.NodeName)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusAccepted)
	case errors.Is(err, ErrNodeMismatch):
		http.Error(w, fmt.Sprintf("node mismatch: pod is on %q, lease is for %q", s.cfg.NodeName, req.Node), http.StatusConflict)
	case errors.Is(err, ErrLeaseConflict):
		http.Error(w, fmt.Sprintf("lease conflict: pod is already committed to lease %q", s.machine.Snapshot().LeaseName), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleRelease(w http.ResponseWriter, _ *http.Request) {
	// Machine.Release never errors: DELETE /lease is unconditionally idempotent
	// (design.md §5).
	_ = s.machine.Release()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	snap := s.machine.Snapshot()
	resp := podclient.StateResponse{
		State:     snap.State,
		LeaseName: snap.LeaseName,
		Node:      snap.Node,
		GPUID:     snap.GPUID,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// simulatedInferLatency is the fixed "inference" latency /infer sleeps for when hot.
// Determinism over realism (design.md §5's demand-injection rationale applies here
// too): a small fixed jitter is enough to look alive without making the demo timing
// unpredictable.
const simulatedInferLatencyBase = 20 * time.Millisecond

func (s *Server) handleInfer(w http.ResponseWriter, r *http.Request) {
	if !s.machine.IsHot() {
		s.metrics.ObserveRequest(http.StatusServiceUnavailable)
		http.Error(w, "no GPU lease", http.StatusServiceUnavailable)
		return
	}

	jitter := time.Duration(rand.IntN(20)) * time.Millisecond // #nosec G404 -- simulated latency only
	select {
	case <-time.After(simulatedInferLatencyBase + jitter):
	case <-r.Context().Done():
		return
	}

	s.metrics.ObserveRequest(http.StatusOK)
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok"))
}

// handleHealthz backs both GET /healthz and GET /readyz. It must always return 200,
// including while warm: warm pods must be Ready or KEDA's replica math breaks
// (design.md §5).
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// Addr returns the address the HTTP server listens on (mainly for tests using an
// ephemeral listener via httptest, which don't go through ListenAndServe).
func (s *Server) Addr() string {
	return s.httpServer.Addr
}

// Handler exposes the underlying http.Handler directly, for tests that want to drive
// it with httptest.NewServer/httptest.NewRequest without binding a real socket.
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}
