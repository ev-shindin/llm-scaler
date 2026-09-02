# Scale a model to zero, and get it back

## Overview

Lets an idle model release its accelerators entirely, and brings it back when a
request arrives. Two mechanisms, and they fail independently:

- **Parking** is WVA's. When a model serves nothing for `retentionPeriod`, it
  scales every variant to zero.
- **Waking** is EPP's queue plus KEDA. A request for a model with no endpoints is
  held in EPP's flow-control queue; WVA reads that queue, publishes an
  activation, and KEDA scales the workload off zero.

Parking is the easy half and the dangerous one. A cluster can park a model
perfectly and be unable to wake it, and nothing about the park looks wrong — so
**check the wake path first**, before you let anything park.

Turning this on is two settings that must agree. Scale-to-zero enabled on the
model, and `minReplicaCount: 0` on **every** variant. Set one without the other
and you get a valid configuration that quietly does not do what it looks like it
does: WVA reports which half is missing rather than leaving you to find out from
the bill.

## Prerequisites

A model already serving under WVA — follow
[Install WVA in a namespace](../install-in-namespace/) first.

Then the wake signal, which is the precondition worth checking before any other:

<!-- guide:prerequisites.flowcontrol start -->
```bash
# Check the WAKE signal first. Parking a model is easy and waking it needs a
# queue that only exists when EPP has flow control enabled — so a cluster
# that fails this check will park a model and never get it back.
# Ask the EPP process what it PARSED, not what the ConfigMap says: EPP reads
# --config-file once at startup, so enabling the gate does not reach a pod
# that is already running.
kubectl logs -n <llmd-namespace> deploy/<epp-deployment> | grep -m1 -i featuregates
# want: featureGates:["flowControl"]   — if absent, or the pod predates the
# ConfigMap edit, restart it:
#   kubectl rollout restart -n <llmd-namespace> deploy/<epp-deployment>
```
<!-- guide:prerequisites.flowcontrol end -->

Ask the process, not the ConfigMap. EPP reads `--config-file` once at startup, so
a ConfigMap that gains `featureGates: [flowControl]` never reaches a pod already
running — and every artifact you would compare (image digest, ConfigMap, feature
gate, auth) still matches. That difference cost a week of failures blamed on the
autoscaler. It recurs on **every** ConfigMap edit, because the EPP Deployment
carries no `checksum/config` annotation to restart it.

<!-- guide:prerequisites.engine start -->
```bash
# One engine per model. Idleness is read from that engine's request counter —
# vllm:request_success_total or sglang:num_requests_total — and WVA asks for
# the one matching the engine it detects. A model running BOTH would need both
# counters summed, so it is refused rather than measured with half its traffic.
kubectl get deploy -n <llmd-namespace> -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.template.spec.containers[0].image}{"\n"}{end}'
```
<!-- guide:prerequisites.engine end -->

## Installation Instructions

**Half one — allow the model to park.**

<!-- guide:deploy.policy start -->
```bash
# Half one: allow the model to park. Absent, this inherits from the
# WVA_SCALE_TO_ZERO deployment flag.
kubectl edit configmap wva-scaling-policy-config -n <wva-namespace>
# under the entry this model resolves to (its scalingPolicy tier, or default):
#   scaleToZero:
#     enabled: true
#     retentionPeriod: 10m   # idle time before parking
#
# Time to zero is retentionPeriod + KEDA's cooldownPeriod, in sequence, and
# applies to the final drop out of service only.
```
<!-- guide:deploy.policy end -->

The entry is the one this model resolves to: its `scalingPolicy` tier from the
ScaledObject's trigger metadata, or `default`. Policy is a **model** property, not
a variant one — a model is at zero only when nothing serves it, so there is
nothing meaningful a single variant could decide here.

**Half two — let every variant reach zero.**

<!-- guide:deploy.bounds start -->
```bash
# Half two: let every variant reach zero. A model is at zero only when
# NOTHING serves it, so one variant left at 1 keeps the whole model up and
# the policy above does nothing.
kubectl get scaledobject -n <llmd-namespace> -o custom-columns=NAME:.metadata.name,MIN:.spec.minReplicaCount,MAX:.spec.maxReplicaCount
```
<!-- guide:deploy.bounds end -->

Every `MIN` must be `0`. One variant left at `1` keeps the whole model up, and the
policy above then does nothing at all.

**Then the trap that costs the most time.**

<!-- guide:deploy.hpa start -->
```bash
# Read the SCALEDOBJECT, not the HPA. KEDA always gives the derived HPA
# minReplicas: 1 — the HPA API cannot express 0 — and drives 1->0 itself, so a
# perfectly healthy parked workload sits behind an HPA reading 1. Only
# spec.minReplicaCount on the ScaledObject decides whether zero is permitted.
# If you changed it after KEDA built the HPA and nothing parks, delete the
# ScaledObject and let KEDA rebuild rather than editing in place.
kubectl get scaledobject -n <llmd-namespace>         -o custom-columns=NAME:.metadata.name,MIN:.spec.minReplicaCount
# minReplicas: 1 on the derived HPA below is EXPECTED, not a fault:
kubectl get hpa -n <llmd-namespace> -o custom-columns=NAME:.metadata.name,MIN:.spec.minReplicas
```
<!-- guide:deploy.hpa end -->

**`minReplicas: 1` on the derived HPA is normal.** KEDA always sets it — the HPA
API has no way to express 0 — and drives the last step to zero itself. A healthy,
parked workload sits behind an HPA reading `1`, so that number tells you nothing
about whether scale-to-zero is working. Verified on a cluster: a model observed at
zero replicas had `minReplicas: 1` on its HPA throughout.

What decides it is `spec.minReplicaCount` on the **ScaledObject**. If you changed
that after KEDA had already built the HPA and nothing parks, delete the
ScaledObject and let KEDA rebuild it rather than editing in place.

## Verification

Start by asking WVA whether it thinks anything is in the way. Silence is the
healthy answer:

<!-- guide:verify.blocked start -->
```bash
# Ask WVA whether anything stops this model parking. No output is the healthy
# answer. Each reason names a CONTRADICTION between the two halves above.
kubectl port-forward -n <wva-namespace> svc/wva-controller-manager-metrics-service 8443:8443 &
curl -sk https://localhost:8443/metrics | grep wva_model_scaling_blocked
# variant-floor        a variant still has minReplicas > 0
# policy-forbids-zero  every variant permits zero, the policy does not
# engine-unsupported   the model runs BOTH vLLM and SGLang
# activation-retention just woken from zero and held; clears itself
# no-wake-signal       EPP exports no flow-control queue: it would not wake
```
<!-- guide:verify.blocked end -->

| reason | what to change |
| --- | --- |
| `variant-floor` | a variant still has `minReplicaCount > 0` — half two is incomplete |
| `policy-forbids-zero` | every variant permits zero but the policy does not — half one is incomplete |
| `engine-unsupported` | the model runs **both** vLLM and SGLang, so no single request counter measures it; either alone is fine |
| `activation-retention` | transient: just woken from zero and held for `retentionPeriod` so the wake is not undone. Clears itself; nothing to do |
| `no-wake-signal` | EPP exports no flow-control queue — **fix before letting it park** |

Then watch it park:

<!-- guide:verify.park start -->
```bash
# Stop sending traffic and wait out retentionPeriod. wva_model_replicas is
# the model's total across variants, so 0 means nothing is serving it.
kubectl get deploy -n <llmd-namespace> -w
curl -sk https://localhost:8443/metrics | grep wva_model_replicas
```
<!-- guide:verify.park end -->

Then prove it comes back. This drives the whole chain on a real cluster and says
which link broke, rather than reporting a wake that some other cause produced:

<!-- guide:verify.wake start -->
```bash
# The whole chain, end to end, on a real cluster: requests queue at zero
# endpoints -> WVA publishes an activation -> the workload leaves zero. A
# wake for some other reason is not a pass, so it checks the queue and the
# activation too, and names WHICH link broke. It parks the model itself
# first, and restores what it changed.
./hack/verify_scale_from_zero.sh <llmd-namespace> <deployment> <modelID>
```
<!-- guide:verify.wake end -->

It parks the model itself first, asserts the HPA precondition before anything
else runs, and restores what it changed. A wake caused by a floor, a manual
scale, or another controller is **not** counted as a pass — the queue depth and
WVA's own activation are checked alongside the replica count.

## Cleanup

<!-- guide:cleanup.disable start -->
```bash
# Set enabled: false and the model is held at one replica again. Leaving the
# variants at minReplicaCount: 0 is then a contradiction WVA will report as
# policy-forbids-zero — raise them back if you want the metric quiet.
kubectl edit configmap wva-scaling-policy-config -n <wva-namespace>
```
<!-- guide:cleanup.disable end -->

## When a model will not park

Two configurations are valid, cost money, and produce no symptom of their own —
the model is up and serving, exactly as every other metric says it should be:

| what you set | what actually happens |
| --- | --- |
| scale-to-zero enabled, one variant floored | that variant keeps serving, the model never reaches zero, the setting is inert — idle accelerators, billed indefinitely |
| every variant at `minReplicaCount: 0`, policy disabled | WVA holds the model up, and the bounds are inert |

The second is most misleading with a **single variant**, where `minReplicaCount: 0`
reads exactly like a deliberate request to park.

Neither is an error, and WVA does not reject either. It reports them on
`wva_model_scaling_blocked` and logs once when the answer changes, because the
thing that is wrong is your expectation, and nothing else will tell you.

## When a model will not wake

`no-wake-signal` means EPP is exporting no flow-control queue **at all** — not
that the queue is empty. An idle queue reports `0` and is healthy; a queue that
does not exist reports nothing, and a model parked behind it is stranded.

WVA reports this for **serving** models too, not just parked ones, and that is
the point. The idle check that parks a model is vLLM's request counter, which has
nothing to do with flow control — so WVA will park a model behind a
flow-control-less EPP and only then discover it cannot get it back. The warning
arrives while the model is still up and the fix is still cheap.

The usual cause is the stale pod described under Prerequisites. Restart EPP.

## Monitoring

| signal | means |
| --- | --- |
| `wva_model_scaling_blocked{reason}` | present only while that reason holds; absence is healthy |
| `wva_model_replicas` | replicas serving a model, across variants. `0` is normal for an idle parked model |
| `WVAModelWillNotScaleToZero` | info, 15m — a configuration contradiction, costing accelerators |
| `WVAModelHasNoWakeSignal` | warning, 10m — parked models here will not come back |
| `WVAModelParkedWhileRequestsRefused` | critical, 5m — at zero while requests are being refused, whatever the cause |

The last one is the one to page on: it alerts on the **symptom**, so it catches
causes nobody has enumerated, including ones not in the table above. Start from
`wva_model_scaling_blocked` for that model — if a reason is present, it is
probably the cause.

Two panels on the [operational dashboard](../../reference/monitoring.md) carry
the same data: *Models that will not scale, by reason* and *Models at zero
replicas*.

## Configuration

| Parameter | Default | Example |
| --- | --- | --- |
| `scaleToZero.enabled` | inherits `WVA_SCALE_TO_ZERO` | `true` |
| `scaleToZero.retentionPeriod` | `10m` | `5m` |
| `scaleFromZero.requirePrefill` | `false` | `true` |
| ScaledObject `minReplicaCount` | `1` | `0` — required on **every** variant |
| ScaledObject `cooldownPeriod` (KEDA) | `300s` | `30s` |

## How long parking actually takes

**Two timers run in sequence, and they add up.** This is the single most common
reason a fleet looks like it "will not park":

```text
last request ──┬─ retentionPeriod ─┬─ ≤1 optimize interval ─┬─ cooldownPeriod ─┬─ 0 replicas
               │ (WVA decides)     │ (trigger goes inactive)│ (KEDA acts)      │
```

WVA reports the KEDA trigger active *until* it decides the model needs zero, and
it only decides that once the idle query over `retentionPeriod` reads zero. So
KEDA's cooldown cannot even begin until WVA is already done waiting. With both
defaults that is **10m + 300s ≈ 15 minutes** from the last request. Halving one
timer halves only its own share.

Both apply to the **final drop out of service** only. An ordinary scale-down while
the model is still serving (10 → 3) goes through the HPA and is not held by either
timer. WVA adds no hold of its own: KEDA already guards that transition per
ScaledObject, from cluster state, so a second WVA-side timer could only disagree
with it.

`retentionPeriod` does double duty: it is how long a model must be idle before it
parks, and how long a just-woken model is held before the idle check may park it
again. Without that hold, a wake is undone before it can serve the request that
asked for it — the request is still queued in EPP while the pod pulls and loads,
so the counter reads idle for precisely the model that has demand waiting.

`scaleFromZero.requirePrefill` applies to P/D models: by default a decode variant
may be woken alone.

## Next

- [After the install](../../reference/operations.md) — what to watch, first-line
  troubleshooting
- [Scaling policy configuration](../../reference/scaling-policy.md) —
  every field of an entry, and how tiers resolve
- [Monitoring](../../reference/monitoring.md) — the dashboard and the full metric
  surface
