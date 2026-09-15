# GPU Lease PoC — Demo Runbook

This is the operator-facing walkthrough. For the design/contract behind every name and number
here, see [`docs/design.md`](design.md) — this document does not repeat rationale that lives
there, only the commands and expected output.

Everything below assumes the `kind-kind-wva-gpu-cluster` context, described in design.md §3
(3 nodes, no taints, KEDA + kube-prometheus-stack already installed).

## Prerequisites

- `kubectl` pointed at `kind-kind-wva-gpu-cluster`
- `kustomize`, `kind` v0.30, Go 1.25.1, `docker`, `helm` v4 (all listed as locally available in
  design.md §7)
- `jq` (used by `hack/demo.sh` for JSON assertions)
- Kueue is **not** pre-installed on this cluster (design.md §3) — it is installed as part of the
  order below.

## Install order

Run these once, in this order, from the repo root.

```sh
# 1. Node labels + simulated GPU capacity (poc.llm-d.ai/gpu-count / gpu-model)
hack/setup-nodes.sh

# 2. Kueue: CRDs/controller (not pre-installed), then this PoC's flavors/queues
#    (exact Kueue install command/version is v0.19.4 per design.md §3; owned by the
#    deploy/kueue phase — see its own docs if the install manifests differ from below)
kubectl apply --server-side -f deploy/kueue/resource-flavors.yaml
kubectl apply --server-side -f deploy/kueue/cluster-queue.yaml
kubectl apply --server-side -f deploy/kueue/namespace.yaml
kubectl apply --server-side -f deploy/kueue/local-queues.yaml

# 3. Build + kind-load the two images (no registry, per design.md §7)
make docker-build IMG=gpu-lease-controller:dev
docker build -t gpu-lease-workload:dev -f cmd/workload/Dockerfile .   # or `make` target if defined
kind load docker-image gpu-lease-controller:dev --name kind-wva-gpu-cluster
kind load docker-image gpu-lease-workload:dev   --name kind-wva-gpu-cluster

# 4. GPULease CRD + controller (also applies the controller's own ServiceMonitor --
#    config/default/kustomization.yaml includes ../prometheus)
make install    # applies config/crd
make deploy IMG=gpu-lease-controller:dev

# 4b. Grant kube-prometheus-stack's Prometheus ServiceAccount permission to read the
#     controller's secured (HTTPS + bearer-token) /metrics endpoint -- without this,
#     the ServiceMonitor from step 4 has a scrape target but every scrape 403s, and
#     gpulease_pool_hot_pods/_warm_pods, gpulease_lease_acquire_failures_total, etc.
#     (design.md §7) never have data. See the comment header in the file for why this
#     is applied directly rather than folded into config/default's kustomize tree.
kubectl apply -f deploy/monitoring/prometheus-metrics-reader-binding.yaml

# 5. app-a / app-b Deployments, Services, ServiceMonitors, the demand ConfigMap
kubectl apply -f deploy/apps/

# 6. KEDA ScaledObjects (last: they immediately start scaling on whatever demand
#    the ConfigMap in step 5 shipped with)
kubectl apply -f deploy/keda/
```

Verify before moving on:

```sh
kubectl get pods -n gpu-lease-poc
kubectl get scaledobject -n gpu-lease-poc
kubectl get hpa -n gpu-lease-poc

# controller's own metrics are being scraped (design.md §7) -- if this ServiceMonitor's
# target isn't "up" in Prometheus, step 4b above (the ClusterRoleBinding) is missing
kubectl get servicemonitor -n gpu-lease-scaffold-system
```

## Live dashboard

In a second terminal, leave this running for the rest of the demo:

```sh
hack/watch.sh          # refresh every 2s
hack/watch.sh 5         # or pass a custom interval in seconds
```

It shows, for `gpu-lease-poc`: pods with their `gpu-lease.llm-d.ai/state` label and node, all
`GPULease`s, Kueue `Workload`s, HPAs, and the current `gpu-lease-demand` ConfigMap — sized to fit
an 80x50 terminal.

## The 7-step walkthrough

Run the whole thing unattended:

```sh
hack/demo.sh --all --yes
```

Or step through it by hand, reading the "about to do / expected outcome" banner before each
Enter press:

```sh
hack/demo.sh --all
```

Or run one step in isolation (useful for re-running just steps 6/7):

```sh
hack/demo.sh --step 6
```

Below, "expected output" shows the shape of what `hack/watch.sh` / the listed `kubectl` command
should display at the end of the step — exact pod names/ages will differ.

### Step 1 — baseline steady state

Command (run by `hack/demo.sh`):

```sh
kubectl -n gpu-lease-poc patch cm gpu-lease-demand --type merge -p '{"data":{"app-a":"20","app-b":"20"}}'
```

Expected: within ~60s, each Deployment converges to 4 replicas, 2 hot + 2 warm.

```
$ kubectl get pods -n gpu-lease-poc -L gpu-lease.llm-d.ai/state
NAME               READY   STATUS    STATE   AGE
app-a-xxxxx-1      1/1     Running   hot     90s
app-a-xxxxx-2      1/1     Running   hot     90s
app-a-xxxxx-3      1/1     Running   warm    90s
app-a-xxxxx-4      1/1     Running   warm    90s
...

$ kubectl get gpuleases
NODE                              GPU   PHASE   POD              DEPLOYMENT   AGE
kind-wva-gpu-cluster-worker       0     Bound   app-a-xxxxx-1    app-a        85s
kind-wva-gpu-cluster-worker       1     Bound   app-a-xxxxx-2    app-a        85s
...
```

### Step 2 — demand 20 → 25 (KEDA scales up, new pod born warm)

```sh
kubectl -n gpu-lease-poc patch cm gpu-lease-demand --type merge -p '{"data":{"app-a":"25"}}'
```

Expected: `ceil(25/10)+2 = 5` replicas. `kubectl get hpa -n gpu-lease-poc` shows app-a's
`REPLICAS` climb to 5. The new pod starts labeled `warm` — Kueue is not involved yet.

### Step 3 — controller reconciles the warm surplus

No action (automatic). Within ~30–60s (controller resync 15s), the warm surplus of 1 is promoted:
3 hot + 2 warm. A new `Bound` `GPULease` appears whose `spec.nodeName` matches the promoted pod's
`spec.nodeName` — verify with:

```sh
kubectl get gpuleases -o wide
kubectl get pod <promoted-pod> -n gpu-lease-poc -o jsonpath='{.spec.nodeName}'
```

### Step 4 — demand 25 → 20 (KEDA scales down, warm pod dies first)

```sh
kubectl -n gpu-lease-poc patch cm gpu-lease-demand --type merge -p '{"data":{"app-a":"20"}}'
```

Expected: 4 replicas. `controller.kubernetes.io/pod-deletion-cost` (hot=100, warm=-100) makes the
ReplicaSet pick the warm pod to delete → transiently 3 hot + 1 warm.

### Step 5 — controller reconciles the warm deficit

No action (automatic). The newest hot pod is demoted (lease deleted, Kueue quota returned) → back
to 2 hot + 2 warm.

### Step 6 — node-pinned over-scale → `QuotaExhaustedOnNode`

```sh
hack/demo.sh --step 6
```

What it does (see the comment above `step6()` in `hack/demo.sh` for the full rationale): pins
`app-a` to one node via `nodeSelector`, temporarily reports more leasable GPUs on that node's label
than the Kueue `ClusterQueue`'s real quota (4) actually grants it, shortens
`admission-timeout` to 20s, and raises demand so a 5th+ lease is attempted there. The controller's
own precheck sees room (the label says so); Kueue's real admission does not.

Expected within ~2 minutes:

```
$ kubectl get gpuleases
...
kind-wva-gpu-cluster-worker   4   Failed   app-a-xxxxx-9   app-a   12s
$ kubectl get gpuleases -o jsonpath='{.items[?(@.status.phase=="Failed")].status.conditions}'
... "type":"Ready","status":"False","reason":"QuotaExhaustedOnNode" ...
```

No pod crash-loops; the pod that failed to acquire a lease simply stays `warm`. The script restores
the node's capacity label and app-a's `nodeSelector`/annotation/demand afterward.

### Step 7 — pod on a node with no leasable GPUs → `NoFreeGPUOnAnyPodNode`

```sh
hack/demo.sh --step 7
```

What it does: zeroes `kind-wva-gpu-cluster-worker2`'s leasable-GPU label, pins `app-a` there, and
raises demand. The WarmPool reconciler's own candidate search finds no warm pod anywhere sitting on
a node with a free GPU, so it never even attempts a Kueue `Workload` — it emits
`NoFreeGPUOnAnyPodNode` directly.

```
$ kubectl get events -n gpu-lease-poc --field-selector reason=NoFreeGPUOnAnyPodNode
```

**Note on `worker2`**: design.md §3 describes `worker2` as shipping with "no leasable GPUs" out of
the box. The actual `hack/setup-nodes.sh` default table gives it the same
`poc.llm-d.ai/gpu-count=4` as the other two nodes, so this step actively zeroes it
(`hack/setup-nodes.sh --node kind-wva-gpu-cluster-worker2 --capacity 0`) rather than relying on any
pre-existing state — see the "Known deviations" section below.

**Note on ordering**: this failure mode is most reliably observed after step 6 has already run in
the same session (app-a/app-b's other pods have by then consumed most of the cluster's real quota
elsewhere), or on a second `--step 7` attempt. Run with `--all` for the most deterministic result;
see the comment above `step7()` in `hack/demo.sh`.

## Demand vs. hot capacity — PromQL for Grafana/Prometheus

Paste into Prometheus's "Graph" tab or a Grafana panel (datasource: the `monitoring` Prometheus):

```promql
# current demand, as republished by every pod (all report the same value; use max, not sum)
max by (deployment) (gpulease_demand_rps{namespace="gpu-lease-poc"})

# current served capacity: 10 rps * number of hot pods, per deployment
sum by (deployment) (gpulease_pod_capacity_rps{namespace="gpu-lease-poc"})

# the exact value KEDA's trigger is scaling on (demand + fixed overprovision term)
# NOTE: no `by (deployment)` here, unlike the queries above -- this one is already
# scoped to a single deployment via the label matcher, and pairing `by (deployment)`
# with `or vector(0)` on an already-scoped query returns 2 elements once real data
# exists (KEDA rejects that as "returned multiple elements") -- see design.md §6.
(max(gpulease_demand_rps{namespace="gpu-lease-poc",deployment="app-a"}) or vector(0)) + (2 * 10)

# hot vs warm pod counts over time (controller-exposed, design.md §7)
gpulease_pool_hot_pods{namespace="gpu-lease-poc"}
gpulease_pool_warm_pods{namespace="gpu-lease-poc"}

# lease acquisition failures by reason (steps 6/7)
sum by (deployment, reason) (gpulease_lease_acquire_failures_total{namespace="gpu-lease-poc"})
```

## Tearing down

```sh
hack/teardown.sh --yes
```

Removes the `gpu-lease-poc` namespace, both ScaledObjects, every `GPULease`, and this PoC's Kueue
`ClusterQueue`/`ResourceFlavor`s. Does **not** touch Kueue, KEDA, the monitoring stack, node
labels, or `llm-d-optimized-baseline`. See the comment header in `hack/teardown.sh` for the exact
scope. Re-running the install order above (from step 1 or step 3, since node labels/Kueue
CRDs/KEDA are untouched) brings the demo back.

## Troubleshooting

**HPA shows `<unknown>` for the external metric / `kubectl get hpa` `TARGETS` column is `<unknown>/40`**

- The Prometheus trigger has no data yet. Most common cause: no `app-a` pod has scraped
  `gpu-lease-demand` and exposed `gpulease_demand_rps` even once (the `or vector(0)` fallback in
  the ScaledObject's query handles the *metric* side of this, but the HPA also needs KEDA's
  `keda-operator` to have successfully reached Prometheus at all).
- Check: `kubectl get scaledobject -n gpu-lease-poc app-a -o yaml` — look at `status.conditions`.
- Check KEDA can reach Prometheus:
  `kubectl -n keda logs deploy/keda-operator | grep -i prometheus`
- Check the query directly against Prometheus (port-forward, then hit `/api/v1/query`):
  `kubectl -n monitoring port-forward svc/prometheus-kube-prometheus-prometheus 9090:9090`

**ServiceMonitor not scraped (target missing in Prometheus)**

- `kube-prometheus-stack` here has `serviceMonitorSelector: {}` /
  `serviceMonitorNamespaceSelector: {}` (design.md §3) — it should scrape any ServiceMonitor in any
  namespace with no extra labels required. If a target is still missing:
  - `kubectl get servicemonitor -n gpu-lease-poc` — does it exist and does its `spec.selector`
    actually match the Service's labels?
  - `kubectl get endpoints <service> -n gpu-lease-poc` — does the Service have any endpoints (i.e.
    do its pods pass their readiness probe)?
  - Check Prometheus's own target page (port-forward Prometheus as above, then open
    `/targets` in a browser) for the scrape error message.

**Controller-side metrics (`gpulease_pool_hot_pods`, `gpulease_lease_acquire_failures_total`, ...)
show no data even though the ServiceMonitor target is `up`**

- Actually check whether the target is `up`, not just present — the controller's `/metrics` is
  HTTPS + bearer-token authenticated (design.md §7). If the target's health is
  `down`/`4xx`/`5xx` (not just missing), Prometheus's own ServiceAccount is not authorized to read
  it: `kubectl get clusterrolebinding gpu-lease-poc-prometheus-metrics-reader` should exist (see
  `deploy/monitoring/prometheus-metrics-reader-binding.yaml`) — re-apply it if missing.
- If the target isn't listed at all, `config/default/kustomization.yaml`'s `- ../prometheus` line
  must be uncommented and `make deploy` re-run.

**A `GPULease` is stuck in `Pending`**

- It is waiting on Kueue admission. `kubectl get workloads -n gpu-lease-poc` and look at its
  `status.conditions` — reason `Pending` (not the PoC's own `QuotaExhaustedOnNode` vocabulary,
  which only appears on the `GPULease`, per design.md §4.7.1) means the flavor for that node is out
  of quota. Reason `Inadmissible` means the `LocalQueue` referenced by the Deployment's
  `gpu-lease.llm-d.ai/local-queue` annotation does not exist — check
  `kubectl get localqueue -n gpu-lease-poc`.
- If it stays `Pending` past `admission-timeout` (default 60s) and does *not* transition to
  `Failed`, the GPULease controller is not running or is stuck: `kubectl get pods -n gpu-lease-poc-system` (or wherever `make deploy` put it) and check its logs.

**A pod is stuck labeled `activating`**

- The controller `POST`ed `/lease` and is polling `GET /state` waiting for `hot`, budgeted by
  `activation-timeout` (default 120s). Check the pod's own logs/`/state` endpoint directly:
  `kubectl exec -n gpu-lease-poc <pod> -- wget -qO- localhost:8000/state`
- If it never reaches `hot` and the timeout fires, expect `Failed`/`ActivationTimeout` on the
  `GPULease` and the pod returning to `warm`.
- If the pod's `NODE_NAME` (downward API) does not match the lease's `nodeName` you will instead
  see `PodUnreachable`/409 immediately, not a timeout — check for a pod that was rescheduled to a
  different node between promotion decision and the `POST /lease` call (`NodeMismatch`).

## Known deviations from `docs/design.md`

- §3 frames `kind-wva-gpu-cluster-worker2` as shipping with no leasable GPUs
  (`status.capacity` "none"). The committed `hack/setup-nodes.sh` gives it
  `poc.llm-d.ai/gpu-count=4` by default like the other two nodes, so step 7 above actively zeroes
  it rather than relying on that framing being true out of the box.
- §4.5 states `hack/setup-nodes.sh` "writes both the labels and
  `deploy/kueue/cluster-queue.yaml` from one table so they cannot drift." The committed script
  only writes node labels/capacity; it does not regenerate `deploy/kueue/cluster-queue.yaml`. Keep
  that file's `nominalQuota` values in sync with `hack/setup-nodes.sh`'s default table by hand
  until/unless this is reconciled.
- §2's step 6 ("scale past that node's GPU count") is implemented here by *raising* the pinned
  node's `poc.llm-d.ai/gpu-count` label above its real (fixed) Kueue quota, not by shrinking it.
  Shrinking the label instead moves the failure earlier, into the controller's own promotion
  precheck (`NoFreeGPUOnAnyPodNode`, step 7's failure) rather than Kueue's admission layer
  (`QuotaExhaustedOnNode`, step 6's required failure) — see the comment above `step6()` in
  `hack/demo.sh` for the full reasoning, grounded in
  `internal/controller/inventory/inventory.go`'s `NodeFreeCounts`.
