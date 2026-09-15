# Proposal: make the KEDA EPP-queue ScaledObject warm-pool aware

Target of this proposal: upstream
[`guides/workload-autoscaling/keda-epp-queue/optimized-baseline/base/scaledobject.yaml`](https://github.com/llm-d/llm-d/blob/main/guides/workload-autoscaling/keda-epp-queue/optimized-baseline/base/scaledobject.yaml)
in `llm-d/llm-d`, adapted to the warm/hot split this PoC implements (see [`../design.md` §6](../design.md#6-keda-wiring)).

## 1. The problem: the trigger assumes every replica has a GPU

Both upstream triggers use `metricType: AverageValue`, so the HPA computes

```
desiredReplicas = ceil(metricValue / threshold)
```

with `threshold` acting as *per-replica capacity* — `16` concurrent requests for
`epp-running-requests`, `1` queued request for `epp-queue-size`. That identity holds only while
**capacity per replica is a constant**, i.e. while every replica holds a GPU.

Under a warm pool it stops holding, in two distinct ways:

1. **Steady-state undercount.** Only *hot* pods hold a GPU and only hot pods are in the
   InferencePool's endpoint set, so `sum(llm_d_epp_request_running{...})` is a hot-pods-only
   number. Dividing it by per-hot-pod capacity yields *the number of hot pods needed* — but the
   HPA applies that answer to `spec.replicas`, which also has to cover the warm buffer. With
   `warm-replicas: 2` the Deployment is permanently 2 replicas short of its own invariant: the
   warm pool has nothing to promote, and the next burst pays a cold start — exactly what the warm
   pool exists to avoid.
2. **Transient overcount during promotion.** A promoted pod is `activating` for up to
   `activation-timeout` before it serves. `epp-queue-size` with `threshold: 1` and
   `scaleUp.stabilizationWindowSeconds: 0` reacts to that queue inside one 15s polling interval
   and adds replicas for a backlog that an in-flight promotion is about to absorb in seconds.
   Constant capacity hid this because there, queue growth genuinely did mean "add a pod".

The upstream config also has two warm-pool-independent sharp edges worth fixing in the same
change:

3. **Missing series is an error, not zero.** `llm_d_epp_flow_control_queue_size` has no series
   until EPP has seen traffic for that model. KEDA's Prometheus scaler treats no-data as a trigger
   error (`<unknown>` on the HPA, flapping `Ready`/`Active` conditions), not as `0`.
4. **`scaleDown` can delete hot pods.** `type: Percent, value: 100, periodSeconds: 15` lets the
   HPA go from `maxReplicaCount` to `minReplicaCount` in one step. With a warm pool that removes
   far more than the warm buffer, so it reaches GPU-holding pods; `pod-deletion-cost` biases *which*
   pods go, it does not bound *how many*.

## 2. Proposed changes

The warm buffer is expressed once, as a `+ warm_desired` term in a KEDA
[`advanced.scalingModifiers`](https://keda.sh/docs/2.20/reference/scaledobject-spec/#advancedscalingmodifiers)
formula, instead of being folded into each trigger's PromQL.

| | Upstream | Proposed | Why |
|---|---|---|---|
| `minReplicaCount` | `1` | `3` | `min-hot-replicas` (1) + `warm-replicas` (2) |
| `maxReplicaCount` | `8` | `10` | keeps the *hot* ceiling at 8 — the GPU budget — after adding the warm buffer |
| replica arithmetic | implicit, per-trigger `threshold` | explicit `scalingModifiers.formula` | fixes (1) |
| trigger names | `epp-queue-size`, `epp-running-requests` | `epp_queue_size`, `epp_running_requests` | **required**: names become formula identifiers, and `-` is subtraction (§3) |
| queries | `sum(...)` | `sum(...) or vector(0)` | fixes (3) |
| `scaleDown.policies` | `Percent 100 / 15s` | `Pods 2 / 60s` | fixes (4): one step never exceeds the warm buffer |

### Why the formula is cleaner than per-trigger PromQL

The mechanical alternative is to add `warmReplicas * threshold` to every trigger's query
(kept in [§6](#6-appendix-the-per-trigger-variant) for reference). It works, but:

- **The warm term has to be repeated in every trigger, and a miss is silent.** The HPA takes the
  max across metrics, so an uncorrected trigger just loses — until it is the one that dominates
  (queue-size does, at low running-request counts), at which point the warm buffer vanishes with no
  error anywhere. There is no single place to look to confirm the invariant is applied.
- **Every term is pre-multiplied by that trigger's capacity**, so the query mixes units: the warm
  buffer is a *replica count*, but it has to be written as `2 * 16` requests to survive the HPA's
  later division. Change `threshold` and you must remember to change the multiplier with it.

With `scalingModifiers` the division happens inside the formula, so every term is already in
replicas, and the composed value *is* the replica count:

```yaml
advanced:
  scalingModifiers:
    formula: "max(epp_queue_size / 1.0, epp_running_requests / 16.0) + warm_desired"
    target: "1"
    metricType: AverageValue
```

`target: "1"` with `AverageValue` makes the HPA compute `ceil(formula / 1)`, so:

```
replicas = ceil(max(queue/1, running/16) + warmDesired)
         = hotPodsNeeded + warmDesired
```

The second equality is exact, not approximate: `ceil` is monotonic, so
`ceil(max(a,b)) == max(ceil(a), ceil(b))` — the formula's explicit `max` reproduces the HPA's
implicit max-across-metrics — and `warmDesired` is an integer, so `ceil(x + n) == ceil(x) + n`
lets it pass through the ceiling untouched. The refactor is therefore semantics-preserving on the
demand side; the only behavioral change is the warm buffer itself.

Three further wins:

- **The warm count becomes its own trigger** (`warm_desired`, reading the controller's
  `gpulease_pool_warm_desired` gauge, which publishes the Deployment's
  `gpu-lease.llm-d.ai/warm-replicas` annotation). The annotation stays the single source of truth;
  no constant is duplicated into PromQL at all.
- **Capacity lives in exactly one place** — the divisors in the formula.
- **It is the only way to express the activation correction.** Subtracting in-flight promotions
  (`- activating`) or clamping (`max(hot_needed, min_hot) + warm`) is ordinary arithmetic in a
  formula; HPA `behavior` is per-direction, not per-metric, and cannot express either. See §5.

## 3. Gotchas, verified against KEDA v2.20.2 source

- **Trigger names become `expr` identifiers, so dashes break.** The formula is compiled with
  `expr.Compile(formula, expr.Env(triggersMap))` where `triggersMap` is keyed by each trigger's
  `name` (`apis/keda/v1alpha1/scaledobject_webhook.go`, `validateScalingModifiersFormula`). In
  [expr](https://github.com/expr-lang/expr) `epp-queue-size` is the subtraction
  `epp - queue - size`, not an identifier. **Upstream's current trigger names cannot be used in a
  formula** — rename to `epp_queue_size` / `epp_running_requests`. This is the one change that is
  mandatory rather than advisory.
- **Per-trigger `threshold` is still required but no longer used for scaling.** The Prometheus
  scaler validates its own metadata, so `threshold` must be present; but all external metric specs
  are replaced by a single `composite-metric` spec (`controllers/keda/hpa.go` — only
  CPU/memory resource specs survive alongside it), and the formula receives raw metric values.
  Leaving `threshold: "16"` next to a formula that also divides by 16 reads as double-counting, so
  set every trigger's `threshold` to `"1"` and comment it: capacity belongs to the formula.
- **`metricType` moves too.** `scalingModifiers.metricType` governs the composite metric (default
  `AverageValue`; `Utilization` is rejected). Per-trigger `metricType` becomes dead configuration —
  drop it. Likewise per-trigger `activationThreshold` is superseded by
  `scalingModifiers.activationTarget`.
- **`target` must parse as a float `> 0`**, and both `formula` and `target` are mandatory once the
  section exists.
- **`or vector(0)` is still needed.** A trigger that errors fails the whole composite metric, so
  the no-data problem (§1.3) does not go away. The alternative is
  `fallback.behavior: scalingModifiers`, which hands a failing trigger to the formula as `nil` so
  expr's `??` can supply a default — more robust, at the cost of only reacting after
  `failureThreshold` polls.
- **You lose per-trigger visibility on the HPA**: `kubectl describe hpa` shows one
  `composite-metric` row instead of two named metrics. Individual trigger values are still on the
  KEDA operator's own metrics endpoint.
- **A trigger with no value yet reaches the formula as `nil`, not as an error.** Observed once on
  2.20.2 at the moment the trigger set was swapped (`error trying to run custom formula: invalid
  operation: <nil> / float64`): the scaler cache had not yet produced a value for the renamed
  trigger. It self-healed on the next poll and the HPA never lost its value, but it is another
  reason to keep `or vector(...)` on every query, and the reason `fallback.behavior:
  scalingModifiers` + expr's `??` is the more robust option for a production config.
- **Requires KEDA ≥ 2.12.0** (formula-based evaluation, upstream `#4998`). This PoC runs 2.20.2.

This shape is verified end-to-end on this PoC's kind cluster (KEDA 2.20.2) with the two-trigger
form in `deploy/keda/scaledobject-app-a.yaml`: the HPA reports a single `composite-metric`,
`ceil(demand/C) + warm_desired` matches the replica count at each step, and raising the
`warm-replicas` annotation from 2 to 3 moved the Deployment from 4 replicas (2 hot + 2 warm) to 5
(2 hot + 3 warm) with no manifest change.

## 4. Proposed file

```yaml
# Copy the Prometheus CA into this namespace as the keda-prometheus-auth Secret
# before applying this file. Extend the TriggerAuthentication for bearer, mTLS,
# basic, or workload-identity authentication when required by your environment.
apiVersion: keda.sh/v1alpha1
kind: TriggerAuthentication
metadata:
  name: keda-prometheus-auth
  namespace: llm-d-optimized-baseline
spec:
  secretTargetRef:
    - parameter: ca
      name: keda-prometheus-auth
      key: ca.crt
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: optimized-baseline-keda-epp
  namespace: llm-d-optimized-baseline
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    # Optimized Baseline example; replace with the Deployment to scale.
    # This Deployment must carry the gpu-lease.llm-d.ai/* annotations (see the guide's
    # prerequisites) or the warm_desired term below overprovisions replicas that
    # nothing ever promotes.
    name: optimized-baseline-nvidia-gpu-vllm-decode
  pollingInterval: 15
  # Applies to scale-to-zero. The generated HPA behavior below controls
  # ordinary scale-down while minReplicaCount is 1 or greater.
  cooldownPeriod: 300
  # min-hot-replicas (1) + warm-replicas (2). Never scales below the warm floor.
  minReplicaCount: 3
  # 8 hot (the GPU budget, i.e. the old maxReplicaCount) + 2 warm.
  maxReplicaCount: 10
  advanced:
    horizontalPodAutoscalerConfig:
      name: keda-hpa-optimized-baseline
      behavior:
        scaleUp:
          stabilizationWindowSeconds: 0
          policies:
            - type: Percent
              value: 100
              periodSeconds: 15
        scaleDown:
          stabilizationWindowSeconds: 300
          # Pods, not Percent: a Percent-100 policy can cut from maxReplicaCount to
          # minReplicaCount in one step, which removes more than the warm buffer and so
          # reaches GPU-holding pods. pod-deletion-cost decides *which* pods go, not how
          # many. Keep value <= warm-replicas.
          policies:
            - type: Pods
              value: 2
              periodSeconds: 60
    # Composes the triggers below into a single composite-metric. Each demand signal is
    # divided by its own per-hot-pod capacity, so every term -- including warm_desired --
    # is a replica count, and target: "1" makes the HPA's ceil(value/target) yield
    # hotPodsNeeded + warmDesired directly.
    #
    # Capacity constants live HERE and nowhere else. The triggers' own `threshold` fields
    # are inert under scalingModifiers (see the guide) and are pinned to "1" so they cannot
    # be mistaken for a second, conflicting source of truth.
    #
    # Trigger names are formula identifiers: they must be underscore-separated, because
    # expr parses `epp-queue-size` as the subtraction `epp - queue - size`.
    scalingModifiers:
      formula: "max(epp_queue_size / 1.0, epp_running_requests / 16.0) + warm_desired"
      target: "1"
      activationTarget: "0"
      metricType: AverageValue
  triggers:
    # Queue depth and running requests are both hot-pods-only numbers: EPP routes only to
    # endpoints in the InferencePool, which the guide's prerequisites restrict to
    # gpu-lease.llm-d.ai/state=hot pods.
    #
    # `or vector(0)`: neither series exists until EPP has served traffic for this model, and
    # a trigger returning no data is an error that fails the whole composite metric -- not a
    # zero. Keep every aggregation free of a `by (...)` clause: `or` only suppresses its right
    # operand on an exact label-set match, and vector() is always `{}`.
    - type: prometheus
      name: epp_queue_size
      metadata:
        # TLS-enabled bundled kube-prometheus-stack example; replace when the
        # Prometheus service, protocol, or authentication setup differs.
        serverAddress: https://llmd-kube-prometheus-stack-prometheus.llm-d-monitoring.svc.cluster.local:9090
        query: >-
          sum(llm_d_epp_flow_control_queue_size{namespace="llm-d-optimized-baseline",service="optimized-baseline-epp",model_name="Qwen/Qwen3-32B"})
          or vector(0)
        # Inert under scalingModifiers; real capacity is the divisor in the formula.
        threshold: "1"
      authenticationRef:
        name: keda-prometheus-auth
    - type: prometheus
      name: epp_running_requests
      metadata:
        serverAddress: https://llmd-kube-prometheus-stack-prometheus.llm-d-monitoring.svc.cluster.local:9090
        query: >-
          sum(llm_d_epp_request_running{namespace="llm-d-optimized-baseline",service="optimized-baseline-epp",model_name="Qwen/Qwen3-32B"})
          or vector(0)
        # Inert under scalingModifiers; real capacity is the divisor in the formula.
        threshold: "1"
      authenticationRef:
        name: keda-prometheus-auth
    # The warm-pool invariant, read from the gpu-lease controller's gauge rather than
    # hardcoded, so the Deployment's gpu-lease.llm-d.ai/warm-replicas annotation stays the
    # single source of truth. `or vector(2)` covers only the window before the controller has
    # reconciled the Deployment once.
    #
    # NOTE the selector is (workload_namespace, deployment), not (namespace, deployment): this
    # gauge is scraped from the gpu-lease controller-manager's metrics Service, so the
    # `namespace` label Prometheus attaches is the *controller's* namespace, not the workload's.
    # `workload_namespace` is the label the controller exposes for the managed Deployment's own
    # namespace; it is deliberately not called `namespace`, since an exposed label that collides
    # with a target label is renamed to `exported_namespace` by Prometheus's default
    # honor_labels: false.
    - type: prometheus
      name: warm_desired
      metadata:
        serverAddress: https://llmd-kube-prometheus-stack-prometheus.llm-d-monitoring.svc.cluster.local:9090
        query: >-
          max(gpulease_pool_warm_desired{workload_namespace="llm-d-optimized-baseline",deployment="optimized-baseline-nvidia-gpu-vllm-decode"})
          or vector(2)
        threshold: "1"
      authenticationRef:
        name: keda-prometheus-auth
```

Worked values, `warm-replicas: 2`:

| queue | running | upstream replicas | proposed replicas | split |
|---|---|---|---|---|
| no series | no series | `<unknown>` / trigger error | 3 (`minReplicaCount`) | 1 hot + 2 warm |
| 0 | 16 | 1 | 3 | 1 hot + 2 warm |
| 0 | 48 | 3 | 5 | 3 hot + 2 warm |
| 5 | 48 | 5 | 7 | 5 hot + 2 warm |
| 0 | 128 | 8 (`max`) | 10 (`max`) | 8 hot + 2 warm |

## 5. Not fixed by this file

- **The activation overshoot (§1.2) still needs a controller change first.** The formula is the
  right place to correct it — `... + warm_desired - activating` stops a burst that an in-flight
  promotion will absorb from also becoming a replica — but there is no metric to reference yet:
  `gpulease_pool_warm_pods` lumps `activating` and `releasing` in with `warm`. Splitting that gauge
  (or adding `gpulease_pool_activating_pods`) is the cheap prerequisite, and is worth doing
  precisely because `scalingModifiers` can then express the fix in one term.
- **`pod-deletion-cost` still has to come from the gpu-lease controller.** Without it, HPA
  scale-down picks victims by its own heuristics and can take hot pods even within a 2-pod step.
- **The InferencePool selector must exclude warm pods** (`gpu-lease.llm-d.ai/state: hot`).
  Everything above depends on the EPP metrics being hot-pods-only; if EPP routes to warm pods the
  numerators stop meaning "load on GPU-backed capacity" and the derivation collapses.

## 6. Appendix: the per-trigger variant

For KEDA < 2.12, or to avoid a composite metric, the same correction can be folded into each
query as `warmReplicas * threshold`, keeping the original `metricType: AverageValue` per trigger:

```promql
(sum(llm_d_epp_request_running{...}) or vector(0))
+
((max(gpulease_pool_warm_desired{workload_namespace="llm-d-optimized-baseline",deployment="optimized-baseline-nvidia-gpu-vllm-decode"}) or vector(2)) * 16)
```

with `threshold: "16"` retained and the multiplier `* 16` kept equal to it. This **must** be
repeated in every trigger — including `epp-queue-size`, where the multiplier is `* 1` — or the warm
buffer silently disappears whenever the uncorrected trigger is the dominant one. Trigger names may
keep their dashes here, since no formula parses them.
