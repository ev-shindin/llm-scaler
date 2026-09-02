# Workload-Variant-Autoscaler (WVA)

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![CI](https://github.com/ev-shindin/llm-scaler/actions/workflows/ci-pr-checks.yaml/badge.svg?branch=main)](https://github.com/ev-shindin/llm-scaler/actions/workflows/ci-pr-checks.yaml)
[![Go](https://img.shields.io/github/go-mod/go-version/ev-shindin/llm-scaler)](go.mod)

> **An advanced fork of [llm-d/llm-d-workload-variant-autoscaler](https://github.com/llm-d/llm-d-workload-variant-autoscaler).**
>
> This fork keeps WVA's core idea -- variant-aware autoscaling for LLM inference --
> and has diverged substantially from it. The largest additions here are a **KEDA
> external scaler**, which lets WVA answer KEDA directly over gRPC instead of
> publishing a metric and hoping; a **warm pool** that bridges a slow scale-up
> by lending a Pod with the model already loaded and asleep, so the variant
> serves while its own replica starts; the **ScaledObject discovery and install
> tooling** behind `make scaledobjects-plan`, which writes an editable plan of
> what would be autoscaled before anything is applied; and **fixture-executing
> check suites** that run the deploy shell against recorded pod specs in CI.
>
> Developed independently. Not affiliated with, nor endorsed by, the llm-d project.


The Workload Variant Autoscaler (WVA) is a Kubernetes-based global autoscaler for inference model servers serving LLMs. WVA works alongside the standard Kubernetes HPA and external autoscalers like KEDA to drive the scale subresource of inference deployments. The high-level details of the algorithms are documented [here](https://llm-d.ai/docs/architecture/advanced/autoscaling). It determines optimal replica counts for a given request traffic load by considering constraints such as GPU availability, energy budget, and performance budget (latency/throughput).

### What is a Variant?

WVA introduces the concept of **variants** — multiple model servers in an InferencePool that all serve the same base model but differ in hardware configuration (e.g., GPU type), serving configuration (e.g., tensor parallelism, max batch size, quantization), or both.

Use cases include:

- **P/D disaggregation**: prefill is one variant, decode is another — variant = role in a disaggregated pipeline.
- **[batch-gateway](https://github.com/llm-d-incubation/batch-gateway)**: variants distinguish batch vs. interactive workloads sharing the same pool.
- **Autoscaler**: a costed serving configuration the autoscaler chooses among.

## Key Features

- **Intelligent Autoscaling**: Optimizes replica count by observing the current state of the system
- **Cost Optimization**: Minimizes infrastructure costs by picking the correct accelerator variant
- **Warm-Pool Bridging**: Optionally holds models loaded and asleep, and lends one to a variant while its own replica starts

## Installing

```bash
export NAMESPACE=<the namespace running llm-d>

make check-prereqs                    # read-only: tools, namespace, Prometheus
make setup-prereqs                    # ONCE per namespace, by a cluster admin
make deploy-wva                       # the controller — no cluster-scoped rights
make scaledobjects-plan               # list your model servers; nothing is applied
make scaledobjects-apply              # register them — this is what makes WVA scale
```

The install is split across two people, because it is split across two levels of
permission: a cluster admin runs `setup-prereqs` once for the namespace, and the
namespace's owner does everything after it, including every later upgrade. If you
are both, `make deploy-wva` does the two in one command.

Prometheus and KEDA are found on the cluster, or installed if it has neither.

The last two steps are not optional. A **KEDA ScaledObject** is how a workload
registers with WVA: the controller has no watch and no listing, so until one exists
it is running and idle.

| Then | |
| --- | --- |
| Full install guide | [deploy/](deploy/) |
| Installing WVA | [docs/guides/](docs/guides/) — pick a path |
| Running it day to day | [operations.md](docs/reference/operations.md) |
| Watching what it decides | [monitoring.md](docs/reference/monitoring.md) |
| Making a workload scalable | [workload-preparation.md](docs/reference/workload-preparation.md) |
| Bridging a slow scale-up | [warm-pool/](docs/guides/warm-pool/) — opt-in; a pool holds accelerators |

## Documentation

See the [architecture and autoscaling design](https://llm-d.ai/docs/architecture/advanced/autoscaling) docs for high-level algorithm details.

See the [docs](docs/) directory for design docs, developer guide, and more.

## How It Works

**Prerequisites:** deploy llm-d infrastructure (model servers), have Prometheus
scraping them, and create a **KEDA ScaledObject** per workload whose trigger points
at WVA's external scaler.

**WVA then:**

1. Learns which workloads it manages **from the KEDA calls themselves** — there is
   no watch, no listing and no opt-in annotation. Being called is being managed,
   and the trigger `metadata` is the per-workload configuration.
2. Continuously reads request rates and server performance from Prometheus.
3. Runs its capacity model — KV-cache utilization, queue depth, token throughput —
   to decide the replica count each model needs, across all its variants at once
   and within the GPU budget any declared limiter allows.
4. Returns that decision to KEDA over the external-scaler gRPC contract. KEDA owns
   the HPA and actuates it; WVA never writes the scale subresource.

## Example

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: llama-8b-scaler
  namespace: llm-inference
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: llama-8b
  pollingInterval: 5
  minReplicaCount: 1        # 0 to allow scale-to-zero (alpha)
  maxReplicaCount: 10
  triggers:
  - type: external-push     # push: WVA wakes a parked workload immediately
    name: wva-external-scaler
    metadata:
      scalerAddress: wva-external-scaler.workload-variant-autoscaler-system.svc.cluster.local:9090
      modelID: meta/llama-3-8b   # required — the only field you must supply
      scalingPolicy: interactive # optional — a named policy tier
      variantCost: "10.0"        # optional — defaults to "10.0"
```

`modelID` is the one required field. The accelerator, the role, GPUs per replica
and the InferencePool are all **derived** from the workload itself, so they cannot
drift from reality.

You do not have to write this by hand: `make scaledobjects-plan` reads `modelID`
off each container's `--served-model-name`, which is the one field nothing else
can check for you — a `modelID` that does not match what the container serves
groups the workload with a model it does not run, and mis-scales both. See
[the guides](docs/guides/).

More examples in [config/samples/keda/](config/samples/keda/).

## Contributing

We welcome contributions! See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

Join the [llm-d autoscaling community meetings](https://llm-d.ai/slack) to get involved.

## License

Apache 2.0 - see [LICENSE](LICENSE) for details.


## Related Projects

- [llm-d workload-variant-autoscaler](https://github.com/llm-d/llm-d-workload-variant-autoscaler) - the upstream project this fork derives from
- [llm-d infrastructure](https://github.com/llm-d/llm-d-infra)
- [llm-d main repository](https://github.com/llm-d/llm-d)

## References
- [WVA paper](https://arxiv.org/abs/2603.09730)
- [WVA use case doc](https://docs.google.com/document/d/1ZcMXO0x42qn4X5cu6efgMomYC4pKPwm6r7L79y1AQH4/edit?tab=t.0)
- [Saturation based design discussion](https://docs.google.com/document/d/1iGHqdxRUDpiKwtJFr5tMCKM7RF6fbTfZBL7BTn6UkwA/edit?tab=t.0#heading=h.mdte0lq44ul4)