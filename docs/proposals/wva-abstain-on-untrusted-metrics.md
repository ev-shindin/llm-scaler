# Declining to answer KEDA when WVA cannot see a workload

**Status: PARKED.** The design below is built and tested on this branch, and it
is not fit to land. Read "Why this is parked" before touching any of it — several
of the defects are ones the implementation was rewritten twice to fix and still
has.

## The problem

Every guard WVA has rejects an input it cannot trust: a not-Ready pod's timing, a
service time longer than the pod's own uptime, a replica whose scrape is behind.
Rejecting is right, but rejecting *enough* of a variant's inputs quietly converts
"the metrics pipeline is broken" into "this workload looks idle" — and an
idle-looking workload is scaled down. The guards make a recommendation safe from
one bad replica; nothing makes the *absence* of a recommendation visible instead
of being rounded off to a low one.

## The design

WVA declines to answer rather than answering from nothing. This is deliberately
KEDA's mechanism rather than a new one of ours:

- `GetMetrics` returns `codes.Unavailable` when the collector has no usable view
  of the workload.
- KEDA propagates no metric to the HPA, and after
  `spec.fallback.failureThreshold` consecutive failures applies `spec.fallback`.
- `deploy/lib/scaledobject.sh` emits that stanza with
  `behavior: currentReplicasIfHigher` so the floor can only hold or raise.

WVA builds no freeze, hold-last-value or floor of its own. `internal/decision`
carries the verdict between the collector (writer) and the external scaler
(reader), following the shape of the contention and GPU-usage stores beside it.

### Two things that are settled and should not be re-litigated

**The trust store is keyed by the SCALEDOBJECT, not the scale target.** Collected
rows carry the ScaledObject name as `VariantName`, and `ScaledObjectRef.Name` is
the same name, so both ends agree. The decision store keys by the scale target
because the actuator writes it from a VariantAutoscaling. Keying trust the same
way is not hypothetical — it was done, and the generator names ScaledObjects
`<target>-wva`, so writer and reader never met and the abstain could not fire at
all.

**An erroring `GetMetrics` also marks the scaler INACTIVE.** KEDA's
`GetMetricsAndActivity` returns on the `GetMetrics` error and never reaches its
`IsActive` call, so WVA answering the 0↔1 gate correctly does not protect it.
The only thing standing between an abstain and a deactivation is
`spec.fallback.replicas != 0` — that is the condition on KEDA's log-only executor
branch, and its absence drops through to `scaleToZeroOrIdle`. `fallback.replicas`
must therefore never be 0, including on a `minReplicaCount: 0` install.
`hack/check-scaledobject-fallback.sh` asserts it.

## Why this is parked

### 1. The freshness half cannot work as designed (CRITICAL)

The abstain has two triggers. The first — "rows arrived and every replica's data
is past the unavailable threshold" — is unreachable, and no amount of tuning
fixes it.

A row exists only if `collectReplicaMetrics` saw `hasKv || hasQueue`, and both
come from `max_over_time(...[1m])` range selectors. So a row implies a raw sample
within the last 60 seconds. `metrics_age` times *that same series*. Age is
therefore structurally bounded below the 1-minute fresh threshold, and the
5-minute unavailable band is an empty interval. Prometheus's default
`--query.lookback-delta=5m` caps it independently.

**Rows are fresh by construction.** That is the finding, and it kills four
consumers built on top of the freshness bands, all of which are dead branches in
the field:

- `internal/collector/trust.go` — `Metadata.Unavailable()` never true, so the
  row-based abstain never fires.
- `saturation_v2/arrival_demand.go` — `medianOf`'s stale skip skips nothing.
- `throughput/sanity.go` — `SanityIssueStaleMetrics` never raised (pre-existing).
- `internal/collector/pod_collapse.go` — the DP-rank age promotion has no effect.

This also explains why three separate consumers compared `FreshnessStatus`
against `"stale"` with `==` for a long time and nobody noticed: the branch was
never taken.

The test that appeared to prove the fix stages a state Prometheus cannot produce
— `kv_cache_usage` present *and* `metrics_age: 600` for the same pod. If the
series were 600s old the row would not exist.

The only staleness signal that actually works is the second trigger: the row
stopped appearing at all (`TrustRecord.LastObserved`). Any revival of this design
should drive the abstain from that alone, or change how rows are produced so an
age band is reachable — not tune thresholds.

### 2. A workload WVA itself parks at zero abstains forever (HIGH)

Scale to zero → no pods → no series → no rows → `publishTrustVerdicts` iterates
nothing → `LastObserved` freezes. Five minutes later the workload is untrusted
permanently, and `wva_model_scaling_blocked{reason="no-trusted-metrics"}` is
pinned at 1 for its model: an alert fired by correct behaviour. The store has no
eviction, so a deleted ScaledObject keeps its record forever too.

`decision.TrustRecord`'s comment claims a workload "parked at zero" is safe. That
is true only of one never collected; the normal route into zero is
running → collected → parked, which leaves a record.

### 3. Foreign rows are counted, not filtered (HIGH)

`replica_metrics.go` detects rows resolving to a variant this model does not own,
increments `PodMappingMissOtherModelVariant`, logs "ignoring them" — and leaves
them in `replicaMetrics`. `publishTrustVerdicts` then writes a verdict for
another model's ScaledObject under *this* model's ID. Triggers: an FMA launcher
rebound to another model, and a warmed-but-unlent warm-pool pod (`resolveScaler`
has no `ScalesAWarmPool` filter, unlike every other registry consumer). Masked
today by defect 1; live the moment that is fixed.

### 4. Smaller, all real

- `WVA_DEFAULT_SO_TEMPLATE` bypasses `render_default_scaledobject` entirely, so
  the `fallback.replicas` floor never runs for operators using the documented
  escape hatch, and the check script only exercises the default renderer.
- `publishTrustVerdicts` passes the *owned* reason set as the *active* set. Works
  only because the slice has one element.
- `staleObservationLimit` and `trustStaleLimit` must stay equal and nothing
  enforces it.
- `collectReplicaMetrics` returns early on a query error without reaching
  `publishTrustVerdicts`, so during a Prometheus outage — the case that drives
  the whole fleet untrusted — the blocked-reason metric is never set.
- The `metrics_age` PromQL template is registered but never executed against a
  mock, so a wrong metric name is not caught. 33 of 34 collector tests mutate the
  `decision.DefaultTrust` singleton.

## What landed instead

The service-time-vs-pod-uptime guard, which is independent of all of this and
survived two rounds of review clean. See the commit
"fix(collector): reject a service time longer than the pod has existed".

## If this is revived

1. Drive the abstain from `LastObserved` only, and delete the age-band machinery
   rather than tuning it.
2. Decide what a parked-at-zero workload means *before* writing code; it is the
   case that breaks the naive rule.
3. Filter foreign rows where they are already detected.
4. Do not reintroduce a freshness band without first showing, with a real
   Prometheus, that a row can carry one.
