# We fixed a scale-down bug, and it made our own feature look worse

*Draft. Measurements from CoreWeave H200s, September 2026.*

We build [llm-scaling-manager](https://github.com/ev-shindin/llm-scaling-manager),
a scaling manager for llm-d inference fleets. One of its features is a **warm
pool**: a Pod that holds an accelerator with models already resident, and lends it
to bridge a scale-up so the rise does not pay a cold model load.

In September we benchmarked it properly. Then we fixed an unrelated bug, re-ran
the same benchmark, and the warm pool went from **1 % cheaper** than plain
autoscaling to **17 % more expensive**.

The feature did not change. The baseline did.

## Why a warm pool exists at all

Autoscaling an LLM fleet is not autoscaling a web service, for one reason: a new
replica is not ready when it is scheduled, it is ready when the model is loaded.

On an 8B model that is seconds. On a large one it is minutes — and not for the
reason people assume. We measured GLM-5.2-FP8 starting on a warm 8×H200 node:

| | |
| --- | ---: |
| start → serving, weights read from local NVMe | 192 s |
| the same start with `--load-format dummy` (no weights read) | 152 s |
| so the weights are | **40 s of 192** |
| first start on a node with a cold JIT cache | 463 s |

**The weights are a fifth of it.** A peer-to-peer weight transfer that made them
free would take a scale-up from 192 s to about 152 s. The rest is process spawn,
imports, memory profiling, kernel warmup and CUDA-graph capture — none of which a
faster disk, a bigger page cache or a peer transfer touches.

So you cannot make a cold start fast. You can only arrange not to pay one, which
is what a warm pool is: insurance, bought by holding an accelerator.

## The benchmark

Two models behind one gateway, bursting out of phase so at most one scale-up is
ever in flight. 4020 seconds, 30 720 requests, three arms with identical traffic:

- **nopool** — autoscaling alone. Every rise pays a cold load.
- **pool** — the same, plus a one-Pod warm pool with both models resident.
- **floor** — no pool, but each model pinned to at least two replicas.

The first run said what we hoped:

| arm | GPU-seconds |
| --- | ---: |
| nopool | 14 893 |
| **pool** | **14 699 (−1 %)** |
| floor | 16 080 (+8 %) |

Latency-of-a-floor, for the price of no insurance. A good result, and wrong.

## The bug

Our decision loop publishes a desired replica count and KEDA's HPA actuates it.
The HPA takes the **maximum** of everything published inside its 300 s
stabilization window — so a single high sample pins the fleet up for five
minutes.

Our engine was oscillating across the scale-down threshold: one cycle it wanted
two replicas, the next three, the next two. Every one of those threes reset the
window. The fleet ratcheted up on noise and stayed there.

The fix holds a published scale-down decision across cycles unless demand
genuinely returns, releasing it the moment utilisation at the held count crosses
the scale-up threshold. It is about 250 lines and it is on by default. In the
re-run, every rise started from one replica per model — where before, as the
benchmark notes, "every cold rise in nopool drives the autoscaler to three or
four replicas and holds them through the 300 s scale-down window".

## The re-run

Same traffic, same arms, the bug fixed in all three:

| arm | GPU-seconds | p95 TTFT per rise |
| --- | ---: | --- |
| **nopool** | **12 129** | 5.1 – 8.8 s |
| pool | 14 138 (+17 %) | 0.11 – 0.83 s |
| floor | 16 080 (+33 %) | 0.09 – 0.13 s |

Plain autoscaling got **19 % cheaper** — 14 893 down to 12 129 — because it had
been the arm the bug hurt most. Every cold rise drove the autoscaler to three or
four replicas and the chatter held them there. The pool arm barely moved: it was
already scaling in smaller steps, so it had less ratchet to lose.

Fixing the baseline took the warm pool from *free* to *17 % more expensive*.

## What we actually think now

The honest claim is narrower than the one we started with, and more useful:

**If you were going to hold a floor, hold a pool instead.** Same rises within
tens of milliseconds, 12 % cheaper, and one Pod covers as many models as never
burst together.

**If you are holding nothing and can tolerate multi-second rises, keep holding
nothing.** Autoscaling alone is the cheapest arm. The pool's accelerator is
charged for the whole run whether lent or idle, and that costs about twice what
the smaller, shorter scale-ups save.

**The lever is models per Pod, not quiet per burst.** Two models and one Pod hold
3 accelerators in the quiet against a floor's 4, so the advantage caps at 25 %,
and only approaches it as the quiet fraction approaches 1. Three models sharing
the Pod move that ceiling; more quiet barely does.

And when one model is always bursting, the fleet never returns to its floor and a
floor is the cheaper insurance. A warm pool is for traffic with quiet in it.

## The part worth generalising

We nearly shipped the first table. It was measured, reproducible, and it flattered
us.

What saved it was that the fix and the benchmark were the same week — so we re-ran
three arms we had already run, instead of trusting a number that was still true
about a system we no longer had. A benchmark is a measurement of one build. Ours
had been measuring a bug as much as a feature, and the feature it flattered was
the one we wanted to be right about.

Caveats, in the open: four scale-up events per arm, one run each, no confidence
intervals. The direction is consistent across all four rises; the margins are one
run's.

---

The benchmark is a make target and runs against any llm-d install with two models
and a shared model cache — the scenario carries its own scrape interval, ceiling
and pool shape, so the arms stay comparable. Method, guards and the full result:
[measured.md](../well-lit-paths/warm-pool-bridge/measured.md).
