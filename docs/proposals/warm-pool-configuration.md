# Warm pool: configuration surface

Status: proposal. Follows the cluster run of 2026-08-25, which took the pool
through a full borrow -> bridge -> return cycle and produced the requirements
below.

## What exists today

Seven process-global flags, one pool, discovered by the label
`app.kubernetes.io/component=warm-pool` in a namespace named by
`--warm-pool-namespace`. Binding is implicit: every variant the controller sees
competes for that one pool.

Turning the pool on takes four steps, and **three of them fail closed and
silently**:

| Step | If you skip it |
|---|---|
| `--warm-pool-*` flags | pool is off — the only honest one |
| RBAC: `patch` on pods | borrow cannot label a Pod into the InferencePool |
| NetworkPolicy `>>> EDIT THIS <<<` | every supervisor read times out; pool reads as empty |
| apply `config/warmpool` | no Pods; nothing to warm |

Two more traps have no configuration at all: a one-Pod pool is structurally
inert (below), and admission needs two misses, so nothing happens on the first
spike.

## Two separable questions

1. **What defines a pool** — its existence, physical shape, and knobs.
2. **How a model is bound to a pool.**

They are worth separating because they have different right answers.

## Part 1: what defines a pool

### Several "knobs" are not configuration at all

Three of the seven flags duplicate facts that already exist on the pool
Deployment:

- `--warm-pool-gpus-per-pod` — its own help text says *"Must match the pool
  Deployment's `nvidia.com/gpu` request."* A value that must equal another
  object's field is not configuration; it is a second copy that can disagree.
  And disagreement fails closed and silently: a variant is *declined* rather
  than loaded, which looks exactly like a variant that legitimately does not
  fit. **Read it from the pod template.**
- `--warm-pool-memory-bytes` — already clamped to the container's limit, and
  `0` already means "use the limit". It is a lower override on a value the Pod
  already states. **Default to the limit; keep the flag as an override only.**
- `--warm-pool-namespace` — for a namespaced WVA this is *always* the namespace
  the ScaledObject lives in. **Derive it.** Cluster-scoped WVA is the case where
  it can differ; see Part 2.

That leaves four genuine policy knobs: `sleepMinSize`, `maxHold`, `preloadTop`,
`gpuMemoryUtilization`.

This matters more than tidiness. Every silent failure this feature has produced
so far came from two places holding one fact. Deriving is not a convenience, it
is the fix.

### Alternatives for where the four knobs live

**A. Flags (today).** One pool per controller process, period. Rejected: it
cannot express more than one pool, and more than one is genuinely needed — a
warm copy is only reusable on the same accelerator, so a cluster with two GPU
types needs two pools, and a TP=4 variant needs a pool whose Pods hold 4 GPUs.

**B. ConfigMap keyed by pool name.** Matches the existing precedent exactly:
named, identity-free scaling-policy tiers selected from trigger metadata
(`internal/config/scaling_policy.go`). Operators already look in a ConfigMap for
WVA tuning.

**C. Annotations on the pool Deployment.** The knobs travel with the object they
describe. Deleting the pool deletes its config; there is no "config for a pool
that does not exist" state and no second watch.

**D. A `WarmPool` CRD.** Rejected on project direction: the VariantAutoscaling
CRD is being removed, and a ScalingPolicy CRD was already considered and
rejected in favour of ConfigMap + trigger metadata. A CRD also does not remove
the Deployment — it adds a controller to create one.

### Recommendation: C, annotations on the pool Deployment

The decisive argument is that **sizing and reserve are one decision**. You cannot
sensibly set `sleepMinSize: 2` on a two-replica pool — that is the inert pool,
which holds GPUs and warms nothing forever. `replicas` and `sleepMinSize` must be
chosen together, and putting them on one object is what makes that possible to
get right and trivial to validate.

```yaml
kind: Deployment
metadata:
  name: wva-warm-pool
  labels:
    app.kubernetes.io/component: warm-pool
    llm-d.ai/warm-pool: default          # the pool's NAME
  annotations:
    llm-d.ai/warm-pool-sleep-min-size: "1"
    llm-d.ai/warm-pool-max-hold: "2m"
    llm-d.ai/warm-pool-preload-top: "2"
    llm-d.ai/warm-pool-gpu-memory-utilization: "0.90"
spec:
  replicas: 3                            # must exceed sleep-min-size
```

Annotations on the Deployment (not the pod template) do not restart Pods, so
retuning a live pool costs nothing.

Honest trade-off: **B is more consistent** with where operators already look. If
consistency is judged to outweigh co-location, B is acceptable — but only now
that the inert-pool case is loud, because B reintroduces exactly the
two-places-one-fact split that made it silent.

## Part 2: binding a model to a pool

**A. Trigger metadata `warmPool: <name>`.** Consistent with `scalingPolicy`: the
model declares what it wants, pools stay identity-free, and the set of models in
a pool is declared by the models.

**B. The pool declares its members** via a selector. Inverts A. Risks two pools
claiming one model, which needs a tie-break rule — the scaling-policy code needed
exactly that (`FindModelOverride` breaks ties lexicographically) and it is a rule
nobody can predict from the outside.

**C. Namespace-implicit.** A model uses the pool in its own namespace.

**D. Explicit cross-namespace reference** `<namespace>/<name>`.

### Recommendation: C by default, A to disambiguate, D opt-in

| Situation | What the operator writes |
|---|---|
| namespaced WVA, one pool in the namespace | **nothing** |
| more than one pool in the namespace | `warmPool: <name>` in trigger metadata |
| more than one pool, no selection | declined, with a reported reason — never a silent pick |
| cluster WVA, pool in another namespace | `warmPool: <namespace>/<name>`, plus RBAC and a NetworkPolicy that admits the controller |

The point of the default is that **the metadata parameter is not boilerplate**.
It appears only where there is real ambiguity, so the common case stays
zero-configuration and identical to today's behaviour. Tagging every ScaledObject
with a pool name would be pure ceremony in the case that is most common.

An unknown pool name resolves to no pool and is *reported*, following the rule
the scaling-policy resolver already sets: the silent fallback is the right
behaviour and the wrong silence. Per project convention this is a `reason` on
`wva_model_scaling_blocked`, not a new gauge.

## Part 3: GPU accounting

Warm pool GPUs **are** already counted, and the accounting is correct. Verified
on the cluster rather than inferred:

- `DiscoverUsageByNamespace` walks every Pod on a GPU node and sums requests,
  attributing by node and explicitly *not* by WVA's managed population — it is
  documented as counting "GPUs held by workloads WVA does not manage at all".
  Pool Pods are scheduled, Running, and request `nvidia.com/gpu`, so they are in.
- Measured during the run: the namespace showed 3 GPUs — 1 decode, 2 pool.
- **Not double-counted while lent.** A borrowed Pod belongs to the pool
  Deployment, not to the scale target, and WVA's replica counts come from the
  scale target. Proven by the bridge itself: it returned exactly when decode
  reached 3/3, not at borrow time, which it would have done had the borrowed Pod
  counted toward `Ready`. The collector cannot see it either — it discovers Pods
  through a Service selector, and llm-d's decode has no Service at all.
- The count is **constant across borrow and return**: the pool holds its GPUs
  whether lending or idle. A bridge can therefore never push a namespace over
  quota and deny the very scale-up it is covering.

### The consequence that belongs in the configuration surface

**The pool is not free.** Its GPUs permanently reduce the namespace's headroom,
so a pool of N Pods lowers the maximum fleet by N. That is correct — they really
are held — but it is a cost decision an operator must make deliberately, and it
should be stated in the docs and logged at startup alongside the pool's size.

Open item worth a test: scale-from-zero uses a *managed* usage view derived from
replica counts, and a pool covering a 0->1 wake is the one case where a borrowed
Pod serves while the variant has zero ordinary replicas.

## Part 4: the silent failures

- **RBAC (`patch` on pods)** — preflight with a `SelfSubjectAccessReview` at
  startup and refuse to enable the pool without it, rather than discovering it at
  the first borrow.
- **NetworkPolicy** — the read failure now reaches the log via the state line; it
  deserves a distinct diagnosis, since "every Pod unreadable" has exactly one
  common cause.
- **Inert pool** — done: a pool whose Pod count equals its reserve now says so at
  Info, naming both numbers and both ways out.
- **Silent success** — done: a deduplicated state line, so a working pool and a
  dead one are no longer indistinguishable.

## Suggested order

1. Derive `gpusPerPod`, memory and namespace; delete the flags. Removes a whole
   class of silent mismatch and shrinks the surface before it is documented.
2. Name pools (`llm-d.ai/warm-pool` label) and read knobs from annotations,
   keeping flags as the fallback for a single unnamed pool.
3. Honour `warmPool` in trigger metadata; decline-and-report on ambiguity.
4. RBAC preflight and the NetworkPolicy diagnosis.
5. Cross-namespace pools, only if a real deployment needs them.

Steps 1-2 are worth doing before any user-facing guide is written: documenting
the current surface means documenting three duplicated facts and a footgun.
