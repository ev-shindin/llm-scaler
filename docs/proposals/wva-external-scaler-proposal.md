# WVA as a KEDA external scaler: compute the target, let KEDA actuate

> **Status:** Draft for discussion · **Audience:** WVA maintainers, llm-d SIG-Autoscaling
> **Companions:** detailed design [`../proposals/wva-keda-external-scaler.md`](wva-keda-external-scaler.md) · evidence [`../../comparison/wva-vs-keda-scaling-study.md`](../../comparison/wva-vs-keda-scaling-study.md) · alternative-framing note [`./wva-external-scaler-alternative.md`](wva-external-scaler-alternative.md)

---

## TL;DR

Two questions, kept separate:

**What computes the replica target?**
> **WVA's measured capacity model — delivered through KEDA's standard external-scaler interface. KEDA always actuates.** WVA returns a `demand` and a per-replica `target`; KEDA/HPA compute `ceil(demand / target)` and scale. WVA stays off the critical path (opt-in per `ScaledObject`, with a plain metric as fallback).

**When do you need it?**
> **Whenever you can't hand-pick a request-per-pod threshold that stays correct as your traffic shape drifts.** Our evidence shows that is essentially always — *even for a single model on a single accelerator.* The threshold `"16 running requests per pod"` is a capacity number that changes with the model, the accelerator, the parallelism, the pod shape, **and the traffic**. `"scale at 75% KV utilization"` does not — because WVA measures the capacity for you.

You configure **intent** (an SLO / utilization tier), not a hand-derived number.

---

## Background: a metric is not a decision

KEDA and WVA are the same shape — both turn signals into a replica count. The redundancy question is only *who computes the number KEDA actuates*:

```text
            same Deployment · same KEDA ScaledObject · KEDA actuates
                                   │
        ┌──────────────────────────┴──────────────────────────┐
   a request threshold                                a capacity model
   "scale at 16 running req/pod"              "scale at 75% of measured KV capacity"
   → a number you must hand-derive            → WVA measures it, per model × accelerator
     per model×accel×shape, and re-tune         and tracks drift
     when traffic drifts
```

A raw metric plus a **static threshold** cannot track a moving capacity → you get either GPU over-provisioning or SLO breaches, and a long tuning loop in between. The [comparison study](../../comparison/wva-vs-keda-scaling-study.md) measures exactly this, on single-variant workloads.

---

## Part 1 — The approach

### WVA computes the target; KEDA actuates

WVA implements KEDA's `ExternalScaler` gRPC service. A `ScaledObject` references it with an `external` trigger and passes per-target facts in the trigger metadata. Everything else (identity, discovery, config) is covered in the [detailed design](wva-keda-external-scaler.md); the shape is:

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata: { name: chat-decode, namespace: chat }
spec:
  scaleTargetRef: { name: chat-decode }        # the Deployment/LWS WVA scales
  minReplicaCount: 1
  maxReplicaCount: 16
  triggers:
    - type: external
      metadata:
        scalerAddress: wva-scaler.wva-system.svc:9090
        modelName: llama-3-70b
        engine: vllm                            # vllm | sglang | …  (picks the right query bodies)
        inferencePool: chat-pool                # grouping key (a pool spans many Deployments)
        scalingPolicy: interactive              # a named, reusable tier (below)
```

- KEDA/HPA actuate; WVA never patches replica counts on the steady path.
- If WVA is down, KEDA `fallback` (or a plain metric trigger) keeps scaling — WVA is optional and off the critical path.

### Configure intent, not thresholds

The operator picks a **reusable policy tier**, not a capacity number. Utilization targets mean the same thing on any model/accelerator/shape, so one policy serves many models:

```yaml
# ConfigMap: wva-policy-interactive   (a tier — no model_name inside; reusable)
scaleUpThreshold: 0.85      # scale up as measured utilization approaches capacity
scaleDownBoundary: 0.70     # no-change band below this
priority: 10                # fair-share weight when GPUs are contended
analyzers:                  # which signals to combine (built-in or config-defined)
  - name: saturation        # built-in capacity model (KV / token supply vs demand)
  - name: ttft-slo          # optional SLO analyzer
    threshold: "0.4"        # per-replica TTFT target, KEDA-compatible
```

WVA measures the per-model, per-accelerator capacity behind those fractions. Cost/priority for fair-share come from the policy and per-target metadata — not from operators guessing thresholds.

### Use KEDA any way you want

WVA-as-external-scaler is **not a lock-in**. The `ScaledObject` is standard KEDA. An operator who *wants* to own the threshold just swaps the trigger for a plain `prometheus` trigger and tunes it by hand:

```yaml
  triggers:
    - type: prometheus                          # you own the metric and the threshold
      metadata:
        serverAddress: http://prometheus.monitoring:9090
        query: sum(vllm:num_requests_running{model_name="llama-3-70b"})
        threshold: "16"                          # a capacity number you accept the burden of
```

So this proposal is a **strict superset**: the right answer by default, and full manual control when you want it.

---

## Part 2 — When does this help?

### Use-case matrix

| Situation | With a raw request threshold | With WVA as external scaler | Evidence |
|---|---|---|---|
| **One model, one accelerator** | You still must hand-derive the per-pod threshold, and it over-scales off-tune | Utilization target measures the capacity | Runs 1–2 (−47%, −59% GPU-time) |
| **Traffic shape drifts** (prompt/gen mix changes) | A threshold tuned on one shape is degenerate on another | Invariant — same target across shapes | Runs 3 + 4 (KEDA pinned at cap on decode-heavy) |
| **Prefill / decode pools** | No notion of role; each Deployment scales alone → one role starves | Roles sized together | Run 5 |
| **Many models sharing a fixed GPU pool** | Every `ScaledObject` competes for any GPU | Cost/priority-aware allocation across the pool | argued |
| **Heterogeneous / scarce GPUs** | No cross-accelerator reasoning | Cheapest-variant allocation under budget | argued |
| **Scale-to-zero** | Manual | `IsActive` gate | design |

### The "do you need it" test

> **Can you hand-pick a request-per-pod threshold for your model on your accelerator that stays correct when your traffic shape drifts?**
>
> - **Yes, confidently** → a plain KEDA `prometheus` trigger is fine (use the escape hatch above).
> - **No / not sure / it drifts** → let WVA compute the target. This is the common case **even for one model** — which is the key difference from a "you only need it when models share a non-growable pool" rule.

### Evidence in one paragraph

The [comparison study](../../comparison/wva-vs-keda-scaling-study.md) runs the *same* Deployment and *same* `ScaledObject` with KEDA actuating both arms; the only variable is what computes the target. Across **five single-variant runs**, the shipped well-lit-path threshold (16 running req/pod) held **1.9×–2.7× the GPUs** WVA held at equal service — buying replicas that did no work at **3.3% KV utilization**, and pinning at its replica cap on decode-heavy traffic while ignoring the rate ladder. WVA held **−47% to −63% GPU-time** at the same latency. The one honest exception is Run 3's tail (a WVA capacity-estimator bug, fix in progress) — the single axis where a reactive threshold wins today.

---

## What this doc does not cover (non-goals)

- The gRPC contract, discovery/registry, managed-mode ownership, and collector internals — see the [detailed design](wva-keda-external-scaler.md).
- The multi-model / heterogeneous-GPU / mixed-priority claims are **argued, not yet measured** (see the study's open items).
- Migration from the current `wva_desired_replicas` external-metrics path.
- The internal capacity-model math (token supply/demand, rate anchoring) — unchanged; this doc is about *delivery*, not the model.

## References

- Evidence: [`comparison/wva-vs-keda-scaling-study.md`](../../comparison/wva-vs-keda-scaling-study.md)
- Detailed design: [`docs/proposals/wva-keda-external-scaler.md`](wva-keda-external-scaler.md)
- Alternative-framing note (why a metric alone is not enough): [`./wva-external-scaler-alternative.md`](wva-external-scaler-alternative.md)
- The "metric shop" optimizer proposal this responds to: https://github.com/lionelvillard/llm-d-workload-variant-autoscaler/blob/worktree-optimizer-proposal/docs/design/wva-optimizer-proposal.md
- KEDA external scalers: https://keda.sh/docs/latest/concepts/external-scalers/
- KEDA Prometheus scaler (the escape hatch): https://keda.sh/docs/latest/scalers/prometheus/
