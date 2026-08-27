# Install a small llm-d model

## Overview

Deploys one small model — `Qwen/Qwen3-0.6B` — with an EPP and an InferencePool,
on a single GPU, so there is something real for WVA to scale. The model only:
[Install WVA in a namespace](../install-in-namespace/) is the step after it, and
the two are separate because WVA's preflight refuses a namespace with no model
servers — the model has to exist first. Use it to try WVA,
to reproduce a scaling question, or as the target for
[`make benchmark-smoke`](../benchmarking/).

It exists because there is no upstream equivalent. llm-d's own guides deploy
models that need a fleet: [Optimized Baseline][ob] — the inference-scheduling
entry point — defaults to `Qwen/Qwen3-32B` across **16 GPUs** (8 replicas, TP=2),
and the disaggregation guides target `gpt-oss-120b`. Those are the right defaults
for what those guides teach. They are the wrong ones for "I have one card and I
want to see this work".

For correctness work with no GPU at all, use
[Test WVA against a full llm-d stack](../testing-with-llm-d/) instead — it runs
simulators on kind.

## Prerequisites

- one GPU, and a node that will schedule onto it
- `kubectl`, and rights to create workloads in your namespace
- the benchmark CLI's prerequisites, which the standup installs for you

<!-- guide:env.static.namespace start -->
```bash
export NAMESPACE=<your-namespace>
export MODEL_ID=Qwen/Qwen3-0.6B
```
<!-- guide:env.static.namespace end -->

<!-- guide:prerequisites.gpu start -->
```bash
# One GPU, and a node that will schedule onto it. This deploys a real model
# server, not a simulator.
kubectl get nodes -o custom-columns=NODE:.metadata.name,GPU:.status.allocatable.'nvidia\.com/gpu' --no-headers
```
<!-- guide:prerequisites.gpu end -->

## Installation Instructions

<!-- guide:deploy.standup start -->
```bash
# Deploys the model server, its EPP and the InferencePool -- and NOTHING else.
# BENCHMARK_WVA_DEPLOY=false is what keeps the autoscaler out of it: this guide
# gets you a model to scale, and installing WVA is the next guide's job.
make benchmark-standup BENCHMARK_NAMESPACE=${NAMESPACE} MODEL_ID=${MODEL_ID}         BENCHMARK_WVA_DEPLOY=false ENVIRONMENT=openshift
```
<!-- guide:deploy.standup end -->

This deploys the model server, its EPP and the InferencePool — and stops
there. `BENCHMARK_WVA_DEPLOY=false` is what keeps the autoscaler out: this guide
gets you something to scale, and installing WVA is the next guide's job. Doing
them together hides which half failed, and WVA's own preflight refuses a
namespace with no model servers in it, so the order is not arbitrary.

### Then install WVA into that namespace

Follow [Install WVA in a namespace](../install-in-namespace/) with the same
`NAMESPACE`. In short:

```bash
export NAMESPACE=<the namespace you just used>
make setup-prereqs        # cluster admin, once per namespace
make deploy-wva           # the controller
make scaledobjects-plan WVA_DEFAULT_SO_PLAN=wva-plan.yaml
# edit wva-plan.yaml: apply: yes|no|adopt, the modelID, the replica bounds
make scaledobjects-apply WVA_DEFAULT_SO_PLAN=wva-plan.yaml
```

The ScaledObject is the registration: WVA has no watch and no listing, so until
one exists it is never called and scales nothing.

### The EPP setting the next guide will refuse without

**The EPP needs the `flowControl` feature gate.** WVA's install preflight is
fatal on its absence — `wva_require_epp_metrics` refuses to continue, and the
only way past it, `SKIP_CHECKS=true`, switches off *every* preflight check rather
than that one. So an EPP without the gate does not produce a degraded install; it
produces no install at all.

The standup above enables it for you. This matters when you bring your own EPP,
or reuse one an llm-d guide deployed, because those have it off unless you turned
it on. Check what an EPP is running with:

```bash
kubectl get cm -n ${NAMESPACE} -o yaml | grep -A3 featureGates
```

Enable it in the EPP's `EndpointPickerConfig`:

```yaml
featureGates:
- flowControl
```

Then restart the EPP. llm-d documents the gate in `guides/flow-control/tuning.md`.

**What it changes about the numbers you will see.** Flow control queues requests
at the *router* and admits only what an engine can absorb, so a backlog shows up
in the EPP rather than as `vllm:num_requests_waiting`. Read the **EPP
Flow-Control Queue** panel for queued demand, and the engine queue only for
whether an engine itself is behind.

On this guide's stack neither queue ever forms, and that is the expected result
rather than a fault: measured on Qwen3-0.6B at 10 req/s, the engine held ~115
concurrent requests with KV at 27% and both queues flat at 0. One replica of a
model this small absorbs the load outright. Queues appear only once offered
concurrency exceeds the engine's batch width (vLLM's default is 256 sequences).

Without the gate neither the queue nor scale-from-zero has anything to read.

### The one setting that matters for a small model

**`gpuMemoryUtilization` has to follow the model.** vLLM reads it as a fraction
of the card's **total** memory, not of what is free, and it then fills that
budget with KV cache. A 32B model wants 0.95 of an 80GB card. A 0.6B model —
1.12 GiB of weights — does not, and asking for 0.95 anyway makes it claim
**73.5 GiB of KV cache** and then fail:

```
Available KV cache memory: 73.53 GiB
torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 594.00 MiB.
GPU 0 has 79.18 GiB of which 500.69 MiB is free.
```

The standup sets `GPU_MEM_UTIL` to **0.90** — one value, for every model. It is
deliberately *not* lowered for small models, and the reason is worth knowing
before you reach for it: per-replica capacity IS the KV cache size, so shrinking
KV to make a small model scale sooner does the autoscaler's job in the wrong
layer. It throws away GPU you paid for to buy an effect `kvCacheThreshold`
gives you for free. Size the card for serving; tune when a replica counts as
full in the scaling policy.

The practical consequence on a small model is worth expecting. At 0.90 a 0.6B
model gets **651,328 tokens** of KV cache, and one replica then absorbs an
enormous amount of traffic without ever looking full. Measured on this guide's
own stack: 121 concurrent requests, KV utilisation peaking at **10.5%**, and WVA
holding at one replica — correctly, because by the signal it sizes on nothing
was saturated. If you want a small model to scale on a small load, lower
`kvCacheThreshold` in the scaling policy. Do not shrink the cache.

**Why it looks intermittent.** 0.95 works on an empty GPU, so this failed only
sometimes and looked like flakiness. It is not: the memory that is already gone
belongs to pods the *scheduler cannot see*. FMA launcher pods request **zero**
GPUs while holding the engine — that is the whole design — so Kubernetes places
your model onto a card it believes is empty, and vLLM then measures 77.3 of 79.18
GiB free and budgets as though it had all of it. Any co-tenant that holds memory
without requesting a GPU does the same thing.

Override it when you need to:

```bash
make benchmark-standup BENCHMARK_NAMESPACE=${NAMESPACE} MODEL_ID=${MODEL_ID} \
     IMG=${IMG} GPU_MEM_UTIL=0.50
```

## Verification

<!-- guide:verify.serving start -->
```bash
kubectl get pods -n ${NAMESPACE}
```
<!-- guide:verify.serving end -->

Both the decode pod and the EPP should be `Running`. A decode pod in
`CrashLoopBackOff` is almost always the memory budget above — check its log for
`torch.OutOfMemoryError` before looking anywhere else.

<!-- guide:verify.model start -->
```bash
# Ask the endpoint what it loaded. This is the name every request must use.
kubectl run probe -n ${NAMESPACE} --rm -i --restart=Never --quiet \
  --image=registry.k8s.io/e2e-test-images/agnhost:2.47 --command -- \
  /bin/sh -c "curl -s http://$(kubectl get svc -n ${NAMESPACE} -o name | grep epp | head -1 | cut -d/ -f2).${NAMESPACE}.svc.cluster.local:80/v1/models"
```
<!-- guide:verify.model end -->

The `id` it returns is the name every request must use. It is the *served* name,
which is not always the path the weights were loaded from.

## Benchmarking

Once everything above is deployed — and not before, since it drives real load
at the model — this is the one command that shows whether WVA is scaling it:

<!-- guide:verify.smoke start -->
```bash
# Drive load through it and snapshot the dashboard.
make benchmark-smoke NAMESPACE=${NAMESPACE}
```
<!-- guide:verify.smoke end -->

Symmetric 1024-in/1024-out at 15 req/s for five minutes, then a snapshot of the
dashboard over
exactly that window. It checks the whole chain first — KEDA, a WVA controller
managing this namespace, model servers, an EPP, a ScaledObject — and names every
gap at once rather than failing five minutes in.

For numbers you intend to compare between runs, use
[Benchmark WVA](../benchmarking/) instead: this one answers "does it scale", not
"how fast is it".

## Cleanup

<!-- guide:cleanup.teardown start -->
```bash
make benchmark-teardown BENCHMARK_NAMESPACE=${NAMESPACE}
```
<!-- guide:cleanup.teardown end -->

This removes **both halves** — the autoscaler and the model — in that order. WVA
goes first deliberately: a namespace-scoped install still creates cluster-scoped
RBAC, which deleting the namespace would strand, and a controller outliving its
workloads keeps calling a scaler for things that are gone.

To remove only the autoscaler and keep the model serving, use `make undeploy-wva`
instead, as [Install WVA in a namespace](../install-in-namespace/) describes.

Verify nothing cluster-scoped was stranded:

```bash
kubectl get clusterrole,clusterrolebinding -o name | grep -i "${NAMESPACE}"
```

## Configuration

| Parameter | Default | Notes |
| --- | --- | --- |
| `BENCHMARK_NAMESPACE` | — (required) | where everything lands |
| `MODEL_ID` | `Qwen/Qwen3-0.6B` | the served model |
| `IMG` | a build of this branch | released images reject this branch's flags |
| `GPU_MEM_UTIL` | `0.90` | fraction of **total** GPU memory; one value for every model, deliberately |
| `WARM_REPLICAS` | `0` | FMA warm pool size, if the scenario uses one |

## Weights, once you scale it

This guide deploys one model on one GPU, so the weights are fetched once and
the cost is invisible. It stops being invisible when WVA starts adding
replicas: without a shared cache, each new pod fetches them again.

llm-d's modelservice chart mounts a cache at `/model-cache` by default, so
there is usually nothing to do. Confirm it before you rely on autoscaling:

```bash
make workload-patch NAMESPACE=${NAMESPACE}
```

It reports both settings that make autoscaling safe — the cache, and a preStop
hook so a removed replica finishes what it is writing — and writes a patch for
whichever is missing.

Listing the pod's PVCs is the obvious check and it is the wrong one: a claim
mounted at one path while the engine downloads to another passes it and
re-downloads on every scale-up anyway. What decides it is where the weights
*land*, which is what the command above actually looks at.

[Weights and the model
cache](../../deployment/workload-preparation.md#weights-and-the-model-cache) has the
detail, including why it makes scale-up cheaper but not faster.

## Next

In order:

1. [Install WVA in a namespace](../install-in-namespace/) — the model is now there
   for it to scale. (The standup above already installs WVA; use this when you
   want the install on its own, or are adding WVA to a model somebody else
   deployed.)
2. `make benchmark-smoke NAMESPACE=<ns>` — drive load at it and snapshot the
   dashboard. Run it **last**, once everything is deployed.
3. [Benchmark WVA](../benchmarking/) — when you want numbers to compare
4. [After the install](../../deployment/operations.md) — what the metrics mean

[ob]: https://github.com/llm-d/llm-d/tree/main/guides/optimized-baseline
