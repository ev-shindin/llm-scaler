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
