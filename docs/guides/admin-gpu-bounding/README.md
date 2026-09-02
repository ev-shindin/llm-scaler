# Bound every WVA by real GPUs

## Overview

Publishes cluster policy declaring a GPU limiter, and grants every WVA on the
cluster the access that policy then requires. Without it, each controller scales
to its workloads' `maxReplicaCount` with no check against the GPUs that exist.

One command, because the ConfigMap is the easy part: the policy must live where a
tenant cannot edit it, every controller must be able to read it, and every
controller must be able to list nodes — and a controller that cannot resolve a
variant's accelerator gives it no budget and never scales it up.

## Prerequisites

Cluster-admin rights, and **accelerators that resolve**. Check first — this is
the failure with no natural symptom:

<!-- guide:prerequisites.accelerators start -->
```bash
# Check accelerators resolve FIRST. A GPU-aware optimizer gives no budget to a
# variant whose accelerator it cannot resolve, and it then never scales up.
# The event is the signal to trust: it is re-emitted for as long as the
# condition holds, and the API server folds the repeats into one entry with a
# count. It hangs on the ScaledObject, so it is in the MODEL's namespace, not
# WVA's -- hence -A. The log line says the same thing but is printed once per
# CHANGE, so on a controller that has been up a while it may have scrolled
# away: an empty grep is not an all-clear.
kubectl get events -A --field-selector reason=AcceleratorNotResolved
# Corroborates, never clears: this line is logged once per change.
kubectl logs -n <wva-namespace> deploy/wva-controller-manager | grep -i "Accelerator not resolved"
```
<!-- guide:prerequisites.accelerators end -->

WVA resolves a variant's accelerator from a GPU key in its `nodeSelector`, or
from the nodes its running pods are on. A workload with neither gets no budget.

## Installation Instructions

<!-- guide:deploy.enable start -->
```bash
# Publishes cluster policy AND grants every controller the node read it then
# requires — grants first, because declaring the limiter without them takes
# out every WVA that lacks node access at once.
make enable-physical-limiter
```
<!-- guide:deploy.enable end -->

Grants come first, then the policy: declaring the limiter without them takes out
every WVA that lacks node access at once. Controllers are discovered rather than
configured — the hazard is the install you forgot.

Policy lands where each controller actually reads it: the well-known namespace
for a self-managed controller, its own ConfigMap for an admin-owned one.

## Verification

<!-- guide:verify.controllers start -->
```bash
# A controller that is not Ready is one this did not reach.
kubectl get pods -A -l app.kubernetes.io/name=workload-variant-autoscaler
```
<!-- guide:verify.controllers end -->

<!-- guide:verify.budget start -->
```bash
# The ConfigMap is the fact; the log line is not. "GPU budgets available" is
# emitted only by the scale-from-zero engine while it judges a wake, so a
# correctly bounded cluster with nothing parked at zero prints nothing and
# looks like a failure. Read the policy instead, and treat the log as a
# bonus when scale-from-zero is in play.
kubectl get configmap wva-scaling-policy-config -n wva-policy \
  -o jsonpath='{.data.default}' | grep -A3 limiters
kubectl logs -n <wva-namespace> deploy/wva-controller-manager | grep "GPU budgets available"
```
<!-- guide:verify.budget end -->

## Cleanup

<!-- guide:cleanup.disable start -->
```bash
# Scaling becomes unbounded again for every WVA that reads this policy.
make disable-physical-limiter
```
<!-- guide:cleanup.disable end -->

## What it does and does not bound

A guardrail, not an enforcement boundary. Whoever owns a controller's Deployment
owns its args and image, so a tenant running WVA inside their own namespace can
change what bounds them — run it
[outside their namespace](../admin-cluster-setup/README.md#keeping-the-controller-out-of-the-tenants-reach)
where the bound must hold, and use a `ResourceQuota` for a tenant you do not
trust. It also bounds only what WVA asks for: raising `maxReplicaCount` or adding
a second KEDA trigger bypasses it.

**Declare one kind, not both.** `limiters:` is a list but only one is built, and
a quota entry wins — declaring `quota` alongside `gpu-inventory` gives the caps
and no physical limiter at all. WVA names what it dropped rather than doing it
silently.

## Configuration

| Parameter | Default | Example |
| --- | --- | --- |
| `WVA_LIMITER_TYPE` | `gpu-inventory` | `quota` |
| `WVA_POLICY_NS` | `wva-policy` | `platform-policy` |
| `WVA_LIMITER_TARGETS` | every controller found | `team-a team-b` |

`WVA_LIMITER_TYPE` is not `WVA_LIMITER`. This one writes the **cluster** policy
every controller reads; `WVA_LIMITER` writes a single install's own policy, which
that install's owner can then edit.

## Next

- [The GPU limiter](../../reference/gpu-limiter.md) — why policy lives where it
  does, and the quota form
