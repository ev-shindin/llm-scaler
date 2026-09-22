package collector

// Freshness: how old each series behind a replica's row is, classified
// against config.FreshnessThresholds. Two readers of the same
// classification: the per-variant gauge (trackMetricFreshness) and the
// per-replica metadata the analyzers gate on (worstFreshnessStatus).

import (
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// freshnessSeverity orders freshness statuses from best to worst, so both the
// per-instance rollup in collectReplicaMetrics and the pod merge above can pick
// the single worst status across a set of metrics.
var freshnessSeverity = map[string]int{"fresh": 0, "stale": 1, "unavailable": 2, "missing": 3}

// classifyTimestamp reports the freshness status of a single metric timestamp,
// along with its age when non-zero. A zero timestamp means the metric was never
// scraped for this pod and is classified "missing". Shared by trackMetricFreshness
// (aggregate gauge) and worstFreshnessStatus (per-replica metadata) so the two
// cannot drift apart.
func classifyTimestamp(timestamp, collectedAt time.Time, thresholds config.FreshnessThresholds) (status string, age time.Duration, hasTimestamp bool) {
	if timestamp.IsZero() {
		return "missing", 0, false
	}
	age = collectedAt.Sub(timestamp)
	return thresholds.DetermineStatus(age), age, true
}

// trackMetricFreshness determines the freshness status of metrics in podMetricData
// and increments the corresponding counters in the freshness status map.
func trackMetricFreshness(
	vaName string,
	data *podMetricData,
	collectedAt time.Time,
	freshnessMap map[string]map[string]int,
) {
	// Initialize inner map if needed
	if freshnessMap[vaName] == nil {
		freshnessMap[vaName] = make(map[string]int)
	}

	thresholds := config.DefaultFreshnessThresholds()

	// Helper to track a single timestamp
	trackTimestamp := func(timestamp time.Time) {
		status, _, _ := classifyTimestamp(timestamp, collectedAt, thresholds)
		freshnessMap[vaName][status]++
	}

	// Track all metric timestamps
	trackTimestamp(data.kvTimestamp)
	trackTimestamp(data.queueTimestamp)
	trackTimestamp(data.avgOutputTokensTimestamp)
	trackTimestamp(data.avgInputTokensTimestamp)
	trackTimestamp(data.prefixCacheHitRateTimestamp)
	trackTimestamp(data.cacheConfigTimestamp)
	trackTimestamp(data.avgITLTimestamp)
	trackTimestamp(data.avgServiceTimeTimestamp)
}

// worstFreshnessStatus returns the least-fresh status across data's *present*
// timestamps (the same set trackMetricFreshness uses) and the age of the oldest
// one, for the per-replica ReplicaMetricsMetadata.
//
// Absent ("missing") timestamps are skipped rather than allowed to dominate the
// rollup: several tracked metrics are legitimately unscraped in common
// deployments — the prefix-cache / cache-config timestamps when prefix
// caching is off. Counting
// those as "missing" (the worst severity) would report a healthy replica as
// "missing" with a near-zero Age, and — because "missing" outranks "stale" —
// would mask a genuinely stale driving metric from the CheckModelMetrics
// stale-metrics gate, which keys on FreshnessStatus == "stale". A replica with
// no present timestamps at all is still reported "missing".
func worstFreshnessStatus(data *podMetricData, collectedAt time.Time) (string, time.Duration) {
	thresholds := config.DefaultFreshnessThresholds()
	timestamps := []time.Time{
		data.kvTimestamp,
		data.queueTimestamp,
		data.avgOutputTokensTimestamp,
		data.avgInputTokensTimestamp,
		data.prefixCacheHitRateTimestamp,
		data.cacheConfigTimestamp,
		data.avgITLTimestamp,
		data.avgServiceTimeTimestamp,
	}

	worst := "fresh"
	var oldestAge time.Duration
	anyPresent := false
	for _, ts := range timestamps {
		status, age, hasTimestamp := classifyTimestamp(ts, collectedAt, thresholds)
		if !hasTimestamp {
			continue // absent-by-design metric must not dominate the rollup
		}
		anyPresent = true
		if age > oldestAge {
			oldestAge = age
		}
		if freshnessSeverity[status] > freshnessSeverity[worst] {
			worst = status
		}
	}
	if !anyPresent {
		return "missing", 0
	}
	return worst, oldestAge
}
