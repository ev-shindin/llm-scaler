# Engine structure: scope, layering, and what to do before the next feature

**Status:** proposal, 2026-09-21. A prerequisite for
the shape-shift proposal ([PR #86](https://github.com/ev-shindin/llm-scaler/pull/86)), whose items 1 and 2
land in the packages this document creates.

## What the code looks like today

Measured on `main` at `fe8340ca` (`internal/`, non-test lines):

| package | src lines | files | what it holds |
|---|---|---|---|
| `engines/steadystate` | 4,148 | 6 | `engine.go` (2,158) + `engine_v2.go` (1,358): the reconcile driver, and with it the analyzer runner, the optimizer caller, the sticky scale-down, the inventory gate, the blocked-reason metric, the policy report, the decision writer and the actuation call. Imports **22** internal packages. |
| `engines/allocation` | 4,464 (+884) | 17 | two optimizers, five limiters and their factory, quota and Kueue inventories, GPU budgets, rescale, and `analyzer_helpers.go` |
| `engines/analyzers/saturation_v2` | 3,279 | 8 | `analyzer.go` (1,606): k1/k2 capacity learning, occupancy demand, scheduler-queue attribution, prefill holds, the throughput windows, plus `throughput_floor.go` (675): the backlog-by-throughput floor and its hold/order rules. Imports `engines/allocation` for two symbols. |
| `engines/analyzers/throughput` | 1,555 | 7 | a second analyzer with its own shape tracker, observation window, ITL model, sanity report and queue pricing |
| `collector` (+ 5 sub-packages) | 2,177 (+2,812) | 5 | `replica_metrics.go` (1,508): queries, per-pod collapse, attribution, freshness, in one file |
| `warmpool` (+ 4 sub-packages) | 3,036 (+4,294) | 8 | `reconciler.go` (1,492), `pool/adapter.go` (1,129), `policy/policy.go` (1,099) |
| `config` | 3,067 | 10 | |
| `metrics` | 1,504 | 1 | every gauge and counter of the controller in one file |
| `decision` | 1,350 | 10 | a leaf: no internal imports. The one package whose scope is clean. |

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
3. **Layering runs both ways.** An analyzer imports the optimizer package
   (`saturation_v2` → `allocation`, for `ReasonError` and
   `ResultIsInformative`); the collector imports `decision`; the engine
   imports the collector, every analyzer and the optimizers at once. The
   dependency direction is not a rule anyone can point to.
4. **The comments are histories, not contracts.** `throughput_floor.go`
   opens with 80 lines on what was measured on which pass and why each
   earlier variant was dropped. That is valuable -- it is the only record
   of why -- but it is in the wrong place: a reader who needs the function's
   contract has to find it inside a narrative, and the narrative goes stale
   the moment the next pass contradicts it (it already has, three times).

## The target

One direction of dependency, one concern per package, the pipeline
readable in one file.

```
internal/
  collect/     scrapes → domain.ReplicaMetrics; sources, locator, attribution, freshness
  signals/     stateless functions and small trackers over ReplicaMetrics:
                 shape (I, O, hit rate; change detection), occupancy (k1/k2),
                 throughput model (ITL(k), token rate, its verification), windows,
                 backlog (queue as work to drain)
  analyze/     demand estimation: turns signals into per-role demand and supply,
                 with the holds that belong to a measurement (borrowed, single-sample,
                 shape just changed). No optimizer, no decision store.
  plan/        optimize/  cost-aware, greedy: demand → replica counts under a budget
               limit/     quota, Kueue, GPU budget, composite: the budget
  policy/      what happens between a plan and an order: sticky scale-down,
                 stabilization, the conceded-supply gate, inventory gate, blocked reasons
  engine/      orchestration only: collect → analyze → plan → policy → decide → actuate,
                 one file, a few hundred lines, no arithmetic
  decision/    the store, as today (a leaf)
  actuate/     actuator + scaler
```

Rules that make it stay this way:

- **Imports point down the list.** `signals` imports `domain` only;
  `analyze` imports `signals` and `domain`; `plan` imports `domain` and
  `decision`; `policy` imports `plan` and `decision`; `engine` imports all
  of them and nothing imports `engine`. A test (`hack/check-import-direction.sh`,
  `go list -deps`) fails the build on an upward edge.
- **A file is one concern and under ~500 lines.** `engine.go` at 2,158 and
  `analyzer.go` at 1,606 are the symptoms; the rule is what stops them
  growing back.
- **The contract at the top, the history in `docs/developer-guide/`.** A
  function's comment says what it computes, what it must never do, and
  points at the measurement (`docs/developer-guide/analyzer-evidence.md#…`)
  that justifies the rule. The narratives move there, dated, per pass.
- **One workload shape, one throughput model, one window type**, in
  `signals`, used by every analyzer and every policy. Which brings in the
  throughput analyzer.

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
target `P`, per engine) becomes an analyzer without Go. Three things in the
current layout undercut it, and the structure above fixes each:

| today | why it hurts a plugin | in the target |
|---|---|---|
| the saturation analyzer is privileged: "always run, needed for PerReplicaCapacity"; other analyzers' results are scored against its capacity | a new analyzer cannot express its demand against the fleet's capacity without running V2 first; an external `(D, P)` uses a constant `P` because the learned one is V2's private state | **capacity is a signal, not an analyzer's output**: `signals/capacity` (k1/k2, the store, eviction) is computed by the engine's input builder and handed to *every* analyzer in `AnalyzerInput.Capacity`. V2 stops being special; an external analyzer may price `D` against the learned `P` or its own constant |
| two registries (`analyzersSnapshot` frozen at start, `externalAnalyzers` reconciled from config) with name-collision rules in `engine.go` | the order and the collision rule are engine internals; a Go plugin registered at runtime is "external" by accident of which map it lands in | **one registry** (`analyze/registry`): name → `domain.Analyzer` + its `AnalyzerConfig`; built-ins register at init, config-driven ones on reconcile, a Go plugin through the same `Register`. Deterministic order, one collision rule, in one file |
| the "capacity builder" joins discovery metadata (cost, accelerator) onto results after `Analyze` | a result is not complete on its own; a remote analyzer could not be asked for one | **the builder runs before, not after**: `engine` builds one complete `AnalyzerInput` per model per cycle (metrics, variant states and metadata, scheduler queue, arrival rate, capacity, fleet shape) and every analyzer returns a complete `AnalyzerResult`. The input and result are the API |
| holds are inside V2's numbers (the floor's cap, the prefill hold) | another analyzer has no way to say "hold, do not order" -- it can only emit a smaller number, which the combiner cannot tell from a measurement | `AnalyzerResult.Holds []Hold{Role, Reason, Cap}`: an analyzer states its holds; `policy` applies them uniformly, and the log says whose hold it was |
| combining is spread over `engine_v2.go` and `allocation.NamedAnalyzerResult` scoring | the rule for two analyzers disagreeing is not written down anywhere a plugin author can read | `analyze/combine`: one documented rule (today: the max of the demands per role, in the unit `P` is in), one place |

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

What a new analyzer then has to do, and nothing else: implement `Name` and
`Analyze`, put demand and per-replica capacity in the **same unit** (so that
`replicas = D / P` -- tokens today; the unit is the result's contract, not
the engine's), per role, marking the roles it has no model for, state any
holds, and register. It may use `signals` (shape,
windows, the ITL model, backlog pricing) and it may ignore them; the engine
computes nothing on its behalf and special-cases nothing.

The external PromQL analyzer keeps working unchanged as one implementation
in `analyze/external`, catalog and all. An **out-of-process** analyzer --
the shape a KEDA-style scaler would take, or a model an operator keeps in
Python -- implements the same contract over gRPC: `AnalyzerInput` and
`AnalyzerResult` are the wire types (versioned, with the fields above), the
in-process wrapper is a `domain.Analyzer` that serialises and calls. That is
a later step and needs nothing from the pipeline that the in-process API
does not already give; it is the reason the input has to be complete
before `Analyze` rather than patched after.

## What to take from the `deanlorenz` fork

The fork this repository descends from carries, on unmerged branches, the
most developed thinking on how analyzers combine -- and it is code, with
tests, not only a design. Checked against our `main` at `fe8340ca` by symbol
(what is *in* our tree, not by patch identity, since upstream squash-merges):

| branch (his) | what it is | in ours? |
|---|---|---|
| `ta-anchor-dynamic-refresh` (33 commits, 2026-08-06..08; includes `ta-anchor-refactor-v2` = upstream PR #1516) | the multi-vote pipeline in `internal/engines/pipeline` (our `engines/allocation`): **one combine core** `combineVotes(votes, up) → (count, binder)` -- max for scale-up, min for scale-down, rounding once at the caller, the *binding analyzer* returned with the count so "how many" and "who decided" cannot disagree; **abstain ≠ zero** -- a role an analyzer has no model for is tagged `ReasonRoleUnmodeled` and casts no vote (an empty ballot is "no basis to act"), which closed the prefill drain; **score as a dominance correction** (`v* = e − Σ(e − v_i)(s_i − s_e)⁺ / Σs_j`, never outside `[min, max]`; uniform scores = plain max/min); the **live-only veto gate** (`roleSpareVetoed`: every live analyzer with an opinion must see spare); **coverage per GPU freed** (`max_i PRC_i[v] / GPUsPerReplica[v]`) as the scale-down tie-break -- shed the variant whose GPU buys the least serving capacity; a per-iteration **anchor refresh** (the binder re-selected as the fleet moves); one fair-share entitlement per model; goldens and invariant specs (`optimizer_invariant7_test.go`, `optimizer_multivote_characterization_test.go`) | **no**: none of `combineVotes`, `ReasonRoleUnmodeled`, `bindingAnchor`, `roleSpareVetoed`, the coverage tie-break. We have the liveness half only (#1481, `f5261c8e`) |
| `docs/developer-guide/multi-analyzer-pipeline.md` on that branch (+592 lines) | the pipeline contract written down: data flow per cycle, the responsibility table (who writes which field), the linearity invariant (`TotalSupply = Σ PRC × ReplicaCount`), the implementor guide, the three ballot collectors, the veto gate, liveness with its three no-data cases | no |
| `analyzer-metric-proposal` (`docs/proposals/analyzer-metric-interface.md`) | the analyzer contract collapsed to two numbers per finest-grain item, `D` and `P`, in the analyzer's own unit; every analyzer's result emitted as Prometheus metrics with a common label set; external analyzers as PromQL with `match` selectors per ScaledObject/role; the "not-defined vs missing vs present" three-state rule | **yes** (our copy is three lines behind); the external wrapper implements its first phase |
| `ta3-e2e`, `ta-correctness-guards`, `ta-veto-liveness`, `ta-model-level-demand` (May-July) | TA fixes: GPS-mismatch window clearing, per-replica unhealthy exclusion, freshness for absent-by-design metrics, model-level demand | yes, through the upstream merges we adopted (`consecutiveGPSMismatches`, `absent-by-design` are in our tree) |
| `benchmark`, `autoscaling-viz` (Aug) | a results tree with `postprocess.py` and a generated `REPORT.md` per run; a real-trace visualiser with router-imbalance reporting | no; candidates for the scorecard of item 3 in the shape-shift proposal |

### And on his fork of *this* repository -- the un-PRed work

`deanlorenz/llm-scaler` (fetched as remote `deanscaler`, 2026-09-21) carries
thirty branches; twelve are the install/preflight/dashboard PRs #15-#26,
merged in August, and #34 (the single `CompositeSignal` at the
engine→optimizer boundary, merged 2026-08-31). The rest is not PRed:

| branch | base | what it is | state |
|---|---|---|---|
| **`composite-analyzer`** (83 commits, 2026-09-12..16) | our `main` at `67229a01` (2026-09-11) | **the successor to #34, and the design this document was groping toward.** A mission spec (v8) with the owner's decisions marked: every analyzer's contract is **two numbers per ScaledObject, `Demand` and `PRC`**; the composite signal is **`N(SO)`, replicas needed**, the max over *eligible* contributors of `ceil(Demand/PRC)`; **eligibility** = non-nil ∧ informative ∧ live, one rule in both directions (a stale analyzer neither raises demand nor vetoes scale-down); **saturation is not privileged** -- it is the identity carrier for infrastructure fields (ready, pending, cost, GPUs) and a *fallback* contributor only when nothing else has a signal, so the composite may come out below saturation alone, which is the point of a second analyzer; a **decision path on the composite** (`C0-agree`, `C1-single`, `C2-sat-fallback`, `C3-default-prc`, `C4-no-signal`) mirroring the analyzers' own `Reason`, logged through one function so a decision is traceable end to end; **no signal → do not autoscale**, as an explicit gate rather than a test on saturation's name; **coverage** = supply/demand per role, **undefined at zero** and carried as `(value, ok)` so an undefined contribution never enters a min or max as a magic 0 or +Inf (`maxOfDefined`/`minOfDefined`, one place for the combination rule); the cross-role rule `coverage(M) = min(cov(prefill), cov(decode)) + cov(both)`; a query API with **one rounding rule per concept** (`replicasForDemand` = ceil, `safeReplicasForSpare` = floor); score deferred -- pure max. ~3,000 lines of code and tests (`aggregation/{demand,model_coverage,undefined}.go`, `allocation/{composite_decision,composite_eligibility,composite_signal_gate,query_api}.go`, `steadystate/composite.go`), plus the spec and a redesign record in `.session/` | implemented and verified on the branch; "not yet reviewed by the user"; no PR |
| `single-analyzer`, `single-analyzer-normalize` (Sep 6-7) | Aug/Sep `main` | the steps between #34 and `composite-analyzer` (compose/reduce design, the zero-demand normalisation) | superseded by `composite-analyzer` |
| `benchmark-runtools` (63), `benchmark-viz` (19), `benchmark-{init,plan,extract}`, `worktree-{benchmark,anchor-offset,multi-variant,run-only-gap,scaler-issues}` (Aug 21 - Sep 6) | Aug `main` | benchmark tooling in `hack/benchmark/`: `report.py`, `render_real_trace.py`, `extract_real_trace.py`, `dump_wva_decision_table.py`, `capture_wva_controller_log.sh`, `env_wizard.py`/`env_guard.py`, a GPU-reservation coupler, results bundles (`coverage.json`, `endpoints.json`, `meta.json`) with a generated report per run, and drafted observability-gap issues | working tooling with committed run bundles (large); no PR |
| `policy-writer` (156), `session-tracking` (92), `agentbus` (21) | unrelated history | agent-workflow conventions and session tooling, not product code | -- |

This supersedes the "port the upstream anchor branch" idea above. The
combine core is not something to port and rename from an August
upstream-shaped tree: it exists on a branch cut from *our* `main` five
days before this document, with the owner's decisions written next to
the code. The upstream anchor branch keeps two things the composite spec
deferred -- the score dominance correction (the spec chose pure max, and
says so) and the coverage-per-GPU-freed scale-down tie-break -- and is the
reference for those if they are ever wanted.

Two of these change the plan above:

- **`analyze/combine` is `composite-analyzer`, reviewed and merged, not
  written.** The structure work's stage 4 starts by reviewing that branch
  against `main` as it stands (it is 10 days behind; #77-#85 touched
  `saturation_v2` and `steadystate`, not the composite files, so the
  rebase should be small), merging it, and only then moving the composite
  into `analyze/combine` and the optimizer into `plan/optimize`. The
  `(value, ok)` convention, `Eligible`, the decision path and the
  no-signal gate are the `Holds`/abstention semantics this document asked
  for, already tested.
- **Item 3 of the shape-shift proposal (the scorecard) starts from
  `benchmark-viz`/`benchmark-runtools`**, not from `compare_runs.sh`:
  `report.py` and the results bundle are the generated report per run;
  `dump_wva_decision_table.py` is the decision timeline; what they lack
  is the windowed TTFT and the target-path columns this week's runs used,
  which are a few dozen lines on top.

What was written earlier in this section about the upstream anchor branch
stands as the record of what is there; the plan below takes the fork's
branch instead.


- **`analyze/combine` and `policy` should be his work, not ours rewritten** (and the section that follows finds it on a branch of this repository, which changes how): What I sketched as "one documented rule, max demand per
  role" and "`Holds` on the result" is a weaker version of `combineVotes`,
  the binder, `ReasonRoleUnmodeled` and the veto gate; his has the tests and
  the measured cases (the prefill drain, the freeze). Stage 4 therefore
  starts by porting `ta-anchor-dynamic-refresh` onto our `allocation`
  package -- 33 commits, `engines/pipeline` → `engines/allocation`,
  `engines/saturation` → `engines/steadystate`, and the commits that touch
  the queueing model we deleted dropped, as with #1516 before -- and only
  then splits it into `plan/optimize` and `analyze/combine`. `AnalyzerResult.Holds`
  becomes `RoleCapacity.Reason` (`ReasonRoleUnmodeled` and the hold reasons
  the floor already has), which is what an abstention is.
- **The developer guide comes with it.** `multi-analyzer-pipeline.md` is
  the contract document this proposal asks for under "comment policy"; it
  already has the responsibility table and the implementor guide. It moves
  into `docs/developer-guide/` with the port and the histories from
  `throughput_floor.go` join it.

What does not transfer: his tree is upstream-shaped (August; still the VA
CR, no KEDA-driven discovery, no external scaler), so the port is by patch
with renames, not a merge, and the fair-share entitlement (a model
`priority` weight) has to be re-checked against our quota limiters (#81)
which did not exist when it was written.

## The plan, in stages that each leave `main` green

Every stage is behaviour-preserving, verified by the existing suites, and
by the benchmark scorecard on the two shape-swap traces (runs 16 and 18
are the fixed references: any stage that moves their windowed p95 or
target path has changed behaviour and stops).

1. **`signals`** -- extract, do not rewrite. (`signals/capacity` -- k1/k2 and the store -- is part of this stage, so that stage 3 can hand capacity to every analyzer through the input.) Move `ShapeTracker`,
   `ObservationWindow`, `ITLModel` and `rollingAverage` out of the two
   analyzers into `internal/signals` with their tests; make
   `rollingAverage` and `ObservationWindow` one type. Move the floor's
   pure arithmetic (`estimateThroughputDemand`, `medianFloat`) there.
   Analyzers keep working unchanged, importing them.
2. **Cut the upward edges.** `ReasonError` / `ResultIsInformative` to
   `domain`; the collector's `decision` import replaced by the value it
   reads. Add the import-direction check. Nothing else moves.
3. **Split the engine by concern**, and the analyzer surface with it: one registry, the input builder before `Analyze`, `Holds` on the result, `analyze/combine`. `sticky_scale_down.go`,
   `inventory_gate.go`, `scaling_blocked.go` and the hold/gate logic now
   inside `engine.go` become `internal/policy`; `engine.go` +
   `engine_v2.go` become one `engine.go` that only sequences the steps.
   The largest single diff of the plan, and pure movement.
4. **Review and merge `deanscaler/composite-analyzer`** (rebased on `main`),
   then **`plan/optimize` and `plan/limit`** from `allocation` and the
   composite into `analyze/combine`; `analyzer_helpers.go` goes to
   `analyze`. The upstream anchor branch is the reference for the score
   correction and the coverage tie-break if either is wanted later.
5. **Collector by concern**: `replica_metrics.go` → `query.go`,
   `collapse.go`, `attribute.go`, `freshness.go` (the sub-packages are
   already right).
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
- Not the warm pool. `warmpool` has the same size problem
  (`reconciler.go` 1,492) and the same fix, but it is a separate subsystem
  with its own guide; it follows the same rules on its own schedule.
- Not a naming exercise. Package names above are proposals; what matters
  is the direction rule and the one-concern rule, which the check enforces.
