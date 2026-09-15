#!/usr/bin/env bash
# hack/demo.sh [--all | --step N] [--yes]
#
# Scripted walkthrough of the 7-step reference scenario in design.md §2, asserting
# after each step against the success criteria in design.md §9. Run hack/watch.sh in a
# second terminal pane to watch pods/GPULeases/Workloads/HPAs live while this runs.
#
# Flags:
#   --all        run all 7 steps in order (default if no --step is given and --all
#                is passed explicitly; there is no implicit default -- see usage()).
#   --step N     run exactly one step (1-7). Steps 3 and 5 are pure assertion passes
#                for the WarmPool reconciler's automatic reaction to steps 2 and 4
#                respectively (design.md's table splits "KEDA acts" from "controller
#                reconciles" into two rows; in the live system the controller's
#                --warm-pool-resync=15s means the reaction is usually already visible
#                by the time step 3/5 runs, so these steps mostly just assert).
#   --yes        skip the interactive "press Enter to continue" pause before each step
#                (default: pause, printing what is about to happen and what to expect).
#
# Dependencies: kubectl, jq, curl (curl only for the Prometheus
# gpulease_lease_acquire_failures_total checks in steps 6/7, design.md §9 criterion 4;
# see the comment above prom_query() for why they assert a real requirement now rather
# than being purely informational, and why a single timeout there still doesn't abort
# the rest of the run).
#
# Assumptions about interfaces owned by other phases (see docs/demo.md and the final
# report of the agent that wrote this file for the full list):
#   - namespace gpu-lease-poc; Deployments named app-a / app-b, each with pod label
#     app=<name> (standard convention -- deploy/apps/*.yaml, Phase 2);
#   - ConfigMap gpu-lease-demand in that namespace, data key = deployment name, value =
#     demand in rps (design.md §5), defaulting to app-a=20, app-b=20 so both start at
#     the documented 2 hot + 2 warm baseline;
#   - hack/setup-nodes.sh supports `--node <name> --capacity <n>` to override one
#     node's leasable-GPU count (used by steps 6/7 below).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SETUP_NODES="${SCRIPT_DIR}/setup-nodes.sh"

NAMESPACE="${GPU_LEASE_NAMESPACE:-gpu-lease-poc}"
APP_A="app-a"
APP_B="app-b"
BASELINE_DEMAND=20   # matches pod-capacity-rps=10 default * (warmReplicas=2 + minHot-ish 2) -> 4 replicas, 2 hot + 2 warm.

ASSUME_YES=false
ONLY_STEP=""
RUN_ALL=false

PASS_COUNT=0
FAIL_COUNT=0

# ---------------------------------------------------------------------------
# output helpers
# ---------------------------------------------------------------------------

if [[ -t 1 ]]; then
  C_RED=$'\033[31m'; C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'; C_BOLD=$'\033[1m'; C_RESET=$'\033[0m'
else
  C_RED=""; C_GREEN=""; C_YELLOW=""; C_BOLD=""; C_RESET=""
fi

info()  { printf '%s[info]%s %s\n' "$C_YELLOW" "$C_RESET" "$*"; }
pass()  { printf '%s[PASS]%s %s\n' "$C_GREEN" "$C_RESET" "$*"; PASS_COUNT=$((PASS_COUNT+1)); }
fail()  { printf '%s[FAIL]%s %s\n' "$C_RED" "$C_RESET" "$*"; FAIL_COUNT=$((FAIL_COUNT+1)); }

usage() {
  cat <<'EOF'
usage: hack/demo.sh (--all | --step N) [--yes]

  --all       run all 7 demo steps from design.md §2 in order
  --step N    run exactly one step (1-7)
  --yes       do not pause for confirmation before each step
  -h, --help  show this help

Examples:
  hack/demo.sh --all
  hack/demo.sh --step 2
  hack/demo.sh --all --yes
EOF
}

# ---------------------------------------------------------------------------
# argument parsing
# ---------------------------------------------------------------------------

while [[ $# -gt 0 ]]; do
  case "$1" in
    --all) RUN_ALL=true; shift ;;
    --step) ONLY_STEP="${2:-}"; shift 2 ;;
    --yes) ASSUME_YES=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 1 ;;
  esac
done

if [[ "$RUN_ALL" == "true" && -n "$ONLY_STEP" ]]; then
  echo "--all and --step are mutually exclusive" >&2
  exit 1
fi
if [[ "$RUN_ALL" != "true" && -z "$ONLY_STEP" ]]; then
  usage >&2
  exit 1
fi
if [[ -n "$ONLY_STEP" ]] && ! [[ "$ONLY_STEP" =~ ^[1-7]$ ]]; then
  echo "--step must be 1-7" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# pause / narration
# ---------------------------------------------------------------------------

pause_step() { # num title action expect
  local num="$1" title="$2" action="$3" expect="$4"
  echo
  printf '%s================================================================================%s\n' "$C_BOLD" "$C_RESET"
  printf '%sSTEP %d: %s%s\n' "$C_BOLD" "$num" "$title" "$C_RESET"
  printf -- '--------------------------------------------------------------------------------\n'
  printf 'About to do:\n  %s\n' "$action"
  printf 'Expected outcome (design.md §2 / §9):\n  %s\n' "$expect"
  printf '%s================================================================================%s\n' "$C_BOLD" "$C_RESET"
  if [[ "$ASSUME_YES" != "true" ]]; then
    read -rp "Press Enter to run step ${num} (Ctrl-C to abort)... " _
  fi
}

# ---------------------------------------------------------------------------
# demand ConfigMap helpers (design.md §5)
# ---------------------------------------------------------------------------

set_demand() { # app rps
  local app="$1" rps="$2"
  info "set_demand ${app}=${rps}"
  kubectl -n "$NAMESPACE" patch configmap gpu-lease-demand --type merge \
    -p "{\"data\":{\"${app}\":\"${rps}\"}}" >/dev/null
}

get_demand() { # app
  kubectl get configmap gpu-lease-demand -n "$NAMESPACE" \
    -o jsonpath="{.data.${1}}" 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# pod / lease inspection helpers
# ---------------------------------------------------------------------------

pod_state_count() { # app state
  kubectl get pods -n "$NAMESPACE" -l "app=$1" -o json 2>/dev/null \
    | jq -r --arg s "$2" '[.items[] | select(.metadata.labels["gpu-lease.llm-d.ai/state"]==$s)] | length' \
    2>/dev/null || echo 0
}
hot_count()        { pod_state_count "$1" hot; }
warm_count()        { pod_state_count "$1" warm; }
total_pods() {
  kubectl get pods -n "$NAMESPACE" -l "app=$1" -o json 2>/dev/null \
    | jq -r '.items | length' 2>/dev/null || echo 0
}

check_counts() { # app want_hot want_warm
  [[ "$(hot_count "$1")" == "$2" && "$(warm_count "$1")" == "$3" ]]
}
check_total() { # app want_total
  [[ "$(total_pods "$1")" == "$2" ]]
}

# A newly Bound GPULease's spec.nodeName must equal the node its claimed pod actually
# runs on (design.md §9 criterion 2).
check_lease_node_matches_pod() { # app
  local app="$1" rows
  rows="$(kubectl get gpuleases -o json 2>/dev/null | jq -r --arg app "$app" \
    '.items[] | select(.status.phase=="Bound" and .spec.deploymentRef.name==$app)
     | [.metadata.name, .spec.nodeName, .spec.claimRef.namespace, .spec.claimRef.name] | @tsv' \
    2>/dev/null)"
  [[ -n "$rows" ]] || return 1
  while IFS=$'\t' read -r lname lnode pns pname; do
    [[ -n "$pname" ]] || continue
    local podnode
    podnode="$(kubectl get pod -n "$pns" "$pname" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)"
    if [[ "$lnode" != "$podnode" ]]; then
      echo "  mismatch: lease ${lname} claims node=${lnode} but pod ${pname} runs on node=${podnode}" >&2
      return 1
    fi
  done <<<"$rows"
  return 0
}

# Every hot pod's (node, gpu-id) pair must be unique (design.md §9 criterion 5). Checks
# the controller-owned pod annotation directly, per design.md §4.4.
check_gpu_ids_unique_per_node() {
  local dup
  dup="$(kubectl get pods -n "$NAMESPACE" -l 'gpu-lease.llm-d.ai/state=hot' -o json 2>/dev/null \
    | jq -r '.items[] | "\(.spec.nodeName)\t\(.metadata.annotations["gpu-lease.llm-d.ai/gpu-id"])"' \
    | sort | uniq -d)"
  [[ -z "$dup" ]]
}

check_failed_reason() { # reason
  kubectl get gpuleases -o json 2>/dev/null | jq -e --arg r "$1" \
    '.items[] | select(.status.phase=="Failed") | (.status.conditions[]? | select(.type=="Ready" and .reason==$r))' \
    >/dev/null 2>&1
}
check_event_reason() { # reason
  kubectl get events -n "$NAMESPACE" -o json 2>/dev/null | jq -e --arg r "$1" \
    '.items[] | select(.reason==$r)' >/dev/null 2>&1
}
check_quota_exhausted()  { check_failed_reason QuotaExhaustedOnNode  || check_event_reason QuotaExhaustedOnNode; }
check_no_free_gpu()      { check_failed_reason NoFreeGPUOnAnyPodNode || check_event_reason NoFreeGPUOnAnyPodNode; }

check_no_crashloop() { # app
  local bad
  bad="$(kubectl get pods -n "$NAMESPACE" -l "app=$1" -o json 2>/dev/null \
    | jq -r '.items[] | select(([.status.containerStatuses[]?.restartCount] | add // 0) > 2) | .metadata.name')"
  [[ -z "$bad" ]]
}

# Prometheus query via a short-lived port-forward.
#
# This asserts a real requirement (design.md §9 criterion 4: step 6 must yield "a
# non-zero gpulease_lease_acquire_failures_total"), not an advisory one -- see the
# controller-manager's ServiceMonitor (config/prometheus/monitor.yaml, enabled via
# config/default/kustomization.yaml's `- ../prometheus`) and its matching RBAC grant
# (deploy/monitoring/prometheus-metrics-reader-binding.yaml) for why this now reliably
# has data: earlier in this PoC's history the controller's own /metrics endpoint had
# no Prometheus scrape target at all (that gap is what's fixed by those two files), so
# this query always returned empty and callers below downgraded it to "best-effort".
# It is a real, working query now; callers still don't abort the whole run on a single
# timeout here (see the `|| info ...` on each call site) so a transient port-forward
# hiccup in a live demo doesn't cost you the remaining steps -- but a failure here is a
# genuine regression, not an environment quirk, and should be investigated as one:
# check `kubectl get servicemonitor -n gpu-lease-scaffold-system` and the Prometheus
# target's health (`/targets` in the Prometheus UI, or the API queried the same way
# this function does) before assuming otherwise.
prom_query() { # promql
  local q="$1" port=39090 pid out
  kubectl -n monitoring port-forward svc/prometheus-kube-prometheus-prometheus "${port}:9090" \
    >/dev/null 2>&1 &
  pid=$!
  sleep 1.5
  out="$(curl -s --max-time 5 "http://127.0.0.1:${port}/api/v1/query" \
    --data-urlencode "query=${q}" 2>/dev/null || echo '{}')"
  kill "$pid" >/dev/null 2>&1 || true
  wait "$pid" 2>/dev/null || true
  echo "$out"
}
check_failure_metric_nonzero() { # app reason
  local val
  val="$(prom_query "sum(gpulease_lease_acquire_failures_total{deployment=\"$1\",reason=\"$2\"})" \
    | jq -r '.data.result[0].value[1] // "0"' 2>/dev/null || echo 0)"
  awk -v v="${val:-0}" 'BEGIN{exit !(v+0>0)}'
}

# ---------------------------------------------------------------------------
# generic polling assertion
# ---------------------------------------------------------------------------

# wait_for "<description>" <timeout-seconds> <check-fn> [args...]
# Polls check-fn every 3s until it returns 0 (PASS) or the timeout elapses (FAIL).
wait_for() {
  local desc="$1" timeout="$2"; shift 2
  local start elapsed
  start="$(date +%s)"
  while true; do
    if "$@"; then
      elapsed=$(( $(date +%s) - start ))
      pass "${desc} (after ${elapsed}s)"
      return 0
    fi
    elapsed=$(( $(date +%s) - start ))
    if (( elapsed >= timeout )); then
      fail "${desc} (timed out after ${timeout}s)"
      return 1
    fi
    sleep 3
  done
}

# ---------------------------------------------------------------------------
# Deployment nodeSelector / annotation helpers (steps 6 & 7)
# ---------------------------------------------------------------------------

get_node_selector_json() { # deployment
  kubectl get deployment "$1" -n "$NAMESPACE" -o json 2>/dev/null \
    | jq -c '.spec.template.spec.nodeSelector // null'
}
pin_node_selector() { # deployment hostname
  kubectl patch deployment "$1" -n "$NAMESPACE" --type=merge \
    -p "{\"spec\":{\"template\":{\"spec\":{\"nodeSelector\":{\"kubernetes.io/hostname\":\"$2\"}}}}}" >/dev/null
}
restore_node_selector() { # deployment original-json (from get_node_selector_json)
  kubectl patch deployment "$1" -n "$NAMESPACE" --type=merge \
    -p "{\"spec\":{\"template\":{\"spec\":{\"nodeSelector\":$2}}}}" >/dev/null
}
get_annotation() { # deployment key
  kubectl get deployment "$1" -n "$NAMESPACE" -o jsonpath="{.metadata.annotations['$2']}" 2>/dev/null || true
}
set_or_clear_annotation() { # deployment key value(may be empty)
  local dep="$1" key="$2" val="$3"
  if [[ -z "$val" ]]; then
    kubectl annotate deployment "$dep" -n "$NAMESPACE" "${key}-" --overwrite >/dev/null 2>&1 || true
  else
    kubectl annotate deployment "$dep" -n "$NAMESPACE" "${key}=${val}" --overwrite >/dev/null
  fi
}

# Shared restore state for steps 6/7 (global, not local, so the EXIT trap below can
# still reach it even if a step aborts partway through under `set -e`).
S67_ACTIVE=0
S67_NODE=""
S67_ORIG_NODE_SELECTOR="null"
S67_ORIG_ADMISSION_TIMEOUT=""
S67_ORIG_DEMAND=""

restore_pin_and_capacity() {
  [[ "$S67_ACTIVE" == "1" ]] || return 0
  info "restoring app-a nodeSelector, admission-timeout annotation, demand, and ${S67_NODE} capacity"
  restore_node_selector "$APP_A" "$S67_ORIG_NODE_SELECTOR" || true
  set_or_clear_annotation "$APP_A" "gpu-lease.llm-d.ai/admission-timeout" "$S67_ORIG_ADMISSION_TIMEOUT" || true
  [[ -n "$S67_ORIG_DEMAND" ]] && set_demand "$APP_A" "$S67_ORIG_DEMAND" || true
  "$SETUP_NODES" --node "$S67_NODE" --capacity 4 || true
  S67_ACTIVE=0
}
trap restore_pin_and_capacity EXIT INT TERM

# ---------------------------------------------------------------------------
# steps 1-7 (design.md §2)
# ---------------------------------------------------------------------------

step1() {
  pause_step 1 "Baseline steady state" \
    "set_demand app-a ${BASELINE_DEMAND} and set_demand app-b ${BASELINE_DEMAND} (pod-capacity-rps default is 10 rps/hot pod)" \
    "KEDA converges both Deployments to 4 replicas each; the WarmPool reconciler converges each to 2 hot + 2 warm within ~60s, with 2 Bound GPULeases and 2 admitted Workloads per app (design.md §9 criterion 1). The rest of this walkthrough drives app-a only; app-b is left at this baseline throughout as a control -- it should stay steady at 2 hot + 2 warm while app-a's steps run, showing that the two apps' warm pools are independent."
  set_demand "$APP_A" "$BASELINE_DEMAND"
  set_demand "$APP_B" "$BASELINE_DEMAND"
  wait_for "app-a: 2 hot + 2 warm within 90s"      90 check_counts "$APP_A" 2 2
  wait_for "app-b: 2 hot + 2 warm within 90s"      90 check_counts "$APP_B" 2 2
  wait_for "app-a: gpu-ids unique per node"        30 check_gpu_ids_unique_per_node
  wait_for "app-a: every Bound lease's node matches its pod's node" 30 check_lease_node_matches_pod "$APP_A"
}

step2() {
  pause_step 2 "Demand 20 -> 25: KEDA scales up, new pod is born warm" \
    "set_demand app-a 25" \
    "KEDA scales app-a to ceil(25/10)+2 = 5 replicas. The new pod ships warm (Kueue is not involved in creating it) -- transiently 2 hot + 3 warm."
  set_demand "$APP_A" 25
  wait_for "app-a: 5 total replicas within 60s" 60 check_total "$APP_A" 5
}

step3() {
  pause_step 3 "WarmPool reconciler reacts to the warm surplus" \
    "(no action here -- this is the automatic reaction to step 2; --warm-pool-resync=15s)" \
    "warm surplus=1 is promoted: POST /lease on that pod's node -> 3 hot + 2 warm. The newly Bound GPULease's spec.nodeName equals the promoted pod's spec.nodeName, and gpu-ids stay unique per node (design.md §9 criterion 2)."
  wait_for "app-a: 3 hot + 2 warm within 90s"       90 check_counts "$APP_A" 3 2
  wait_for "app-a: every Bound lease's node matches its pod's node" 30 check_lease_node_matches_pod "$APP_A"
  wait_for "app-a: gpu-ids unique per node"         30 check_gpu_ids_unique_per_node
}

step4() {
  pause_step 4 "Demand 25 -> 20: KEDA scales down" \
    "set_demand app-a 20" \
    "KEDA scales app-a to 4 replicas. controller.kubernetes.io/pod-deletion-cost (warm=-100, hot=100) makes the ReplicaSet delete a WARM pod first -> transiently 3 hot + 1 warm. No pod is deleted by this script; the ReplicaSet does it."
  set_demand "$APP_A" "$BASELINE_DEMAND"
  wait_for "app-a: 4 total replicas within 90s" 90 check_total "$APP_A" 4
}

step5() {
  pause_step 5 "WarmPool reconciler reacts to the warm deficit" \
    "(no action here -- automatic reaction to step 4)" \
    "warm deficit=1 is resolved by demoting the newest hot pod: DELETE /lease, GPULease deleted, Kueue quota returned -> back to 2 hot + 2 warm (design.md §9 criterion 3)."
  wait_for "app-a: 2 hot + 2 warm within 90s" 90 check_counts "$APP_A" 2 2
  wait_for "app-a: gpu-ids unique per node"   30 check_gpu_ids_unique_per_node
  wait_for "app-b (control): still 2 hot + 2 warm" 15 check_counts "$APP_B" 2 2 \
    || info "app-b control check failed -- see notes above on independence of the two warm pools"
}

# Step 6 -- node-pinned over-scale -> QuotaExhaustedOnNode.
#
# IMPORTANT implementation note (deviates from "shrink" in favour of something that
# actually reaches the Kueue-side failure): internal/controller/inventory/inventory.go's
# NodeFreeCounts derives a node's free-GPU count purely from its poc.llm-d.ai/gpu-count
# label vs. existing GPULease objects. That is fully self-consistent with the
# WarmPool reconciler's own promotion precheck (design.md §4.6): if you *shrink* a
# pinned node's label below its real Kueue quota, the controller's own precheck
# ("no warm pod sits on a node with a free GPU") catches the exhaustion FIRST and
# reports NoFreeGPUOnAnyPodNode -- i.e. step 7's failure, not step 6's.
#
# To reach the Kueue-side QuotaExhaustedOnNode instead, the node's *label* (what the
# controller believes) must say there is room while the ClusterQueue's real
# nominalQuota (fixed at 4 in deploy/kueue/cluster-queue.yaml, not owned by this file)
# does not. So this step INFLATES one node's poc.llm-d.ai/gpu-count label above 4 via
# hack/setup-nodes.sh's --node/--capacity override, then pins app-a to that node and
# drives demand up: the controller happily attempts a 5th+ lease there (label says
# there's room), Kueue refuses to admit it (only 4 real units of quota exist for that
# node's ResourceFlavor) -> Failed/QuotaExhaustedOnNode.
step6() {
  local node="kind-wva-gpu-cluster-worker"
  local inflated_capacity=8

  pause_step 6 "Node-pinned over-scale -> QuotaExhaustedOnNode" \
    "Pin app-a to node ${node} only; temporarily report ${inflated_capacity} leasable GPUs on it (above its real Kueue quota of 4 -- see the comment above step6() in hack/demo.sh for why); shorten app-a's admission-timeout to 20s; drive demand to 80 (targetHot=8)." \
    "The controller happily tries a 5th+ lease on ${node} (its label says there's room); Kueue never admits it (only 4 real quota units exist there) -> a GPULease goes Failed/QuotaExhaustedOnNode within ~2*admission-timeout. Existing hot pods keep serving; nothing crash-loops (design.md §9 criterion 4)."

  S67_NODE="$node"
  S67_ORIG_NODE_SELECTOR="$(get_node_selector_json "$APP_A")"
  S67_ORIG_ADMISSION_TIMEOUT="$(get_annotation "$APP_A" "gpu-lease.llm-d.ai/admission-timeout")"
  S67_ORIG_DEMAND="$(get_demand "$APP_A")"
  [[ -n "$S67_ORIG_DEMAND" ]] || S67_ORIG_DEMAND="$BASELINE_DEMAND"
  S67_ACTIVE=1

  "$SETUP_NODES" --node "$node" --capacity "$inflated_capacity"
  pin_node_selector "$APP_A" "$node"
  set_or_clear_annotation "$APP_A" "gpu-lease.llm-d.ai/admission-timeout" "20s"
  set_demand "$APP_A" 80

  wait_for "app-a: a GPULease fails with QuotaExhaustedOnNode within 180s" 180 check_quota_exhausted
  wait_for "app-a: no pod is crash-looping"                                 15 check_no_crashloop "$APP_A"
  wait_for "app-a: gpulease_lease_acquire_failures_total{reason=QuotaExhaustedOnNode} > 0 (design.md §9 criterion 4)" \
    60 check_failure_metric_nonzero "$APP_A" "QuotaExhaustedOnNode" \
    || info "did not abort the run for this alone -- see the comment above prom_query() if it keeps failing"

  restore_pin_and_capacity
}

# Step 7 -- pod on a node with no leasable GPUs -> NoFreeGPUOnAnyPodNode.
#
# Zeroes worker2's poc.llm-d.ai/gpu-count label (matching design.md §3's framing of
# worker2 as "no leasable GPUs" literally) and pins app-a there, then raises demand.
# Unlike step 6, no Workload is ever attempted: the WarmPool reconciler's own precheck
# (§4.6) finds no candidate node with a free GPU and stops before touching Kueue.
#
# Determinism note: this precheck runs across ALL of app-a's warm pods, wherever they
# are. If, at the moment this runs, app-a still has warm pods sitting on nodes with
# real free capacity (e.g. run in isolation, right after a fresh baseline), the
# reconciler may satisfy the higher demand from those first and never need worker2 at
# all. Running the full --all sequence (steps 1-6 first) or re-running --step 7 after
# a first attempt makes this reliable in practice, since by then app-a/app-b's other
# hot pods have consumed most of the real cluster-wide quota. This is exactly the kind
# of live-cluster timing Phase 6 ("run the 7-step scenario end-to-end, fix fallout") is
# tasked with hardening; the assertion below uses a generous timeout and clearly
# reports PASS/FAIL either way rather than hanging.
step7() {
  local node="kind-wva-gpu-cluster-worker2"

  pause_step 7 "Pod on a GPU-less node -> NoFreeGPUOnAnyPodNode" \
    "Zero out ${node}'s leasable GPU count; pin app-a to ${node} only; shorten admission-timeout to 20s; drive demand to 80." \
    "No warm pod anywhere has a free GPU to promote to; the WarmPool reconciler emits event NoFreeGPUOnAnyPodNode and bumps its failure metric, without ever creating a Kueue Workload. Existing hot pods keep serving; nothing crash-loops."

  S67_NODE="$node"
  S67_ORIG_NODE_SELECTOR="$(get_node_selector_json "$APP_A")"
  S67_ORIG_ADMISSION_TIMEOUT="$(get_annotation "$APP_A" "gpu-lease.llm-d.ai/admission-timeout")"
  S67_ORIG_DEMAND="$(get_demand "$APP_A")"
  [[ -n "$S67_ORIG_DEMAND" ]] || S67_ORIG_DEMAND="$BASELINE_DEMAND"
  S67_ACTIVE=1

  "$SETUP_NODES" --node "$node" --capacity 0
  pin_node_selector "$APP_A" "$node"
  set_or_clear_annotation "$APP_A" "gpu-lease.llm-d.ai/admission-timeout" "20s"
  set_demand "$APP_A" 80

  wait_for "app-a: NoFreeGPUOnAnyPodNode within 180s"                       180 check_no_free_gpu
  wait_for "app-a: no pod is crash-looping"                                  15 check_no_crashloop "$APP_A"
  wait_for "app-a: gpulease_lease_acquire_failures_total{reason=NoFreeGPUOnAnyPodNode} > 0" \
    60 check_failure_metric_nonzero "$APP_A" "NoFreeGPUOnAnyPodNode" \
    || info "did not abort the run for this alone -- see the comment above prom_query() if it keeps failing"

  restore_pin_and_capacity
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------

run_step() {
  case "$1" in
    1) step1 ;; 2) step2 ;; 3) step3 ;; 4) step4 ;; 5) step5 ;; 6) step6 ;; 7) step7 ;;
    *) echo "no such step: $1" >&2; exit 1 ;;
  esac
}

if [[ -n "$ONLY_STEP" ]]; then
  run_step "$ONLY_STEP"
else
  for n in 1 2 3 4 5 6 7; do
    run_step "$n"
  done
fi

echo
printf '%s================================================================================%s\n' "$C_BOLD" "$C_RESET"
printf 'RESULT: %s%d passed%s, %s%d failed%s\n' "$C_GREEN" "$PASS_COUNT" "$C_RESET" "$C_RED" "$FAIL_COUNT" "$C_RESET"
printf '%s================================================================================%s\n' "$C_BOLD" "$C_RESET"

[[ "$FAIL_COUNT" -eq 0 ]]
