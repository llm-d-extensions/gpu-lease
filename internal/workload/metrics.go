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
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metric names, exactly as design.md §5 names them. Do not rename without updating
// design.md and deploy/keda's PromQL, which references gpulease_demand_rps by name.
const (
	metricPodHot           = "gpulease_pod_hot"
	metricPodCapacityRPS   = "gpulease_pod_capacity_rps"
	metricPodGPUID         = "gpulease_pod_gpu_id"
	metricDemandRPS        = "gpulease_demand_rps"
	metricRequestsTotal    = "gpulease_requests_total"
	metricActivationSecond = "gpulease_activation_seconds"
)

// Metrics holds this pod's Prometheus instrumentation (design.md §5). It uses a
// private registry containing *exactly* the six metrics design.md §5 names -- no
// Go-runtime/process default collectors -- so /metrics stays a small, predictable
// surface for the demo.
//
// gpulease_pod_hot, gpulease_pod_capacity_rps and gpulease_pod_gpu_id carry
// ConstLabels deployment/pod/node, since those three values are fixed for the
// lifetime of one pod process (design.md §5: "all labeled deployment, pod, node").
// gpulease_demand_rps deliberately carries *only* a deployment ConstLabel: it is
// deployment-wide demand (every pod of a Deployment reports the same value), and the
// KEDA PromQL uses `max by (deployment)` over it -- adding pod/node here would
// fragment that into one series per pod and break the query (design.md §5, §6).
type Metrics struct {
	registry *prometheus.Registry

	podHot         prometheus.Gauge
	podCapacityRPS prometheus.Gauge
	podGPUID       prometheus.Gauge
	demandRPS      prometheus.Gauge
	requestsTotal  *prometheus.CounterVec
	activationSecs prometheus.Histogram
}

// NewMetrics builds and registers a pod's Metrics. deployment/pod/node are baked in
// as ConstLabels (ownNode is this pod's NODE_NAME, i.e. always equal to the "node"
// label value emitted).
func NewMetrics(deployment, pod, node string) *Metrics {
	podLabels := prometheus.Labels{"deployment": deployment, "pod": pod, "node": node}

	m := &Metrics{
		registry: prometheus.NewRegistry(),
		podHot: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        metricPodHot,
			Help:        "1 if this pod currently holds a bound GPU lease and is hot, 0 otherwise.",
			ConstLabels: podLabels,
		}),
		podCapacityRPS: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        metricPodCapacityRPS,
			Help:        "This pod's serving capacity in requests/sec: POD_CAPACITY_RPS when hot, 0 otherwise.",
			ConstLabels: podLabels,
		}),
		podGPUID: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        metricPodGPUID,
			Help:        "The GPU index of this pod's current lease, or -1 if it holds none.",
			ConstLabels: podLabels,
		}),
		demandRPS: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        metricDemandRPS,
			Help:        "Deployment-wide demand in requests/sec, republished from ConfigMap gpu-lease-demand. Every pod of a Deployment reports the same value; consume with max by (deployment).",
			ConstLabels: prometheus.Labels{"deployment": deployment},
		}),
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        metricRequestsTotal,
			Help:        "Total POST /infer requests handled by this pod, by HTTP status code.",
			ConstLabels: podLabels,
		}, []string{"code"}),
		activationSecs: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:        metricActivationSecond,
			Help:        "Observed warmup latency (POST /lease accepted -> state hot), in seconds.",
			ConstLabels: podLabels,
			// WARMUP_SECONDS defaults to 10s; spread buckets around that.
			Buckets: []float64{1, 2, 5, 8, 10, 12, 15, 20, 30, 60},
		}),
	}

	m.registry.MustRegister(m.podHot, m.podCapacityRPS, m.podGPUID, m.demandRPS, m.requestsTotal, m.activationSecs)

	// Start warm: not hot, no capacity, no GPU held.
	m.podHot.Set(0)
	m.podCapacityRPS.Set(0)
	m.podGPUID.Set(-1)
	m.demandRPS.Set(0)

	return m
}

// Handler returns the http.Handler to mount at GET /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// ApplyEvent updates pod_hot/pod_capacity_rps/pod_gpu_id and, when present, observes
// gpulease_activation_seconds, from a Machine transition Event. capacityRPS is the
// pod's configured POD_CAPACITY_RPS, applied only while hot (design.md §5).
func (m *Metrics) ApplyEvent(ev Event, capacityRPS int32) {
	hot := ev.New == StateHot
	if hot {
		m.podHot.Set(1)
		m.podCapacityRPS.Set(float64(capacityRPS))
		m.podGPUID.Set(float64(ev.Snapshot.GPUID))
	} else {
		m.podHot.Set(0)
		m.podCapacityRPS.Set(0)
		if ev.New == StateWarm {
			// -1: no lease held. Left unchanged while activating/releasing so the
			// gauge continues to reflect the (still relevant) reserved GPU id.
			m.podGPUID.Set(-1)
		}
	}
	if ev.ActivationDuration > 0 {
		m.activationSecs.Observe(ev.ActivationDuration.Seconds())
	}
}

// SetDemandRPS republishes the deployment-wide demand read from ConfigMap
// gpu-lease-demand (design.md §5's demand injection).
func (m *Metrics) SetDemandRPS(v float64) {
	m.demandRPS.Set(v)
}

// ObserveRequest records one POST /infer response by HTTP status code.
func (m *Metrics) ObserveRequest(code int) {
	m.requestsTotal.WithLabelValues(strconv.Itoa(code)).Inc()
}
