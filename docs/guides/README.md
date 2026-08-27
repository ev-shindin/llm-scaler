# WVA guides

Each guide takes one reader from nothing to a working install. Follow one; they
do not need to be combined.

```bash
source docs/guides/env.sh
```

## Installing

| guide | for |
| --- | --- |
| [Install a small llm-d model](install-small-model/) | **first, if nothing is serving yet.** One 0.6B model on ONE GPU — llm-d's own guides start at 16 |
| [Install WVA in a namespace](install-in-namespace/) | **start here** when llm-d already serves. One team, one namespace |
| [Install WVA for the whole cluster](install-cluster-wide/) | one controller for every namespace |

## Cluster administration

| guide | for |
| --- | --- |
| [Cluster-admin setup for a namespace](admin-cluster-setup/) | let a namespace's owner install WVA themselves |
| [Bound every WVA by real GPUs](admin-gpu-bounding/) | scaling is otherwise bounded only by `maxReplicaCount` |

## Advanced

| guide | for |
| --- | --- |
| [Scale a model to zero, and get it back](scale-to-zero/) | release an idle model's accelerators — and check it can wake before it parks |
| [Test against a full llm-d stack](testing-with-llm-d/) | llm-d + WVA on kind, emulated GPUs, no hardware |
| [Benchmark WVA](benchmarking/) | drive load through a real stack and compare runs |
| [Bridge a scale-up with a warm pool](warm-pool/) | hold models loaded and asleep so a scale-up serves while its replica starts |

## Reference

| page | covers |
| --- | --- |
| [Configuration](../deployment/configuration.md) | every variable the installer reads |
| [After the install](../deployment/operations.md) | verifying the install, first-line troubleshooting |
| [Watching what WVA decides](../deployment/monitoring.md) | the dashboard, the metrics, the logs |
| [Preparing a workload](../deployment/workload-preparation.md) | the model cache, draining, `make workload-patch` |
| [Install methods](../deployment/install-methods.md) | GitOps, direct Kustomize, what the script does |
| [The GPU limiter](../deployment/gpu-limiter.md) | where policy lives, and the accelerator precondition |

## Checking it works, last

```bash
make benchmark-smoke NAMESPACE=<namespace>
```

Decode-heavy load at 10 req/s for five minutes against what you already
installed, then a dashboard snapshot over that window. It stands nothing up and
needs no benchmark CLI; it checks the whole chain first — KEDA, a WVA
controller managing *this* namespace, model servers, an EPP, a ScaledObject —
and names every gap at once. Runs on plain Kubernetes and OpenShift.

## Editing a guide

Bash blocks between `<!-- guide:… start -->` markers are generated from
`guide.yaml`. Edit the YAML, then:

```bash
make guides-render     # rewrite the blocks
make guides-check      # CI: fail if a README has drifted
```

Prose outside the markers is preserved. The commands a reader copies are the
commands the YAML declares, so a guide cannot drift from what it documents.
