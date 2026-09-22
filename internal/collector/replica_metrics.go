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
	"sync"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/locator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
)

// BridgeResolver answers which variant a warm-pool Pod is lent to right now.
// The decision store implements it; the collector takes it as an interface so
// that collecting does not import deciding -- the lending is a decision
// output that reaches the collector as an input, the way everything else the
// collector attributes by does.
type BridgeResolver interface {
	VariantFor(namespace, pod string, maxAge time.Duration, now time.Time) (string, bool)
}

// ReplicaMetricsCollector collects replica-level metrics for both saturation
// analysis and queueing model analysis using the source infrastructure.
//
// One collection is four steps, one file each: the queries (query.go), the
// series into per-instance data (extract.go), each instance into the row the
// analyzers read -- or a reason it is not one (attribute.go), with the
// series' freshness classified beside it (freshness.go); then the instances
// of one Pod are merged into its replica (pod_collapse.go).
type ReplicaMetricsCollector struct {
	source    source.MetricsSource
	k8sClient client.Client
	// bridges says which Pods are lent bridges this cycle and to whom. Nil
	// means no warm pool: no Pod is a bridge.
	bridges BridgeResolver
	// apiReader is UNCACHED, and is how Pod serving state is read. The manager's
	// cache holds no Pods, so reading them through k8sClient would start a Pod
	// informer -- cluster-wide on a cluster-scoped install. See podStates.
	apiReader client.Reader
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
	// cyclePods is namespace -> pod name -> serving state, listed once per
	// namespace per cycle. Nil outside a cycle, like cycleResults.
	cyclePods map[string]namespacePods
	cycleMu   sync.Mutex
}

// NewReplicaMetricsCollector creates a new replica metrics collector.
func NewReplicaMetricsCollector(metricsSource source.MetricsSource, k8sClient client.Client, apiReader client.Reader, recorder record.EventRecorder, podLocator locator.PodLocator) *ReplicaMetricsCollector {
	return &ReplicaMetricsCollector{
		source:                metricsSource,
		k8sClient:             k8sClient,
		apiReader:             apiReader,
		recorder:              recorder,
		locator:               podLocator,
		metricsAvailableState: make(map[string]bool),
	}
}

// SetBridgeResolver names where the warm pool's lending is read from. The
// engine wires the decision store here; a collector without one attributes
// every Pod by its owner walk alone.
func (c *ReplicaMetricsCollector) SetBridgeResolver(r BridgeResolver) {
	c.bridges = r
}

// warmPoolLendingMaxAge is how old the pool's lending map may be before a Pod in
// it stops being treated as a bridge.
//
// The pool republishes every reconcile pass (5s), so anything approaching this
// means its reconciler has stopped. Attributing a Pod to a variant on a lending
// that may since have ended would add demand for load nobody is carrying, and
// keep adding it for as long as the controller ran.
const warmPoolLendingMaxAge = 2 * time.Minute

// bridgeFor asks the resolver, if there is one, whether pod is a lent bridge.
func (c *ReplicaMetricsCollector) bridgeFor(namespace, pod string) (string, bool) {
	if c.bridges == nil {
		return "", false
	}
	return c.bridges.VariantFor(namespace, pod, warmPoolLendingMaxAge, time.Now())
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
	c.cyclePods = make(map[string]namespacePods)
}

// EndCycle closes the cycle opened by BeginCycle and releases the memoized
// results.
func (c *ReplicaMetricsCollector) EndCycle() {
	c.cycleMu.Lock()
	defer c.cycleMu.Unlock()
	c.cycleResults = nil
	c.cyclePods = nil
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

// collectReplicaMetrics is the internal implementation that collects per-replica metrics.
func (c *ReplicaMetricsCollector) collectReplicaMetrics(
	ctx context.Context,
	modelID string,
	namespace string,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
) ([]domain.ReplicaMetrics, error) {
	logger := ctrl.LoggerFrom(ctx)

	results, engines, err := c.queryReplicaSeries(ctx, modelID, namespace, scaleTargets)
	if err != nil {
		return nil, err
	}
	podData, err := c.extractPodMetrics(ctx, modelID, namespace, engines, results)
	if err != nil {
		return nil, err
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
		metric, pairedAs, ok := c.attributeInstance(ctx, modelID, namespace, instanceKey, data, collectedAt, scaleTargets, vaMetricsFreshnessStatus)
		if pairedAs != "" {
			pairedAttributions++
			if pairedExample == "" {
				pairedExample = pairedAs
			}
		}
		if !ok {
			continue
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

	// Only set this after all pods have been processed, making sure not to include pods without metrics (which attributeInstance skips).
	// This ensures that the discovered pod count reflects only those pods that produced replica metrics.
	metrics.SetMetricsPodsDiscovered(namespace, len(replicaMetrics))
	logger.V(logging.DEBUG).Info("Collected replica metrics",
		"modelID", modelID,
		"namespace", namespace,
		"replicaCount", len(replicaMetrics),
		"engineInstances", instanceCount)

	return replicaMetrics, nil
}
