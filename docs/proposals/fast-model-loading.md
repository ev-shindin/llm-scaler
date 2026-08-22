# Fast model loading — a review from first principles

Seven documents and ~4,700 lines have accumulated on this subject
(`fma-*.md`). They are accurate but they grew incrementally around one
mechanism, and the commercial case was never stated in a form that can be
falsified. This document restates the problem from the beginning, keeps what
measurement supports, and discards what it does not.

**Status:** design review. The mechanism it argues for has since been built and
measured (§2.7); the economic case it makes has not been tested, because the
measurements that would test it are not available on any cluster we have (§6.1).

**Scope decisions taken as input:** one mechanism serves both use cases; pool
nodes may be required to have local NVMe; host RAM may be assumed available; no
cap on model size; forked or patched vLLM is permitted.

---

## 1. The problem, in business terms

A model server that is not already running takes **~41 s [M]** to serve its first
token — download or read weights, load them onto the GPU, initialise the engine,
pass readiness, get admitted by the EPP. On a GPU-starved cluster it is unbounded,
because the pod waits for a GPU before any of that starts.

Two situations make that expensive, and they have different buyers.

**Burst.** A model is serving and demand rises. The autoscaler asks for more
replicas; capacity arrives 41 s later. For 41 s the existing replicas absorb the
excess as queueing, so the cost is paid in tail latency — TTFT p95 measured at
19.7 s under load on a saturated variant, against 0.0195 s inter-token latency.
The buyer is whoever owns the SLO.

**Parking.** A model is idle and has been scaled to zero. Nothing is running, so
nothing costs anything; the first request after idle pays the full 41 s. The buyer
is whoever wants the cost savings of scale-to-zero without the latency that
currently comes with it.

The two share a mechanism — a model that is resident but not serving, made to
serve quickly — and that is why one design covers both. They do **not** share an
economic argument, and conflating them is the main flaw in the existing corpus.

## 2. Why a shared warm pool can pay

### 2.1 The honest competitor is not "do nothing"

The existing design frames the pool as insurance: premium = K GPUs held
continuously, payout = `T_ordinary - T_wake` per spike. That is right, but it is
benchmarked against doing nothing, which is the wrong baseline.

The real competitor is **one spare replica of the model itself**. It costs the
same GPU, and unlike a warm slot it *serves traffic while it waits*. For any model
busy enough to use the headroom, per-model spare capacity beats a warm slot
outright.

So the pool has exactly one thing to sell: **one slot covers many models**. Every
other argument is downstream of that ratio.

### 2.2 The pool is a loss system, and that is the good news

K slots, each held for W seconds by an arriving spike, spikes arriving at rate
lambda: this is M/M/K/K, and the probability that a spike finds every slot busy is
Erlang B. Offered load in erlangs is `a = lambda x W`.

| offered load `a` | K=1 | K=2 | K=3 | K=4 | K=6 | K=8 |
| --- | --- | --- | --- | --- | --- | --- |
| 0.25 | 20.0% | 2.4% | 0.2% | 0.03% | ~0 | ~0 |
| 0.50 | 33.3% | 7.7% | 1.3% | 0.2% | 0.01% | ~0 |
| 1.00 | 50.0% | 20.0% | 6.2% | 1.5% | 0.1% | 0.02% |
| 2.00 | 66.7% | 40.0% | 21.1% | 9.5% | 1.2% | 0.1% |
| 4.00 | 80.0% | 61.5% | 45.1% | 31.1% | 11.7% | 3.0% |

Read as coverage, at a 2% blocking target and a 60 s hold:

| spikes per model per hour | K=2 covers | K=4 covers | K=8 covers |
| --- | --- | --- | --- |
| 0.5 | 26 models | 131 | 435 |
| 1 | 13 | 65 | 217 |
| 2 | 6 | 32 | 108 |
| 4 | 3 | 16 | 54 |
| 12 | 1 | 5 | 18 |

At one spike per model per hour, **K=4 covers 65 models** — 4 GPUs where per-model
headroom would need 65, a 94% saving. That is the case, and it is a strong one.

### 2.3 Capacity is not coverage: the pool is a cache

The loss model above answers "is a slot free?". It quietly assumes any free slot
can serve any model, and that is false: **a slot only helps if that Pod already
holds the model's weights.** Otherwise the spike pays full admission and gets
today's behaviour. Coverage is therefore two independent questions:

- **capacity** — is a slot free? Erlang B, §2.2.
- **membership** — is my model resident anywhere? A *cache* question, and one the
  earlier documents never asked.

With P Pods each holding a warm set of S models, the pool holds `C = P x S`
memberships across N models. So the pool is a **cache of loaded models**, sized by
the working set rather than by the model count, and inference popularity is
strongly skewed, which is what makes a small cache viable:

| N models | skew | C=16 | C=32 | C=64 | C=128 |
| --- | --- | --- | --- | --- | --- |
| 100 | Zipf s=1.0 | 65% | 78% | 91% | 100% |
| 200 | Zipf s=1.0 | 58% | 69% | 81% | 92% |
| 500 | Zipf s=1.0 | 50% | 60% | 70% | 80% |
| 500 | Zipf s=0.8 | 34% | 43% | 55% | 68% |

**A miss costs exactly what today costs** — the cold path — so the pool degrades
gracefully and can be switched off without risk. That also fixes the headline
claim, which was overstated:

```
effective mean wake  =  hit x ~3 s  +  (1 - hit) x ~41 s
```

| N=200, Zipf s=1.0 | hit rate | mean wake |
| --- | --- | --- |
| C=16 | 58% | 19.1 s |
| C=32 | 69% | 14.8 s |
| C=64 | 81% | 10.3 s |
| C=128 | 92% | 5.9 s |

**The pool does not deliver 3 s. It delivers ~3 s to most spikes and ~41 s to the
rest** — a mean around 10 s at a plausible size, against 41 s today. That is the
number to put in front of anyone paying for it, and it is still a 4x improvement.

**This also gives the tiers their real job.** Memberships at level 1 cost host RAM
equal to the weights — 64 warm copies of an 8 B model is ~1 TB across the pool.
Level 2 memberships cost almost nothing in RAM.

> **Tier A buys cache SPEED. Tier B buys cache SIZE.**

Which is a two-level cache: hot models resident in RAM, warm models on local
disk, everything else cold. That is a better argument for building both tiers
than the current design's ("we have not measured which wins"), and it is
conditional on the same open question — whether level-2 wake is I/O-bound at all
(§3.2).

**Sizing therefore needs two numbers, not one:** K slots for capacity, C
memberships for coverage. Only K was ever discussed.

### 2.4 The assumption the whole case rests on

**Spikes must be close to independent.** Erlang B assumes arrivals do not
coordinate. Real inference traffic often does: a business-hours ramp lifts every
model at once, and a shared pool is worthless precisely when everything spikes
together, because K slots serve K models and the other N-K get the cold path.

This is the single most important thing to measure, and it is measurable today
from historical scaling events without building anything: take WVA's own
`wva_replica_scaling_total` and `wva_desired_replicas`, and compute the
distribution of concurrent scale-ups across models. If the 95th percentile of
concurrent scale-ups across N models is close to N, the pool does not work at this
cluster and no amount of engineering fixes it.

The rest of the sizing inputs are equally measurable and equally unmeasured:

- `lambda` — scale-up events per model per hour, from the same series;
- `W` — hold time per slot = `min(T_ordinary_start, maxHoldSeconds)`, from replica
  start times, which WVA already records as
  `wva_scale_from_zero_wake_seconds`;
- the correlation structure above.

**No pool should be built before these three numbers exist.** They are hours of
querying, not weeks of engineering, and they decide both K and whether to proceed.

### 2.5 The sensitivity that can kill the burst case

```
payout per spike = T_ordinary - T_wake
```

The premium is unaffected by `T_ordinary`. So anything that makes ordinary starts
faster shrinks the payout without shrinking the cost. If cold start falls from
41 s to 15 s — plausible from storage work alone — the payout drops from ~38 s to
~12 s, a two-thirds cut, while K GPUs are still held.

That is not hypothetical: CRIU snapshotting, weight-loading work and local-NVMe
placement all target exactly this number, and one of them is being built next
door (§4.4).

**Kill criterion.** If measured `T_ordinary` on the target cluster falls below
~15 s, the burst case does not justify the premium and the design should be cut
back to parking only. Parking survives, because at zero replicas the alternative
is not a faster start but no start at all.

### 2.6 Where parking's economics differ

For a parked model there is no spare replica to compare against — the model is at
zero by choice. The comparison is:

| option | GPU cost | first-token latency after idle |
| --- | --- | --- |
| scale to zero, cold wake | 0 | ~41 s [M] |
| scale to zero, warm slot | K/N of a GPU | 0.3-3 s [M] |
| never scale to zero (min=1) | 1 GPU per model | normal |

Warm parking is therefore a *middle* product: most of the saving of scale-to-zero,
most of the responsiveness of min=1. Its buyer is anyone who wants to park models
but cannot accept a 41 s first request — and its coverage ratio is even better
than burst's, because parked models spike rarely by definition.

### 2.7 The pool was built and measured (2026-08-21)

Not a projection. A GPU-**holding** pool Pod was deployed to pokprod
(`config/warmpool/`), two models were made resident in it, and every number below
was measured on it the same day.

| step | measured |
| --- | --- |
| create an instance -> engine serving | **~37 s** |
| `/sleep` level 1 | **~70 ms**, GPU 77,782 MiB -> **1,386 MiB** |
| `/wake_up` | **~355 ms** |
| engine answers after wake | **~8 ms** |
| **full switch: sleep one model, wake another, first answer** | **~437 ms** |
| output after a wake | correct (`2+2=` -> `4`) |

Four consecutive switches in both directions: 435, 439, 440, 435 ms. **A ~41 s
cold start becomes ~0.44 s** — two orders of magnitude, and an order better than
the 2-3 s FMA wakes, because nothing here binds a Pod or schedules anything: both
models are already resident in the Pod that serves them.

Two models on one 80 GiB card: **75,670 MiB awake alongside 1,466 MiB asleep.**
That is the warm-set arithmetic made concrete — a sleeper costs ~1.4 GiB, so the
warm set per GPU is bounded by what the AWAKE model leaves behind.
`--gpu-memory-utilization` is therefore the knob that trades KV cache for
coverage: at 0.90 about four sleepers fit beside the awake model; at 0.50 roughly
twenty-eight would.

**And no sleeper failed to wake.** The failure that motivated this whole design —
two of three unbound sleepers dead with `cumem` errors — did not occur, because
the Pod holds its GPUs for as long as it holds models.

Traffic through an InferencePool HAS since been served: the EPP dispatched to
the pool Pod as an ordinary peer endpoint, 19 of 30 requests in the run that
checked it.

What this still does not show: behaviour under concurrent load, more than two
models, or anything about how often a real fleet would hit rather than miss
(§6.1).

## 3. What measurement already forces

These are settled by experiment and constrain any design, whatever mechanism it
chooses.

### 3.1 Holding the GPU is what makes a wake possible at all

Of three sleeping instances whose GPU was **not** held by the sleeping process,
**two could not be woken**:

```
pfw8b: HTTP 500  CUDA error: invalid argument   cumem_allocator.cpp:145
v6fdd: HTTP 500  CUDA error: out of memory      cumem_allocator.cpp:139
zhwmp: woke correctly
```

The mechanism is understood: the sleeping engine's `cumem` handles refer to memory
on a device whose allocation was released when the holder went away; another
tenant then took it. Observed directly — the node's other five GPUs were 76-80 GB
used by others while our sleeper's card held 1,399 MiB and was the only one free.

**Consequence:** a warm copy that does not own its device is not merely slower, it
is unreliable. Every design where the sleeper is a passenger (FMA launchers
request no GPU; the requester holds it) inherits this.

**But this is not the only failure mode, and an earlier draft of this section
wrongly implied it explains the whole measurement history.** There are two, and
they bite in different situations:

| failure | cause | evidence | who suffers |
| --- | --- | --- | --- |
| requester lands where no warm capacity is | binding is **node-local** | 0 woke / 3 rebuilt: two requesters landed on nodes hosting no launcher at all | **burst** |
| sleeper's device was released and taken | GPU held by the requester, not the sleeper | 2 of 3 unbound sleepers unwakeable, `cumem` errors | **parking** |

The first is fixed, by configuration, and the result is decisive: pinning
requesters to the launcher node set and changing nothing else gave **3 woke / 0
rebuilt at 2 s, 3 s, 3 s — roughly 90 s down to 3 s, with no code change.**

So warm wakes already work today when placement is right. The GPU-ownership
problem is specific to a sleeper that must survive while **no requester holds the
device** — which is exactly the parking case, and not the burst case.

### 3.2 Storage-backed sleep does not pay on shared storage, and we do not fully know why

Level-2 sleep discards weights and re-reads them on wake. The often-quoted "8 B
wakes in ~37 s on this PVC" is an **extrapolation [P], not a measurement** — and
tracking it down resolves what an earlier draft called an unexplained gap.

Two different numbers exist for the same filesystem, and they measure different
things:

| figure | method | implied bandwidth |
| --- | --- | --- |
| 1.5 GB safetensors read in **~2.8 s [M]** | as the engine actually loads it | **~430-540 MB/s** |
| **1.7 GB/s [M]** cold | raw sequential, O_DIRECT | 4x higher |

The 37 s came from extrapolating the first to 16 GB; the 9.4 s from the second.
There is no missing 27 s — there is an engine load path running at roughly a
quarter of the device's raw sequential bandwidth, which is ordinary.

**The consequence still stands, and is now better founded:** what a faster disk
buys is bounded by the loader, not by the disk. Before committing to an NVMe
tier, measure the engine's *effective* load bandwidth on NVMe, not the device's.

**And storage is not what makes a cold start slow.** Measured directly: the same
1.5 GB read takes ~2.8 s whether or not that launcher has ever served the model,
because `/model-cache` is a shared RWX mount and there is no per-node download.
Of a ~41 s cold start, `torch.compile` is only ~3 s once the shared compile cache
hits (12.35 s on a miss), engine init ~9.3 s, weights ~2.8 s. **The remaining
~32 s is process spawn, vLLM import, weight load, profiling, warmup and API
startup** — none of which a cache removes and all of which sleep mode skips.
That is the real reason a warm wake wins, and it is stronger than "we avoid
reading the weights".
Identifying it is a prerequisite, not a detail.

### 3.3 What is already settled and needs no further work

| question | answer |
| --- | --- |
| Does a NotReady Pod receive EPP traffic? | **No** — zero dispatch and zero polling on kind; Ready gets ~20 GET/s |
| Does a woken engine return correct output? | **Yes, byte-identical** to a never-slept reference (greedy, fixed seed) |
| Readiness -> EPP propagation | **admit 462 ms, drain 631 ms** — sub-second both ways |
| Several engine instances on one card? | **Yes** — 1,399 MiB sleeper coexists with a live instance, no leak on delete |

So the Kubernetes plumbing (readiness gates traffic — see §5's note: an ordinary probe, not a Pod readiness gate), the correctness question,
and multi-tenancy of a single card are all answered. The remaining risk is
concentrated in the wake mechanism itself.

### 3.4 Ordering is load-bearing

Clear the proxy (readiness follows it) -> wait for the drain -> unlabel ->
`/sleep`. Sleeping first leaves a Ready-but-asleep window, which *is* the 503
condition. On wake the label goes FIRST, then `/wake_up`, then the proxy is
pointed: labelling early costs nothing and takes the EPP's measured ~462 ms
admit off the critical path.

This is cheap to get right and expensive to get wrong — and the wait is longer
than it looks. It has to cover the kubelet noticing the probe fail and the Pod
leaving its EndpointSlice, not just the ~630 ms the EPP itself takes to drain.

### 3.5 Large models weaken the technique, whatever the mechanism

No size cap was imposed, so this has to be said plainly: at TP=8 every mechanism
converges to roughly 4-10 s, because NCCL re-initialisation (1-3 s) and the sheer
volume of weights dominate. vLLM's own CUDA checkpoint RFC says the same —
"potentially negating benefits at large TP scales". A 70 B model at level 1 needs
~140 GB of host RAM per warm copy.

Warm pooling is a small-to-medium-model technique. Permitting large models does
not make them pay; it means the design must **decline** them explicitly rather
than silently underperform. Admission should refuse a model whose predicted wake
is not materially better than its cold start.

## 4. The mechanism space, now that NVMe and patched vLLM are allowed

| mechanism | wake, 8 B | cost | maturity | needs |
| --- | --- | --- | --- | --- |
| **A. RAM-backed sleep (level 1)** | **0.3-3 s [M]** | ~16 GB host RAM per warm model | works on stock vLLM today | `VLLM_SERVER_DEV_MODE=1` |
| **B. NVMe-backed sleep (level 2)** | ~3-10 s [P], **unvalidated** | ~MB RAM; local disk | works today | local NVMe; §3.2 gap resolved |
| **C. Shared-FS sleep (level 2)** | **37 s [M]** | ~MB RAM | works today | — (and does not pay) |
| **D. CRIU snapshot / restore** | ~5-10 s [claimed] | local NVMe per node | **prototype, "not for production"** | privileged node agent, patched vLLM + Model Express + llm-d router |
| **E. Just make cold start faster** | n/a — moves `T_ordinary` | **no premium at all** | ordinary engineering | storage placement, page cache, loader work |

Recommendation, given the inputs:

- **A is the default tier.** It is the only mechanism measured to deliver seconds,
  it needs nothing unmerged, and host RAM is now assumable. Its cost is honest and
  bounded: the Pod's memory request *is* the warm-set budget.
- **B is worth exactly one experiment, not a tier commitment.** Resolve §3.2
  first. If the missing 27 s is I/O, B becomes attractive on NVMe; if it is engine
  re-init, B is dead and the "build both tiers" commitment in the current design
  should be withdrawn.
- **D is adopt-later, not build-now** — even with patched vLLM permitted. Its
  foundation (vLLM RFC #34303) is an unmerged draft, its CRD surface is unsettled
  (`SnapshotReplica`/`SnapshotFleet`, not the documented `SnapshotCheckpoint`), and
  it needs a privileged DaemonSet. Permission to patch vLLM does not make an
  unmerged upstream draft a foundation.
- **E should run in parallel and is under-valued.** It has no premium, it helps
  every path including the pool's own admission step, and per §2.4 it is also the
  thing most likely to invalidate the burst case. Doing it deliberately is better
  than being surprised by it.

## 5. Architecture, reduced to its minimum

The mechanism does not need FMA's Kubernetes half. That conclusion is already in
the record — the warm-pool master notes "essentially just the launcher" is reused
and "the FMA fork is NOT on the critical path".

**It cannot be taken further than that.** An earlier draft of this section claimed
the launcher was unnecessary too; that is wrong. A GPU is allocated to a
*container*, so several models resident on one GPU means several vLLM processes in
one container, which needs a supervisor to spawn, sleep, wake and kill them —
which is what the launcher is. Reuse it or write a minimal one, but something
plays that role. See
[the implementation design](fast-model-loading-implementation.md).

**A pool is an ordinary Deployment.** One per `(accelerator, TP size, tier)` was
the intent; what was built is one per NAMESPACE, single tier, with no accelerator
or TP dimension.

- Each Pod owns its GPUs (§3.1 — non-negotiable) and holds several models, at most
  one awake.
- **Readiness gates traffic** (§3.3) — but through an ORDINARY readinessProbe
  against the proxy, not a Pod readiness GATE. What was built probes the proxy's
  `/readyz`, which answers 200 only while a model is awake in that Pod. The
  difference matters: a gate is a status condition someone has to patch, so it
  would need `pods/status` on the controller and would stop working the moment
  the controller died. A probe needs neither. Read "gate" throughout §3 as this
  probe.
- **WVA drives** wake and sleep from the loop it already runs, through five calls:
  `ListWarm`, `Warm`, `Activate`, `Deactivate`, `Evict`.
- Tier is expressed as *intent* (`desired-tier: ram|disk|none`), not as mechanism
  calls, so mechanism D can be swapped in behind the same port later.

What this deletes relative to the FMA-based proposals: the dual-pods controller
(~3.5k lines), the launcher-populator, `LauncherPopulationPolicy`, the
requester/SPI/probe/proxy path, and `pod-helper.go`. What it keeps: vLLM's own
`/sleep`, `/wake_up`, `/is_sleeping`.

Policy is where the real work is, and it is small in code and large in
consequence: anti-affinity of models across Pods (models on one card contend for
the single awake slot), eviction when the warm set is full, hold timeouts, and
admission refusal per §3.5.


### 5.1 Awake slots per GPU

A Pod may hold **M simultaneously awake instances** on one GPU, not one.

**What the existing measurements actually show, stated precisely, because an
earlier draft of this section overstated them:**

- one **sleeper** (1,399 MiB) coexisting with one **awake** instance at
  `--gpu-memory-utilization 0.15` (14,339 MiB) on the same GPU UUID, back to
  1,399 MiB on delete, no leak, sleep state undisturbed;
- `total=2 running=2` in one launcher — but on **two different GPUs**;
- the cluster already runs `--sleeper-limit=2` (sleepers per GPU) and
  `maxInstances: 4` (instances per launcher).

**Two simultaneously awake instances on ONE GPU has not been demonstrated**, and
the shared-pool plan asserts the opposite is the practical case: "only one
instance per GPU can be awake at a realistic `gpu-memory-utilization`".

That is the real constraint on M, and it is a budget, not a barrier: two awake
instances need utilisation split between them, so each gets roughly half the KV
cache and therefore half the batch width. The coverage below is what M buys; the
KV split is what it costs, and no measurement of that trade exists yet.

The arithmetic is still worth having, because more servers pay superlinearly in a
loss system:

| GPUs held | M=1 | M=2 | M=3 |
| --- | --- | --- | --- |
| K=2 | 13 models | 65 | 136 |
| K=4 | 65 | 217 | 396 |
| K=8 | 217 | 589 | 997 |

**3.3x the coverage for the same GPUs at K=4.** The objection is that co-resident
engines split HBM bandwidth and decode is bandwidth-bound, so both run slower --
disqualifying for steady-state serving. **It is not disqualifying here**, because
the pool serves only during the bridge, so the penalty is bounded in time by
construction, and a degraded bridge beats 41 s of nothing.

The conclusion is robust to being wrong about the size of the penalty. A slower
bridge holds its slot longer, so W scales with it:

| bridge penalty | W | M=2 covers | vs M=1 |
| --- | --- | --- | --- |
| 1.0x | 60 s | 217 | 65 |
| 1.5x | 90 s | 145 | 65 |
| 2.0x | 120 s | 108 | 65 |
| 3.0x | 180 s | 72 | 65 |

Even at 3x, M=2 still wins **if two awake instances are possible at all**. Two
things are unmeasured, not one: whether two engines can be awake on one GPU at a
utilisation that leaves each a usable KV cache, and the throughput penalty when
they are. The first decides whether M>1 exists; the second sets its value.

Like sleep level, **M is a pool property, not a runtime knob**: vLLM fixes
`gpu_memory_utilization` at process start and Pod resources are immutable. It also
relaxes an existing design rule -- models on one card no longer contend for a
single awake slot, so the warm set wants spreading such that expected concurrent
wakes per Pod stay under M, rather than blanket anti-affinity.

**MIG is the wrong tool for this.** Slices are fixed at node configuration time
and cannot be resized without draining the node, which is backwards for a pool
whose entire value is dynamic reuse across heterogeneous models; and the slice
caps model size. MIG suits static per-tenant partitioning, not a shared cache.

## 6. What must be measured, in order

| # | measurement | decides | cost |
| --- | --- | --- | --- |
| 1 | Spike **correlation** across models (§2.4) | whether a shared pool works at all | hours, from existing metrics |
| 2 | `lambda` and `W` (§2.4) | K, and the coverage ratio | hours, same source |
| 3 | `T_ordinary` on the target cluster | whether the burst case survives (§2.5) | already instrumented |
| 4 | The 27 s gap in level-2 wake (§3.2) | whether tier B exists | one experiment |
| 5 | Concurrent-wake bandwidth | whether K wakes at once share a bottleneck | one experiment |
| 6 | **Popularity skew** across models | C, the membership budget, and the honest mean-wake claim (§2.3) | hours, from EPP per-model request counts |
| 7 | **Co-residency penalty** under load | M, awake slots per GPU (§5.1) | one experiment |

1-3 and 6 are queries against metrics WVA and the EPP already emit. **They should
be done before any code is written**, because they can each independently end the
project, and that is a feature.

### 6.1 They were run, and the cluster cannot answer them (2026-08-21)

Measured on pokprod, 7 days, read-only queries:

| question | result |
| --- | --- |
| `lambda`, scale-up events per model | **0.40/hour for one model; 0.00 for the other seven** |
| correlation | **7 busy 5-minute buckets in 2,016**; never two models at once |
| popularity skew | 3 models carry traffic; one takes **76%** of requests |
| `T_ordinary` from `wva_scale_from_zero_wake_seconds` | **no observations at all in 7 days** |

**This is not evidence that spikes are independent. It is evidence of an idle
cluster.** pokprod is a test and benchmark cluster: the scale-ups in it are our
own runs, and 7 busy buckets in a week cannot distinguish independent arrivals
from correlated ones. Nor can 3 active models say anything useful about a Zipf
exponent over N.

So the gating measurements are **not takeable on the infrastructure we have.**
Sizing K or C from this data would be fabrication with a citation.

### 6.2 Therefore: ship the measurement, do not guess it

The way out is to stop treating Phase 0 as an analysis we perform once, and make
it something WVA computes continuously wherever it runs — including on clusters
we will never see.

A **warm-pool value estimator** in WVA: from the scaling events and per-model
request counts it already collects, compute what a pool *would* have delivered.

- replay scale-ups against a simulated cache of size C -> **hit rate**;
- replay them against K slots held for W -> **blocked spikes**;
- combine with the measured cold start -> **mean wake, with and without a pool**;
- publish as `wva_warmpool_would_hit_ratio`, `wva_warmpool_would_block_ratio`,
  `wva_warmpool_would_save_seconds`.

It needs no GPUs, no pool, and no new data source; it is a few hundred lines over
metrics that exist. It converts the unanswerable question into one **each adopter
answers on their own fleet**, and it is the honest precondition for asking anyone
to hold K GPUs. It is also independently useful: a cluster where the estimator
says "3% hit rate" has learned something real about itself.

**This is now the recommended first deliverable**, ahead of any pool code.

## 7. Phasing

- **Phase 0 — measure.** Attempted 2026-08-21 and **inconclusive on the available
  cluster** (§6.1). Replaced by: **build the value estimator (§6.2)** and run it
  wherever WVA already runs. Exit criterion unchanged in substance — `N/K >= 5` at
  a 2% blocking target and `T_ordinary >= 15 s` — but now evaluated from a
  cluster with organic traffic rather than from ours.
- **Phase 1 — parking on tier A.** One RAM-backed pool, warm parking for
  scaled-to-zero models, driven by the existing scale-to-zero path. Smallest
  surface, existing customer, and the pool sits idle by design so blocking risk
  is lowest.

  **With one blocker that has to be solved first, and it was missed in the first
  draft of this document.** Warmth decays while a model is parked: FMA's
  populator reaps launchers it considers excess, so a model parked for minutes
  wakes warm and one parked overnight wakes cold. `retentionPeriod` chooses when
  a model parks; **nothing chooses how long its warmth survives**, and there is
  no knob until upstream ask 2 (`minLauncherCount`) lands. Until then **no wake
  SLO can honestly be quoted for a long-parked model** — which is most of the
  parking case. Phase 1 therefore needs either that upstream change, a fork, or
  an explicit scope of "recently parked only".
- **Phase 2 — burst on the same pool.** Same mechanism, hold timeouts and
  handover to ordinary replicas. Requires phase 0 to have passed.
- **Phase 3 — tier B, only if item 4 says it exists.**
- **Phase 4 — adopt mechanism D behind the port**, when it is production-ready.

Phase 2 is deliberately *after* phase 1 despite being the larger prize, because it
is the phase whose economics can be invalidated by work happening elsewhere.

## 8. Open questions

- **Correlation is unmeasured and can end this.** (§2.4)
- **The level-2 27 s gap is unexplained.** (§3.2)
- **Multi-node (LWS) sleep has never been demonstrated**, and vLLM's own RFC defers
  it. Out of scope until phase 3 at the earliest.
- **Who owns the K GPUs?** A pool is a cluster-level asset paid for by one budget
  and consumed by many tenants. On a shared cluster that is a chargeback question
  before it is a technical one, and it has not been asked.
- **Does the snapshot work make the pool redundant rather than cheaper?** The
  current design assumes composition (snapshot lowers the floor, the pool removes
  it for K models). If snapshot restore reaches ~5 s for everyone, the pool's
  remaining payout is ~2-4 s per spike, which likely does not justify K GPUs.

## 9. Relationship to the existing documents

| document | status |
| --- | --- |
| `fma-warm-pool-design.md` | superseded on framing and economics; its measurements and sequencing detail remain the reference |
| `fma-launcher-owned-warm-pool.md` | historical — why launcher-owned alternatives were rejected |
| `fma-shared-warm-pool.md`, `fma-fork-problem-statement.md`, `fma-upstream-requests.md` | scoped to an FMA fork that §5 removes from the critical path |
| `fma-warm-pool-wva.md` | a workaround for an FMA reuse bug; not needed if the pool is not FMA-based |
| `fma-aware-attribution.md` | still live and independent — WVA cannot currently attribute FMA-held GPUs |

Nothing here contradicts a measurement in those documents. It disagrees with them
about what the measurements imply.
