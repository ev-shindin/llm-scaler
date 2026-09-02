# Scale a P/D-disaggregated model

> **Experimental.** The scenario stands up and runs, and WVA's role handling is
> covered end to end. What is short is the third leg: there is no published
> benchmark comparison for this shape, and the scenario's image pinning is still
> being settled. Take it as a working recipe, not a settled default.

Prefill and decode have different shapes — prefill is compute-bound, decode is
memory-bandwidth-bound — so llm-d can run them as separate deployments connected
by a KV transport. WVA treats each role as its own variant of the same model,
which means the two are scaled apart on their own evidence rather than together
on an average of both.

**Use it when** you already run P/D disaggregation, or are evaluating it for
long-context workloads on medium-to-large models, and want the roles scaled
independently.

**Do not use it when** you have not measured that a single-role deployment is
the problem. P/D adds a transport dependency and a second deployment to operate;
on short-context traffic it buys little.

## What it needs

- An llm-d P/D deployment: prefill and decode workloads with the role carried on
  each pod template.
- A KV transport between them. NIXL supports TCP, but high-bandwidth networking
  (IB, RoCE, EFA) is what makes the shape worth having.
- A ScaledObject per role, each with the model's `modelID` in trigger metadata.

## Setting it up

The scenario file is
`hack/benchmark/scenarios/guides/pd-disaggregation.yaml`, adapted from llm-d's
own P/D guide, and it is run through [Benchmark WVA](../../guides/benchmarking/).
It stands up two prefill and two decode pods for Qwen3-32B, with the accelerator
profile auto-detected.

## Verifying it

```bash
# each role carries its own target
wva_desired_replicas{exported_namespace="<ns>"}

# required capacity is reported per role for a P/D model
wva_required_capacity{exported_namespace="<ns>"}
```

The role appears on the workload's pod template; the engine reports required and
spare capacity per role rather than one number for the pair.

## What it costs

Two deployments to operate instead of one, plus the transport. WVA itself adds
nothing: the roles cost what they would cost anyway.

## How it is tested

End-to-end: `test/e2e/scale_from_zero_pd_test.go` — three specs, checking that
the P/D role is carried on each workload's pod template, that a queued request
wakes the **decode** variant, and that prefill is **never woken alone**.

That is scale-from-zero behaviour for the P/D shape. Steady-state scaling of a
P/D model rides on [the saturation path](../scale-on-saturation/) and its suites;
there is no P/D-specific steady-state spec.

## Tuning it

Per-role thresholds follow the ordinary policy surface:
[scaling policy](../../reference/scaling-policy.md). What the roles mean to the
engine's capacity model:
[the steady-state engine](../../concepts/steady-state-engine.md).
