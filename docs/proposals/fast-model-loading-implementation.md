# Warm pool — implementation design

How to build the pool described in
[the review](fast-model-loading.md). That document argues *whether*; this one is
*how*, at the level of packages, objects, ports and failure paths.

**Status:** implementation design, not built. Phase 0 measurement still gates it.

---

## 1. A correction, up front

The review said the FMA launcher is not needed. **That is wrong**, and the reason
constrains everything below.

A GPU is allocated to a **container**, not a Pod. Several models resident on one
GPU therefore means several vLLM processes inside **one container**, which needs a
supervisor process to spawn, sleep, wake and kill them. That is exactly what FMA's
`launcher.py` does, and it is why the warm-pool master record kept it as the one
thing reused.

So the honest inventory is:

| component | build/reuse | why |
| --- | --- | --- |
| in-Pod supervisor | **reuse FMA's launcher**, or write a minimal one (~300 lines) | a GPU belongs to one container; multiple engines need a parent |
| dual-pods controller (~3.5k lines) | **not needed** | a pool is an ordinary Deployment |
| launcher-populator, `LauncherPopulationPolicy` | **not needed** | WVA does the allocation |
| requester / SPI / probes / proxy | **not needed** | traffic arrives through the InferencePool, not a requester |
| `pod-helper.go` (`removeGPUResourceLimits`) | **not needed** | our Pods hold their GPUs deliberately (review §3.1) |

Reuse-versus-rewrite of the supervisor is a real decision, deferred to §11: the
FMA launcher is proven and has the CRUD API already, but it arrives with the
dual-pods assumptions attached.

### 1a. And the reuse case is stronger than "the supervisor"

A sweep of the older documents (prompted by asking whether FMA already supports
M>1) says the pool may not need building at all:

- **`maxInstances: 4` per launcher and `--sleeper-limit=2` per GPU are already
  running on pokprod.** Several models resident on one GPU is a *flag*, not a
  feature to build. A sleeper costs ~1.4 GiB of GPU memory, so on an 80 GiB card
  memory is nowhere near binding — **`--sleeper-limit` is**, and it is
  controller-global rather than per pool (upstream ask 4).
- **Requester Pods already solve the traffic problem** that §3 solves with labels
  and a router. Each requester is an ordinary Pod with its own IP, so port 8000
  is free in every one, membership is ordinary Deployment labelling, and the
  requester's TCP proxy landed upstream in `2c01cf8`. **That deletes item 9 (the
  model-aware local router) from §11 entirely.**
- The shared-pool plan's conclusion was explicit: **"Do not fork FMA"** — the
  mechanism is a flag plus an allocation policy, and the allocation policy is
  WVA's job. Which is the same conclusion §7 of this document reaches from the
  other direction.

**So the recommendation changes.** Do not build a parallel pool. Take FMA's
launcher-plus-requester as the data plane, raise `--sleeper-limit` to the number
of models a GPU should cover, and put the allocation policy in WVA — which is
what it is for. What remains genuinely unresolved is narrower and sharper than
"should we fork":

| question | status |
| --- | --- |
| GPU ownership: launcher holds no GPU, requester does | the parking failure mode. Needs inversion (a fork) **or** acceptance that only bound sleepers are reliable |
| warmth decay: the populator reaps launchers | no knob until upstream ask 2 (`minLauncherCount`) |
| per-pool `--sleeper-limit` | upstream ask 4 |
| a launcher hosting several instances cannot be scraped | upstream ask 3 — this one blocks *measuring* M>1 |

The rest of this document — objects, ports, state machine, policy, RBAC — still
describes what WVA must do, whether the Pods are ours or FMA's. Read §2-§4 as the
shape of a pool we build only if reusing FMA's is refused.

## 2. Objects

One **Deployment per pool**, where a pool is a `(accelerator, TP size, tier)`
triple. No CRD, no webhook, no operator.

```
Deployment  wva-pool-h100-tp1-ram        replicas: K
  Pod
    container "engines"                  requests: nvidia.com/gpu: <TP>
                                         requests: memory: <warm-set budget>
      supervisor (PID 1)                 :8081  control API, cluster-internal
      vllm instance, model A (asleep)    :9001
      vllm instance, model B (serving)   :8000  the InferencePool's target port
    readinessGates:
      - conditionType: wva.llm-d.ai/warm-ready
    labels:
      wva.llm-d.ai/pool: h100-tp1-ram
      llm-d.ai/model: <set at wake, removed at sleep>     <- membership
```

Three properties are fixed at Pod creation and therefore belong to the pool, not
to a runtime call: **TP size** (GPU count), **tier** (`gpu_memory_utilization` and
sleep level), and **M**, the number of simultaneously awake instances — vLLM fixes
utilisation at process start and Pod resources are immutable.

The Pod's **memory request is the warm-set budget**. At tier A each warm model
costs roughly its weight size in host RAM; the request is what makes that
schedulable rather than hopeful.

## 3. How traffic reaches a woken model

This is the part with the sharp edges.

**Membership is per model, and it is a label.** The EPP dispatches to Pods that
its InferencePool selects. A pool Pod joins model A's pool by carrying the label
that pool selects (`llm-d.ai/model`, already a constant in
`internal/constants/labels.go`), and leaves by having it removed. Because it is
per-model, one Pod can be a member of model A's pool and not model B's — which is
what makes M>1 tractable.

**Readiness is per Pod, and it gates everything.** Measured: a NotReady Pod
receives zero EPP traffic — no dispatch and no polling.

An earlier draft proposed a readiness **gate** that WVA patches. **That was
wrong, and needlessly expensive.** The proxy already knows whether a model is
awake, so an ordinary `readinessProbe` against its `/readyz` does the same job:
200 when a model is awake, 503 otherwise. FMA reaches the same conclusion from
the other side — it never writes Pod status at all, and derives the serving port
from the requester's own readinessProbe. Two things follow: the controller needs
no `pods/status` permission (§9), and a Pod whose controller has died still stops
taking traffic when its model sleeps, which a patched gate cannot manage.

**The port is the constraint that limits M.** The EPP dials the InferencePool's
`targetPortNumber` — typically 8000 — and only one process in a Pod can bind it.

- **M=1 (phase 1):** the awake instance binds 8000. Nothing else is needed.
- **M>1 (phase 2):** a **model-aware local router** binds 8000 and forwards by the
  `model` field of the OpenAI request body to the right local instance. It must
  stream, and it adds a hop. This is the only new networking code in the design,
  and it is the price of the 3.3x coverage in review §5.1.

**Ordering is load-bearing** and is already measured (admit 462 ms, drain 631 ms):

```
wake:   /wake_up  ->  poll until the engine answers  ->  add model label
                  ->  set gate true                          (Ready)
sleep:  set gate false OR remove model label
                  ->  wait ~1 s (2 s margin)  ->  /sleep
```

Sleeping before the traffic is gone is precisely the Ready-but-asleep window that
produces 503s.

## 4. The supervisor API

Small, synchronous, cluster-internal, one Pod's worth of state. Whether it is
FMA's launcher or ours, WVA needs exactly this:

| call | does | cost |
| --- | --- | --- |
| `GET /instances` | list: model, state, port, GPU, resident bytes | ms |
| `POST /instances` | spawn an engine for a model and load it | ~41 s [M] |
| `POST /instances/{id}/sleep?level=N` | vLLM `/sleep` | sub-second |
| `POST /instances/{id}/wake` | vLLM `/wake_up`, then confirm it answers | **0.3-3 s** [M] tier A |
| `DELETE /instances/{id}` | terminate, free RAM and GPU | seconds |

The supervisor owns process lifecycle and nothing else. **No policy lives in the
Pod** — which model to hold, wake or evict is WVA's decision, so that the cache
policy can change without touching the data plane.

## 5. WVA side

New package `internal/engines/warmpool`, running on the existing optimize loop.

```go
// Port — the mechanism boundary. Tier intent, never mechanism calls, so a
// CRIU-style backend can be substituted without touching policy.
type Pool interface {
    ListWarm(ctx context.Context) ([]Membership, error)
    Warm(ctx context.Context, m ModelRef, tier Tier) error   // admission, slow
    Activate(ctx context.Context, m ModelRef) (Endpoint, error)
    Deactivate(ctx context.Context, m ModelRef) error
    Evict(ctx context.Context, m ModelRef) error
}

type Membership struct {
    Model    ModelRef
    Pod      types.NamespacedName
    State    State   // Absent | Loading | Asleep | Waking | Serving | Draining
    Tier     Tier    // RAM | Disk
    LastUsed time.Time
}
```

Integration points that already exist:

- **`internal/engines/scalefromzero`** decides a model must wake. Today it
  publishes an activation; it gains one branch — ask the pool first, fall through
  to the cold path on a miss. `publishActivation` and `processInactiveModel` are
  the seams.
- **`internal/controller/inferencepool_reconciler.go`** and the pod datastore
  already know pools, pods and `llm-d.ai/model`, so membership bookkeeping has a
  home.
- **`internal/collector/source/pod`** already speaks HTTP to pods, so talking to a
  supervisor needs no new transport.
- **`wva_scale_from_zero_wake_seconds`** already measures first-publish to
  observed-serving on the 100 ms loop — the pool's own success metric exists and
  will show the improvement without new instrumentation.

## 6. State machine, per (Pod, model)

```
Absent ──Warm()──► Loading ──(engine up)──► Asleep
                      │                        │
                   (fail)                   Activate()
                      ▼                        ▼
                   Absent                   Waking ──(answers)──► Serving
                                               │                     │
                                            (fail)              Deactivate()
                                               ▼                     ▼
                                        evict + cold path        Draining
                                                                     │
                                                                  (drain)
                                                                     ▼
                                                                  Asleep
```

While a model is in `Loading`, its Pod is `admitting` (§7a.1) and is not part of
the reserve.

`Waking -> fail` is not hypothetical: two of three unbound sleepers could not be
woken (review §3.1). Our Pods hold their GPUs, which should remove the cause, but
the transition must exist and must fall back to the cold path rather than retry —
a sleeper that fails to wake is corrupt and gets evicted.

## 7. Cache policy — where the real work is

The mechanism is small; the policy is the product.

### Admission — what earns a place in the warm set

Admission and eviction are separate decisions, and conflating them is the classic
way to build a cache that thrashes. Eviction asks "what do I drop when full";
admission asks "is this worth keeping at all".

**Not plain demand-filling.** The obvious policy — model X missed, so load X — is
wrong here for three reasons:

1. **It spends the reserve exactly when it is scarce.** A Pod that is admitting
   cannot serve a wake, so filling on a miss during a burst removes a Pod from
   the reserve at the moment concurrent spikes are arriving. That inverts what
   `sleepMinSize` exists to guarantee.
2. **One-hit wonders pollute the warm set.** Models that spike once and never
   again would evict models that spike often. The standard answer is a frequency
   filter — admit on the SECOND miss within a window, not the first — which is
   the idea behind 2Q and TinyLFU.
3. **The model is about to be served anyway.** The ordinary replicas are already
   starting; a warm copy pays only on the NEXT spike, which may be hours away.

So residency has three sources, in descending order of confidence:

| source | policy | why |
| --- | --- | --- |
| **parked models** | admit eagerly, bypassing the filter | no ordinary replicas exist, so the pool is their only fast path, and the next wake is near-certain. The alternative is a guaranteed ~41 s |
| **popular models** | **preload** while the pool is idle, top-C by request share | prefetch beats demand-fill when the distribution is known, and skew is what makes a small warm set work |
| **everything else** | admit on the second miss in a window | keeps one-off models out |

Every source is subject to the same two guards:

```
admit(model, pod) requires:
    free - 1 >= sleepMinSize          # never spend the reserve to fill the cache
    no borrow blocked recently        # not during a burst
    predicted_wake(model) << cold_start(model)   # refuse models a pool cannot help
```

The last guard is review §3.5 made operational: at TP=8 or 70 B, every mechanism
converges to seconds and the pool should decline rather than underperform
quietly.

**The parameters are empirical, and the tool to settle them exists.**
`internal/warmpool/estimator` already carries `FillOnMiss` for exactly this: replay
a trace with demand-fill on and off, with and without a frequency filter, and read
the hit rates. Baking a window length or a C into the design now would be guessing
where measuring is cheap.
- **Eviction.** LRU by last-use, with explicit pinning. LRU is right because this
  is a cache and popularity is skewed; pinning exists because the operator knows
  things the recency signal does not.
- **Placement.** Spread copies of a model across Pods so that expected concurrent
  wakes per Pod stay under M. With M=1 this degenerates to the anti-affinity rule
  the earlier design stated.
- **Hold timeout.** A slot is released when ordinary replicas serve, or at
  `maxHoldSeconds`, whichever comes first. Without the timeout a stuck scale-up
  holds a slot forever and the loss model stops applying.

All four are pure functions over `[]Membership` plus demand — unit-testable with no
cluster, which is where the test weight should sit.

## 7a. Scaling logic: borrow the bridge, return it, keep a reserve

The pool exists to cover the ~41 s (measured: 33-37 s here) between "more
capacity is wanted" and "an ordinary replica serves". So a scale-up **borrows**
pool capacity, and gives it back when the ordinary replicas arrive.

### 7a.1 Pod states, and what `sleepMinSize` counts

The invariant is one awake instance per Pod, so a pool Pod is in exactly one of
two states:

| state | meaning | can serve a wake? | in its InferencePool? | Ready? |
| --- | --- | --- | --- | --- |
| **free** | every instance asleep | yes | no | **no** — `/readyz` 503 |
| **admitting** | loading a model to make it resident | **no**, for ~33-37 s | no | no |
| **lent** | one instance awake, serving | no | yes, for that model | yes |

**`admitting` is a state, not a detail.** A Pod loading a model cannot serve a
wake for anything else, and admission needs transient GPU memory of roughly the
full weights. A Pod that is admitting is therefore NOT part of the reserve, and
counting it as free is how a pool comes to report a reserve it cannot honour.

`sleepMinSize` is the floor on **free** Pods: the reserve the pool keeps for the
next spike. It is not a count of sleeping *instances* — a Pod with eight models
resident and one awake is lent, not free, because it can serve no further wake.

```
free(pool)      = |{ Pod : every instance asleep, not admitting }|
admitting(pool) = |{ Pod : loading a model }|
lent(pool)      = |{ Pod : one instance awake }|
replicas(pool)  = free + admitting + lent      # the pool Deployment's size
invariant:        free >= sleepMinSize          (maintained by scaling the pool)
admission rule:   admit only while free - 1 >= sleepMinSize
```

`sleepMinSize` is the K of the loss model in the review: the number of concurrent
spikes the pool can cover before one is blocked.

**Per pool, not per model — decided.** A per-model reserve would mean N reserved
GPUs for N models, which is precisely the per-model headroom a pool is supposed
to beat, and it would beat it while serving no traffic. The entire proposition is
that **K shared slots cover N models**, and that only works if the reserve is
shared: whichever model spikes takes a free Pod, and the Pod is fungible because
what makes it usable for a given model is residency, not reservation.

The consequence to hold onto is that a reserve of K is drawn down by ANY model in
the pool. Two models spiking together consume two Pods, so `sleepMinSize` is
sized against concurrent spikes across the whole pool — which is the loss model,
and why spike independence (review §2.4) is the assumption that decides whether
the number can be small.

### 7a.2 Scale-up: borrow N, and start N

WVA decides a variant needs `N` more replicas. Both things happen at once:

```
on scale_up(variant, N):
    ordinary.desired += N                       # ~33-37 s to serve
    candidates = free pods with `variant` resident
    borrow     = min(N, |candidates|)
    for pod in first `borrow` candidates:       # ~0.44 s each to serve
        wake(pod, variant)                      # /wake_up, then point the proxy
        label(pod, variant)                     # joins the InferencePool
        pod.hold_until = now + maxHoldSeconds
    record: hits=borrow, blocked=(N - borrow if candidates exhausted)
            misses=(N - borrow if variant not resident anywhere)
```

Both paths run; neither waits for the other. If the pool covers the whole spike
the user sees capacity in under a second; if it covers none the behaviour is
exactly today's. **Nothing is worse than today in any branch** — that is what
makes the pool safe to switch on.

`blocked` and `misses` are recorded separately because they say different things:
a block means the reserve was too small (raise `sleepMinSize`), a miss means the
warm set was wrong (raise the warm set, or change what is admitted to it).

### 7a.3 Handover: return as soon as the ordinary replicas serve

```
on ordinary.ready >= ordinary.desired (for the variant):
    for pod in lent pods of `variant`:
        unlabel(pod)                  # leaves the InferencePool
        wait drain (~0.7 s measured)
        clear the proxy upstream      # /readyz -> 503, Pod NotReady
        sleep(pod, variant)           # ~70 ms
```

Ordering is load-bearing and already measured: unlabel and drain **before**
sleeping, because sleeping first leaves a Ready-but-asleep window, which is
exactly the 503 condition.

`maxHoldSeconds` bounds it. If the ordinary replicas never arrive — quota,
unschedulable, image pull — the borrowed Pod is returned anyway, with an event
saying why. That is deliberate: holding indefinitely converts insurance into
capacity, and a pool permanently lent to one variant covers nobody else. The
variant then degrades to what it would have had without a pool.

### 7a.4 Scale-down: return borrowed Pods first

```
on scale_down(variant, N):
    give_back = min(N, |lent pods of variant|)
    return those Pods first (as in 7a.3)
    ordinary.desired -= (N - give_back)
```

Two reasons this order, not the other:

- A borrowed Pod is **temporary by construction**; an ordinary replica is the
  steady state. Shrinking the steady state while a bridge is still up would
  leave the variant depending on capacity that is about to be taken away.
- A returned Pod immediately becomes reserve for **every** model in the pool,
  whereas an ordinary replica freed to the cluster helps only whoever the
  scheduler gives its GPU to next.

### 7a.5 The accounting rule, which is where this goes wrong

**Pool capacity must not be counted as capacity when deciding how much to
scale.** A lent Pod serves traffic, so it lowers queueing and per-replica load on
the ordinary replicas. If the optimizer sees that relief, it concludes no
scale-up is needed, the ordinary replicas never arrive, the handover never
fires — and the pool stays lent forever, having *prevented* the very scale-up it
exists to cover. The mechanism would quietly convert itself from insurance into
permanent capacity for whichever variant spiked first.

So:

```
demand   = measured from the ROUTER (arrival rate, queue at the EPP)  -- includes
           traffic served by lent Pods, which is correct: it is real demand
capacity = ordinary READY replicas x per-replica capacity             -- lent Pods
           are excluded, deliberately
desired  = f(demand, capacity)   as today
```

Concretely, in this codebase: `wva_current_replicas` comes from the scale target,
which is the ordinary Deployment, so the *count* is already right. What is not
automatically right is **attribution** — a lent Pod carries the variant's
`llm-d.ai/model` label to join the InferencePool, so the collector will otherwise
scrape it as a replica of that variant and average its metrics in. Pool Pods must
be excluded from replica attribution by their `app.kubernetes.io/component:
warm-pool` label, while the router-side demand metrics stay untouched.

This is the same shape as the FMA attribution problem already documented in
`fma-aware-attribution.md`: an engine serving a variant from a Pod that no
ScaledObject owns. The difference is that here we own both sides, so the rule can
simply be stated and enforced.

### 7a.6 Replenishing the reserve

After a borrow, `free` has dropped. Two ways back, in order of preference:

1. **A handover returns the Pod** — the normal case, and free again in ~1 s.
2. **Grow the pool** when `free < sleepMinSize` and the shortfall persists past a
   debounce window: `replicas(pool) += sleepMinSize - free`. A new Pod is useful
   only once it has models resident, and admission costs a full load (~37 s), so
   growth is slow by nature and must not be triggered by a transient borrow.

Shrinking back is the mirror, and deliberately lazier: `free > sleepMinSize +
slack` for a sustained period. A Pod deleted is a warm set thrown away, so the
cost of shrinking too eagerly is paid on the next spike as misses.

**`sleepMinSize` is a floor, not a target.** The pool may hold more free Pods than
it — that is what makes a burst of concurrent spikes survivable.

### 7a.7 Edge cases, and what each does

| case | behaviour |
| --- | --- |
| `N` > free Pods with the model resident | borrow what exists, record the rest as blocked; ordinary path covers them |
| model resident nowhere | miss; optionally admit it to a free Pod for *next* time (policy) |
| a second spike for the same variant while lent | borrow another free Pod; a lent Pod cannot be lent twice |
| ordinary replicas arrive before the wake finishes | sleep immediately; the wake was wasted, not harmful |
| lent Pod dies | it leaves the InferencePool with the Pod; ordinary path unaffected |
| scale to zero with the variant still resident | this is parking: keep it resident, no ordinary replicas at all |
| WVA restarts while Pods are lent | reconcile from observed state -- lent is discoverable from labels plus the supervisor's instance list, not from memory |

### 7a.8 Configuration

```yaml
pools:
  - name: h100-tp1-ram
    replicas: 4              # free + lent
    sleepMinSize: 2          # floor on free -- the reserve
    maxHoldSeconds: 120      # a borrowed Pod is returned by then, always
    growDebounceSeconds: 60  # how long free < sleepMinSize before growing
    shrinkSlack: 1           # free must exceed sleepMinSize by this to shrink
```

### 7a.9 What to measure once it runs

Both ratios that decide whether any of this pays, plus the two that say which
knob is wrong:

```
wva_warmpool_free_pods                 vs sleepMinSize  -- is the reserve holding?
wva_warmpool_borrow_total{outcome}     hit | blocked | miss
wva_warmpool_bridge_seconds            borrow -> handover: how long a bridge lasts
wva_warmpool_hold_expired_total        bridges returned because ordinary never came
```

`bridge_seconds` is the honest measure of what the pool is worth: it should track
the ordinary start time (~33-37 s). If it sits at `maxHoldSeconds`, ordinary
scale-ups are failing and the pool is masking it.

## 8. Configuration

ConfigMap, in the shape the repo already uses for scaling policy:

```yaml
pools:
  - name: h100-tp1-ram
    accelerator: NVIDIA-H100-80GB-HBM3
    tensorParallel: 1
    tier: ram                 # ram | disk
    replicas: 4               # K, from the loss model
    awakeSlots: 1             # M; >1 requires the local router
    warmSetPerPod: 8          # S; C = replicas x warmSetPerPod
    memoryPerWarmModel: 20Gi  # the Pod memory request is derived from this
    maxHoldSeconds: 120
policy:
  admit:
    parkedWithinHours: 24
    minPredictedSaving: 10s
  evict: lru
  pin: []
```

`replicas` and `warmSetPerPod` are the two numbers review §2.3 says must both be
sized: K for capacity, C for coverage.

## 9. RBAC — a real privilege increase

Today the manager ClusterRole has `pods: get, list, watch`. The pool needs:

| resource | verb | why |
| --- | --- | --- |
| `pods` | `patch` | add/remove the model label (membership) |
| `deployments` | `create, update, patch` | own the pool Deployments |

**`pods/status` is NOT needed**, which is the second thing the readiness probe
buys (§3). An earlier draft asked for it to satisfy a readiness gate and called
it "a notable privilege" — correctly, which is why not needing it matters. It
also retires the trap that came with it: a JSON *merge* patch on pod conditions
replaces the array and wipes `Ready`, and that patch produced a spurious
measurement once already. There is now no such patch.

## 10. Failure modes, and what each degrades to

| failure | behaviour | degrades to |
| --- | --- | --- |
| wake fails (cumem OOM / invalid argument) | evict the sleeper, fall through | cold start — today's behaviour |
| supervisor crashes | Pod restarts, every warm model in it is lost | cold cache, refills by admission |
| pool full, no free slot | blocking, per the loss model | cold start |
| model not resident | cache miss | cold start |
| node drained | Deployment reschedules; memberships lost | cold cache |
| WVA down | pool keeps serving whatever is awake; nothing new wakes | cold start |

**Every path degrades to what happens today.** Nothing goes `Pending`, so the pool
can be switched off at any moment by scaling the Deployment to zero.

## 11. Work breakdown

| # | work | size | depends on |
| --- | --- | --- | --- |
| 1 | supervisor: reuse FMA launcher vs write minimal | 2-4 d | decision below |
| 2 | pool Deployment rendering + config | 2 d | — |
| 3 | `Pool` port + HTTP adapter to the supervisor | 3 d | 1 |
| 4 | readiness gate + label membership + drain ordering | 3 d | RBAC |
| 5 | cache policy (admit/evict/place/hold) | 4-5 d | — (pure, testable first) |
| 6 | scale-from-zero integration + miss fallback | 2 d | 3, 4 |
| 7 | metrics: hit/miss, slots busy, memberships, evictions | 1 d | — |
| 8 | e2e with a fake supervisor | 3 d | 3 |
| 9 | model-aware local router (M>1) | 4 d | **phase 2 only** |

Roughly **three weeks to phase 1** (items 1-8, M=1, tier A, parking only), with
item 5 the only genuinely hard one.

**Supervisor decision.** Write a minimal one. FMA's launcher is proven and has the
API, but it carries the dual-pods assumptions — instance IDs hashed from GPU UUIDs,
the ISC-derived port, reclaim behaviour — and we would be forking it to remove
them. A supervisor that spawns processes, calls three vLLM endpoints and reports a
list is a few hundred lines with no policy in it. Reuse if the fork proves
smaller than the rewrite; measure that on day one, not by argument.

## 12. Testing

- **Policy: unit, no cluster.** Admission, eviction, placement and hold are pure
  functions over memberships and demand. This is where most tests belong.
- **Reconcile: envtest.** Gate patching, label add/remove, ordering. Run in WSL —
  envtest suites fail on native Windows here.
- **e2e: kind, fake supervisor.** A container serving the supervisor API with
  scripted latencies exercises hit, miss, wake-failure, eviction and drain without
  a GPU. The SGLang emitter fixture is the precedent for this pattern.
- **The one thing kind cannot cover** is a real wake, which needs a real engine on
  a real GPU. That stays a manual measurement on pokprod, reported through
  `wva_scale_from_zero_wake_seconds`.

## 13. Not built

Multi-node (LWS) pools; a CRD; a webhook; a scheduler plugin; cross-node model
migration; CRIU. Each is either out of scope per the review or waiting on a
measurement that has not been taken.
