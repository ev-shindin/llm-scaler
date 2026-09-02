# Hold several large models on one set of GPUs

> **Experimental.** The rule is covered by nine unit test files and a benchmark
> variant config, and it has been exercised on a cluster — but **no end-to-end
> suite covers a retained pool**. That is the missing leg. Everything else here
> is built and shipped.

Some models are too large to start on demand: by the time one loads, the request
that wanted it has timed out. For those, a warm pool stops being insurance and
becomes the serving capacity itself. The models live in the pool, exactly one is
awake at a time, and no ordinary replicas are coming — so something has to
decide **which model holds the accelerators**, or whichever one woke first keeps
them however the load moves.

**Use it when** you have more large models than you have GPUs to run them on,
their traffic does not peak together, and a switch between them is cheaper than
buying hardware for the sum of them.

**Do not use it when** two of the models are busy at the same time. A retained
pool cannot make two models comfortable at once, and it knows that — it will
refuse to switch rather than move the shortage from one model to the other and
pay a drain and a wake to do it. If your models peak together, this path is the
wrong shape and the answer is more accelerators.

**This is not the bridge case.** If your models start fast enough that ordinary
replicas do arrive, you want
[the warm pool bridge](../warm-pool-bridge/) instead, where the pool covers the
gap and hands the work back.

## What it needs

- A pool sized for the largest model, matching one shape: GPUs per Pod, how
  they are divided, and the accelerator — a warm copy is only reusable on the
  one it was loaded on.
- Memory limits that hold the whole warm set. A level-1 sleeper is charged for
  its weights in shared memory, so the pod template's memory limit is the budget
  and one model too many OOM-kills the launcher, taking **every resident model**
  with it.
- The retained knobs, in the pool ScaledObject's trigger metadata:

```yaml
warmPoolRetained: "true"
warmPoolSwitchSpareThreshold: "20"    # percent; unset = switch only on scale-up need
warmPoolMinSwitchInterval: "10m"      # default 10m
```

## The rule that decides who is awake

In the order it is applied:

1. **A candidate must be under more pressure than the model already awake** —
   either below the spare threshold when the awake one is not, or wanted for
   scale-up when the awake one is not.
2. **Equal pressure is not enough.** If the awake model is *also* short, nothing
   moves.
3. **Ties go to the tightest** — least spare capacity wins.
4. **No switch within `warmPoolMinSwitchInterval`, unless the candidate needs to
   scale up now.** A model heading toward trouble can wait; one short of
   capacity cannot, and no replicas are coming to relieve it.

Spare is a fraction of each model's *own* supply, so one threshold compares
models of different sizes. Nothing is decided from a reading that does not
exist: an unmeasured model is neither a candidate nor a reason to stay put.

## Setting it up

[The warm pool guide](../../guides/warm-pool/), which covers pool sizing, the
warm-set budget, and the retained section specifically. `deploy/warmpool.sh plan`
groups a namespace's models by the shape a pool can serve and prints the create
lines.

## Verifying it

```bash
# which model the pool is holding awake, and why it moved
kubectl logs -n <wva-namespace> deploy/<wva> | grep "retained pool is switching"
```

The line names the model, the reason and the interval. The decision **not** to
switch is logged too, at debug, with its reason — which is the more useful of
the two when you are asking why nothing happened.

## What it costs

The whole pool, continuously: those accelerators serve one model at a time and
are unavailable to anything else. That is the trade — you are buying the ability
to serve N models from the hardware for one, at the price of a switch whenever
demand moves.

## How it is tested

- Unit: `internal/warmpool/policy/retained_test.go`,
  `policy/switch_test.go`, `policy/intent_test.go`, `retained_pass_test.go`,
  `retained_stay_log_test.go`, `switch_test.go`, `switch_storm_test.go`,
  `internal/decision/awake_test.go`, and
  `internal/engines/analyzers/saturation_v2/warmpool_bridge_test.go` — covering
  the pressure comparison, the tie-break, interval preemption, and the switch
  storm that an earlier version of this rule produced.
- Benchmark: `hack/benchmark/scenarios/guides/variants/v2-retained-switch.yaml`.
- **Not covered end to end.** `test/e2e/` contains no retained-pool spec; the
  warm-pool suites there exercise the bridge case.

## Tuning it

The knobs above, and the design reasoning — including why a switch needs both a
threshold and an interval — are in
[proposals/warm-pool-retained.md](../../proposals/warm-pool-retained.md).
