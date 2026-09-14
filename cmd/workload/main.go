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

// Command workload is the GPU workload simulator binary (design.md §5): an HTTP
// server on podclient.DefaultPort (8000) implementing the pod-side of the GPULease
// activation protocol (POST/DELETE /lease, GET /state), a fake /infer endpoint, and
// Prometheus metrics that feed KEDA's scaling PromQL.
//
// All configuration comes from the environment, populated by deploy/apps' Deployment
// manifests via the downward API (NODE_NAME/POD_NAME/POD_NAMESPACE) plus literal env
// vars (DEPLOYMENT_NAME, POD_CAPACITY_RPS, WARMUP_SECONDS, TEARDOWN_SECONDS,
// DEMAND_CONFIGMAP). See internal/workload.Config and design.md §5 for the full
// contract.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/llm-d-extensions/gpu-lease/internal/workload"

	pocv1alpha1 "github.com/llm-d-extensions/gpu-lease/api/v1alpha1"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := loadConfig(logger)
	if err != nil {
		// Fail fast (design.md §5 / task brief): NODE_NAME and POD_NAMESPACE are
		// required for correct behavior (node-mismatch checks and the demand
		// ConfigMap watch are both scoped by them), so there is no sane default to
		// run with if they are missing.
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	k8sClient := buildK8sClient(logger)

	srv := workload.NewServer(cfg, logger, k8sClient)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	logger.Info("starting gpu-lease workload simulator",
		"node", cfg.NodeName, "pod", cfg.PodName, "namespace", cfg.PodNamespace,
		"deployment", cfg.DeploymentName, "podCapacityRPS", cfg.PodCapacityRPS,
		"warmup", cfg.WarmupDuration, "teardown", cfg.TeardownDuration,
		"demandConfigMap", cfg.DemandConfigMapName,
	)

	if err := srv.ListenAndServe(ctx); err != nil {
		logger.Error("workload simulator exited with error", "error", err)
		os.Exit(1)
	}
	logger.Info("workload simulator shut down cleanly")
}

// loadConfig reads env vars into a workload.Config, applying design.md §5's defaults
// and failing on the two required-but-missing vars.
func loadConfig(logger *slog.Logger) (workload.Config, error) {
	nodeName := os.Getenv("NODE_NAME")
	podNamespace := os.Getenv("POD_NAMESPACE")
	if nodeName == "" {
		return workload.Config{}, fmt.Errorf("NODE_NAME is required (expected via the downward API, spec.nodeName)")
	}
	if podNamespace == "" {
		return workload.Config{}, fmt.Errorf("POD_NAMESPACE is required (expected via the downward API, metadata.namespace)")
	}

	cfg := workload.Config{
		NodeName:            nodeName,
		PodName:             os.Getenv("POD_NAME"),
		PodNamespace:        podNamespace,
		DeploymentName:      os.Getenv("DEPLOYMENT_NAME"),
		PodCapacityRPS:      envInt32(logger, "POD_CAPACITY_RPS", pocv1alpha1.DefaultPodCapacityRPS),
		WarmupDuration:      envSeconds(logger, "WARMUP_SECONDS", workload.DefaultWarmupDuration),
		TeardownDuration:    envSeconds(logger, "TEARDOWN_SECONDS", workload.DefaultTeardownDuration),
		DemandConfigMapName: envString("DEMAND_CONFIGMAP", workload.DefaultDemandConfigMapName),
	}

	if cfg.DeploymentName == "" {
		logger.Warn("DEPLOYMENT_NAME is unset; metrics will carry an empty deployment label and gpulease_demand_rps will never resolve a key in the demand ConfigMap")
	}

	return cfg, nil
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt32(logger *slog.Logger, key string, def int32) int32 {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		logger.Warn("ignoring unparseable env var, using default", "key", key, "value", raw, "default", def, "error", err)
		return def
	}
	return int32(v)
}

func envSeconds(logger *slog.Logger, key string, def time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		logger.Warn("ignoring unparseable/non-positive env var, using default", "key", key, "value", raw, "default", def, "error", err)
		return def
	}
	return time.Duration(v * float64(time.Second))
}

// buildK8sClient builds an in-cluster Kubernetes clientset for the demand ConfigMap
// watch. It degrades gracefully (returns nil, just logs why) rather than failing the
// whole process: the workload simulator's HTTP contract (POST/DELETE /lease, GET
// /state) does not depend on Kubernetes API access, only gpulease_demand_rps does.
func buildK8sClient(logger *slog.Logger) kubernetes.Interface {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		logger.Warn("no in-cluster Kubernetes config available; gpulease_demand_rps will stay at its initial value", "error", err)
		return nil
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		logger.Warn("failed to build Kubernetes clientset; gpulease_demand_rps will stay at its initial value", "error", err)
		return nil
	}
	return client
}
