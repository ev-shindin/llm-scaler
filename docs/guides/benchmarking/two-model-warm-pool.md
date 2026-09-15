# Two models, anti-phase bursts, warm pool on and off

[← Benchmarking guide](README.md)

A warm pool is insurance: every pool Pod holds its accelerators continuously,
lending or idle, so a pool of *N* lowers your maximum fleet by *N*. The case for
paying that is that **one pool covers many models**. This scenario is the
measurement of that claim, and it is the one warm-pool number this repo has
never taken — every other has been a single model bridging its own scale-up,
where the pool is pure overhead at steady state.

Two models, each with its own EPP and its own ScaledObject behind one gateway,
take bursts that do not coincide. The sum of their demand is flat; each model's
own demand swings. Nothing in the system sees the sum — each model scales on its
own signal — so without a pool both models pay a full cold model load on every
rise, at the same moment the other model is giving capacity back. With a pool,
one Pod bridges whichever model is rising and is handed back in time for the
other.

The run is done twice with the same traffic, once with the pool and once
without, and compared on time-to-first-token **and** on accelerator-seconds.

## The phases

Default shape — a 2 minute lead-in, then four 8 minute phases (34 minutes):

| phase | window | model A | model B | what it is for |
| --- | --- | --- | --- | --- |
| lead-in | 0–120s | 3 rps | 3 rps | both stacks serve before anything is measured, so the first burst is a scale-up and not a first-request penalty |
| 1 | 120–600s | 3 rps | **9 rps** | B rises. A is idle, and A's replicas are capacity B does not have |
| 2 | 600–1080s | **9 rps** | 3 rps | A rises while B falls — the swap the pool is supposed to absorb |
| 3 | 1080–1560s | 3 rps | **9 rps** | B rises again, with whatever state the first cycle left |
| 4 | 1560–2040s | **9 rps** | 3 rps | A rises again |

**Why 8 minutes and not 4.** Scale-down stabilization here is 300s — fast up,
slow down, because under-provisioning costs TTFT irrecoverably while
over-provisioning only costs money. A phase shorter than that means the falling
model still holds every replica it grew right through the rising model's whole
window: the two never actually compete, and the anti-phase premise is not
exercised at all. `preflight` refuses below 420s.

The anti-phase property is **structural**, not written out: the generator builds
B's rate from the opposite level of A's, so no edit can make the two models
burst together while the file still claims they do not.

**The load has to be heavy enough to force a scale-up, and that is not obvious.**
Measured on an H200: one replica of an 8B model absorbed **40 rps** of
1000-token requests without queueing, because `maxNumSeq` defaults to 256 — so
neither arm ever added a replica and there was nothing to compare. The scenario
caps concurrency at `maxNumSeq: 32`, which is a normal production value, and the
defaults below (3 → 9 rps, 1000 input and 500 output tokens) then take each model
from one replica to three and back. Capping concurrency does not bias the
comparison: what a pool bridges is a **model load**, whose cost is fixed by the
weights and the storage, so this changes when a scale-up is triggered, not how
long the new replica takes to arrive — identically in both arms.

**The burst must also be SERVABLE at full scale**, and that is the other half of
the calibration. At 1000-token outputs, 12 rps was above what even three
replicas could deliver: the queue grew without bound, the driver pegged at its
worker cap, and both arms would have measured a permanently overloaded system
— "which one is less broken", not what a bridge is worth. Measured here, one
replica serves ~3.8 rps of 500-token requests and three serve ~11, so 3 → 9 rps
needs a scale-up and is comfortably served once it lands. If you change models,
accelerators or token counts, **re-measure both ends**: the rate that forces a
scale-up, and the rate the fleet can still serve at its ceiling.

Arrivals are a **Poisson process at the phase's rate, open-loop** — issued
whether or not earlier requests have returned. A closed-loop driver cannot
measure this: when the server slows it sends less, so the queue never grows and
TTFT stays flat, which makes a cold scale-up look free. The arrival times are
computed **before the run starts**, from a per-model seed, so both arms send
byte-identical traffic.

Change the shape with `PHASE_SECONDS`, `CYCLES`, `LOW_RPS`, `HIGH_RPS`.

## What makes the two arms comparable

Four ways they can silently stop being, each of which still produces a complete
and plausible table. The tooling enforces all four rather than trusting them:

| | enforced by |
| --- | --- |
| The pool arm must not be allowed **more cluster**. Insurance lowers the ceiling, so each model is capped at `MAX_REPLICAS - ⌈POOL_REPLICAS/2⌉` in the pool arm and `MAX_REPLICAS` without it — the same peak either way. | `run` writes `budget.json`; the report **refuses** if the pool arm could reach more accelerators |
| The fleet must start at the floor. With 300s scale-down stabilization and one arm always running first, the second would start on an already-scaled fleet and pay no cold load at all. | `reset` pins both ScaledObjects at `MIN_REPLICAS`, waits for it, then releases them |
| The pool must be **warm**. A cold pool pays a model load *into* the pool on the first burst, on top of the replica's own — a true measurement of a pool nobody would operate that way. | `warm` pins a copy of each model and waits; `run ARM=pool` re-checks |
| The traffic must be identical. | arrivals pre-computed from `SEED`; the report refuses if the seeds or the schedule differ |

## What it needs

- **Free accelerators for the peak**: `2 × MAX_REPLICAS × GPUS_PER_REPLICA`. At
  the defaults that is **6**, and it is the same for both arms. `preflight`
  counts what is actually free on schedulable, Ready nodes and refuses below it,
  because a run that spends its bursts Pending measures the scheduler.
- **One accelerator kind.** A warm copy is only reusable on the accelerator it
  was loaded on. `preflight` refuses if the cluster advertises more than one and
  `ACCELERATOR` is not set — a pool pinned to the wrong product is never
  eligible to lend, and nothing fails: the run completes and reports that the
  pool did nothing.
- **Two models of similar size**, both loadable from the model cache the pool
  mounts (`CACHE_CLAIM`, default `model-pvc`). A pool loads its warm copies
  through the same claim the models use.
- Everything [the warm pool guide](../warm-pool/) needs: the two images, the
  RBAC to patch Pods, the NetworkPolicy.

## Running it

Every step is separate and says what it found. The expensive failures here are
the ones discovered at minute 40 of a 45-minute run.

```bash
export BENCHMARK_NAMESPACE=<your namespace>
```

The models, their stack names and their accelerator budget live in
`hack/benchmark/scenarios/guides/two-model-warm-pool.yaml`. `MODEL_A`/`MODEL_B`
and `STACK_A`/`STACK_B` in the driver must match it; `verify` fails loudly if
they drift.

**1. Preflight — before anything exists.**

```bash
make benchmark-two-model-preflight
```

Free accelerators against the run's peak, one accelerator kind, the phase length
against scale-down stabilization, and the model cache claim.

**2. Stand up both stacks.**

```bash
make benchmark-two-model-standup
```

**One** standup, two stacks, one shared gateway. Not two standups: the
single-model guide uses `gateway.className: epponly`, which deploys no Gateway
at all and which the renderer refuses for a multi-stack scenario — so there
would be no shared address to drive.

**3. Verify, before any load.**

```bash
make benchmark-two-model-verify
```

Exactly one gateway Service, **two** EPP deployments, a ready decode replica per
stack, and a real request answered per model on its own path. A stack that 404s
for one model produces a full run of failures that reads like a pool result.

**4. First arm — no pool.**

```bash
make benchmark-two-model-reset
make benchmark-two-model-run ARM=nopool
```

Refuses if a pool is present. ~34 minutes at the defaults.

**5. Create the pool.**

```bash
make benchmark-two-model-pool-create
```

Created as a **bridge**, with `--max` equal to `--replicas` so the pool's own
ScaledObject cannot resize it mid-run and move accelerators under the
measurement. An idle pool Pod reports **NotReady on purpose** — that is what
keeps it out of the InferencePool. Do not wait on `readyReplicas`.

**6. Warm it — both models resident, before any load.**

```bash
make benchmark-two-model-warm
make benchmark-two-model-residency   # what each Pod is actually holding
```

This is the step that decides whether the run means anything. `warm` pins one
copy of each model with `warmPoolCopies: "1"` and then **waits** until both are
resident, by asking each pool Pod's supervisor over loopback — the pool's own
NetworkPolicy admits that port only from the WVA controller, deliberately, so a
probe Pod could never reach it.

Pinning matters beyond the first burst: in automatic mode a quiet variant loses
its slot to a busier one, and here each model is the quiet one for half of every
cycle — exactly before its own burst arrives.

**7. Second arm — with the pool.**

```bash
make benchmark-two-model-reset
make benchmark-two-model-run ARM=pool
```

Refuses if no pool is present, re-checks residency (the pool can lose a copy
between `warm` and `run`), and lowers each model's ceiling so the two arms peak
at the same number of accelerators.

`SKIP_WARM_GATE=1` says explicitly that a cold pool is what you meant to measure.

**8. Report.**

```bash
make benchmark-two-model-report
```

**9. Tear down — it is a shared cluster.**

```bash
make benchmark-two-model-teardown
make benchmark-two-model-status      # confirm nothing still holds an accelerator
```

## Reading the result

**TTFT over the whole run** is the least interesting. Most of a run is steady
state, where the pool does nothing but hold accelerators.

**TTFT per rise** — the first 90s after a model's rate goes up — is where a
bridge can matter. It is printed **one row per rise**, not pooled, because each
rise is a *single scale-up event*: the hundreds of requests inside it are not
independent samples, and pooling them yields a number that looks like n=700 and
carries the weight of n=1.

**Accelerator-seconds, including the pool's own.** A pool arm that wins on TTFT
while holding extra accelerators has not won; it has spent. Note this excludes
the pool's warm-up, which happens before the run — so it *understates* the cost.

**Failures are counted beside every percentile.** A request that did not deliver
all the tokens it asked for is a failure, not a fast success: counting torn
streams as successes lets a saturating arm shed its worst requests while its p95
improves.

The report **refuses** rather than tabulating when the arms are not comparable:
different schedule or seed, unissued arrivals (the driver, not the cluster, was
the limit), driver queueing above 250 ms at p95, or a pool arm that was allowed
more accelerators.

**How far this goes.** Four scale-up events per arm, one run each, no repetition
and no confidence interval. A difference of the same order as the spread between
the two rises of the same model is not a result. To claim a direction, repeat
the pair and check the sign is stable.

## What it does not measure

- **Throughput and ITL.** The output length is pinned (`ignore_eos`), so decode
  work per request is constant by construction; this run is about the arrival of
  capacity, not the quality of it.
- **A-versus-B.** The two models have different tokenizers, so the same prompt
  string is not the same number of tokens for each. Comparing arms is valid;
  comparing model A against model B is not.
- **In-phase bursts.** Both models spiking together is a capacity question, not
  a sharing one, and the pool's answer there is simply "a pool of two lends
  twice".
- **More than one pool.** One accelerator kind, one pool. See
  [several pools](../warm-pool/multi-pool.md).
