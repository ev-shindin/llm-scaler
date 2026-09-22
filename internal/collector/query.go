package collector

import (
	"context"
	"fmt"
	"math"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// refreshShared executes queries, reusing any result already fetched in this
// cycle for the same (query, params) pair.
//
// Only the queries missing from the memo are sent to the source, so the first
// model in a namespace pays for the namespace-scoped queries and the rest read
// them, while a mixed-engine namespace still fetches each engine's variant
// exactly once.
//
// A result carrying a query error is memoized like any other: the query did
// fail for this namespace this cycle, and re-running it once per model would
// multiply a Prometheus outage by the number of models rather than surface it
// once.
//
// params must contain exactly the parameters the queries take. Passing extra
// ones (a modelID a namespace-scoped query ignores, say) does not change the
// PromQL, but it does change the memo key and would silently defeat sharing.
func (c *ReplicaMetricsCollector) refreshShared(
	ctx context.Context,
	queries []string,
	params map[string]string,
) (map[string]*source.MetricResult, error) {
	results := make(map[string]*source.MetricResult, len(queries))
	missing := queries

	c.cycleMu.Lock()
	if c.cycleResults != nil {
		missing = make([]string, 0, len(queries))
		for _, name := range queries {
			// A memoized nil records a query the source returned nothing for;
			// that is an answer, so it is not asked again this cycle.
			if cached, ok := c.cycleResults[source.BuildCacheKey(name, params)]; ok {
				if cached != nil {
					results[name] = cached
				}
				continue
			}
			missing = append(missing, name)
		}
	}
	c.cycleMu.Unlock()

	if len(missing) == 0 {
		return results, nil
	}

	fetched, err := c.source.Refresh(ctx, source.RefreshSpec{Queries: missing, Params: params})
	if err != nil {
		return nil, err
	}

	c.cycleMu.Lock()
	for _, name := range missing {
		result := fetched[name]
		if result != nil {
			results[name] = result
		}
		if c.cycleResults != nil {
			c.cycleResults[source.BuildCacheKey(name, params)] = result
		}
	}
	c.cycleMu.Unlock()

	return results, nil
}

// queryReplicaSeries fetches every per-replica series for one model: the
// engine-specific queries for each engine the model's variants run, merged
// back under their logical names and cut down to this model's series. The
// engines are returned with the results because the SGLang cache-config
// series is read through its physical key downstream (extractPodMetrics).
func (c *ReplicaMetricsCollector) queryReplicaSeries(
	ctx context.Context,
	modelID string,
	namespace string,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
) (map[string]*source.MetricResult, []inferenceengine.Engine, error) {
	logger := ctrl.LoggerFrom(ctx)

	// Every replica query is namespace-scoped: the model is selected from the
	// returned series, not by a PromQL matcher, so the params carry no modelID.
	params := map[string]string{
		source.ParamNamespace: namespace,
	}

	// Determine which inference engines this model's variants run. Engine-specific
	// queries are refreshed once per present engine (vLLM pods emit vllm:* series,
	// SGLang pods emit sglang:* series — the per-pod results are disjoint and are
	// merged back under the logical query name below). For vLLM-only models this
	// is identical to the previous fixed query list.
	engines := inferenceengine.Present(scaleTargets)

	// Log the engine detected for each scale target, plus the resolved engine set.
	// inferenceengine.Detect defaults to vLLM when a leader pod template is nil or
	// unresolvable, or when an SGLang image/command isn't matched — so a misdetected
	// SGLang variant silently gets vllm:* queries and emits nothing. Logging the
	// per-target engine lets operators tell "wrong engine detected" apart from
	// "engine correct, but no metrics".
	if debug := logger.V(logging.DEBUG); debug.Enabled() {
		for key, st := range scaleTargets {
			debug.Info("Detected inference engine for scale target",
				"scaleTarget", key, "engine", inferenceengine.Detect(st).String())
		}
		engineNames := make([]string, len(engines))
		for i, e := range engines {
			engineNames[i] = e.String()
		}
		debug.Info("Resolved inference engines for model",
			"modelID", modelID, "namespace", namespace, "engines", engineNames)
	}

	// Refresh all Prometheus-sourced queries:
	// - Saturation: KV cache, queue length, cache config, prefix cache hit rate
	// - Shared (saturation + queueing model): avg input tokens, avg output tokens
	// - Queueing model: scheduler dispatch rate, avg TTFT, avg ITL
	// - Throughput analyzer: generation token rate, instantaneous KV usage (k*), request rate
	queries := buildEngineQueryList(engines, engineSpecificReplicaQueries, agnosticReplicaQueries)

	// Execute the query with timing. refreshShared skips whatever another model
	// in this namespace already fetched this cycle, so the timing observed here
	// is the cost of the queries this collection actually issued.
	startTime := time.Now()
	results, err := c.refreshShared(ctx, queries, params)
	duration := time.Since(startTime).Seconds()
	metrics.ObserveMetricsCollectionDuration(duration, constants.QueryTypeKVCache)
	metrics.ObserveMetricsCollectionDuration(duration, constants.QueryTypeQueueLength)
	metrics.ObserveMetricsCollectionDuration(duration, constants.QueryTypeCacheConfig)

	if err != nil {
		reason := prometheus.CategorizePrometheusError(err)
		metrics.IncMetricsCollectionErrors(constants.QueryTypeKVCache, reason)
		metrics.IncMetricsCollectionErrors(constants.QueryTypeQueueLength, reason)
		metrics.IncMetricsCollectionErrors(constants.QueryTypeCacheConfig, reason)
		return nil, nil, fmt.Errorf("failed to refresh replica metrics: %w", err)
	}

	// Re-key engine-specific results under their logical query names so the per-pod
	// processing below is engine-agnostic. For SGLang-only models this renames the
	// "sglang/<query>" results to "<query>"; for mixed-engine models it concatenates
	// the per-engine series. The structural cache-config difference is handled by a
	// dedicated SGLang pass after the vLLM cache-config block.
	mergeEngineResults(results, engines, engineSpecificReplicaQueries)

	// Take this model's slice of the namespace-wide series. Everything below
	// operates on model-scoped results, as it did when the model was a PromQL
	// matcher.
	filterResultsToModel(results, engineSpecificReplicaQueries, modelID)

	return results, engines, nil
}

// CollectSchedulerQueueMetrics collects model-level queue metrics from the
// llm-d inference scheduler flow control layer. These metrics are not per-pod
// but per-model, representing requests queued upstream before reaching the engine.
// Returns nil (not an error) when flow control metrics are unavailable.
//
// The two queries take no parameters at all: the flow-control metrics have no
// namespace label to scope them by (#2309) and are no longer filtered by model
// either, so a single cluster-wide execution of each covers every model the
// controller manages and this picks its own out of the result.
func (c *ReplicaMetricsCollector) CollectSchedulerQueueMetrics(
	ctx context.Context,
	modelID string,
) *domain.SchedulerQueueMetrics {
	logger := ctrl.LoggerFrom(ctx)

	queries := []string{
		registration.QuerySchedulerQueueSize,
		registration.QuerySchedulerQueueBytes,
	}

	results, err := c.refreshShared(ctx, queries, nil)
	if err != nil {
		logger.V(logging.DEBUG).Info("Scheduler queue metrics unavailable",
			"modelID", modelID, "error", err)
		return nil
	}

	var queueSize, queueBytes int64
	hasData := false

	if result := results[registration.QuerySchedulerQueueSize]; result != nil && !result.HasError() {
		for _, value := range result.Values {
			if eppSeriesModel(value.Labels) != modelID {
				continue
			}
			if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) {
				queueSize += int64(value.Value)
				hasData = true
			}
		}
	}

	if result := results[registration.QuerySchedulerQueueBytes]; result != nil && !result.HasError() {
		for _, value := range result.Values {
			if eppSeriesModel(value.Labels) != modelID {
				continue
			}
			if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) {
				queueBytes += int64(value.Value)
				hasData = true
			}
		}
	}

	if !hasData {
		return nil
	}

	logger.V(logging.DEBUG).Info("Collected scheduler queue metrics",
		"modelID", modelID,
		"queueSize", queueSize,
		"queueBytes", queueBytes)

	return &domain.SchedulerQueueMetrics{
		QueueSize:  queueSize,
		QueueBytes: queueBytes,
	}
}
func (c *ReplicaMetricsCollector) CollectModelArrivalRate(
	ctx context.Context,
	modelID, namespace string,
) float64 {
	logger := ctrl.LoggerFrom(ctx)

	params := map[string]string{
		source.ParamNamespace: namespace,
	}

	results, err := c.refreshShared(ctx, []string{registration.QueryModelArrivalRate}, params)
	if err != nil {
		// Categorize rather than swallow: a broken or misconfigured arrival query and
		// genuine zero traffic both surface here as a zero rate, but only the former is a
		// fault. Record the categorized reason as a collection error so an operator can
		// tell the two apart (a nonzero arrival_rate error counter means a query problem,
		// not idle traffic). Demand still falls back to 0 — zero only ever permits
		// scale-down, gated by the multi-analyzer live-consensus veto.
		reason := prometheus.CategorizePrometheusError(err)
		metrics.IncMetricsCollectionErrors(constants.QueryTypeArrivalRate, reason)
		logger.V(logging.DEBUG).Info("Model arrival rate unavailable",
			"modelID", modelID, "namespace", namespace, "reason", reason, "error", err)
		return 0
	}

	result := results[registration.QueryModelArrivalRate]
	if result == nil {
		return 0
	}
	if result.HasError() {
		reason := prometheus.CategorizePrometheusError(result.Error)
		metrics.IncMetricsCollectionErrors(constants.QueryTypeArrivalRate, reason)
		logger.V(logging.DEBUG).Info("Model arrival rate result carried an error",
			"modelID", modelID, "namespace", namespace, "reason", reason)
		return 0
	}

	// Strictly target_model_name, with no model_name fallback — see the note on
	// QueryModelArrivalRate. The series for other models in this namespace are
	// skipped here rather than in PromQL.
	var arrivalRate float64
	for _, value := range result.Values {
		if value.Labels[seriesTargetModelLabel] != modelID {
			continue
		}
		if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value >= 0 {
			arrivalRate += value.Value
		}
	}

	logger.V(logging.DEBUG).Info("Collected model arrival rate",
		"modelID", modelID, "namespace", namespace, "arrivalRate", arrivalRate)

	return arrivalRate
}
