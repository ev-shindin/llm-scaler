# Warm pool: configuration surface

Status: steps 1-3 implemented; cross-namespace pools (step 5) deferred. Follows
the cluster run of 2026-08-25, which took the pool through a full borrow ->
bridge -> return cycle and produced the requirements below.

One thing changed shape during implementation. Step 1 turned out to be mostly
DONE already: `doesNotFit` preferred the Pod's declared capacity over the flag,
and every pool Pod carries that capacity including an empty one. So the work was
deleting flags that could only ever agree or silently disagree, not building a
derivation.

And one requirement was missing from this document entirely -- accelerator
matching, added as Part 3a below. It is what makes multiple pools necessary
rather than merely possible.

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

## Part 3a: matching the GPU model, not just the count

The fit check compared GPU COUNT and stopped, so a pool of A100 Pods would warm
a model pinned to H100: the right number of the wrong GPU. The load is spent for
nothing, because the workload's own affinity refuses the node the warm copy sits
on, so no bridge from it can serve.

**Reading `nodeSelector` alone would have been useless.** A live llm-d decode
Deployment pins its accelerator entirely through

```yaml
affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
      - matchExpressions:
        - key: nvidia.com/gpu.product
          operator: In
          values: [NVIDIA-H100-80GB-HBM3]
```

and writes **no `nodeSelector` at all** -- checked on the cluster before writing
the check. A `nodeSelector`-only version would find nothing, conclude every
workload is portable, and never fire on the layout it exists to protect. Both
forms are read, plus the provider label aliases, without which a CoreWeave fleet
demonstrably on H200s reads as having no accelerator.

Deliberately NOT treated as a requirement: a *preferred* affinity (the scheduler
may ignore it); a `NotIn` term (says what a workload refuses, not what it needs);
a multi-valued `In` (says the workload is portable between those models); and any
key that is not a GPU-model label, or a workload pinned to a zone would appear to
demand an accelerator called `us-east-1a`.

Only a PROVEN mismatch blocks. Nodes are cluster-scoped, so a namespace-scoped
install may legitimately not read them; declining every admission there would
silently disable the pool, which is the failure this whole document exists to
remove. An unknown accelerator therefore allows, and appears in the pool state
line so its absence is visible rather than assumed.

This is also what settles Part 2: multiple pools are **necessary**, not a
convenience. One pool cannot serve two accelerator types, because a warm copy is
only reusable on the GPU it was loaded on.

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

## Part 5: what stays warm, and how big the pool is

Two questions the automatic policy cannot answer, added after the first four
steps landed.

### Which models, and how many copies

Automatic admission ranks parked variants, then the busiest, then anything that
has missed twice. It cannot express two things:

- a **low-traffic but latency-critical** model, which popularity ranking will
  never warm and which no amount of tuning `preload-top` will reach;
- **two simultaneous scale-ups of one model**, because a single warm copy
  bridges a single scale-up and the second goes cold with free Pods beside it.

One trigger-metadata key covers both, and the opt-out as well:

| `warmPoolCopies` | means |
| --- | --- |
| *absent* | automatic — at most one copy, ranked by the pool |
| `"0"` | never warm this model |
| `"1"` | pin one copy |
| `"N"` | N copies, so N scale-ups bridge at once |

`"1"` is deliberately not the same as absent: absent lets a quiet model lose its
slot to a busier one. Copies land in different Pods, since a second copy beside
the first shares its accelerators and could never serve a second bridge.

This is also the only way to weight a shared pool. Automatic mode holds one copy
of each model and no more, so "two of A, one of B" is otherwise inexpressible.

### The pool's own size

The pool gets **its own ScaledObject**, and is scaled the way every other
workload here is: WVA publishes a size, KEDA writes it.

The first attempt did not. It patched `spec.replicas` directly, which needed a
permission `internal/controller/rbac.go` explicitly warns against — *"the single
permission a cluster admin is most right to refuse"* — with an instruction not
to re-add it without first answering "why is WVA writing when KEDA actuates?".
Answered honestly, it should not be. Scaling through KEDA means the controller
needs no write permission at all, not even one scoped by `resourceNames` to a
single object.

Three things fell out of that, which is how you know it was the right shape:

- **The hysteresis went away.** Counting consecutive short and idle passes was a
  hand-rolled stabilization window. An HPA has those, so the asymmetry a warm
  pool needs — grow promptly, shrink slowly, because too small costs latency on
  every spike while too large only costs money — now lives in the ScaledObject's
  `behavior` block where it can be seen and tuned.
- **The min/max annotations went away**, because `minReplicaCount` and
  `maxReplicaCount` already say it. Two ways to bound one number is exactly the
  duplicated-fact problem this document exists to remove; it had crept back in.
- **What remains is one stateless formula:** `lent + reserve + 1`. Lent Pods
  cannot count toward the reserve because they are serving; the `+1` is what
  makes admission possible at all.

A pool's trigger carries `warmPoolName` instead of `modelID` — a pool serves no
model — and both consumers that build variants skip those entries, so the pool
does not become a phantom variant every engine has to special-case.

## Order, and what is done

1. **Done.** Derived `gpusPerPod`, memory and namespace; deleted the flags.
2. **Done.** Pools are named by the `llm-d.ai/warm-pool` label and discovered
   from the Deployments that declare them; knobs are annotations on the
   Deployment, layered over the flags; each pool keeps its own reserve.
3. **Done.** `warmPool` in trigger metadata, needed only to disambiguate;
   unresolvable selection is declined and reported per variant.
3a. **Done.** Accelerator matching, from nodeAffinity as well as nodeSelector.
4. **Done.** RBAC preflight (`SelfSubjectAccessReview` for pods `patch`, the pool
   refuses to start without it) and a distinct diagnosis when EVERY Pod is
   unreadable at once.
5. **Done.** `warmPoolCopies` for pinning, opting out and non-even
   distribution; the pool's own ScaledObject for its size.
6. **Deferred.** Cross-namespace pools, only if a real deployment needs them.

The user guide is written: [Bridge a scale-up with a warm
pool](../guides/warm-pool/).

### Verified on the cluster, 2026-08-25

Steps 1, 2, 3 and 3a were run on pokprod. One log line covers three of them:

```
warm pool enabled  {"namespace":"evgensh-wva-test","sleepMinSize":1,"maxHold":"2m0s",
                    "memoryBudgetBytes":0,"gpuMemoryUtilization":0.9,"preloadTop":2}
warm pool state    {"pool":"default","state":"pods=2 free=2 resident=2 variants=1
                    lent=0 accelerator=NVIDIA-H100-80GB-HBM3"}
```

`memoryBudgetBytes: 0` is the derived default and `gpusPerPod` is gone entirely
(step 1); `pool: default` is discovered from the Deployment label rather than
configured (step 2); the accelerator was read from the node the Pod runs on
(step 3a). A model was then admitted and reached `running`.

Step 3 was exercised by adding a second pool Deployment at `replicas: 0`, which
costs no GPU but makes the namespace ambiguous:

```
variant will get no warm copy  {"variant":"qwen-...-decode-wva",
  "reason":"names no warm pool and this namespace has 2 (default, second);
            set the \"warmPool\" trigger metadata key"}
```

Setting `warmPool: default` in the trigger metadata restored it on the next KEDA
call, and both pools reported their own state independently. The warm copy was
NOT destroyed while the variant was unassignable, which is the right behaviour:
eviction answers pressure, not a variant that stopped asking.

The step 4 preflight is verified in BOTH directions, on two clusters.

Allowing, on pokprod: the pool starts silently. Denying, on kind, where nothing
else uses the cluster -- `patch` was removed from the ClusterRole the
controller's ServiceAccount draws it from, the API server then answered `no`,
and the controller refused to start the pool:

```
ERROR setup the warm pool is disabled: this controller may not patch Pods, so it
could never lend one. Grant patch on pods in the pool namespace ... and restart.
Running without it would hold GPUs, warm models, and fail every borrow.
```

RBAC was restored afterwards and the pool started again. This deliberately was
NOT done on pokprod: there the ServiceAccount draws `patch` from a ClusterRole
shared with other namespaces' installs, so revoking it to observe one log line
would have broken other tenants.

### Cluster testing, 2026-08-26

Also verified on pokprod: the pool's own ScaledObject end to end -- raising the
reserve changed the published size, the HPA followed, and KEDA moved the
Deployment, with WVA writing nothing; the shrink direction likewise;
`warmPoolCopies: "2"` warming the model in both Pods; and eviction releasing
both copies on `"0"`, then automatic mode re-warming one when the key was
removed.

The existing warm-pool e2e passes on kind against this build -- 6 specs, 0
failures -- which covers the pool Pod's traffic gate and the shipped
NetworkPolicy.

**Still not verified on any cluster:** the accelerator MISMATCH decline, and two
pools each holding Pods. Both need e2e infrastructure that does not exist yet: a
controller with the warm pool ENABLED, plus a pool Pod pinned to a node whose
accelerator label the test chooses. The suite deploys its controller without
warm-pool flags, and the pool fixture cannot pin a node. Neither gap is a
guess -- both paths are unit-tested and mutation-checked, and both of their
INPUTS are proven on a cluster (the accelerator is read from the node and
appears in the pool state line; the access review answers correctly both
ways) -- but the decision itself has not been observed end to end.

**Not verified on a cluster:** everything in Part 5 — `warmPoolCopies` and the
pool ScaledObject are unit-tested only, and the second changes how the pool's
size reaches Kubernetes at all.

**Not verified on a cluster:** the accelerator MISMATCH path. Every GPU node on
pokprod is an H100, so there is no second type to be declined against, and the
alternatives -- repinning the live workload to a fake accelerator -- would take
the tenant's own deployment down to test a log line. The reading of both sides is
verified above; the decline itself rests on mutation-checked unit tests.

A user-facing guide is now worth writing, which it was not before: the surface no
longer contains three duplicated facts and a footgun.
