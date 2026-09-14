#!/usr/bin/env bash
# hack/setup-nodes.sh
#
# Single source of truth for the PoC's GPU inventory (design.md §4.5).
# Labels the 3 kind-cluster nodes with poc.llm-d.ai/gpu-count and
# poc.llm-d.ai/gpu-model (derived from each node's existing gpu-config
# label) and patches status.capacity["poc.llm-d.ai/gpu"] for realism.
#
# Idempotent: safe to re-run. Values are derived from the NODES table
# below; deploy/kueue/cluster-queue.yaml's nominalQuota per flavor MUST be
# kept equal to gpu-count here so they cannot drift (design.md §4.5).
#
# Usage:
#   hack/setup-nodes.sh                              # apply the table as-is
#   hack/setup-nodes.sh --capacity 2                 # apply to ALL nodes
#   hack/setup-nodes.sh --node <name> --capacity N   # shrink one node only
#                                                     # (demo: simulate GPU
#                                                     # exhaustion without
#                                                     # touching the others)
#
# Cluster facts this table encodes (design.md §3):
#   kind-wva-gpu-cluster-control-plane  gpu-config=4H100   -> H100,  count=4
#   kind-wva-gpu-cluster-worker         gpu-config=4A100   -> A100,  count=4
#   kind-wva-gpu-cluster-worker2        gpu-config=4MI300X -> MI300X, count=4
set -euo pipefail

# name:model:count
NODES=(
  "kind-wva-gpu-cluster-control-plane:H100:4"
  "kind-wva-gpu-cluster-worker:A100:4"
  "kind-wva-gpu-cluster-worker2:MI300X:4"
)

OVERRIDE_NODE=""
OVERRIDE_CAPACITY=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --capacity)
      OVERRIDE_CAPACITY="$2"
      shift 2
      ;;
    --node)
      OVERRIDE_NODE="$2"
      shift 2
      ;;
    *)
      echo "unknown argument: $1" >&2
      exit 1
      ;;
  esac
done

capacity_for() {
  local node="$1" default_count="$2"
  if [[ -n "$OVERRIDE_CAPACITY" ]]; then
    if [[ -z "$OVERRIDE_NODE" || "$OVERRIDE_NODE" == "$node" ]]; then
      echo "$OVERRIDE_CAPACITY"
      return
    fi
  fi
  echo "$default_count"
}

for entry in "${NODES[@]}"; do
  IFS=':' read -r node model default_count <<<"$entry"
  count="$(capacity_for "$node" "$default_count")"

  echo "== ${node}: gpu-model=${model} gpu-count=${count} =="

  # 1. Node labels (design.md §4.5) -- leasable GPU IDs are [0, count).
  kubectl label node "$node" \
    "poc.llm-d.ai/gpu-count=${count}" \
    "poc.llm-d.ai/gpu-model=${model}" \
    --overwrite

  # 2. status.capacity["poc.llm-d.ai/gpu"] via the status subresource.
  #    Harmless/informational only: no real pod ever requests this
  #    resource name, and it does not survive a kubelet restart (the
  #    kubelet is the sole writer of its own capacity thereafter).
  #    "/" in the resource name must be escaped as "~1" per JSON Patch
  #    (RFC 6901) when used inside a JSON Pointer path.
  kubectl patch node "$node" \
    --subresource=status \
    --type='json' \
    -p="[{\"op\":\"add\",\"path\":\"/status/capacity/poc.llm-d.ai~1gpu\",\"value\":\"${count}\"}]"
done

echo
echo "-- verification --"
kubectl get nodes -L poc.llm-d.ai/gpu-count,poc.llm-d.ai/gpu-model
