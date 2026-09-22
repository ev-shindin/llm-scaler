package collector

import (
	"context"
	"math"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/locator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// minUptimeForServiceTimeBound is how long a Pod must have been running before
// its uptime is used to bound the service time it reports.
//
// Not slack on the comparison -- a request cannot outlive the process that
// served it, and that holds from the first second. It is a floor on the input:
// in a Pod's first moments its uptime is the same order as the scrape interval
// and the query's own evaluation lag, so the comparison would be measuring the
// metrics pipeline rather than the engine. Anything an engine can report before
// it has been up a minute is a startup artifact the readiness gate above has
// already dropped; this only has to cover a Pod that passes readiness
// immediately, which is short-lived by definition.
const minUptimeForServiceTimeBound = time.Minute

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
	podName = seriesPodName(labels)

	// A series is not evidence that the Pod behind it still exists.
	//
	// Prometheus keeps a Pod's series for about five minutes after it goes, and
	// the locator resolves a Pod name to its scale target from a cache that is
	// deliberately never invalidated -- correct, since ownerReferences cannot
	// change, and together the reason a DELETED Pod went on contributing supply
	// long after it was gone. Measured on pokprod: a variant with one replica
	// reported four.
	//
	// Dropped HERE rather than at row assembly because this is the one point
	// every series from every query passes through, so nothing downstream can
	// use a Pod this rejected. An empty instanceKey is the existing "skip"
	// signal and every caller already honours it, which also keeps these out of
	// the mapping-miss count: a Pod that has been deleted is not a Pod WVA
	// failed to map.
	if podName != "" && c.podIsGone(ctx, namespace, podName) {
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info(
			"dropping a series whose Pod is gone or terminating",
			"pod", podName, "namespace", namespace)
		return "", "", ""
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

// attributeInstance turns one engine instance's scraped data into the
// ReplicaMetrics row the analyzers read, or says why it is not one: an
// instance with neither of the two saturation series, one nothing this
// optimizer drives could be resolved for, or an LWS worker (its leader
// reports for the replica). Attribution is the ownerReference walk
// buildInstanceKey did, overridden for a warm-pool bridge by the variant it
// is lent to; the FMA pairing hop is reported through pairedAs ("pod ->
// variant", empty otherwise) so the caller can say once per cycle that the
// hop carried something -- set even when the row is then dropped as a
// worker, as it was counted before. Per-request timings are dropped for a
// Pod that is not Ready or that reports a request older than itself; the
// rest is taken as scraped. Each series' freshness is folded into
// freshness (per variant) and the worst of them into the row's metadata.
func (c *ReplicaMetricsCollector) attributeInstance(
	ctx context.Context,
	modelID string,
	namespace string,
	instanceKey string,
	data *podMetricData,
	collectedAt time.Time,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	freshness map[string]map[string]int,
) (domain.ReplicaMetrics, string, bool) {
	logger := ctrl.LoggerFrom(ctx)
	pairedAs := ""

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
		return domain.ReplicaMetrics{}, "", false
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
	if bridgeFor, lent := c.bridgeFor(namespace, podName); lent {
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
		return domain.ReplicaMetrics{}, "", false
	}
	if isFMALauncher(c.locator, ctx, namespace, podName) {
		// A launcher's owner chain cannot reach a scale target, so if it
		// resolved at all it did so through the pairing. Say so, and the
		// caller reports once per cycle: without this, a working hop and a
		// broken one both look like silence.
		pairedAs = podName + " -> " + vaName
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
		return domain.ReplicaMetrics{}, "", false
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
	trackMetricFreshness(vaName, data, collectedAt, freshness)
	freshnessStatus, freshnessAge := worstFreshnessStatus(data, collectedAt)
	// Read from the Pod, never inferred from the scrape. A row exists
	// because something answered /metrics, which happens before the Pod
	// is Ready; see domain.ReplicaMetrics.Ready.
	ready := c.podReady(ctx, namespace, podName)
	// Per-request timing (service time, ITL) is excluded for a pod that
	// hasn't passed readiness -- unlike TokensInUse/KvCacheUsage below,
	// which the Ready field's own doc comment explains are counted
	// regardless, because a starting Pod's GPU and KV cache are real.
	// Timing is different: it describes the cost of a REQUEST, not a
	// resource the Pod holds, and a pod still failing its readiness probe
	// is exactly the population most likely to report a startup artifact
	// rather than a real one. Observed directly on a live run: a decode
	// pod failing its readiness probe reported a batch of "completions"
	// averaging ~145 hours of service time each, which was then averaged
	// unweighted across the fleet by the arrival-rate demand floor and inflated
	// the demand floor ~590x for several minutes. Excluding it here is a
	// second, independent layer under that call site's own median-based
	// aggregation -- if a value like this shouldn't be trusted, the
	// cleanest place to say so is where the Pod's own trust state is
	// already known, not downstream in every consumer.
	//
	// Timing ONLY. AvgInputTokens/AvgOutputTokens are the same kind of
	// per-request quantity and were considered here, but they are also read
	// per-replica by the saturation analyzer's waitingQueueDemand to price
	// a pod's waiting queue. A starting pod's queue is work the fleet has
	// already accepted, so zeroing its shape would erase real demand and
	// read as "idle" -- the failure this whole change exists to avoid,
	// pointing the other way. Their outlier defence lives in
	// the throughput floor, the only consumer that aggregates them across
	// replicas.
	avgITL := data.avgITL
	avgServiceTime := data.avgServiceTime
	if !ready {
		// At DEFAULT, not DEBUG. The incident this guards against was
		// visible only as a moved demand floor; now that both layers
		// discard the reading, a silent drop would leave the NEXT
		// occurrence with no trace at all -- and DEBUG is V(4) against a
		// shipped default of -v=2, so a DEBUG line would be invisible in
		// exactly the run that needs it. Logged only when there was
		// something to drop, so a normally-starting pod stays quiet.
		if data.avgITL > 0 || data.avgServiceTime > 0 {
			logger.V(logging.DEFAULT).Info("dropping timing metrics from a not-Ready pod",
				"pod", podName,
				"namespace", namespace,
				"variant", vaName,
				"avgServiceTime", data.avgServiceTime,
				"avgITL", data.avgITL)
		}
		avgITL = 0
		avgServiceTime = 0
	}
	// Second guard, independent of readiness: no request can have taken
	// longer than the Pod reporting it has been running.
	//
	// Every filter above this point tests one value in isolation -- NaN,
	// Inf, negative, outside [0,1] -- and the reading that caused the
	// incident passed all of them: 521368 is finite, positive, and a
	// perfectly ordinary float. What makes 145 hours impossible is not the
	// number itself but the Pod: it had just started, and was still failing
	// its readiness probe when it reported an average request older than
	// its own process.
	//
	// The Pod's uptime is used rather than any arithmetic on the other
	// metrics, and the difference matters. An earlier version of this guard
	// compared the measured service time against the decode-only
	// reconstruction AvgOutputTokens x AvgITL and rejected large ratios.
	// That is wrong for a shape this project actually serves: service time
	// is prefill + O x ITL, so the ratio is 1 + prefill/(O x ITL) and grows
	// without bound as O shrinks. At 30-50k prefill and a SINGLE decode
	// token -- reranking, classification, scoring long documents -- the
	// ratio runs to several hundred while every number involved is
	// correct, and rejecting it would discard a real measurement and drop
	// the floor to a decode-only estimate that understates that shape by
	// the same factor. Uptime carries no assumption about the shape at all.
	//
	// Fails OPEN, like podReady: an unreadable listing, a Pod absent from
	// it, or a Pod with no start time yields no bound, and a bound that
	// cannot be established must not delete a value it cannot judge.
	//
	// The minimum uptime is not a tolerance on the comparison, which is
	// exact. It is a guard on the INPUT: within the first seconds a Pod's
	// uptime and its first scrape are separated by scrape interval and
	// evaluation lag, and comparing against a number that small measures
	// the pipeline rather than the engine. Readiness already covers most of
	// that window; this covers a Pod that passes readiness immediately.
	if avgServiceTime > 0 {
		if uptime, known := c.podUptime(ctx, namespace, podName, collectedAt); known &&
			uptime >= minUptimeForServiceTimeBound && avgServiceTime > uptime.Seconds() {
			logger.V(logging.DEFAULT).Info("dropping a service time longer than the pod has existed",
				"pod", podName,
				"namespace", namespace,
				"variant", vaName,
				"avgServiceTime", avgServiceTime,
				"podUptimeSeconds", uptime.Seconds())
			avgServiceTime = 0
		}
	}
	metric := domain.ReplicaMetrics{
		PodName:               podName,
		ModelID:               modelID,
		Namespace:             namespace,
		VariantName:           vaName,
		FromWarmPool:          fromWarmPool,
		Ready:                 ready,
		KvCacheUsage:          kvUsage,
		QueueLength:           queueLen,
		NumGpuBlocks:          data.numGpuBlocks,
		BlockSize:             data.blockSize,
		TotalKvCapacityTokens: totalKvCapacityTokens,
		TokensInUse:           tokensInUse,
		AvgOutputTokens:       data.avgOutputTokens,
		AvgInputTokens:        data.avgInputTokens,
		PrefixCacheHitRate:    data.prefixCacheHitRate,
		AvgITL:                avgITL,
		AvgServiceTime:        avgServiceTime,
		GenerationTokenRate:   data.generationTokenRate,
		KvUsageInstant:        data.kvUsageInstant,
		RequestRate:           data.requestRate,
		Metadata: &domain.ReplicaMetricsMetadata{
			CollectedAt:     collectedAt,
			Age:             freshnessAge,
			FreshnessStatus: freshnessStatus,
		},
	}

	return metric, pairedAs, true
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
