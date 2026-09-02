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

Three things about a quota surprise people, and all three are consequences of
that middle row:

**A quota does not bound the cluster.** It is an allowance granted to WVA and
spendable only by WVA, so it can hand out capacity that non-WVA workloads have
already taken. Charging it the physical figure instead would be worse — a
namespace with a 4-GPU allowance sharing space with an unrelated 4-GPU training
job would read as fully spent while WVA had placed nothing, and every scale-up
would be refused against an untouched allowance. If you need both guarantees,
declare both limiters.

**Ceilings, not reservations.** A finite cap guarantees a tenant never *exceeds*
it. It does not guarantee the tenant can *reach* it: the cluster aggregate is the
sum of the finite caps, so a namespace that is unlimited (`-1`) or excluded draws
from that shared aggregate without contributing to it, and can consume budget a
capped peer would otherwise have used. Nobody exceeds their own authorization —
but it is a real fairness gap, and worth knowing before you promise a team its
number.

**A missing entry denies.** `0` or no entry for an accelerator type means no
allocation at all, not "unbounded" — `-1` is unbounded. A typo in a type name is
therefore a denial, which is the safe direction and a confusing one.

Fairness is per **model**, not per namespace: two models in one namespace each
get their own fair-share slot, and the namespace cap bounds each of them and
their running sum rather than pooling one model's allowance for the other. The
quota clamps after the fair-share split is computed — it is a ceiling on the
outcome, not an input to it.

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
