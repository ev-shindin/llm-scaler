package saturation_v2

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// SaturationAnalyzer implements the domain.Analyzer interface using a
// token-based capacity model with memory-bound (k1) and compute-bound (k2)
// constraints. It is the sole saturation analysis path.
type SaturationAnalyzer struct {
	// mu protects computeCapacityHistory from concurrent access.
	mu sync.Mutex
	// computeCapacityHistory stores rolling averages of observed k2 values,
	// keyed by "modelID|accelerator|gpuCount|role|outputBucket|queueThreshold".
	// TODO: check if we need to use other model parameters as key in the future.
	computeCapacityHistory map[string]*rollingAverage
	// lastAccelerator remembers, per variant, the last accelerator that actually
	// RESOLVED.
	//
	// A workload that pins no GPU product has its accelerator observed from the
	// nodes its pods landed on (variantmeta.observeAcceleratorFromNodes), and that
	// observation REQUIRES AGREEMENT: pods on two different products report "no
	// single answer" and the variant reads unresolved. That is the right
	// accounting answer -- charging a spread fleet to one pool is the bug the
	// removed acceleratorName label used to cause -- but it makes the value change
	// with pod PLACEMENT, and the accelerator is part of the k2 history key.
	//
	// On a heterogeneous cluster the result is a self-sustaining oscillation, and
	// this is measured rather than theorised. A kind cluster labelled
	// NVIDIA-H100-SXM5-80GB on one node and NVIDIA-A100-PCIE-80GB on another: at
	// one replica the fleet is single-type and resolves, so k2 comes from history;
	// the scale-up puts the second replica on the other product, resolution
	// reports no single answer, the key moves to an empty bucket, k2 falls back to
	// k1 and capacity jumps 2 -> 8; the variant then scales back DOWN, which
	// restores single-type placement and the cycle repeats. Scaling up is what
	// destroys the deduction; losing the deduction is what causes the scale-down.
	//
	// Keying history on the last resolved accelerator breaks that loop without
	// touching the accounting: this is a cache key for a capacity estimate, not a
	// statement about which pool owns the GPUs. The cost is that a genuinely mixed
	// fleet averages observations from two hardware types under one key, which is
	// coarse -- and still strictly better than alternating between a real estimate
	// and k1 every time a replica lands elsewhere.
	lastAccelerator map[string]acceleratorMemo
	capacityStore   *CapacityKnowledgeStore
}

// acceleratorMemo is the last accelerator that resolved for a variant, with the
// time it was last used -- so it can age out on the same timeout as the k2
// history it keys.
type acceleratorMemo struct {
	name     string
	lastUsed time.Time
}

// NewSaturationAnalyzer creates a new V2 saturation analyzer backed by the
// given capacity store.
func NewSaturationAnalyzer(store *CapacityKnowledgeStore) *SaturationAnalyzer {
	return &SaturationAnalyzer{
		computeCapacityHistory: make(map[string]*rollingAverage),
		lastAccelerator:        make(map[string]acceleratorMemo),
		capacityStore:          store,
	}
}

// Name returns the analyzer identifier for logging and result metadata.
// Note: the config value "saturation" (in analyzerName YAML field) selects this analyzer,
// but the descriptive name here is used in AnalyzerResult.AnalyzerName for observability.
func (a *SaturationAnalyzer) Name() string {
	return "saturation-token-based"
}

// EvictStaleHistory removes k2 history entries that have not been updated
// within the given timeout. This prevents unbounded memory growth from
// deleted models or workload buckets that are no longer active.
//
// It prunes the accelerator memo on the same timeout, and here rather than in a
// second sweep so the two cannot drift: both are per-variant state that exists
// only to key or stabilise capacity, and a variant that has gone quiet for the
// timeout has no use for either. The returned count remains the number of
// HISTORY entries evicted, which is what its callers report.
func (a *SaturationAnalyzer) EvictStaleHistory(timeout time.Duration) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	evicted := 0
	for key, ra := range a.computeCapacityHistory {
		if time.Since(ra.lastUpdated) > timeout {
			delete(a.computeCapacityHistory, key)
			evicted++
		}
	}
	for key, memo := range a.lastAccelerator {
		if time.Since(memo.lastUsed) > timeout {
			delete(a.lastAccelerator, key)
		}
	}
	return evicted
}

// Analyze computes capacity signals for a model across all its variants.
func (a *SaturationAnalyzer) Analyze(ctx context.Context, input domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	satConfig, ok := input.Config.(*config.ScalingPolicy)
	if !ok {
		return nil, fmt.Errorf("expected *ScalingPolicy, got %T", input.Config)
	}
	logger := ctrl.LoggerFrom(ctx)

	// Build GPU count and P/D role lookups from variant states. The role decides
	// how a waiting request is charged against a replica's KV capacity, so it must
	// be known before per-replica demand is computed.
	// Accelerator joins these two: it is variant-level identity from discovery, not
	// a per-instance measurement, so it is read from the variant state rather than
	// repeated on every ReplicaMetrics record.
	gpusByVariant := make(map[string]int, len(input.VariantStates))
	rolesByVariant := make(map[string]string, len(input.VariantStates))
	accelByVariant := make(map[string]string, len(input.VariantStates))
	for _, vs := range input.VariantStates {
		gpusByVariant[vs.VariantName] = vs.GPUsPerReplica
		rolesByVariant[vs.VariantName] = vs.Role
		accelByVariant[vs.VariantName] = vs.AcceleratorName
	}

	// Phase 1: Per-replica capacity computation
	replicaCapacities := make([]ReplicaCapacity, 0, len(input.ReplicaMetrics))
	for _, rm := range input.ReplicaMetrics {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		gpuCount := gpusByVariant[rm.VariantName]
		rc := a.computeReplicaCapacity(rm, satConfig, input.ModelID, input.Namespace, gpuCount,
			rolesByVariant[rm.VariantName], accelByVariant[rm.VariantName], logger)
		if rc != nil {
			replicaCapacities = append(replicaCapacities, *rc)
		}
	}

	// Phase 2: Per-variant aggregation
	variantCapacities := a.aggregateByVariant(replicaCapacities, input.ReplicaMetrics, input.VariantStates, input.ModelID, input.Namespace, satConfig.KvCacheThreshold, logger)

	// Model-level demand D (the analyzer owns demand attribution). Supply,
	// utilization, and RoleCapacities are assembled downstream by the engine's
	// capacity-build step from the per-variant capacities and RoleDemand, so they
	// are not set here — the analyzer emits only the measured (D, P) signal.
	totalDemand := aggregation.SumTotalDemand(variantCapacities)

	// Track active roles for queue demand attribution.
	activeRoles := make(map[string]bool)
	for _, vc := range variantCapacities {
		activeRoles[canonicalRole(vc.Role)] = true
	}

	// Add scheduler queue demand (requests queued upstream in llm-d flow control).
	queueDemand := estimateSchedulerQueueDemand(input.SchedulerQueue, input.ReplicaMetrics, activeRoles)
	totalDemand += queueDemand.total
	if input.SchedulerQueue != nil {
		logger.Info("scheduler-queue-demand",
			"modelID", input.ModelID, "namespace", input.Namespace,
			"eppQueueSize", input.SchedulerQueue.QueueSize, "eppQueueBytes", input.SchedulerQueue.QueueBytes,
			"estimatedTokens", queueDemand.total, "byRole", queueDemand.byRole)
	}

	// Per-role demand attribution (P/D disaggregation); nil when non-disaggregated.
	// The builder pairs this with the per-role supply it recomputes.
	roleDemand := a.aggregateRoleDemand(variantCapacities, queueDemand.byRole)

	// Floor the demand at what the offered load requires (arrival_demand.go).
	//
	// Everything above measures occupancy, which falls as capacity rises: a fleet
	// that is keeping up looks idle, and the target follows the signal down. The
	// floor is computed from arrival rate, request shape and per-token cost --
	// none of which move when replicas are added -- so it holds the fleet at the
	// size the LOAD implies once occupancy stops implying anything.
	//
	// Strictly a floor. It never lowers demand, so it cannot authorise a
	// scale-down that occupancy would not already have authorised on its own.
	if floor := estimateArrivalDemand(input); floor.Tokens > totalDemand {
		logger.Info("arrival-demand-floor",
			"modelID", input.ModelID, "namespace", input.Namespace,
			"occupancyDemand", totalDemand, "flooredTo", floor.Tokens,
			"arrivalRate", floor.Lambda, "serviceTimeSec", floor.W, "serviceTimeFrom", floor.WSource,
			"tokensPerRequest", floor.TokensPerRequest)
		totalDemand = floor.Tokens
		raiseRoleDemandTo(roleDemand, totalDemand)
	} else if floor.Reason != "" && (floor.HasArrivalSignal || totalDemand > 0) {
		// Not an error: a model with no traffic has no arrival rate, and a fleet
		// nobody is using should not be held up by a fabricated floor. Logged so
		// that "the floor never binds" can be told apart from "the floor could
		// never be computed", which look identical from the outside.
		//
		// At DEFAULT, because that is the only level that ships: -v defaults to
		// logging.DEFAULT, so a V(DEBUG) line here would be invisible in every
		// real deployment and this comment would be describing something that
		// never happens.
		//
		// The gate is two-sided, because the two ways this declines to answer
		// need opposite treatment.
		//
		// HasArrivalSignal means load IS arriving and could not be sized -- a
		// real gap, worth saying however quiet the fleet looks. Gating that on
		// occupancy would lose it exactly when it matters: a sample landing
		// between completions, or a replica still warming, reads zero occupancy
		// while requests are demonstrably arriving.
		//
		// Occupancy covers the other side, where nothing is arriving at all.
		// Note that gating on len(ReplicaMetrics) instead would NOT work: an idle
		// pod still reports metrics -- the engine publishes 0 rather than nothing
		// -- so a model parked at minReplicaCount 1 would log every cycle
		// forever. Occupancy drains to zero within the metric's own 1m window, so
		// this self-extinguishes a few lines after traffic stops.
		//
		// It does not bound every case: a fleet that is genuinely serving on an
		// engine publishing neither timing keeps Reason set indefinitely. That is
		// a misconfiguration worth shouting about rather than one worth
		// suppressing, so it is left loud.
		logger.V(logging.DEFAULT).Info("arrival-demand-floor unavailable",
			"modelID", input.ModelID, "namespace", input.Namespace, "reason", floor.Reason)
	}

	result := &domain.AnalyzerResult{
		AnalyzerName:      a.Name(),
		ModelID:           input.ModelID,
		Namespace:         input.Namespace,
		AnalyzedAt:        time.Now(),
		VariantCapacities: variantCapacities,
		TotalDemand:       totalDemand,
		RoleDemand:        roleDemand,
	}

	return result, nil
}

// computeReplicaCapacity computes the capacity breakdown for a single replica.
// The role argument is the replica's P/D role, which determines how requests
// waiting in the local engine queue are charged (see waitingQueueDemand).
// An empty role is treated as domain.RoleBoth.
// Returns nil if the replica has no V2 capacity data (TotalKvCapacityTokens == 0).
func (a *SaturationAnalyzer) computeReplicaCapacity(
	rm domain.ReplicaMetrics,
	config *config.ScalingPolicy,
	modelID, namespace string,
	gpuCount int,
	role string,
	accelerator string,
	logger logr.Logger,
) *ReplicaCapacity {
	if rm.TotalKvCapacityTokens <= 0 {
		// TODO: implement proper demand estimation when vllm:cache_config_info is absent.
		// Currently we fall back to percentage-based demand using the deployment-derived
		// capacity from the capacity store. A better approach would be to estimate
		// TotalKvCapacityTokens from deployment args (num_gpu_blocks_override, block_size)
		// or use a dedicated percentage-based demand signal.
		return a.computeReplicaCapacityFallback(rm, config, modelID, namespace, role, accelerator, logger)
	}

	// Compute demand: tokens already resident in KV cache plus the role-aware
	// footprint of requests still waiting in the local engine queue.
	localQueueDemand := waitingQueueDemand(rm, role)
	replicaDemand := rm.TokensInUse + localQueueDemand

	// k1: memory-bound capacity
	k1 := int64(float64(rm.TotalKvCapacityTokens) * config.KvCacheThreshold)

	// k2: compute-bound capacity
	var engineParams *EngineParams
	if rec := a.capacityStore.Get(namespace, modelID, rm.VariantName); rec != nil {
		engineParams = rec.EngineParams
	}
	k2, k2Priority := a.computeK2(
		modelID, namespace, rm.VariantName, accelerator,
		gpuCount,
		rm.QueueLength, rm.TokensInUse,
		rm.AvgOutputTokens, rm.AvgInputTokens,
		config.QueueLengthThreshold,
		engineParams,
		k1,
		rm.TotalKvCapacityTokens,
		role,
		logger,
	)

	effectiveCapacity := k1
	bound := "k1-memory"
	if k2 < k1 {
		effectiveCapacity = k2
		bound = "k2-compute"
	}
	// Per-replica, per-cycle diagnostics: two lines per replica per optimize
	// cycle, the highest-volume logging in the controller. V(logging.DEFAULT) is
	// the shipped verbosity (cmd/main.go defaults -v to logging.DEFAULT), so
	// hack/benchmark/dump_k2_decisions.py still sees them out of the box, while an
	// operator who does not want them can drop to -v=1 without losing the V(1)
	// diagnostics elsewhere. replica-capacity-skipped is deliberately not among
	// them: it reports a degradation rather than a decision.
	logger.V(logging.DEFAULT).Info("replica-capacity-decision",
		"modelID", modelID, "namespace", namespace, "variant", rm.VariantName, "pod", rm.PodName,
		"k1MemoryBound", k1, "k2ComputeBound", k2, "k2Source", k2Labels[k2Priority],
		"effectiveCapacity", effectiveCapacity, "boundBy", bound,
		"tokensInUse", rm.TokensInUse, "localQueueDemand", localQueueDemand, "replicaDemand", replicaDemand,
		"queueLength", rm.QueueLength, "queueThreshold", config.QueueLengthThreshold)

	// Update capacity store with live data, preserving EngineParams from any
	// existing record (parsed from deployment args and needed for FindCompatible).
	var existingParams *EngineParams
	if existing := a.capacityStore.Get(namespace, modelID, rm.VariantName); existing != nil && existing.EngineParams != nil {
		existingParams = existing.EngineParams
	}
	a.capacityStore.Update(namespace, modelID, rm.VariantName, CapacityRecord{
		AcceleratorName:       accelerator,
		GpuCount:              gpuCount,
		NumGpuBlocks:          rm.NumGpuBlocks,
		BlockSize:             rm.BlockSize,
		TotalKvCapacityTokens: rm.TotalKvCapacityTokens,
		EffectiveCapacity:     effectiveCapacity,
		EngineParams:          existingParams,
		LearnedFrom:           learnedFromLive,
	})

	return &ReplicaCapacity{
		PodName:               rm.PodName,
		VariantName:           rm.VariantName,
		AcceleratorName:       accelerator,
		TokensInUse:           rm.TokensInUse,
		TotalKvCapacityTokens: rm.TotalKvCapacityTokens,
		MemoryBoundCapacity:   k1,
		ComputeBoundCapacity:  k2,
		K2Priority:            k2Priority,
		EffectiveCapacity:     effectiveCapacity,
		ReplicaDemand:         replicaDemand,
		FromWarmPool:          rm.FromWarmPool,
	}
}

// computeReplicaCapacityFallback handles the case where vllm:cache_config_info
// is not available (TotalKvCapacityTokens == 0). It uses the deployment-derived
// capacity from the capacity store and estimates demand from KvCacheUsage percentage.
// This allows V2 to work with model servers that don't emit cache_config_info
// (e.g., the llm-d-inference-sim).
func (a *SaturationAnalyzer) computeReplicaCapacityFallback(
	rm domain.ReplicaMetrics,
	cfg *config.ScalingPolicy,
	modelID, namespace string,
	role string,
	accelerator string,
	logger logr.Logger,
) *ReplicaCapacity {
	rec := a.capacityStore.Get(namespace, modelID, rm.VariantName)
	if rec == nil || rec.EffectiveCapacity <= 0 {
		// Not a routine decision: this replica contributes no capacity at all,
		// so supply is under-counted and the controller over-scales. Unlike the
		// other per-replica lines it stays at Info, where -v cannot hide it.
		logger.Info("replica-capacity-skipped",
			"modelID", modelID, "namespace", namespace, "variant", rm.VariantName, "pod", rm.PodName,
			"reason", "no vllm:cache_config_info and no usable capacity-store record")
		return nil
	}

	// Apply KvCacheThreshold to match the main path (where k1 = totalTokens * threshold).
	// For deployment-derived records, EffectiveCapacity is the raw estimate; the threshold
	// reduces it to the usable portion, consistent with the normal code path.
	//
	// Floor at one token rather than letting the product truncate to zero: rec.EffectiveCapacity
	// is already known positive, and a zero here does not mean "no capacity", it means
	// "less than one token of capacity". Reporting zero makes the variant unsizable —
	// the engine sees a shortfall it cannot divide by a per-replica capacity, so it asks
	// for capacity and can never act on the answer.
	effectiveCapacity := int64(float64(rec.EffectiveCapacity) * cfg.KvCacheThreshold)
	if effectiveCapacity <= 0 {
		effectiveCapacity = 1
	}

	// Estimate demand from KV cache usage percentage applied to the record's RAW
	// capacity, not the thresholded one. Charging usage against the thresholded
	// capacity would put KvCacheThreshold on both sides of demand/supply, where it
	// cancels: utilization would collapse to KvCacheUsage and the threshold would
	// have no effect on the scaling decision at all. Against the raw capacity the
	// ratio is KvCacheUsage/KvCacheThreshold, which is what the main path computes
	// (TokensInUse / (TotalKvCapacityTokens × threshold)) — utilization reaches 1.0
	// exactly when observed KV occupancy reaches the configured ceiling.
	//
	// This is a coarse approximation — KvCacheUsage reflects memory pressure, not
	// exact token demand — but it's sufficient when token-level metrics are absent.
	kvUsageDemand := int64(rm.KvCacheUsage * float64(rec.EffectiveCapacity))

	// Add the role-aware footprint of requests waiting in the local engine queue,
	// matching the main path.
	//
	// Caveat, pre-existing and not introduced here: for a deployment-derived
	// record, EffectiveCapacity is EffectiveMaxBatchedTokens — a *per-step* token
	// budget the store itself calls "a safe lower bound" — while this addend is in
	// absolute KV tokens. The two are not the same unit, so on that record a deep
	// queue can push replicaDemand past effectiveCapacity and report saturation
	// that the replica's actual KV occupancy does not support. Raising the
	// per-request charge lowers the queue depth at which that happens. Tracked
	// separately; fixing it means pairing the fallback's demand and capacity units,
	// not adjusting this line.
	localQueueDemand := waitingQueueDemand(rm, role)
	replicaDemand := kvUsageDemand + localQueueDemand

	logger.V(logging.DEFAULT).Info("replica-capacity-store-fallback",
		"modelID", modelID, "namespace", namespace, "variant", rm.VariantName, "pod", rm.PodName,
		"reason", "no vllm:cache_config_info; using capacity-store record",
		"storeEffectiveCapacity", rec.EffectiveCapacity, "storeLearnedFrom", rec.LearnedFrom,
		"kvCacheUsagePct", rm.KvCacheUsage, "effectiveCapacity", effectiveCapacity,
		"kvUsageDemand", kvUsageDemand, "localQueueDemand", localQueueDemand, "replicaDemand", replicaDemand)

	return &ReplicaCapacity{
		PodName:               rm.PodName,
		VariantName:           rm.VariantName,
		AcceleratorName:       accelerator,
		TokensInUse:           replicaDemand,
		TotalKvCapacityTokens: effectiveCapacity, // synthetic: store-derived
		MemoryBoundCapacity:   effectiveCapacity,
		ComputeBoundCapacity:  effectiveCapacity,
		K2Priority:            k2SrcFallback,
		EffectiveCapacity:     effectiveCapacity,
		ReplicaDemand:         replicaDemand,
		FromWarmPool:          rm.FromWarmPool,
	}
}

// computeK2 determines the compute-bound capacity using a priority chain:
// 1. Observed (queue saturated) → use tokensInUse as k2
// 2. Historical → rolling average from previous observations
// 3. Derived (from deployment args) → formula-based estimate
// 4. Fallback → k1 (memory-bound only)
// Returns the k2 value and the priority level (1–4) that produced it.
func (a *SaturationAnalyzer) computeK2(
	modelID, namespace, variantName, accelerator string,
	gpuCount int,
	queueLen int, tokensInUse int64,
	avgOutput, avgInput float64,
	queueThreshold float64,
	engineParams *EngineParams,
	k1 int64,
	kvCeiling int64,
	role string,
	logger logr.Logger,
) (int64, k2Source) {
	outputBucket := classifyOutputLength(avgOutput)
	// Scoped by role, not just model/accelerator/bucket: prefill's own
	// avgOutputTokens is always ~0-1 (it hands off to decode before
	// generating anything), so it always lands in the "short" bucket -- the
	// same bucket a cold/fresh decode replica lands in before it's served
	// real traffic. Without the role in the key, one role's P1-obs seeds
	// history the other role then reads back via P2-hist, silently reusing
	// an unrelated role's occupancy reading as its own capacity estimate.
	//
	// The queue threshold is in the key because it DEFINES what a P1 observation
	// means: k2 is recorded as the occupancy seen when the queue was considered
	// saturated, so a reading taken at threshold 2 is not a capacity estimate at
	// threshold 100. Nothing else invalidates history -- EvictStaleHistory is
	// age-based and knows nothing about policy -- so without this an operator
	// retuning queueLengthThreshold keeps being sized by observations recorded
	// under the old one. Measured: a k2 of 2 learned under a low threshold kept
	// a variant at utilization 1.0 under a threshold of 100, where P1 could not
	// fire at all.
	historyKey := fmt.Sprintf("%s|%s|%d|%s|%s|q%g",
		modelID, a.stableAccelerator(namespace, variantName, accelerator),
		gpuCount, canonicalRole(role), outputBucket, queueThreshold)

	// Priority 1: Observed (queue saturated)
	//
	// A reading above the KV cache's PHYSICAL ceiling is a scrape artifact
	// (e.g. a mid-cycle admission/eviction race between the metrics snapshot
	// and the cache accounting), not real demand: accepting it would seed the
	// rolling average with a value every subsequent P2-hist cycle inherits,
	// long after the artifact itself is gone.
	//
	// The bound is kvCeiling, NOT k1. k1 is TotalKvCapacityTokens x
	// KvCacheThreshold (0.80 by default), so occupancy between k1 and the
	// ceiling is entirely legitimate -- and with the queue saturated it is the
	// most informative reading there is. Discarding that band would throw away
	// exactly the observations P1-obs exists to capture, and leave the analyzer
	// reporting a capacity ABOVE what the replica is demonstrably holding.
	// Such a reading cannot inflate this cycle either: effectiveCapacity is
	// min(k1, k2), so k1 still bounds it.
	//
	// Falls through to Priority 2 rather than returning k1 directly, so a real
	// historical/derived signal still wins over an untimely fallback-to-k1.
	//
	// The value returned is the rolling average AFTER folding this observation
	// in, not the raw sample: tokensInUse under a saturated queue reflects
	// whatever happens to be admitted this instant, which churns with
	// admission/eviction independently of the backlog it's meant to size for
	// (a replica can shed most of its resident batch while its wait-list stays
	// pinned). Returning it raw lets one noisy cycle single-handedly swing
	// capacity -- and everything downstream that divides by it -- long before
	// the next cycle can correct it. Blending it into the same window P2-hist
	// already reads gives a lone outlier 1/RollingAverageWindowSize weight
	// instead of 100%, while a real, sustained shift still dominates the
	// average once the window has turned over.
	//
	// The window is per VARIANT, not per replica -- historyKey carries no pod
	// identity -- and computeK2 runs once per replica per cycle. So a variant
	// with R replicas writes R samples per cycle and the window spans
	// RollingAverageWindowSize/R cycles: a sustained shift converges in ~2-3
	// cycles at R=4 and takes the full window at R=1. Damping is therefore
	// deepest at one replica, which is where scale-up latency matters most.
	// That is a property to keep in mind when tuning the window, not a bug:
	// outlier weight is 1/N regardless of R, and it is only the time constant
	// that moves.
	//
	// A window that has gone stale is reset rather than blended into. Without
	// that, a variant returning after a quiet period would have its first
	// genuine observation diluted to 1/N against samples describing behaviour
	// from before the gap -- an exposure this smoothing creates and the raw
	// return did not have.
	if queueLen >= int(queueThreshold) && tokensInUse > 0 {
		k2Observed := tokensInUse
		if kvCeiling > 0 && k2Observed > kvCeiling {
			logger.V(logging.DEFAULT).Info("k2-decision",
				"modelID", modelID, "namespace", namespace, "variant", variantName,
				"priority", k2ReasonObsImplausible, "historyKey", historyKey,
				"queueLength", queueLen, "queueThreshold", queueThreshold,
				"reason", "observed tokensInUse exceeds the KV cache's physical ceiling; discarding as implausible",
				"k2Observed", k2Observed, "k1", k1, "kvCeiling", kvCeiling)
		} else {
			a.mu.Lock()
			ra, ok := a.computeCapacityHistory[historyKey]
			if !ok || ra.Stale(HistoryEvictionTimeout) {
				ra = newRollingAverage(RollingAverageWindowSize)
				a.computeCapacityHistory[historyKey] = ra
			}
			ra.Add(float64(k2Observed))
			k2Smoothed := int64(ra.Average())
			historyLen := ra.Len()
			a.mu.Unlock()
			logger.V(logging.DEFAULT).Info("k2-decision",
				"modelID", modelID, "namespace", namespace, "variant", variantName,
				"priority", k2Labels[k2SrcObserved], "historyKey", historyKey,
				"queueLength", queueLen, "queueThreshold", queueThreshold,
				"k2Observed", k2Observed, "k2", k2Smoothed, "historyWindowLen", historyLen)
			return k2Smoothed, k2SrcObserved
		}
	}

	// Priority 2: Historical — lock must cover Average() since Add() mutates
	// the same slice from Priority 1 under the same lock.
	a.mu.Lock()
	var histAvg float64
	var histLen int
	if ra, ok := a.computeCapacityHistory[historyKey]; ok {
		histAvg = ra.Average()
		histLen = ra.Len()
	}
	a.mu.Unlock()
	if histAvg > 0 {
		logger.V(logging.DEFAULT).Info("k2-decision",
			"modelID", modelID, "namespace", namespace, "variant", variantName,
			"priority", k2Labels[k2SrcHistorical], "historyKey", historyKey,
			"queueLength", queueLen, "queueThreshold", queueThreshold,
			"k2", int64(histAvg), "historyWindowLen", histLen)
		return int64(histAvg), k2SrcHistorical
	}

	// Priority 3: Derived from deployment args
	//
	// The formula assumes avgOutput is a genuine per-request output-token
	// count feeding an iterative decode-batching steady-state estimate.
	// Prefill's own vLLM instance reports avgOutput~0-1 (it hands off to
	// decode before generating anything), which collapses the formula's O
	// terms and returns ~EffectiveMaxBatchedTokens regardless of prefill's
	// real per-replica behavior -- not a derived signal at all, just the
	// batch-token budget echoed back. Skip straight to the k1 fallback for
	// prefill rather than report a number that looks derived but isn't.
	//
	// The batch-token budget alone (a prior revision reported it directly as
	// P3-prefill) isn't a substitute: every other capacity figure in this
	// chain is a product -- k1 is capacity x threshold, decode's own P3 is
	// concurrency x per-request footprint -- scaled to the same order of
	// magnitude as demand. The raw per-step budget has no such scaling and
	// disagreed with this replica's own directly observed behavior: at
	// EffectiveMaxBatchedTokens=65536, vllm:num_requests_waiting read 0 across
	// every sample of a full benchmark run, yet P3-prefill's ~65536 read as
	// 5-10x under water against demand snapshots in the hundreds of thousands
	// -- capacity that looked starved while the replica was never once
	// observed to queue. k1 over-states capacity by the same 1-2 orders of
	// magnitude in the other direction, but at least it bounds something real
	// (the KV cache this replica actually has) rather than a per-step figure
	// compared against an accumulated one.
	isPrefill := canonicalRole(role) == domain.RolePrefill
	if !isPrefill {
		if k2Derived := estimateCapacityFromParams(engineParams, avgInput, avgOutput); k2Derived > 0 {
			logger.V(logging.DEFAULT).Info("k2-decision",
				"modelID", modelID, "namespace", namespace, "variant", variantName,
				"priority", k2Labels[k2SrcDerived], "historyKey", historyKey,
				"avgInputTokens", avgInput, "avgOutputTokens", avgOutput,
				"engineParams", engineParams, "k2", k2Derived)
			return k2Derived, k2SrcDerived
		}
	}

	// Priority 4: Fallback to k1
	reason := "no observed/historical/derived k2; capacity is memory-bound only"
	if isPrefill {
		reason = "prefill role: derived-from-args formula assumes decode-style output length; skipped"
	}
	logger.V(logging.DEFAULT).Info("k2-decision",
		"modelID", modelID, "namespace", namespace, "variant", variantName,
		"priority", k2Labels[k2SrcFallback], "historyKey", historyKey,
		"reason", reason,
		"k1", k1)
	return k1, k2SrcFallback
}

// aggregateByVariant groups replica capacities by variant and computes
// per-variant capacity metrics.
func (a *SaturationAnalyzer) aggregateByVariant(
	replicaCapacities []ReplicaCapacity,
	inputMetrics []domain.ReplicaMetrics,
	variantStates []domain.VariantReplicaState,
	modelID, namespace string,
	kvCacheThreshold float64,
	logger logr.Logger,
) []domain.VariantCapacity {
	// Group replicas by variant
	byVariant := make(map[string][]ReplicaCapacity)
	for _, rc := range replicaCapacities {
		byVariant[rc.VariantName] = append(byVariant[rc.VariantName], rc)
	}

	// Compute model-level workload averages from live replica metrics.
	// Used for capacity estimation of zero-replica variants with deployment-derived params.
	modelAvgInput, modelAvgOutput, _ := computeModelWorkloadAverages(inputMetrics)

	result := make([]domain.VariantCapacity, 0, len(variantStates))
	for _, vs := range variantStates {
		replicas := byVariant[vs.VariantName]

		var perReplicaCapacity float64
		var totalDemand float64
		// Bridges serving this variant, counted apart from its own replicas.
		var warmPoolReplicas int
		// P for a bridge, measured from the bridges themselves. Kept apart from
		// perReplicaCapacity because the pool runs its engines at a lower
		// --gpu-memory-utilization than the workload, so the two are not
		// interchangeable readings. See the split in the loop below.
		var bridgePerReplica float64
		// accelerator is an analyzer input (discovery-resolved, on VariantReplicaState),
		// used below for cross-variant capacity lookup. It is NOT emitted on the output;
		// the capacity builder fills per-variant identity from discovery.
		accelerator := vs.AcceleratorName

		readyCount := vs.CurrentReplicas - vs.PendingReplicas
		if readyCount < 0 {
			readyCount = 0
		}

		// replicaCount sets VariantCapacity.ReplicaCount, which aggregation.go uses
		// to recompute supply totals, and divides totalDemand for the per-variant
		// utilization. It is in scale-target units (pods, or LWS groups) on every
		// branch: readyCount is scale-target status, and len(replicas) counts the
		// collector's per-pod rows, which merge a pod's engine instances into one
		// replica (see collector.collapseToPods). pendingCount shares that unit,
		// as SumTotalAnticipatedSupply adds the two.
		replicaCount := readyCount
		pendingCount := vs.PendingReplicas

		var capacityLabel string
		if len(replicas) > 0 {
			// BRIDGES count toward demand and not toward supply.
			//
			// A bridge is a warm pool Pod lent to this variant while it is short.
			// The traffic it is serving is this variant's traffic, so its demand
			// belongs in the total like any replica's -- leave it out and demand
			// reads lowest exactly while a bridge is covering the shortfall, then
			// appears from nowhere when the Pod goes back.
			//
			// Its capacity is a different matter. The Pod is borrowed and returns
			// when the ordinary replicas arrive, so counting it as supply would
			// tell the optimizer the fleet is already big enough and suppress the
			// scale-up the bridge exists to bridge. The pool would then hold the
			// Pod indefinitely: the replicas that would release it are the ones
			// it talked the optimizer out of creating.
			//
			// Its capacity IS measured, and carried out separately for the
			// retained-pool switching decision, where the pool is the capacity
			// and there are no ordinary replicas coming.
			// P IS MEASURED OVER OWN REPLICAS, AND A BRIDGE IS PRICED APART.
			//
			// Both are measurements, so both are the analyzer's to emit; what is
			// DERIVED from them (supply, and the bridges' worth) belongs to the
			// capacity-build step and is not computed here.
			//
			// The two readings differ for a structural reason: the pool runs its
			// engines at a lower --gpu-memory-utilization than the workload does,
			// because it fits several sleepers in a Pod where a workload replica
			// has the GPU to itself. Less of the GPU is KV cache, so a bridge's
			// memory-bound capacity is genuinely smaller. See
			// domain.VariantCapacity.PerReplicaCapacity.
			ownCapacities := make([]int64, 0, len(replicas))
			bridgeCapacities := make([]int64, 0, len(replicas))
			ownRows := make([]ReplicaCapacity, 0, len(replicas))
			ownReplicas := 0
			for _, rc := range replicas {
				// Demand is summed over EVERY row, bridges included: the traffic a
				// bridge is serving is this variant's traffic. Only capacity splits.
				totalDemand += float64(rc.ReplicaDemand)
				if rc.FromWarmPool {
					warmPoolReplicas++
					bridgeCapacities = append(bridgeCapacities, rc.EffectiveCapacity)
					continue
				}
				ownReplicas++
				ownCapacities = append(ownCapacities, rc.EffectiveCapacity)
				ownRows = append(ownRows, rc)
			}
			bridgePerReplica = float64(median(bridgeCapacities))
			if len(ownCapacities) > 0 {
				perReplicaCapacity = float64(median(ownCapacities))
				capacityLabel = k2SourceLabel(ownRows)
			} else {
				// Only bridges reported. The variant's own per-replica capacity is
				// unmeasured this cycle, and the bridge's reading is the closest
				// thing to it that was actually observed -- lower than the truth,
				// which asks for one replica too many rather than one too few.
				// Zero would be worse than either: the optimizer skips a variant
				// whose per-replica capacity is <= 0 (cost_aware_optimizer.go), so
				// a variant a bridge is currently carrying would stop being scaled
				// at all -- exactly while it is short.
				perReplicaCapacity = bridgePerReplica
				capacityLabel = k2SourceLabel(replicas)
			}
			// Prefer the live count over readyCount: it is what actually reported
			// capacity this cycle, where readyCount is (lagging) scale-target status.
			replicaCount = ownReplicas
		} else if rec := a.capacityStore.Get(namespace, modelID, vs.VariantName); rec != nil && rec.EffectiveCapacity > 0 {
			// No ready replicas — use stored capacity, enhanced with k2 derivation
			// for deployment-derived records when workload data is available.
			perReplicaCapacity = a.estimateStoredCapacity(rec, modelID, namespace, vs.VariantName, accelerator, vs.GPUsPerReplica,
				kvCacheThreshold, modelAvgInput, modelAvgOutput, logger)
			capacityLabel = satReasonP0Store
		} else if rec := a.lookupCompatibleCapacity(namespace, modelID, vs.VariantName, accelerator, vs.GPUsPerReplica); rec != nil {
			// No own record — try cross-variant estimation from a compatible variant
			perReplicaCapacity = float64(rec.EffectiveCapacity)
			capacityLabel = satReasonP0Store
			logger.Info("variant-capacity-source",
				"modelID", modelID, "namespace", namespace, "variant", vs.VariantName,
				"reason", "no own capacity record; borrowed from a compatible variant",
				"perReplicaCapacity", perReplicaCapacity, "engineParamsSource", rec.LearnedFrom)
		} else {
			capacityLabel = satReasonNoData
			logger.Info("variant-capacity-source",
				"modelID", modelID, "namespace", namespace, "variant", vs.VariantName,
				"reason", "no live replicas, no own capacity-store record, no compatible variant found")
		}

		totalCapacity := float64(replicaCount) * perReplicaCapacity

		var utilization float64
		if totalCapacity > 0 {
			utilization = totalDemand / totalCapacity
		}

		// Per-variant identity (Cost, AcceleratorName) is intentionally not set here:
		// the capacity builder fills it from the discovery step, so the analyzer's
		// output is the measured capacity signal, not laundered identity.
		result = append(result, domain.VariantCapacity{
			VariantName:      vs.VariantName,
			Role:             vs.Role,
			ReplicaCount:     replicaCount,
			PendingReplicas:  pendingCount,
			WarmPoolReplicas: warmPoolReplicas,
			// Both readings are MEASURED, so both are the analyzer's to emit, and
			// they are kept apart because they are different numbers. What they
			// are worth in total -- WarmPoolCapacity -- is derived, so the
			// capacity-build step owns it and nothing is set for it here.
			WarmPoolPerReplicaCapacity: bridgePerReplica,
			PerReplicaCapacity:         perReplicaCapacity,
			TotalDemand:                totalDemand,
			Utilization:                utilization,
			Reason:                     capacityLabel,
		})
		if warmPoolReplicas > 0 {
			logger.Info("warm-pool-bridge-supply",
				"modelID", modelID, "namespace", namespace, "variant", vs.VariantName,
				"bridges", warmPoolReplicas, "ownReplicas", replicaCount,
				// Both prices, because the whole point of measuring them apart is
				// that they differ -- a reader given only one cannot tell whether
				// the split is working, or whether the pool is running the engine
				// on the same terms as the workload.
				"bridgePerReplica", bridgePerReplica,
				"ownPerReplicaCapacity", perReplicaCapacity,
				"totalDemand", totalDemand,
				"note", "bridge demand is counted, bridge capacity is not supply")
		}
	}

	return result
}

// aggregateRoleDemand groups the variant capacities' demand by role and returns
// the analyzer's per-role demand attribution — the demand half of the (D, P)
// contract. Returns nil when no disaggregation is active (all variants are role
// "both" or empty). The queueDemandByRole map adds scheduler queue demand
// attributed to each role (nil when there's no queue demand); a role that no
// variant serves is ignored, so queue demand is never charged to a role with no
// supply behind it.
//
// Per-role supply is deliberately not computed here: the engine's capacity-build
// step recomputes it from the same VariantCapacities and pairs it with this map.
func (a *SaturationAnalyzer) aggregateRoleDemand(
	variantCapacities []domain.VariantCapacity,
	queueDemandByRole map[string]float64,
) map[string]float64 {
	if !aggregation.IsDisaggregated(variantCapacities) {
		return nil
	}

	demand := aggregation.DemandByRole(variantCapacities)

	// Add scheduler queue demand attributed to each role.
	for role, qd := range queueDemandByRole {
		if _, ok := demand[role]; ok {
			demand[role] += qd
		}
	}
	return demand
}

// lookupCompatibleCapacity searches the capacity store for a record from
// another variant with matching hardware and engine parameters. This enables
// capacity estimation for zero-replica variants that have no prior data.
// The search is cross-namespace since capacity depends on hardware + config,
// not namespace.
func (a *SaturationAnalyzer) lookupCompatibleCapacity(namespace, modelID, variantName, accelerator string, gpuCount int) *CapacityRecord {
	// Get EngineParams for this variant (from deployment-derived record)
	rec := a.capacityStore.Get(namespace, modelID, variantName)
	if rec == nil || rec.EngineParams == nil {
		return nil
	}
	return a.capacityStore.FindCompatible(modelID, accelerator, gpuCount, rec.EngineParams)
}

// estimateStoredCapacity returns a capacity estimate for a zero-replica variant
// using its stored CapacityRecord. For learnedFromLive records (from a previously running
// pod), the stored EffectiveCapacity is authoritative. For "deployment" records,
// it tries to compute a better estimate using the k2 derivation formula with
// model-level workload averages, bounded by:
//  1. A compatible variant's live EffectiveCapacity (already min(k1,k2))
//  2. Own k1 if TotalKvCapacityTokens is known (from num_gpu_blocks_override)
//
// Falls back to stored EffectiveCapacity (EffectiveMaxBatchedTokens) when no
// workload data is available.
//
// accelerator and gpuCount are the variant's hardware keys and come from the
// discovery metadata, NOT from rec. A stored record's own copies can be empty or
// stale — a deployment-derived record is written before any pod has reported —
// and keying the compatibility search off them silently finds no match, which
// reads as "no comparable variant exists" rather than "we looked with a blank
// key". These are the same keys the record is written under, so read and write
// agree by construction.
func (a *SaturationAnalyzer) estimateStoredCapacity(
	rec *CapacityRecord,
	modelID, namespace, variantName string,
	accelerator string,
	gpuCount int,
	kvCacheThreshold float64,
	modelAvgInput, modelAvgOutput float64,
	logger logr.Logger,
) float64 {
	if rec == nil {
		return 0
	}

	// Live records have observed capacity — use directly
	if rec.LearnedFrom == learnedFromLive {
		logger.Info("zero-replica-capacity-estimate",
			"modelID", modelID, "namespace", namespace, "variant", variantName,
			"source", "stored-live", "reason", "prior live observation reused while replica count is zero",
			"perReplicaCapacity", rec.EffectiveCapacity)
		return float64(rec.EffectiveCapacity)
	}

	// For deployment-derived records, try k2 derivation with workload data
	if rec.EngineParams != nil && modelAvgOutput > 0 {
		if derived := estimateCapacityFromParams(rec.EngineParams, modelAvgInput, modelAvgOutput); derived > 0 {
			bounded := derived
			boundedBy := "derived"

			// Bound by own k1 if TotalKvCapacityTokens is known (num_gpu_blocks_override)
			if rec.TotalKvCapacityTokens > 0 && kvCacheThreshold > 0 {
				k1 := int64(float64(rec.TotalKvCapacityTokens) * kvCacheThreshold)
				if k1 > 0 && k1 < bounded {
					bounded = k1
					boundedBy = "own-k1"
				}
			}

			// Bound by compatible variant's live EffectiveCapacity (already min(k1,k2))
			if compatible := a.capacityStore.FindCompatible(modelID, accelerator, gpuCount, rec.EngineParams); compatible != nil && compatible.LearnedFrom == learnedFromLive && compatible.EffectiveCapacity > 0 {
				if compatible.EffectiveCapacity < bounded {
					bounded = compatible.EffectiveCapacity
					boundedBy = "compatible-variant-live"
				}
			}

			logger.Info("zero-replica-capacity-estimate",
				"modelID", modelID, "namespace", namespace, "variant", variantName,
				"source", "deployment-derived", "boundedBy", boundedBy,
				"derivedCapacity", derived, "perReplicaCapacity", bounded,
				"modelAvgInputTokens", modelAvgInput, "modelAvgOutputTokens", modelAvgOutput)
			return float64(bounded)
		}
	}

	// Fallback: stored EffectiveCapacity (EffectiveMaxBatchedTokens from LoadFromDeployment)
	logger.Info("zero-replica-capacity-estimate",
		"modelID", modelID, "namespace", namespace, "variant", variantName,
		"source", "deployment-stored-fallback",
		"reason", "no workload averages or derivation available; using raw stored EffectiveMaxBatchedTokens",
		"perReplicaCapacity", rec.EffectiveCapacity)
	return float64(rec.EffectiveCapacity)
}

// estimateCapacityFromParams computes a capacity estimate using the k2 derivation
// formula: N_steady = min(B * O / (I + O), S), capacity = N_steady * (I + O/2).
// Used by computeK2 (Priority 3) for per-replica estimation and by
// estimateStoredCapacity for zero-replica variants with model-level workload averages.
// Returns 0 if estimation is not possible.
func estimateCapacityFromParams(params *EngineParams, avgInput, avgOutput float64) int64 {
	if params == nil || params.EffectiveMaxBatchedTokens <= 0 || avgOutput <= 0 {
		return 0
	}

	B := float64(params.EffectiveMaxBatchedTokens)
	S := float64(params.MaxNumSeqs)
	I := avgInput
	O := avgOutput

	nSteady := B * O / (I + O)
	if nSteady > S {
		nSteady = S
	}
	k2Derived := int64(nSteady * (I + O/2))
	if k2Derived > 0 {
		return k2Derived
	}
	return 0
}

// computeModelWorkloadAverages computes the model-level average input tokens,
// output tokens, and prefix cache hit rate from replica metrics across all
// variants. These averages enable capacity estimation for zero-replica variants
// using the k2 derivation formula, and scheduler queue demand estimation.
func computeModelWorkloadAverages(replicaMetrics []domain.ReplicaMetrics) (avgInput, avgOutput, avgHitRate float64) {
	var count int
	for _, rm := range replicaMetrics {
		if rm.AvgInputTokens > 0 || rm.AvgOutputTokens > 0 {
			avgInput += rm.AvgInputTokens
			avgOutput += rm.AvgOutputTokens
			avgHitRate += rm.PrefixCacheHitRate
			count++
		}
	}
	if count > 0 {
		avgInput /= float64(count)
		avgOutput /= float64(count)
		avgHitRate /= float64(count)
	}
	return avgInput, avgOutput, avgHitRate
}

// canonicalRole normalizes an empty variant role to domain.RoleBoth, matching
// aggregation.AggregateByRole.
// stableAccelerator returns the accelerator to key history under, preferring the
// last one that resolved for this variant over an unresolved reading.
//
// Capacity is a property of the hardware, so it belongs in the key; but "the pods
// currently disagree about it" is not a different accelerator, and treating it as
// one is what makes the fleet oscillate. A resolved reading always wins and is
// remembered; an unresolved one falls back to what was remembered, and only when
// nothing was ever resolved does the unresolved value itself get used, which
// keeps a never-resolvable variant behaving exactly as it does today.
func (a *SaturationAnalyzer) stableAccelerator(namespace, variantName, accelerator string) string {
	key := namespace + "/" + variantName
	a.mu.Lock()
	defer a.mu.Unlock()
	if constants.IsAcceleratorResolved(accelerator) {
		a.lastAccelerator[key] = acceleratorMemo{name: accelerator, lastUsed: time.Now()}
		return accelerator
	}
	if last, ok := a.lastAccelerator[key]; ok {
		// Touched on READ as well as on write: the memo should live as long as the
		// variant is being analysed, not as long as its accelerator keeps
		// resolving -- a variant that stops resolving is exactly the one this
		// exists for. A variant that stops being analysed goes quiet on both, and
		// EvictStaleHistory then ages it out with the k2 history beside it.
		last.lastUsed = time.Now()
		a.lastAccelerator[key] = last
		return last.name
	}
	return accelerator
}

func canonicalRole(role string) string {
	if role == "" {
		return domain.RoleBoth
	}
	return role
}

// waitingQueueDemand estimates the KV-token demand of the requests waiting in a
// replica's local engine queue (vllm:num_requests_waiting / sglang:num_queue_reqs).
// Their footprint is projected from the replica's average request shape, since
// the queue metric carries no per-request token counts.
//
// Note these requests are not uniformly blockless: on the decode side a
// transfer-pending request (vLLM's WAITING_FOR_REMOTE_KVS) has already had
// blocks allocated, so part of its prompt KV is also inside KvCacheUsage and is
// counted twice. The overlap shrinks toward zero under KV pressure, when block
// allocation starts failing — i.e. in the saturated regime where the scaling
// decision is actually made.
//
// The per-request cost is role-aware, because a replica only pays for the KV it
// actually materializes:
//
//	Prefill:      AvgInputTokens                   (prompt KV only; output is generated elsewhere)
//	Decode/Both:  AvgInputTokens + AvgOutputTokens (holds prompt KV and grows it per generated token)
//
// Charging decode replicas for input alone understates demand for
// long-generation workloads, which are precisely the ones whose KV pressure is
// output-driven. That is the under-reporting this addresses. It mirrors the role
// attribution estimateSchedulerQueueDemand already applies model-wide.
//
// # Why the full output length, and how it relates to the capacity side
//
// I+O is a request's KV footprint at its LAST decode step, not its mean. It is
// deliberately a peak, no-preemption planning charge: once admitted, a decode
// request's KV grows monotonically and the engine cannot shed it without
// preemption and recompute, so a replica expected to host the request needs room
// for its final size.
//
// I+O is this analyzer's demand-side unit. It is deliberately not the unit used
// by the time-averaged models elsewhere, and the distinction matters when reading
// the two together:
//
//   - Saturation V2 (here, demand): I+O — peak footprint. Sizes for what a
//     replica must be able to hold, not what it holds on average.
//   - Throughput analyzer (throughput.WorkloadShape.KVreq): ILeff + O/2 —
//     time-averaged. A SEPARATE analyzer with its own model and its own
//     supply/demand pairing; it does not constrain this term and is not
//     inconsistent with it.
//
// One asymmetry is genuinely internal to this analyzer and should not be confused
// with the above: estimateCapacityFromParams prices a *concurrent* request at
// I + O/2 when deriving k2, so a queued request is charged more than it will
// occupy on average, and its charge drops once it is admitted and starts being
// measured through KvCacheUsage instead. Both effects bias this term toward
// scale-up.
//
// Note also that this term and the resident term are both derived from 1-minute
// maxima (max_over_time on kv_cache_usage_perc and num_requests_waiting), whose
// peaks need not coincide, so their sum can exceed any demand the replica
// actually saw at a single instant. That is a pre-existing property of the
// collector's queries, not of this function, but it compounds the bias above.
//
// That trade is chosen on purpose: under-provisioning decode capacity causes
// preemption and recompute thrash, which costs more than a spare replica.
// Revisiting it means changing the demand/capacity pair together, not this
// function alone.
//
// Returns 0 when the queue is empty, or when token metrics are absent or
// non-finite.
func waitingQueueDemand(rm domain.ReplicaMetrics, role string) int64 {
	if rm.QueueLength <= 0 {
		return 0
	}

	tokensPerRequest := rm.AvgInputTokens
	if canonicalRole(role) != domain.RolePrefill {
		tokensPerRequest += rm.AvgOutputTokens
	}
	// Compute in float64 and validate before converting, so one check covers
	// NaN, infinities, and finite values too large for an int64. Converting an
	// out-of-range float64 to int64 is implementation-defined in Go (amd64
	// yields math.MinInt64, arm64 saturates), and either way the result is
	// garbage that reads downstream as "idle, remove replicas" or as enormous
	// demand. Note `x <= 0` would NOT catch NaN, since every NaN comparison is
	// false. The collector filters NaN/Inf on the token averages, so this is a
	// guard rather than a live bug; it does not filter QueueLength, which is a
	// bare int conversion, but a garbage value there is caught either by the
	// QueueLength <= 0 check above or by the range bound below.
	//
	// Multiplying before truncating also drops the per-request rounding the
	// previous truncate-then-multiply form accumulated: at queue 3 and an average
	// of 100.9 tokens this yields 302 rather than 300.
	// The bound is >=, not >: float64(math.MaxInt64) rounds up to 2^63, which is
	// one past the largest representable int64, so a demand of exactly 2^63 would
	// pass a > check and then overflow to MinInt64.
	demand := float64(rm.QueueLength) * tokensPerRequest
	if !(demand > 0) || demand >= float64(math.MaxInt64) {
		return 0
	}

	return int64(demand)
}

// schedulerQueueDemand holds the estimated token demand from scheduler-queued
// requests, broken down by P/D role for disaggregated models.
type schedulerQueueDemand struct {
	total  float64            // model-level total (inputTokens + outputTokens)
	byRole map[string]float64 // per-role demand: "prefill", "decode", "both"
}

// estimateSchedulerQueueDemand estimates the token demand from requests queued
// in the llm-d inference scheduler's flow control layer, with per-role
// attribution for P/D disaggregated models.
//
// These requests have not yet reached any engine pod, so we estimate their
// token footprint using two independent signals:
//
//	inputTokens = max(queueBytes / BytesPerToken, queueSize * avgInputTokens)
//	             * (1 - prefixCacheHitRate)
//	outputTokens = queueSize * avgOutputTokens
//
// Role attribution:
//   - Prefill: inputTokens (prompt KV must be computed and stored)
//   - Decode:  inputTokens + outputTokens (receives KV transfer + generates output)
//   - Both:    inputTokens + outputTokens (handles full request lifecycle)
//   - Model-level total: inputTokens + outputTokens (unchanged for backward compat)
//
// The prefix cache hit rate reduces expected input token KV demand because
// a fraction of prompt tokens will hit the prefix cache and reuse existing
// KV blocks. This does NOT apply to the local engine queue
// (vllm:num_requests_waiting / sglang:num_queue_reqs) because those requests
// have not yet had prefix cache lookup performed.
func estimateSchedulerQueueDemand(
	sq *domain.SchedulerQueueMetrics,
	replicaMetrics []domain.ReplicaMetrics,
	activeRoles map[string]bool,
) schedulerQueueDemand {
	if sq == nil || (sq.QueueSize == 0 && sq.QueueBytes == 0) {
		return schedulerQueueDemand{}
	}

	// Compute model-level averages from replica metrics
	avgInput, avgOutput, avgHitRate := computeModelWorkloadAverages(replicaMetrics)

	// Estimate input tokens from two signals, take the max for robustness
	tokensFromBytes := float64(sq.QueueBytes) / BytesPerToken
	tokensFromCount := float64(sq.QueueSize) * avgInput
	inputTokens := tokensFromBytes
	if tokensFromCount > inputTokens {
		inputTokens = tokensFromCount
	}

	// Apply prefix cache hit rate reduction to input tokens only
	inputTokens *= (1 - avgHitRate)

	// Estimate output tokens (no cache reduction — output must be generated)
	outputTokens := float64(sq.QueueSize) * avgOutput

	total := inputTokens + outputTokens

	// Build per-role attribution
	byRole := make(map[string]float64)
	if len(activeRoles) > 0 {
		for role := range activeRoles {
			switch role {
			case domain.RolePrefill:
				byRole[domain.RolePrefill] = inputTokens
			case domain.RoleDecode:
				byRole[domain.RoleDecode] = inputTokens + outputTokens
			default: // domain.RoleBoth or unknown
				byRole[role] = total
			}
		}
	}

	return schedulerQueueDemand{total: total, byRole: byRole}
}

// k2SourceLabel returns the K2Priority label for the lower-median replica by
// EffectiveCapacity. Sorts a copy and picks index (n-1)/2, which always
// resolves to an actual replica — no average is taken, so even-length slices
// never produce a value that matches no element.
// Returns "" when replicas is empty.
func k2SourceLabel(replicas []ReplicaCapacity) string {
	if len(replicas) == 0 {
		return ""
	}
	sorted := make([]ReplicaCapacity, len(replicas))
	copy(sorted, replicas)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].EffectiveCapacity < sorted[j].EffectiveCapacity
	})
	medIdx := (len(sorted) - 1) / 2
	if label, ok := k2Labels[sorted[medIdx].K2Priority]; ok {
		return label
	}
	return allocation.ReasonError
}

// median returns the median value from a sorted slice of int64 values.
// Returns 0 if the slice is empty.
//
// Averages the central pair on an even count, which is NOT what medianOf in
// arrival_demand.go does -- it takes the lower of the two. The difference is
// deliberate and follows from what each input is: this one blends learned
// per-replica capacities, where every reading is trusted and the midpoint is
// the better estimate, while medianOf exists to survive a reading that should
// not be trusted at all. See medianOf's doc comment for the two-replica case
// that decides it.
func median(values []int64) int64 {
	n := len(values)
	if n == 0 {
		return 0
	}

	sorted := make([]int64, n)
	copy(sorted, values)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	if n%2 == 0 {
		return (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return sorted[n/2]
}
