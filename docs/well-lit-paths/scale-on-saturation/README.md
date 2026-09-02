# Scale a model on saturation

The default path, and the one every other path builds on. WVA reads what your
inference engines are actually doing — KV-cache occupancy, queues, token rates —
turns it into a supply-and-demand statement per model, and answers KEDA with a
replica count. KEDA actuates it.

**Use it when** a model's load varies and a hand-set replica count is either
wasting accelerators at the trough or missing SLO at the peak, and when the
thing that saturates first is the engine.

**Do not use it when** the bottleneck is somewhere WVA cannot see — a gateway,
a tokenizer service, the network — or when nothing scrapes vLLM/SGLang and EPP
metrics into Prometheus. WVA has no opinion it did not read from a metric.

## What it needs

- KEDA on the cluster, and Prometheus scraping the model servers and the EPP.
- A KEDA `ScaledObject` per workload. This is not optional: the controller has
  no watch and no listing, so until a ScaledObject exists it is running and idle.
- Nothing else. WVA holds no accelerators of its own.

## Setting it up

[Install WVA in a namespace](../../guides/install-in-namespace/) covers the
whole sequence. The two steps that matter here are the last two:

```bash
make scaledobjects-plan     # lists your model servers; applies nothing
make scaledobjects-apply    # registers them — this is what makes WVA scale
```

`scaledobjects-plan` writes an editable plan first, so you see what would be
autoscaled before anything is.

## Verifying it

Three things, in this order:

```bash
# 1. WVA is deciding at all: a target per model, per cycle
wva_desired_replicas{exported_namespace="<your-namespace>"}

# 2. It decided for a REASON: supply, demand and the threshold it compared them to
kubectl logs -n <wva-namespace> deploy/<wva> | grep analyzer-result

# 3. KEDA acted on it
kubectl get scaledobject,hpa -n <your-namespace>
```

The log line's fields are described in [the cycle log](../../reference/cycle-log.md);
`exported_namespace` is the workload's namespace and plain `namespace` is the
controller's, which is [why alerts group by the former](../../reference/monitoring.md).

## What it costs

No accelerators. The controller is a small deployment, and the recurring cost is
the Prometheus queries it makes each cycle — one set per namespace, not per pod.

## How it is tested

- End-to-end: `test/e2e/saturation_v2_test.go`,
  `saturation_analyzer_path_test.go`, `saturation_config_test.go`,
  `external_scaler_keda_test.go`, `smoke_keda_test.go`.
- Benchmark scenario: `hack/benchmark/scenarios/guides/workload-autoscaling.yaml`,
  driven by [Benchmark WVA](../../guides/benchmarking/).
- The recorded numbers in
  [benchmark-results.md](../../developer-guide/benchmark-results.md) were
  measured on WVA v0.6.0's earlier saturation engine. They are a historical
  baseline; they do not describe this engine.

## Tuning it

Thresholds, analyzer selection and policy tiers are in the
[scaling policy reference](../../reference/scaling-policy.md). What the engine
measures, and how a measurement becomes a replica count, is in
[the steady-state engine](../../concepts/steady-state-engine.md).
