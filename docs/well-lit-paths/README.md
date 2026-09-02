# Well-lit paths

A well-lit path is a recipe for one scenario that is **documented, tested and
benchmarked** — the term is llm-d's, and the bar is theirs. Each page below says
which end-to-end suites and which benchmark scenario back it, by filename, so
you can check the claim rather than take it.

Start from the problem you have.

| If this is your problem | Take this path | Status |
| --- | --- | --- |
| Replica counts are set by hand, or by a CPU threshold that has nothing to do with serving | [Scale a model on saturation](scale-on-saturation/) | Stable |
| A model sits idle for hours holding accelerators | [Scale to zero, and get back](scale-to-zero/) | Stable |
| Scale-ups are too slow: by the time a replica loads, the spike is over | [Bridge a scale-up with a warm pool](warm-pool-bridge/) | Stable |
| Several models are too large to start on demand, and there are not GPUs for all of them | [Hold several large models on one set of GPUs](retained-pool/) | **Experimental** |
| Autoscaling can ask for more GPUs than the cluster has | [Bound a fleet by real GPUs](bound-by-gpus/) | Stable |
| One model, two accelerator types, and you want the cost-efficient one first | [Serve one model on two accelerator variants](accelerator-variants/) | Stable |
| Prefill and decode have different shapes and you want them scaled apart | [Scale a P/D-disaggregated model](pd-disaggregation/) | **Experimental** |

Everything here assumes WVA is installed and a workload is registered. If it is
not, start at [Install WVA in a namespace](../guides/install-in-namespace/) —
the paths pick up after it.

## How these differ from the guides

A **guide** ([guides/](../guides/)) is the steps: run these commands, in this
order, and check this at the end. A **path** is the decision above the steps —
what the scenario buys you, what it costs, when not to take it, and what
evidence exists that it works. Paths link down into guides; they do not repeat
them.

## What "experimental" means

The path works and is exercised, but at least one of the three legs is missing
or moving: the recipe may change shape, the coverage may be narrower than the
scenario, or the defaults are not yet settled. Each experimental page says which
leg is short.
