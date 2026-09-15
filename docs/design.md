# GPU Lease PoC — Design

Status: design approved for implementation. This document is the **implementation contract**.
Implementers must not deviate from the names, annotations, API shapes, or HTTP contracts below
without updating this file.

## 1. Goal

Demonstrate on a **GPU-less kind cluster** that pods can transition from **warm** (running, no GPU)
to **hot** (holding a GPU lease) when a lease is acquired, with:

- **KEDA** driving replica count from demand metrics (it has no idea warm/hot exists),
- **Kueue** acting as the shared, quota-enforcing allocator of GPU leases,
- a **new controller** that maintains the invariant *"always N warm pods"* by promoting surplus
  warm pods to hot (acquiring a lease) and demoting hot pods to warm (releasing a lease).

A **GPU lease** is identified by the pair `(node, gpuID)`. A lease may only be granted to a pod
**already running on that node**.

## 2. Reference scenario (the demo)

Two Deployments (`app-a`, `app-b`) in namespace `gpu-lease-poc`, each starting at
**2 hot + 2 warm = 4 replicas**.

| Step | Action | Expected outcome |
|---|---|---|
| 1 | demand=20 rps, capacity=10 rps/hot pod | KEDA → 4 replicas; controller → 2 hot, 2 warm |
| 2 | demand→25 | KEDA → 5 replicas. New pod starts **warm** (Kueue not involved). Now 2 hot + 3 warm |
| 3 | controller reconciles | warm surplus=1 → leases a GPU on that pod's node, `POST /lease` → 3 hot + 2 warm |
| 4 | demand→20 | KEDA → 4. `pod-deletion-cost` makes k8s delete a **warm** pod → 3 hot + 1 warm |
| 5 | controller reconciles | warm deficit=1 → demotes newest hot pod, releases lease → 2 hot + 2 warm |
| 6 | pin `app-a` to one node, then **inflate that node's `gpu-count` label above its Kueue `nominalQuota`** | lease acquisition **fails**: `QuotaExhaustedOnNode`, pod stays warm, event + metric emitted |
| 7 | **zero a node's `gpu-count` label** while warm pods are running on it | lease acquisition **fails**: `NoFreeGPUOnAnyPodNode` |

Steps 6 and 7 are the two required failure modes, and the mechanisms are **not** interchangeable —
they exercise two different components, so each must be provoked in the one way that reaches it:

- **Step 7 (`NoFreeGPUOnAnyPodNode`)** is *our* precheck: the WarmPool consults `inventory`, finds no
  warm pod on a node with a free GPU, and never creates a Workload. Kueue is never involved.
- **Step 6 (`QuotaExhaustedOnNode`)** is *Kueue's* refusal, and it can only be observed if our
  precheck believes there is room. Simply scaling past a node's GPU count does **not** work: the
  precheck fires first and reports `NoFreeGPUOnAnyPodNode`, i.e. the wrong failure mode. Inflating
  the `gpu-count` label above the real `nominalQuota` makes inventory permissive and hands the
  decision to Kueue. See §4.5 on why that asymmetry is the safe direction.

## 3. Cluster facts (already true — do not re-provision)

Context `kind-kind-wva-gpu-cluster`, 3 nodes, **no taints** (control-plane is schedulable):

| Node | `gpu-config` label | Model | `status.capacity` today |
|---|---|---|---|
| `kind-wva-gpu-cluster-control-plane` | `4H100` | H100 | `nvidia.com/gpu: 4` |
| `kind-wva-gpu-cluster-worker` | `4A100` | A100 | `nvidia.com/gpu: 4` |
| `kind-wva-gpu-cluster-worker2` | `4MI300X` | MI300X | **no `nvidia.com/gpu`** (carries `amd.com/gpu.count`) |

The last column describes **pre-existing** capacity/labels from an unrelated PoC on this cluster and
has **no bearing on leasable inventory** — all three nodes are equally leasable via
`poc.llm-d.ai/gpu-count` (§4.5). Nothing in this repo reads the vendor labels.

Installed: **KEDA 2.20.2** (`keda` ns), **kube-prometheus-stack** (`monitoring` ns) with
`serviceMonitorSelector: {}` and `serviceMonitorNamespaceSelector: {}` — i.e. **it scrapes
ServiceMonitors in every namespace, no labels required**.
Prometheus URL: `http://prometheus-kube-prometheus-prometheus.monitoring.svc.cluster.local:9090`.

**Not installed: Kueue.** Install `v0.19.4` (API group `kueue.x-k8s.io/v1beta2`).

Do **not** disturb namespace `llm-d-optimized-baseline` (an unrelated running PoC). Its
ScaledObject `optimized-baseline-nvidia-gpu-vllm-token-aware` is a useful reference for the
KEDA + Prometheus trigger pattern.

## 4. Architecture

```
                 ┌──────────────┐  scrape /metrics   ┌────────────┐
                 │  app-a pods  │◀───────────────────│ Prometheus │
                 │ (warm & hot) │                    └─────┬──────┘
                 └──────┬───────┘                          │ PromQL
                        │ POST /lease, DELETE /lease       ▼
                        │ GET  /state              ┌───────────────┐
                        │                          │ KEDA Scaled-  │
                 ┌──────┴────────────┐             │ Object → HPA  │
                 │  gpu-lease-        │             └──────┬────────┘
                 │  controller        │  scale replicas    │
                 │  (this PoC)        │◀───────────────────┘
                 └──────┬─────────────┘
                        │ create/delete GPULease (CRD)
                        ▼
                 ┌────────────────────┐  Workload   ┌──────────────────┐
                 │  GPULease reconciler│───────────▶│ Kueue ClusterQueue│
                 └────────────────────┘  admitted?  │ 1 flavor per node │
                                                    └──────────────────┘
```

Two reconcilers, one binary:

1. **WarmPool reconciler** — watches annotated `Deployment`s (+ their Pods, + `GPULease`s).
   Enforces the warm-count invariant. Decides *which* pod to promote/demote.
2. **GPULease reconciler** — owns the lifecycle of one `(node, gpuID)` lease: creates the Kueue
   `Workload`, waits for admission, calls the pod's REST API, tracks state.

### 4.1 Why Kueue is modeled with a standalone `Workload`

The deployment's pods must **not** be gated by Kueue ("no resource claim"). So the lease is a
*separate* object. We create a standalone `kueue.x-k8s.io/v1beta2 Workload` (a documented pattern —
Kueue's own docs show a `Workload` sample with no owner reference) whose single podSet:

- requests `poc.llm-d.ai/gpu: 1`,
- sets `nodeSelector: {kubernetes.io/hostname: <pod's node>}`.

Kueue **skips any ResourceFlavor whose `nodeLabels` are incompatible with the podSet's
nodeSelector**, so pinning the hostname makes exactly one flavor eligible → the lease is charged
against *that node's* quota. If that node's quota is exhausted the Workload is never admitted →
this is precisely the required "all GPUs used on a node" failure. No pod is ever created for the
Workload; it exists only to hold quota.

Kueue provides **quota**; it does not provide **identity**. The controller assigns the concrete
`gpuID` itself (§4.4).

### 4.2 CRD: `GPULease` (the only CRD)

Group/version `poc.llm-d.ai/v1alpha1`, **cluster-scoped** (a GPU is a cluster-level resource).
Name is **derived**: `<nodeName>-gpu-<gpuID>`, e.g. `kind-wva-gpu-cluster-worker-gpu-2`.
This makes etcd itself the mutual-exclusion mechanism for `(node, gpuID)`: a duplicate allocation
fails with `AlreadyExists`.

```go
type GPULeaseSpec struct {
    NodeName string `json:"nodeName"`
    GPUID    int32  `json:"gpuID"`
    GPUModel string `json:"gpuModel,omitempty"` // informational, from node label
    // Pod that must run on NodeName and receives the lease.
    ClaimRef      ObjectRef `json:"claimRef"`
    DeploymentRef ObjectRef `json:"deploymentRef"`
    LocalQueueName string   `json:"localQueueName"`
}
type ObjectRef struct {
    Namespace string    `json:"namespace"`
    Name      string    `json:"name"`
    UID       types.UID `json:"uid,omitempty"`
}
type GPULeaseStatus struct {
    Phase        GPULeasePhase `json:"phase,omitempty"` // Pending|Admitted|Activating|Bound|Failed|Releasing
    WorkloadName string        `json:"workloadName,omitempty"`
    Message      string        `json:"message,omitempty"`
    AdmittedAt   *metav1.Time  `json:"admittedAt,omitempty"`
    BoundAt      *metav1.Time  `json:"boundAt,omitempty"`
    Conditions   []metav1.Condition `json:"conditions,omitempty"`
}
```

Condition types: `QuotaReserved`, `PodActivated`, `Ready`.
Failure reasons (exact strings, used by metrics and tests):
`QuotaExhaustedOnNode`, `NoFreeGPUOnAnyPodNode`, `PodUnreachable`, `ActivationTimeout`,
`PodGone`, `NodeMismatch`.

`printcolumn` markers: `NODE`, `GPU`, `PHASE`, `POD`, `DEPLOYMENT`, `AGE` — `kubectl get gpuleases`
is a primary demo surface, so make it read well.

Finalizer `poc.llm-d.ai/release-lease` guarantees the Kueue `Workload` is deleted (quota freed)
before the `GPULease` disappears. The `Workload` also carries an `ownerReference` to the `GPULease`
as a GC safety net (a namespaced dependent may have a cluster-scoped owner — this is legal).

### 4.3 Deployment opt-in (annotations, no second CRD)

The controller "watches these deployments" literally: it reconciles Deployments carrying

| Annotation | Default | Meaning |
|---|---|---|
| `gpu-lease.llm-d.ai/managed` | — | `"true"` to opt in (required) |
| `gpu-lease.llm-d.ai/warm-replicas` | `2` | the invariant: desired warm pod count |
| `gpu-lease.llm-d.ai/min-hot-replicas` | `1` | floor on hot pods; wins over the warm target |
| `gpu-lease.llm-d.ai/local-queue` | `<deployment>-gpu` | Kueue LocalQueue to charge |
| `gpu-lease.llm-d.ai/pod-capacity-rps` | `10` | per-hot-pod capacity (docs/metrics only) |
| `gpu-lease.llm-d.ai/activation-timeout` | `120s` | pod warmup budget |
| `gpu-lease.llm-d.ai/admission-timeout` | `60s` | Kueue admission budget before declaring failure |

### 4.4 Pod state: label + annotations

State is a **label** (not an annotation) because Services and Prometheus must select on it:

`gpu-lease.llm-d.ai/state: warm | activating | hot | releasing`

Annotations (controller-owned): `gpu-lease.llm-d.ai/lease`, `/gpu-id`, `/gpu-model`,
and `controller.kubernetes.io/pod-deletion-cost` = `100` for hot / `-100` for warm.

**`pod-deletion-cost` is load-bearing**: it makes the ReplicaSet delete *warm* pods first on
scale-down, so KEDA scaling down does not tear down a GPU-holding pod. Step 4 of the demo depends
on it.

The Deployment's pod template must ship with `gpu-lease.llm-d.ai/state: warm` so every new replica
is born warm.

**Traffic**: `Service <app>` selects `gpu-lease.llm-d.ai/state=hot` → warm pods receive no traffic.
A second headless `Service <app>-all` selects all pods, for scraping.

### 4.5 GPU inventory and ID allocation

Single source of truth: **node labels**, applied by `hack/setup-nodes.sh`.

- `poc.llm-d.ai/gpu-count: "4"` — leasable GPUs (IDs `0..count-1`)
- `poc.llm-d.ai/gpu-model: "H100"`

Kueue `ResourceFlavor` quota per node **must equal** `gpu-count`.

> **Two sources of truth — Kueue is authoritative.** `hack/setup-nodes.sh` writes only the node
> labels; it does **not** generate `deploy/kueue/cluster-queue.yaml` (an earlier draft of this
> section claimed it did). The `nominalQuota` values in that file and the `gpu-count` labels must
> therefore be kept in sync **by hand**.
>
> Our inventory (`internal/controller/inventory`) is an *optimization*: it avoids creating Workloads
> that Kueue would certainly reject. It must stay **permissive relative to Kueue, never stricter**:
> - label overstates quota → the WarmPool creates a Workload, Kueue refuses, we surface
>   `QuotaExhaustedOnNode`. Visible, diagnosable.
> - label *understates* quota → we never even try, and capacity silently disappears with no error
>   anywhere. This is the failure mode to fear, and the reason the sync direction matters.
>
> This asymmetry is also what makes demo step 7 work: inflating a node's `gpu-count` above its real
> `nominalQuota` is the way to exercise Kueue-side rejection rather than our own precheck.

**All three nodes are leasable.** Each carries `gpu-count: "4"` and has a matching flavor with
`nominalQuota: 4`, and none is tainted, so the control-plane node is schedulable too. The app
Deployments therefore carry **no `nodeSelector`**: pods must be free to land on any node, both so
worker2's quota is usable and so warm pods can end up on a node with no free GPU — which is exactly
how `NoFreeGPUOnAnyPodNode` is demonstrated. The vendor labels present on this cluster
(`nvidia.com/gpu.count`, `amd.com/gpu.count`) belong to an unrelated PoC; nothing here reads them.
`gpu-model` differs per node (H100 / A100 / MI300X) on purpose, to prove the model string is
plumbed through to the pod annotation.
The script also patches `status.capacity["poc.llm-d.ai/gpu"]` on each node for realism/visibility
(harmless; no real pod ever requests it, and it is lost on kubelet restart — documented).

Allocation: free ID = lowest index in `[0, count)` not held by an existing `GPULease` on that node.

### 4.6 Reconcile algorithm (WarmPool)

```
R          = ready pods of the Deployment
desiredWarm = ann(warm-replicas)      // 2
minHot      = ann(min-hot-replicas)   // 1
targetHot   = clamp(len(R) - desiredWarm, minHot, len(R))
held        = pods with state in {hot, activating}

if len(held) < targetHot:  promote (targetHot - len(held)) warm pods
if len(held) > targetHot:  demote  (len(held) - targetHot) hot pods
```

Deriving `targetHot` from actual replica count (rather than counting warm directly) makes the loop
idempotent and correct in both directions, and self-heals when KEDA deletes a hot pod.

**Promotion candidate order**: warm pods whose node has a free GPU, sorted by (node free count
desc, pod age asc). If **no** warm pod sits on a node with a free GPU → emit event
`NoFreeGPUOnAnyPodNode`, bump metric, requeue in 30s. Never delete or evict a pod to make room.

**Demotion order**: newest hot pod first (LIFO) — least disruptive.

Promotion: allocate ID → create `GPULease` → label pod `activating`.
Demotion: label pod `releasing` → `DELETE /lease` → delete `GPULease` → label pod `warm`.

**Garbage collection**: a `GPULease` is released when its `claimRef` pod is gone, no longer Ready,
no longer part of the Deployment, or has moved node (`NodeMismatch`).

### 4.7 GPULease reconciler state machine

```
Pending    → create Workload (queueName, podSet requests poc.llm-d.ai/gpu:1, nodeSelector hostname)
           → wait for Workload condition Admitted=True
           → timeout(admission-timeout) ⇒ Failed/QuotaExhaustedOnNode (delete Workload, pod→warm)
Admitted   → POST /lease to pod  ⇒ Activating   (unreachable ⇒ Failed/PodUnreachable)
Activating → poll GET /state until "hot"
           → timeout(activation-timeout) ⇒ Failed/ActivationTimeout
Bound      → pod labeled hot, deletion-cost=100. Steady state.
Releasing  → DELETE /lease, delete Workload, remove finalizer.
```

Read the assigned flavor back from `status.admission.podSetAssignments[0].flavors` to prove the
lease landed on the intended node, and record it in the `GPULease` status message.

**Failure reasons are our vocabulary, and they must stay distinguishable.** Kueue rejecting a
Workload as `Inadmissible` (missing LocalQueue/ClusterQueue, or no flavor matching the pinned node)
maps to `QueueMisconfigured`, **not** `QuotaExhaustedOnNode`, and is surfaced immediately without
waiting out `admission-timeout` — waiting cannot fix a manifest. These strings are the `reason` label
on `gpulease_lease_acquire_failures_total`, and the operator response differs completely (edit YAML
vs. wait for / add capacity), so conflating them would make the metric useless.

**`Failed` leases are retained, then reclaimed** (`DefaultFailedLeaseRetention`, 60s; owned by the
WarmPool GC pass). This is not cosmetic bookkeeping — it is required for correctness:

- `inventory.usedGPUIDs()` counts **every** `GPULease` on a node regardless of phase, and a lease's
  name (`<node>-gpu-<id>`) occupies that `(node, gpuID)` pair in etcd.
- A `Failed` lease's pod has been reverted to warm and is alive, Ready and on the right node, so it
  is **not** an orphan under `leaseOrphanReason`'s pod-identity rules.
- Without an explicit sweep, every failed promotion would therefore pin one GPU id forever. Since
  §2 step 6 provokes a failure deliberately, repeating the demo would consume a node's ids until
  promotions there reported `NoFreeGPUOnAnyPodNode` instead of the real cause — the demo would
  silently stop working, in a way that looks like a controller bug.

Retention rather than "skip `Failed` leases in the inventory" is deliberate: it keeps exactly one
writer of the `(node, gpuID)` namespace. Skipping them would open a window where inventory thinks an
id is free while an etcd object still holds its name, surfacing as `AlreadyExists` churn during
promotion. The window also keeps the failure observable in `kubectl get gpuleases` long enough to be
the demo's evidence and a human's debugging signal.

### 4.7.1 Verified Kueue behaviour (Phase 0 spike — binding facts)

The standalone-`Workload` approach is **validated on the live cluster**; the `batch/v1 Job` fallback
is NOT needed and must not be implemented. Full report in `docs/kueue-spike.md`. Binding facts:

- Assigned flavor path is exactly `status.admission.podSetAssignments[0].flavors["poc.llm-d.ai/gpu"]`.
- **Admitted**: conditions `QuotaReserved=True` (reason `QuotaReserved`) and `Admitted=True`
  (reason `Admitted`). No pod is ever created. Quota is returned ~1.2s after Workload deletion.
- **Over quota**: `QuotaReserved=False` with reason **`Pending`** — *not* a Kueue reason named
  `QuotaExhaustedOnNode`. Message form:
  `couldn't assign flavors to pod set main: flavor h100-control-plane doesn't match node affinity,
  ... insufficient unused quota for poc.llm-d.ai/gpu in flavor a100-worker, 1 more needed`.
- **Bad queue**: `QuotaReserved=False` with reason **`Inadmissible`**, message
  `LocalQueue <name> doesn't exist`. Handle as a distinct, non-retryable-by-waiting error.

> The failure reasons in §4.2 (`QuotaExhaustedOnNode`, …) are **this PoC's own vocabulary on
> `GPULease.status`**. The controller derives them from Kueue's boolean `QuotaReserved` plus the
> `admission-timeout`; it must **never** string-match Kueue's `reason`/`message` text.

- **`spec.podSets` is fully immutable** after create (webhook `vworkload.kb.io` rejects any change,
  including `count` and nested `nodeSelector`), and `spec.queueName` is immutable once quota is
  reserved. The controller must therefore **always delete + recreate, never patch** a Workload.
- A namespaced `Workload` owned by a cluster-scoped object is accepted, and deleting the owner does
  trigger real GC of the Workload — the §4.2 safety net is confirmed to work.

## 5. Workload simulator (`cmd/workload`)

A tiny Go "inference server" standing in for vLLM. Listens on `:8000`.

| Route | Behaviour |
|---|---|
| `POST /lease` | body `{leaseName,node,gpuID,gpuModel}`. **409** if node ≠ own `NODE_NAME` (downward API). Else `202`, start async warmup (`WARMUP_SECONDS`, default `10`) → state `hot`. |
| `DELETE /lease` | teardown (`TEARDOWN_SECONDS`, default `2`) → state `warm`. Idempotent. |
| `GET /state` | `{state,leaseName,node,gpuID}`; `state ∈ warm|activating|hot`. |
| `POST /infer` | `200` after simulated latency when hot; **503** `no GPU lease` when warm. |
| `GET /healthz`, `/readyz` | always `200` — **warm pods are Ready**; readiness must not depend on hotness or KEDA's replica math breaks. |
| `GET /metrics` | Prometheus. |

Metrics (all labeled `deployment`, `pod`, `node`):

- `gpulease_pod_hot` `1|0`
- `gpulease_pod_capacity_rps` — `POD_CAPACITY_RPS` when hot, `0` when warm
- `gpulease_pod_gpu_id`
- `gpulease_demand_rps{deployment}` — **deployment-wide** demand, see below
- `gpulease_requests_total{code}`, `gpulease_activation_seconds`

**Demand injection.** Determinism matters more than realism for a demo, so demand is a dial, not a
load test: each pod **watches ConfigMap `gpu-lease-demand`** in its namespace (key = deployment
name, value = rps) and republishes it as `gpulease_demand_rps`. Every pod reports the same value,
so the PromQL uses `max by (deployment)`, not `sum`. Changing demand is instant and exact:

```sh
kubectl -n gpu-lease-poc patch cm gpu-lease-demand --type merge -p '{"data":{"app-a":"25"}}'
```

The real `/infer` path stays available for a traffic-driven variant (documented in `docs/demo.md`,
not the default).

## 6. KEDA wiring

One `ScaledObject` per deployment. The triggers deliberately know nothing about warm/hot — a KEDA
[`advanced.scalingModifiers`](https://keda.sh/docs/2.20/reference/scaledobject-spec/#advancedscalingmodifiers)
formula divides each demand signal by *per-hot-pod capacity* and adds the warm buffer as one more
term, so every term of the formula is already a replica count:

```yaml
minReplicaCount: 4          # 2 hot + 2 warm floor
maxReplicaCount: 10
pollingInterval: 10
advanced:
  scalingModifiers:
    formula: "demand_rps / 10.0 + warm_desired"   # 10.0 = pod-capacity-rps
    target: "1"                                   # => ceil(formula / 1)
    activationTarget: "0"
    metricType: AverageValue
triggers:
- type: prometheus
  name: demand_rps            # underscores: names are formula identifiers (see below)
  metadata:
    serverAddress: http://prometheus-kube-prometheus-prometheus.monitoring.svc.cluster.local:9090
    query: |
      max(gpulease_demand_rps{namespace="gpu-lease-poc",deployment="app-a"}) or vector(0)
    threshold: "1"            # inert under scalingModifiers; capacity is the divisor above
- type: prometheus
  name: warm_desired          # the warm-replicas annotation, not a constant
  metadata:
    serverAddress: http://prometheus-kube-prometheus-prometheus.monitoring.svc.cluster.local:9090
    query: |
      max(gpulease_pool_warm_desired{workload_namespace="gpu-lease-poc",deployment="app-a"}) or vector(2)
    threshold: "1"
```

`target: "1"` with `metricType: AverageValue` makes the HPA compute `ceil(formulaValue / 1)`, and
`warmDesired` is an integer, so `ceil(x + n) == ceil(x) + n` gives exactly

```
replicas = ceil(demand / C) + warmReplicas = hotPodsNeeded + warmReplicas
```

demand=20, C=10 → **4** (2 hot + 2 warm). demand=25 → **5** (3 hot + 2 warm). Exactly the scenario.

**Why the formula rather than folding the warm term into PromQL.** The mechanical alternative —
`(demand or vector(0)) + (warmReplicas * C)` with `threshold: C`, which is what this PoC ran
first — works, but it duplicates two constants into every query. The warm count is a *replica
count* that has to be written pre-multiplied by capacity to survive the HPA's later division, so
the multiplier and `threshold` must be kept equal by hand, and the Deployment's
`gpu-lease.llm-d.ai/warm-replicas` annotation stops being the single source of truth. Under
`scalingModifiers` the division happens inside the formula, so the warm buffer is added in its own
units as its own trigger, reading the controller's `gpulease_pool_warm_desired` gauge (§7) — change
the annotation and no manifest changes. Capacity then lives in exactly one place, the divisor. With
more than one demand signal the formula's explicit `max()` also replaces the HPA's implicit
max-across-metrics, so a per-trigger warm term cannot be silently forgotten on whichever trigger
happens to dominate (`ceil` is monotonic, so `max(ceil(a), ceil(b)) == ceil(max(a, b))` — the
refactor is exact, not approximate). The `- activating` correction for promotion overshoot is
likewise expressible only in a formula, since HPA `behavior` is per-direction, not per-metric; see
`docs/proposals/keda-epp-queue-warm-pods.md`, which proposes this same shape upstream.

**KEDA specifics** (verified against v2.20.2, the version this PoC runs; `scalingModifiers` needs
≥ 2.12): trigger names become identifiers in a formula compiled with
[expr](https://github.com/expr-lang/expr), where `-` is subtraction — a trigger named
`demand-plus-overprovision` parses as `demand - plus - overprovision` and the ScaledObject is
rejected, hence the underscore names. Per-trigger `threshold` is still required by the Prometheus
scaler's metadata validation but is inert (KEDA replaces all external metric specs with a single
`composite-metric` and hands the formula raw values), so both are pinned to `"1"`; per-trigger
`metricType`/`activationThreshold` are superseded by `scalingModifiers.metricType`/`.activationTarget`
and dropped. `kubectl describe hpa` shows one `composite-metric` row instead of one row per named
trigger — individual trigger values remain on the KEDA operator's own metrics endpoint. A trigger
that errors fails the whole composite metric, so `or vector(...)` matters more, not less, than
before.

The `warm_desired` query selects on `workload_namespace`, **not** `namespace`: that gauge is scraped
from the controller-manager's metrics Service, so the `namespace` label Prometheus attaches is the
*controller's* namespace. `workload_namespace` is the label the controller exposes for the managed
Deployment's own namespace (`internal/controller/metrics.go`); it deliberately isn't called
`namespace`, since an exposed label colliding with a target label is renamed to
`exported_namespace` by Prometheus's default `honor_labels: false`.

Note both aggregations are `max(...)` with **no** `by (deployment)` clause, even though the metric
selector already pins `deployment="app-a"`. This is deliberate, not a simplification: PromQL's `or`
only drops `vector(0)`'s result when its label set exactly equals a left-hand series's label set
(default vector matching = all labels). `vector(0)` always yields one series with an **empty**
label set `{}`. `max(...)` with no `by` also collapses to `{}` (since the query is already scoped
to one deployment, no grouping is needed to get down to a single series), so once real data
exists the two `{}` series match and `or` correctly drops `vector(0)`. `max by (deployment) (...)`
instead yields `{deployment="app-a"}`, which is never equal to `{}` — so `or vector(0)` never
suppresses, and once the metric has data **both** series survive (the real one and `vector(0)`'s
zero), producing two results. KEDA's Prometheus scaler rejects any query returning more
than one element (`"...returned multiple elements"`), which surfaces as the ScaledObject's
`TriggerError`/`FailedGetExternalMetric` conditions and `kubectl get hpa` showing `<unknown>` —
this is exactly the failure this PoC hit when the query was first written with `by (deployment)`
here; see `deploy/keda/scaledobject-app-a.yaml` for the fixed, deployed version and the same
reasoning restated there. The `max by (deployment)` form is still correct — and required — for the
*unscoped, cross-deployment* reference queries in `docs/demo.md` (e.g. Grafana panels querying all
deployments at once), since those need the grouping to keep app-a/app-b as separate series; it is
only wrong here because this query is already filtered down to a single deployment.

HPA behavior tuned for a live demo: `scaleUp.stabilizationWindowSeconds: 0`,
`scaleDown.stabilizationWindowSeconds: 30`, 2 pods / 15s both directions.
ServiceMonitor `interval: 10s` so the end-to-end reaction is ~20–30s.

## 7. Repo layout

```
Makefile  PROJECT  README.md
api/v1alpha1/                 gpulease_types.go, groupversion_info.go
cmd/manager/main.go           controller binary
cmd/workload/main.go          simulator binary
internal/controller/          warmpool_controller.go, gpulease_controller.go,
                              inventory.go, kueue.go, metrics.go, podclient/
internal/workload/            server.go, state.go, demand.go, metrics.go
config/                       kustomize: crd, rbac, manager, prometheus, samples (kubebuilder-generated)
deploy/kueue/                 resource-flavors.yaml, cluster-queue.yaml, local-queues.yaml
deploy/apps/                  app-a.yaml, app-b.yaml, services, servicemonitors, demand-cm.yaml
deploy/keda/                  scaledobject-app-a.yaml, scaledobject-app-b.yaml
deploy/monitoring/            prometheus-metrics-reader-binding.yaml
hack/setup-nodes.sh           node labels + capacity + generates cluster-queue.yaml
hack/kind-load.sh  hack/demo.sh  hack/watch.sh
test/e2e/                     scenario test
docs/design.md (this)  docs/demo.md
```

Scaffold with `kubebuilder` (available locally, as are `kustomize`, `kind` v0.30, Go 1.25.1,
docker 29.5.3, helm v4). Module path `github.com/llm-d-extensions/gpu-lease`.
Images built locally and `kind load`-ed (no registry): `gpu-lease-controller:dev`,
`gpu-lease-workload:dev`, `imagePullPolicy: IfNotPresent`.

Controller: single replica, leader election on, `--warm-pool-resync=15s`,
exposes its own metrics + a ServiceMonitor:

- `gpulease_pool_hot_pods{workload_namespace,deployment}` / `_warm_pods` / `_warm_desired`
  (`workload_namespace` is the *managed Deployment's* namespace; the `namespace` label Prometheus
  attaches to these series is the controller's own — see §6)
- `gpulease_leases_active{node}` / `gpulease_node_gpus_free{node}`
- `gpulease_lease_acquire_failures_total{deployment,reason}`
- `gpulease_promotions_total` / `gpulease_demotions_total`

The metrics endpoint is the kubebuilder-scaffolded secure default: HTTPS on `:8443`,
authenticated via `TokenReview` and authorized via `SubjectAccessReview` against the
nonResourceURL `/metrics` (`cmd/manager/main.go`, controller-runtime's
`filters.WithAuthenticationAndAuthorization` — the same posture kube-rbac-proxy used to
provide). This means enabling the ServiceMonitor alone (`config/prometheus/monitor.yaml`,
pulled in by `config/default/kustomization.yaml`'s `- ../prometheus`) is not sufficient:
the scraper also needs a `SubjectAccessReview`-authorized identity. The ServiceMonitor
already has Prometheus present its own pod's ServiceAccount token
(`bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token`); what's missing
from the kubebuilder scaffold is binding that ServiceAccount to the scaffolded
`metrics-reader` `ClusterRole` (`config/rbac/metrics_reader_role.yaml` — nonResourceURLs:
`["/metrics"]`, verbs: `["get"]`), which nothing does by default. `deploy/monitoring/
prometheus-metrics-reader-binding.yaml` is that missing `ClusterRoleBinding`, granting it
to kube-prometheus-stack's Prometheus ServiceAccount (`prometheus-kube-prometheus-
prometheus`, namespace `monitoring`). It is applied directly (`kubectl apply -f`), not
folded into `config/default`'s kustomize tree: that tree's `namespace:
gpu-lease-scaffold-system` transform unconditionally rewrites `ClusterRoleBinding`
subjects' `namespace:` field (verified against the scaffolded `metrics_auth_role_binding.yaml`,
whose placeholder subject namespace `system` is rewritten the same way), which would
silently clobber a subject that is deliberately in the external `monitoring` namespace.
Without this binding the ServiceMonitor has a scrape target but every scrape is
`401`/`403`'d and the four metric families above never have data — this exact gap is what
caused `hack/demo.sh`'s steps 6/7 `gpulease_lease_acquire_failures_total` checks to see no
data before it was fixed (see the comment above `prom_query()` in `hack/demo.sh`).

## 8. Delivery phases

| Phase | Work | Parallel? |
|---|---|---|
| 0 | **Kueue spike** on the live cluster: install v0.19.4, flavors/quota, standalone `Workload` admission, hostname pinning, over-quota behaviour. Output: verified YAML + exact condition/status paths. | with 1 |
| 1 | Repo scaffold: kubebuilder project, `GPULease` API types, Makefile, Dockerfiles, RBAC. Defines the Go interfaces phases 2–4 compile against. | with 0 |
| 2 | Workload simulator + `deploy/apps` manifests | ✔ |
| 3 | GPULease reconciler + Kueue integration + inventory/allocator (needs 0 + 1) | ✔ |
| 4 | WarmPool reconciler + pod REST client + controller metrics (needs 1) | ✔ |
| 5 | Kueue/KEDA manifests, `hack/` scripts, README + `docs/demo.md` | ✔ |
| 6 | Deploy to kind, run the 7-step scenario end to end, fix fallout, e2e test | last |

File ownership is disjoint per phase to keep parallel agents from colliding; phases 2–5 must not
edit `api/` or each other's directories.

## 9. Verification

**Unit/envtest** (`make test`): `targetHot` math across scale up/down/edge cases; ID allocator
(exhaustion, gaps, `AlreadyExists` race); state-machine transitions and every failure reason;
simulator handlers incl. node mismatch → 409.

**End-to-end** (`hack/demo.sh`, against the live kind cluster): walk all 7 steps of §2, asserting
after each — `kubectl get pods -L gpu-lease.llm-d.ai/state`, `kubectl get gpuleases`,
`kubectl get workloads -n gpu-lease-poc`, `kubectl get hpa`. Success criteria:

1. steady state 2 hot + 2 warm per deployment, 4 `Bound` GPULeases, 4 admitted Workloads;
2. demand 20→25 converges to 3 hot + 2 warm within ~60s, with a *new* `Bound` lease whose
   `spec.nodeName` equals the promoted pod's `spec.nodeName`;
3. demand 25→20 returns to 2 hot + 2 warm, lease deleted, Kueue quota returned;
4. node-pinned over-scale yields `Failed`/`QuotaExhaustedOnNode` and a non-zero
   `gpulease_lease_acquire_failures_total`, with **no** pod crash-looping;
5. every hot pod's `gpu-id` is unique within its node.
