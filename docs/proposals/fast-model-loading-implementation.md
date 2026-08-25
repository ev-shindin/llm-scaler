# Warm pool — implementation design

How to build the pool described in
[the review](fast-model-loading.md). That document argues *whether*; this one is
*how*, at the level of packages, objects, ports and failure paths.

**Status.** The mechanism is BUILT and running: `internal/warmpool` (port,
adapter, policy, reconciler), `internal/warmpool/proxy` + `cmd/warmpool-proxy`,
`config/warmpool`, wired into the controller behind `--warm-pool-namespace` and
off by default. A pool Pod has served real EPP traffic on pokprod with ~437 ms
model switches.

What is NOT built, and is marked so where it appears below: eviction and
pinning, spreading a model across Pods, the ConfigMap configuration surface
(flags instead — see §8), and pool growth and shrink (`GrowBy` is reported, not
acted on). Phase 0 measurement still gates whether any of it pays.

Preloading and the admission guard WERE on that list and are now built. Naming
them here after they landed was not a small slip: the list is what a reader
trusts to know where to look, so anything on it is a thing nobody examines.
Preloading ranks by a variant's share of the fleet (§14a); the guard declines a
model that cannot fit the Pod it would go into, by GPU count or by weight
(§14c).

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
| requester / SPI | **not needed** | traffic arrives through the InferencePool, not a requester |
| proxy | **NEEDED, and built** | a sleeping engine keeps its socket, so instances cannot take turns binding the pool's target port — see §3 |
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
  *(Superseded: a pool Pod holds several engines, so port 8000 is NOT free in
  it, and a proxy was needed and written. See §3.)*
- The shared-pool plan's conclusion was explicit: **"Do not fork FMA"** — the
  mechanism is a flag plus an allocation policy, and the allocation policy is
  WVA's job. Which is the same conclusion §7 of this document reaches from the
  other direction.

**So the recommendation changes.** *(NOT WHAT WAS BUILT — see below.)* Do not
build a parallel pool. Take FMA's launcher-plus-requester as the data plane, raise `--sleeper-limit` to the number
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

**Reusing FMA's data plane WAS refused, on measurement.** FMA's controller
strips the GPU from the launcher Pod and hands it to a transient requester;
when that requester goes, the sleepers die with `cumem` errors — measured, twice.
So the pool owns its Pods: an ordinary Deployment that keeps the GPU, running
FMA's launcher UNMODIFIED as the in-Pod supervisor, with our own proxy and the
allocation policy in WVA. §2-§4 are therefore the built design, not a fallback.

## 2. Objects

One **Deployment per pool**. No CRD, no webhook, no operator.

The sketch below is WHAT WAS BUILT (`config/warmpool/`). An earlier draft
described a pool as an `(accelerator, TP size, tier)` triple with a readiness
gate and a `wva.llm-d.ai/pool` label; none of that exists. A pool is one
namespace, one tier, and is identified by the two ordinary
`app.kubernetes.io/` labels its own Deployment selects on.

```
Deployment  wva-warm-pool                replicas: K
  Pod
    container "inference-server"         requests: nvidia.com/gpu: <TP>
                                         requests: memory: <warm-set budget>
      supervisor (PID 1)                 :8001  control API, controller-only
      vllm instance, model A (asleep)    :9001
      vllm instance, model B (awake)     :9002
    container "proxy"
      serving                            :8000  the InferencePool's target port
      control                            :8002  controller-only; re-points it
    labels:
      app.kubernetes.io/name: workload-variant-autoscaler
      app.kubernetes.io/component: warm-pool          <- pool capacity
      llm-d.ai/model: <set at wake, removed at sleep> <- membership
```

Both instances bind their own ports for life; the proxy owns 8000 and follows
whichever is awake. Readiness is an ORDINARY probe against the proxy's
`/readyz`, not a readiness gate — see §3 — which is what keeps the controller
out of `pods/status`.

Three properties are fixed at Pod creation and therefore belong to the pool, not
to a runtime call: **TP size** (GPU count), **tier** (`gpu_memory_utilization` and
sleep level), and **M**, the number of simultaneously awake instances — vLLM fixes
utilisation at process start and Pod resources are immutable.

The Pod's **memory request is the warm-set budget**. At tier A each warm model
costs roughly its weight size in host RAM; the request is what makes that
schedulable rather than hopeful.

## 3. How traffic reaches a woken model

This is the part with the sharp edges.

**Membership is per model, and it is a label on the Pod.** The EPP dispatches to
Pods that its InferencePool selects, and selectors select Pods, so this is
Pod-level by construction.

**Apply the target pool's own selector — do not assume a key.** The pool we
tested selects `{llm-d.ai/inferenceServing: "true", llm-d.ai/model: <name>}`, but
a selector belongs to the tenant and another may require `llm-d.ai/role=decode`
or something else. Membership is therefore "apply exactly the labels in that
InferencePool's `spec.selector.matchLabels`, and remove exactly those at sleep",
read from the object. WVA already reconciles InferencePools, so it has them;
hardcoding `llm-d.ai/model` is a bug waiting for the first tenant who differs.

**With one guard, which the sentence above does not admit.** A selector naming
`app.kubernetes.io/name` or `app.kubernetes.io/component` with a different value
is refused outright, and those two keys are never removed at sleep. A tenant's
selector is theirs to write and generic keys are common, so "exactly the labels"
taken literally would rewrite the Pod out of its OWN Deployment's selector: the
ReplicaSet releases it, and what is left is a GPU-holding orphan with no
controller, invisible to `ListWarm` and recoverable only by hand.

**A Pod carries one value per key, so it belongs to one model's pool at a time.**
That fits the one-awake invariant — and is an independent reason M>1 does not
work: even where memory allowed two awake models, the Pod could join only one
InferencePool, so the second model's traffic would never reach it. Membership
caps M at 1 regardless of the KV-cache arithmetic.

**Adoption is not a risk.** Adding another Deployment's labels to a Pod would
normally invite its ReplicaSet to adopt it; Kubernetes only adopts Pods with no
controller ownerRef, and pool Pods are owned by the pool's own ReplicaSet. The
decode ReplicaSet's selector also carries `pod-template-hash`, which a pool Pod
never has.

### Who writes what: local facts, global decisions

| concern | owner | why |
| --- | --- | --- |
| **readiness** — "is a model awake in me?" | the **Pod** (proxy `/readyz`) | a local fact, knowable only there |
| **membership** — "serve model X for that pool" | **WVA** | a global decision, knowable only from cluster state |

They fail safe in opposite directions, which is what makes the pair robust: if
WVA dies mid-bridge a stale label remains, but readiness still gates, so the Pod
leaves service the moment its model sleeps; and a misbehaving Pod can drop itself
out of service but cannot put itself INTO someone's InferencePool.

**The Pod does not label itself**, deliberately. It would need `pods: patch` in
the namespace — held by the container that runs model code, the least trusted
component here — plus `inferencepools: get,list` to know which labels to apply.
And wake and sleep are ordered sequences; two writers invites the race where a
Pod re-labels itself just as WVA decides to evict it.

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

- **M=1: the awake instance binds 8000 and nothing else is needed.** THIS IS
  WRONG, and it is the load-bearing error in this section. A sleeping vLLM
  process KEEPS ITS SOCKET -- `/sleep` frees GPU memory, not the listener -- so
  the instances cannot take turns binding 8000 even when only one is awake. A
  proxy is required at M=1, not at M>1.
- **What was built:** `internal/warmpool/proxy` owns 8000 in every pool Pod and
  forwards to whichever instance is awake, so waking a different model is a
  re-point rather than a rebind. It is an HTTP reverse proxy rather than a TCP
  one for two reasons found in review: the upstream resolves PER REQUEST, so a
  client holding a keep-alive connection across a wake reaches the model that is
  awake now; and a byte pump that stops when either side closes truncates a
  streamed completion the moment the client half-closes its request side, which
  is the ordinary shape of LLM traffic.
- **M>1 would additionally need routing by the `model` field** of the request
  body. That part remains unbuilt, and is the price of the 3.3x coverage in
  review §5.1.

**Ordering is load-bearing** and is already measured (admit 462 ms, drain 631 ms,
wake 355 ms, sleep 70 ms):

```
wake:   label  ->  /wake_up  ->  poll until the engine answers  ->  point the proxy
                                                                   (Ready; traffic arrives)
sleep:  clear the proxy  (Ready -> false)  ->  drain ~1 s  ->  unlabel  ->  /sleep
```

**The label is deliberately first on wake and last on sleep.** Because readiness
is what actually admits traffic, labelling early costs nothing and takes the
EPP's ~460 ms admit latency off the critical path -- it overlaps the wake instead
of following it.

On sleep the proxy is cleared FIRST, because that is the gate. Unlabelling first
would also work but propagates more slowly and leaves a window where the Pod is
Ready, still a pool member, and about to sleep -- which is exactly the
Ready-but-asleep condition that produces 503s.

## 4. The supervisor API

Small, synchronous, cluster-internal, one Pod's worth of state. Whether it is
FMA's launcher or ours, WVA needs exactly this:

| call | does | cost |
| --- | --- | --- |
| `GET /v2/vllm/instances` | list: id, status, the options it was started with | ms |
| `PUT /v2/vllm/instances/{id}` | spawn an engine for a model and load it | ~33-37 s [M] |
| `DELETE /v2/vllm/instances/{id}` | terminate, free RAM and GPU | seconds |

**Sleep and wake are NOT supervisor calls.** They are vLLM's own API, and the
control plane calls the engine directly -- `/sleep`, `/wake_up`, `/is_sleeping`
on the instance's port -- which is also how FMA's controller does it. An earlier
draft of this table listed them here, contradicting `warmpool/README.md`.

The supervisor owns process lifecycle and nothing else. **No policy lives in the
Pod** — which model to hold, wake or evict is WVA's decision, so that the cache
policy can change without touching the data plane.

## 5. WVA side

Every method above takes the POD as its first argument in the code
(`Warm(ctx, pod, model, tier)` and so on); the sketch omits it. That is
deliberate rather than an oversight in the implementation: which Pod to use is
the policy's decision, and an adapter that chose for itself would be making
allocation decisions inside the data plane. `Membership` also carries the
`Endpoint` the instance listens on, which the sketch does not show.

Package `internal/warmpool`, running as its own manager Runnable: a 5 s
housekeeping tick plus a decision-store trigger, so a borrow starts when WVA
DECIDES rather than when KEDA next polls. `internal/engines/scalefromzero` is
untouched -- an earlier draft proposed adding a branch there, and the trigger
made it unnecessary.

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

- **`internal/engines/scalefromzero`** decides a model must wake. An earlier
  draft had it gain a branch here — ask the pool first, fall through to the cold
  path on a miss. **That was not built and is not needed.** The pool subscribes
  to the decision store directly (§5's Runnable), so it learns of a scale-up at
  the moment WVA decides one rather than when this engine acts on it, and
  scale-from-zero is untouched.
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
a sleeper that fails to wake is corrupt and should be evicted. **NOT BUILT:**
the reconciler returns the Pod to the reserve and records a miss, leaving the
instance resident; see the eviction note in §7.

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
| **popular models** | **preload** while the pool is idle, top-C by share. BUILT: share is a variant's portion of desired replicas across the fleet, which tracks share of requests because holding utilisation roughly constant is what an autoscaler does. `--warm-pool-preload-top` selects how many; an entirely parked fleet has no signal and falls back to the other two sources | prefetch beats demand-fill when the distribution is known, and skew is what makes a small warm set work |
| **everything else** | admit on the second miss in a window | keeps one-off models out |

Every source is subject to the same two guards:

```
admit(model, pod) requires:
    free - 1 >= sleepMinSize          # never spend the reserve to fill the cache
    no borrow blocked recently        # not during a burst
    predicted_wake(model) << cold_start(model)   # NOT BUILT
```

The first two guards are implemented. The third -- review §3.5 made operational,
so that at TP=8 or 70 B the pool declines rather than underperforming quietly --
is **not built**: nothing yet estimates a model's wake against its cold start.

**The parameters are empirical, and the tool to settle them exists.**
`internal/warmpool/estimator` already carries `FillOnMiss` for exactly this: replay
a trace with demand-fill on and off, with and without a frequency filter, and read
the hit rates. Baking a window length or a C into the design now would be guessing
where measuring is cheap.
- **Eviction.** LRU by last-use, with explicit pinning. LRU is right because this
  is a cache and popularity is skewed; pinning exists because the operator knows
  things the recency signal does not.
  **NOT BUILT.** `Plan.Evict` exists and the reconciler carries it out, but the
  policy never populates it and `Membership.LastUsed` is never set, so a Pod that
  reaches `MaxInstancesPerPod` keeps its warm set forever. This is the largest
  unimplemented piece of the policy.
- **Placement.** Spread copies of a model across Pods so that expected concurrent
  wakes per Pod stay under M. With M=1 this degenerates to the anti-affinity rule
  the earlier design stated.
  **NOT BUILT, and currently prevented:** admission skips any model that is
  resident anywhere, so a model lives in exactly one Pod. Admission does spread
  DIFFERENT models across Pods (the roomiest Pod takes the next one), which is a
  weaker property than the one described here.
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
three states -- two of them serving no traffic:

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
        label(pod, variant)                     # joins the InferencePool
        wake(pod, variant)                      # /wake_up, then point the proxy
        pod.hold_until = now + maxHoldSeconds
    record: hits=borrow, blocked=(N - borrow if candidates exhausted)
            misses=(N - borrow if variant not resident anywhere)
```

The label goes FIRST, as in §3 and as the code does. Readiness is what admits
traffic, so labelling early costs nothing and takes the EPP's measured ~462 ms
admit off the critical path. (An earlier draft of this block had the two
reversed, which is the same defect that was corrected for the sleep ordering
in §7a.3.)

Note also that `blocked` here means "candidates exhausted", and candidates are
drawn from FREE Pods only -- so a variant resident in Pods that are all lent or
loading is blocked, not missed. The distinction decides which knob an operator
turns, and the code makes it explicitly.

Both paths run; neither waits for the other. If the pool covers the whole spike
the user sees capacity in under a second; if it covers none the behaviour is
exactly today's. **Nothing is worse than today in any branch** — that is what
makes the pool safe to switch on.

`blocked` and `misses` are recorded separately because they say different things:
a block means the reserve was too small (raise `sleepMinSize`), a miss means the
warm set was wrong (raise the warm set, or change what is admitted to it).

### 7a.3 Handover: return as soon as the ordinary replicas serve

```
on ordinary.ready rising (for the variant), per bridge rather than all at once:
    for pod in lent pods of `variant`:
        clear the proxy upstream      # /readyz -> 503, Pod NotReady
        wait drain (~0.7 s measured)
        unlabel(pod)                  # leaves the InferencePool
        sleep(pod, variant)           # ~70 ms
```

Bridges are returned INCREMENTALLY, not at a threshold: the policy hands back
any lending beyond `desired - ready`, so each ordinary replica that becomes
ready releases one pool Pod. Waiting for the full count would hold the reserve
hostage to the slowest replica.

Ordering is load-bearing and already measured. **The proxy is cleared first**,
because it is the gate: readiness follows the upstream, so clearing it is what
actually stops the EPP dispatching. Unlabelling first would also work eventually
but propagates more slowly, leaving a window where the Pod is Ready, still a pool
member, and about to sleep -- which is the Ready-but-asleep 503 condition.

(An earlier draft of this section stated the reverse order, contradicting §3.
§3 was right, the code follows it, and this is now the same sentence in both
places.)

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

**Not yet enforced in the collector.** Today it holds by accident: the
modelserver PodMonitor selects `llm-d.ai/role In (decode,prefill)` and a pool Pod
declares no `modelserver` port, so it is not scraped. A lent Pod carrying a
tenant selector that includes that role label is one manifest edit away from
being counted as a replica of the variant it is covering -- which is exactly the
self-suppression this section forbids. The collector should filter
`app.kubernetes.io/component: warm-pool` explicitly.

> **NOT BUILT.** Growth and shrink are reported, not acted on: the policy
> computes `GrowBy` and the reconciler logs it. Growing costs a full model load
> per Pod, and a shortfall that lasts one cycle is a borrow doing its job, so
> the decision is left to an operator.

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
| WVA restarts MID-ADMISSION | the engine is awake, in no InferencePool, with nothing pointed at it. It is reclaimed on the next pass -- see "orphans" below -- rather than counted as cover for the variant it was loading for |

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
```

`wva_warmpool_hold_expired_total` was listed here in an earlier draft and is not
implemented; an expired hold is currently visible only as a long
`bridge_seconds` observation.

`bridge_seconds` is the honest measure of what the pool is worth: it should track
the ordinary start time (~33-37 s). If it sits at `maxHoldSeconds`, ordinary
scale-ups are failing and the pool is masking it.

## 8. Configuration

> **NOT BUILT AS A ConfigMap — read this section as a wish list.** The
> configuration surface is seven flags:
>
> **Superseded.** The flag table below described the surface before
> `docs/proposals/warm-pool-configuration.md`. What is true now:
>
> | where | what it decides |
> | --- | --- |
> | pool **Deployment** annotations | `sleep-min-size`, `max-hold`, `preload-top`, `gpu-memory-utilization` — per pool, layered over the flags below |
> | pool **Deployment** labels | `llm-d.ai/warm-pool` names the pool; a namespace may hold several |
> | pool **Deployment** pod spec | GPUs per Pod and the memory limit, READ rather than configured |
> | the Pod's **node** | the accelerator a warm copy is reusable on |
> | pool **ScaledObject** | the pool's own size, via KEDA; `minReplicaCount` must exceed the reserve |
> | model **trigger metadata** | `warmPool` selects a pool, `warmPoolCopies` pins / opts out / asks for N copies |
> | remaining **flags** | `--warm-pool-namespace` (derived from the watched namespace when empty), plus the four knobs above as defaults |
>
> `--warm-pool-gpus-per-pod` is DELETED: its own help said it had to match the
> Deployment's GPU request, and a value that must equal another object's field
> is a second copy that can only agree or fail silently.
> `--warm-pool-memory-bytes` defaults to 0, meaning take the container's limit.
>
> Of the unimplemented list below, `accelerator`, `replicas`, `pin` and
> per-model copy counts now exist; `growDebounceSeconds` and `shrinkSlack` were
> implemented and then deleted, because an HPA's stabilization windows do the
> same job in an object operators already read.
>
> Everything else below has no counterpart: `tensorParallel`, `tier`,
> `awakeSlots`, `minPredictedSaving`, `parkedWithinHours` and `evict`
> are unimplemented, and `warmSetPerPod` is fixed at 16 rather than
> configurable at 8. The tier is hardcoded to RAM and the reconcile interval to
> 5 s.

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

Today the manager ClusterRole has `pods: get, list, watch, patch` — the `patch`
verb described below has since been added, and is in
`config/base/rbac/manager-clusterrole.yaml`. Before the pool it was
`get, list, watch`. The pool needs:

| resource | verb | why |
| --- | --- | --- |
| `pods` | `patch` | add/remove the model label (membership) |

`deployments: create, update, patch` was claimed by an earlier draft and is **not
needed**: the controller never writes a Deployment. Pool growth is reported, not
acted on, so the pool Deployment is an operator's object. Do not grant it.

**`pods/status` is NOT needed**, which is the second thing the readiness probe
buys (§3). An earlier draft asked for it to satisfy a readiness gate and called
it "a notable privilege" — correctly, which is why not needing it matters. It
also retires the trap that came with it: a JSON *merge* patch on pod conditions
replaces the array and wipes `Ready`, and that patch produced a spurious
measurement once already. There is now no such patch.

### 9a. Orphans: an engine that is awake and serving nothing

There is one state the state machine reaches that nobody intends, and it is
worth naming because the obvious handling of it is wrong.

An engine is AWAKE, in no InferencePool, and has no traffic pointed at it. It
gets there whenever the sequence that woke it did not finish: the controller
restarted between a model load and the sleep that should have followed it, an
Activate timed out between the wake and the re-point, or a Deactivate failed at
its last step. `stateOf` reports it as `Waking`, because that is what "answers,
not sleeping, proxy pointed elsewhere" looks like from outside.

The first implementation counted `Waking` as LENT, reasoning that a Pod
mid-activation is spoken for. That reasoning is sound and the state is not: a
pass observes before it acts, passes are serialised, and `Activate` runs to
completion inside the pass that decided it — so every `Waking` a decision ever
sees is already finished with, one way or the other.

Counting them as lent made the damage worse. The variant they were charged to
read as covered, so the borrow it actually needed was suppressed, and nothing
forced the situation open until the hold timeout — which, after a restart, had
also lost its start time and so began again from zero. A restart landing
mid-admission could therefore idle a GPU **and** withhold capacity from the
variant that needed it, for the full hold period, with no signal.

What happens instead:

- if the orphan's variant is SHORT, the borrow finishes what the interrupted
  sequence began. The engine is already awake for the right model; sleeping it
  and waking it again would cost a drain, a sleep and a wake to arrive back
  where it started.
- otherwise it is returned, which sleeps the model and puts the Pod back in the
  reserve. The warm set survives; only the GPU is given up.

A return a VARIANT asked for is never dropped this way, even when the same Pod
is borrowed again in the same cycle. That case is the hold timeout doing its
job: a bridge whose ordinary replicas never arrived is handed back while the
variant is still short, the shortfall takes it again, and the observation of how
long the bridge lasted — the signal that a scale-up is failing behind the pool —
is made each time round.

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
| 4 | readiness (an ordinary probe, not a gate) + label membership + drain ordering | 3 d | RBAC |
| 5 | cache policy (admit/evict/place/hold) | 4-5 d | — (pure, testable first) |
| 6 | scale-from-zero integration + miss fallback | 2 d | 3, 4 |
| 7 | metrics: hit/miss, slots busy, memberships, evictions | 1 d | — |
| 8 | e2e with a fake supervisor | 3 d | 3 |
| 9 | model-aware local router (M>1) | 4 d | **phase 2 only.** The per-Pod proxy itself was needed at M=1 and IS built; only routing by the request body's `model` field remains |

Roughly **three weeks to phase 1** (items 1-8, M=1, tier A, parking only), with
item 5 the only genuinely hard one.

**Supervisor decision.** *(Superseded: FMA's launcher is used unmodified — see
`warmpool/README.md`. Instance IDs, ports and GPUs are all caller-supplied, so
model-keyed identity and a local port range were WVA-side choices needing no
change to it.)* Write a minimal one. FMA's launcher is proven and has the
API, but it carries the dual-pods assumptions — instance IDs hashed from GPU UUIDs,
the ISC-derived port, reclaim behaviour — and we would be forking it to remove
them. A supervisor that spawns processes, calls three vLLM endpoints and reports a
list is a few hundred lines with no policy in it. Reuse if the fork proves
smaller than the rewrite; measure that on day one, not by argument.

## 12. Testing

> **What exists is not this.** The reconciler, policy, adapter and proxy are
> covered by ordinary Go tests with hand-written fakes and the controller-runtime
> fake client (~90% of `internal/warmpool`). There is no envtest suite. There IS
> an e2e suite — `test/e2e/warm_pool_test.go`, six specs, green on a real
> OpenShift cluster — which runs the REAL proxy image next to an emulated
> supervisor and engines, and applies the shipped NetworkPolicy rather than a
> copy of it. What it covers is what a unit test cannot reach: kubelet probe
> timing, policy enforcement, and real traffic through the proxy. What it cannot
> cover is a real vLLM sleeping and waking, which needs a GPU. Read the rest of
> this section as a plan for the envtest half.

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

## 13. Out of scope

Multi-node (LWS) pools; a CRD; a webhook; a scheduler plugin; cross-node model
migration; CRIU. Each is either out of scope per the review or waiting on a
measurement that has not been taken.

## 14. Built, and not described above

Decisions taken during implementation that no earlier section anticipated. They
are listed here rather than folded in, so that the difference between what was
designed and what was learned stays visible.

**A NetworkPolicy is the pool's only boundary** (`config/warmpool/`). Two of a
pool Pod's ports are dangerous: `:8001` spawns processes with caller-supplied
argv AND environment in a container mounting the shared model cache read-write —
arbitrary execution and a write primitive over other tenants' weights — and
`:8002` re-points where the Pod sends the traffic it receives. Both are
restricted to the controller. §9 discusses RBAC as "a real privilege increase"
and says nothing about this, which is the larger one.

A shared secret on those ports is deliberately NOT built. Authenticating `:8002`
while `:8001` sits behind the same rule would be theatre: identical reachability,
and the unauthenticated one is strictly more dangerous. Closing it means
authentication inside FMA's launcher, which means forking it.

**The admitted set is one NAMED namespace, and that costs an edit.** An earlier
version carried a second rule with a bare `podSelector` so a namespace-scoped
install would work untouched. A bare podSelector matches the policy's own
namespace — the tenant's — so it admitted anyone who could create a Pod there
and set two well-known labels: `kubectl run` away from arbitrary execution on
`:8001`, in a container mounting the shared model cache read-write. It applied
on cluster-scoped installs too, where it grants nothing needed. Naming the
namespace confines that exposure to installs which have actually chosen
co-tenancy, and getting the name wrong fails loudly rather than open — the
controller is denied, every `/is_sleeping` fails, and the pool simply never
wakes anything.

Nothing expressible in a NetworkPolicy makes a namespace-scoped install safe
against its own namespace's occupants, because there the controller and the
tenant genuinely share a namespace. A cluster that cares should install WVA
cluster-scoped.

**Probes are `exec`, not `httpGet`.** A kubelet probe originates from the NODE,
and no NetworkPolicy `from:` selector can name a node — so any restricted port
is one the kubelet may or may not reach depending on the CNI. Running the check
inside the container removes the question. The supervisor uses `python3`; the
proxy image is distroless, so the proxy binary makes the request itself
(`warmpool-proxy --check=ready|live`).

**The proxy will only forward to an engine.** An upstream must be loopback AND
in the pool's engine port range. Loopback alone is not a boundary, because the
dangerous ports are loopback too: an upstream of `127.0.0.1:8001` would turn the
serving port — the one every Pod in the namespace may dial — into a route to the
supervisor, straight through the NetworkPolicy.

**Readiness follows the engine, not the pointer.** An upstream that is set is not
an engine that answers. A vLLM process that crashed leaves the pointer untouched,
so readiness on the pointer alone keeps the Pod in its InferencePool and turns
every dispatch into a 502 — and the controller cannot put a dead engine to sleep,
so the Pod stays lent and its GPU is lost.

**Engine options are derived from the ordinary replicas' pod spec**
(`Demand.EngineOptionsFrom`), not configured. The two must MATCH: a different
`--gpu-memory-utilization` is a different torch.compile cache key, and a miss
costs ~9 s of compile on top of the load. Configuring them separately would make
that divergence silent and permanent. `--enable-sleep-mode` is added if absent,
since without it `/sleep` and `/wake_up` do not exist and the pool has no
mechanism at all.

**Every call the reconcile loop makes is bounded**, by three separate deadlines:
one observation, one act, one admission. The engine client's default timeout is
120 s, so one Pod that accepts a connection and never answers would otherwise
hold a pass for two minutes per instance behind it. An admission runs off-cycle
under its own budget, single-flighted per variant, because a ~35 s load must not
be re-issued every 5 s while it is still in progress.

**`DrainWait` is 4 s, not the ~0.7 s the measurements report.** The measurement
is of the EPP draining; the wait has to cover the whole chain — the kubelet
noticing `/readyz` fail, the Pod leaving its EndpointSlice, and then that drain.



### 14a. Sizing the warm set is a GPU question, not a host-memory one

`MaxInstancesPerPod` is 16, and reading that as "sixteen models per Pod" is
wrong in a way that only shows up on a cluster. It bounds the PORT RANGE. The
memory bound is elsewhere and much tighter.

Measured on an 80 GiB H100: one awake model at `--gpu-memory-utilization 0.95`
plus one sleeper left **4.4 GiB free**. The awake figure is not the model's
size — vLLM CLAIMS that share of the card and fills the remainder with KV cache
— and each sleeper holds ~1.4 GiB of GPU residue whatever its size. So at 0.95
a Pod has room for about three sleepers.

Host memory is not the constraint. The GPU nodes measured here carry ~2 TiB
each across 8 GPUs, so ~250 GiB per GPU, against a 48 GiB Pod limit. There is
an order of magnitude of headroom there and none at all on the card.

The dial is therefore `--gpu-memory-utilization`, now exposed as
`--warm-pool-gpu-memory-utilization`. Lowering it trades the awake model's KV
cache — and so its throughput — for warm-set breadth. It is not free in a second
way either: the value is part of the torch.compile cache key, so a pool that
uses a different one from the ordinary replicas misses the cache they populated,
measured at 12.35 s against 3.08 s. That cost is paid once per model per value
rather than per load, which is what makes the trade worth having at all.

Both of those are now measured. See §14d.

### 14b. A Pod on a new node must not start cold

Growing the pool means a Pod on a node that has never served these models, and
whether that is cheap or ruinous is decided entirely by where the weights live.

They live on the shared claim the ordinary replicas already use, mounted at
`/model-cache` with `HF_HOME` pointing at it. So a new Pod READS the weights
rather than fetching them, and finds the torch.compile cache another Pod
populated already there. Measured on Spectrum Scale, that read costs about the
same whether or not the node has served the model before.

Two consequences worth stating plainly, because both are load-bearing and
neither is enforced by anything:

**The claim must be ReadWriteMany.** Every Pod mounts it and Pods land on
different nodes, so an RWO claim means the first replica runs and the second is
stuck Pending on a multi-attach error. Nothing reports that as anything other
than a pool which never grew. The claim is the tenant's, not the pool's, so this
is a prerequisite rather than something the manifest can guarantee.

**A node-local cache would be faster and would give this up.** Weights on local
NVMe read faster than over a network filesystem, and would cut what remains of
the ~35 s load. It would also mean every new node starts cold, and every model
is fetched once per node. That is a real trade with a real upside; it is not the
one this design makes.

What growth still costs, unavoidably, is the LOAD: a new Pod must read each
model it admits into its own host memory and GPU, at ~35 s each, serially, each
holding a reserve slot while it happens. Nothing transfers resident state
between Pods -- different processes, different cgroups -- and vLLM loads weights
from a file rather than from another process's memory. That is why `GrowBy` is
reported and never acted on: the cost is real enough to be a person's decision.

The Pods also spread across nodes by preference. A pool concentrated on one node
is not insurance: losing that node loses every warm model at once, and the next
spike pays a full cold start for each of them.

### 14c. Parallelism: what a pool Pod can and cannot warm

A pool Pod holds a fixed number of GPUs, and a warm copy inherits the ordinary
replicas' parallelism flags. Those two facts decide the whole answer, and
getting them wrong used to be expensive rather than loud: the admission was
accepted, the engine could not start, and the ~35 s load was spent and re-spent
every cycle.

| | GPUs it asks for | in the pool |
|---|---|---|
| tensor parallel, TP=N | N | **yes**, in a Pod holding N. The engine sees every device the Pod has, so no placement logic is needed |
| pipeline parallel, PP=N | N | **yes**, same footing, within one Pod |
| expert parallel | **none of its own** | **yes**. It shards a mixture-of-experts model's experts across the ranks TP and DP already provide, so it changes how GPUs are used, not how many. Counted as a multiplier it would refuse admissions that fit |
| data parallel, DP=N | N (each rank is an engine replica) | **counted, least certain**. A DP engine is several engine cores, and whether sleep and wake reach all of them has not been shown here |
| anything spanning NODES (LWS) | — | **no**, and not a gap this could close — see below |

The base manifest holds one GPU, which is the common case and the cheap one.
`config/warmpool-multi-gpu` is the same pool with four, for models whose engines
span more. Only one number is involved now: the Deployment decides what the Pod
gets and WVA reads it from the Pod, so there is nothing to keep in step. A model
asking for more devices than a Pod holds is declined with a reason naming both
figures. Running both shapes at once means two pools with different
`llm-d.ai/warm-pool` names, each selected by the models that want it.

**Why LWS is a different design rather than a missing feature.** A LeaderWorkerSet
engine is several Pods; a pool Pod is one Pod holding several engines. Warming
one would mean holding an entire GROUP warm and coordinating sleep across every
member of it — and it rests on multi-node sleep, which has been claimed and
never demonstrated here. Such variants never reach the pool anyway: `Demand`
reads a scale target as a Deployment, so a non-Deployment target is skipped
before any of this applies.

**Since measured.** Both TP=2 and DP=2 sleep and wake correctly on two H100s in
one Pod, waking to a first token in 208 ms and 229 ms against 247 ms for a
single GPU — see §14d. The earlier "should behave, never run" caveat is retired.
The DP asymmetry is the part to size for: a DP=2 sleeper holds twice the host
memory of a TP=1 one, because each rank keeps its own copy of the weights.

**One optimisation deliberately not taken.** The supervisor can place an
instance on particular devices (`InstanceSpec` carries `gpu_uuids`) and the pool
never sets it, so every instance sees every GPU. With one engine awake at a time
that costs the awake model nothing, but sleepers all settle on the same devices
and their residue accumulates there instead of spreading. It shows up as a warm
set smaller than the arithmetic suggests, not as a failure.

### 14d. What a sleeper actually costs, measured

The sizing arithmetic rested on two assumptions, both taken from vLLM's
documentation rather than from this cluster, and both wrong in ways that
mattered.

**Level-1 sleep offloads to SHARED memory, not anonymous memory.** Measured on
an H100, an 8B model asleep:

| | awake | asleep |
|---|---|---|
| `anon` | 2,922 MiB | 2,922 MiB |
| `shmem` | 100 MiB | **21,494 MiB** |
| `memory.current` | 18,769 MiB | **40,250 MiB** |
| GPU | 78,161 MiB | 2,021 MiB |

Anonymous memory does not move at all. Anything watching `anon` -- which is the
obvious thing to watch, and what the first measurement here did -- reports a
sleeper as costing nothing whatsoever. The cost is real, it is shmem, and the
container's cgroup is charged for every byte of it.

**The cost is not the weights.** Two points:

| model | weights | charged while asleep |
|---|---|---|
| 0.6B | 1.1 GiB | 4.1 GiB |
| 8B | 14.9 GiB | 23.4 GiB |

which fits **2.6 GiB + 1.4x weights**. Estimating raw weights, which is what the
admission check did when it first shipped, understates an 8B by 57% -- in the
direction that admits a model which does not fit and OOM-kills the Pod, taking
every model already resident in it. `pool.ShapeOf` returns the fitted figure now.

A linear fit on two points is exactly as good as it sounds. It is enough to
decide admissions because both terms err upward relative to the weights, and the
consequences are asymmetric: too high declines an admission and costs a cold
start, too low costs the Pod.

**What this means for a Pod's memory limit:** at the 48 GiB this shipped with,
one 8B with room to spare, or two at a squeeze. That is the regime where the
pool loses to a spare replica -- see §2.8 of the review document, where the
saving works out to a factor of C, the models held per Pod. The base manifest is
128 GiB now, which is four. A 32B sleeper needs ~86 GiB, so even there C=1.

**GPU residue does not scale with the model, but is not constant either.**
1,399 MiB for a 0.6B, 2,021-2,843 MiB for an 8B across runs, and 4,319 MiB per
GPU at TP=2. So the "~1.4 GiB per sleeper" used earlier to reason about warm-set
size is a floor, not a figure.

**Tensor and data parallelism both sleep and wake correctly.** This was the
caveat carried through every earlier document -- vLLM propagates `/sleep` by
collective RPC, so it *should* work -- and it is now run, on two H100s in one
Pod:

| | load | sleep | wake + first token | after waking |
|---|---|---|---|---|
| TP=2 | 71.6 s | 966 ms | **208 ms** | answers correctly |
| DP=2 | 73.6 s | 1,225 ms | **229 ms** | answers correctly |

Both dropped each GPU to residue on sleep and restored it on wake. The wake
times are in the same band as the single-GPU case, which is the result that
matters: parallelism does not cost the pool its speed.

One asymmetry worth carrying: a DP=2 sleeper held **twice** the shmem of a TP=1
one, while a TP=2 sleeper held about the same. Each data-parallel rank is its
own engine with its own copy of the weights; tensor parallelism shards a single
copy across devices. The estimate multiplies by the DP size for that reason and
deliberately does not multiply by TP.

