# Engine structure: scope, layering, and what to do before the next feature

**Status:** proposal, 2026-09-21, revised after review. Decisions in this
document are the repository owner's; each one is marked **Decision** where
the text asks for it. A prerequisite for the shape-shift proposal
([PR #86](https://github.com/ev-shindin/llm-scaler/pull/86)), whose items 1
and 2 land in the packages this document creates. It continues, and must
stay consistent with, the plan already on `main`:
[`docs/plans/analyzers/analyzer-architecture-refactor.md`](../plans/analyzers/analyzer-architecture-refactor.md),
whose phases 1-3 (discovery metadata, the capacity builder overlay,
analyzers emitting pure `(D, P)`) are done.

## What the code looks like today

Measured on `main` at `2a4270e7` (after #85; `internal/`, non-test lines of
the package's own directory):

| package | src lines | files | what it holds |
|---|---|---|---|
| `engines/steadystate` | 4,148 | 6 | `engine.go` (2,158) + `engine_v2.go` (1,358): the reconcile driver, and with it the analyzer runner, the capacity builder, the optimizer caller, the sticky scale-down, the inventory gate, the blocked-reason metric, the policy report, the decision writer and the actuation call. Imports **24** internal packages directly (29 transitively). |
| `engines/allocation` | 4,464 (+884 in `multi_backup`) | 17 | two optimizers, five limiters and their factory, quota and Kueue inventories, GPU budgets, rescale, and `analyzer_helpers.go`; imports 11 internal packages (config, kueue, gpunodes, accelerator, metrics among them) |
| `engines/analyzers/saturation_v2` | 3,336 | 8 | `analyzer.go` (1,663): k1/k2 capacity learning, occupancy demand, scheduler-queue attribution, prefill holds, the throughput windows; `throughput_floor.go` (675): the backlog-by-throughput floor and its hold/order rules. Imports `engines/allocation` for two string constants, `allocation.ReasonError` and `allocation.ReasonNoData`. |
| `engines/analyzers/throughput` | 1,555 | 7 | a second analyzer with its own shape tracker, observation window, ITL model, sanity report and queue pricing |
| `engines/scalefromzero` | 2,274 | 6 | a second engine (scale-from-zero), 20 internal imports including `allocation`, `decision`, `registry`, `datastore` |
| `collector` (+ 5 sub-packages) | 2,177 (+2,812) | 5 | `replica_metrics.go` (1,508): queries, per-pod collapse, attribution, freshness, in one file; one call into `decision` (`decision.BridgeVariant`) |
| `warmpool` (+ 4 sub-packages) | 3,036 (+4,294) | 8 | `reconciler.go` (1,492), `pool/adapter.go` (1,129), `policy/policy.go` (1,099) |
| `controller` | 1,167 | 8 | the reconcilers; wired from `cmd/main.go`, which itself imports 23 internal packages |
| `config` | 3,067 | 10 | |
| `metrics` | 1,504 | 1 | every gauge and counter of the controller in one file |
| `registry` | 974 | 3 | imported by `cmd`, `collector/locator`, `scalefromzero`, `steadystate`, `scaler`, `utils`, `warmpool` |
| `scaler` | 761 | 4 | the KEDA external scaler; imports `decision`, `registry` |
| `decision` | 1,350 | 10 | a leaf: no internal imports; imported by nine packages (`actuator`, `collector`, `allocation`, `scalefromzero`, `steadystate`, `gpuusage`, `scaler`, `warmpool`, `cmd`) |

Four structural problems, each with a cost that has already been paid:

1. **The engine is everything.** `steadystate` is the only place the
   pipeline is visible, and it is 3,500 lines across two files whose split
   (`engine.go` / `engine_v2.go`) is historical, not by concern. A reader
   asking "where is the decision made" reads both. Every policy added this
   quarter -- sticky scale-down, the conceded-supply gate, holds -- went in
   here because there was nowhere else.
2. **Two analyzers, one problem, two vocabularies.** `saturation_v2` and
   `throughput` each implement a workload shape, a per-variant state, a
   rolling window, a throughput model and a queue pricing. The three
   shape-switch defects of 2026-09-20 (#84, #85, the drain burst) are all in
   `saturation_v2`'s throughput model -- a max-window of requests per
   second under output-length buckets -- while `throughput` next to it
   carries a model that has none of those failure modes (below). Nobody
   chose one; each was extended where the last fix landed.
3. **Layering is not a rule.** An analyzer imports the optimizer package
   (`saturation_v2` → `allocation`, for two reason strings) -- the one
   import that runs against the order below; the collector reads a decision
   output (the warm pool's lending) straight from the decision store to
   attribute Pods -- downward as an import, a cycle as a pipeline; and the
   engine imports the collector, every analyzer and the optimizers at once.
   The dependency direction is not a rule anyone can point to.
4. **The comments are histories, not contracts.** `throughput_floor.go`
   opens with 80 lines on what was measured on which pass and why each
   earlier variant was dropped. That is valuable -- it is the only record
   of why -- but it is in the wrong place: a reader who needs the function's
   contract has to find it inside a narrative, and the narrative goes stale
   the moment the next pass contradicts it (it already has, three times).

## The target

One direction of dependency, one concern per package, the pipeline
readable in one file. Two orders matter and they are not the same:

**Pipeline order** (what a cycle does):

```
collect → analyze → plan → policy → decide → actuate
```

**Dependency order** (who may import whom; a package imports only what is
below it):

```
engine/            orchestration only: the pipeline above, in one file, no arithmetic;
                   also the home of the second driver (scale-from-zero), which
                   shares plan/ and policy/; nothing imports either driver
policy/            what happens between a plan and an order: sticky scale-down,
                   stabilization, the conceded-supply gate, inventory gate, blocked reasons
plan/optimize/     cost-aware, greedy: demand → replica counts under a budget
plan/limit/        quota, Kueue, GPU budget, composite: the budget
analyze/           demand estimation: signals → per-role demand and supply, with the
                   holds that belong to a measurement; the combine; the registry;
                   the external (PromQL) wrapper
signals/           stateless functions and small trackers over ReplicaMetrics: shape,
                   capacity (k1/k2 and its store), throughput models, windows, backlog,
                   and today's engines/aggregation
collect/  actuate/ one layer, two concerns: collect/ is scrapes → domain.ReplicaMetrics
                   (sources, locator, attribution, freshness); actuate/ is actuator +
                   scaler + registry (what talks to the cluster and to KEDA); siblings
                   may import each other (collector/locator reads the registry)
decision/          the store, as today
domain/            the types
infrastructure     config, constants, metrics, logging, prometheus, accelerator,
                   gpunodes, kueue, inferenceengine, variant, datastore, utils/*:
                   leaves, importable from anywhere; they import each other, domain,
                   constants and logging -- with two edges into the pipeline that exist
                   today and are to be removed: utils → registry, datastore → collector/source
```

Today's helper packages take these places: `engines/aggregation` and
`engines/common` are `signals`; `engines/executor` and `engines/variantmeta`
are `analyze`; `gpuusage` (publishes GPU usage from the decision store,
imports `decision` and `gpunodes`, nothing in the pipeline imports it) is a
reporter that sits beside `engine/`, like `warmpool/`. `controller/` and
`cmd/main.go` sit above `engine/` (they wire it). `warmpool/` is a separate
subsystem that imports `decision`, `registry`, `datastore` and `metrics` and
nothing in the pipeline imports it, so it sits beside `engine/` and the rule
holds for it as-is.

Rules that make it stay this way:

- **Imports point down the dependency order.** The check
  (`hack/check-import-direction.sh`, over each package's direct imports from
  `go list`; landed in PR #88, run by `make test`) enforces the relative
  order among the named pipeline packages -- `engine`, `policy`, `plan/*`,
  `analyze`, `signals`, `collect`/`actuate`, `decision`, `domain` -- and
  lets any of them import the infrastructure leaves. It does not claim
  `plan` imports only `domain` and `decision`: `allocation` today
  legitimately reads `config`, `kueue`, `gpunodes`, `accelerator` and
  `metrics`, and will keep doing so. What it forbids is the one upward edge
  that existed on `main` (`saturation_v2` → `allocation`, reported by the
  check there and gone in #88) and any new one. The collector's read of
  the decision store is downward by this order and was removed in #88 for
  the other reason: collecting must not read a decision output directly.
- **A file is one concern and under ~500 lines.** `engine.go` at 2,158 and
  `analyzer.go` at 1,663 are the symptoms; the rule is what stops them
  growing back.
- **The contract at the top, the history in `docs/developer-guide/`.** A
  function's comment says what it computes, what it must never do, and
  points at the measurement that justifies the rule. The narratives move to
  a new `docs/developer-guide/analyzer-evidence.md` (the developer guide is
  where AGENTS.md puts component internals), dated, per pass.
- **One workload shape, one throughput model**, in `signals`, used by every
  analyzer and every policy. The two window types are *not* unified: the
  saturation window (`rollingAverage`: count-bounded, keeps a max,
  `RaiseLast`/`Touch`/`Stale` on wall-clock time) and the throughput
  window (`ObservationWindow`: count- and age-bounded `(k, ITL, t)` samples
  with injected time, cleared on shape change) have different semantics,
  and a merge would change one of them. They move as they are.

## Borrow from the throughput analyzer -- in both directions

`engines/analyzers/throughput` already has the representation the
shape-shift proposal asks for:

| concept | `throughput` (TA) | `saturation_v2` (V2) | keep |
|---|---|---|---|
| workload shape | `ShapeTracker`: `(IL, OL, hit rate)` per variant, a change declared when either moves past a tolerance; the observation window is cleared on a change, the hardware baseline is kept | six output-length buckets, per replica (per role since #85), no `IL` | **TA's**, made per role and fed the early signals (arriving `IL` from EPP queue bytes) |
| throughput model | `ITL(k) = A·k + B`, `k` = KV utilization: Tier-1 OLS over the window, Tier-2 pins `B` (hardware) when the fit cannot be trusted; `μ_dec(k*)` in tokens/s; verified against the observed generation-token rate (GPS mismatch clears the window) | max-window of saturated **requests/s** per bucket; borrow the nearest bucket when empty | **TA's**: continuous in load, no buckets, no borrowing, and a drain does not burst tokens/s |
| demand | `λ × OL` in tokens/s against `μ_dec` supply | resident KV + queue residency, then a floor of `(λ + B/T)/μ` in requests | **both**: occupancy (V2) is the right measure while the fleet holds; the token-rate demand (TA) is the right one while it does not. V2's floor becomes TA's demand expressed through the shared model |
| scheduler queue | `QueueSize / (drainFactor × ITL(k_sat))`: work to drain, in tokens | `B / T` in requests, since #70; residency before it | one function in `signals/backlog`, token-denominated |
| P/D roles | splits queue demand across non-prefill roles | prefill holds under decode saturation (#76), gateway queue attributed to decode | **V2's** role logic, generalised by the arriving-`IL` discriminator |
| capacity | -- | k1 (memory) / k2 (compute) learning, history, eviction | **V2's** |
| trust rules | sanity report, GPS mismatch counter | borrowed / single-sample holds, `MinThroughputSamplesToOrder`, fold-and-spacing on samples | both, as `analyze`'s hold reasons on one shape-change event |

So the answer to "should V2 borrow (a neighbouring bucket)" is no: with a
model continuous in load there is nothing to borrow between; shape enters
through `O` (tokens per request) and through `k` (KV per request), both of
which the fleet's shape gives directly. What V2 should take from TA is
the shape tracker and the ITL model; what TA should take from V2 is the
capacity learning, the role attribution and the hold discipline. The
result is one analyzer; the second becomes a configuration of the first
(occupancy-led or throughput-led demand), not a parallel implementation.

## Analyzers as plugins: built-in, external, and the next one

The plugin boundary already exists -- `domain.Analyzer` (`Name`, `Analyze(ctx,
AnalyzerInput) → AnalyzerResult`) -- and `engines/analyzers/external` shows
it works: a config-driven PromQL definition (a demand query `D`, a per-replica
target `P`, per engine) becomes an analyzer without Go. Since the refactor
plan's phase 3, every analyzer emits pure `(D, P)` and the capacity builder
overlays identity (cost, accelerator, role) from discovery. Three things in
the current layout still undercut the boundary, and the structure above
fixes each:

| today | why it hurts a plugin | in the target |
|---|---|---|
| the saturation analyzer is privileged: "always run, needed for PerReplicaCapacity"; other analyzers' results are scored against its capacity | a new analyzer cannot express its demand against the fleet's capacity without running V2 first; an external `(D, P)` uses a constant `P` because the learned one is V2's private state | **capacity is a signal, not an analyzer's output**: `signals/capacity` (k1/k2, the store, eviction) is computed by the engine's input builder and handed to *every* analyzer in `AnalyzerInput.Capacity`. V2 stops being special; an external analyzer may price `D` against the learned `P` or its own constant |
| two registries (`analyzersSnapshot` frozen at `StartOptimizeLoop`, `externalAnalyzers` reconciled from config under a lock) with a name-collision rule in `engine.go` | the order and the collision rule are engine internals; a Go plugin registered at runtime is "external" by accident of which map it lands in | **one registry** (`analyze/registry`): name → `domain.Analyzer` + its `AnalyzerConfig`; built-ins register at init, config-driven ones on reconcile, a Go plugin through the same `Register`. **Decision:** the lock-free frozen snapshot and the "register before Start" contract are dropped on purpose -- every analyzer takes the reconciled, locked path the external ones already take (a read lock per cycle) -- and the collision rule that survives is "a built-in name is reserved; a later registration of it is an error", as today |
| the capacity builder joins discovery metadata onto results *after* `Analyze` (`buildCapacities`, phase 3 of the refactor plan) | a result is not complete on its own; a remote analyzer could not be asked for one | **the builder runs before, not after**: `engine` builds one complete `AnalyzerInput` per model per cycle (metrics, variant states and metadata, scheduler queue, arrival rate, capacity, fleet shape) and every analyzer returns a complete `AnalyzerResult`. The input and result are the API. This is the same information the phase-3 overlay carries, moved from after the call to before it |
| holds are inside V2's numbers (the floor's cap, the prefill hold) | another analyzer has no way to say "hold, do not order" -- it can only emit a smaller number, which the combiner cannot tell from a measurement | `AnalyzerResult` carries holds per role (`Role, Reason, Cap`; in the composite branch's vocabulary, a per-SO decision path); `policy` applies them uniformly, and the log says whose hold it was |
| combining is spread over `engine_v2.go` and `allocation.NamedAnalyzerResult` scoring | the rule for two analyzers disagreeing is not written down anywhere a plugin author can read | `analyze/combine`: one documented rule, one place -- the composite branch below |

**The grain is per role.** The finest-grain item is the ScaledObject (a
variant), and on a P/D fleet every variant carries a role: per-replica
capacity is per SO, demand is per (model, role) -- the request stream is
per model and every request passes through both roles, so each role must
keep up with all of it -- and a non-disaggregated fleet is the one-role
case (`both`), not another contract. An analyzer states a signal for each
role it models and **marks the roles it does not** (`ReasonRoleUnmodeled`
upstream, the `(value, ok)` convention on the composite branch): the
throughput analyzer has no prefill model, its unmarked zero read as
"prefill needs nothing", and the prefill fleet drained. Shape, holds and
the combine are per role for the same reason -- an `I`-up shift is
prefill's event, an `O`-up shift decode's, a hold on decode must not
freeze prefill, and a role with an empty ballot is "no basis to act" for
that role only; the model-level coverage rule
(`min(cov(prefill), cov(decode)) + cov(both)`) is what ties the roles back
together for the fleet-level view.

**Decision -- who owns the identity fields.** The composite branch's
redesign record (`.session/composite-signal-redesign.md` §6, D1) makes
saturation the unconditional source of the non-`(D, P)` fields on the
result, "read from sat today only because that is where the data currently
lives" -- "in an ideal world it would be a separate, non-per-analyzer
computation". The refactor plan already assigns identity to discovery
(§3.1: `VariantMetadata` "is the authoritative per-variant identity + state
for one cycle"; §3.2: cost, accelerator name and the rest "leave the
result"), with one recorded deviation: replica counts stay
analyzer-measured ("measured this cycle, not a lagging status") and the
builder caps them at discovery's `CurrentReplicas`. This proposal keeps
that deviation and completes the rest: the input builder owns cost,
accelerator, role, GPUs per replica and pending from discovery; the ready
count a replica row proves is a measurement and stays the collector's, on
the input; no analyzer -- saturation included -- is the source of any
identity field. D1 is the interim the composite branch had to live with;
stage 3 below retires it, and the composite is adjusted then.

What a new analyzer then has to do, and nothing else: implement `Name` and
`Analyze`, put demand and per-replica capacity in the **same unit** (so that
`replicas = D / P` -- tokens today; the unit is the result's contract, not
the engine's), per role, marking the roles it has no model for, state any
holds, and register. It may use `signals` (shape, windows, the ITL model,
backlog pricing) and it may ignore them; the engine computes nothing on its
behalf and special-cases nothing.

The external PromQL analyzer keeps working unchanged as one implementation
in `analyze/external`, catalog and all. An **out-of-process** analyzer --
the shape a KEDA-style scaler would take, or a model an operator keeps in
Python -- implements the same contract over gRPC: `AnalyzerInput` and
`AnalyzerResult` are the wire types (versioned, with the fields above), the
in-process wrapper is a `domain.Analyzer` that serialises and calls. That is
a later step and needs nothing from the pipeline that the in-process API
does not already give; it is the reason the input has to be complete
before `Analyze` rather than patched after.

## What the colleague's forks carry

Two forks, checked 2026-09-21 by symbol against our `main` (not by patch
identity, since upstream squash-merges).

### `deanlorenz/llm-d-workload-variant-autoscaler` -- a sibling fork of upstream

| branch | what it is | in ours? |
|---|---|---|
| `ta-anchor-dynamic-refresh` (33 commits over our merge-base, 2026-08-06..08; includes `ta-anchor-refactor-v2` = upstream PR #1516) | the multi-vote pipeline in `internal/engines/pipeline` (our `engines/allocation`): **one combine core** `combineVotes(votes, up) → (count, binder)` -- max for scale-up, min for scale-down, rounding once at the caller, the *binding analyzer* returned with the count; **abstain ≠ zero** -- a role an analyzer has no model for is tagged `ReasonRoleUnmodeled` and casts no vote; **score as a dominance correction** (`v* = e − Σ(e − v_i)(s_i − s_e)⁺ / Σs_j`); the **live-only veto gate** (`roleSpareVetoed`); **coverage per GPU freed** (`max_i PRC_i[v] / GPUsPerReplica[v]`) as the scale-down tie-break; a per-iteration anchor refresh; goldens and invariant specs | **no**: none of `combineVotes`, `ReasonRoleUnmodeled`, `bindingAnchor`, `roleSpareVetoed`, the coverage tie-break. We have the liveness half only (#1481, `f5261c8e`) |
| the same branch's `docs/developer-guide/multi-analyzer-pipeline.md` | our `main` already has this guide (562 lines); the branch's is +579/−58 against its own merge-base (a 475-line version) and +618/−184 against ours: the three ballot collectors, the veto gate, the abstention rule, liveness's three no-data cases | partly: the guide, yes; his update to it, no |
| `analyzer-metric-proposal` | the analyzer contract collapsed to `D` and `P` per finest-grain item; results as Prometheus metrics; external analyzers as PromQL with `match` per ScaledObject/role | **yes** (`docs/proposals/analyzer-metric-interface.md`, three lines behind his) |
| `ta3-e2e`, `ta-correctness-guards`, `ta-veto-liveness`, `ta-model-level-demand` | TA fixes | yes, through the upstream merges we adopted |
| `benchmark`, `autoscaling-viz` | a results tree with `postprocess.py` and a generated `REPORT.md`; a real-trace visualiser | no; superseded by the branches on the other fork below |

### `deanlorenz/llm-scaler` -- his fork of this repository (remote `deanscaler`)

Thirty branches; twelve are the install/preflight/dashboard PRs #15-#26
(merged, August) and #34 (the single `CompositeSignal` at the
engine→optimizer boundary, merged 2026-09-02). The rest is not PRed:

| branch | base | what it is | state |
|---|---|---|---|
| **`composite-analyzer`** (83 commits, authored 2026-09-08..16, rebased 09-16) | our `main` at `67229a01` (2026-09-11) | **the successor to #34** (its spec says so: "spinoff of `single-analyzer`, keeps PR #34's single-entry shape"), and the design the plugin section above is groping toward, with the owner's decisions marked in a mission spec (v8) and a redesign record. Every analyzer's contract is **two numbers per ScaledObject, `Demand` and `PRC`**; the composite is **`N(SO)`, replicas needed**, the max over *eligible* contributors of `ceil(Demand/PRC)`; **eligibility** = non-nil ∧ informative ∧ live, one rule in both directions; **saturation is not privileged** -- identity carrier (D1, see the decision above) and *fallback* contributor only when nothing else has a signal; a **decision path on the composite** (`C0-agree`, `C1-single`, `C2-sat-fallback`, `C3-default-prc` (unbuilt), `C4-no-signal`) mirroring the analyzers' own `Reason`; **no signal → do not autoscale** as an explicit gate; **coverage** = supply/demand per role, **undefined at zero** and carried as `(value, ok)` (`maxOfDefined`/`minOfDefined`); the cross-role rule above; a query API with **one rounding rule per concept**; score deferred -- pure max. ~3,000 lines of code and tests | implemented and verified on the branch; reviewed here on 2026-09-21 (below); no PR |
| `single-analyzer`, `single-analyzer-normalize` (Sep 6-7) | Aug/Sep `main` | the steps between #34 and `composite-analyzer` | superseded |
| `benchmark-runtools` (63), `benchmark-viz` (19), `benchmark-{init,plan,extract}`, `worktree-*` (Aug 21 - Sep 6) | Aug `main` | `hack/benchmark/`: `report.py`, `render_real_trace.py`, `extract_real_trace.py`, `dump_wva_decision_table.py`, `capture_wva_controller_log.sh`, `env_wizard.py`/`env_guard.py`, a GPU-reservation coupler, results bundles with a generated report per run, drafted observability-gap issues | working tooling with committed run bundles (large); no PR |
| `policy-writer`, `session-tracking`, `agentbus` | unrelated history | agent-workflow conventions, not product code | -- |

**The review of `composite-analyzer` against `main` (2026-09-21, test-merge
only).** The design is sound and it is one review short of mergeable. What
must change before it merges, in the order found: (1) **a regression of
#82** -- `computeCurrentGPUUsage[ByNamespace]` now charges a model's GPUs to
the quota only when `CompositeHasSignal`, so a running replica with no
per-variant decision path this cycle (startup, a scrape gap, every SO at
`C4-no-signal`) is not charged and the warm pool is told a GPU is free that
a model holds; `TestASteadyFleetStillPublishesHeadroom` passes on
`main` before #85 (`fe8340ca`) and fails on the branch merged onto today's
`main`. The quota charge is a fact about
`VariantStates.CurrentReplicas`, not about signal; (2) the saturation
fallback checks `Eligible(sat)` (whole result) and PRC > 0 but not the
per-SO `Reason` filter the contributor loop applies, so an SO with
`Reason: error` beside an informative one is priced from it --
contradicting the branch's own regression guard §2.9; (3) the eligibility
guard on that fallback has no test on the real `buildComposite` (the
decision-path specs run against a hand copy, `resolveSOForTest`); dropping
the guard is green. Plus the branch's own open items (eight spec fixes,
one a code change; a comparison of the pre-#34 aggregations for the
multi-analyzer case). The rebase is not small: `engine_v2.go` and
`cost_aware_optimizer.go` overlap #76/#82, with one content conflict.

This is what `analyze/combine` is. The upstream anchor branch keeps the
two things the composite spec deliberately deferred -- the score dominance
correction and the coverage-per-GPU-freed tie-break -- and is the reference
for those if they are ever wanted; the benchmark branches are the base for
item 3 of the shape-shift proposal (the scorecard): `report.py` and the
results bundle are the report per run, `dump_wva_decision_table.py` the
decision timeline; what they lack is the windowed TTFT and target-path
columns this week's runs used.

## The plan, in stages that each leave `main` green

Every stage is behaviour-preserving, verified by the existing suites, and
by the benchmark scorecard on the two shape-swap traces (runs 16 and 18 of
2026-09-20 are the fixed references: any stage that moves their windowed
p95 or target path has changed behaviour and stops).

1. **`signals`** -- extract, do not rewrite. Move `ShapeTracker`,
   `ObservationWindow` and `ITLModel` out of `throughput`, and
   `rollingAverage`, the capacity store (k1/k2, history, eviction) and the
   floor's arithmetic (`estimateThroughputDemand`, `medianFloat`,
   `throughputFloor` and its terms, with `ReplicaCapacity`, the per-replica
   record they take) out of `saturation_v2`, into `internal/signals` with
   their tests, as they are (the two windows stay two types, above).
   `engines/aggregation` **stays where it is** for now: the colleague's
   `composite-analyzer` branch adds three files to it, and moving the
   directory first would turn stage 4's rebase into a conflict; the check
   already ranks it at the signals layer, the moved code keeps importing it
   at its current path, and it moves under `signals` after stage 4.
   Analyzers keep working unchanged, importing the moved code.
2. **Cut the upward edge, and make the direction a rule** -- done in
   PR #88. `ReasonError` and `ReasonNoData` are `domain` constants,
   aliased under their old names in `allocation` (`ResultIsInformative`
   stays there: it takes the optimizer's `NamedAnalyzerResult`); the
   collector's `decision.BridgeVariant` call is replaced by an injected
   `BridgeResolver` that the engine wires to the decision store; the
   import-direction check runs as a `make test` prerequisite. Nothing else
   moved.
3. **Split the engine by concern**, and the analyzer surface with it.
   `sticky_scale_down.go`, `inventory_gate.go`, `scaling_blocked.go`,
   `policy_report.go` and the hold/gate logic now inside `engine.go` become
   `internal/policy`; `engine.go` + `engine_v2.go` become one `engine.go`
   that only sequences the steps; one registry; the input builder before
   `Analyze` (retiring the phase-3 overlay and the composite's D1);
   `scalefromzero` becomes the second driver beside it. The largest single
   diff of the plan, and pure movement except for the two decisions marked
   above.
4. **Review and merge `deanscaler/composite-analyzer`**, rebased on `main`,
   with the three fixes from the review above; then **`plan/optimize` and
   `plan/limit`** from `allocation`, the composite into `analyze/combine`,
   and `analyzer_helpers.go` to `analyze`.
5. **Collector by concern** -- done. `replica_metrics.go` keeps
   the type, the cycle and the sequencing; the 850-line collection became
   four phases in four files: `query.go` (fetch, merge, filter),
   `extract.go` (series → per-instance data), `attribute.go` (instance →
   the row the analyzers read, or why not), `freshness.go`; `pod_collapse.go`
   already held the per-Pod merge (the sub-packages were already right).
   Same statements, moved.
6. **Comment policy**, applied file by file as each is touched: contract
   at the top, the measured history moved to
   `docs/developer-guide/analyzer-evidence.md` with its date and pass.

Then, and only then, the shape-shift items: item 1 (the token-rate model)
is "make `analyze` use `signals/itl` for its floor" -- a small change once
stages 1-3 exist, and a large one before them; item 2 (the shape-change
event) is a `signals/shape` consumer in `analyze` and a hold in `policy`.

## What this is not

- Not a rewrite. Every stage moves code with its tests; the arithmetic
  does not change until the shape-shift items, which are separate PRs with
  their own measurements.
- Not the warm pool. **Decision:** `warmpool` has the same size problem
  (`reconciler.go` 1,492) and the same fix, but it is a separate subsystem
  with its own guide and nothing in the pipeline imports it; it follows the
  same rules on its own schedule, and this proposal does not touch it.
- Not a naming exercise. **Decision:** the package names above are the
  proposal; the owner may rename any of them at stage 1 without changing
  the plan. What matters is the direction rule and the one-concern rule,
  which the check enforces.
