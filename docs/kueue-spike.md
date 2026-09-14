# Phase 0 — Kueue spike findings

Verified against the live `kind-kind-wva-gpu-cluster` cluster, Kueue **v0.19.4**
(`kueue.x-k8s.io/v1beta2`, confirmed via `kubectl api-resources | grep kueue`).
All commands below were actually run; all YAML below is verbatim from what
was applied and admitted.

## 1. Approach validated: standalone `Workload` (no Job fallback needed)

The standalone-`Workload` pattern from design.md §4.1 works exactly as
documented, on every point the spike was asked to check:

- a standalone `Workload` (no `ownerReferences`, no `spec.podSets[].template`
  running anything real) is admitted by Kueue,
- hostname pinning (`nodeSelector: {kubernetes.io/hostname: <node>}`) selects
  exactly the intended `ResourceFlavor`,
- an over-quota `Workload` on a pinned node is correctly left un-admitted,
- deleting an admitted `Workload` returns quota and admits the next pending
  one, quickly,
- owner-less `Workload`s are not garbage-collected or complained about by
  Kueue over a 3m36s soak.

**The `batch/v1` Job fallback (design.md §4.7 "if standalone Workloads
misbehave") was not needed and was not built.** Phase 3 should implement the
GPULease reconciler directly against standalone `Workload` objects.

## 2. Exact status path for the assigned flavor

Design.md's assumed path is correct, verified verbatim:

```
status.admission.podSetAssignments[0].flavors["poc.llm-d.ai/gpu"]
```

Example observed value: `"a100-worker"` (the ResourceFlavor name), when the
podSet's `nodeSelector` pinned `kubernetes.io/hostname:
kind-wva-gpu-cluster-worker`.

Full `status.admission` shape observed:

```yaml
status:
  admission:
    clusterQueue: gpu-pool
    podSetAssignments:
    - name: main
      count: 1
      flavors:
        poc.llm-d.ai/gpu: a100-worker
      resourceUsage:
        poc.llm-d.ai/gpu: "1"
```

`podSetAssignments` is indexed the same order as `spec.podSets`; with a
single podSet named `main`, index `[0]` is safe to hardcode as long as the
controller only ever creates one podSet per Workload (as designed).

## 3. Exact condition type/reason/message strings

These are **Kueue's** condition strings on the `Workload` object itself —
**not** the same as the `GPULease` CRD's own failure-reason vocabulary in
design.md §4.2 (`QuotaExhaustedOnNode`, `NoFreeGPUOnAnyPodNode`, ...). See the
gotcha in §5 below: **the GPULease controller must translate Kueue's
conditions into its own reasons; it cannot just copy Kueue's `reason`
field.**

### Admitted (quota available)

```yaml
status:
  conditions:
  - type: QuotaReserved
    status: "True"
    reason: QuotaReserved
    message: "Quota reserved in ClusterQueue gpu-pool"
  - type: Admitted
    status: "True"
    reason: Admitted
    message: "The workload is admitted"
```

Corresponding Events (`kubectl get events -n gpu-lease-poc`):

```
Normal   QuotaReserved   Quota reserved in ClusterQueue gpu-pool, wait time since queued was 0s; Flavors considered: main: h100-control-plane(NoFit;flavor h100-control-plane doesn't match node affinity)
Normal   Admitted        Admitted by ClusterQueue gpu-pool, wait time since reservation was 0s
```

### Quota exhausted on the pinned node (the "6th step" failure mode)

With 4 Workloads already admitted against the `a100-worker` flavor (nominal
quota 4) and a 5th pinned to the same hostname:

```yaml
status:
  conditions:
  - type: QuotaReserved
    status: "False"
    reason: Pending
    message: >-
      couldn't assign flavors to pod set main: flavor h100-control-plane
      doesn't match node affinity, flavor mi300x-worker2 doesn't match node
      affinity, insufficient unused quota for poc.llm-d.ai/gpu in flavor
      a100-worker, 1 more needed
```

No `Admitted` condition is present at all while `QuotaReserved` is `False`.

Corresponding Event:

```
Warning   Pending   couldn't assign flavors to pod set main: flavor h100-control-plane doesn't match node affinity, flavor mi300x-worker2 doesn't match node affinity, insufficient unused quota for poc.llm-d.ai/gpu in flavor a100-worker, 1 more needed
```

**Deviation to flag for Phase 3**: Kueue's own `reason` for this case is
`Pending`, **not** `QuotaExhaustedOnNode`. `QuotaExhaustedOnNode` is a
`GPULease`-status reason the *controller* must produce itself, by detecting
`QuotaReserved.status == "False"` (or `Admitted` condition absent) after the
`admission-timeout` budget, and by pattern-matching the message for
`"insufficient unused quota for poc.llm-d.ai/gpu in flavor <the one matching
flavor>"` vs. the (unreachable, in our design) case where *no* flavor's
`nodeLabels` even matches — the latter is what should map to
`NoFreeGPUOnAnyPodNode` when demand pins pods onto a node with zero
`poc.llm-d.ai/gpu` flavor quota at all (e.g. `worker2`, which has a flavor
but the controller only requests it if a pod actually lands there — see
design.md step 7). Recommendation: **do not string-match Kueue's free-text
message** for anything beyond logging; key off `QuotaReserved` boolean status
plus elapsed time vs. `admission-timeout`, and use `status.admission` being
unset as the authoritative "still not admitted" signal.

### Missing/nonexistent LocalQueue (a related gotcha, not in the original ask but discovered while probing validation)

```yaml
status:
  conditions:
  - type: QuotaReserved
    status: "False"
    reason: Inadmissible
    message: "LocalQueue does-not-exist doesn't exist"
```

This is a **third**, distinct `QuotaReserved=False` reason (`Inadmissible`)
that the controller will hit if it ever races Workload-creation ahead of
LocalQueue-creation, or typos `spec.queueName` — it is not rejected by the
webhook, it just parks forever with this message.

### Deactivation (`spec.active: false`) — encountered incidentally

Setting `spec.active: false` on an admitted Workload adds an `Evicted`
condition (`reason: Deactivated`) without clearing the prior `QuotaReserved`/
`Admitted` conditions (they remain `True` from their original
`lastTransitionTime`, `observedGeneration` doesn't catch up cleanly in our
test even after setting `active: true` again). **Design.md's own release
flow deletes the Workload rather than deactivating it**, so this path is not
on Phase 3's critical path — but if a future change considers reusing
`spec.active` for pause/resume, budget time to verify the re-activation
condition bookkeeping more thoroughly than this spike did.

## 4. Admission timing

- **Cold admission** (quota available): `QuotaReserved` and `Admitted`
  conditions land with the **same `lastTransitionTime` as
  `metadata.creationTimestamp`**, i.e. sub-second, in every trial (6 Workloads
  created across 2 batches, all admitted within the same wall-clock second as
  creation).
- **Quota return → re-admission of a pending Workload**: measured by
  deleting an admitted Workload and polling (0.3s granularity) the pending
  one's `Admitted` condition: **≈1.1–1.4s** elapsed. This is the effective
  bound the `admission-timeout` (default 60s per design.md §4.3) needs to
  comfortably exceed.

No `AdmissionCheck`s were configured (none exist in this ClusterQueue), so
there is no external gating latency in this measurement — it is pure
Kueue-scheduler-loop latency.

## 5. Minimal Workload YAML that worked, verbatim

```yaml
apiVersion: kueue.x-k8s.io/v1beta2
kind: Workload
metadata:
  name: spike-wl-1
  namespace: gpu-lease-poc
spec:
  queueName: app-a-gpu
  podSets:
  - name: main
    count: 1
    template:
      spec:
        nodeSelector:
          kubernetes.io/hostname: kind-wva-gpu-cluster-worker
        containers:
        - name: main
          image: registry.k8s.io/pause:3.9
          resources:
            requests:
              poc.llm-d.ai/gpu: "1"
        restartPolicy: Never
```

Notes on required fields, verified empirically:

- `spec.podSets` must have **at least 1 entry** — an empty list is rejected
  at create time by the validating webhook (`vworkload.kb.io`):
  `spec.podSets: Invalid value: 0: spec.podSets in body should have at least
  1 items`.
- `spec.queueName` is **not** required by the webhook — see the
  `Inadmissible` case in §3. The controller must set it correctly itself;
  Kueue will not fail fast on a bad value.
- `template.spec.containers[].resources.requests` is where
  `poc.llm-d.ai/gpu: "1"` must go (standard pod resource request shape —
  Kueue reads the podSet's pod template like any other pod spec). No special
  Kueue-specific request field.
- No pod is ever created for a standalone Workload with no owning
  Job/controller integration — confirmed: `kubectl get pods -n
  gpu-lease-poc` was empty throughout every experiment, admitted or not.
- No labels are required on the `Workload` itself. (`kueue.x-k8s.io/queue-name`
  is a **label** used by the Job/Pod *integrations* to opt a `batch/v1 Job`
  or bare Pod into Kueue; it is irrelevant for standalone `Workload`s, which
  use the `spec.queueName` **field** instead — don't confuse the two paths.)

## 6. Gotchas for the Phase 3 controller author

1. **`spec.podSets` is immutable after creation, as a whole.** Both of these
   were rejected by the same webhook, with the same error shape:
   - changing `podSets[0].count` (`2` instead of `1`):
     `Invalid value: {...}: field is immutable`
   - changing `podSets[0].template.spec.nodeSelector["kubernetes.io/hostname"]`
     (i.e. trying to re-pin the node in place):
     `Invalid value: {...}: field is immutable`

   **Conclusion: the GPULease reconciler can never patch a Workload's
   podSet/nodeSelector/resource request. Any change to what is being
   requested (different node, different GPU count) requires delete +
   recreate of the Workload**, not a patch. This matches design.md §4.7's
   state machine, which never patches an existing Workload's podSet — it
   only creates and deletes — so no design change is needed, but this
   confirms the assumption was correct and load-bearing.

2. **`spec.queueName` is immutable once quota is reserved**: `queueName is
   immutable while workload quota reserved`. Same implication as #1 — to
   move a lease request to a different LocalQueue, delete+recreate.

3. **`spec.active` is the one field that *is* mutable post-admission** (used
   by Kueue itself for suspend/resume-style deactivation), but its condition
   bookkeeping on reactivation was not clean in this spike (see §3) — avoid
   relying on it; design.md's delete-based release flow sidesteps this
   entirely and should be kept as-is.

4. **Kueue does not garbage-collect owner-less Workloads, and logs no
   warning about them.** Left an owner-less, admitted Workload alone for
   3m36s: it stayed `Admitted`, no pod ever appeared, and
   `kubectl -n kueue-system logs deployment/kueue-controller-manager` showed
   no warning/error mentioning it or "owner" (the only `error`-level log line
   in the whole spike was an unrelated, expected one: `Skipping admission
   check controller setup: Provisioning Requests not supported` — this
   kind cluster has no cluster-autoscaler CRD installed, which is fine and
   expected, not a spike failure).

5. **A namespaced `Workload` accepts an `ownerReference` to a cluster-scoped
   object, and native Kubernetes GC honors it.** Tested with a disposable
   `ClusterRole` as owner (used instead of a real `Node`, to avoid
   destabilizing the shared kind cluster — deleting a `Node` to prove the
   point was judged too destructive; a `ClusterRole` proves the identical
   principle: a cluster-scoped owner type with a namespaced dependent):
   - `kubectl apply` of the Workload with a correct `apiVersion/kind/name/uid`
     `ownerReferences` entry pointing at the `ClusterRole` **succeeded**
     (API server does not reject cross-scope owner references).
   - Deleting the `ClusterRole` caused the standard Kubernetes garbage
     collector to delete the dependent `Workload` within a few seconds.
   - **This directly confirms design.md §4.2's claim** ("a namespaced
     dependent may have a cluster-scoped owner — this is legal") and means
     the `Workload → GPULease` ownerReference GC safety net will work as
     designed, with no special-casing needed.

6. **The two "no fit" failure sub-cases produce different messages, useful
   for future diagnostics but not for control flow** (see §3's
   recommendation to key off boolean status, not message text):
   - the pinned-but-full case names the flavor and says `insufficient unused
     quota for <resource> in flavor <name>, N more needed`;
   - Kueue also always reports the *other*, hostname-incompatible flavors in
     the same message (`flavor <X> doesn't match node affinity`) — this is
     how the spike **verified** hostname pinning correctly excludes the
     unrelated flavors (h100-control-plane, mi300x-worker2) rather than just
     assuming it from the admitted flavor name.

7. **`registry.k8s.io/pause:3.9`** was used as the (never-scheduled)
   container image in every spike podSet; this is harmless since no pod is
   ever created for a standalone Workload, but keep it as the placeholder
   image if Phase 3 ever needs one for schema validity.

## 7. Cluster state left behind (per task instructions)

Left installed/in place: Kueue v0.19.4 controller (`kueue-system` ns), node
labels `poc.llm-d.ai/gpu-count`/`poc.llm-d.ai/gpu-model` + `status.capacity`
patch on all 3 nodes, namespace `gpu-lease-poc`, 3 `ResourceFlavor`s
(`h100-control-plane`, `a100-worker`, `mi300x-worker2`), `ClusterQueue`
`gpu-pool`, `LocalQueue`s `app-a-gpu`/`app-b-gpu`. All experiment `Workload`s
and the disposable `ClusterRole` were deleted; `gpu-pool`'s
`flavorsUsage`/`admittedWorkloads` are back to `0` on every flavor.

Reproduction fixtures (the exact YAML used for the experiments above, kept
for reference) live in `hack/spike-workload.yaml`, `hack/spike-workloads-5.yaml`,
`hack/spike-owner-test.yaml`, `hack/spike-invalid-empty-podsets.yaml` — these
are throwaway/exploratory, not part of the Phase 3 contract; the contract
files are `deploy/kueue/*.yaml` and `hack/setup-nodes.sh`.
