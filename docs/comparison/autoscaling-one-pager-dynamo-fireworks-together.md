# LLM Autoscaling Landscape — Dynamo · Fireworks · Together

> One-page summary · verified **2026-08-09** · WVA shown as baseline · full detail: [autoscaling-nvidia-mooncake-sglang-vs-wva.md](./autoscaling-nvidia-mooncake-sglang-vs-wva.md)

**Two categories, not three competitors.** NVIDIA **Dynamo** is deployable software and WVA's only true peer. **Fireworks** and **Together** are managed SaaS: they scale *one endpoint you rent*, and the fleet problem WVA solves is the provider's, structurally invisible through their APIs. Compare Dynamo on scope; compare the SaaS pair on control law and signal choice.

## Capability matrix

| | **WVA** (baseline) | **NVIDIA Dynamo Planner** | **Fireworks AI** | **Together AI** |
|---|---|---|---|---|
| **Category** | Deployable OSS (K8s controller) | Deployable OSS | Managed SaaS | Managed SaaS |
| **Control law** | Saturation optimizer; weighted multi-analyzer ballot | SLA perf-model → replicas | Threshold per target; **max across targets wins** | Proportional `ceil(N × observed/target)` |
| **Signals** | Queue depth, KV util, request rate, token supply/demand | Prometheus rate + ISL/OSL + KV-hit; event-plane ForwardPassMetrics | 5 per-replica rates: `default` (0–1), `tokens_generated_per_second`, `prompt_tokens_per_second`, `requests_per_second`, `concurrent_requests` | 8, **exactly one at a time**: `inflight_requests` (default), `ttft`, `e2e_latency`, `gpu_utilization`, `token_utilization` (= KV-cache), `throughput_per_replica`, `decoding_speed`, `cache_hit_rate` |
| **Forecasting** | **None** (reactive) | **Yes** — Constant / **ARIMA** / Kalman / Prophet, 1-interval look-ahead | None | None |
| **SLA → capacity** | Learns online from metrics; no profiling step | Interpolated perf model, **required** for the SLA planner. **No traces needed** — you pass target ISL/OSL + TTFT/ITL. Default **AIConfigurator: 20–30 s, no GPU**; opt-in **AIPerf: 2–4 h, GPU**. Load-based mode needs no profiling but is `enable_load_scaling=False` by default | None | Latency metrics only (`p50–p99`, default `p95`) — **and they don't work, see below** |
| **Multi-model / global** | **Yes** — multi-tenant, global fair-share | No — single DGD | No — per endpoint | No — per endpoint |
| **Heterogeneous GPUs** | **Yes**, first-class | No — one GPU type | Not exposed (one SKU/deployment) | Not exposed (one `hardware` string) |
| **Cost aware** | **Yes** — relative `variant-cost` + **GPU quota/fair-share limiter** | No — cost-blind, only `max_gpu_budget=8` | Per-GPU-second billing; `max` is the only ceiling | `max_replicas` is the only ceiling |
| **P/D disaggregation** | Roles first-class, role-aware demand | **Independent P/D scaling** (core feature) | Not exposed | Not exposed |
| **Scale to zero** | Reactive scale-from-zero engine | Not a focus | **Headline feature** — idle window default **1 h** (min 5 m) | **No auto-wake** — `min=max=0` is a *stopped* state; `inactive_timeout` auto-stops |
| **Up / down windows** | HPA/KEDA stabilization | 180 s predictive · 5 s reactive (off by default) | **30 s** / **10 m** | **0 s** / **5 m** |
| **Actuation** | Metrics → HPA/KEDA (opt-in annotation) | Own controller patches DGD CRD; **VirtualConnector** publishes decisions without managing infra | Platform-internal | Platform-internal |
| **Scale-down drain** | Handled by HPA/KEDA + scale target | **Graceful** — prefill SIGTERM finishes current request; decode etcd-lease revoke, finishes stream | Not documented | Stabilization window only |
| **Currency** | Active | **v1.3.1 (6 Aug 2026)** | Static since **5 Feb 2026** | **DMI launched 16 Jul 2026** — iterating weekly |

## Verdicts

**Dynamo — the real peer, and the only one with prediction.** Forecasting plus a quantitative SLA→capacity model, productized independent P/D scaling, self-contained operator. **Scale-down drains gracefully**: prefill workers get a SIGTERM and exit after finishing the current queued request ("no remote prefill request is dropped"); decode workers have their etcd lease revoked, leave the router, and "finish all the current requests in their original stream and exit gracefully." Pays for its strengths with scope: single-model, homogeneous GPUs, cost-blind, and pre-deployment profiling required. Its 2026 roadmap (Global Planner, Grove — #9178) targets exactly WVA's turf, so the edge is real today but contested.

**Fireworks — simplest policy, best cost UX.** A clean threshold autoscaler over LLM-native rates whose real product is **scale-to-zero with a tunable idle window**. No latency scaling, no forecasting. Cold requests get an immediate `503 DEPLOYMENT_SCALING_UP`, deliberately not queued.

**Together — richest metric menu, and the most honest engineering.** Proportional control, one metric per deployment, eight to choose from. Notably ships KV-cache utilization (`token_utilization`) — the same signal WVA leans on.

## The one finding worth acting on

Together ships `ttft` p95, then **published that it doesn't work**. In their own experiment, `ttft` *and* `gpu_utilization` both **failed to trigger scale-up** while `inflight_requests` succeeded:

- **TTFT** stays low under saturation — continuous batching absorbs queue pressure into *end-to-end* latency, so the metric looks healthy exactly when you need to scale.
- **GPU utilization** misleads — *"utilization measures arithmetic intensity, not pressure"*; a GPU reads 60% busy while the queue backs up.

A competitor's published negative result validating WVA's signal choice: **scale on saturation/queue, not latency or GPU busy-ness.** Cite it when challenged with "why not just target TTFT?" or "why not DCGM utilization?". It argues against a naive TTFT analyzer in the ballot.

**Published cold-start figures** (Together, 1×H100 — rare public data, useful for scale-from-zero calibration): base model → READY **86 s**; fine-tune (18 GB) → READY **145 s**; READY → first token **+26–40 s**; scale-up 1→2 **≈2.5 min**. This ~2-minute floor is why both SaaS vendors set scale-down windows of 5–10 minutes.

## Implications for WVA

1. **Forecasting is the one real gap.** Dynamo has it; neither SaaS platform does. It stays the clearest borrow — and the seam the Cluster Capacity Planner fills at a higher altitude.
2. **Don't oversell "no profiling."** Dynamo's default profiling path is a **20–30 s offline calculation with no GPU and no traces** — too cheap to call a burden. The defensible version of the claim is that Dynamo needs that step **per model × per GPU type before the SLA planner runs at all**, which compounds across a heterogeneous multi-model fleet; WVA has no such bootstrap. Argue the compounding, not the one-off cost.
3. **The structural moat holds.** Multi-model global scope, heterogeneous accelerators, cost-awareness, and GPU fair-share are absent from *all three* — and for the SaaS pair they are unreachable by construction, not merely unshipped.
4. **Signal choice is now externally validated** — treat Together's negative result as evidence, not opinion.
5. **Ergonomics are the honest weakness.** Both SaaS platforms expose scaling as a few named knobs with sane defaults; WVA exposes a ConfigMap of analyzers and thresholds. More powerful, much less approachable.

---

*Sources — primary docs only.* Dynamo: [planner design](https://docs.nvidia.com/dynamo/design-docs/component-design/planner-design.md) · [profiler](https://docs.nvidia.com/dynamo/components/profiler.md) · [releases](https://github.com/ai-dynamo/dynamo/releases). Fireworks: [autoscaling](https://docs.fireworks.ai/deployments/autoscaling) · [billing & scaling](https://docs.fireworks.ai/faq/deployment/ondemand/billing-scaling) · [changelog](https://docs.fireworks.ai/updates/changelog). Together: [scaling reference](https://docs.together.ai/docs/dedicated-endpoints/scaling) · [engineering write-up](https://www.together.ai/blog/autoscaling-endpoints-for-llm-inference) · [changelog](https://docs.together.ai/docs/changelog).

*Caveat:* Together's control law, negative result, and cold-start figures are **vendor self-reported**, not independently reproduced. A secondary-source claim that Together scales on arbitrary Prometheus metrics is **contradicted by its primary docs** and excluded.
