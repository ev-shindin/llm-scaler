# Warm pool

A small population of GPU-**holding** Pods that keep several models resident but
asleep, so capacity for any of them arrives in seconds instead of ~41 s.

Design: [`docs/proposals/fast-model-loading.md`](../docs/proposals/fast-model-loading.md)
and [the implementation design](../docs/proposals/fast-model-loading-implementation.md).

**Status: built, and running on a test cluster.** A pool Pod holding two models
has served real EPP traffic on pokprod, switching between them in ~437 ms. It is
off by default in the controller (`--warm-pool-namespace`), and the proxy image
in `config/warmpool` is still a personal-namespace build -- see the note there
before deploying anywhere that matters.

## Why this exists rather than using Fast Model Actuation

FMA solves a neighbouring problem and most of it is genuinely reusable — the
launcher here is FMA's, copied rather than rewritten. One thing is not reusable,
and it is the thing that decides whether a warm pool works:

**In FMA the launcher requests no GPU; the *requester* holds it.** When the
requester goes away the device allocation is released while the sleeping engine's
`cumem` handles still refer to memory on that device, which another tenant then
takes. Measured on pokprod: two of three unbound sleepers **could not be woken at
all** —

```
pfw8b: HTTP 500  CUDA error: invalid argument   cumem_allocator.cpp:145
v6fdd: HTTP 500  CUDA error: out of memory      cumem_allocator.cpp:139
```

A warm copy that does not own its device is not slower, it is unreliable. So here
**the pool Pod holds the GPUs for as long as it holds models**, which is the one
structural change from FMA and the reason this is a separate component rather
than a fork.

The second reason is scope: FMA's Kubernetes half (dual-pods controller,
launcher-populator, `LauncherPopulationPolicy`, the requester/SPI/proxy path)
exists to pair transient requester Pods with launchers. A pool needs none of it —
WVA already runs a control loop, knows the InferencePools, and decides what
should be warm.

## What is copied, and from where

Copied from `ev-shindin/llm-d-fast-model-actuation`, branch `feat/reuse-by-model`,
commit **`aa072ef`** (upstream `2c01cf8` plus the port-conflict reclaim fix):

| file | origin | why it earns its place |
| --- | --- | --- |
| `supervisor/launcher.py` | `inference_server/launcher/launcher.py` | spawns and supervises vLLM engines inside one container. Proven: every 2-3 s wake measured on pokprod came from an instance it created |

> **Vendored, not maintained here.** These files are a copy taken at fork commit
> `aa072ef`; edit them upstream and re-copy rather than in place. Note also that
> the image the pool actually runs (pinned by digest in
> `config/warmpool/warmpool-deployment.yaml`, currently `launcher:v0.6.4`) is a
> DIFFERENT build from this copy -- the copy is here to be read, not to be what
> executes, and nothing checks that the two stay in step. The
> tests under `supervisor/tests/` came with the source and are not wired into any
> make target or CI job.
| `supervisor/gputranslator.py` | `inference_server/launcher/gputranslator.py` | GPU UUID ↔ index mapping via pynvml, with a mock mode for tests |
| `supervisor/tests/` | the same tree | the evidence the above works; kept so adaptation is not blind |

## The split of responsibilities

The launcher has **no sleep or wake endpoint**, and that is correct rather than
missing: in FMA the *controller* calls vLLM's own HTTP API directly
(`inference-server.go` calls `/wake_up`, `/sleep` and `/is_sleeping` on the
instance). WVA takes that role.

| responsibility | where |
| --- | --- |
| spawn / list / delete engines in a Pod | supervisor (this directory) |
| `/sleep`, `/wake_up`, `/is_sleeping` | **WVA**, over HTTP to the engine's port |
| which models are warm, which wakes, which is evicted | **WVA** — it is an allocation problem, which is what WVA is for |
| joining a woken model to its InferencePool | **WVA** — label membership; readiness comes from an ordinary probe against the proxy's `/readyz`, not a Pod readiness gate |

No policy lives in the Pod. That is deliberate: the cache policy is the part most
likely to change, and it must be changeable without touching the data plane.

## What had to change, and where it changed

All four are done. Note where: three of them turned out to be decisions WVA
makes when CALLING the launcher, not modifications to the launcher itself, which
is why the copy here runs unmodified.

1. **The Pod holds the GPUs.** Done in the manifest. The pool Deployment requests `nvidia.com/gpu`, and
   the supervisor allocates the container's own devices among instances rather
   than being handed a device by a requester.
2. **Instance identity keyed by model**, not by a hash over GPU UUIDs. Done in
   WVA (`pool.InstanceID`): the launcher takes the ID from its caller. The hash is
   correct *for FMA* — `CUDA_VISIBLE_DEVICES` is fixed at process start, so a
   sleeper is not portable between GPUs — but our reuse question is "is this model
   resident in this Pod", which the model name answers.
3. **Ports assigned from a local range**, not derived from an
   InferenceServerConfig. Done in WVA (`pool.freePort`): the launcher takes the
   port from its caller too.
   FMA's ISC-derived port is what made two instances of one model collide, and the
   port-conflict fix in `aa072ef` is a workaround for it.
4. **Drop the launcher-notifier sidecar** — never copied, so nothing to do:
   it maintains the `dual-pods.llm-d.ai/sleeping` label for the dual-pods
   controller. WVA reads instance state from the supervisor instead, which the
   measurements showed is the reliable source — the Pod label is per-Pod and flips
   for reasons unrelated to whether a given model is asleep.
