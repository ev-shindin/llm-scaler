# Alternative to the WVA Optimizer Proposal: keep the capacity model in WVA, deliver it as a KEDA external scaler

> **Status:** Draft for discussion (untracked working doc) · **Date:** 2026-08-04
> **Responds to:** [`wva-optimizer-proposal.md`](https://github.com/lionelvillard/llm-d-workload-variant-autoscaler/blob/worktree-optimizer-proposal/docs/design/wva-optimizer-proposal.md) ("WVA as a metric shop")
> **Companion:** [`docs/proposals/wva-keda-external-scaler.md`](wva-keda-external-scaler.md)

---

## 1. Thesis

The optimizer proposal is right about two things and wrong about one that matters.

**Right:** KEDA should be the actuator, and WVA should stay off the critical path (optional, failure-tolerant). We keep both.

**Wrong:** it treats the *scaling target* — the threshold a metric is compared against — as either an operator config value or something that "emerges" from a performance model, and it explicitly defers **per-model/accelerator threshold selection** and **traffic-shape drift** as out of scope. That is the whole problem, not a detail. **The right threshold is a measured capacity, specific to a model × accelerator × parallelism × pod-shape, and it drifts with the traffic shape.** Computing it is exactly WVA's job — and, as our own evidence shows, it is needed **even for a single model with a single variant.**

**Alternative:** WVA computes the capacity-model decision internally and delivers it through KEDA's **standard external-scaler interface**. KEDA still actuates; WVA stays optional and off the critical path; and the operator is never asked to hand-derive a threshold they cannot know. Operators who *want* to own thresholds keep that option — they simply use a plain KEDA `prometheus` trigger instead.

## 2. What the optimizer proposal says

It adopts "Approach A": **WVA becomes a metric shop** offering a spectrum of signals (L0 raw load → L1 per-model demand → L2 coordinated allocation); **KEDA/HPA always actuates**; WVA is one optional source. L1 ("replicas needed per model for SLO") is designated to **EPP (extended)** or WVA. Its install rule: *"If two models both spike, can you just add GPUs, or must they share a pool that can't grow? Only the latter requires WVA."* Threshold selection and drift are listed as **not addressed**.

We agree with the actuation stance. We disagree that a single model doesn't need WVA, and that the threshold is a deferrable detail.

## 3. The flaw: a metric is not a decision, and a threshold is a measured capacity

Publishing a metric to KEDA and letting a formula compare it to a threshold only works if **someone knows the right threshold.** But the threshold is not a policy knob:

- **"16 running requests per pod" is a capacity number.** It moves with the model, the accelerator, the tensor/parallelism config, and the pod shape. Telling an operator the *metric name* (`num_requests_running`) without the right number is useless — they have no basis to choose 16 vs 24 vs 40 for *their* model on *their* GPU.
- **It also moves with the traffic shape.** A threshold tuned on 300/300-token traffic over-scales on 1000/250, and is degenerate on 100/1000 (decode-heavy).
- **A single Prometheus metric + static threshold cannot track a moving capacity.** The result is either over-provisioning (GPU waste) or under-provisioning (SLO breach) — never balanced. Operators are left to *re-derive the threshold per model × accelerator × config, and re-tune it whenever the workload shape drifts.* That is a long, unstable trial-and-error loop.

By contrast, **"scale at 75% KV utilization" means the same thing on any model, accelerator, or request shape** — because WVA *measures* the per-variant capacity itself and expresses the target as a utilization fraction. The hard, model-specific, drift-sensitive part is done by measurement, not by an operator's guess.

## 4. The evidence — and it is all single-variant

Our comparison study ([`comparison/wva-vs-keda-scaling-study.md`](../../comparison/wva-vs-keda-scaling-study.md); figures `comparison/figures/src/exp*.png`) tests **exactly the optimizer proposal's framing**: the *same* Deployment, the *same* KEDA ScaledObject, KEDA actuating in every run — the **only** variable is *what computes the number KEDA actuates*: a **request threshold** (KEDA alone) vs **WVA's capacity model** (WVA + KEDA). The KEDA arm runs the **shipped well-lit-path config** (16 running / 50 queued requests per pod; from llm-d PR #1981, the KEDA conversion of the EPP autoscaling guide) — not a strawman.

Every run is **one model, one accelerator, one variant.** Results:

| Run | Shape | GPUs held (WVA vs KEDA) | Finding |
|---|---|---|---|
| 1 | symmetric 300/300, 10 req/s | **1.00 vs 1.88** (−47%) | Bursts cross the request line though the mean fits one pod → KEDA cycles 1↔2↔3, drops ~30 requests; WVA stable. Same latency. |
| 2 | prefill-heavy 1000/250, rising | **1.00 vs 2.46** (−59%) | KEDA steps to 4 replicas at **3.3% KV utilization** — counting requests, not measuring work. Nothing queued on either arm: the extra replicas did no work, only cost. |
| 3 | prefill-heavy 1000/250, sustained @24 | **1.56 vs 4.01** (−61%) | WVA cheaper but **pays in the tail** (drain cycles cut live streams). The one axis where a reactive threshold wins today. |
| 4 | decode-heavy 100/1000, sustained @24 | **2.21 vs 5.97** (−63%) | KEDA **pinned at its 10-replica cap within 3 min**, ignoring the 16→20→24 rate ladder — the threshold calibrated for prefill-heavy is *degenerate* on decode-heavy. No tail cost for WVA. |
| 5 | prefill / decode on 6 GPUs | **3:3 vs 5:1** | A per-Deployment threshold has no notion of role; sizing each role alone starves one. |

**The mechanism is Runs 3 + 4:** the *same* load and *same* config on opposite token shapes. The request threshold over-scales on one shape and pegs at the cap on the other — because *"a fixed request threshold is only valid for the shape it was tuned on."* WVA's utilization target is invariant across both.

**Honest caveat (kept, because it strengthens the point):** Run 3 shows WVA's current tail cost — its compute-capacity estimate recovers toward the memory bound as the queue drains, so it releases replicas while arrivals are unchanged; a replacement estimator exists behind a build flag and is not yet measured on this workload. This is a WVA bug to fix, **not** a defense of static thresholds — the threshold-calibration failure is independent and reproduced in every run.

**What the evidence forces:** even for a single model / single variant, the naive threshold either over-scales (Runs 1, 2, 4) or is fragile. This **directly refutes the install rule** that WVA is only needed when models contend for a non-growable pool. The capacity model earns its place at one model.

## 5. Why "the operator picks it" or "EPP emits it" doesn't rescue the metric-shop model

- **Operator picks the threshold.** They have no basis to choose it (it's a per-model×accelerator capacity) and it drifts with shape — so they get the long trial-and-error loop above, and land on either SLO breaches or idle GPUs.
- **"It emerges from EPP's roofline / WVA's optimizer."** If producing the threshold *requires a capacity model*, then a capacity model is on the path — which is WVA's job. The proposal even concedes EPP must be **"extended"** to map load→replicas (L1). That extension *is* re-implementing WVA's capacity model inside EPP. Forking the capacity model into EPP is strictly worse than keeping it in WVA and delivering it through a standard interface.
- **A raw Prometheus metric is not enough.** Right scaling needs *measured per-variant capacity + drift tracking*, not *metric + static threshold*. The metric shop ships the metric and leaves the hard half unsolved.

## 6. The alternative: WVA as a KEDA external scaler

Keep the capacity model in WVA; deliver its decision through KEDA's **external-scaler gRPC contract** (full design in [`docs/proposals/wva-keda-external-scaler.md`](wva-keda-external-scaler.md)):

- WVA computes the full target — `demand ÷ measured per-variant capacity` → a replica count — and returns it to KEDA; **KEDA/HPA actuate.** (Satisfies "KEDA is the actuator.")
- The scaler is **opt-in per ScaledObject** and off the critical path: if WVA is unavailable, KEDA `fallback` / a plain `prometheus` trigger keeps scaling. (Satisfies "WVA optional, failure-tolerant.")
- The operator configures **intent** (an SLO tier / policy), not a hand-derived capacity number. The model/accelerator/shape-specific part is *measured by WVA*, so it tracks drift automatically.
- It handles the cases the metric shop concedes it cannot: **role-aware P/D sizing**, **cost/quota-aware allocation across a shared pool**, and **traffic-shape invariance** — the exact rows the evidence marks WVA-favoured.

This is **not** "Approach B" (WVA taking over actuation). Actuation stays in KEDA. The only thing that moves back into WVA is the *computation of the right target* — which is where the measurement lives.

## 7. Freedom preserved: use KEDA any way you want

WVA-as-external-scaler is **not** a lock-in — the ScaledObject is standard KEDA. An operator who wants to own the threshold can **swap the external trigger for a `prometheus` trigger** on a raw metric and tune it by hand — precisely the metric-shop world, available as a *choice*. So this proposal is the **strict superset**:

- **Default:** the right answer, computed by WVA's capacity model, delivered via the external scaler — no threshold guessing.
- **Escape hatch:** full manual KEDA control (Prometheus trigger + your own thresholds) for anyone who wants it.

The optimizer proposal offers *only* the manual world and marks the hard part out of scope. We offer the correct default **and** the manual world.

## 8. Recommendation

Adopt **WVA as a KEDA external scaler** as the way WVA participates in the KEDA stack. It meets every *valid* goal of the optimizer proposal — KEDA actuates, WVA is optional and off the critical path, one stack fits elastic/fixed/mixed capacity — while fixing its central omission: **who computes the right threshold, and how it tracks drift.** The evidence says that omission is not deferrable: it is the difference between −47% to −63% GPU-time at equal service and a fleet pinned at its cap on the wrong token shape, **at a single model.**

## Sources

- Optimizer proposal ("metric shop"): https://github.com/lionelvillard/llm-d-workload-variant-autoscaler/blob/worktree-optimizer-proposal/docs/design/wva-optimizer-proposal.md
- Comparison study (numbers, figures, KEDA-arm config): [`comparison/wva-vs-keda-scaling-study.md`](../../comparison/wva-vs-keda-scaling-study.md)
- KEDA well-lit-path config under test: llm-d PR #1981 (KEDA conversion of the EPP autoscaling guide)
- WVA-as-external-scaler design: `docs/proposals/wva-keda-external-scaler.md`
- Label/metric surface backing "utilization means the same thing, request counts don't": `internal/collector/registration/*.go`, `internal/constants/metrics.go`
