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
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/llm-d-extensions/gpu-lease/internal/controller/podclient"
)

func newTestServer() *Server {
	return NewServer(Config{
		NodeName:         "own-node",
		PodName:          "app-a-0",
		PodNamespace:     "gpu-lease-poc",
		DeploymentName:   "app-a",
		PodCapacityRPS:   10,
		WarmupDuration:   30 * time.Millisecond,
		TeardownDuration: 20 * time.Millisecond,
	}, nil, nil) // nil k8sClient: no DemandWatcher, exercised elsewhere.
}

func doJSON(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("json.Marshal() err = %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestServer_HealthzAlwaysOK(t *testing.T) {
	s := newTestServer()
	for _, path := range []string{"/healthz", "/readyz"} {
		rec := doJSON(t, s.Handler(), http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
	}
}

func TestServer_StateStartsWarm(t *testing.T) {
	s := newTestServer()
	rec := doJSON(t, s.Handler(), http.MethodGet, "/state", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /state = %d, want 200", rec.Code)
	}
	var resp podclient.StateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal() err = %v, body = %s", err, rec.Body.String())
	}
	if resp.State != StateWarm {
		t.Fatalf("State = %q, want %q", resp.State, StateWarm)
	}
}

func TestServer_LeaseNodeMismatchIs409(t *testing.T) {
	s := newTestServer()
	rec := doJSON(t, s.Handler(), http.MethodPost, "/lease", podclient.ActivateRequest{
		LeaseName: "l1", Node: "some-other-node", GPUID: 0, GPUModel: "H100",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("POST /lease with mismatched node = %d, want 409", rec.Code)
	}
}

func TestServer_LeaseAcceptedThenHotThenInferOK(t *testing.T) {
	s := newTestServer()

	rec := doJSON(t, s.Handler(), http.MethodPost, "/lease", podclient.ActivateRequest{
		LeaseName: "l1", Node: "own-node", GPUID: 4, GPUModel: "H100",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /lease = %d, want 202", rec.Code)
	}

	// While still warming up, /infer must refuse (no GPU lease yet).
	rec = doJSON(t, s.Handler(), http.MethodPost, "/infer", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /infer while activating = %d, want 503", rec.Code)
	}

	time.Sleep(300 * time.Millisecond)

	rec = doJSON(t, s.Handler(), http.MethodGet, "/state", nil)
	var resp podclient.StateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal() err = %v", err)
	}
	if resp.State != StateHot {
		t.Fatalf("State after warmup = %q, want %q", resp.State, StateHot)
	}
	if resp.GPUID != 4 {
		t.Fatalf("GPUID = %d, want 4", resp.GPUID)
	}

	rec = doJSON(t, s.Handler(), http.MethodPost, "/infer", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /infer while hot = %d, want 200", rec.Code)
	}
}

func TestServer_ReleaseIsAlways204(t *testing.T) {
	s := newTestServer()
	rec := doJSON(t, s.Handler(), http.MethodDelete, "/lease", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /lease while warm = %d, want 204", rec.Code)
	}
}

func TestServer_MetricsEndpointServesRegisteredNames(t *testing.T) {
	s := newTestServer()
	// gpulease_requests_total is a CounterVec: it has no exported series (and so
	// does not appear in /metrics output) until at least one /infer call has
	// incremented some "code" label value.
	doJSON(t, s.Handler(), http.MethodPost, "/infer", nil)

	rec := doJSON(t, s.Handler(), http.MethodGet, "/metrics", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, name := range []string{
		metricPodHot, metricPodCapacityRPS, metricPodGPUID, metricDemandRPS, metricRequestsTotal, metricActivationSecond,
	} {
		if !bytes.Contains([]byte(body), []byte(name)) {
			t.Errorf("metrics output missing %q", name)
		}
	}
}
