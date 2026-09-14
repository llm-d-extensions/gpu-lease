#!/usr/bin/env bash
# hack/watch.sh [interval-seconds]
#
# Single-screen live dashboard for the GPU Lease PoC demo (design.md §2/§9).
# Repaints every $INTERVAL seconds (default 2) with, for namespace
# gpu-lease-poc:
#   - pods + their gpu-lease.llm-d.ai/state label (warm/activating/hot/releasing)
#   - GPULeases (cluster-scoped; NODE/GPU/PHASE/POD/DEPLOYMENT printcolumns)
#   - Kueue Workloads (one per in-flight/bound lease)
#   - HPAs KEDA manages (keda-hpa-app-a, keda-hpa-app-b)
#   - the gpu-lease-demand ConfigMap (the demand "dial", design.md §5)
#
# Designed to fit an 80x50 terminal without wrapping: each section is capped to
# a small, fixed set of columns via `kubectl ... -o wide` only where it adds
# demo-relevant info (node placement), and long fields are left for
# `kubectl describe` / `kubectl get -o yaml` rather than crammed in here.
#
# Run this in its own terminal pane next to hack/demo.sh.
set -euo pipefail

NAMESPACE="${GPU_LEASE_NAMESPACE:-gpu-lease-poc}"
INTERVAL="${1:-2}"

if ! [[ "$INTERVAL" =~ ^[0-9]+$ ]]; then
  echo "usage: $0 [interval-seconds]" >&2
  exit 1
fi

hr() { printf '%.0s-' $(seq 1 "${COLUMNS:-80}"); printf '\n'; }

render() {
  local now
  now="$(date '+%Y-%m-%d %H:%M:%S')"

  printf 'gpu-lease-poc live dashboard  (namespace=%s, refresh=%ss)  %s\n' \
    "$NAMESPACE" "$INTERVAL" "$now"
  hr

  echo "PODS  (state label = gpu-lease.llm-d.ai/state)"
  kubectl get pods -n "$NAMESPACE" -L gpu-lease.llm-d.ai/state \
    -o custom-columns='NAME:.metadata.name,STATE:.metadata.labels.gpu-lease\.llm-d\.ai/state,READY:.status.containerStatuses[0].ready,PHASE:.status.phase,NODE:.spec.nodeName' \
    --sort-by=.metadata.name 2>&1 || echo "  (no pods / namespace not created yet)"
  echo

  echo "GPULEASES"
  kubectl get gpuleases -o wide 2>&1 || echo "  (no GPULeases / CRD not installed yet)"
  echo

  echo "KUEUE WORKLOADS  (-n $NAMESPACE)"
  kubectl get workloads -n "$NAMESPACE" 2>&1 || echo "  (no Workloads)"
  echo

  echo "HPA  (KEDA-managed, -n $NAMESPACE)"
  kubectl get hpa -n "$NAMESPACE" 2>&1 || echo "  (no HPAs / ScaledObjects not applied yet)"
  echo

  echo "DEMAND  (configmap/gpu-lease-demand, key=deployment name, value=rps)"
  # shellcheck disable=SC2016 # jsonpath's $k/$v are template vars, not shell vars.
  kubectl get configmap gpu-lease-demand -n "$NAMESPACE" \
    -o jsonpath='{range $k, $v := .data}{$k}{"="}{$v}{"\n"}{end}' 2>&1 \
    || echo "  (configmap gpu-lease-demand not found)"
  echo

  hr
  echo "Ctrl-C to exit. Run hack/demo.sh in another pane to drive the scenario."
}

# A hand-rolled clear-loop rather than shelling out to the external `watch`
# binary: `watch` is not preinstalled on macOS, and piping our multi-command
# render() through `watch -c "..."` string-escaping is more fragile than just
# looping here. Only depends on bash + kubectl (+ coreutils clear/date/seq).
while true; do
  clear
  render
  sleep "$INTERVAL"
done
