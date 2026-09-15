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

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Controller-side metrics named in design.md §7. Both the WarmPool and GPULease
// reconcilers report through these; they are registered with controller-runtime's
// global metrics registry (the same registry the manager's /metrics endpoint serves) by
// the init() below, so importing this package is enough to make them show up.
//
// The per-Deployment gauges are labeled ("workload_namespace", "deployment"), not just
// ("deployment"): a Deployment is only unique within its namespace, and the `namespace`
// label Prometheus attaches to these series is the *controller's* namespace (it comes
// from the scrape target, which is the controller-manager metrics Service), so it cannot
// be used to disambiguate two managed Deployments of the same name in different
// namespaces. The label deliberately is not called `namespace`: an exposed label that
// collides with a target label gets renamed to `exported_namespace` by Prometheus
// (honor_labels: false, the default), which is exactly the kind of surprise a KEDA
// trigger query should not have to know about. See deploy/keda/scaledobject-app-a.yaml,
// which selects gpulease_pool_warm_desired on (workload_namespace, deployment).
var (
	// poolLabels is the label set of every per-Deployment pool gauge below; keep the
	// WithLabelValues call order (namespace, name) in sync with it.
	poolLabels = []string{"workload_namespace", "deployment"}

	// PoolHotPods is the observed number of hot pods per managed Deployment.
	PoolHotPods = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpulease_pool_hot_pods",
			Help: "Number of pods currently in state hot, per managed Deployment.",
		},
		poolLabels,
	)

	// PoolWarmPods is the observed number of warm (+ activating/releasing) pods per
	// managed Deployment.
	PoolWarmPods = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpulease_pool_warm_pods",
			Help: "Number of pods currently in state warm, per managed Deployment.",
		},
		poolLabels,
	)

	// PoolWarmDesired is the configured warm-replicas target per managed Deployment. It
	// is what makes the Deployment's gpu-lease.llm-d.ai/warm-replicas annotation the
	// single source of truth for the warm buffer: the KEDA ScaledObject reads this gauge
	// as its `warm_desired` trigger and adds it, as a replica count, in the
	// advanced.scalingModifiers formula (design.md §6) rather than hardcoding the number
	// into PromQL.
	PoolWarmDesired = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpulease_pool_warm_desired",
			Help: "Configured gpu-lease.llm-d.ai/warm-replicas target, per managed Deployment.",
		},
		poolLabels,
	)

	// LeasesActive is the number of Bound (or in-flight) GPULeases per node.
	LeasesActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpulease_leases_active",
			Help: "Number of GPULease objects that currently exist, per node.",
		},
		[]string{"node"},
	)

	// NodeGPUsFree is the number of unleased GPU indices per node (inventory.NodeFreeCounts).
	NodeGPUsFree = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpulease_node_gpus_free",
			Help: "Number of GPU indices on the node not currently held by any GPULease.",
		},
		[]string{"node"},
	)

	// LeaseAcquireFailuresTotal counts lease-acquisition failures by Deployment and
	// GPULease failure reason (one of the poc.Reason* constants).
	LeaseAcquireFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gpulease_lease_acquire_failures_total",
			Help: "Count of lease acquisition failures, by Deployment and failure reason.",
		},
		[]string{"deployment", "reason"},
	)

	// PromotionsTotal counts warm->hot promotions decided by the WarmPool reconciler.
	PromotionsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "gpulease_promotions_total",
			Help: "Total count of warm pods promoted to hot (lease acquisitions started).",
		},
	)

	// DemotionsTotal counts hot->warm demotions decided by the WarmPool reconciler.
	DemotionsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "gpulease_demotions_total",
			Help: "Total count of hot pods demoted to warm (lease releases started).",
		},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		PoolHotPods,
		PoolWarmPods,
		PoolWarmDesired,
		LeasesActive,
		NodeGPUsFree,
		LeaseAcquireFailuresTotal,
		PromotionsTotal,
		DemotionsTotal,
	)
}
