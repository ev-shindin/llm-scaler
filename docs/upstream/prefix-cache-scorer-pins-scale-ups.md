# Bug report draft for llm-d-inference-scheduler

Filed against: `llm-d/llm-d-inference-scheduler` (the shipped scheduling
profile). The mechanism lives in GIE's `prefix-cache-scorer` and
`queue-scorer`, so a cross-reference to
`kubernetes-sigs/gateway-api-inference-extension` is appropriate.

---

## Title

With the shipped scheduling profile, a newly added replica receives no traffic
when prompts share prefixes — the prefix-cache scorer pins the pool to the
first replica regardless of its load

## Environment

- EPP: `ghcr.io/llm-d/llm-d-router-endpoint-picker:v0.9.0` (llm-d-inference-scheduler v0.9.0, gateway-api-inference-extension v1.5.0)
- Scheduling profile: the shipped `optimized-baseline` profile, unchanged:

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

- Engine: vLLM v0.26.0, `--max-num-seq 32`, block size 64, one H200 per replica
- Model: `unsloth/Meta-Llama-3.1-8B-Instruct` (also reproduced with `Qwen/Qwen3-8B`)
- Gateway: istio, one InferencePool, one EPP
- Load generator: inference-perf v0.6.1

## Steps to reproduce

1. Deploy one model at **one** replica behind the EPP with the profile above.
2. Start an open-loop load whose prompts share prefixes. This is inference-perf's
   own `shared_prefix` dataset at the values its shipped profiles use; 1000
   input / 500 output tokens at 9 rps is enough to fill one replica's 32
   sequence slots:

   ```yaml
   load:
     type: poisson
     interval: 0
     stages:
     - rate: 9
       duration: 300
   api:
     type: completion
     streaming: true
   server:
     type: vllm
     model_name: unsloth/Meta-Llama-3.1-8B-Instruct
     base_url: http://<gateway>
     ignore_eos: true
   tokenizer:
     pretrained_model_name_or_path: unsloth/Meta-Llama-3.1-8B-Instruct
   data:
     type: shared_prefix
     shared_prefix:
       num_groups: 32
       num_prompts_per_group: 64
       system_prompt_len: 750
       question_len: 250
       output_len: 500
   ```

3. After ~60 s, scale the Deployment to **two** replicas and wait for the new
   Pod to be Ready. It appears in the EPP's endpoint set within about a second
   of Ready (`inference_pool_ready_pods` goes 1 → 2).
4. Read `vllm:prompt_tokens_total` from both engines' `/metrics` at that
   moment and again 3 minutes later. Compare the deltas.

## Expected

The second replica takes a substantial share of the traffic — the first is at
29–32 of its 32 sequence slots, the second is empty.

## Actual

The second replica serves **≈1 %** of the prompt tokens; the first keeps
**≈99 %**, still at 29–32 running sequences, while the second sits idle.

Measured over a 38-minute run in which an autoscaler took the model from one
replica to three and back twice: the first engine served **99.0 %** of the
model's prompt tokens; each replica the autoscaler added served between 0.3 %
and 2.0 %. With `num_groups: 1` (a single shared prefix) the added replicas
served **exactly 0** tokens against 6,000,692 on the first, while Ready and
listed in the EPP's backends.

Control: the same steps with a dataset that shares no prefixes (inference-perf
`synthetic`, which slices each prompt from a random offset into a corpus):

```yaml
data:
  type: synthetic
  input_distribution:  {min: 1000, max: 1000, mean: 1000, std_dev: 0, total_count: 4000}
  output_distribution: {min: 500,  max: 500,  mean: 500,  std_dev: 0, total_count: 4000}
```

An empty second replica took **49.4 %** of the load from its first minute
(50.6 / 49.4 over 3 minutes, TTFT p50 58 ms). So endpoint discovery, readiness
and the data path are fine; the routing decision is what changes.

Two details worth stating:

- The engines ran with `--no-enable-prefix-caching`. The EPP's prefix index is
  its own estimate, not informed by the engine, so the preference cost the
  whole scale-up and bought nothing (`vllm:prompt_tokens_by_source_total{source="local_cache_hit"}` stayed at 0).
- With two InferencePools behind one gateway, the aggregate across pools looked
  spread (the busiest engine held 49 % of all tokens) while each pool was pinned
  to one engine. The symptom is per pool, and easy to miss in fleet-wide
  metrics.

## Analysis

As far as we can read the plugins:

- `prefix-cache-scorer` hashes 64-token blocks chained from the first token and
  scores an endpoint by the fraction of the request's blocks it has recorded
  for that endpoint. A prompt whose opening was seen before scores ≈ 0.75–1.0
  on the endpoint that served it and 0 everywhere else — a 2.25–3.0 point lead
  at weight 3.
- `queue-scorer` scores on the engine's **waiting** queue. vLLM admits up to
  `--max-num-seq` sequences before anything waits, so a replica running 29–32
  sequences reports a waiting queue of 0–1 and scores the same as an idle one.
  The scorer that should push back does not see the saturation.
- `kv-cache-utilization-scorer` does see it, but at weight 2 its largest
  possible lead (utilisation difference × 2, ≈ 1.2 here) is below the prefix
  lead.
- The prefix index is LRU with a large per-endpoint capacity. Within a run
  nothing the first endpoint recorded ages out, and an endpoint that receives
  no requests records nothing, so the lock-in has no exit.
- `no-hit-lru-scorer` only acts on requests with no hit on **any** endpoint.
  With 2048 distinct prompts cycled, every request after the first pass has a
  hit somewhere.

The result is self-reinforcing: the endpoint that served a prefix first wins
every later request carrying it, whatever its load; the new endpoint never
receives one, so never records one, so never starts winning.

## Impact

Any autoscaled deployment whose traffic has recurring prefixes (system prompts,
few-shot templates, multi-turn) gets no benefit from a scale-up under the
default profile: every replica added is paid for and idle, and latency under a
burst is that of a one-replica pool however large the fleet. The failure is
silent — the Pods are Ready, the EPP lists every endpoint, and fleet-wide
metrics look spread.

## Suggested fix

Any of these breaks the lock-in; we have not measured which is best:

- **Saturation-aware prefix scoring** — discount or zero the prefix score for an
  endpoint whose running-request count (not only its waiting queue) is at or
  near `max-num-seq`. A prefix hit on an engine that cannot admit the request
  is not worth the wait behind it.
- **A queue scorer that sees running requests** — score on running + waiting
  against the engine's admission limit, so a full engine loses to an idle one
  even with an empty waiting queue.
- **A bootstrap share for new endpoints** — route a fraction of low-hit traffic
  to an endpoint with an empty prefix index until it has recorded something.
  `no-hit-lru-scorer` is close to this but triggers only on requests with no
  hit anywhere.
- At minimum, **document** that the shipped weights assume prefix diversity
  across requests, and that with recurring prefixes an added replica will not
  receive traffic.
