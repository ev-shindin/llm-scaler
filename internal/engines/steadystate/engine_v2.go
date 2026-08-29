package steadystate

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// runV2AnalysisOnly runs the saturation analyzer and returns the raw AnalyzerResult
// without building targets. The optimizer handles target building across all models.
func (e *Engine) runV2AnalysisOnly(
	ctx context.Context,
	modelID, namespace string,
	replicaMetrics []domain.ReplicaMetrics,
	config config.ScalingPolicy,
	variantStates []domain.VariantReplicaState,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	schedulerQueue *domain.SchedulerQueueMetrics,
	arrivalRate float64,
) (*domain.AnalyzerResult, error) {
	logger := ctrl.LoggerFrom(ctx)

	// 1. Pre-populate capacity store with scale target-derived params
	for _, va := range variantAutoscalings {
		key := utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())
		scaleTarget := scaleTargets[key]
		if scaleTarget == nil {
			logger.V(logging.DEBUG).Info("No scale target found for VA, skipping capacity store pre-population",
				"variant", va.Name, "scaleTargetKey", key)
			continue
		}
		// Get accelerator name from scale target nodeSelector/nodeAffinity or VA label
		accelerator := accelerator.GetAcceleratorNameFromScaleTarget(va, scaleTarget)
		gpuCount := scaleTarget.GetTotalGPUsPerReplica()
		e.capacityStore.LoadFromScaleTarget(namespace, modelID, va.Name, accelerator, gpuCount, scaleTarget)
		logger.V(logging.DEBUG).Info("Pre-populated capacity store from scale target",
			"variant", va.Name, "accelerator", accelerator, "gpuCount", gpuCount)
	}

	// 2. Build AnalyzerInput
	input := domain.AnalyzerInput{
		ModelID:        modelID,
		Namespace:      namespace,
		ReplicaMetrics: replicaMetrics,
		VariantStates:  variantStates,
		Config:         &config,
		SchedulerQueue: schedulerQueue,
		ArrivalRate:    arrivalRate,
	}

	// 3. Run V2 analyzer
	result, err := e.saturationV2Analyzer.Analyze(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("V2 saturation analysis failed: %w", err)
	}

	// Analysis results are logged by the caller (runAnalyzersAndScore) after the
	// applyUniversalThreshold post-step, so the single Info line can include the
	// real RequiredCapacity/SpareCapacity. They are left zero here, so logging
	// them at this point would always report 0 and be misleading.
	return result, nil
}

// analyzerLivenessStaleCycles is the number of optimization cycles an
// analyzer's last informative result may age before it is treated as stale
// (non-live) for the scale-down veto gate. Fixed for now; revisit as a
// per-deployment config field if operators need to tune it.
// TODO: make configurable if needed.
const analyzerLivenessStaleCycles = 3

// runAnalyzersAndScore runs the V2 saturation analyzer, applies the universal
// threshold post-step to every analyzer's result (using per-analyzer config
// overrides where set), and computes the weighted composite score from
// saturation's signal and the model's priority.
//
// The engine applies applyUniversalThreshold to every analyzer (saturation and
// all registered non-saturation analyzers) and collects the calibrated results
// into a per-analyzer slice returned to the optimizer. Saturation is always the
// first entry; it is the keeper of per-variant metadata (Cost, AcceleratorName,
// Role) until a future pre-analysis-extraction PR separates that concern.
func (e *Engine) runAnalyzersAndScore(
	ctx context.Context,
	modelID, namespace string,
	replicaMetrics []domain.ReplicaMetrics,
	config config.ScalingPolicy,
	variantStates []domain.VariantReplicaState,
	variantMetadata []domain.VariantMetadata,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	schedulerQueue *domain.SchedulerQueueMetrics,
	arrivalRate float64,
) ([]allocation.NamedAnalyzerResult, error) {
	logger := ctrl.LoggerFrom(ctx)

	// metaByVariant is the authoritative discovery metadata the capacity builder
	// joins onto every analyzer's output (empty on paths that pass no metadata,
	// e.g. unit tests, in which case the build step is a no-op join).
	metaByVariant := metadataByVariant(variantMetadata)

	// Run saturation analyzer (always needed for PerReplicaCapacity).
	baseResult, err := e.runV2AnalysisOnly(ctx, modelID, namespace, replicaMetrics, config,
		variantStates, scaleTargets, variantAutoscalings, schedulerQueue, arrivalRate)
	if err != nil {
		return nil, err
	}

	satUp, satDown := config.AnalyzerThresholds(domain.SaturationAnalyzerName)

	// Build AnalyzerInput once; shared by all non-saturation analyzers.
	// Note: &config has had saturation's per-entry threshold overrides applied
	// (the loop above). Non-saturation analyzers therefore receive the
	// saturation-adjusted config rather than the original. This is harmless
	// on this branch (their results are discarded), and the clean fix —
	// engine applies thresholds universally after each analyzer runs —
	// is tracked on multi-analyzer-threshold (PR #1228).
	input := domain.AnalyzerInput{
		ModelID:        modelID,
		Namespace:      namespace,
		ReplicaMetrics: replicaMetrics,
		VariantStates:  variantStates,
		Config:         &config,
		SchedulerQueue: schedulerQueue,
		ArrivalRate:    arrivalRate,
	}

	// Collect per-analyzer results. Saturation is first; each non-saturation
	// analyzer is run, calibrated with its resolved thresholds, and appended.
	//
	// The capacity-build step writes the engine-owned aggregates onto the named
	// entry, so each entry is constructed *before* the build and its mutable
	// Remaining/Spare counters are seeded from the built RC/SC afterwards.
	namedResults := []allocation.NamedAnalyzerResult{
		buildNamedResult(ctx, domain.SaturationAnalyzerName, baseResult, config, metaByVariant, satUp, satDown),
	}
	// What each variant is getting from the pool, for the retained-pool switching
	// decision. Published from saturation's result because that is the analyzer
	// that measures per-replica capacity; it is deliberately absent from every
	// supply total above.
	publishWarmPoolSupply(namespace, namedResults[0])
	for _, entry := range e.analyzerRunEntries() {
		if entry.name == domain.SaturationAnalyzerName {
			continue
		}
		if !config.AnalyzerEnabled(entry.name) {
			continue
		}
		result := runRegisteredAnalyzer(ctx, logger, entry, modelID, input)
		if result == nil {
			continue
		}
		up, down := config.AnalyzerThresholds(entry.name)
		namedResults = append(namedResults,
			buildNamedResult(ctx, entry.name, result, config, metaByVariant, up, down))
	}
	e.updateLivenessAndSetLive(ctx, namespace, modelID, namedResults)
	e.recordAnalyzerMetrics(namespace, modelID, namedResults)

	for _, nr := range namedResults {
		logAnalyzerResult(ctx, modelID, namespace, nr)
	}
	return namedResults, nil
}

// analyzerDemandSeries identifies one wva_analyzer_demand series within a model
// instance. Role is "" for a non-disaggregated model.
type analyzerDemandSeries struct {
	analyzer string
	role     string
}

// analyzerTargetSeries identifies one wva_analyzer_target series within a model
// instance.
type analyzerTargetSeries struct {
	analyzer string
	variant  string
}

// analyzerSeries is the set of analyzer metric series emitted for one model
// instance in a cycle, plus the identity needed to evict them wholesale when the
// model departs.
type analyzerSeries struct {
	namespace string
	modelID   string
	demand    map[analyzerDemandSeries]struct{}
	target    map[analyzerTargetSeries]struct{}
}

// recordAnalyzerMetrics publishes the wva_analyzer_demand / wva_analyzer_target
// series for every analyzer that ran this cycle, straight from the (D, P) it
// produced: demand per model instance (per role when disaggregated) and the
// per-replica target per variant. This exposes every analyzer's reasoning — the
// same D/P contract the optimizer and the external-analyzer wrapper use.
//
// Absence is meaningful for these series: a missing one means the analyzer did
// not report, which is different from reporting zero. So after emitting, any
// series published last cycle but not this one is deleted — otherwise a role
// that disappears, a variant that is removed, or an analyzer that stops running
// would leave its last value frozen in the registry forever, and no consumer
// could tell it apart from a live reading.
func (e *Engine) recordAnalyzerMetrics(namespace, modelID string, results []allocation.NamedAnalyzerResult) {
	current := analyzerSeries{
		namespace: namespace,
		modelID:   modelID,
		demand:    make(map[analyzerDemandSeries]struct{}),
		target:    make(map[analyzerTargetSeries]struct{}),
	}

	for _, nr := range results {
		if nr.Result == nil {
			continue
		}
		if len(nr.RoleCapacities) > 0 {
			for role, rc := range nr.RoleCapacities {
				e.metricsEmitter.RecordAnalyzerDemand(nr.Name, namespace, modelID, role, rc.TotalDemand)
				current.demand[analyzerDemandSeries{analyzer: nr.Name, role: role}] = struct{}{}
			}
		} else {
			e.metricsEmitter.RecordAnalyzerDemand(nr.Name, namespace, modelID, "", nr.Result.TotalDemand)
			current.demand[analyzerDemandSeries{analyzer: nr.Name}] = struct{}{}
		}
		for _, vc := range nr.Result.VariantCapacities {
			e.metricsEmitter.RecordAnalyzerTarget(nr.Name, namespace, modelID, vc.VariantName, vc.PerReplicaCapacity)
			current.target[analyzerTargetSeries{analyzer: nr.Name, variant: vc.VariantName}] = struct{}{}
		}
	}

	// Evict after emitting, never before, so a series that survives the cycle is
	// never briefly absent from a concurrent scrape.
	e.evictStaleAnalyzerSeries(namespace, modelID, current)
}

// evictStaleAnalyzerSeries deletes the analyzer series this model published on
// the previous cycle but not on this one, then records the current set.
func (e *Engine) evictStaleAnalyzerSeries(namespace, modelID string, current analyzerSeries) {
	if e.lastAnalyzerSeries == nil {
		e.lastAnalyzerSeries = make(map[string]analyzerSeries)
	}
	modelKey := utils.GetNamespacedKey(namespace, modelID)
	for prev := range e.lastAnalyzerSeries[modelKey].demand {
		if _, still := current.demand[prev]; !still {
			e.metricsEmitter.DeleteAnalyzerDemand(prev.analyzer, namespace, modelID, prev.role)
		}
	}
	for prev := range e.lastAnalyzerSeries[modelKey].target {
		if _, still := current.target[prev]; !still {
			e.metricsEmitter.DeleteAnalyzerTarget(prev.analyzer, namespace, modelID, prev.variant)
		}
	}
	e.lastAnalyzerSeries[modelKey] = current
}

// pruneAnalyzerSeries evicts every analyzer series belonging to a model that is
// no longer reconciled. Mirrors pruneLastGoodAnalysis, including its empty-set
// guard — though at this call site activeKeys is never empty, because a cycle
// with no active models returns before reaching here (and evicts everything via
// evictAllAnalyzerSeries instead). The guard is kept for symmetry with
// pruneLastGoodAnalysis and to stay safe if the call site moves.
//
// Note this only covers models that stop being *enumerated*. A model that is
// still enumerated but skips analysis this cycle — config not loaded, a
// prepare/collect error — keeps its last published series deliberately: a
// transient collection blip should not make the series flap in and out, and
// staleness there is reported separately through the analyzer liveness signal.
func (e *Engine) pruneAnalyzerSeries(activeKeys map[string]bool) {
	if len(activeKeys) == 0 || e.lastAnalyzerSeries == nil {
		return
	}
	for modelKey, series := range e.lastAnalyzerSeries {
		if !activeKeys[modelKey] {
			e.metricsEmitter.DeleteAnalyzerSeriesForModel(series.namespace, series.modelID)
			delete(e.lastAnalyzerSeries, modelKey)
		}
	}
}

// evictAllAnalyzerSeries removes every analyzer series the engine has published,
// for every model it tracks. Used when a cycle analyzes no models at all, where
// the per-model prune has nothing to compare against: without this, the series
// would keep their last values with nothing left to refresh or contradict them.
// Safe to call repeatedly — it empties its own bookkeeping, so subsequent idle
// cycles are no-ops.
func (e *Engine) evictAllAnalyzerSeries() {
	for modelKey, series := range e.lastAnalyzerSeries {
		e.metricsEmitter.DeleteAnalyzerSeriesForModel(series.namespace, series.modelID)
		delete(e.lastAnalyzerSeries, modelKey)
	}
}

// updateLivenessAndSetLive refreshes e.lastGoodAnalysis for this model with
// every informative result's AnalyzedAt (or the current time, as a fail-safe,
// when an informative result carries a zero-valued AnalyzedAt), then sets
// nr.Live on each entry in place based on whether its last good analysis (if
// any) is within the staleness window. Applies uniformly to every analyzer,
// including saturation — no name-based exemption. After the liveness pass it
// runs the warn-only demand-liveness detector (see detectDemandLiveness).
func (e *Engine) updateLivenessAndSetLive(
	ctx context.Context,
	namespace, modelID string,
	namedResults []allocation.NamedAnalyzerResult,
) {
	if e.lastGoodAnalysis == nil {
		e.lastGoodAnalysis = make(map[string]map[string]time.Time)
	}
	modelKey := utils.GetNamespacedKey(namespace, modelID)
	if e.lastGoodAnalysis[modelKey] == nil {
		e.lastGoodAnalysis[modelKey] = make(map[string]time.Time)
	}
	perAnalyzer := e.lastGoodAnalysis[modelKey]

	// Config is nil in unit tests that construct a minimal Engine directly
	// (bypassing NewEngine, which panics on a nil Config in production). A present
	// Config returning a non-positive interval falls back to the same 30s default —
	// a zero/negative interval would otherwise zero the threshold below and latch
	// every analyzer non-live, blocking all scale-down.
	interval := 30 * time.Second
	if e.Config != nil && e.Config.OptimizationInterval() > 0 {
		interval = e.Config.OptimizationInterval()
	}
	now := time.Now()
	threshold := analyzerLivenessStaleCycles * interval

	for i := range namedResults {
		nr := &namedResults[i]
		if allocation.ResultIsInformative(*nr) {
			at := nr.Result.AnalyzedAt
			if at.IsZero() {
				// Fail-safe: an informative result observed this cycle is current by
				// definition, so a forgotten AnalyzedAt cannot silently disarm the veto.
				at = now
			}
			perAnalyzer[nr.Name] = at
		}
		lastGood, ok := perAnalyzer[nr.Name]
		nr.Live = ok && now.Sub(lastGood) <= threshold
	}

	e.detectDemandLiveness(ctx, modelID, namespace, namedResults, perAnalyzer, now, threshold)
}

// demandLatchSuffix is appended to an analyzer name to form the synthetic inner
// key under which a demand latch is stored in lastGoodAnalysis. The NUL byte
// guarantees the key cannot collide with any real analyzer name (analyzer names
// are printable identifiers), so the Live/veto path — which only reads the map
// via keyed lookups on real analyzer names (perAnalyzer[nr.Name]) and never
// ranges over it in the decision path — can never observe this key and can
// never have an nr.Live flipped by it.
//
// DEFERRED (future, per-pod demand): when demand becomes per-replica, the demand
// latch key extends with a pod component (analyzerName + demandLatchSuffix +
// "\x00" + podID) and a per-replica latch is tracked alongside the model-level
// one. Not built now — 0.9 demand is model-level only.
const demandLatchSuffix = "\x00demand"

// detectDemandLiveness emits a warn-only observability signal when the
// throughput analyzer has a live capacity/supply signal but has reported no
// demand (Result.TotalDemand > 0) for at least the staleness window. It never
// sets nr.Live, never touches RoleSpare, and never gates any decision.
//
// Why warn-only, and why a veto here would be wrong:
//   - Zero demand is a legitimate state, not necessarily a fault. With no
//     served-rate floor, arrival→0 correctly drives TotalDemand→0, which only
//     permits scale-down and never forces a scale action. A missing or zero
//     arrival signal can therefore never cause a spurious scale-up or a spurious
//     veto, so vetoing on "demand looks absent" would defeat the very scale-down
//     the liveness gate exists to enable.
//   - The veto path is already covered by the supply/capacity liveness gate: an
//     analyzer with no capacity signal is excluded from the scale-down vote via
//     nr.Live. This detector is strictly additive telemetry pointing a human at
//     a likely-broken arrival query.
//
// The detector pairs two latches in the same per-model map:
//   - Supply latch: perAnalyzer[throughput.AnalyzerName], the analyzer's
//     last-informative timestamp (maintained by the liveness loop above; the
//     throughput analyzer resolves per-replica capacity regardless of arrival).
//   - Demand latch: perAnalyzer[throughput.AnalyzerName+demandLatchSuffix],
//     stamped with AnalyzedAt each cycle TotalDemand > 0.
//
// The signal is a timestamp gap, not a boolean, so a cold-start EPP scrape lag
// (supply comes up a cycle or two before the first arrival scrape) does not
// false-positive: on the first cycle the demand latch is seeded to the current
// supply timestamp, so the gap starts at zero and only reaches the staleness
// window after demand has genuinely been absent for that long.
func (e *Engine) detectDemandLiveness(
	ctx context.Context,
	modelID, namespace string,
	namedResults []allocation.NamedAnalyzerResult,
	perAnalyzer map[string]time.Time,
	now time.Time,
	threshold time.Duration,
) {
	var tp *allocation.NamedAnalyzerResult
	for i := range namedResults {
		if namedResults[i].Name == throughput.AnalyzerName {
			tp = &namedResults[i]
			break
		}
	}
	if tp == nil {
		return // throughput not participating this cycle
	}

	demandKey := throughput.AnalyzerName + demandLatchSuffix

	// Stamp the demand latch whenever demand is observed.
	if tp.Result != nil && tp.Result.TotalDemand > 0 {
		perAnalyzer[demandKey] = tp.Result.AnalyzedAt
	}

	// Only meaningful while supply is live now; otherwise the supply liveness
	// gate already covers the situation and there is nothing to seed or compare.
	supplyTS, hasSupply := perAnalyzer[throughput.AnalyzerName]
	if !hasSupply || now.Sub(supplyTS) > threshold {
		return
	}

	// Seed the demand latch to the current supply timestamp the first time we
	// see live supply, so a fresh model / cold start starts with a zero gap.
	demandTS, hasDemand := perAnalyzer[demandKey]
	if !hasDemand {
		perAnalyzer[demandKey] = supplyTS
		return
	}

	if supplyTS.Sub(demandTS) >= threshold {
		logger := ctrl.LoggerFrom(ctx)
		logger.Info("throughput analyzer has a live capacity signal but has reported no demand "+
			"for at least the staleness window; the request-arrival query is likely misconfigured "+
			"or EPP is not reporting arrivals — scale-up will not trigger until arrivals are observed",
			"modelID", modelID,
			"namespace", namespace,
			"analyzer", throughput.AnalyzerName,
			"noDemandFor", supplyTS.Sub(demandTS).String(),
		)
	}
}

// pruneLastGoodAnalysis evicts liveness state for models that are no longer
// active. activeKeys is the set of currently-active model keys (as produced by
// utils.GetNamespacedKey(namespace, modelID)); any outer key in
// e.lastGoodAnalysis absent from activeKeys belongs to a departed model (its
// VariantAutoscalings are gone) and is deleted, bounding the map to live models
// rather than letting it grow unboundedly across the controller's lifetime.
//
// Guards against an empty active set: a cycle that enumerates no models (e.g.
// saturation config not loaded yet) must not wipe accumulated state, so pruning
// is skipped when activeKeys is empty. This also means a genuine all-models-removed
// state is indistinguishable from a transient empty cycle here: those entries leak
// until a model reappears. Accepted, bounded leak — distinguishing the two cases
// would need a separate "models were present last cycle" signal.
func (e *Engine) pruneLastGoodAnalysis(activeKeys map[string]bool) {
	if len(activeKeys) == 0 || e.lastGoodAnalysis == nil {
		return
	}
	for modelKey := range e.lastGoodAnalysis {
		if !activeKeys[modelKey] {
			delete(e.lastGoodAnalysis, modelKey)
		}
	}
}

// runRegisteredAnalyzer invokes a single non-saturation analyzer's Analyze
// method, isolating the call from the rest of the cycle. Errors are logged
// and nil is returned; panics are recovered and logged. Returns the result
// so the caller can apply the universal threshold post-step before discarding.
func runRegisteredAnalyzer(
	ctx context.Context,
	logger logr.Logger,
	entry analyzerEntry,
	modelID string,
	input domain.AnalyzerInput,
) (result *domain.AnalyzerResult) {
	defer func() {
		if r := recover(); r != nil {
			// Plugin failure is non-fatal; log at debug to avoid spamming
			// operator logs every optimize cycle.
			logger.V(logging.DEBUG).Info("registered analyzer panicked; result discarded",
				"name", entry.name, "modelID", modelID, "panic", fmt.Sprintf("%v", r))
			result = nil
		}
	}()
	var err error
	result, err = entry.analyzer.Analyze(ctx, input)
	if err != nil {
		// Plugin failure is non-fatal; log at debug to avoid spamming
		// operator logs every optimize cycle.
		logger.V(logging.DEBUG).Info("registered analyzer failed; result discarded",
			"name", entry.name, "modelID", modelID, "error", err)
		return nil
	}
	return result
}

// applyUniversalThreshold recalibrates RequiredCapacity and SpareCapacity in
// place for the analyzer result and every RoleCapacity entry using the pure
// formula:
//
//	RC = max(0, TotalDemand/scaleUp − TotalAnticipatedSupply)
//	SC = max(0, TotalSupply    − TotalDemand/scaleDown)
//
// TotalAnticipatedSupply is read as-is — zero is a literal value, not a
// sentinel. The analyzer is responsible for populating it correctly (see
// internal/engines/aggregation for shared helpers). The asymmetry preserves
// the conservative "don't double-scale while replicas are launching, don't
// count pending as removable" stance.
//
// The same formula and the same (scaleUp, scaleDown) are applied at every
// scope — model-level and each RoleCapacity entry. There are no per-role
// threshold overrides. A non-positive scaleUp or scaleDown leaves the
// corresponding signal unchanged.
func applyUniversalThreshold(nr *allocation.NamedAnalyzerResult, scaleUp, scaleDown float64) {
	if nr == nil || nr.Result == nil {
		return
	}
	demand := nr.Result.TotalDemand

	if scaleUp > 0 {
		rc := demand/scaleUp - nr.TotalAnticipatedSupply
		if rc < 0 {
			rc = 0
		}
		nr.RequiredCapacity = rc
	}
	if scaleDown > 0 {
		sc := nr.TotalSupply - demand/scaleDown
		if sc < 0 {
			sc = 0
		}
		nr.SpareCapacity = sc
	}

	for role, rc := range nr.RoleCapacities {
		if scaleUp > 0 {
			v := rc.TotalDemand/scaleUp - rc.TotalAnticipatedSupply
			if v < 0 {
				v = 0
			}
			rc.RequiredCapacity = v
		}
		if scaleDown > 0 {
			v := rc.TotalSupply - rc.TotalDemand/scaleDown
			if v < 0 {
				v = 0
			}
			rc.SpareCapacity = v
		}
		nr.RoleCapacities[role] = rc
	}
}

// gpuUsageViews assembles both measures of current GPU usage for this cycle, so
// each constraint provider can be fed the one it actually asked for (see
// allocation.UsageBasis). They answer different questions and are not
// interchangeable: broadcasting one figure to every provider is what this
// replaces.
//
// PHYSICAL comes from the shared snapshot. The engine READS that now; it used to
// write it. internal/gpuusage is its sole producer, discovering usage from the
// pods occupying GPU nodes rather than from whatever population this cycle
// happened to assemble, which changes what the number means in two ways:
//
//   - it no longer depends on discovery. A population sum is blind to any
//     workload WVA has not been called about, so it reported a full cluster as
//     empty during the window before KEDA's first call — and that is exactly when
//     scale-from-zero is placing wakes;
//   - it includes GPUs held by workloads WVA does not manage. Budgets get
//     correspondingly tighter, which is the point: they were over-stated before.
//
// MANAGED is summed from this cycle's population, and is the figure a quota draws
// against — an allowance granted to WVA may only be spent by WVA. It is always
// available here, because the population is what this engine is holding.
//
// The requests serve one further purpose on BOTH views: every namespace being
// optimized must be PRESENT in the per-namespace map, with an empty inner map if
// it holds nothing. A namespace-scoped quota is materialised only for namespaces
// that appear there (see DefaultLimiter.ComputeConstraints), so a namespace whose
// fleet currently holds no GPUs would silently lose its cap and be judged against
// the cluster aggregate — able to exceed its own limit, or refused because a
// different namespace is full. The population sum gives this by construction; a
// discovered view cannot, because it only sees namespaces holding a GPU right now.
func gpuUsageViews(requests []allocation.ModelScalingRequest) allocation.GPUUsageViews {
	views := allocation.GPUUsageViews{
		ManagedByType:      computeCurrentGPUUsage(requests),
		ManagedByNamespace: computeCurrentGPUUsageByNamespace(requests),
	}
	// The warm pools too, and HERE rather than only where the managed figure is
	// published. These views are what the constraint providers are given, so a
	// quota built without them does not bind on the pool at all -- the published
	// figure would say the pool costs something while the limiter enforcing it
	// carried on as though it did not.
	addWarmPoolGPUs(views.ManagedByType, views.ManagedByNamespace)
	if snap, ok := decision.LatestGPUUsage(); ok {
		views.PhysicalByType = snap.ByType
		views.PhysicalByNamespace = withActiveNamespaces(snap.ByNamespace, requests)
	}
	return views
}

// withActiveNamespaces returns byNamespace with an entry for every namespace in
// requests, adding empty ones as needed. The stored snapshot must not be mutated
// (GPUUsageStore.Get documents it as shared), so it copies — but only when there
// is something to add, which is the uncommon case once a namespace has any
// GPU-holding pod.
func withActiveNamespaces(
	byNamespace map[string]map[string]int,
	requests []allocation.ModelScalingRequest,
) map[string]map[string]int {
	out := byNamespace
	copied := false
	for _, req := range requests {
		if _, present := out[req.Namespace]; present {
			continue
		}
		if !copied {
			out = make(map[string]map[string]int, len(byNamespace)+1)
			maps.Copy(out, byNamespace)
			copied = true
		}
		out[req.Namespace] = map[string]int{}
	}
	return out
}

// gpuUsageByType accumulates a request's current GPU usage into perType.
//
// Accelerator identity comes from the request's discovery metadata, the only
// place that carries it. Replica counts come from VariantReplicaState rather
// than from the analyzer's VariantCapacity: both are in scale-target units, but
// GPU accounting must reflect what is actually allocated, not what reported
// metrics this cycle.
//
// KNOWN LIMITATION — unresolved accelerators are not attributable. A variant
// with neither a nodeSelector/nodeAffinity GPU key nor the
// inference.optimization/acceleratorName label resolves to
// constants.DefaultAcceleratorName ("unknown"), so its GPUs are keyed under a
// placeholder. TypeInventory.GetResourcePools iterates the DISCOVERED types, so
// that entry never lands in a pool: a quota fed this view sees less consumed than
// there is, and reports more of its allowance free than it should. SetUsed,
// meanwhile, sums every key into totalUsed, so the aggregate includes them — the
// two views disagree, though nothing reads the aggregate today.
//
// This is not fixable here: usage that cannot be attributed to a type cannot be
// charged to a pool, and charging it to every candidate type would be worse.
// The durable fix is resolving the accelerator. Both behaviours are pinned by
// internal/engines/pipeline/unresolved_accelerator_usage_test.go so they cannot
// drift silently.
func gpuUsageByType(req allocation.ModelScalingRequest, perType map[string]int) {
	stateMap := make(map[string]domain.VariantReplicaState, len(req.VariantStates))
	for _, s := range req.VariantStates {
		stateMap[s.VariantName] = s
	}
	for _, m := range req.Variants {
		state := stateMap[m.VariantName]
		gpusPerReplica := state.GPUsPerReplica
		if gpusPerReplica <= 0 {
			gpusPerReplica = 1
		}
		perType[m.AcceleratorName] += state.CurrentReplicas * gpusPerReplica
	}
}

// computeCurrentGPUUsage sums the GPUs WVA's own variants hold, per accelerator
// type, across this cycle's population. This is the ManagedUsage view: the only
// consumption an operator-declared quota may be charged for.
func computeCurrentGPUUsage(requests []allocation.ModelScalingRequest) map[string]int {
	usage := make(map[string]int)
	for _, req := range requests {
		if !hasSaturationResult(req) {
			continue
		}
		gpuUsageByType(req, usage)
	}
	return usage
}

// computeCurrentGPUUsageByNamespace mirrors computeCurrentGPUUsage but buckets
// usage by namespace, then accelerator type. Every request's namespace is
// represented (with at least an empty per-type map) so namespaces carrying a
// quota but zero current usage are still surfaced as active namespaces to the
// constraint providers, and therefore still constrained.
func computeCurrentGPUUsageByNamespace(requests []allocation.ModelScalingRequest) map[string]map[string]int {
	usage := make(map[string]map[string]int)
	for _, req := range requests {
		perType, ok := usage[req.Namespace]
		if !ok {
			perType = make(map[string]int)
			usage[req.Namespace] = perType
		}
		if !hasSaturationResult(req) {
			continue
		}
		gpuUsageByType(req, perType)
	}
	return usage
}

// hasSaturationResult reports whether the request carries a saturation analyzer
// result. A request without one was not measured this cycle, so its replica
// counts are not evidence of anything and must not be charged to a quota.
func hasSaturationResult(req allocation.ModelScalingRequest) bool {
	for _, e := range req.AnalyzerResults {
		if e.Name == domain.SaturationAnalyzerName {
			return e.Result != nil
		}
	}
	return false
}

// reportUnattributedGPUs surfaces usage that could not be charged to any
// accelerator type.
//
// GetResourcePools iterates the DISCOVERED types, so a bucket keyed by the
// unresolved sentinel never lands in a pool: those GPUs are held but invisible,
// every pool over-states how much is free by that amount, and the wake capacity
// check can allow a placement it should refuse. Nothing about that fails — it is
// silent over-provisioning, which is why it is worth a metric and a warning
// rather than a debug line.
//
// This reports the condition; it deliberately does not change the budget policy,
// which is a separate decision about whether an unattributable variant should
// block scaling for everyone.
func reportUnattributedGPUs(ctx context.Context, byType map[string]int) {
	unattributed, names := unattributedGPUs(byType)
	metrics.SetUnattributedGPUs(unattributed)
	if unattributed == 0 {
		return
	}
	ctrl.LoggerFrom(ctx).Info(
		"GPUs are held by variants whose accelerator could not be resolved; "+
			"they are charged to no accelerator pool, so free capacity is over-stated by this amount "+
			"and a wake may be allowed onto an accelerator that is full. "+
			"Set a GPU product nodeSelector on the workload, or the accelerator label on its scaler.",
		"unattributedGPUs", unattributed, "underKeys", names)
}

// gpuConstraintProviders returns the ConstraintProvider(s) backing the GPU
// limiter for the V2 optimizer path. A limiter that is itself a
// ConstraintProvider (a *DefaultLimiter) contributes itself; a CompositeLimiter
// contributes each constituent that is a ConstraintProvider, so multi-entry
// quota configs are all consulted. Other limiter shapes (e.g. NoOpLimiter)
// contribute nothing.
func gpuConstraintProviders(l allocation.Limiter) []allocation.ConstraintProvider {
	return allocation.ConstraintProvidersFrom(l)
}

// collectV2ModelRequest performs V2 analysis for a single model and returns
// a ModelScalingRequest for the optimizer, or nil if analysis should be skipped.
func (e *Engine) collectV2ModelRequest(
	ctx context.Context,
	modelID, namespace string,
	replicaMetrics []domain.ReplicaMetrics,
	config config.ScalingPolicy,
	variantStates []domain.VariantReplicaState,
	variantMetadata []domain.VariantMetadata,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	schedulerQueue *domain.SchedulerQueueMetrics,
	arrivalRate float64,
) (*allocation.ModelScalingRequest, error) {
	namedResults, err := e.runAnalyzersAndScore(ctx, modelID, namespace, replicaMetrics, config,
		variantStates, variantMetadata, scaleTargets, variantAutoscalings, schedulerQueue, arrivalRate)
	if err != nil {
		return nil, fmt.Errorf("collecting V2 model request for %s/%s: %w", namespace, modelID, err)
	}

	// Detect P/D disaggregation: true when any variant has role != domain.RoleBoth
	disaggregated := false
	for _, vs := range variantStates {
		if vs.Role != "" && vs.Role != domain.RoleBoth {
			disaggregated = true
			break
		}
	}

	return &allocation.ModelScalingRequest{
		ModelID:         modelID,
		Namespace:       namespace,
		AnalyzerResults: namedResults,
		VariantStates:   variantStates,
		Variants:        variantMetadata,
		Priority:        config.Priority,
		Disaggregated:   disaggregated,
	}, nil
}

// metadataByVariant indexes discovery metadata by variant name for the capacity
// builder.
func metadataByVariant(variantMetadata []domain.VariantMetadata) map[string]domain.VariantMetadata {
	byName := make(map[string]domain.VariantMetadata, len(variantMetadata))
	for _, m := range variantMetadata {
		byName[m.VariantName] = m
	}
	return byName
}

// buildNamedResult wraps one analyzer's raw (D, P) result in the optimizer-facing
// entry: it runs the capacity-build step to derive the engine-owned aggregates,
// then seeds the optimizer's mutable allocation counters from the built signals.
//
// Ordering matters — Remaining/Spare are the optimizer's working copies of RC/SC,
// so they can only be seeded after buildCapacities has computed them.
func buildNamedResult(
	ctx context.Context,
	name string,
	result *domain.AnalyzerResult,
	config config.ScalingPolicy,
	metaByVariant map[string]domain.VariantMetadata,
	scaleUp, scaleDown float64,
) allocation.NamedAnalyzerResult {
	nr := allocation.NamedAnalyzerResult{
		Name:              name,
		Result:            result,
		Score:             config.AnalyzerScore(name),
		ScaleUpThreshold:  scaleUp,
		ScaleDownBoundary: scaleDown,
	}
	buildCapacities(ctx, &nr, metaByVariant, scaleUp, scaleDown)
	nr.Remaining = nr.RequiredCapacity
	nr.Spare = nr.SpareCapacity
	return nr
}

// buildCapacities is the dedicated capacity-building step between the analyzers
// and the optimizer. For one analyzer's result it (1) joins the authoritative
// per-variant identity (cost, accelerator, role) from the discovery step onto the
// analyzer's per-variant capacities, and (2) computes the engine-owned scaling
// signals (RequiredCapacity/SpareCapacity) via the universal threshold. The
// analyzer supplies only the measured signal (demand + per-replica capacity); the
// builder assembles the structure the optimizer consumes. The metadata join is a
// no-op when metaByVariant is empty (paths without discovery keep analyzer values).
func buildCapacities(ctx context.Context, nr *allocation.NamedAnalyzerResult, metaByVariant map[string]domain.VariantMetadata, scaleUp, scaleDown float64) {
	if nr == nil || nr.Result == nil {
		return
	}
	result := nr.Result
	// (1) Reconcile the analyzer's per-variant view with discovery's.
	//
	// Role is joined because the per-role supply grouped below must be keyed the
	// same way discovery keys the fleet, or RoleDemand and that supply would pair
	// up wrongly. Cost and accelerator left VariantCapacity entirely; the
	// optimizer reads those from VariantMetadata.
	//
	// ReplicaCount is clamped to the scale target's own count, so that the supply
	// assembled below cannot describe a fleet larger than the one the target is
	// already committed to. See clampReplicaCountToScaleTarget.
	if len(metaByVariant) > 0 {
		for i := range result.VariantCapacities {
			if m, ok := metaByVariant[result.VariantCapacities[i].VariantName]; ok {
				result.VariantCapacities[i].Role = m.Role
				clampReplicaCountToScaleTarget(&result.VariantCapacities[i], m)
			}
		}
	}
	// (2) Assemble supply from each variant's (ReplicaCount, per-replica P), and
	// the model-level utilization from demand vs supply.
	nr.TotalSupply = aggregation.SumTotalSupply(result.VariantCapacities)
	nr.TotalAnticipatedSupply = aggregation.SumTotalAnticipatedSupply(result.VariantCapacities)
	if nr.TotalSupply > 0 {
		nr.Utilization = result.TotalDemand / nr.TotalSupply
	} else {
		nr.Utilization = 0
	}
	// (3) Assemble per-role capacities: supply grouped by role, demand from the
	// analyzer's per-role attribution (RoleDemand).
	nr.RoleCapacities = buildRoleCapacities(ctx, result)
	// (4) Engine-owned scaling signals (RC/SC) from demand vs supply.
	applyUniversalThreshold(nr, scaleUp, scaleDown)
	// (5) Flag a shortfall the optimizer cannot act on.
	warnUnsizableShortfall(ctx, nr)
}

// clampReplicaCountToScaleTarget caps a variant's measured replica count at the
// scale target's own count. It never raises it.
//
// ReplicaCount is what actually reported capacity this cycle, which is the right
// basis for supply while the fleet is steady. During a scale-down that is
// already in flight it is not: the scale target has been reduced while the
// condemned replicas keep reporting, so the measured count runs AHEAD of the
// target. Supply built from the larger count then credits the optimizer with
// spare capacity that the in-flight scale-down has already claimed, and
// safeRemovalReplicasForRole subtracts it a second time from the smaller count —
// removing those replicas twice for one decision.
//
// Measured on a 14 QPS benchmark: with the target already down to 3 and five
// replicas still reporting, SC came to two replicas of slack and the decision
// was 3 − 2 = 1, mid-load. Clamped, the same inputs hold at 3.
//
// This is the departure half of what TotalAnticipatedSupply does for arrivals:
// there, a launching replica counts toward supply so a scale-up is not ordered
// twice while it boots. PendingReplicas only ever describes replicas arriving,
// so nothing covered the same double-count on the way down.
//
// The clamp is ONE-SIDED on purpose. When the measured count is LOWER than the
// target — replicas still launching, or a pod that missed a scrape — the
// measured value still wins, because it is what demonstrably served this cycle.
// Counting capacity that has not arrived yet is how a fleet gets scaled down
// onto replicas that cannot take the load.
//
// The two signals it feeds are NOT conservative in the same direction, and the
// scale-up side is the one to watch. SC shrinks, which can only hold replicas.
// RC is sized against TotalAnticipatedSupply, which shrinks too, while demand is
// still summed over the larger pod set — including resident KV on the condemned
// replicas that is about to drain. Since a scale-up rounds up
// (analyzer_helpers.go), a fractional shortfall orders a whole replica. Replaying
// the benchmark: of the 45 passes where the clamp fires, 5 turn RC positive, each
// anticipating by 15-30s a scale-up that pass made anyway. Excluding condemned
// demand symmetrically would settle it, and needs a signal Kubernetes does not
// publish (see below).
//
// A CurrentReplicas of zero is skipped rather than clamped to zero. Zero reaches
// here only for a variant whose scale target IS readable and reports no replicas
// — a parked one — because discovery drops a variant whose scale target it could
// not resolve before any metadata exists (variantmeta.discovery, `continue` on an
// unresolved target), so that case never arrives at all. Clamping a parked
// variant to zero would erase supply for replicas still draining, and nothing is
// gained: a decision based at zero replicas cannot remove any.
//
// Kubernetes publishes no terminating-replica count on a scale target's status,
// so the symmetric fix — a departing counterpart to PendingReplicas — would mean
// listing pods, which is the cluster-wide watch this design removed. Clamping to
// a count the scale target already publishes is what makes this cheap.
func clampReplicaCountToScaleTarget(vc *domain.VariantCapacity, m domain.VariantMetadata) {
	if m.CurrentReplicas > 0 {
		vc.ReplicaCount = min(vc.ReplicaCount, m.CurrentReplicas)
	}
}

// warnUnsizableShortfall logs when an analyzer reports that more capacity is
// needed while no variant has a per-replica capacity to supply it with. The
// optimizer sizes a scale-up as demand/target ÷ per-replica capacity, so with
// every capacity at zero there is nothing to divide by: the engine asks for
// capacity every cycle and can never act on the answer, which reads from the
// outside exactly like a stuck autoscaler.
//
// The usual cause is configuration rather than load — a KvCacheThreshold small
// enough that the usable-capacity estimate rounds away, or a variant whose
// capacity was never learned — so the log names the variants involved.
func warnUnsizableShortfall(ctx context.Context, nr *allocation.NamedAnalyzerResult) {
	result := nr.Result
	if nr.RequiredCapacity <= 0 || len(result.VariantCapacities) == 0 {
		return
	}
	for _, vc := range result.VariantCapacities {
		if vc.PerReplicaCapacity > 0 {
			return // at least one variant can absorb the shortfall
		}
	}
	names := make([]string, 0, len(result.VariantCapacities))
	for _, vc := range result.VariantCapacities {
		names = append(names, vc.VariantName)
	}
	sort.Strings(names)
	ctrl.LoggerFrom(ctx).Info("analyzer needs capacity but no variant has a per-replica capacity to scale with",
		"modelID", result.ModelID, "namespace", result.Namespace,
		"analyzer", result.AnalyzerName, "requiredCapacity", nr.RequiredCapacity,
		"variants", names,
		"hint", "check kvCacheThreshold: a value small enough to round the usable-capacity estimate to zero makes the shortfall unsizable")
}

// buildRoleCapacities pairs per-role supply (grouped from the variant capacities)
// with the analyzer's per-role demand to produce the RoleCapacities the optimizer
// consumes for P/D-disaggregated models. Returns nil when the analyzer emitted no
// per-role demand (non-disaggregated). RequiredCapacity/SpareCapacity are left
// zero here — the universal threshold post-step fills them.
//
// Pairing the two halves relies on an invariant the type system cannot express:
// the roles an analyzer keyed its demand by must be the roles the variant
// capacities are keyed by. Both come from the same discovery output in a cycle
// (the engine projects VariantMetadata into the analyzers' VariantReplicaState
// and re-joins it in step (1) of buildCapacities), so they agree in practice. A
// role carrying demand with no variants behind it would otherwise silently
// produce a zero-supply bucket, which the threshold post-step reads as an
// unservable shortfall and turns into a spurious scale-up — so log it rather
// than let it pass unnoticed.
func buildRoleCapacities(ctx context.Context, result *domain.AnalyzerResult) map[string]domain.RoleCapacity {
	roleDemand := result.RoleDemand
	if len(roleDemand) == 0 {
		return nil
	}
	totals := aggregation.AggregateByRole(result.VariantCapacities)
	out := make(map[string]domain.RoleCapacity, len(roleDemand))
	for role, demand := range roleDemand {
		t, ok := totals[role]
		if !ok && demand > 0 {
			// modelID/namespace included so the line is actionable: the engine
			// reconciles many models per cycle into one controller log.
			ctrl.LoggerFrom(ctx).Info("analyzer attributed demand to a role with no variants",
				"modelID", result.ModelID, "namespace", result.Namespace,
				"analyzer", result.AnalyzerName, "role", role, "demand", demand,
				"variantRoles", rolesOf(totals))
		}
		out[role] = domain.RoleCapacity{
			Role:                   role,
			TotalSupply:            t.TotalSupply,
			TotalDemand:            demand,
			TotalAnticipatedSupply: t.TotalAnticipatedSupply,
		}
	}
	return out
}

// rolesOf returns the roles present in a per-role aggregation, sorted so the
// log line above is stable across cycles.
func rolesOf(totals map[string]aggregation.ScopeTotals) []string {
	roles := make([]string, 0, len(totals))
	for role := range totals {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}

// logAnalyzerResult emits one INFO "analyzer-result" line for a single named
// analyzer result. Called for every analyzer that ran in a model's reconcile
// cycle, after the universal threshold post-step has been applied.
func logAnalyzerResult(ctx context.Context, modelID, namespace string, nr allocation.NamedAnalyzerResult) {
	if nr.Result == nil {
		return
	}
	logger := ctrl.LoggerFrom(ctx)

	type variantEntry struct {
		Name string  `json:"name"`
		PRC  float64 `json:"prc"`
		// Role is included because V2 charges waiting requests by P/D role, so
		// demand is not interpretable without knowing which role was resolved —
		// a missing llm-d.ai/role label reads as "both" and changes the charge.
		// Emitted unconditionally: an analyzer that leaves Role unset (the
		// queueing model does) is treated as "both" downstream, so the value is
		// canonicalized below and the field always carries a meaningful role
		// rather than being absent.
		Role   string `json:"role"`
		Reason string `json:"reason,omitempty"`
	}
	variants := make([]variantEntry, 0, len(nr.Result.VariantCapacities))
	for _, vc := range nr.Result.VariantCapacities {
		role := vc.Role
		if role == "" {
			role = domain.RoleBoth
		}
		variants = append(variants, variantEntry{
			Name:   vc.VariantName,
			PRC:    vc.PerReplicaCapacity,
			Role:   role,
			Reason: vc.Reason,
		})
	}

	logger.Info("analyzer-result",
		"modelID", modelID,
		"namespace", namespace,
		"analyzer", nr.Name,
		"supply", nr.TotalSupply,
		"demand", nr.Result.TotalDemand,
		"util", nr.Utilization,
		"rc", nr.RequiredCapacity,
		"sc", nr.SpareCapacity,
		"scaleUpThreshold", nr.ScaleUpThreshold,
		"scaleDownBoundary", nr.ScaleDownBoundary,
		"variants", variants,
	)
}

// logScalingDecisions emits one INFO "scaling-decision" line per model after
// the optimizer has produced per-variant decisions.
func logScalingDecisions(
	ctx context.Context,
	modelRequests []allocation.ModelScalingRequest,
	decisions []domain.VariantDecision,
) {
	logger := ctrl.LoggerFrom(ctx)

	type modelKey struct{ ns, modelID string }
	type decisionEntry struct {
		Name   string `json:"name"`
		Curr   int    `json:"curr"`
		Tgt    int    `json:"tgt"`
		Action string `json:"action"`
		// AtMax reports that the target sits on the variant's own maxReplicas
		// ceiling, so it cannot grow further however much demand asks for.
		//
		// WasLimited does not cover this and deliberately never will: it means
		// GPUs ran out, and marking a self-imposed ceiling as scarcity would
		// raise a spurious ResourceConstrained warning (see
		// gpu_limit_attribution.go). But without SOME signal the two outcomes
		// are indistinguishable in the log -- a run pinned at its ceiling and a
		// run that got everything it asked for both read as a steady target.
		//
		// That is not hypothetical: a benchmark capped at 4 while demand implied
		// 12 produced a P99 TTFT of 73s and an average of 3.21 replicas, and
		// nothing in the output said the number was a ceiling rather than a
		// decision. Read it beside `demand` and `prc` in analyzer-result: at max
		// with demand still climbing means raise maxReplicas, which is a
		// different remedy from adding GPUs.
		AtMax bool `json:"atMax,omitempty"`
	}

	// maxReplicas per variant, from the request the optimizer was given. The
	// decisions themselves do not carry the ceiling.
	maxByVariant := make(map[modelKey]map[string]int, len(modelRequests))
	for _, req := range modelRequests {
		k := modelKey{req.Namespace, req.ModelID}
		for _, st := range req.VariantStates {
			if st.MaxReplicas == nil || *st.MaxReplicas <= 0 {
				continue
			}
			if maxByVariant[k] == nil {
				maxByVariant[k] = make(map[string]int, len(req.VariantStates))
			}
			maxByVariant[k][st.VariantName] = *st.MaxReplicas
		}
	}

	grouped := make(map[modelKey][]decisionEntry, len(modelRequests))
	for _, d := range decisions {
		k := modelKey{d.Namespace, d.ModelID}
		atMax := false
		if m, ok := maxByVariant[k][d.VariantName]; ok {
			atMax = d.TargetReplicas >= m
		}
		grouped[k] = append(grouped[k], decisionEntry{
			Name:   d.VariantName,
			Curr:   d.CurrentReplicas,
			Tgt:    d.TargetReplicas,
			Action: string(d.Action),
			AtMax:  atMax,
		})
		metrics.SetVariantAtMaxReplicas(d.Namespace, d.VariantName, atMax)
	}

	for _, req := range modelRequests {
		k := modelKey{req.Namespace, req.ModelID}
		entries := grouped[k]
		if len(entries) == 0 {
			continue
		}
		logger.Info("scaling-decision",
			"modelID", req.ModelID,
			"namespace", req.Namespace,
			"decisions", entries,
		)
	}
}

// unattributedGPUs totals usage that no accelerator pool accounts for, and names
// the keys it was found under.
//
// Both the empty string and the "unknown" sentinel land here: they arrive by
// different routes — discovery resolving nothing at all versus resolving to the
// placeholder — and have identical consequences, so they are counted together.
// Keys come back sorted so the log line is stable across cycles.
func unattributedGPUs(byType map[string]int) (total int, keys []string) {
	for accType, gpus := range byType {
		if constants.IsAcceleratorResolved(accType) {
			continue
		}
		total += gpus
		keys = append(keys, accType)
	}
	sort.Strings(keys)
	return total, keys
}

// publishHeadroomForIdleFleet answers "how many GPUs may this namespace still
// take" when there is nothing to optimize.
//
// The usage it measures against is gpuUsageViews(nil): no variants hold
// anything, so what remains is whatever the warm pools hold. That is the whole
// point -- a pool is WVA's own consumption and is charged against the same
// allowance, so a pool at its quota must read as no headroom left rather than as
// a namespace nobody bounds.
//
// Silent on every failure. Publishing a wrong figure here is worse than
// publishing none: an absent namespace reads as unbounded, which is the
// behaviour that existed before this function, while a fabricated one would cap
// a pool for a reason nobody could find.
func (e *Engine) publishHeadroomForIdleFleet(ctx context.Context) {
	providers := gpuConstraintProviders(e.currentGPULimiter())
	if len(providers) == 0 {
		return // nothing bounds anything; the pool grows freely, as it should
	}
	views := gpuUsageViews(nil)
	if _, missing := views.MissingBasis(providers); missing {
		return // no observation on a basis some provider needs
	}
	var constraints []*allocation.ResourceConstraints
	for _, cp := range providers {
		usageByType, usageByNS := views.For(cp)
		constraint, err := cp.ComputeConstraints(ctx, usageByType, usageByNS)
		if err != nil {
			continue
		}
		constraints = append(constraints, constraint)
	}
	if len(constraints) == 0 {
		return
	}
	allocation.PublishNamespaceHeadroom(constraints, time.Now())
}

// publishWarmPoolSupply records what BRIDGES are contributing to each variant.
//
// Zero is published as readily as a positive figure, and for every variant the
// analyzer saw. A switching decision needs "this variant has no bridge" as much
// as it needs the number, and publishing only the non-zero ones would leave the
// last reading standing after the Pod went back -- saying a variant is being
// carried by a pool that has already reclaimed it.
func publishWarmPoolSupply(namespace string, nr allocation.NamedAnalyzerResult) {
	if nr.Result == nil {
		return
	}
	now := time.Now()
	for _, vc := range nr.Result.VariantCapacities {
		if vc.VariantName == "" {
			continue
		}
		decision.PublishWarmPoolSupply(namespace, vc.VariantName,
			vc.WarmPoolReplicas, vc.WarmPoolCapacity, now)
	}
}
