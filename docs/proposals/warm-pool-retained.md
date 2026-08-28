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

### 1. The drain wait must be sized to the workload

`Adapter.DrainWait` is **4 seconds**, a constant, with no flag reaching it. The
return sequence clears the proxy first — the gate shuts and the EPP drains in
~630 ms — then waits `DrainWait`, then sleeps the engine.

For a big model with 30-second requests that cuts in-flight work on every
switch. This is a prerequisite, not a refinement: it must be configurable, and
the pool must not sleep an engine before its longest in-flight request can
finish. Sizing it from the workload's own latency SLO is better than a number an
operator has to guess, and either beats 4 seconds.

### 2. Both-asleep is a state worth avoiding

For small models a Pod holding two sleepers is fine — the reserve exists to be
lent. For a retained pool of big models it is waste: the GPUs are held, nothing
serves, and the pool's value is entirely in the model that is awake. A retained
pool should keep one model awake by default and treat "all asleep" as a
transient, not a resting state.

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

1. **Configurable drain wait**, sized to the workload. A prerequisite, and
   useful on its own: today's 4 seconds is wrong for any model with slow
   requests, retained pool or not.
2. **Switch as an explicit action** in the policy, distinct from borrow and
   return, with the trigger stated in terms of demand rather than idleness.
3. **Arrival-rate estimate per variant**, which is what both the switch and the
   eviction decision are missing.
4. **Placement rules**, once there is a demand signal to place against. The two
   structural rules above can be enforced before that; the counts cannot.
