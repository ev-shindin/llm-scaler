# Upstream issue draft: the shipped prefix-cache scorer pins a model to one replica, so a scale-up adds capacity that is never used

Target: `llm-d/llm-d-inference-scheduler` (the shipped scheduling profile),
with a note for `kubernetes-sigs/gateway-api-inference-extension` (the
`prefix-cache-scorer` plugin).

## Summary

With llm-d's shipped scheduling profile — `prefix-cache-scorer` at weight 3
over `queue-scorer` 2 and `kv-cache-utilization-scorer` 2 — a workload whose
prompts share prefixes is routed to whichever replica served each prefix
first, **regardless of that replica's load**. A replica added by the
autoscaler receives no requests, therefore caches no prefixes, therefore never
starts winning: a self-reinforcing lock-in. Measured over a 38-minute run with
two models scaling 1 → 3 each: **99.0 % and 98.7 %** of each model's prompt
tokens went to one engine, and every replica the autoscaler added served
0.3–2.0 %. In an earlier run with a single shared prefix, the added replicas —
including one that was awake, Ready, in the EndpointSlice and in the EPP's own
backend list — served **exactly zero** tokens against 6,000,692 on the first.

The scorer's index is an estimate maintained by the EPP; it is not informed by
the engine. In this stack the engines ran with `--no-enable-prefix-caching`, so
the preference bought nothing at all (`vllm:prompt_tokens_by_source_total{source="local_cache_hit"}` = 0 throughout) while costing the whole scale-up.

## Environment

| | |
| --- | --- |
| EPP | `ghcr.io/llm-d/llm-d-router-endpoint-picker:v0.9.0` (llm-d-inference-scheduler v0.9.0, GIE v1.5.0) |
| Profile | the shipped `optimized-baseline` profile, `flowControl` on (below) |
| Engine | vLLM v0.26.0, `--max-num-seq 32`, `--no-enable-prefix-caching`, block size 64 |
| Models | `unsloth/Meta-Llama-3.1-8B-Instruct`, `Qwen/Qwen3-8B`, one InferencePool + one EPP each, one shared gateway |
| Hardware | 1 × H200 per replica |
| Load | inference-perf v0.6.1, Poisson open-loop, 1000 input / 500 output tokens |

```yaml
apiVersion: llm-d.ai/v1alpha1
kind: EndpointPickerConfig
featureGates:
- flowControl
plugins:
- type: queue-scorer
- type: kv-cache-utilization-scorer
- type: prefix-cache-scorer
- type: no-hit-lru-scorer
schedulingProfiles:
- name: default
  plugins:
  - pluginRef: queue-scorer
    weight: 2
  - pluginRef: kv-cache-utilization-scorer
    weight: 2
  - pluginRef: prefix-cache-scorer
    weight: 3
  - pluginRef: no-hit-lru-scorer
    weight: 2
```

## What was observed

**Run A — the suite's own dataset shape.** inference-perf `shared_prefix`,
`num_groups: 32`, `num_prompts_per_group: 64` (2048 distinct prompts, cycled;
750 shared + 250 unique tokens each), 3 → 9 rps bursts, autoscaler at 1 → 3
replicas per model. Per-engine `vllm:prompt_tokens_total` over the whole run:

| model | busiest engine | every added replica |
| --- | ---: | ---: |
| Llama-3.1-8B | 99.0 % | 0.3–2.0 % each |
| Qwen3-8B | 98.7 % | 0.3–2.0 % each |

During a burst the busiest engine reached 29 of its 32 sequence slots
(`vllm:num_requests_running`) with an added replica idle beside it. TTFT p95
in the burst windows, without a warm pool, was 5.6–9.3 s.

Note that **the aggregate across the two InferencePools hid this**: the busiest
engine held 48.9 % of all prompt tokens, which reads as a healthy spread. The
pin is per pool.

**Run B — a single shared prefix** (`num_groups: 1`), same everything else:
one engine 6,000,692 prompt tokens; every other Ready replica **0**, including
a warm-pool Pod that was awake and listed in the EPP's backends.

**Run C — no shared prefix.** inference-perf `synthetic` (each prompt a fresh
random slice of a corpus), otherwise identical. A second replica that started
**empty** took **49.4 %** of 9 rps from its first minute (p50 TTFT 58 ms at 2
replicas), and over a full autoscaled run every added replica took ~50 % of
its model's traffic within two minutes of Ready.

So the router, not the autoscaler or the engines, decides whether a scale-up
does anything, and with the shipped weights it decides against it for any
workload with recurring prefixes.

## Why (our reading of the scorers)

- `prefix-cache-scorer` hashes 64-token blocks chained from the first token and
  scores an endpoint by the fraction of the request's blocks it has recorded
  for that endpoint. A request whose opening was seen before scores ≈ 0.75–1.0
  on the endpoint that served it and 0 on every other. Weight 3 → a 2.25–3.0
  point lead.
- `queue-scorer` scores on the engine's **waiting** queue, normalised across
  endpoints. vLLM admits up to `max-num-seq` before anything waits, so a
  replica running 29–32 sequences has a waiting queue of 0–1 and scores the
  same as an idle one. The scorer that should have pushed back does not see
  the saturation.
- `kv-cache-utilization-scorer` does see it, but at weight 2 its maximum lead
  (utilisation difference × 2 ≈ 1.2) is smaller than the prefix lead.
- The prefix index is LRU with a large per-endpoint capacity; within a run
  nothing the first endpoint recorded ages out, and an endpoint that receives
  no requests records nothing. The lock-in has no exit.
- `no-hit-lru-scorer` only acts on requests with **no** hit anywhere. With 2048
  prompts cycled, every request after the first pass has a hit.

## Reproduction

Any llm-d install with the shipped profile and one model at one replica.

1. Load with inference-perf's `shared_prefix` dataset at a rate that needs a
   second replica — here 9 rps of 1000/500-token requests against one H200 at
   `--max-num-seq 32`:

   ```yaml
   load:
     type: poisson
     interval: 0
     stages:
     - rate: 9
       duration: 180
   api: {type: completion, streaming: true}
   server: {type: vllm, model_name: <model>, base_url: http://<gateway>/<route>, ignore_eos: true}
   tokenizer: {pretrained_model_name_or_path: <model>}
   data:
     type: shared_prefix
     shared_prefix: {num_groups: 32, num_prompts_per_group: 64, system_prompt_len: 750, question_len: 250, output_len: 500}
   ```

2. Add a second replica (scale the Deployment, or let the autoscaler do it) and
   wait for it to be Ready — it appears in the EPP's endpoint set within a
   second of Ready.
3. Read `vllm:prompt_tokens_total` on both engines before and after 3 minutes
   of load. Expected: ≈ 50/50. Observed: ≈ 99/1.
4. Repeat with `data: {type: synthetic, input_distribution: {min: 1000, max: 1000, mean: 1000, std_dev: 0, total_count: 4000}, output_distribution: {min: 500, max: 500, mean: 500, std_dev: 0, total_count: 4000}}`.
   Observed: 50.6 / 49.4.

Step 3 is the whole test: if the second replica is taking traffic, the pin is
not reproduced. The full two-model, anti-phase version of this measurement,
with the guard that refuses a run where one engine did a model's work, is
`hack/benchmark/two_model_pool.sh` in this repository.

## What we think a fix looks like

Any one of these breaks the lock-in; we have not measured which is best:

- **Saturation-aware prefix scoring.** Discount or zero the prefix score for an
  endpoint whose running-request count (not just its waiting queue) is at or
  near `max-num-seq`. A prefix hit on an engine that cannot admit the request
  is not worth the wait behind it.
- **Make the queue scorer see running requests**, e.g. score on
  `running + waiting` against the engine's admission limit, so a full engine
  loses to an idle one even with an empty waiting queue.
- **Bootstrap for new endpoints.** Give an endpoint with an empty prefix index
  a share of no-hit-or-low-hit traffic until it has recorded something — the
  `no-hit-lru-scorer` almost does this, but only for requests with no hit
  anywhere.
- At minimum, **document** that the shipped weights assume prefix diversity,
  and that with recurring prefixes a scale-up will not receive traffic.

## Cost of the current behaviour

For an autoscaler this is total: every replica it adds is paid for and serves
nothing, and the model's TTFT during a burst is that of a one-replica fleet no
matter what the fleet is. The failure is silent — the fleet is Ready, the EPP
lists every endpoint, and the aggregate metrics look spread.
