/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package collector provides replica metrics collection functionality.
//
// This package provides ReplicaMetricsCollector which collects replica-level
// metrics for both saturation analysis and queueing model analysis using the
// source infrastructure. Saturation metrics (KV cache, queue length, token
// capacity) and queueing model metrics (scheduler dispatch rate, max batch
// size) are collected together and exposed via the shared ReplicaMetrics struct.
//
// # Pod label fallback
//
// Every processing block in Refresh() extracts a pod identity from Prometheus
// labels using a two-step fallback:
//
//	podName := value.Labels["pod"]
//	if podName == "" {
//	    podName = value.Labels["pod_name"]
//	}
//
// Engine metrics are typically scraped via a PodMonitor or ServiceMonitor that
// applies the Prometheus operator's default target-relabeling, which produces
// a "pod" label. Some scrape configurations (e.g., raw Prometheus scrape jobs,
// kube-state-metrics–style configs) instead expose the pod identity as
// "pod_name". The fallback handles both conventions so the collector works
// regardless of how the Prometheus scrape is configured.
package collector

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/locator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
)

// ReplicaMetricsCollector collects replica-level metrics for both saturation
// analysis and queueing model analysis using the source infrastructure.
type ReplicaMetricsCollector struct {
	source    source.MetricsSource
	k8sClient client.Client
	recorder  record.EventRecorder
	locator   locator.PodLocator
	// metricsAvailableState tracks whether metrics were available in the previous
	// cycle for each VA (keyed by namespace/name). Used for edge-triggered events.
	metricsAvailableState map[string]bool
	mu                    sync.Mutex

	// cycleResults memoizes query results for the span of one optimize cycle,
	// keyed by query name and parameters. The queries are namespace-scoped (the
	// EPP flow-control ones cluster-scoped), so their key does not mention the
	// model and every model in a namespace reads the first one's fetch.
	// Nil outside a cycle — see BeginCycle.
	cycleResults map[source.CacheKey]*source.MetricResult
	cycleMu      sync.Mutex
}

// NewReplicaMetricsCollector creates a new replica metrics collector.
func NewReplicaMetricsCollector(metricsSource source.MetricsSource, k8sClient client.Client, recorder record.EventRecorder, podLocator locator.PodLocator) *ReplicaMetricsCollector {
	return &ReplicaMetricsCollector{
		source:                metricsSource,
		k8sClient:             k8sClient,
		recorder:              recorder,
		locator:               podLocator,
		metricsAvailableState: make(map[string]bool),
	}
}

// BeginCycle opens an optimize cycle, arming the memo that lets every model in a
// namespace share one execution of the namespace-scoped queries. Pair it with
// EndCycle.
//
// Sharing is deliberately opt-in per cycle rather than time-based: results are
// reused only within the collection they were fetched for, never carried into
// the next one. A caller that drives the collector without opening a cycle
// leaves the memo nil and refreshes independently every time — correct, just
// without the sharing.
func (c *ReplicaMetricsCollector) BeginCycle() {
	c.cycleMu.Lock()
	defer c.cycleMu.Unlock()
	c.cycleResults = make(map[source.CacheKey]*source.MetricResult)
}

// EndCycle closes the cycle opened by BeginCycle and releases the memoized
// results.
func (c *ReplicaMetricsCollector) EndCycle() {
	c.cycleMu.Lock()
	defer c.cycleMu.Unlock()
	c.cycleResults = nil
}

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

// recordUnattributedReadyPodsEvent emits a Warning/UnattributedReadyPods K8s event for va.
// Deduplication: at most one event per VA per cycle; vaEventTracker records which VAs have
// already received an event this cycle so repeated calls are no-ops for those VAs.
func (c *ReplicaMetricsCollector) recordUnattributedReadyPodsEvent(
	va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	readyCount int32,
	vaEventTracker map[string]bool,
) {
	if c.recorder == nil {
		return
	}
	key := utils.GetNamespacedKey(va.Namespace, va.Name)
	if vaEventTracker != nil {
		if _, ok := vaEventTracker[key]; ok { // one event per VA per cycle
			return
		}
	}
	c.recorder.Event(llmdVariantAutoscalingV1alpha1.EventTarget(va), corev1.EventTypeWarning, constants.K8SEventUnattributedReadyPods,
		fmt.Sprintf("%s has %d ready pod(s) but none attributed; "+
			"verify the pods' ownerReferences reach the scale target %q drives",
			va.Name, readyCount, va.Name))
	if vaEventTracker != nil {
		vaEventTracker[key] = true
	}
}

func (c *ReplicaMetricsCollector) recordMetricsUnavailableEvent(
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	vaEventTracker map[string]bool,
	reason string,
) {
	if c.recorder == nil {
		return
	}

	for _, va := range variantAutoscalings {
		key := utils.GetNamespacedKey(va.Namespace, va.Name)
		if vaEventTracker != nil {
			if _, ok := vaEventTracker[key]; ok { // ensures only one event is recorded per VA
				continue
			}
		}
		c.recorder.Event(llmdVariantAutoscalingV1alpha1.EventTarget(va), corev1.EventTypeWarning, constants.K8SEventMetricsUnavailable, reason)
		if vaEventTracker != nil {
			vaEventTracker[key] = true
		}
	}
}

// CollectReplicaMetrics collects per-replica metrics for all replicas of a model and records
// K8S events on failures. This wrapper ensures MetricsUnavailable events are emitted when
// metrics collection fails or returns no data, using edge-triggered emission (only on
// transitions from available → unavailable) to avoid flooding the event stream.
//
// The collected metrics serve both the saturation analyzer and the queueing model analyzer:
//   - Saturation metrics: KV cache usage, queue length, token capacity, prefix cache hit rate
//   - Queueing model metrics: scheduler dispatch rate (arrival rate), max batch size
//
// Parameters:
//   - ctx: Context for the operation
//   - modelID: The model identifier to collect metrics for
//   - namespace: The namespace where the model is deployed
//   - scaleTargets: Map of Deployment/LWS namespace/name to Deployment/LWS
//   - variantAutoscalings: Map of VariantAutoscaling namespace/name to VariantAutoscaling object
//
// Returns:
//   - []domain.ReplicaMetrics: Per-pod metrics for saturation and queueing model analysis
//   - error: Any error that occurred during collection
func (c *ReplicaMetricsCollector) CollectReplicaMetrics(
	ctx context.Context,
	modelID string,
	namespace string,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	vaEventTracker map[string]bool,
) ([]domain.ReplicaMetrics, error) {
	replicaMetrics, err := c.collectReplicaMetrics(ctx, modelID, namespace, scaleTargets)

	// Determine if metrics are available in this cycle
	metricsAvailable := err == nil && len(replicaMetrics) > 0

	// Check previous state and emit events only on available → unavailable transitions
	c.mu.Lock()
	defer c.mu.Unlock()

	for key, va := range variantAutoscalings {
		previouslyAvailable, seen := c.metricsAvailableState[key]

		// Edge-triggered: only emit event on available → unavailable transition
		// Don't emit on first observation (we don't know previous state - VA may have started at zero)
		shouldEmitEvent := seen && previouslyAvailable && !metricsAvailable

		if shouldEmitEvent {
			if err != nil {
				c.recordMetricsUnavailableEvent(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{key: va}, vaEventTracker, "Failed to collect metrics for model")
			} else if len(replicaMetrics) == 0 {
				c.recordMetricsUnavailableEvent(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{key: va}, vaEventTracker, "No saturation metrics available for model")
			}
		}

		// Update state for next cycle
		c.metricsAvailableState[key] = metricsAvailable
	}

	// Warn when a VA has Ready pods but none are attributed to it this cycle.
	// Only runs when the model produced at least one attributed replica — model-wide
	// emptiness is the availability path above; the scrape-lag gate keeps quiet there.
	if err == nil && len(replicaMetrics) > 0 {
		attributed := make(map[string]int, len(variantAutoscalings))
		for i := range replicaMetrics {
			attributed[replicaMetrics[i].VariantName]++
		}

		// A row that resolved to a variant this model does not own. The engine
		// never looks it up, so it is already ignored rather than mis-charged --
		// but ignored silently, and this is the one place that can see it, since
		// only here are "the variants of this model" and "what each row resolved
		// to" both in hand.
		//
		// An FMA launcher rebound to another model lands here: same pod, same
		// port, pairing now naming the new model's requester, while samples from
		// before the rebind are still inside the query window.
		owned := make(map[string]struct{}, len(variantAutoscalings))
		for _, va := range variantAutoscalings {
			owned[va.Name] = struct{}{}
		}
		foreign := make(map[string]int)
		for i := range replicaMetrics {
			name := replicaMetrics[i].VariantName
			if name == "" {
				continue
			}
			if _, ours := owned[name]; !ours {
				foreign[name]++
			}
		}
		for name, n := range foreign {
			metrics.IncPodMappingMiss(namespace, constants.PodMappingMissOtherModelVariant)
			ctrl.LoggerFrom(ctx).Info(
				"rows resolved to a variant this model does not own; ignoring them",
				"namespace", namespace, "model", modelID, "variant", name, "rows", n,
				"note", "expected briefly after an FMA launcher is rebound to another model; sustained means a pairing points at a variant of a model the pod does not serve")
		}

		for _, va := range variantAutoscalings {
			if attributed[va.Name] > 0 {
				continue
			}
			stKey := utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())
			st, ok := scaleTargets[stKey]
			if !ok || st == nil {
				continue
			}
			if ready := st.GetStatusReadyReplicas(); ready > 0 {
				ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("VA has ready pods but none attributed",
					"va", va.Name, "namespace", va.Namespace, "readyReplicas", ready)
				c.recordUnattributedReadyPodsEvent(va, ready, vaEventTracker)
			}
		}
	}

	if err != nil {
		return nil, err
	}
	return replicaMetrics, nil
}

// buildInstanceKey returns (instanceKey, podName, vaName) for a series's labels.
//
// vaName is resolved by walking the pod's ownerReferences to the managed scaler
// that drives it, and is the scaler's name. That walk is the only source of
// variant identity: the llm_d_ai_variant metric label used to short-circuit it,
// but neither vLLM nor SGLang emits that label, so on a real deployment it only
// ever appeared where an operator had relabelled it in — and it is no longer
// carried in the query groupings either (#1263).
//
// Returns vaName="" when the pod has no managed scaler above it; the caller
// treats that as "skip".
func (c *ReplicaMetricsCollector) buildInstanceKey(ctx context.Context, namespace string, labels map[string]string) (instanceKey, podName, vaName string) {
	podName = labels["pod"]
	if podName == "" {
		podName = labels["pod_name"]
	}

	if podName != "" && c.locator != nil {
		ms, err := c.locator.Locate(ctx, namespace, podName)
		switch {
		case err != nil:
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("locator.Locate failed; treating pod as unmanaged",
				"pod", podName, "namespace", namespace, "error", err)
		case ms == nil:
			// No managed scaler in the pod's owner chain — the pod is unmanaged.
			// Leaves vaName="" so the caller skips it.
		default:
			// The synthetic VariantAutoscaling is always keyed by the ScaledObject
			// name, so use it directly.
			vaName = ms.Name
		}
	}

	instance := labels["instance"]
	port := ""
	if instance != "" && podName != "" {
		if idx := strings.LastIndex(instance, ":"); idx != -1 {
			port = instance[idx+1:]
		}
	}

	switch {
	case podName != "" && port != "":
		instanceKey = podName + ":" + port
	case instance != "":
		instanceKey = instance
	case podName != "":
		instanceKey = podName
	default:
		return "", "", ""
	}
	return instanceKey, podName, vaName
}

// isLWSWorker checks if a pod is part of an LWS and is a worker (non-leader).
// Returns true if the pod has the leaderworkerset.sigs.k8s.io/worker-index label
// with a value other than "0" (leader pods have worker-index="0").
// Returns false for non-LWS pods or LWS leader pods.
// Uses the locator's GetPodLabels which reuses the same pod fetch that Locate performs.
func (c *ReplicaMetricsCollector) isLWSWorker(ctx context.Context, namespace, podName string) bool {
	if podName == "" || c.locator == nil {
		return false
	}

	labels := c.locator.GetPodLabels(ctx, namespace, podName)
	if labels == nil {
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("isLWSWorker: nil labels, treating pod as non-worker",
			"pod", podName, "namespace", namespace)
		return false
	}

	workerIndex, hasLabel := labels[lwsv1.WorkerIndexLabelKey]
	if !hasLabel {
		return false
	}

	return workerIndex != "0"
}

// collectReplicaMetrics is the internal implementation that collects per-replica metrics.
func (c *ReplicaMetricsCollector) collectReplicaMetrics(
	ctx context.Context,
	modelID string,
	namespace string,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
) ([]domain.ReplicaMetrics, error) {
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
		return nil, fmt.Errorf("failed to refresh replica metrics: %w", err)
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

	// podMetricData holds per-pod metric values and timestamps
	type podMetricData struct {
		podName        string // Actual pod name for K8s API lookups
		vaName         string // scaler name the pod's ownerReference walk resolved to
		kvUsage        float64
		kvTimestamp    time.Time
		hasKv          bool
		queueLen       int
		queueTimestamp time.Time
		hasQueue       bool
		// V2 fields for token-based capacity analysis
		numGpuBlocks                int64
		blockSize                   int64
		avgOutputTokens             float64
		avgOutputTokensTimestamp    time.Time
		avgInputTokens              float64
		avgInputTokensTimestamp     time.Time
		prefixCacheHitRate          float64
		prefixCacheHitRateTimestamp time.Time
		hasCacheConfig              bool
		cacheConfigTimestamp        time.Time
		// Queueing model fields
		avgITL                  float64
		avgITLTimestamp         time.Time
		avgServiceTime          float64
		avgServiceTimeTimestamp time.Time
		// Throughput analyzer fields
		generationTokenRate float64
		kvUsageInstant      float64
		requestRate         float64
	}

	// classifyTimestamp reports the freshness status of a single metric timestamp,
	// along with its age when non-zero. A zero timestamp means the metric was never
	// scraped for this pod and is classified "missing". Shared by trackMetricFreshness
	// (aggregate gauge) and worstFreshnessStatus (per-replica metadata) so the two
	// cannot drift apart.
	classifyTimestamp := func(timestamp, collectedAt time.Time, thresholds config.FreshnessThresholds) (status string, age time.Duration, hasTimestamp bool) {
		if timestamp.IsZero() {
			return "missing", 0, false
		}
		age = collectedAt.Sub(timestamp)
		return thresholds.DetermineStatus(age), age, true
	}

	// trackMetricFreshness determines the freshness status of metrics in podMetricData
	// and increments the corresponding counters in the freshness status map.
	trackMetricFreshness := func(
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
	worstFreshnessStatus := func(data *podMetricData, collectedAt time.Time) (string, time.Duration) {
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

	// Extract per-pod metrics from results
	podData := make(map[string]*podMetricData)

	// Process KV cache results
	if result := results[registration.QueryKvCacheUsage]; result != nil {
		if result.HasError() {
			return nil, fmt.Errorf("KV cache query failed: %w", result.Error)
		}
		for _, value := range result.Values {
			instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
			if instanceKey == "" {
				continue
			}

			if podData[instanceKey] == nil {
				podData[instanceKey] = &podMetricData{
					podName: podName,
					vaName:  vaName,
				}
			}
			podData[instanceKey].kvUsage = value.Value
			podData[instanceKey].kvTimestamp = value.Timestamp
			podData[instanceKey].hasKv = true

			logger.V(logging.DEBUG).Info("KV cache metric",
				"instanceKey", instanceKey,
				"pod", podName,
				"usage", value.Value,
				"usagePercent", value.Value*100)
		}
	}

	// Process queue length results
	if result := results[registration.QueryQueueLength]; result != nil {
		if result.HasError() {
			return nil, fmt.Errorf("queue length query failed: %w", result.Error)
		}
		for _, value := range result.Values {
			instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
			if instanceKey == "" {
				continue
			}

			if podData[instanceKey] == nil {
				podData[instanceKey] = &podMetricData{
					podName: podName,
					vaName:  vaName,
				}
			}
			podData[instanceKey].queueLen = int(value.Value)
			podData[instanceKey].queueTimestamp = value.Timestamp
			podData[instanceKey].hasQueue = true

			logger.V(logging.DEBUG).Info("Queue metric",
				"instanceKey", instanceKey,
				"pod", podName,
				"queueLength", int(value.Value))
		}
	}

	// Process cache config info results (V2)
	//
	// vllm:cache_config_info has no model_name label (see QueryCacheConfigInfo),
	// so it is queried namespace-wide and may include pods of other models in the
	// same namespace. Attach cache config only to instances already discovered by
	// the model-scoped KV/queue queries above; skip unknown instances so foreign
	// pods are not introduced into this model's metrics (and do not inflate the
	// discovered-pods / freshness counters).
	if result := results[registration.QueryCacheConfigInfo]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, podName, _ := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}

				data := podData[instanceKey]
				if data == nil {
					// Instance not seen by the model-scoped queries: it belongs to a
					// different model (or lacks KV/queue metrics) — not one of ours.
					continue
				}

				// Parse num_gpu_blocks and block_size from string labels
				if blocksStr, ok := value.Labels["num_gpu_blocks"]; ok && blocksStr != "" {
					if blocks, err := strconv.ParseInt(blocksStr, 10, 64); err == nil {
						data.numGpuBlocks = blocks
					}
				}
				if sizeStr, ok := value.Labels["block_size"]; ok && sizeStr != "" {
					if size, err := strconv.ParseInt(sizeStr, 10, 64); err == nil {
						data.blockSize = size
					}
				}
				if data.numGpuBlocks > 0 && data.blockSize > 0 {
					data.hasCacheConfig = true
					data.cacheConfigTimestamp = value.Timestamp
				}

				logger.V(logging.DEBUG).Info("Cache config info metric",
					"instanceKey", instanceKey,
					"pod", podName,
					"numGpuBlocks", data.numGpuBlocks,
					"blockSize", data.blockSize)
			}
		}
	}

	// Process SGLang cache config (structural difference from vLLM).
	//
	// SGLang exposes total KV-cache token capacity directly via
	// sglang:max_total_num_tokens (the value), rather than as
	// num_gpu_blocks/block_size labels. We map the capacity onto the existing
	// numGpuBlocks × blockSize computation by setting blockSize = 1 and
	// numGpuBlocks = capacity, so the downstream TotalKvCapacityTokens math is
	// unchanged. Only runs when an SGLang variant is present for this model.
	// Read through the physical key, which filterResultsToModel leaves alone
	// (cache_config_info is unpartitioned because the vLLM variant has no model
	// identity). The SGLang variant does carry model_name, so it is filtered
	// here.
	if containsEngine(engines, inferenceengine.EngineSGLang) {
		sglangCacheKey := registration.EngineQuery(inferenceengine.EngineSGLang, registration.QueryCacheConfigInfo)
		if result := results[sglangCacheKey]; result != nil && !result.HasError() {
			for _, value := range result.Values {
				if value.Labels[seriesModelLabel] != modelID {
					continue
				}
				instanceKey, podName, _ := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}
				data := podData[instanceKey]
				if data == nil {
					// Not seen by the model-scoped KV/queue queries — skip.
					continue
				}
				capacity := int64(value.Value)
				if capacity > 0 {
					data.numGpuBlocks = capacity
					data.blockSize = 1
					data.hasCacheConfig = true
					data.cacheConfigTimestamp = value.Timestamp
				}

				logger.V(logging.DEBUG).Info("SGLang cache config metric",
					"instanceKey", instanceKey,
					"pod", podName,
					"totalKvCapacityTokens", capacity)
			}
		}
	}

	// Process average output tokens results (V2)
	if result := results[registration.QueryAvgOutputTokens]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}

				if podData[instanceKey] == nil {
					podData[instanceKey] = &podMetricData{
						podName: podName,
						vaName:  vaName,
					}
				}
				// NaN check: rate division by zero produces NaN
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) {
					podData[instanceKey].avgOutputTokens = value.Value
					podData[instanceKey].avgOutputTokensTimestamp = value.Timestamp
				}
			}
		}
	}

	// Process average input tokens results (V2)
	if result := results[registration.QueryAvgInputTokens]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}

				if podData[instanceKey] == nil {
					podData[instanceKey] = &podMetricData{
						podName: podName,
						vaName:  vaName,
					}
				}
				// NaN check: rate division by zero produces NaN
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) {
					podData[instanceKey].avgInputTokens = value.Value
					podData[instanceKey].avgInputTokensTimestamp = value.Timestamp
				}
			}
		}
	}

	// Process prefix cache hit rate results (V2)
	if result := results[registration.QueryPrefixCacheHitRate]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}

				if podData[instanceKey] == nil {
					podData[instanceKey] = &podMetricData{
						podName: podName,
						vaName:  vaName,
					}
				}
				// NaN check: rate division by zero produces NaN when no prefix cache queries
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value >= 0 && value.Value <= 1 {
					podData[instanceKey].prefixCacheHitRate = value.Value
					podData[instanceKey].prefixCacheHitRateTimestamp = value.Timestamp
				}
			}
		}
	}

	// Process average ITL results (seconds)
	if result := results[registration.QueryAvgITL]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}

				if podData[instanceKey] == nil {
					podData[instanceKey] = &podMetricData{
						podName: podName,
						vaName:  vaName,
					}
				}
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value > 0 {
					podData[instanceKey].avgITL = value.Value
					podData[instanceKey].avgITLTimestamp = value.Timestamp

					logger.V(logging.DEBUG).Info("Avg ITL metric",
						"instanceKey", instanceKey,
						"pod", podName,
						"avgITLSeconds", value.Value)
				}
			}
		}
	}

	// Process average service time results (seconds, queue wait excluded)
	if result := results[registration.QueryAvgServiceTime]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}

				if podData[instanceKey] == nil {
					podData[instanceKey] = &podMetricData{
						podName: podName,
						vaName:  vaName,
					}
				}
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value > 0 {
					podData[instanceKey].avgServiceTime = value.Value
					podData[instanceKey].avgServiceTimeTimestamp = value.Timestamp

					logger.V(logging.DEBUG).Info("Avg service time metric",
						"instanceKey", instanceKey,
						"pod", podName,
						"avgServiceTimeSeconds", value.Value)
				}
			}
		}
	}

	// Process generation token rate results (tokens/sec) — throughput analyzer μ_dec^obs
	if result := results[registration.QueryGenerationTokenRate]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, _, _ := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}
				if podData[instanceKey] == nil {
					continue // skip pods the KV/queue queries didn't see (scrape skew)
				}
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value >= 0 {
					podData[instanceKey].generationTokenRate = value.Value
				}
			}
		}
	}

	// Process instantaneous KV usage (k*) results (0.0–1.0) — throughput analyzer k*
	if result := results[registration.QueryKvUsageInstant]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, _, _ := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}
				if podData[instanceKey] == nil {
					continue // skip pods the KV/queue queries didn't see (scrape skew)
				}
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value >= 0 && value.Value <= 1 {
					podData[instanceKey].kvUsageInstant = value.Value
				}
			}
		}
	}

	// Process engine request completion rate (req/s) — throughput analyzer fallback λ_req
	if result := results[registration.QueryRequestRate]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, _, _ := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}
				if podData[instanceKey] == nil {
					continue // skip pods the KV/queue queries didn't see (scrape skew)
				}
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value >= 0 {
					podData[instanceKey].requestRate = value.Value
				}
			}
		}
	}

	// Track metrics freshness status per pod
	vaMetricsFreshnessStatus := make(map[string]map[string]int)

	// Build replica metrics from pod data
	replicaMetrics := make([]domain.ReplicaMetrics, 0, len(podData))
	collectedAt := time.Now()

	// Pods attributed through the FMA pairing rather than their own owner chain,
	// summarised once after the loop.
	pairedAttributions := 0
	pairedExample := ""

	for instanceKey, data := range podData {
		// Use the actual pod name (not instance IP:port) for logging
		podName := data.podName
		if podName == "" {
			// Fallback: if pod name wasn't extracted from labels, use instanceKey
			// This handles cases where the metric doesn't have pod label
			podName = instanceKey
		}

		// The scaler this pod belongs to, resolved by buildInstanceKey's
		// ownerReference walk.
		vaName := data.vaName

		// Skip pods that have no metrics at all. This can happen when the query returns pods that
		// were scaled up then scaled down, i.e. no longer running in the namespace.
		if !data.hasKv && !data.hasQueue {
			continue
		}

		kvUsage := data.kvUsage
		queueLen := data.queueLen

		if !data.hasKv {
			logger.Info("Pod missing KV cache metrics, using 0",
				"pod", podName,
				"instance", instanceKey,
				"model", modelID,
				"namespace", namespace)
			kvUsage = 0
		}
		if !data.hasQueue {
			logger.Info("Pod missing queue metrics, using 0",
				"pod", podName,
				"instance", instanceKey,
				"model", modelID,
				"namespace", namespace)
			queueLen = 0
		}

		// A BRIDGE is attributed to the variant it is lent to, not to the pool
		// that owns it.
		//
		// A warm pool Pod's ownerReference walk reaches the POOL's scale target,
		// because that is what created it -- so the walk either finds nothing
		// this model drives, or finds the pool. Neither is the answer: while the
		// Pod is lent it is serving one variant's traffic, and that traffic is
		// what the analyzer has to see. Checked BEFORE the unattributed path so a
		// lent Pod is not reported as a mapping miss, and before the FMA hop
		// because the two cannot both apply.
		fromWarmPool := false
		if bridgeFor, lent := decision.BridgeVariant(namespace, podName, warmPoolLendingMaxAge, time.Now()); lent {
			if vaName != "" && vaName != bridgeFor {
				logger.V(logging.DEBUG).Info(
					"a warm pool Pod resolved to a scale target of its own; attributing it to the variant it is lent to",
					"pod", podName, "resolved", vaName, "lentTo", bridgeFor, "namespace", namespace)
			}
			vaName = bridgeFor
			fromWarmPool = true
		}

		if vaName == "" {
			// Neither the ownerReferences walk nor the FMA pairing hop reached a
			// managed scaler, so this pod's metrics belong to nothing this
			// optimizer drives. Count it so the otherwise-silent skip is
			// observable; the pod is unattributed, so the metric is keyed by
			// namespace and reason only.
			reason, why := unattributedReason(c.locator, ctx, namespace, podName)
			metrics.IncPodMappingMiss(namespace, reason)
			logger.Info("Skipping pod: nothing this optimizer drives could be resolved for it",
				"pod", podName,
				"instance", instanceKey,
				"reason", reason,
				"detail", why,
				"scale targets", getScaleTargetNames(scaleTargets))
			continue
		}
		if isFMALauncher(c.locator, ctx, namespace, podName) {
			// A launcher's owner chain cannot reach a scale target, so if it
			// resolved at all it did so through the pairing. Count it, and report
			// once per cycle below: without this, a working hop and a broken one
			// both look like silence.
			pairedAttributions++
			if pairedExample == "" {
				pairedExample = podName + " -> " + vaName
			}
		}

		// Skip LWS worker pods (non-leaders). Only LWS leader pods (worker-index="0")
		// should be included in ReplicaMetrics, as they represent the LWS replica.
		// For LWS, each leader pod emits vLLM metrics representing the entire replica
		// (leader + workers), so including worker pods would double-count metrics.
		if c.isLWSWorker(ctx, namespace, podName) {
			logger.V(logging.DEBUG).Info("Skipping LWS worker pod (non-leader)",
				"pod", podName,
				"instance", instanceKey,
				"namespace", namespace)
			continue
		}

		// Compute V2 derived fields (zero-valued when unavailable, backward compatible)
		var totalKvCapacityTokens int64
		var tokensInUse int64
		if data.hasCacheConfig {
			// Overflow-safe multiplication: check before computing
			if data.numGpuBlocks > 0 && data.blockSize > math.MaxInt64/data.numGpuBlocks {
				totalKvCapacityTokens = math.MaxInt64
			} else {
				totalKvCapacityTokens = data.numGpuBlocks * data.blockSize
			}
			// Use math.Round for accurate float-to-int conversion and clamp to valid range
			rounded := math.Round(kvUsage * float64(totalKvCapacityTokens))
			if rounded < 0 {
				rounded = 0
			} else if rounded > float64(totalKvCapacityTokens) {
				rounded = float64(totalKvCapacityTokens)
			}
			tokensInUse = int64(rounded)
		}

		// Track freshness for metrics in this pod
		trackMetricFreshness(vaName, data, collectedAt, vaMetricsFreshnessStatus)
		freshnessStatus, freshnessAge := worstFreshnessStatus(data, collectedAt)
		metric := domain.ReplicaMetrics{
			PodName:               podName,
			ModelID:               modelID,
			Namespace:             namespace,
			VariantName:           vaName,
			FromWarmPool:          fromWarmPool,
			KvCacheUsage:          kvUsage,
			QueueLength:           queueLen,
			NumGpuBlocks:          data.numGpuBlocks,
			BlockSize:             data.blockSize,
			TotalKvCapacityTokens: totalKvCapacityTokens,
			TokensInUse:           tokensInUse,
			AvgOutputTokens:       data.avgOutputTokens,
			AvgInputTokens:        data.avgInputTokens,
			PrefixCacheHitRate:    data.prefixCacheHitRate,
			AvgITL:                data.avgITL,
			AvgServiceTime:        data.avgServiceTime,
			GenerationTokenRate:   data.generationTokenRate,
			KvUsageInstant:        data.kvUsageInstant,
			RequestRate:           data.requestRate,
			Metadata: &domain.ReplicaMetricsMetadata{
				CollectedAt:     collectedAt,
				Age:             freshnessAge,
				FreshnessStatus: freshnessStatus,
			},
		}

		replicaMetrics = append(replicaMetrics, metric)
	}

	// One line per cycle when the FMA pairing hop carried anything, because the
	// alternative is that a working hop and a broken one are both silent. An
	// operator who has just applied the launcher PodMonitor needs to see this
	// appear; one who is debugging a flat variant needs to see that it does not.
	if pairedAttributions > 0 {
		logger.Info("Attributed FMA launcher pods through their dual-pods pairing",
			"count", pairedAttributions,
			"example", pairedExample,
			"model", modelID,
			"namespace", namespace)
	}

	for vaName, statuses := range vaMetricsFreshnessStatus {
		for status, count := range statuses {
			metrics.SetMetricsFreshnessStatus(vaName, status, count)
		}
	}

	// Merge each pod's engine instances into one replica. Everything above works
	// per instance because that is how the engine is scraped ("pod:port", one
	// series per DP rank); everything below the collector counts in scale-target
	// replicas. See collapseToPods.
	instanceCount := len(replicaMetrics)
	replicaMetrics = collapseToPods(replicaMetrics)

	// Only set this after all pods have been processed, making sure not to include pods without metrics (which are skipped above).
	// This ensures that the discovered pod count reflects only those pods that produced replica metrics.
	metrics.SetMetricsPodsDiscovered(namespace, len(replicaMetrics))
	logger.V(logging.DEBUG).Info("Collected replica metrics",
		"modelID", modelID,
		"namespace", namespace,
		"replicaCount", len(replicaMetrics),
		"engineInstances", instanceCount)

	return replicaMetrics, nil
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

// CollectModelArrivalRate collects the model-level request arrival rate (req/s)
// from the llm-d inference scheduler. It sums the source metric across the whole
// model with no pod_name/port labels to reconcile against vLLM's per-instance
// metrics — which is exactly why the per-instance form of this query no longer
// exists. Returns 0 (not an error) when the metric is unavailable.
// warmPoolLendingMaxAge is how old the pool's lending map may be before a Pod in
// it stops being treated as a bridge.
//
// The pool republishes every reconcile pass (5s), so anything approaching this
// means its reconciler has stopped. Attributing a Pod to a variant on a lending
// that may since have ended would add demand for load nobody is carrying, and
// keep adding it for as long as the controller ran.
const warmPoolLendingMaxAge = 2 * time.Minute

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

// isFMALauncher reports whether a pod is a Fast Model Actuation server-providing
// pod. Its owner chain cannot reach a scale target by design, so a launcher that
// resolved did so through the pairing hop.
//
// Labels come from the locator's cache, which the attribution walk has already
// populated for this pod, so this costs a map lookup and no API call.
func isFMALauncher(loc locator.PodLocator, ctx context.Context, namespace, podName string) bool {
	if loc == nil {
		return false
	}
	return loc.GetPodLabels(ctx, namespace, podName)[constants.ComponentLabelKey] == constants.LauncherComponent
}

// unattributedReason classifies why a pod resolved to no managed scaler, and
// returns the metric reason plus a human-readable detail for the log.
//
// The three outcomes are deliberately kept apart. A warm FMA spare is expected
// and permanent, and burying it in the same counter as a real bug makes the
// counter useless in exactly the namespaces where attribution is hardest. A
// launcher that declared a pairing and still did not resolve is worth chasing.
// Anything else is the ordinary "this pod is not ours" case.
//
// What it deliberately does NOT do is guess between the causes of the second
// case — partner deleted, partner unmanaged, or a model disagreement. The
// collector cannot tell them apart, and this investigation began with a log line
// that named one cause for a multi-cause condition and sent every reader after
// the wrong one.
func unattributedReason(loc locator.PodLocator, ctx context.Context, namespace, podName string) (reason, detail string) {
	if loc == nil {
		return constants.PodMappingMissUnresolved, "no locator wired"
	}
	labels := loc.GetPodLabels(ctx, namespace, podName)
	if labels[constants.ComponentLabelKey] != constants.LauncherComponent {
		return constants.PodMappingMissUnresolved,
			"the walk up its ownerReferences reached no Deployment or LWS under a ScaledObject"
	}
	if partner := labels[constants.DualPodsPairLabelKey]; partner != "" {
		return constants.PodMappingMissPairingUnresolved,
			"FMA launcher paired with " + partner + ", which itself resolved to no scale target, no longer exists, or serves a different model"
	}
	return constants.PodMappingMissUnboundLauncher,
		"FMA launcher with no bound instance; it is a warm spare and is serving nothing"
}

// getScaleTargetNames extracts scale target names from the scale target map.
func getScaleTargetNames(scaleTargets map[string]scaletarget.ScaleTargetAccessor) []string {
	names := make([]string, 0, len(scaleTargets))
	for _, scaleTarget := range scaleTargets {
		names = append(names, scaleTarget.GetName())
	}
	return names
}
