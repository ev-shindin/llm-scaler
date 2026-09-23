# llm-scaling-manager: project proposal

An analytical scaling manager for llm-d inference: multi-model, variant- and
P/D-aware — scaling, warm capacity and placement under one GPU budget.

It began as a fork of llm-d's Workload Variant Autoscaler
(`llm-d/llm-d-workload-variant-autoscaler`, the path still in this tree's
`go.mod`) and has diverged substantially since.

## 1. The problem

Autoscaling an LLM fleet is not autoscaling a web service, for one reason: **a
new replica is not available when it is scheduled, it is available when the model
is loaded.** Everything else follows from that.

Measured on this fleet, two models behind one gateway on 8×H200:

| | |
| --- | ---: |
| p95 TTFT during a cold rise, 8B model | **5.1 – 8.8 s** |
| the same rise with a warm replica available | **0.11 – 0.83 s** |
| GLM-5.2-FP8 start, warm node, weights read | **192 s** |
| ...of which the weights are | **40 s** |
| ...the same start with a cold JIT cache | **463 s** |

Two consequences the usual answers do not address:

- **Faster storage does not fix it.** Weights are a fifth of a GLM start. A peer
  transfer that made the weight term free would take a scale-up from 192 s to
  ~152 s. The rest is process spawn, import, memory profiling, kernel warmup and
  CUDA-graph capture.
- **Threshold autoscaling cannot see saturation coming.** CPU and even
  request-rate triggers move after the queue has already formed, and the
  stabilization window then holds the replicas long after the burst.

## 2. What this project does

Three things, in decreasing order of how settled they are.

**Decides replica counts from a closed-form capacity model.** Request rate and
per-server performance from Prometheus, an inter-token-latency fit, and a
fair-share allocation across every variant of every model at once, within
whatever GPU budget a declared limiter allows. Not a forecaster and not a
threshold ladder — a model that can be read, argued with, and checked against the
numbers it produced.

**Publishes, rather than actuates.** It implements the KEDA external-scaler gRPC
contract; KEDA owns the HPA and writes the scale subresource. That is why this is
a *scaling manager* and not an autoscaler: the decision is ours, the actuation is
Kubernetes'.

**Manages capacity, not only replica counts.** A shared warm pool holds
accelerators with models resident and lends them to bridge a scale-up;
scale-to-zero parks idle workloads; the limiter constrains every decision against
a GPU budget.

## 3. What is measured

Two models bursting out of phase, 4020 s and 30 720 requests per arm, one run
each, CoreWeave H200s, 2026-09-18. Full method and every comparability guard in
[the benchmark guide](guides/benchmarking/two-model-warm-pool.md); the numbers
are reproduced from [measured.md](well-lit-paths/warm-pool-bridge/measured.md).

| arm | GPU-seconds | p95 TTFT per rise |
| --- | ---: | --- |
| autoscaling alone | **12 129** | 5.1 – 8.8 s |
| + one-Pod warm pool | 14 138 (+17 %) | 0.11 – 0.83 s |
| floor of 2 replicas/model | 16 080 (+33 %) | 0.09 – 0.13 s |

**Read it honestly.** Autoscaling alone is the cheapest arm and pays for it with
five-to-nine-second rises. The warm pool buys floor-like latency for **12 % less
than the floor it replaces** — and costs **17 % more** than no insurance at all.
The pool's own accelerator is charged for the whole run, and that costs about
twice what the smaller, shorter scale-ups save.

So the claim is not "a warm pool is cheaper". It is: *if you were going to hold a
floor, hold a pool instead; if you were holding nothing and can tolerate multi-
second rises, keep holding nothing.* The pool's advantage over a floor caps at
25 % for two models and one Pod, and the lever that grows it is **more models
sharing the same Pod**, not more quiet.

Other measured results: a **437 ms model switch against a ~41 s cold start** for a
pool Pod serving real gateway traffic; peer weight transfer between replicas
verified byte-identical and ~9× faster than reloading from storage.

## 4. What did not work

Published because the negative results were expensive and are worth more than
another feature.

- **Sparse process snapshots do not work today.** Checkpointing an engine that
  has already paid its ~150 s of construction, then refilling weights from a
  peer, would remove the part no transfer can touch. The mechanism works on a
  single process — GPU to 0 MiB, dump, restore, identical checksum — and **hangs
  on a real multi-rank engine**: the driver's own thread spins after the copy
  completes. Fifteen experiments narrowed it to NVIDIA's checkpoint path, not
  anything vLLM can release.
- **GPU-less warm launchers cannot wake.** An unbound sleeper often cannot wake
  at all; holding the accelerator is what makes the wake possible.
- **A weights PVC is not a latency fix.** 430 MB/s measured. It avoids
  re-downloads, not cold starts.

## 5. Scope and non-goals

**In scope:** horizontal replica decisions across models, variants and P/D roles;
warm capacity; scale-to-zero; GPU-budget constraints; the KEDA external-scaler
transport.

**Not in scope:** vertical scaling and node provisioning (the cluster autoscaler
owns those); request routing and scheduling (the gateway and EPP own those);
forecasting — the model is closed-form, which is a deliberate trade of
anticipation for explainability.

## 6. Status

Running on CoreWeave H200s and on OpenShift. The scaling path, warm pool,
scale-to-zero and quota limiting are built and cluster-verified; replica
reallocation across priorities is designed and not built; P/D role switching is
experimental.

Apache 2.0. The Go module path remains upstream's, so imports are unchanged.
