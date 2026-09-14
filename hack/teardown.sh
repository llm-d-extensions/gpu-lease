#!/usr/bin/env bash
# hack/teardown.sh --yes
#
# Removes the GPU Lease PoC's own resources so the demo can be redeployed cleanly.
# Destructive scope, exactly (and only) this):
#   - namespace gpu-lease-poc (Deployments, Services, ConfigMaps, Pods, HPAs, Kueue
#     LocalQueues/Workloads -- everything namespaced lives here and goes with it)
#   - the two ScaledObjects deploy/keda/scaledobject-app-{a,b}.yaml target (they live
#     IN the namespace above, so `kubectl delete namespace` already removes them; they
#     are also deleted explicitly first so KEDA's own cleanup runs before the namespace
#     disappears out from under it)
#   - every GPULease custom resource (cluster-scoped, so NOT removed by deleting the
#     namespace); their finalizer (poc.llm-d.ai/release-lease, api/v1alpha1/names.go) is
#     stripped first so deletion cannot hang if the controller is already gone
#   - the Kueue ClusterQueue (gpu-pool) and ResourceFlavors this PoC created
#     (deploy/kueue/cluster-queue.yaml, resource-flavors.yaml)
#
# Explicitly NOT touched by this script (owned by other phases / shared infra):
#   - Kueue itself (its CRDs, controller, webhooks)
#   - KEDA itself (its CRDs, controller, ScaledObject/TriggerAuthentication CRDs)
#   - the monitoring stack (Prometheus/Grafana, any ServiceMonitors outside this
#     namespace)
#   - namespace llm-d-optimized-baseline and anything in it
#   - node labels/capacity applied by hack/setup-nodes.sh (harmless to leave; rerunning
#     hack/setup-nodes.sh resets them before the next demo)
#   - deploy/kueue/local-queues.yaml's LocalQueues are namespaced (gpu-lease-poc) and
#     are removed as part of the namespace delete above, not listed separately
#
# Requires --yes (no destructive action runs without it -- even --dry-run=client/server
# validation of this file does not require it, since no kubectl calls happen at parse
# time).
set -euo pipefail

NAMESPACE="${GPU_LEASE_NAMESPACE:-gpu-lease-poc}"
CONFIRM=false

usage() {
  cat <<EOF
usage: hack/teardown.sh --yes

Deletes the GPU Lease PoC's own resources:
  - namespace ${NAMESPACE} (Deployments, Services, ConfigMaps, HPAs, LocalQueues, Workloads)
  - ScaledObjects app-a / app-b in that namespace
  - every GPULease custom resource (cluster-scoped; finalizers cleared first)
  - Kueue ClusterQueue gpu-pool and its ResourceFlavors

Does NOT touch: Kueue itself, KEDA itself, the monitoring stack, or namespace
llm-d-optimized-baseline.

--yes is required; there is no default action without it.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --yes) CONFIRM=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 1 ;;
  esac
done

if [[ "$CONFIRM" != "true" ]]; then
  usage >&2
  echo >&2
  echo "refusing to run without --yes" >&2
  exit 1
fi

echo "This will delete:"
echo "  - namespace ${NAMESPACE} and everything in it"
echo "  - ScaledObjects app-a/app-b (implied by the namespace delete above)"
echo "  - ALL GPULease custom resources (cluster-scoped)"
echo "  - Kueue ClusterQueue gpu-pool and its ResourceFlavors"
echo "It will NOT touch Kueue, KEDA, the monitoring stack, or llm-d-optimized-baseline."
echo

# ---------------------------------------------------------------------------
# 1. ScaledObjects first, so KEDA removes its own HPA/finalizers cleanly before
#    the namespace (and the Deployments it targets) disappear underneath it.
# ---------------------------------------------------------------------------
echo "==> deleting ScaledObjects in ${NAMESPACE}"
kubectl delete scaledobject app-a app-b -n "$NAMESPACE" --ignore-not-found --timeout=30s || true

# ---------------------------------------------------------------------------
# 2. GPULeases are cluster-scoped: deleting the namespace never touches them.
#    Strip the release finalizer first so this cannot hang if the controller
#    (which normally removes the finalizer after releasing the GPU) is not
#    running -- this is a teardown, not a graceful drain.
# ---------------------------------------------------------------------------
echo "==> clearing finalizers on GPULeases and deleting them"
lease_names="$(kubectl get gpuleases -o name 2>/dev/null || true)"
if [[ -n "$lease_names" ]]; then
  while IFS= read -r res; do
    [[ -n "$res" ]] || continue
    kubectl patch "$res" --type=json -p='[{"op":"remove","path":"/metadata/finalizers"}]' >/dev/null 2>&1 || true
  done <<<"$lease_names"
  kubectl delete gpuleases --all --timeout=30s || true
else
  echo "  (none found -- GPULease CRD not installed, or no instances)"
fi

# ---------------------------------------------------------------------------
# 3. The namespace: Deployments, Services, ConfigMaps (including
#    gpu-lease-demand), HPAs, LocalQueues, Workloads all go with it.
# ---------------------------------------------------------------------------
echo "==> deleting namespace ${NAMESPACE}"
kubectl delete namespace "$NAMESPACE" --ignore-not-found --timeout=60s || {
  echo "  namespace delete did not finish within the timeout -- it may be stuck" >&2
  echo "  terminating on a finalizer (e.g. a leftover Kueue Workload). Check with:" >&2
  echo "    kubectl get namespace ${NAMESPACE} -o yaml" >&2
}

# ---------------------------------------------------------------------------
# 4. Kueue objects this PoC owns that are NOT namespaced by gpu-lease-poc:
#    the ClusterQueue and its ResourceFlavors. Kueue itself (CRDs/controller)
#    is untouched.
# ---------------------------------------------------------------------------
echo "==> deleting ClusterQueue gpu-pool and its ResourceFlavors"
kubectl delete clusterqueue gpu-pool --ignore-not-found --timeout=30s || true
kubectl delete resourceflavor h100-control-plane a100-worker mi300x-worker2 \
  --ignore-not-found --timeout=30s || true

echo
echo "teardown complete."
