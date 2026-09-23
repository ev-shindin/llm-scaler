# Sub-second scale-ups on llm-d: a multi-model autoscaler with a shared warm pool

*Measurements on CoreWeave H200s, September 2026.*

Traffic triples. The autoscaler notices, asks for a second replica, and
Kubernetes schedules it in about a second. Then the pod spends the next forty
seconds loading model weights and compiling kernels, and every request that
arrived in the meantime is queued behind a replica that is running but cannot
serve. By the time it can, the spike is half over.

That gap is what this post is about. On two 8B models behind one gateway, a cold
scale-up pushed p95 time-to-first-token to **5.1–8.8 s**. With one shared warm
GPU bridging the rise, the same spikes served at **0.11–0.83 s** — and cost
**12 % less GPU** than the usual fix of holding a floor of spare replicas.

We are [llm-scaling-manager](https://github.com/ev-shindin/llm-scaling-manager): a
multi-model autoscaler for llm-d, with warm capacity and scale-to-zero on top. It
decides the replica count; KEDA and the HPA actuate it. It began as a fork of
llm-d's Workload Variant Autoscaler and has diverged substantially since.

## Why a scale-up is slow, and not for the reason people assume

The models in that benchmark are 8B — deliberately, because they are the *cheap*
case. A model server that is not already running takes **~41 s** to serve its
first request (33–37 s on this cluster). That is the gap those rises are paying,
and it is already long enough to ruin a spike.

It gets worse with model size, and the instinct — weights are the problem, so put
them on faster storage — gets less right as it does. We measured GLM-5.2-FP8,
a 744B mixture-of-experts, starting on a warm 8×H200 node:

| | |
| --- | ---: |
| start → serving, weights read from local NVMe | 192 s |
| the same start with `--load-format dummy` (no weights read at all) | 152 s |
| so the weights account for | **40 s of 192** |
| first start on a node with a cold JIT cache | 463 s |

**The weights are a fifth of it.** A perfect peer-to-peer weight transfer would
take that start from 192 s to about 152 s. The rest is process spawn, imports,
memory profiling, kernel warmup and CUDA-graph capture — none of which a faster
disk, a bigger page cache or a peer transfer touches.

So at 8B a cold start costs ~40 s, at GLM scale it costs three to eight minutes,
and in neither case is it mostly the weights. A cold start cannot be made fast.
It can only be *not paid*. That is what a
warm pool is: a Pod holding an accelerator with models already resident, lent to
a model that is scaling up so it serves while its own replica starts. On a pool
Pod serving real gateway traffic we measured a **437 ms model switch against a
~41 s cold start**.

## What it costs, measured three ways

Two models bursting out of phase — the shape a pool exists for — 4020 seconds and
30 720 requests per arm, identical traffic in all three:

| arm | p95 TTFT per rise | GPU-seconds |
| --- | --- | ---: |
| autoscaling alone | 5.1 – 8.8 s | **12 129** |
| + one-Pod warm pool | **0.11 – 0.83 s** | 14 138 (+17 %) |
| floor of 2 replicas per model | 0.09 – 0.13 s | 16 080 (+33 %) |

Read that honestly, because the uncomfortable column is the useful one.
**Autoscaling alone is the cheapest arm** — it just pays for it with
five-to-nine-second rises. The pool buys floor-like latency and costs 17 % more
than holding nothing, because its accelerator is charged for the whole run.

The claim that survives is narrower and more actionable:

- **If you were going to hold a floor, hold a pool instead.** Same rises within
  tens of milliseconds, for **12 % less**, and one Pod covers every model that
  does not burst at the same time as another.
- **If you hold nothing and can live with multi-second rises, keep holding
  nothing.** We would rather tell you that than sell you a Pod.
- **The lever is models per Pod, not quiet per burst.** Two models and one Pod
  hold 3 accelerators in the quiet against a floor's 4, so the advantage caps at
  25 % here; a third model sharing the same Pod moves the ceiling, more quiet
  barely does.

No failures in any arm — 30 720 of 30 720 served, three times over.

## The part that is not the warm pool

A bridge only helps if the decision underneath it is right, and LLM autoscaling
breaks the assumptions a web-service autoscaler is built on. The llm-d community's
problem-space analysis (*Autoscaling in llm-d: The Production Problem Space*, by
Naina Singh) sets out nine structural reasons why. Three shape this design most:

**Load is not request count.** Two requests to one model can differ by 1000× in
compute — a 100-token chat completion against a 100K-token summarization. We
scale on token throughput, KV-cache pressure and the prefill/decode ratio,
through a closed-form model: an inter-token-latency fit plus a fair-share
allocation. Closed-form is deliberate. At $2–4/hr per GPU, an operator asked to
trust a scaling decision needs to be able to *read* it.

**Models compete for the same GPUs.** This is the part most autoscalers do not
attempt. Ours decides for the whole fleet at once rather than per Deployment:
every model, every variant, one GPU budget, one allocation. A shared warm pool is
the same idea in hardware — one held accelerator insuring several models, which
is why its economics improve with every model added to it.

**Platforms change capacity without asking.** In a self-service deployment the
platform onboards a tenant or deploys a model, and the inference layer has to
adapt with no human in the loop. Ours learns what it manages from the KEDA calls
themselves — no watch, no listing, no opt-in annotation. A newly registered
workload is managed on its first call.

## What we have not solved

Stated plainly, because a launch post that claims everything is worth less than
one that tells you where the edges are.

**Prediction.** A replica is ready minutes after the decision, so the right
question is where load will be *then*, not where it is now. We do not forecast —
the model is closed-form, trading anticipation for explainability. Against a
152 s construction floor on a large model that is a real gap, not a stylistic
choice.

**SLA-target-driven scaling.** Scaling on *"will this batch tier miss its
deadline"* rather than *"is queue depth > N"* is not built. We have named policy
tiers, which is a coarse approximation. Nobody upstream has solved this either.

**Maintenance pre-scaling and failure replacement.** Not built.

**Snapshots, which would have removed the 152 s.** Checkpointing an engine that
has already paid its construction cost, then refilling weights from a peer, is
the one mechanism that removes the part no transfer can touch. We built it and it
does not work yet: clean on a single process — GPU released to 0 MiB, dumped,
restored, identical checksum — and it **hangs on a real multi-rank engine**, with
the driver's own thread spinning after the data movement completes. Fifteen
experiments narrowed it to NVIDIA's checkpoint path rather than anything the
inference server can release.

Two smaller negative results, for anyone about to spend a month on them: a
GPU-less warm launcher **cannot wake** — holding the accelerator is what makes
the wake possible — and a weights PVC is not a latency fix (430 MB/s measured; it
avoids re-downloads, not cold starts).

## Try it

The benchmark above is a make target and runs against any llm-d install with two
models and a shared model cache. The scenario carries its own scrape interval,
replica ceiling and pool shape, so the three arms stay comparable:

```bash
export NAMESPACE=<the namespace running llm-d>
make check-prereqs          # read-only: tools, namespace, Prometheus
make setup-prereqs          # once per namespace, by a cluster admin
make deploy-wva             # the controller
make scaledobjects-plan     # list your model servers; nothing is applied yet
make scaledobjects-apply    # register them — this is what makes it scale
```

Method, guards and the full result:
[measured.md](../well-lit-paths/warm-pool-bridge/measured.md). The warm pool is
opt-in and off by default: [warm-pool-bridge](../well-lit-paths/warm-pool-bridge/).
What the project does and does not cover, property by property against the llm-d
problem space: [the project proposal](../project-proposal.md).

Caveats, in the open: four scale-up events per arm, one run each, no confidence
intervals. The direction is consistent across all four rises; the margins are one
run's.
