# gpu-lease

> **This is a proof of concept running on a GPU-less `kind` cluster.** There is no real GPU
> hardware anywhere in this setup. "GPU count" is a node label (`poc.llm-d.ai/gpu-count`) applied
> by `hack/setup-nodes.sh`, and a "GPU lease" is a Kubernetes custom resource plus a Kueue quota
> reservation — nothing here allocates a physical device. The point is to validate the
> **control-plane pattern** (KEDA + Kueue + a small controller cooperating to move pods between
> "warm" and "hot") so it can later be pointed at a cluster with real GPUs.

## The problem

Autoscaling GPU-backed inference pods is expensive to get wrong in both directions:

- **Scale to zero / low-and-slow**: a request that arrives when no pod is holding a GPU pays the
  full cold-start cost (drivers, model load) before it can be served.
- **Always-hot**: keeping every replica holding a GPU wastes the most expensive resource in the
  cluster on idle capacity.

This PoC's answer is a **warm pool**: every replica of a Deployment is always *running* (passes
readiness, keeps its place in the Service's endpoint list machinery, counts toward HPA math), but
only some of them hold an actual GPU ("hot"). The rest ("warm") are one lease-acquisition away from
serving traffic — much cheaper than a cold start, much cheaper than staying hot. A small controller
continuously promotes warm pods to hot (and demotes hot back to warm) to track demand, while a
totally separate, ordinary-looking autoscaler decides *how many replicas* should exist at all.

## Architecture

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

Three systems, three jobs, deliberately kept ignorant of each other:

| System | Decides | Does NOT know about |
|---|---|---|
| **KEDA** | *how many replicas* a Deployment should have, from a Prometheus demand metric | warm vs. hot pods, GPUs, Kueue |
| **Kueue** | *whether a GPU lease may be granted on a given node*, as a pure quota check | pods, Deployments, replica counts — it never gates a single application pod |
| **gpu-lease controller** (this repo) | *which* warm pod gets promoted to hot (and which hot pod gets demoted), and the concrete `(node, gpuID)` identity of each lease | how many replicas should exist (that's KEDA's call) |

### Why Kueue doesn't gate the deployment pods

A natural instinct is "make the pod's own resource request the thing Kueue admits" — but that
would make every pod wait on a queue just to *start*, defeating the warm pool (a warm pod must be
`Running` and `Ready` immediately, with no GPU at all). Instead, a GPU lease is modeled as a
**separate, standalone Kueue `Workload`** with no owner reference and no pod behind it — it exists
purely to hold one unit of quota for `poc.llm-d.ai/gpu` on one specific node (via a
`nodeSelector: {kubernetes.io/hostname: <node>}` that makes exactly one `ResourceFlavor` eligible).
The controller creates one of these only when it decides to promote a pod, waits for Kueue to admit
it, and only then calls that pod's `/lease` API. If the node's quota is exhausted, the `Workload`
simply never gets admitted — Kueue enforces the *shared, per-node* GPU budget without ever knowing
a Deployment or a pod exists. See [`docs/design.md` §4.1](docs/design.md#41-why-kueue-is-modeled-with-a-standalone-workload)
for the full reasoning and [`docs/kueue-spike.md`](docs/kueue-spike.md) for the empirical
verification of this pattern against a live Kueue install.

### Warm → hot lifecycle

```
   born warm ──▶ [warm] ──promote──▶ [activating] ──ready──▶ [hot] ──demote──▶ [releasing] ──▶ [warm]
                    ▲                                                                            │
                    └────────────────────────────────────────────────────────────────────────────┘
```

- **warm**: `Running`, `Ready`, no GPU lease. Receives no application traffic (only a headless
  all-pods Service scrapes it).
- **activating**: a `GPULease` has been created and admitted by Kueue; the controller has called
  `POST /lease` on the pod and is waiting for it to report `hot`.
- **hot**: holds a `Bound` `GPULease`; the app's own `Service` routes traffic to it;
  `controller.kubernetes.io/pod-deletion-cost=100` protects it from scale-down.
- **releasing**: the controller called `DELETE /lease` and is tearing the `GPULease` down; the pod
  returns to `warm` (`pod-deletion-cost=-100` — the *next* thing KEDA deletes on scale-down, so a
  hot pod is never sacrificed to satisfy a smaller replica count).

State lives on the pod as the label `gpu-lease.llm-d.ai/state`, precisely so Services and
Prometheus can select on it — see [`docs/design.md` §4.4](docs/design.md#44-pod-state-label--annotations).

## Quickstart

Full operator runbook, exact commands, expected output at every step, and a troubleshooting
section: **[`docs/demo.md`](docs/demo.md)**.

```sh
hack/setup-nodes.sh                                   # label nodes with simulated GPU capacity
kubectl apply --server-side -f deploy/kueue/           # ResourceFlavors, ClusterQueue, LocalQueues
make install && make deploy IMG=gpu-lease-controller:dev
kubectl apply -f deploy/apps/                          # app-a, app-b, Services, demand ConfigMap
kubectl apply -f deploy/keda/                          # ScaledObjects
hack/watch.sh &                                        # live dashboard, second terminal
hack/demo.sh --all                                     # walk the 7-step scenario
```

## Deployment annotation reference

Opt a Deployment into the warm pool by adding `gpu-lease.llm-d.ai/managed: "true"` plus, optionally,
any of the following (design.md §4.3):

| Annotation | Default | Meaning |
|---|---|---|
| `gpu-lease.llm-d.ai/managed` | — | `"true"` to opt in (required) |
| `gpu-lease.llm-d.ai/warm-replicas` | `2` | the invariant: desired warm pod count |
| `gpu-lease.llm-d.ai/min-hot-replicas` | `1` | floor on hot pods; wins over the warm target |
| `gpu-lease.llm-d.ai/local-queue` | `<deployment>-gpu` | Kueue LocalQueue to charge |
| `gpu-lease.llm-d.ai/pod-capacity-rps` | `10` | per-hot-pod capacity (docs/metrics only) |
| `gpu-lease.llm-d.ai/activation-timeout` | `120s` | pod warmup budget |
| `gpu-lease.llm-d.ai/admission-timeout` | `60s` | Kueue admission budget before declaring failure |

## Useful commands for the demo

```sh
# the primary demo surface: node, gpu id, phase, claiming pod, deployment, age
kubectl get gpuleases

# pods with their warm/hot state as a column
kubectl get pods -n gpu-lease-poc -L gpu-lease.llm-d.ai/state -o wide

# what KEDA is currently doing
kubectl get scaledobject,hpa -n gpu-lease-poc

# the demand "dial" (design.md §5) — everything downstream reacts to this
kubectl get configmap gpu-lease-demand -n gpu-lease-poc -o yaml
kubectl -n gpu-lease-poc patch cm gpu-lease-demand --type merge -p '{"data":{"app-a":"25"}}'

# Kueue's view: quota reservations backing the currently-hot pods
kubectl get workloads -n gpu-lease-poc
kubectl get clusterqueue gpu-pool -o wide

# why a lease failed
kubectl get gpuleases -o jsonpath='{range .items[?(@.status.phase=="Failed")]}{.metadata.name}{" "}{.status.message}{"\n"}{end}'
kubectl get events -n gpu-lease-poc --field-selector reason=NoFreeGPUOnAnyPodNode
```

## Further reading

- [`docs/design.md`](docs/design.md) — the implementation contract: CRD schema, reconcile
  algorithms, GPU inventory/ID allocation, the full 7-step reference scenario, and KEDA/Kueue
  wiring derivations.
- [`docs/kueue-spike.md`](docs/kueue-spike.md) — the Phase 0 spike that validated the
  standalone-`Workload` pattern against a live Kueue install, including the exact condition/reason
  strings Kueue uses (distinct from this PoC's own `GPULease.status` failure vocabulary).
- [`docs/demo.md`](docs/demo.md) — the full operator runbook (install order, step-by-step demo
  commands and expected output, PromQL, troubleshooting).

## Development

This repo is a standard [kubebuilder](https://book.kubebuilder.io/) project (`make manifests`,
`make test`, `make docker-build`, etc. all work as usual — run `make help` for the full target
list). `api/v1alpha1/` holds the single `GPULease` CRD; `internal/controller/` holds the two
reconcilers (`WarmPool`, `GPULease`) described above; `cmd/workload/` is the tiny simulated
inference server pods run instead of a real model server.

## License

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
