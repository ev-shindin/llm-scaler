# Retained pools: holding several big models on one set of GPUs

**Status: design, not built.** What a pool has to do differently when the models
in it are large enough that nobody can afford a set of cards each.

## The case

A 70B-class model takes **300+ seconds** to start. That is past any request
timeout, so a model you have not already started is not slow — it is absent.
Having two such models available at once, the ordinary way, means two full sets
of accelerators, one of which is idle whenever the other is busy.

A **retained pool** holds several big models on ONE set of GPUs: one awake and
serving, the rest asleep and resident. Switching between them means draining the
awake one and waking another.

Measured on two H100 nodes (vLLM 0.26.0, a two-Pod TP=2 engine):

| | |
| --- | --- |
| awake | 78,015 MiB per rank |
| asleep, level 1 | **2,745 MiB per rank** |
| sleep | 0.07 – 0.71 s |
| wake | **0.36 s** |
| cold start, 70B class | **300+ s** (operator figure) |
| drain, expected | **< 30 s** (operator figure) |

The switch is therefore **drain-dominated**: call it 30 seconds against 300+,
and the wake is noise. That ratio is the whole argument, and it is why this is
worth building even though the pool holds GPUs continuously.

## What it is, concretely

A configuration option on a pool, not a new component. The pool machinery is
unchanged: pool Pods, a supervisor per Pod, one awake engine at a time, the
proxy as the traffic gate. What a retained pool changes is **who decides what is
awake**, and the fact that the answer changes over time rather than being set
once.

Two things follow immediately, and neither is a cost model:

### 1. Draining is the engine's job — SETTLED

This started as "make `Adapter.DrainWait` configurable", on the reasoning that 4
seconds cannot cover a 30-second request. That was the wrong fix for a real bug.

`/sleep` takes a mode, `Literal["abort", "wait", "keep"]`, and defaults to
**abort** — every in-flight request cancelled the moment the call lands. WVA sent
the default, so returning a borrowed Pod cut the responses it was still writing.
`mode=wait` is vLLM's own graceful drain: queue new adds, keep stepping until
what is running has finished, then sleep. It is the engine-level version of a
preStop hook plus a grace period, and the pool now uses it.

So no fixed wait can be right, and none is needed. Waiting a set number of
seconds is not the same as waiting for the work to finish; only the engine knows
when that is. What remains:

- `DrainWait` keeps a narrower job — the ROUTING chain, so no NEW request arrives
  after the engine is told to stop taking work (kubelet probe, EndpointSlice, EPP
  stops dispatching, ~630 ms measured). It stays short.
- The bound on a slow drain is the caller's deadline, because the sleep call
  blocks while the engine drains. `ActTimeout` is 60 s today, which covers a
  sub-30-second drain. A workload with longer requests raises that.

### 2. Both-asleep is a state worth avoiding

For small models a Pod holding two sleepers is fine — the reserve exists to be
lent. For a retained pool of big models it is waste: the GPUs are held, nothing
serves, and the pool's value is entirely in the model that is awake. A retained
pool should keep one model awake by default and treat "all asleep" as a
transient, not a resting state.

## When a Pod goes back: two different rules

A bridge and a retained pool answer this differently, and conflating them was the
first thing this design got wrong.

**A bridge sleeps at `min(the replica is really serving, warmPoolMaxHold)`.**
`warmPoolMaxHold` is not a lease on the Pod -- it is *how long to wait for the
ordinary replica*, after which the pool gives up on one that is not coming.

The first term is weaker in the code than in that sentence. The return rule reads
`Status.ReadyReplicas`, the kubelet's probe, where what matters is a replica
*serving*: taking requests and reporting metrics WVA can see. A replica that is
Ready but not yet producing metrics is one the pool has handed traffic back to on
the strength of a probe. Counting replicas that are Ready **and** reporting is a
narrow change -- the collector already reads per-Pod metrics -- and it is the
difference between "Kubernetes says it is up" and "it is doing the work".

**A retained pool has no such timeout.** Nothing is coming to relieve it, so
there is nothing to wait for and nothing to give up on. `warmPoolRetained: "true"`
turns the timeout off, and that is built. What still returns a Pod is the variant
no longer wanting it -- retained removes the timer, not the accounting, or the
second model would never get a turn.

### Where the switch signal comes from -- DECIDED

Something has to say *sleep this one and wake that one*. **It is WVA's own
signal, produced by the engine/optimizer and carried internally** -- not an
annotation, not a CRD field, not an operator action. The component that already
weighs demand against capacity is the one that knows a switch is worth making.

That settles the shape without settling the logic, which is the right order: the
decision rule needs the probability term this document keeps returning to, and
nothing about the transport depends on it. What the shape has to provide:

- **It names both sides.** Sleep A *and* wake B, decided together. A signal that
  only says "wake B" leaves the pool to work out what to displace -- the same
  judgement made twice, in two places, from different information.
- **It is a decision, not a wish.** The optimizer says what should be awake; the
  pool actuates it and reports what happened. If the pool may overrule it, the
  model that ends up awake is whichever component ran last.
- **It is idempotent and re-derivable.** A pool reconciles every few seconds, so
  "B should be awake" must mean the same thing on the tenth pass as the first,
  and must survive a controller restart by being recomputed rather than
  remembered.

The natural carrier is the decision store the pool already reads -- the same
place a borrow learns a variant is short. Recording what should be AWAKE beside
what is DESIRED keeps one component deciding and one acting, and puts the new
fact where the pool already looks.

Deliberately open: what makes the optimizer emit it. That is the cost model, and
guessing at it would move GPUs on a rule nobody wrote down.

## Graceful termination, at parity with regular replicas

A regular model server gets a drain patch from `make workload-patch`:

```yaml
preStop: exec: command: ["/bin/sh", "-c", "sleep ${WVA_DRAIN_SLEEP_SECONDS:-45}"]
terminationGracePeriodSeconds: <grace>
```

A fixed sleep covering two things at once: the endpoint's removal propagating
through kube-proxy and the EPP, and whatever is in flight finishing.

A pool Pod now has the same coverage and does the second half properly rather
than by guessing a duration -- its preStop asks every awake engine to sleep with
`mode=wait`, which finishes what is running and only then stops. The first half
needs no cover: a Pod with a deletion timestamp has already left its
EndpointSlice, so nothing new is arriving.

One honest gap remains, and it is the same for both: the grace period is the real
bound. 120 seconds here, after which SIGKILL cuts whatever is left.

## The three decisions, which are not one decision

The current policy has exactly one of these, and only in a hand-driven form.

### Switch — sleep A, wake B

**Cheapest, and completely absent from the code.** Both models stay resident;
the pool pays drain plus wake. Nothing in `policy.Decide` models this: a model
is either resident or not, and residency does not distinguish awake from asleep
except through `stateOf`, which is an observation rather than a choice.

Trigger: demand for B arrives while A is awake and idle. The judgement is which
model should hold the GPUs *now*, and it is cheap enough to make often.

### Evict — remove a resident model entirely

**Mechanism exists, policy does not.** `evictions()` acts only on variants that
named an explicit `warmPoolCopies`; automatic mode never evicts, on the stated
reasoning that a model the pool warmed is one it judged worth warming. A lent
copy is never evicted.

Eviction is expensive in one direction only: getting the model back costs a
300-second load. So it should be rare, and driven by *memory pressure* — a new
model cannot be admitted because the Pod's budget is full — rather than by
idleness. The candidate is the resident model with the lowest expected near-term
demand, which is the same probability term the cost model needs.

### Place — which models live in which Pods, and how many copies

Two rules are structural rather than economic, and can be stated now:

- **Anti-affinity between copies of one model.** Two copies in one Pod cannot
  both be awake, so the second buys nothing. Copies of a model belong in
  different Pods.
- **Affinity between models that are never hot together.** A Pod is useful in
  proportion to how rarely its residents contend. Models that peak at different
  times share a Pod well; models that peak together should not.

The count of copies is the economic part: one copy per Pod bridges one
simultaneous scale-up. That is `warmPoolCopies` today, set by hand.

## What is still missing, and why it is not written yet

Every decision above reduces to comparing **the value of a warm copy against the
value of a serving replica**, which is [open question
1](warm-pool-open-questions.md#1-the-optimizer-does-not-know-a-pool-replica-from-a-regular-one).
Today's measurements pin one half of it: a warm copy costs ~2.7 GiB per rank
plus its share of the pool's cards, and removes a 300-second cold start.

The other half is the **probability of needing the model soon**, and nothing in
WVA estimates it. Arrival rate per variant is the obvious source, and the
saturation analyzer already reads enough to derive one. Until it exists, every
rule here would be a guess that moves GPUs.

What NOT to do meanwhile, for the avoidance of re-litigating:

- Do not make the reserve adaptive on a hunch.
- Do not let the pool win contention against a model replica by default.
- Do not evict on idleness alone — for a 300-second model, being idle for a
  minute says very little about the next minute.

## Order of work

1. ~~Configurable drain wait~~ — **done**, and differently: the engine drains
   itself on `mode=wait`, and a pool Pod drains before it dies.
2. ~~Turn off the hold timeout for a retained pool~~ — **done**, via
   `warmPoolRetained: "true"`.
3. **An explicit switch signal**: sleep this model, wake that one. Settled as a
   WVA-internal signal from the engine/optimizer, carried through the decision
   store the pool already reads -- see above. The decision RULE stays open; the
   transport does not depend on it.
4. **Return on SERVING, not on Ready.** The bridge rule reads
   `Status.ReadyReplicas`, the kubelet's probe. What matters is a replica taking
   requests and reporting metrics. The collector already reads per-Pod metrics,
   so this is a narrow change with a real consequence: today a bridge can be
   handed back to a replica that has passed a probe and is not yet doing the
   work.
5. **Arrival rate into the pool.** Lambda is already estimated per variant
   (`arrivalFloor.Lambda`, by Little's law) and used for scaling. It does not
   reach the pool, and nothing turns it into a probability of needing a model
   soon -- which is what eviction and any demand-led switch are missing.
6. **Placement rules**, once there is a demand signal to place against. The two
   structural rules above can be enforced before that; the counts cannot.

## Two lines of work with measurements already behind them

Both make a switch cheaper still, both were investigated on this branch, and
neither is finished. The point of this section is that the earlier work should be
picked up rather than repeated.

### Switching a P/D pair

A disaggregated model is two engines, prefill and decode, and switching one
without the other serves nothing. The pool has no notion of a pair: it holds
models, and a P/D variant is two of them that must sleep and wake together.

**What is already known**, from `test/experiments/pd-role-swap/` (measured
2026-08-27 on Qwen3-8B): the difference between the two roles is *two scheduler
fields* -- `max_num_batched_tokens` and `max_num_seqs` -- each cached at init
into `max_num_scheduled_tokens` and `max_num_running_reqs`, and bounded above by
what the engine launched with. llm-d gives both roles the same `vllmCommon` and
the same `kv_role: kv_both`, so nothing else distinguishes them.

That matters here because KV geometry must NOT change: llm-d requires prefill and
decode to agree on layout, page size, dtype and attention variant, which is what
makes the NixlConnector handoff work. A swap has to preserve geometry rather than
reconfigure it -- and since geometry follows the model, it already does.

So the vLLM-side change is small and scoped: making those two fields settable
after init, and the pair's sleep and wake atomic enough that nothing is routed to
a half-awake pair. Scope it against vLLM before planning it; it is the one item
here that is not purely a WVA change.

### Filling a Pod from a sibling replica

Waking from level 1 costs 0.36 s because the process still holds its weights. The
expensive case is the one before it: getting weights into a Pod that has none,
which is the 300-second load a retained pool exists to avoid paying twice.

**This was measured, and it works** -- see
[warm-pool-weight-transfer.md](warm-pool-weight-transfer.md), reproduction in
`test/experiments/weight-transfer/`. On pokprod, Qwen3-0.6B, two H100s: a peer
broadcast filled a level-2 sleeper with **1.40 GiB in 0.11 s (13.1 GB/s)**
against **999 ms** for `reload_weights` from storage, and the refilled engine
produced byte-identical output. It works on a plain scale-up too, with no pool at
all: a replica started `--load-format dummy` serves nonsense until the broadcast
makes it correct, which removes the weight-read term from a cold start while
holding no extra accelerator.

Three prerequisites are already written down there, each of which cost a cycle to
find -- the receiver must be STARTED with `--weight-transfer-config
'{"backend":"nccl"}'`, the sender must drive the NCCL engine directly because
`WeightTransferTrainerFactory` registers none in v0.26.0, and the two halves must
overlap or they deadlock.

**Why it stopped, and what would restart it.** The interconnect here is the
limit: this cluster's fabric is slow enough that the transport half cannot be
measured honestly, and the whole question is whether it stays fast at 70B+ scale.
That needs a cluster with RDMA. Nothing about this should be designed from the
numbers this one produces.

P/D is the natural pairing for it, and that is not a coincidence: a P/D
deployment guarantees a peer holding the *same* weights, which is the hard part
of choosing a sender.
