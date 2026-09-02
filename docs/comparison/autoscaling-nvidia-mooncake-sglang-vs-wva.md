# Autoscaling Landscape vs. WVA — NVIDIA Dynamo, Mooncake, SGLang, Fireworks AI, Together AI

> **Status:** Research comparison (untracked working doc) · **Date:** 2026-08-03 · **Managed providers added:** 2026-08-09
> **Baseline:** llm-d Workload Variant Autoscaler (**WVA**)
> **Compared — deployable systems:** NVIDIA Dynamo Planner, Mooncake, SGLang · **managed platforms:** Fireworks AI, Together AI
> All claims are sourced against latest docs/code — see [Sources](#sources).

---

## TL;DR — one peer autoscaler, and two managed platforms that set the customer-facing bar

| System | Is it an autoscaler? | One-line posture |
|---|---|---|
| **WVA** (baseline) | **Yes** | Global, SLO/cost-aware, **heterogeneous, multi-model** controller; emits desired replicas → HPA/KEDA. |
| **NVIDIA Dynamo Planner** | **Yes** | SLA-driven controller with **traffic forecasting** + **pre-deployment profiling**; scales prefill/decode independently; **single-model, homogeneous, cost-blind**. |
| **Mooncake** | **No** | KVCache-aware scheduler + **early-rejection admission control** over a *fixed* pool. Sheds load; does not add capacity. Scheduling "brain" is **not open-source**. |
| **SGLang** | **No (native)** | Engine + **router/gateway** (membership + cache-aware LB). Scaling delegated entirely to **external K8s** (HPA/KEDA, OME/Knative/LWS). |
| **Fireworks AI** | **Yes, but** | **Managed SaaS.** Per-endpoint threshold autoscaler on LLM-native rates; **scale-to-zero is the headline feature**. No SLO model, no forecasting, not deployable. |
| **Together AI** | **Yes, but** | **Managed SaaS.** Per-endpoint **proportional** autoscaler, `ceil(N × observed/target)`, **one metric only**; richest metric menu of anyone here (8, incl. TTFT/e2e p-tiles). No scale-to-zero-with-wake. |

**The only true head-to-head is still WVA vs. Dynamo.** Mooncake and SGLang occupy different boxes: Mooncake *rejects* requests when overloaded; SGLang *routes* over whatever an external autoscaler provisions.

**Fireworks and Together are a different category again — and the distinction matters.** They are *not* alternatives to WVA: you cannot run them on your cluster, and the hard part WVA solves (packing many models × variants onto a heterogeneous, quota-constrained GPU fleet) is the *provider's* internal problem, invisible to and unsolvable by the customer. What they are is the **competitive bar for what a customer experiences** — and, more usefully, a **published record of what a well-resourced team chose to ship** after operating LLM autoscaling at scale.

> *When overloaded: WVA and Dynamo **add capacity**; Mooncake **sheds load**; SGLang **defers to Kubernetes**; Fireworks and Together **add replicas to one endpoint** and bill you for them.*

**2026 currency (verified 2026-08-03).** All three verdicts hold on current-year releases. **Dynamo** is under heavy 2026 development (**GA v1.0 Mar 2026**; latest **v1.3.1, 6 Aug 2026**) — the one shipped change of note is that **AIConfigurator (AIC) latency prediction replaced the hand-tuned cost model**, plus a plugin-pipeline refactor and MTP/observability plumbing; its single-DGD / homogeneous / cost-blind / average-ISL-OSL character is **unchanged** (cost, heterogeneity, multi-DGD, and distribution-aware planning are all roadmap — #9178 Global Planner, Grove — **not yet shipped**). **SGLang** (v0.5.16, Jul 2026; router 0.3.2, Jan 2026) and **Mooncake** (v0.3.12, Jul 2026) added **no** scaling control loop in 2026 — posture unchanged.

---

## At-a-glance comparison

| Dimension | **WVA** | **Dynamo Planner** | **Mooncake** | **SGLang** |
|---|---|---|---|---|
| **Type** | Global SLO/cost autoscaler | SLA-driven autoscaler (2 modes) | Scheduler + admission control | Engine + router (no autoscaler) |
| **What it scales** | Replicas per model×variant | Prefill/decode replicas (independent) | Nothing (P:D ratio **preset**) | Nothing native (external scales replicas) |
| **Scope** | **Multi-model, multi-tenant, global** | **Single DGD** (GlobalPlanner = same model only) | Single cluster, fixed | Per-deployment (external) |
| **Signals** | Prometheus: queue depth, KV util, request rate, token supply/demand | Prometheus (rate, ISL/OSL, KV-hit) **+** event-plane ForwardPassMetrics | KV reuse, P/D load, TTFT/TBT, predicted decode load | Engine Prometheus (`num_queue_reqs`, `token_usage`, TTFT/TPOT) → external |
| **Forecasting** | **None** (reactive; EWMA/rate-anchored smoothing only) | **Yes** — Constant/ARIMA(default)/Kalman/Prophet, 1-interval look-ahead | For **admission** only (predict decode load to reject early), not capacity | None |
| **SLA→capacity** | Saturation optimizer; greedy-by-saturation; learns online, **no profiling needed** | Interpolated perf model from **pre-deployment profiling** (AIC rapid / AIPerf thorough) | No (SLA → which requests to admit/reject) | None (thresholds live in external KEDA rules) |
| **Cost / heterogeneity** | **Yes** — heterogeneous accelerators first-class; cost-aware optimizer (relative `variant-cost`); **GPU quota/fair-share limiter** | **No** — homogeneous single GPU type, cost-blind; only a GPU-count cap (`max_gpu_budget=8`) | No (reuses idle CPU/DRAM/SSD on fixed HW) | No (only heterogeneous-TP KV transfer, a data-plane opt) |
| **Actuation** | **Metrics-first** → Prometheus Adapter → **HPA/KEDA** → `scale` (opt-in `llm-d.ai/managed`); optional direct scale for scale-from-zero | **Own controller** patches DGD CRD via operator (`DynamoGraphDeploymentScalingAdapter`); **VirtualConnector** publishes decisions without managing infra (suggest-only) | Actuates on **requests** (route/reject/replicate KV); no fleet actuation | Dynamic worker register/deregister + K8s service discovery (membership, not scaling) |
| **P/D disaggregation** | Roles first-class (`llm-d.ai/role`), role-aware demand | **Independent prefill/decode scaling** (core feature) | Separate P/D pools, **preset ratio** | Separate pools, **manual** scaling |
| **Maturity** | Incubation/active (VA-CR → annotation migration); paper published | GA v1.0 (Mar 2026, unverified), v1.3.1 (6 Aug 2026); both modes production-ready | Production at Kimi; **Conductor + rejection policy NOT open-sourced** | Engine mature; router shipped but mid-rewrite; PD scaling manual |
| **Openness** | Open (Apache-2, llm-d) | Open (ai-dynamo) | **Partial** — transport/store open; scheduler internal | Open (sgl-project) |

---

## Per-system detail

### NVIDIA Dynamo Planner — the real peer

Dynamo offers three ways to scale a `DynamoGraphDeployment`: generic **HPA**, **KEDA**, and the LLM-aware **Planner** (all write through a common `DynamoGraphDeploymentScalingAdapter`). The Planner has **two modes** that can run together:

- **Throughput-based (predictive)** — forecasts next-interval request rate + ISL/OSL, consults an engine performance model, and computes replicas to hit **TTFT/ITL** SLA targets. Cadence **180 s**. Establishes a **minimum replica floor**.
- **Load-based (reactive)** — uses event-plane **ForwardPassMetrics** (queue depth, KV utilization) for ±1-replica bursts. Cadence **5 s**. Off by default (`enable_load_scaling=false`). Can scale *above* but not *below* the throughput floor.

**Forecasting:** four pluggable predictors — Constant, **ARIMA (default)**, Kalman (local-linear-trend), Prophet. Look-ahead = one adjustment interval.

**SLA→capacity:** an interpolated perf model mapping `(throughput, ISL/OSL, context) → (TTFT, ITL)`, bootstrapped from **pre-deployment profiling** — *Rapid* mode via AI Configurator (offline, ~30 s, no GPU) or *Thorough* mode via AIPerf (online, 2–4 h, GPU). Profiler also sweeps tensor-parallelism configs.

**Actuation:** its own controller patches the DGD replica counts (`update_graph_replicas`) via the Kubernetes operator — **not** HPA. A **VirtualConnector** can publish scaling decisions without managing the deployment infrastructure (suggest-only).

**Where WVA leads:** multi-model/global scope (Dynamo is **single-DGD**; even GlobalPlanner requires all pools serve the *same* model), **heterogeneous GPU types** (Dynamo assumes one `system` e.g. `h200_sxm`), **cost-awareness + GPU fair-share/quota** (Dynamo is cost-blind with only a GPU-count cap), and **no mandatory pre-deployment profiling** (WVA learns online from metrics + deployment args).

**Where Dynamo leads:** **traffic forecasting** (WVA is reactive — this is WVA's biggest gap), a **quantitative SLA→capacity perf model** with profiling, **productized independent prefill/decode scaling**, and a **self-contained operator** (vs WVA's dependence on HPA/KEDA plumbing).

**Shared limitation:** both assume **average ISL/OSL** — neither is distribution-aware (bimodal interactive+batch).

> **Correction (2026-08-09):** an earlier revision of this doc stated that Dynamo *"terminates in-flight requests on scale-down (no graceful drain)."* **That is wrong.** Dynamo drains gracefully on both roles: to scale down a **prefill** worker the planner sends **SIGTERM**, which the worker stores and acts on only after finishing the request already pulled from the queue, so *"no remote prefill request is dropped"*; to scale down a **decode** worker it **revokes the worker's etcd lease**, which immediately removes it from the router so it receives no new work, after which it *"finishes all the current requests in their original stream and exits gracefully."* Graceful drain is therefore a Dynamo **strength**, not a gap — see [sla_planner.md](https://github.com/ai-dynamo/dynamo/blob/main/docs/planner/sla_planner.md).

**2026 status:** GA in **March 2026**; latest **v1.3.1 (6 Aug 2026)**, actively developed (planner commits through late Jul 2026). The notable 2026 *shipped* change is that **AIConfigurator (AIC) latency prediction replaced the hand-tuned cost model** — the planner now sizes on AIC-modeled serving cost — alongside a plugin-pipeline refactor (OBSERVE→PREDICT→PROPOSE→RECONCILE→CONSTRAIN→EXECUTE), MTP/speculative accept-length correction, and SLA-target Grafana dashboards. Crucially, the areas where **WVA leads — cost-awareness, heterogeneous GPUs, multi-DGD/multi-cluster, distribution-aware planning — remain roadmap, not shipped** (the planned **Global Planner** + **Grove** topology-aware scheduler target exactly this turf; roadmap #9178). So WVA's structural edges are real *today* but are explicitly in Dynamo's crosshairs — a moat to defend, not assume.

### Mooncake — not an autoscaler (load-shedding scheduler)

Mooncake explicitly **rules out elastic scaling** ("elastically scaling out the inference cluster is typically unfeasible"), presets the prefill:decode instance ratio, and instead makes **overload-oriented scheduling** its core contribution: a **prediction-based early-rejection** policy that forecasts post-prefill decode load and rejects requests *before* wasting prefill compute if they'd miss SLO. The **Conductor** global scheduler does KVCache-aware routing, cache replication/swap, and P/D assignment — all **request-level**, never fleet-level.

**Contrast with WVA:** opposite response to overload — Mooncake **sheds**, WVA **adds**. Also: Mooncake's scheduler ("brain") and rejection policy are **internal to Kimi, not open-sourced**; only the transfer engine and KV store are public. There is **no operator/HPA/controller** anywhere in the paper or repo. SLO-awareness is strong (goodput under TTFT/TBT); cost/heterogeneity awareness is absent.

**2026:** unchanged — 2026 releases (v0.3.12, Jul 2026) are storage/transfer-engine only; the new `mooncake-ep`/`mooncake-pg` "elastic" refers to *fault-tolerant expert parallelism*, not autoscaling; Conductor remains closed-source. (The "elastic/scalable Mooncake" cloud recipes get elasticity from **RBG**, a separate K8s orchestrator, with Mooncake as just the KV data plane.)

### SGLang — not a native autoscaler (engine + router)

SGLang ships an **engine** (mature) and a **router / "Model Gateway"** (cache-aware L7 load balancer with dynamic worker membership). The router **register/deregisters workers** (`/workers` API + K8s service discovery) and load-balances (`cache_aware` default, `power_of_two`, `bucket`) — but **creates/destroys no replicas**. All closed-loop scaling is **external**: HPA/KEDA read SGLang's Prometheus metrics (`num_queue_reqs`, `num_running_reqs`, `token_usage`, TTFT/TPOT), and ecosystem stacks (OME → KEDA/Knative/LWS/Kueue) supply scale-to-zero and PD-aware scaling. PD disaggregation scaling is documented as **manual**.

**Contrast with WVA:** SGLang is a superb *target* for an autoscaler but contributes none of the metric→decision→replica-count control loop. WVA (or Dynamo) is exactly the missing brain; in fact SGLang is a supported Dynamo backend.

**2026:** unchanged — releases through **v0.5.16 (Jul 2026)** and the 2026 roadmaps (#12780, #13098, #21703) add *routing* intelligence (semantic/SLO/session/KV-event-aware routing) and explicitly delegate elasticity to **OME/Kubernetes** ("Auto scaling in OME"); the "Autonomous Model Gateway" is routing automation, not replica scaling.

---

## Managed platforms — Fireworks AI and Together AI

Both sell **dedicated/on-demand endpoints**: you pick a model and a GPU SKU, set replica bounds and a scaling policy, and the platform scales *your endpoint* between those bounds. Neither is deployable software, and neither exposes the multi-model fleet problem — the provider absorbs that. Compare them on **control law and signal choice**, not on scope.

| Dimension | **Fireworks AI** | **Together AI** | **WVA** (for reference) |
|---|---|---|---|
| **Control law** | Threshold per load target; with several targets, **max replica count across all** wins | **Proportional**: `ceil(N × observed/target)`, dampened by windows, clamped to bounds | Saturation optimizer; multi-analyzer ballot with per-analyzer scores |
| **Signals** | `default` (0–1 load fraction), `tokens_generated_per_second`, `prompt_tokens_per_second` (added Feb 2026, prefill-heavy), `requests_per_second`, `concurrent_requests` — all **per replica** | Exactly 8: `inflight_requests` (default), `ttft`, `e2e_latency`, `gpu_utilization`, **`token_utilization` (= KV-cache utilization, 0–100)**, `throughput_per_replica`, `decoding_speed`, `cache_hit_rate` (prompt-cache, 0–100). No custom/Prometheus metric support | Queue depth, KV utilization, request rate; token supply/demand |
| **Multi-signal** | **Yes** — several targets, max wins | **No** — *"A deployment scales on a single metric, so you set exactly one"* | **Yes** — weighted combine across analyzers |
| **Latency/SLO scaling** | **None** | `ttft` / `e2e_latency` with `p50/p90/p95/p99` (default `p95`) — **but see below** | Saturation-derived, not latency-triggered |
| **Forecasting** | None | None | None |
| **Scale-up window** | `--scale-up-window`, default **30 s** | `--scale-up-window`, default **0 s** (blog recommends 60 s) | HPA/KEDA stabilization |
| **Scale-down window** | `--scale-down-window`, default **10 m** | `--scale-down-window`, default **5 m** | HPA/KEDA stabilization |
| **Scale to zero** | **Yes, headline feature** — `--scale-to-zero-window` default **1 h** (min 5 m), `--min-replica-count` default **0** | **No auto-wake.** `min=max=0` is an explicit *stopped* state; `inactive_timeout` (minutes) auto-stops | Reactive scale-from-zero engine |
| **Cold-start UX** | **503 `DEPLOYMENT_SCALING_UP`, not queued** — client must retry with backoff | Requests to a stopped endpoint error; restart is manual (1–2 min) | — |
| **Heterogeneous GPUs** | Not exposed (pick one SKU per deployment; base quota 8×A100 / 8×H100) | Not exposed (one `hardware` string per endpoint) | **First-class** |
| **P/D disaggregation** | Not exposed | Not exposed | Roles first-class |
| **Cost model** | Per **GPU-second** while replicas are up — *"costs accumulate even if there are no active API calls"* | Per replica-time; `max_replicas` is the cost ceiling | Relative `variant-cost` + quota limiter |

### The finding worth stealing: Together's own data says both TTFT *and* GPU utilization are bad scaling signals

Together ships `ttft` p95 — the most SLO-shaped knob any vendor here offers — and then their **own** engineering write-up reports that it *did not work*. In their published experiment, policies based on **`ttft` and `gpu_utilization` both failed to scale**, while `inflight_requests` correctly added replicas and cut latency spikes. Two distinct causes:

- **TTFT** stays low under saturation because **continuous batching absorbs queue pressure into end-to-end latency**, not first-token latency. The metric looks healthy precisely when you most need to scale.
- **GPU utilization** misleads because *"utilization measures arithmetic intensity, not pressure"* — a GPU can read 60% busy while the engine's queue is already backing up.

This is independent, production-sourced confirmation of the choice WVA already made: **scale on saturation/queue depth, not on observed latency or GPU busy-ness.** Worth citing directly whenever WVA is challenged with "why not just target TTFT?" or "why not just use DCGM utilization?" — the answer is no longer theoretical, it is a competitor's own published negative result. It argues *against* adding a naive TTFT-target analyzer to the multi-analyzer ballot, and *for* keeping the saturation signal primary.

Note the convergence: Together's default (`inflight_requests`) and its `token_utilization` (KV-cache utilization) are the same two signals WVA leans on. Two teams reached the same conclusion independently.

### Published cold-start numbers (useful calibration for scale-from-zero)

Together published measured figures on 1×H100 — rare, and directly relevant to WVA's scale-from-zero and stabilization work:

| Event | Time |
|---|---|
| Base model (Qwen3.5-9B) → replica READY | **86 s** |
| Custom fine-tune (18 GB weights) → READY | **145 s** |
| READY → first token served | **+26–40 s** |
| Scale-up 1 → 2 replicas (end to end) | **~2.5 min** |
| Restart from STOPPED | **1–2 min** |

Two consequences. First, a **~2-minute** provisioning latency is the real-world floor, which is why Together defaults `scale_down_window` to 5 minutes and Fireworks to 10 — both are protecting against paying that cost twice, exactly the concern behind WVA's stabilization work. Second, it explains why **Fireworks answers cold requests with a 503 rather than queueing them**: holding a connection for 2 minutes is worse than failing fast and letting the client retry. WVA's scale-from-zero path should assume the same order of magnitude.

**2026 currency (verified 2026-08-09).** Both surfaces are *new*, so treat them as moving targets.

- **Together's Dedicated Model Inference (DMI) launched 16 Jul 2026** — the entire autoscaling surface described here is roughly three weeks old, and the changelog is still landing changes weekly: **23 Jul** added `worker_engine_kv_cache_utilization` and `worker_engine_cache_hit_rate` Prometheus gauges (observability, distinct from the scaling metrics); **28 Jul** added CLI replica-bound inference (`--min-replicas` mirrors to max). Expect this to keep moving.
- **Fireworks has been essentially static since 5 Feb 2026**, the single 2026 changelog entry touching scaling: it added the `prompt_tokens_per_second` load target for prefill-heavy workloads and changed scaled-to-zero deployments to return `503` immediately with retry guidance rather than attempting to initialize.
- One claim circulating in secondary coverage — that Together can autoscale on **any custom Prometheus metric** from a worker's `/metrics` endpoint — is **not supported by the official scaling docs**, which enumerate exactly eight built-in metrics and no custom-metric parameter. Excluded here pending a primary source.

### What this changes for WVA — and what it doesn't

- **Doesn't change the moat.** Neither platform does multi-model global allocation, heterogeneous accelerators, cost-aware placement, quota/fair-share, or P/D scaling. Every one of those is *structurally absent* from a per-endpoint API, not merely unshipped — the customer never sees the fleet.
- **Doesn't close the forecasting gap.** Both are purely reactive. Dynamo remains the only system here with prediction, so that gap in WVA is unchanged.
- **Does set a UX bar WVA should not ignore.** Scale-to-zero with a tunable idle window is a *headline* commercial feature at Fireworks, and both expose scaling policy as a handful of named knobs with sane defaults. WVA's equivalent surface is a ConfigMap of analyzers and thresholds — more powerful, considerably less approachable.
- **Does validate the signal choice** (see TTFT above), which is the single most useful takeaway here.

---

## WVA vs. Dynamo — the head-to-head that matters

| | **WVA advantage** | **Dynamo advantage** |
|---|---|---|
| Scope | Multi-model, multi-tenant, global fair-share | — |
| Hardware | Heterogeneous accelerators, cost-aware | — |
| Governance | GPU **quota/limiter** across namespaces | — |
| Deployment | No pre-deployment profiling; composes with existing HPA/KEDA | Self-contained operator |
| Prediction | — | **Traffic forecasting** (ARIMA/Kalman/Prophet) |
| SLA model | — | Quantitative perf model + profiling |
| P/D | Role-aware demand | **Productized independent P/D scaling** |
| Both weak on | — | **Distribution awareness** (avg ISL/OSL); real-$ cost is relative/absent |

**Takeaways for WVA's roadmap:**
1. **Forecasting is the clearest borrow.** Dynamo's predictor abstraction (Constant/ARIMA/Kalman/Prophet at a fixed look-ahead) is exactly what WVA lacks — WVA is reactive with only EWMA/rate-anchored smoothing. (This is also the seam the proposed **Cluster Capacity Planner** fills at a higher altitude — forecast-driven fleet allocation above the reactive autoscaler.)
2. **WVA's structural edges are real and defensible:** heterogeneity, cost, multi-model global scope, and GPU fair-share are things *none* of the three do. Dynamo is deliberately single-model/homogeneous.
3. **Neither is distribution-aware** (bimodal interactive vs. batch) — open ground for both WVA and the planner.

---

## Sources

**WVA (baseline)**
- Repo: https://github.com/llm-d/llm-d-workload-variant-autoscaler
- KEDA integration: [docs/proposals/wva-external-scaler-proposal.md](../proposals/wva-external-scaler-proposal.md)
- Component doc: https://llm-d.ai/docs/architecture/Components/workload-variant-autoscaler
- Paper (WVA global optimization control plane): https://arxiv.org/abs/2603.09730

**NVIDIA Dynamo Planner**
- Autoscaling overview (HPA/KEDA/Planner + scaling adapter): https://docs.nvidia.com/dynamo/kubernetes-deployment/operate/autoscaling.md
- Planner component: https://docs.nvidia.com/dynamo/components/planner.md
- Planner design (modes, predictors, regression models, limitations): https://docs.nvidia.com/dynamo/design-docs/component-design/planner-design.md
- Profiler (rapid/thorough, TP sweep, SLA defaults): https://docs.nvidia.com/dynamo/components/profiler.md
- Global Planner (single-model, no cost/heterogeneity): https://docs.nvidia.com/dynamo/components/planner/global-planner-guide.md
- Code — planner: https://github.com/ai-dynamo/dynamo/tree/main/components/src/dynamo/planner
- Code — defaults: https://raw.githubusercontent.com/ai-dynamo/dynamo/main/components/src/dynamo/planner/config/defaults.py
- Code — K8s connector (`update_graph_replicas`): https://raw.githubusercontent.com/ai-dynamo/dynamo/main/components/src/dynamo/planner/connectors/kubernetes.py
- **2026** releases (**v1.3.1, 6 Aug 2026**; v1.3.0, 22 Jul 2026 — AIC replaces hand-tuned cost model): https://github.com/ai-dynamo/dynamo/releases
- **2026** planner commits (active through late Jul 2026): https://github.com/ai-dynamo/dynamo/commits/main/components/src/dynamo/planner
- **2026** roadmap #9178 (Global Planner, Grove, distribution-aware — *future work*): https://github.com/ai-dynamo/dynamo/issues/9178
- **2026** InfoQ (31 Jan 2026, Profiler + SLO Planner): https://infoq.com/news/2026/01/nvidia-dynamo-ai-kubernetes/
- **2026** Azure AKS Part 4 (2 Jun 2026, Grove; homogeneous H100; heterogeneous = future): https://blog.aks.azure.com/2026/06/02/dynamo-on-aks-part-4

**Mooncake**
- Paper (arXiv v4, Sep 2025) — "elastic scaling unfeasible", early rejection, Conductor: https://arxiv.org/abs/2407.00079 · PDF: https://arxiv.org/pdf/2407.00079 · HTML (preset P:D): https://arxiv.org/html/2407.00079v3
- Repo (components; no scheduler/autoscaler dir): https://github.com/kvcache-ai/Mooncake
- FAST'25 release: https://github.com/kvcache-ai/Mooncake/tree/main/FAST25-release
- **2026** releases (v0.3.12, Jul 2026 — storage/transfer-engine only, no scheduler): https://github.com/kvcache-ai/Mooncake/releases
- **2026** LMSYS "Elastic EP" (25 Mar 2026 — "elastic" = partial-failure tolerance, NOT autoscaling): https://www.lmsys.org/blog/2026-03-25-eep-partial-failure-tolerance/
- **2026** kvcache.ai blog (SSD offload — capacity via memory, not pool scaling): https://kvcache.ai/blog/scaling-kv-cache-beyond-memory/
- ACM ToS 2026 reprint (Conductor still design/paper only, not open): https://dl.acm.org/doi/10.1145/3773772

**SGLang**
- Router / Model Gateway: https://docs.sglang.io/advanced_features/sgl_model_gateway.html · source: https://github.com/sgl-project/sglang/blob/main/docs/advanced_features/sgl_model_gateway.md
- Router roadmap (no autoscaler): https://github.com/sgl-project/sglang/issues/10341
- PD disaggregation (manual scaling): https://docs.sglang.io/advanced_features/pd_disaggregation.html · PD roadmap: https://github.com/sgl-project/sglang/issues/21703
- K8s deployment (LWS, no HPA/KEDA): https://docs.sglang.io/references/multi_node_deployment/deploy_on_k8s.html
- Engine Prometheus metrics: https://docs.sglang.io/references/production_metrics.html
- OME integration (KEDA/Knative/LWS scale-to-zero): https://www.lmsys.org/blog/2025-07-08-ome/
- Router package: https://pypi.org/project/sglang-router/
- **2026** releases (v0.5.16, 25 Jul 2026): https://github.com/sgl-project/sglang/releases · router 0.3.2 (15 Jan 2026): https://pypi.org/project/sglang-router/#history
- **2026** roadmaps — Q1 #12780, Autonomous Model Gateway #13098, Q2 PD #21703 (30 Mar 2026): https://github.com/sgl-project/sglang/issues/12780 · https://github.com/sgl-project/sglang/issues/13098 · https://github.com/sgl-project/sglang/issues/21703
- **2026** LMSYS GTC 2026 (25 Mar 2026): https://www.lmsys.org/blog/2026-03-25-gtc2026/

**Fireworks AI** (managed)
- Autoscaling reference (all params, load targets, windows, defaults): https://docs.fireworks.ai/deployments/autoscaling
- On-demand deployments guide: https://docs.fireworks.ai/guides/ondemand-deployments
- Billing & scaling FAQ (per-GPU-second, charges accrue while replicas up): https://docs.fireworks.ai/faq/deployment/ondemand/billing-scaling
- Auto-scaling FAQ: https://docs.fireworks.ai/faq-new/deployment-infrastructure/do-you-support-auto-scaling
- **2026** changelog (5 Feb 2026 — `prompt_tokens_per_second`; scaled-to-zero returns 503): https://docs.fireworks.ai/updates/changelog

**Together AI** (managed)
- Autoscaling configuration reference (8 metrics, targets, percentiles, windows): https://docs.together.ai/docs/dedicated-endpoints/scaling
- Create-endpoint API (`autoscaling.min_replicas`/`max_replicas`, `inactive_timeout`): https://docs.together.ai/reference/createendpoint
- Engineering write-up — control law, metric selection, **TTFT and GPU-utilization negative result**, cold-start measurements: https://www.together.ai/blog/autoscaling-endpoints-for-llm-inference
- Dedicated Model Inference product page: https://www.together.ai/dedicated-model-inference
- **2026** changelog (DMI launch 16 Jul 2026; KV-cache gauges 23 Jul; CLI replica-bound inference 28 Jul): https://docs.together.ai/docs/changelog

*Freshness note (managed platforms, verified 2026-08-09):* Fireworks' scaling surface is unchanged since **5 Feb 2026**. Together's **DMI launched 16 Jul 2026** and is iterating weekly — the eight-metric list, `0 s`/`300 s` window defaults and single-metric restriction are from the official scaling reference, while the control law (`ceil(N × observed/target)`), the TTFT/GPU-utilization negative result and all cold-start figures are from Together's own engineering write-up and are **vendor self-reported, not independently reproduced**. Cold-start numbers are 1×H100 and model-size dependent. A secondary-source claim of custom-Prometheus-metric scaling is contradicted by the primary docs and is excluded.

*Freshness note (verified 2026-08-03):* checked against 2026 releases — Dynamo **v1.3.1 (6 Aug 2026)**, SGLang **v0.5.16 (25 Jul 2026)**, Mooncake **v0.3.12 (Jul 2026)**. Dynamo defaults are from `main`; Dynamo/SGLang docs are the unversioned "latest". Mooncake facts are from the arXiv paper (the open repo still omits the scheduler in 2026). Where a system exposes metrics but no controller (SGLang), rows reflect native capability, not what an external autoscaler adds. Some 2026 doc deep-links (Dynamo `latest/planner/*`) are JS-rendered and 404 to plain fetchers; the design-docs URL and `main` code are the authoritative primaries.
