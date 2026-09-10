package collector

import (
	"slices"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
)

// engineSpecificReplicaQueries are the logical query names collected per replica
// whose metric source is the inference engine (vLLM/SGLang). Each is refreshed
// once per present engine and merged back under its logical name.
//
// This is the per-replica subset of registration.EngineSpecificQueries (the
// authoritative list of engine-specific logical queries). Every entry here MUST
// also appear there — TestEngineSpecificReplicaQueriesSubset enforces it so the
// two lists cannot drift. It intentionally omits registration.QueryModelRequestCount,
// which is engine-specific but a scale-to-zero query (collected on demand by the
// enforcer, not per replica in this collector).
var engineSpecificReplicaQueries = []string{
	registration.QueryKvCacheUsage,
	registration.QueryMetricsAge,
	registration.QueryQueueLength,
	registration.QueryCacheConfigInfo,
	registration.QueryAvgOutputTokens,
	registration.QueryAvgInputTokens,
	registration.QueryPrefixCacheHitRate,
	registration.QueryAvgITL,
	registration.QueryAvgServiceTime,
	registration.QueryGenerationTokenRate,
	registration.QueryKvUsageInstant,
	registration.QueryRequestRate,
}

// agnosticReplicaQueries are the logical query names collected per replica whose
// metric source is engine-independent (the EPP inference scheduler). They are
// refreshed once and shared across engines.
//
// Empty today. Its only member was the EPP's per-pod dispatch rate, removed
// because nothing consumed it: the demand floor and the throughput analyzer both
// size from the MODEL-level arrival rate (QueryModelArrivalRate), and the
// per-variant figure the throughput analyzer displays falls back to the engine's
// own completion rate. The extension point stays because the next engine-agnostic
// per-replica metric belongs here.
var agnosticReplicaQueries = []string{}

// unpartitionedReplicaQueries lists the logical replica queries whose series
// cannot be partitioned by model_name, because they do not carry that label.
//
// Everything else in engineSpecificReplicaQueries is filtered down to the model
// being collected; keeping this as the exception list (rather than a second
// copy of the inclusion list) means a query added there is partitioned by
// default and cannot silently leak another model's replicas into this one's.
//
// vllm:cache_config_info is the only such query: it is an info-style metric
// whose labels come from CacheConfig fields, never from the request path. The
// collector attributes it by instance key instead — it attaches cache config
// only to instances the model-scoped KV/queue results already discovered.
var unpartitionedReplicaQueries = map[string]bool{
	registration.QueryCacheConfigInfo: true,
}

// buildEngineQueryList returns the physical query names to refresh for the given
// set of present engines: each agnostic query once, plus each engine-specific
// query for every present engine. For a single-engine vLLM model this is exactly
// the agnostic queries plus the bare engine-specific names (unchanged behavior).
func buildEngineQueryList(engines []inferenceengine.Engine, engineSpecific, agnostic []string) []string {
	queries := make([]string, 0, len(agnostic)+len(engineSpecific)*len(engines))
	queries = append(queries, agnostic...)
	for _, logical := range engineSpecific {
		for _, eng := range engines {
			queries = append(queries, registration.EngineQuery(eng, logical))
		}
	}
	return queries
}

// mergeEngineResults re-keys engine-specific query results under their logical
// names so downstream per-pod processing is engine-agnostic.
//
//   - Single engine: the physical result is aliased to the logical name (a no-op
//     for vLLM, where physical == logical; a rename for SGLang). The physical key
//     is left in place so engine-specific consumers (e.g. the SGLang cache-config
//     pass) can still read it.
//   - Multiple engines: the per-engine series are concatenated into a new result
//     under the logical name. Series are disjoint by pod, so concatenation is safe.
//
// The rename aliases through a copy rather than rewriting QueryName in place:
// the result is shared with every other model in the namespace through the
// collector's per-cycle memo, so mutating it would race with — and rewrite the
// query name under — their collection.
func mergeEngineResults(results map[string]*source.MetricResult, engines []inferenceengine.Engine, logicalNames []string) {
	if len(engines) == 1 {
		eng := engines[0]
		for _, logical := range logicalNames {
			phys := registration.EngineQuery(eng, logical)
			if phys == logical {
				continue // vLLM: already keyed by logical name.
			}
			if r := results[phys]; r != nil {
				aliased := *r
				aliased.QueryName = logical
				results[logical] = &aliased
			}
		}
		return
	}

	for _, logical := range logicalNames {
		var merged *source.MetricResult
		var firstErr error
		for _, eng := range engines {
			r := results[registration.EngineQuery(eng, logical)]
			if r == nil {
				continue
			}
			if merged == nil {
				merged = &source.MetricResult{QueryName: logical, CollectedAt: r.CollectedAt}
			}
			merged.Values = append(merged.Values, r.Values...)
			if firstErr == nil && r.Error != nil {
				firstErr = r.Error
			}
			if !r.CollectedAt.IsZero() && (merged.CollectedAt.IsZero() || r.CollectedAt.Before(merged.CollectedAt)) {
				merged.CollectedAt = r.CollectedAt
			}
		}
		if merged != nil {
			// Only surface an error when NO engine produced any series. A partial
			// success — one engine errored (e.g. transient timeout) while another
			// returned values — must not mark the merged result errored, or the
			// downstream HasError() checks would discard the healthy engine's pods
			// and blackhole scaling for the whole mixed-engine model.
			if len(merged.Values) == 0 {
				merged.Error = firstErr
			}
			results[logical] = merged
		}
	}
}

// containsEngine reports whether the engine set includes the given engine.
func containsEngine(engines []inferenceengine.Engine, target inferenceengine.Engine) bool {
	return slices.Contains(engines, target)
}

// seriesModelLabel is the label engine (vLLM/SGLang) and EPP series carry to
// identify the model being served.
const seriesModelLabel = "model_name"

// seriesTargetModelLabel is the label EPP series carry to identify the model a
// request resolved to after routing (e.g. a specific LoRA adapter). EPP leaves
// it empty when no rewrite happened.
const seriesTargetModelLabel = "target_model_name"

// eppSeriesModel returns the model an EPP series belongs to: target_model_name
// when set, else model_name. This is the Go-side form of the "or" clause the
// model-scoped EPP queries used to carry, and it is applied per series so a
// single namespace-wide (or, for the flow-control metrics, cluster-wide)
// execution can be split across models.
func eppSeriesModel(labels map[string]string) string {
	if target := labels[seriesTargetModelLabel]; target != "" {
		return target
	}
	return labels[seriesModelLabel]
}

// filterResultsToModel restricts the named results to the series belonging to
// modelID, replacing each entry with a model-scoped copy.
//
// The queries behind these results are namespace-scoped and shared by every
// model in the namespace, so this is where a model's slice of them is taken. A
// series whose model_name differs belongs to another model; one carrying no
// model_name at all cannot be attributed to any model. Both are dropped, which
// is exactly what the model_name matcher these queries used to carry did on the
// Prometheus side.
//
// Queries in unpartitionedReplicaQueries are left untouched — their series have
// no model identity to filter on.
//
// The copy matters: the underlying results are shared through the collector's
// per-cycle memo, so filtering in place would empty them for every model
// collected afterwards.
func filterResultsToModel(results map[string]*source.MetricResult, logicalNames []string, modelID string) {
	for _, name := range logicalNames {
		if unpartitionedReplicaQueries[name] {
			continue
		}
		if r := results[name]; r != nil {
			results[name] = filterSeries(r, func(labels map[string]string) bool {
				return labels[seriesModelLabel] == modelID
			})
		}
	}
}

// filterSeries returns a copy of result holding only the values whose labels
// satisfy keep. The input is never modified.
func filterSeries(result *source.MetricResult, keep func(labels map[string]string) bool) *source.MetricResult {
	filtered := *result
	filtered.Values = make([]source.MetricValue, 0, len(result.Values))
	for _, value := range result.Values {
		if keep(value.Labels) {
			filtered.Values = append(filtered.Values, value)
		}
	}
	return &filtered
}
