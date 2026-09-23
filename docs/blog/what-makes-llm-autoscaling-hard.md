# What makes LLM autoscaling hard, and what we have measured about each part

*Draft. Measurements from CoreWeave H200s and OpenShift, September 2026.*

The llm-d community's problem-space analysis — *Autoscaling in llm-d: The
Production Problem Space*, by Naina Singh — sets out nine structural properties
that make autoscaling LLM inference a different problem from autoscaling a web
service, and orders the user stories by when operators actually hit them. It is
the clearest statement of the problem we have seen, and we agree with essentially
all of it.

This is our answer to it: for each hard part, what we built, what we measured,
and — for the parts we have not solved — what we know that is worth knowing
anyway. The property-by-property version, with the sources, is in our
[project proposal](../project-proposal.md).

We are [llm-scaling-manager](https://github.com/ev-shindin/llm-scaling-manager): a
multi-model autoscaler for llm-d, with warm capacity and scale-to-zero on top. It
decides the replica count; KEDA and the HPA actuate it. It began as a fork of
llm-d's Workload Variant Autoscaler and has diverged substantially since.

## The short version

| The hard part | Where we are |
| --- | --- |
| Load is not request count | **Built.** Token throughput, KV-cache pressure, an inter-token-latency fit |
| Scale-up takes minutes | **Built + measured.** Warm pool: rises of 5.1–8.8 s p95 become 0.11–0.83 s |
| Replicas are deeply stateful | **Built.** Sleep/wake, resident models in a shared pool, scale-to-zero parking |
| Multiple models compete for the same GPUs | **Built.** One joint decision across every model and variant, inside one GPU budget |
| Disaggregation splits the scaling unit | **Built.** P/D role is a variant and scales on its own bottleneck |
| Self-service platforms change capacity without warning | **Built.** No watch, no listing, no annotation — being called is being managed |
| Weights should come from a peer, not from storage | **Measured, not shipped.** Byte-identical, ~9× faster than reloading from storage |
| Predicting where load will be when the replica is ready | **Not built.** We are closed-form, deliberately. See below |
| Maintenance pre-scaling and failure replacement | **Not built** |
| SLA-target-driven scaling per workload class | **Not built.** Named policy tiers only |

The rest is the evidence.

## Scale-up takes minutes, and not for the reason people assume

The problem space puts a 5–9 minute figure on a new replica, up to 15–20 with
node provisioning. Our measurements agree, and locate the time somewhere
awkward. GLM-5.2-FP8 on a warm 8×H200 node:

| | |
| --- | ---: |
| start → serving, weights read from local NVMe | 192 s |
| the same start with `--load-format dummy` (no weights read at all) | 152 s |
| so the weights account for | **40 s of 192** |
| first start on a node with a cold JIT cache | 463 s |

**The weights are a fifth of it.** A perfect peer-to-peer weight transfer would
take that start from 192 s to about 152 s. The remaining 152 s is process spawn,
imports, memory profiling, kernel warmup and CUDA-graph capture — untouched by a
faster disk, a bigger page cache, or a peer.

Which is why "make the cold start fast" is the wrong goal. The goal is to not
pay one.

## So: a warm pool, and what it actually costs

A pool Pod holds an accelerator with models already resident and lends it to
bridge a scale-up. On a pool Pod serving real gateway traffic we measured a
**437 ms model switch against a ~41 s cold start**.

Two models bursting out of phase, 4020 s and 30 720 requests per arm, identical
traffic in all three arms:

| arm | GPU-seconds | p95 TTFT per rise |
| --- | ---: | --- |
| autoscaling alone | **12 129** | 5.1 – 8.8 s |
| + one-Pod warm pool | 14 138 (+17 %) | 0.11 – 0.83 s |
| floor of 2 replicas per model | 16 080 (+33 %) | 0.09 – 0.13 s |

We publish the uncomfortable column too. **Autoscaling alone is the cheapest
arm.** The pool costs 17 % more than holding nothing, because its accelerator is
charged for the whole run and that costs about twice what the smaller, shorter
scale-ups save.

The claim that survives is narrower and more useful:

- **If you were going to hold a floor, hold a pool instead** — the same rises
  within tens of milliseconds, for **12 % less**, and one Pod covers every model
  that does not burst at the same time as another.
- **If you hold nothing and can live with multi-second rises, keep holding
  nothing.**
- The lever that grows the advantage is **more models per Pod**, not more quiet.
  Two models and one Pod hold 3 accelerators in the quiet against a floor's 4, so
  the ceiling is 25 %.

That is the honest shape of the P0 story *"new replicas serving within 30 seconds
of the scaling decision"*: we serve them in under a second, and we tell you what
the insurance costs.

## Load is not request count

Two requests to one model can differ by 1000× in cost. We scale on token
throughput, KV-cache pressure and the prefill/decode ratio, through a closed-form
capacity model — an inter-token-latency fit plus a fair-share allocation across
every variant of every model at once, inside whatever GPU budget a declared
limiter allows.

Closed-form matters for a reason the problem space implies but does not say: at
$2–4/hr per GPU, an operator asked to trust a scaling decision needs to be able
to *read* it. A model you can argue with beats a model that is merely accurate.

## Multiple models compete for the same GPUs

This is the part most autoscalers do not attempt. Ours decides for the whole
fleet at once rather than per Deployment: every model, every variant, one GPU
budget, one allocation. A shared warm pool is the same idea in hardware — one
held accelerator insuring several models, which is why its economics improve with
every model added to it.

Moving capacity *between* workloads by priority — the P1 story about
lower-priority models yielding GPUs — is designed and not built.

## Self-service platforms change capacity without asking

Property 7 in the problem space: the platform onboards a tenant or deploys a
model, and the inference layer must adapt without a human.

Ours learns what it manages **from the KEDA calls themselves**. No watch, no
listing, no opt-in annotation — being called is being managed, and the trigger
metadata is the per-workload configuration. A newly registered workload is
managed on its first call, which is the property that self-service needs.

## The parts we have not solved

Put on the table, because the problem space asks for exactly this.

**Prediction.** Property 4 asks the autoscaler to predict where load will be when
the replica is ready. We do not. The model is closed-form — a deliberate trade of
anticipation for explainability — and against a 152 s floor for a large model,
that is a real gap, not a stylistic choice. Dynamo Planner forecasts; we do not.

**SLA-target-driven scaling.** The P2 story — scale on *"will this batch tier
miss its deadline"* rather than *"is queue depth > N"* — is not built. We have
named policy tiers, which is a coarse approximation. The problem space is right
that nobody upstream has solved this.

**Maintenance pre-scaling and failure replacement.** Not built.

**Snapshots, which would have removed the 152 s.** Checkpointing an engine that
has already paid its construction cost, then refilling weights from a peer, is
the one mechanism that removes the part no transfer can touch. We built it and it
does not work yet: the mechanism is clean on a single process — GPU released to
0 MiB, process dumped, restored, identical checksum — and **hangs on a real
multi-rank engine**, with the driver's own thread spinning after the data
movement has completed. Fifteen experiments narrowed it to NVIDIA's checkpoint
path rather than anything the inference server can release. We would rather say
that than leave it on a roadmap.

Two smaller negative results, for anyone about to spend a month on them: a
GPU-less warm launcher **cannot wake** — holding the accelerator is what makes
the wake possible — and a weights PVC is not a latency fix (430 MB/s measured; it
avoids re-downloads, not cold starts).

## Where this leaves an operator

If you are hitting the N+1 problem — replicas that take minutes while your
traffic has already spiked — there is something to run today, and the numbers
above tell you what it costs before you install it.

If you are further along, running mixed tiers with real SLAs, the honest answer
is that the SLA-driven part is unsolved by everyone, ourselves included, and the
useful thing we can offer is a decision model you can read and a fleet-wide
allocation to build it on.

Every figure here is reproducible: the benchmark is a make target that runs
against any llm-d install with two models and a shared model cache, and the
scenario carries its own scrape interval, ceiling and pool shape so the arms stay
comparable. Method, guards and the full result:
[measured.md](../well-lit-paths/warm-pool-bridge/measured.md).

Caveats, in the open: four scale-up events per arm, one run each, no confidence
intervals. The direction is consistent across all four rises; the margins are one
run's.
