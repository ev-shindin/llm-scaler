# Warm pool: what is decided, and what is not

**Status: open questions, deliberately unbuilt.** Three things the warm pool
raises that are bigger than a bug fix. Each is written down because leaving them
implicit is how they get half-answered by whoever touches the code next.

Two are about how WVA *reasons* about a pool. One is about what the pool *runs*.

---

## 1. The optimizer does not know a pool replica from a regular one

**Where this stands.** A pool's GPUs are now charged against the namespace's
quota, and a pool no longer asks for Pods the namespace cannot afford. The
*accounting* is honest. The *reasoning* is not: to the optimizer both kinds of
replica are simply GPUs held in a namespace.

They are not the same thing:

| | regular replica | pool replica |
| --- | --- | --- |
| serves traffic | yes | only while lent |
| exists to | answer requests now | make the NEXT scale-up fast |
| costs | its GPUs | its GPUs |
| removing it | drops capacity now | slows a future scale-up |

An optimizer that cannot tell them apart cannot answer the question an operator
actually has: **given N free GPUs, how many go to serving and how many to
warming?** Today that split is set by hand — the pool's `--replicas` and
`reserve` on one side, `maxReplicas` on the other — and neither side knows the
other exists beyond the contention hold and the headroom cap, both of which are
one-directional brakes rather than a decision.

Consequences visible today:

- **The pool always yields.** Contention makes the pool stop growing while a
  model replica is denied a GPU. That is the safe default and it is not always
  right: a pool Pod that would make the next five scale-ups take seconds instead
  of minutes may be worth more than one more replica now.
- **Nothing ever trades the other way.** There is no path by which the optimizer
  gives up a replica to let a pool exist, however much cold-start latency that
  would save.
- **The reserve is a fixed number, not a decision.** `sleepMinSize` is how many
  Pods stay free. Whether that is the right number depends on arrival rate and
  ramp time, which the optimizer measures and the pool never sees.

**What a real answer needs.** A cost model where a warm Pod's value is the
cold-start latency it removes, weighted by the probability of needing it —
comparable against the value of a serving replica, in units the optimizer already
reasons in. That is a design exercise, not a patch.

**What NOT to do meanwhile:** do not let the pool win contention by default, and
do not make `sleepMinSize` adaptive on a guess. Both would move GPUs on a model
nobody has written down.

---

## 2. The pool's vLLM version is FMA's to choose, not ours

**Measured 2026-08-28.** The pool's engine and the fleet's engine are different
images:

| | image | vLLM |
| --- | --- | --- |
| model servers | `vllm/vllm-openai:v0.26.0` | 0.26.0 |
| pool Pods | `llm-d-fast-model-actuation/launcher:v0.6.4` | 0.26.0 |

They agree today. Nothing keeps them agreeing. The model-server image is ours to
bump; the launcher's vLLM is whatever FMA built it on, because the launcher
imports vLLM and runs engines as child processes in its own container.

**Why it matters.** A lent pool Pod serves real traffic. If the fleet moves to a
newer vLLM and the launcher lags, some requests are answered by one engine
version and some by another — with no signal that it is happening.

**What breaking the coupling costs.** Less than it looks. `launcher.py` needs
vLLM, `fastapi`, `pydantic`, `uvloop` and its own `gputranslator`; the first is
the base image and the rest are already in it:

```dockerfile
FROM vllm/vllm-openai:v0.28.0        # the vLLM version WE choose
COPY warmpool/supervisor/ /app/
ENTRYPOINT ["python3", "/app/launcher.py"]
```

The real cost is import drift. The launcher reaches into four vLLM entrypoints,
and `vllm.entrypoints.serve.utils.api_utils` in particular is not a stable
surface. Owning the image means owning that breakage — which at least fails at
container start rather than silently.

**Recommendation:** build it when a vLLM version is needed that FMA has not
shipped, not before. Until then the digest pin plus a registry mirror covers the
availability risk. Note that building it also answers "what if FMA removes the
image", so the two motivations converge.

---

## 3. A warm copy is only as good as the engine it was loaded by

Follows from 2, and is worth stating separately because it constrains any fix.

The pool's promise is that a warm copy can serve a scale-up. That holds only
while the warm engine is interchangeable with a fresh one. Two ways it stops
being true:

- **Version skew** (above): the warm copy runs a different vLLM from the fleet.
- **Option skew**: the warm copy was created with the engine options the model
  had when it was admitted. A model whose Deployment later changes its args has
  a resident copy built to the old ones. `Warm` refuses to reuse an instance
  whose options differ, which turns this into a decline rather than wrong
  answers — the right behaviour, and worth keeping in mind as a reason a pool
  can look full while warming nothing.

---

## What has been decided

For the avoidance of re-litigating:

- **Pool GPUs count against the quota.** A pool is WVA's own consumption.
- **The pool does not ask for what the namespace cannot afford.** Pending Pods
  are not a queue.
- **The pool yields to model replicas under contention.** Not because it is
  optimal, but because the alternative is a pool starving live traffic, and
  nobody has written the model that would say when that is worth it. See 1.
- **Sleep level 1 only.** Level 2 discards weights; the reload is a separate call
  with no failure signal, and a model that answers fast with garbage is worse
  than one that answers slowly.
