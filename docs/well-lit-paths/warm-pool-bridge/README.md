# Bridge a scale-up with a warm pool

A scale-up is slow because a new replica must load a model before it can serve.
A warm pool holds a few Pods with models already loaded and asleep, and lends
one to a variant that is scaling up. The borrowed Pod joins the model's
InferencePool and serves while the ordinary replica loads, then is handed back
the moment that replica is ready.

**Use it when** your spikes arrive faster than a replica starts, and the cost of
the gap — queued requests, missed TTFT, a load shed — is worth holding
accelerators to cover.

**Do not use it when** load is steady, or when accelerators are the scarce
thing. The pool is **insurance, not capacity**: every pool Pod holds its
accelerators continuously, lending or idle, and a pool of N lowers your maximum
fleet by N. That is a cost decision, which is why WVA never creates a pool for
you.

## What it needs

- Accelerators free for the pool itself, **on the same GPU model** as the
  workloads it will warm. A warm copy is only reusable on the accelerator it was
  loaded on, so one pool cannot serve two GPU types.
- A shared model cache the pool can read, so a warm copy loads from local
  storage rather than a download.
- RBAC letting WVA `patch` Pods. The shipped ClusterRole has it; a hand-scoped
  one may not, and WVA refuses to start a pool it could never lend from.
- Two container images from two owners — the FMA launcher, which is not ours,
  and this repo's pool proxy.
- A pool whose replica count **exceeds** its reserve. A pool that does not can
  never warm anything at all.

## Setting it up

[Bridge a scale-up with a warm pool](../../guides/warm-pool/) — sizing, the two
images, the NetworkPolicy, and how the pool is declared. The pool is an ordinary
Deployment; its knobs are annotations on it.

## Several models in one pool

One pool holds many models, and that is the normal case rather than an advanced
one. Three decisions come with it:

**Which models can share a pool.** A pool serves exactly one
**(accelerator, GPUs-per-replica)** shape, because neither is negotiable at run
time. Every other difference between models — size, traffic, policy — a pool
absorbs. Two GPU models on the cluster means two pools.

```bash
deploy/warmpool.sh plan -n <namespace>
```

`plan` groups the namespace's model ScaledObjects by that pair, says which could
share a pool, and names models that select a pool nobody declared — worse than
selecting none, because it reads as configured.

**How many fit in one Pod.** The pod template's memory limit is the warm-set
budget, not a safety margin: a level-1 sleeper's weights are charged to the
cgroup in shared memory. Measured on an H100, a resident model costs roughly
2.6 GiB + 1.4x its weights, so a 128Gi Pod holds about five 8B models and hits
the hard ceiling of 16 instances long before memory on anything smaller than
about 3B. Overcommitting does not fail politely: it OOM-kills the launcher and
takes **every model already resident in that Pod** with it. Changing the limit
rolls the pool, so size it at deploy time.

**Which of them stay warm.** By default the pool ranks: parked models first,
then the busiest, then anything that has missed twice — right for almost
everything. Two things it cannot infer are set per model in the ScaledObject
trigger:

| `warmPoolCopies` | means |
| --- | --- |
| *absent* | automatic — the pool ranks it and holds at most one copy |
| `"0"` | never warm this model, and release it if it already is |
| `"1"` | always keep one warm, whatever the ranking thinks |
| `"N"` | keep N warm, so N scale-ups of this model can bridge at once |

If your models are too large for ordinary replicas to arrive at all, the pool
stops being a bridge and becomes the capacity itself — that is
[the retained pool path](../retained-pool/).

## Verifying it

```bash
# the pool exists and holds what it says
kubectl get deploy -n <ns> -l app.kubernetes.io/component=warm-pool

# a bridge was actually lent, and to whom
kubectl logs -n <wva-namespace> deploy/<wva> | grep warm-pool-bridge
```

A bridge is **not** one of the variant's own serving replicas, and the engine
prices it apart from them — so a bridged variant showing one more Pod than
`wva_current_replicas` is correct, not a miscount.

## What it costs

N accelerators, continuously, for as long as the pool exists. Size it by how
often you spike and how long a replica takes to start — never by peak load.

## How it is tested

End-to-end, eight suites: `test/e2e/warm_pool_test.go`,
`warm_pool_borrow_test.go`, `warm_pool_return_drain_test.go`,
`warm_pool_drain_gpu_test.go`, `warm_pool_group_test.go`,
`warm_pool_policy_test.go`, `warm_pool_quota_test.go`,
`warm_pool_attribution_test.go` — covering the borrow, the hand-back, the drain,
multi-GPU groups, the policy that vets an engine before pointing at it, quota
refusal, and how a bridge is attributed.

## Tuning it

Pool size, the retained-model rule and the switch interval are annotations,
documented in the guide. The reasoning behind the design, including what was
measured and rejected, is in
[proposals/fast-model-loading.md](../../proposals/fast-model-loading.md).
