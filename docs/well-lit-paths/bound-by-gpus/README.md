# Bound a fleet by real GPUs

Without a limiter, each controller scales its workloads up to their
`maxReplicaCount` and nothing checks that against the accelerators that exist.
A limiter gives the optimizer a budget: either the GPU inventory actually
discovered on the cluster, or per-accelerator caps an operator declares.

**Use it when** more than one team scales on the same accelerators, or when
`maxReplicaCount` was set as a safety limit rather than a real ceiling — which
is to say, on any shared cluster.

**Do not mistake it for enforcement.** WVA's limiter is advisory: it stops WVA
*asking* for replicas that cannot be placed. The boundary that actually holds is
a Kubernetes `ResourceQuota`. A limiter without a quota bounds a well-behaved
autoscaler, not the cluster.

## What it needs

- Cluster-admin rights, once: the policy must live where a tenant cannot edit
  it, and every controller must be able to read it.
- Every controller able to **list nodes**. A controller that cannot resolve a
  variant's accelerator gives that variant no budget and then never scales it
  up — a failure with no natural symptom, which is why the guide checks
  `reason=AcceleratorNotResolved` first.
- A decision about which limiter, below.

## Which limiter

They answer different questions, and a deployment that needs both answers
declares both — each is then fed the measure it is asking about.

| | `gpu-inventory` (the default) | `quota` |
| --- | --- | --- |
| Question | is the hardware there? | is this tenant within its allowance? |
| Counts | every GPU-holding Pod on the cluster | only what **WVA's own variants** hold |
| Declared | nothing to declare — discovered | per accelerator type, at cluster or namespace scope |

Declaring a quota has consequences an operator has to know before promising a
team a number — what it counts, why a cap is a ceiling and not a reservation,
and why a missing entry denies. Those, and both scope shapes, are in
[capping what each tenant may take](../tenant-gpu-quotas/).

This page is about the other question: not exceeding the hardware that exists.

## Setting it up

[Bound every WVA by real GPUs](../../guides/admin-gpu-bounding/) — one command,
because the ConfigMap is the easy part and the access it then requires is not.

## Verifying it

```bash
# the policy the controllers actually read
kubectl get configmap wva-scaling-policy-config -n <system-namespace> -o yaml

# what the optimizer decided, and the reason code behind each entry
kubectl logs -n <wva-namespace> deploy/<wva> | grep scaling-decision
```

`wva_model_scaling_blocked` carries the reason as a label. A model that wanted
more and did not get it is a normal, visible outcome here — not an error.

## What it costs

No accelerators, and no latency. What it costs is a conversation: a limiter
turns "why did we not scale" from an invisible event into a declared policy
somebody owns.

## How it is tested

- End-to-end: `test/e2e/limiter_test.go` (labelled `full`), and
  `test/e2e/warm_pool_quota_test.go` for the refusal path when a pool asks for
  accelerators the quota will not give.
- The accounting itself — and three ways a GPU budget can over-state free
  capacity — is written up in
  [gpu-capacity-accounting.md](../../concepts/gpu-capacity-accounting.md).

## Tuning it

The limiter is selected by the `limiters:` list on the scaling-policy
ConfigMap's `default` entry — there is no flag and no environment variable, and
because it is dynamic configuration the engine rebuilds it without a restart.
Schema, scopes and validation: [quota limiter](../../reference/quota-limiter.md).
Where the policy lives and who may edit it:
[the GPU limiter](../../reference/gpu-limiter.md).
