# Fast Model Actuation: what was tried, what was measured, why it was dropped

**Status: closed.** This replaces eight FMA documents removed on 2026-08-27
(`docs/guides/fma/`, and seven `docs/proposals/fma-*.md`). They are in git
history if the detail is ever wanted; this records what was learned so nobody
re-runs the experiments.

**Read this before proposing anything FMA-shaped, and before deleting anything
FMA-named — the warm pool that replaced it still runs FMA's launcher.**

## What FMA is

Fast Model Actuation splits a model server across two Pods. A **requester**
Deployment carries the llm-d identity and is what a scaler moves; **launcher**
Pods, owned by a `LauncherConfig`, hold the GPU and run vLLM. The launcher keeps
an engine resident so a requester can bind to it in seconds instead of paying a
cold model load.

The promise was a scale-up that serves in ~3 s instead of ~41 s.

## Why it was dropped

Measured on pokprod over 2026-08-18/19, and the failures were structural rather
than incidental:

- **Placement is the bug, not GPU alignment.** A warm bind is node-local. Pinning
  launchers without also pinning requesters made every scale-up a cold load
  (~80–90 s), because the requester landed on a node with no launcher. Adding
  `warmAffinity` fixed placement — 3/3 correct — and still produced **0 wakes and
  3 rebuilds**.
- **Unbound sleepers often cannot wake at all.** They fail with cumem OOM or
  "invalid argument". Holding the GPU is what makes a wake possible, which
  undercuts the premise that launchers can be parked cheaply and bound on demand.
- **A pool spans nodes OR wakes reliably — not both**, without reuse-by-model
  inside FMA itself. That needed an upstream change we did not control.

A fork was started (`ev-shindin/llm-d-fast-model-actuation`, branch
`feat/reuse-by-model`); its first commit fixed a port-conflict reclaim and was
validated on pokprod (0 destroy events, first `warmAffinity` wake at 2 s). It was
not carried further.

`fma-upstream-requests.md` gathered change requests against FMA
`v0.6.0-alpha.13`. **None were tracked** — the document itself recorded that
nothing was filed, accepted, or rejected. Treat them as unraised.

## What replaced it

`internal/warmpool` — a pool of Pods holding engines already loaded on a GPU,
using vLLM sleep mode rather than FMA's bind/unbind. It is built, tested and
cluster-verified: **~437 ms model switch against ~41 s cold** on pokprod.

The design that shipped is
[`fast-model-loading.md`](fast-model-loading.md) (the argument) and
[`fast-model-loading-implementation.md`](fast-model-loading-implementation.md)
(what was built). Neither is FMA-derived.

Why it works where FMA did not: the pool Pod owns its GPU for its whole life, so
there is no bind step to place wrongly and no unbound sleeper to fail to wake.
The two failure modes above cannot occur.

## What is still FMA in this repo, and must not be swept up

Dropping FMA as an *actuation strategy* did not remove FMA from the tree. Four
things are live:

1. **The warm pool's supervisor container is FMA's launcher image** —
   `ghcr.io/llm-d-incubation/llm-d-fast-model-actuation/launcher:v0.6.0-alpha.13`
   in `config/warmpool/warmpool-deployment.yaml`. The pool drives it over the
   launcher's own `/v2/vllm/instances` API. **This is a hard dependency of the
   shipping feature.**

   It is not merely a supervisor: `launcher.py` does
   `from vllm.entrypoints.openai.api_server import run_server`, so the image is a
   full vLLM runtime with the launcher on top. The pool needs both halves — vLLM
   because a warm copy IS an engine loaded on this Pod's GPU, and the launcher
   because its API is how the controller creates, lists and removes a resident
   model. This repo builds neither: it publishes the warm-pool PROXY and pulls
   the launcher. A mirrored or air-gapped registry has to carry
   `ghcr.io/llm-d-incubation` for the pool to start at all.

   The source is vendored at `warmpool/supervisor/` to be READ, not shipped —
   nothing checks it against the tag actually pulled, so it can quietly become a
   different version from the one running.
2. **Launcher attribution** in `internal/collector/locator` — pairing a launcher
   Pod to its requester's scaler, so a launcher's metrics are charged to the
   right variant instead of dropped. Roughly eight tests cover it.
3. **`config/fma-launcher-metrics/`** — the PodMonitor that scrapes launchers.
   Without it launcher metrics are not collected at all, and the blindness is
   silent: `wva_pod_mapping_miss_total` never increments, because no launcher Pod
   ever reaches attribution to be rejected by it.
4. **`test/e2e/fma_attribution_test.go` and `fma_parking_test.go`**, plus
   `test/e2e/fixtures/fma_layout_builder.go`.

Removing any of these is a behaviour change, not a documentation change, and
needs its own decision.

## Traps worth keeping

- **WVA cannot measure an FMA variant by default.** A launcher's owner is a
  `LauncherConfig`, not a Deployment or LWS under a ScaledObject, so the
  ownerReference walk ends with nothing and the Pod is dropped. That is what the
  locator pairing exists to fix.
- **The PodMonitor-overwrite trap.** A second PodMonitor selecting the same Pods
  hid launcher metrics entirely on pokprod. Symptom: the series simply do not
  exist, with nothing reporting an error.
- **GPU quota does not bound FMA's real consumption**, because a `LauncherConfig`
  does not express a warm pool. When planning capacity in an FMA namespace,
  subtract the launcher pool by hand — see
  [`../deployment/gpu-limiter.md`](../deployment/gpu-limiter.md).
