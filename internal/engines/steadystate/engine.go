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

package steadystate

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	accel "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	actuator "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/actuator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/locator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/external"
	saturation_v2 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation_v2"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/executor"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/variantmeta"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// analyzerEntry binds a registered analyzer to its name. The engine stores
// these in registration order so runAnalyzersAndScore iterates analyzers
// deterministically.
type analyzerEntry struct {
	name     string
	analyzer domain.Analyzer
}

type Engine struct {
	client   client.Client
	scheme   *runtime.Scheme
	executor executor.Executor

	// Recorder - use wrapper function recordEvent to limit number of events per va in an optimization cycle
	Recorder record.EventRecorder

	// vaEventTracker tracks whether a K8S event has been issued for a variant in an optimization cycle.
	// Key is namespace/name from utils.GetNamespacedKey.
	vaEventTracker map[string]bool

	// policies reports which scaling policy tier each model resolved to, and the
	// two ways that resolution goes wrong silently — an unknown tier name, and one
	// model's variants naming different tiers. Change-throttled; see policyReporter.
	policies *policyReporter

	Config *config.Config // Unified configuration (injected from main.go)

	// ReplicaMetricsCollector is the collector for replica metrics using the source infrastructure
	ReplicaMetricsCollector *collector.ReplicaMetricsCollector

	// ScaleToZeroEnforcer applies scale-to-zero and minimum replica enforcement
	ScaleToZeroEnforcer *allocation.Enforcer

	// GPULimiter constrains scaling decisions based on available GPU resources.
	// Only applied when EnableLimiter is true in the saturation config. When a
	// limiterBuilder is set (production), it is rebuilt live at the top of each
	// optimize cycle whenever the effective limiter config changes.
	GPULimiter allocation.Limiter

	// limiterBuilder rebuilds GPULimiter from the current effective config. Set by
	// SetLimiterBuilder in production; nil in tests (which inject a static
	// GPULimiter). limiterSig is the signature of the config that built the current
	// GPULimiter, used to skip rebuilds when nothing changed. Both are read and
	// written only from the single optimize goroutine after StartOptimizeLoop, but
	// SetLimiterBuilder may run before it, so limiterMu guards them.
	limiterBuilder func() (allocation.Limiter, error)
	limiterSig     string
	limiterMu      sync.Mutex

	// metricsRegistry is used to access metrics sources for request count queries
	metricsRegistry *source.SourceRegistry
	// Variants is the set of workloads KEDA has called WVA about — discovery. Nil
	// falls back to the annotation-sourced listing.
	Variants *registry.Registry
	// VariantEnricher resolves each registered workload's scale target with an
	// uncached read. Refreshed at the top of every cycle; a no-op for entries
	// still inside their freshness window.
	VariantEnricher *registry.Enricher

	// saturationV2Analyzer is the V2 token-based saturation analyzer (initialized once).
	// Also pre-registered in analyzers under domain.SaturationAnalyzerName.
	// Typed as domain.Analyzer to allow injection in tests.
	saturationV2Analyzer domain.Analyzer

	// capacityStore is shared with the V2 analyzer for caching capacity knowledge.
	capacityStore *saturation_v2.CapacityKnowledgeStore

	// lastGoodAnalysis records, per model (keyed by utils.GetNamespacedKey(namespace,
	// modelID)) and analyzer name, the AnalyzedAt of the most recent informative
	// (non-error, capacity-bearing) result. Used to gate the scale-down veto: an
	// analyzer whose last good analysis for a model is absent or staler than
	// analyzerLivenessStaleCycles cycles does not participate in that model's vote.
	// Keyed per model (not just per analyzer name) because one Engine instance
	// serves all models — a global-by-name map would leak one model's freshness
	// into another's. In-memory only; reset on process restart / leader failover
	// (safe: non-live → no scale-down until refreshed).
	// Unguarded, like vaEventTracker: safe today because PollingExecutor runs
	// optimize cycles sequentially in one goroutine and models within a cycle
	// are processed serially. Parallelizing model processing would need to
	// synchronize this map — the top-level per-model insert would race first.
	lastGoodAnalysis map[string]map[string]time.Time

	// lastAnalyzerSeries records, per model (keyed identically to
	// lastGoodAnalysis), the wva_analyzer_demand / wva_analyzer_target series
	// emitted on the previous cycle. Prometheus gauges have no way to enumerate
	// their own children, so the engine tracks what it published in order to
	// evict series it stops publishing. Absence is meaningful for these metrics
	// — a role that disappears or an analyzer that stops running must drop its
	// series rather than freeze at its last value — see pruneAnalyzerSeries.
	// Unguarded for the same reason as lastGoodAnalysis: optimize cycles run
	// sequentially in one goroutine and models are processed serially.
	lastAnalyzerSeries map[string]analyzerSeries

	// lastBlockedModels records, keyed identically, every model this engine has
	// published wva_model_scaling_blocked reasons for. Same reason as
	// lastAnalyzerSeries — a GaugeVec cannot enumerate its own children — but the
	// consequence is sharper: a reason series is PRESENT only while it holds, so
	// a model that goes away leaves a series asserting that a workload which no
	// longer exists will never park, and it alerts forever. Nothing else can clear
	// it, because the producer that would clear it never runs for that model
	// again. See pruneBlockedModels.
	lastBlockedModels map[string]blockedModelRef

	// analyzers is the engine's analyzer registry, mutated only during setup
	// (NewEngine + RegisterAnalyzer). After StartOptimizeLoop it is frozen —
	// further RegisterAnalyzer calls return an error. The optimize goroutine reads
	// analyzersSnapshot, never analyzers, so iteration is race-free without
	// runtime locking.
	analyzers []analyzerEntry

	// analyzersSnapshot is the frozen, registration-ordered view that
	// runAnalyzersAndScore iterates. Built from analyzers in StartOptimizeLoop
	// before the goroutine launches. Saturation always runs and drives scaling
	// decisions; other registered analyzers are invoked but their results are
	// not consumed yet — combine and per-analyzer threshold logic lands in
	// follow-up PRs.
	analyzersSnapshot []analyzerEntry

	// started transitions to true in StartOptimizeLoop. Late RegisterAnalyzer
	// calls return an error so the contract "register before Start" is enforced
	// rather than just documented.
	started bool

	// externalMu guards externalAnalyzers, which — unlike the frozen built-in
	// registry above — may change while the optimize goroutine runs.
	externalMu sync.RWMutex
	// externalAnalyzers holds config-driven external analyzers keyed by name.
	// It is mutated at runtime (UpsertExternalAnalyzer/RemoveExternalAnalyzer)
	// when the analyzer catalog changes, and read under RLock each cycle. This is
	// the "analyzers can be added at run-time" path; the built-in snapshot stays
	// frozen and lock-free.
	externalAnalyzers map[string]domain.Analyzer

	// optimizer is the V2 scaling optimizer that produces VariantDecisions from
	// AnalyzerResults. Selected per-cycle from the declared limiters:
	// CostAwareOptimizer when none is declared, GreedyByScoreOptimizer when one is.
	optimizer allocation.ScalingOptimizer

	metricsEmitter *metrics.MetricsEmitter
}

// NewEngine creates a new instance of the saturation engine.
// Config must be non-nil (validated in main.go before engine creation).
// gpuLimiter is the operator-selected limiter built by
// allocation.NewLimiterFromConfig in main.go and must be non-nil — tests
// that do not exercise the limiter path can pass allocation.NewNoOpLimiter.
// Panics if cfg or gpuLimiter is nil to fail fast on programming errors.
func NewEngine(client client.Client, apiReader client.Reader, scheme *runtime.Scheme, recorder record.EventRecorder, metricsRegistry *source.SourceRegistry, cfg *config.Config, gpuLimiter allocation.Limiter) *Engine {
	if cfg == nil {
		panic("config is nil in NewEngine - this should not happen (validated in main.go before engine creation)")
	}
	if gpuLimiter == nil {
		panic("gpuLimiter is nil in NewEngine - production callers must use allocation.NewLimiterFromConfig; tests should pass allocation.NewNoOpLimiter")
	}
	promSource := metricsRegistry.Get("prometheus") // assume prometheus source is registered

	// Create request count function wrapper for scale-to-zero enforcer
	requestCountFunc := func(ctx context.Context, engine inferenceengine.Engine, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
		return registration.CollectModelRequestCountForEngine(ctx, promSource, engine, modelID, namespace, retentionPeriod)
	}

	capacityStore := saturation_v2.NewCapacityKnowledgeStore()
	satV2 := saturation_v2.NewSaturationAnalyzer(capacityStore)

	// Initialize with default optimizer. The actual optimizer is selected
	// per-cycle in optimize() from the ConfigMap's live limiters: list, since
	// config arrives after engine init.
	var scalingOptimizer allocation.ScalingOptimizer = allocation.NewCostAwareOptimizer()

	// Declared before the locator so the closure below can read the field that is
	// wired after construction — see locator.New on why it takes a func.
	var engine Engine

	podLocator, err := locator.New(apiReader, func() *registry.Registry { return engine.Variants })
	if err != nil {
		// locator.New only fails when defaultCacheSize <= 0, which is a
		// programming error we cannot recover from at runtime.
		panic(fmt.Sprintf("locator.New: %v", err))
	}

	engine = Engine{
		client:                  client,
		scheme:                  scheme,
		Recorder:                recorder,
		Config:                  cfg,
		ReplicaMetricsCollector: collector.NewReplicaMetricsCollector(promSource, client, recorder, podLocator),
		ScaleToZeroEnforcer:     allocation.NewEnforcer(requestCountFunc, cfg),
		GPULimiter:              gpuLimiter,
		policies:                newPolicyReporter(),
		metricsRegistry:         metricsRegistry,
		saturationV2Analyzer:    satV2,
		capacityStore:           capacityStore,
		lastGoodAnalysis:        make(map[string]map[string]time.Time),
		lastAnalyzerSeries:      make(map[string]analyzerSeries),
		lastBlockedModels:       make(map[string]blockedModelRef),
		optimizer:               scalingOptimizer,
		metricsEmitter:          metrics.NewMetricsEmitter(),
		analyzers: []analyzerEntry{
			{name: domain.SaturationAnalyzerName, analyzer: satV2},
		},
		externalAnalyzers: make(map[string]domain.Analyzer),
	}

	engine.executor = executor.NewPollingExecutor(executor.PollingConfig{
		Config: executor.Config{
			OptimizeFunc: engine.optimize,
		},
		Interval:     cfg.OptimizationInterval(),
		RetryBackoff: 100 * time.Millisecond,
	})

	// Register saturation queries in the metrics registry: the base queries
	// (kv_cache_usage, queue_length) plus the token-based analyzer's own
	// (cache_config_info, avg_output_tokens, etc.).
	registration.RegisterSaturationQueries(metricsRegistry)

	// Register scale-to-zero queries in the metrics registry
	registration.RegisterScaleToZeroQueries(metricsRegistry)

	// Register queueing model queries (scheduler dispatch rate per endpoint).
	// These are collected alongside saturation metrics into the shared
	// ReplicaMetrics struct and used by the queueing model analyzer to
	// estimate per-replica arrival rate and model queue behavior.
	registration.RegisterQueueingModelQueries(metricsRegistry)

	return &engine
}

// RegisterAnalyzer adds an external analyzer to the engine's analyzer
// registry. Returns an error if called after StartOptimizeLoop or if name
// is already registered — callers must check the error. The analyzer is
// appended in registration order.

// addWarmPoolGPUs charges each namespace's warm pools into the managed usage
// about to be published.
//
// Namespaces are ADDED, not merely updated: a namespace whose only WVA
// consumption is a pool has no scaling requests, so it would otherwise be absent
// from the managed figure entirely and its quota would read as untouched while
// the pool holds GPUs inside it.
func addWarmPoolGPUs(byType map[string]int, byNamespace map[string]map[string]int) {
	for namespace, pools := range decision.WarmPoolGPUs() {
		perType, ok := byNamespace[namespace]
		if !ok {
			perType = make(map[string]int)
			byNamespace[namespace] = perType
		}
		for accelerator, gpus := range pools {
			if gpus <= 0 {
				continue
			}
			perType[accelerator] += gpus
			byType[accelerator] += gpus
		}
	}
}

func (e *Engine) RegisterAnalyzer(name string, a domain.Analyzer) error {
	if e.started {
		return errors.New("RegisterAnalyzer: called after StartOptimizeLoop")
	}
	for i := range e.analyzers {
		if e.analyzers[i].name == name {
			return fmt.Errorf("RegisterAnalyzer: duplicate analyzer name %q", name)
		}
	}
	e.analyzers = append(e.analyzers, analyzerEntry{name: name, analyzer: a})
	return nil
}

// UpsertExternalAnalyzer registers or replaces a config-driven external analyzer
// by name. Unlike RegisterAnalyzer it is safe to call while the optimize loop is
// running (the analyzer participates from the next cycle), so the analyzer
// catalog can change without a restart. A name that collides with a built-in
// analyzer is still stored here but never runs — the built-in wins in
// analyzerRunEntries.
func (e *Engine) UpsertExternalAnalyzer(name string, a domain.Analyzer) {
	e.externalMu.Lock()
	defer e.externalMu.Unlock()
	e.externalAnalyzers[name] = a
}

// RemoveExternalAnalyzer retires a runtime external analyzer. Idempotent.
func (e *Engine) RemoveExternalAnalyzer(name string) {
	e.externalMu.Lock()
	defer e.externalMu.Unlock()
	delete(e.externalAnalyzers, name)
}

// analyzerRunEntries returns the analyzers to run this cycle: the frozen built-in
// snapshot followed by the runtime external analyzers (sorted by name for a
// deterministic order). An external analyzer whose name collides with a built-in
// is skipped — the built-in registry wins name resolution.
func (e *Engine) analyzerRunEntries() []analyzerEntry {
	builtinNames := make(map[string]struct{}, len(e.analyzersSnapshot))
	for _, en := range e.analyzersSnapshot {
		builtinNames[en.name] = struct{}{}
	}

	e.externalMu.RLock()
	defer e.externalMu.RUnlock()

	names := make([]string, 0, len(e.externalAnalyzers))
	for name := range e.externalAnalyzers {
		if _, dup := builtinNames[name]; dup {
			continue // built-in wins name resolution
		}
		names = append(names, name)
	}
	sort.Strings(names)

	entries := make([]analyzerEntry, 0, len(e.analyzersSnapshot)+len(names))
	entries = append(entries, e.analyzersSnapshot...)
	for _, name := range names {
		entries = append(entries, analyzerEntry{name: name, analyzer: e.externalAnalyzers[name]})
	}
	return entries
}

// reconcileExternalAnalyzers rebuilds the runtime external-analyzer registry from
// the current catalog (Config.ExternalAnalyzerCatalog, which the ConfigMap
// reconciler refreshes when wva-analyzers changes). Called once per optimize
// cycle, so a catalog edit takes effect without a restart. A catalog label that
// collides with a built-in analyzer is skipped (built-in wins name resolution); a
// definition that fails to build (bad threshold, empty query) is skipped and
// logged, never fatal.
func (e *Engine) reconcileExternalAnalyzers(ctx context.Context) {
	src := e.metricsRegistry.Get("prometheus")
	if src == nil {
		return
	}
	logger := ctrl.LoggerFrom(ctx)
	catalog := e.Config.ExternalAnalyzerCatalog()

	builtin := make(map[string]struct{}, len(e.analyzersSnapshot))
	for _, en := range e.analyzersSnapshot {
		builtin[en.name] = struct{}{}
	}

	desired := make(map[string]struct{}, len(catalog))
	for label, def := range catalog {
		if _, isBuiltin := builtin[label]; isBuiltin {
			logger.V(logging.DEBUG).Info("Skipping external analyzer: name collides with a built-in", "label", label)
			continue
		}
		d, err := toExternalDefinition(label, def)
		if err != nil {
			logger.Info("Skipping malformed external analyzer definition", "label", label, "error", err)
			continue
		}
		a, err := external.New(d, src)
		if err != nil {
			logger.Info("Skipping invalid external analyzer definition", "label", label, "error", err)
			continue
		}
		e.UpsertExternalAnalyzer(label, a)
		desired[label] = struct{}{}
	}

	// Retire external analyzers no longer present (or no longer valid) in the catalog.
	for _, name := range e.externalAnalyzerNames() {
		if _, ok := desired[name]; !ok {
			e.RemoveExternalAnalyzer(name)
		}
	}
}

// externalAnalyzerNames returns the names of the currently registered runtime
// external analyzers.
func (e *Engine) externalAnalyzerNames() []string {
	e.externalMu.RLock()
	defer e.externalMu.RUnlock()
	names := make([]string, 0, len(e.externalAnalyzers))
	for name := range e.externalAnalyzers {
		names = append(names, name)
	}
	return names
}

// toExternalDefinition converts a catalog entry to an external.Definition,
// parsing the KEDA-style string thresholds to float64. When the entry has no
// engines map it is treated as a single engine-agnostic body.
func toExternalDefinition(label string, def config.ExternalAnalyzerDef) (external.Definition, error) {
	bodies := make(map[string]external.Body)
	if len(def.Engines) > 0 {
		for engine, body := range def.Engines {
			th, err := strconv.ParseFloat(body.Threshold, 64)
			if err != nil {
				return external.Definition{}, fmt.Errorf("engine %q: threshold %q: %w", engine, body.Threshold, err)
			}
			bodies[engine] = external.Body{Query: body.Query, Threshold: th}
		}
	} else {
		th, err := strconv.ParseFloat(def.Threshold, 64)
		if err != nil {
			return external.Definition{}, fmt.Errorf("threshold %q: %w", def.Threshold, err)
		}
		bodies[""] = external.Body{Query: def.Query, Threshold: th}
	}
	return external.Definition{Label: label, Bodies: bodies}, nil
}

// StartOptimizeLoop starts the optimization loop for the saturation engine.
// It runs until the context is cancelled.
//
// Before launching the goroutine, the registered analyzers are snapshotted
// to a frozen slice that runAnalyzersAndScore iterates. The started flag is
// flipped so subsequent RegisterAnalyzer calls return an error. The snapshot is the
// natural place to invoke any future per-analyzer Init(ctx) hook.
func (e *Engine) StartOptimizeLoop(ctx context.Context) {
	e.analyzersSnapshot = make([]analyzerEntry, len(e.analyzers))
	copy(e.analyzersSnapshot, e.analyzers)
	e.started = true

	e.recordActiveOptimizer() // record active optimizer
	metrics.SetConfigOptimizationInterval(float64(e.Config.OptimizationInterval().Seconds()))
	e.executor.Start(ctx)
}

func (e *Engine) recordActiveOptimizer() {
	// Record metrics for which optimizer is active
	optimizerNames := []string{"greedy-by-score", "cost-aware"}
	for _, name := range optimizerNames {
		isActive := false // default is false
		if name == e.optimizer.Name() {
			isActive = true // only one active at a time
		}
		e.metricsEmitter.RecordOptimizerActiveMetric(name, isActive)
	}
}

func (e *Engine) recordDefaultConfigMetrics() {
	metrics.SetConfigOptimizationInterval(float64(e.Config.OptimizationInterval().Seconds()))

	globalSatCfgMap := e.Config.ScalingPolicyConfig()
	// record global default config
	//
	// The limiter_enabled label keeps its name and its meaning — "is scaling being
	// limited" — but now comes from whether any limiter is declared, since the
	// enableLimiter flag it used to read is gone. Renaming the label would break
	// the operational dashboard and the zero-allocatable-GPU alert, which both
	// reference it.
	if cfg, ok := globalSatCfgMap["default"]; ok {
		limiterEnabled := e.Config.EffectiveLimiterMode() != config.LimiterTypeNone
		metrics.SetConfigInfo(cfg.GetAnalyzerName(), limiterEnabled, e.Config.ScaleToZeroEnabled())
	}
}

// SetLimiterBuilder installs a builder that rebuilds the GPU limiter from the
// current effective config. The engine calls it at the top of each optimize
// cycle and swaps the limiter when the effective limiter mode / quota entries
// change — so switching the ConfigMap's limiters: list takes effect without a
// restart. Call before StartOptimizeLoop. Passing nil disables live rebuilds
// (the injected GPULimiter is used as-is, e.g. in tests).
func (e *Engine) SetLimiterBuilder(builder func() (allocation.Limiter, error)) {
	e.limiterMu.Lock()
	defer e.limiterMu.Unlock()
	e.limiterBuilder = builder
	// Seed the signature from the current config so the first cycle does not
	// rebuild the limiter that was already constructed from the same config.
	e.limiterSig = limiterSignature(e.Config)
}

// refreshLimiter rebuilds GPULimiter when the effective limiter config changed
// since it was last built. A build error is logged and the previous limiter is
// kept, so a transient bad config never leaves the engine without a limiter.
func (e *Engine) refreshLimiter(ctx context.Context) {
	e.limiterMu.Lock()
	defer e.limiterMu.Unlock()
	if e.limiterBuilder == nil {
		return
	}
	sig := limiterSignature(e.Config)
	if sig == e.limiterSig && e.GPULimiter != nil {
		return
	}
	limiter, err := e.limiterBuilder()
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "failed to rebuild GPU limiter from config; keeping the previous limiter",
			"effectiveMode", e.Config.EffectiveLimiterMode())
		return
	}
	e.GPULimiter = limiter
	e.limiterSig = sig
	ctrl.LoggerFrom(ctx).Info("GPU limiter (re)built from config",
		"type", e.Config.EffectiveLimiterMode(), "name", limiter.Name())
}

// limiterSignature detects when the limiter config changed. Delegates to the
// shared implementation so this engine and the scale-from-zero engine cannot
// disagree about what a change is.
func limiterSignature(cfg *config.Config) string {
	return allocation.LimiterSignature(cfg)
}

// currentGPULimiter returns the active GPU limiter, read under limiterMu so it is
// safe against a concurrent live rebuild in refreshLimiter (the field is written
// only there, also under the lock).
func (e *Engine) currentGPULimiter() allocation.Limiter {
	e.limiterMu.Lock()
	defer e.limiterMu.Unlock()
	return e.GPULimiter
}

// optimize performs the optimization logic.
func (e *Engine) optimize(ctx context.Context) (retErr error) {
	start := time.Now()
	var modelsProcessed int
	defer func() {
		status := "success"
		if retErr != nil {
			status = "error"
		}
		metrics.ObserveOptimizationDuration(time.Since(start).Seconds(), status)
		metrics.SetModelsProcessed(modelsProcessed)
	}()

	logger := ctrl.LoggerFrom(ctx)
	e.refreshLimiter(ctx)             // rebuild the GPU limiter if the ConfigMap changed its type/entries
	e.reconcileExternalAnalyzers(ctx) // sync the runtime external-analyzer registry with the catalog
	e.recordDefaultConfigMetrics()    // record as soon as possible to reflect any changes in configuration

	if e.Config.ScaleToZeroEnabled() {
		logger.Info("Scaling to zero is enabled")
	}

	e.VariantEnricher.Refresh(ctx) // resolve the scale target of anything newly registered

	activeVAs, _, err := utils.ActiveVariantAutoscaling(ctx, e.client, e.Variants)
	if err != nil {
		logger.Error(err, "Unable to get active variant autoscalings")
		return err
	}

	if len(activeVAs) == 0 {
		logger.Info("No active VariantAutoscalings found, skipping optimization")
		// This cycle analyzes nothing, so no analyzer series gets refreshed and
		// the per-model prune below is never reached. Absence is meaningful for
		// those series: left alone they would hold their last busy-cycle values
		// indefinitely — a scale-to-zero fleet idling overnight would keep
		// reporting the demand it saw at peak, with no other series to
		// contradict it. This is a genuine empty state, not a transient one: a
		// failed list returns above, so reaching here means there really are no
		// active models.
		e.evictAllAnalyzerSeries()
		// Same argument, sharper consequence: a blocked-reason series asserts that
		// a named model will never park. With no models left, every such assertion
		// is about a workload that is gone.
		e.evictAllBlockedModels()

		// The PHYSICAL picture is not published from here — internal/gpuusage
		// discovers it independently, precisely so a parked fleet still has one.
		//
		// The MANAGED picture is, and it is published as ZERO. Unlike the physical
		// figure, "WVA's variants hold no GPUs" is a statement this path can make
		// with confidence: the list above succeeded, and every variant it returned
		// is at zero replicas. It is also the statement that has to be made. A
		// quota is charged only for what WVA holds, so a fleet parked at zero has
		// spent none of its allowance — and withholding that would leave the
		// scale-from-zero engine unable to establish it from anywhere else, refusing
		// every wake under a quota limiter for want of a figure only a running fleet
		// could produce. That is a deadlock: nothing running, so nothing published,
		// so nothing may start.
		//
		// The per-namespace view is empty here, not zeroed per namespace: there is
		// no population to enumerate namespaces from. Consumers materialise the
		// namespace they are deciding about (see scalefromzero gpuConstraints), which
		// is what keeps a namespace-scoped quota applying to a parked namespace.
		decision.PublishManagedGPUUsage(map[string]int{}, map[string]map[string]int{})
		// And what is LEFT of each allowance, which nothing else here would say.
		//
		// Headroom is otherwise published from inside the optimizer, so a
		// namespace with no models never gets an answer -- and the warm pool,
		// which is the only reader, treats no answer as unbounded and grows past
		// the quota it is charged against. A namespace holding only a pool is
		// exactly that case, and it is not a corner: a shared pool namespace has
		// no models by construction.
		e.publishHeadroomForIdleFleet(ctx)
		return nil
	}

	// Initialize vaEventTracker for this optimize cycle
	e.vaEventTracker = make(map[string]bool)

	// Open the collector's cycle so the models grouped below share one execution
	// of each namespace-scoped query instead of one per model.
	if e.ReplicaMetricsCollector != nil {
		e.ReplicaMetricsCollector.BeginCycle()
		defer e.ReplicaMetricsCollector.EndCycle()
	}

	// Collect accelerator inventory (only in limited mode AND only when the
	// inventory-based limiter is active). In quota mode (an inline limiters: quota
	// entry), the controller deliberately runs without consulting physical capacity —
	// listing Nodes here would defeat that contract (and trigger a
	// controller-runtime Node informer for the lifetime of the process). The
	// collected inventory is currently only logged anyway (see comment at
	// internal/collector/collector.go), so skipping it in quota mode loses
	// nothing of operational value.
	if e.Config.LimitedModeEnabled() && shouldCollectClusterInventory(e.Config) {
		inventory, err := collector.CollectInventoryK8S(ctx, e.client)
		if err != nil {
			logger.Error(err, "Failed to collect cluster inventory")
			// do not proceed to optimization if inventory collection fails in limited mode
			return err
		}
		// always print inventory until optimizer consumes it
		logger.Info("Collected cluster accelerator inventory (Limited Mode)", "inventory", inventory)
	}

	// Group VAs by model for per-model capacity analysis
	modelGroups := utils.GroupVariantAutoscalingByModel(activeVAs)
	modelsProcessed = len(modelGroups)
	logger.Info("Grouped VAs by model",
		"modelCount", len(modelGroups),
		"totalVAs", len(activeVAs))

	// Create VA lookup map for applySaturationDecisions (used to access VA status and update decisions)
	// Use namespace/vaName as key to avoid collisions when multiple namespaces have same VA name
	// Use slice index directly to avoid pointer-to-loop-variable bug
	vaMap := make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling, len(activeVAs))
	for i := range activeVAs {
		vaMap[utils.GetNamespacedKey(activeVAs[i].Namespace, activeVAs[i].Name)] = &activeVAs[i]
	}

	// Create map to store current allocations populated during metrics collection
	// Keyed by VariantAutoscaling Namespace/Name
	currentAllocations := make(map[string]*domain.Allocation)

	// Read saturation config for analyzer selection.
	globalSatCfgMap := e.Config.ScalingPolicyConfig()
	analyzerName := ""
	if cfg, ok := globalSatCfgMap["default"]; ok {
		cfg.ApplyDefaults()
		analyzerName = cfg.GetAnalyzerName()
	}

	// Select the optimizer from the declared limiters (both are stateless, safe to
	// swap). A declared limiter IS the request to be limited — the separate
	// enableLimiter flag is gone, so a quota can no longer be declared and then
	// silently not enforced, which is what that flag allowed. V2
	// (saturation-token-based) is the sole analysis path and always uses the
	// optimizer pipeline.
	limiterMode := e.Config.EffectiveLimiterMode()
	savedOptimizer := e.optimizer
	if limiterMode == config.LimiterTypeNone {
		e.optimizer = allocation.NewCostAwareOptimizer()
	} else {
		e.optimizer = allocation.NewGreedyByScoreOptimizer()
	}
	if savedOptimizer != e.optimizer {
		e.recordActiveOptimizer() // optimizer has changed, record active optimizer
	}
	logger.V(logging.DEBUG).Info("Optimizer selected", "analyzer", analyzerName,
		"optimizer", e.optimizer.Name(), "limiter", limiterMode)

	// V2 (saturation): saturation_v2.Analyzer → AnalyzerResult → Optimizer.Optimize → Enforcer bridge.
	mode := modeLabelForAnalyzer(analyzerName)
	allDecisions := e.optimizeV2(ctx, modelGroups, currentAllocations)

	// STEP 3: Apply decisions and update VA status
	// Always call applySaturationDecisions, even with empty decisions.
	// This function also updates VA.Status.CurrentAlloc with collected metrics
	// and emits HPA metrics, which must happen every reconciliation cycle.
	if len(allDecisions) > 0 {
		logger.Info("Applying scaling decisions",
			"totalDecisions", len(allDecisions))
	} else {
		logger.Info("No scaling decisions to apply, updating VA status with metrics")
	}
	e.applySaturationDecisions(ctx, allDecisions, vaMap, currentAllocations)

	logger.Info("Optimization completed successfully",
		"mode", mode,
		"modelsProcessed", len(modelGroups),
		"decisionsApplied", len(allDecisions))

	return nil
}

// modeLabelForAnalyzer returns the human-readable mode label for the given
// analyzer name, used in the "Optimization completed successfully" log entry.
// V2 is the sole analysis path; the label distinguishes a config that named the
// saturation analyzer from one that left it unset.
func modeLabelForAnalyzer(analyzerName string) string {
	switch analyzerName {
	case domain.SaturationAnalyzerName:
		return domain.SaturationAnalyzerName
	default:
		return "saturation-only"
	}
}

// recordEvent ensures only one event is recorded per VA in an optimization cycle.
// Exception: K8SEventResourceConstrained events bypass deduplication and can be
// recorded alongside other event types (e.g., ScaledUp + ResourceConstrained).
func (e *Engine) recordEvent(
	va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	eventType, reason, message string,
) {
	if e.Recorder == nil {
		return
	}

	target := llmdVariantAutoscalingV1alpha1.EventTarget(va)

	if reason == constants.K8SEventResourceConstrained {
		// This is the only exception where a variant can have 2 K8S events in an optimize cycle: K8SEventScaledUp & K8SEventResourceConstrained
		e.Recorder.Event(target, eventType, reason, message)
		return
	}

	key := utils.GetNamespacedKey(va.Namespace, va.Name)
	if e.vaEventTracker != nil {
		if _, ok := e.vaEventTracker[key]; ok { // ensures only one event is recorded per VA
			return
		}
	}
	e.Recorder.Event(target, eventType, reason, message)
	if e.vaEventTracker != nil {
		e.vaEventTracker[key] = true
	}
}

func (e *Engine) recordOptimizationFailedEvent(
	variantAutoscalings []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	message string,
) {
	if e.Recorder == nil {
		return
	}
	for _, va := range variantAutoscalings {
		e.recordEvent(&va, corev1.EventTypeWarning, constants.K8SEventOptimizationFailed, message)
	}
}

func (e *Engine) recordScalingEvent(
	va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	action domain.SaturationAction,
	targetReplicas int,
	reason string,
) {
	if e.Recorder == nil {
		return
	}
	switch action {
	case domain.ActionScaleUp:
		e.recordEvent(va, corev1.EventTypeNormal, constants.K8SEventScaledUp, reason)
	case domain.ActionScaleDown:
		if targetReplicas == 0 {
			e.recordEvent(va, corev1.EventTypeNormal, constants.K8SEventScaledToZero, reason)
		} else {
			e.recordEvent(va, corev1.EventTypeNormal, constants.K8SEventScaledDown, reason)
		}
	}
}

// resolveModelPolicy resolves the effective config for a model, layering in the
// policy tier its variants selected via trigger metadata.
//
// The policy is resolved for the MODEL, not per variant, because that is the unit
// WVA scales: the optimizer distributes the model's replicas across its variants
// against one set of thresholds. Variants that disagree are a configuration error,
// reported and resolved deterministically rather than silently half-applied.
func (e *Engine) resolveModelPolicy(
	ctx context.Context,
	configMap map[string]config.ScalingPolicy,
	modelID, namespace string,
	vas []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) config.ScalingPolicy {
	policy, conflicting := modelPolicy(vas)
	if len(conflicting) > 1 {
		e.policies.reportPolicyConflict(ctx, namespace, modelID, conflicting, policy)
	}

	// A named-but-absent tier resolves to the default entry, which is the right
	// outcome and the wrong silence — report it against the variants that asked.
	if policy != "" {
		if entry, ok := configMap[policy]; !ok || !config.PolicyEntryKey(policy, entry) {
			known := slices.Sorted(maps.Keys(config.NamedPolicies(configMap)))
			for i := range vas {
				if vas[i].Spec.ScalingPolicy == policy {
					e.policies.reportUnknownPolicy(ctx, namespace, vas[i].Name, policy, known)
				}
			}
		}
	}

	resolved := config.ResolveScalingPolicyForTier(configMap, modelID, namespace, policy)
	e.policies.reportEffectivePolicy(ctx, namespace, modelID, policy, resolved)
	return resolved
}

// modelPolicy returns the policy tier a model scales under, plus every distinct
// tier its variants named when they disagree.
//
// Ties break on the lexicographically smallest name so a conflict resolves the
// same way on every cycle and every replica of the controller: an allocation that
// flips between tiers as map order changes would be worse than either tier.
func modelPolicy(vas []llmdVariantAutoscalingV1alpha1.VariantAutoscaling) (string, []string) {
	var named []string
	for i := range vas {
		if p := vas[i].Spec.ScalingPolicy; p != "" && !slices.Contains(named, p) {
			named = append(named, p)
		}
	}
	if len(named) == 0 {
		return "", nil
	}
	slices.Sort(named)
	return named[0], named
}

// selectV2Optimizer chooses the optimizer and GPU constraints for a V2
// optimization cycle.
//
// When the configured optimizer is GreedyByScore it is GPU-aware and expects
// per-type resource constraints. Those constraints are computed from the GPU
// limiter's inventory. GreedyByScore interprets absent constraints as zero
// available capacity (deny-all), NOT as unlimited — so if it were run with
// empty constraints it would silently suppress all scale-up.
//
// Therefore, whenever real constraints cannot be obtained — the limiter does
// not provide constraints (no ConstraintProvider), no usage has been observed on
// a basis some provider needs, or computing them failed because, e.g., node
// objects are not readable on this cluster — we fall back to the cost-aware
// optimizer, which is the engine's unlimited path (the same optimizer used when
// no limiter is declared), so scale-up proceeds unconstrained instead of being
// blocked. A constraint that is present but reports zero GPUs is left intact:
// that is a genuine "no capacity" signal and should still block scale-up.
//
// Providers do not all consume the same usage figure. A physical inventory is fed
// every GPU held on the cluster's nodes; a quota is fed only what WVA's own
// variants hold, because an allowance granted to WVA may only be spent by WVA.
// See allocation.UsageBasis and gpuUsageViews.
func (e *Engine) selectV2Optimizer(
	ctx context.Context,
	requests []allocation.ModelScalingRequest,
) (allocation.ScalingOptimizer, []*allocation.ResourceConstraints) {
	logger := ctrl.LoggerFrom(ctx)

	// GreedyByScore is currently the only GPU-aware optimizer; any future
	// constraint-consuming optimizer must be added to this guard.
	optimizer := e.optimizer
	if _, ok := optimizer.(*allocation.GreedyByScoreOptimizer); !ok {
		return optimizer, nil
	}

	// Collect constraints from every provider backing the GPU limiter: a single
	// DefaultLimiter, or each constituent of a CompositeLimiter (so multi-entry
	// quota configs are all consulted, and namespace-scoped providers contribute
	// per-namespace caps via NamespacePools).
	providers := gpuConstraintProviders(e.currentGPULimiter())
	if len(providers) == 0 {
		return allocation.NewCostAwareOptimizer(), nil
	}

	// Each provider is fed the measure of usage IT asked for: physical inventories
	// the GPUs actually held on the nodes, quotas only what WVA's own variants
	// hold. One figure broadcast to both answers the wrong question for one of
	// them — see allocation.UsageBasis.
	views := gpuUsageViews(requests)
	if basis, missing := views.MissingBasis(providers); missing {
		// No observation on a basis some provider needs. Constraints built on an
		// absent usage figure would not be "unknown", they would be a confident
		// claim that nothing is running — and GreedyByScore would allocate the
		// whole cluster against it. Fall back to the unlimited optimizer, the same
		// posture this function already takes when no provider can supply
		// constraints.
		logger.V(logging.DEBUG).Info("No GPU usage observation yet on a basis some provider needs; "+
			"using the unlimited optimizer for this cycle", "basis", basis.String())
		return allocation.NewCostAwareOptimizer(), nil
	}

	var constraints []*allocation.ResourceConstraints
	for _, cp := range providers {
		usageByType, usageByNS := views.For(cp)
		constraint, err := cp.ComputeConstraints(ctx, usageByType, usageByNS)
		if err != nil {
			logger.Error(err, "Failed to compute GPU constraints, skipping provider", "provider", cp.Name())
			continue
		}
		constraints = append(constraints, constraint)
	}

	// GreedyByScore treats absent constraints as zero available capacity
	// (deny-all), not as unlimited. When no provider could supply constraints
	// (limiter is not constraint-backed, or every provider failed — e.g. GPU
	// capacity cannot be discovered), fall back to the cost-aware optimizer, the
	// engine's unlimited path, so scale-up proceeds instead of being silently
	// blocked.
	if len(constraints) == 0 {
		return allocation.NewCostAwareOptimizer(), nil
	}
	return optimizer, constraints
}

// resolveRescaleFlags builds the scope-coupled rescale enablement for this cycle:
// the cluster flag from the global saturation `default` config, plus a per-namespace
// flag from each active namespace's OWN `default` config (never the global fallback,
// so the cluster flag cannot enable rescale on a namespace quota).
func (e *Engine) resolveRescaleFlags(requests []allocation.ModelScalingRequest) allocation.RescaleFlags {
	flags := allocation.RescaleFlags{Cluster: e.Config.RescaleEnabledCluster()}
	seen := make(map[string]bool)
	for _, req := range requests {
		ns := req.Namespace
		if ns == "" || seen[ns] {
			continue
		}
		seen[ns] = true
		if enabled, hasLocal := e.Config.RescaleEnabledForNamespaceLocal(ns); hasLocal && enabled {
			if flags.ByNamespace == nil {
				flags.ByNamespace = make(map[string]bool)
			}
			flags.ByNamespace[ns] = true
		}
	}
	return flags
}

// optimizeV2 runs the V2 token-based optimizer path (saturation-token-based).
// Collects AnalyzerResults for all models, calls the optimizer once, then applies enforcer per-model.
func (e *Engine) optimizeV2(
	ctx context.Context,
	modelGroups map[string][]llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	currentAllocations map[string]*domain.Allocation,
) []domain.VariantDecision {
	logger := ctrl.LoggerFrom(ctx)

	// Prune liveness state for departed models. Any model still present in
	// modelGroups is active this cycle (config-not-loaded or transient-failure
	// models are enumerated but skipped below — they have not departed); keys
	// absent from modelGroups belong to models whose VariantAutoscalings are
	// gone, so their lastGoodAnalysis entries are evicted here. Keyed identically
	// to updateLivenessAndSetLive (utils.GetNamespacedKey(namespace, modelID)).
	activeKeys := make(map[string]bool, len(modelGroups))
	for _, modelVAs := range modelGroups {
		activeKeys[utils.GetNamespacedKey(modelVAs[0].Namespace, modelVAs[0].Spec.ModelID)] = true
	}
	e.pruneLastGoodAnalysis(activeKeys)
	e.pruneAnalyzerSeries(activeKeys)
	e.pruneBlockedModels(activeKeys)

	// Stage 1: Collect ModelScalingRequests for all models
	requests := make([]allocation.ModelScalingRequest, 0, len(modelGroups))
	// modelReplicaMetrics collects per-model replica metrics for KV token enrichment
	modelReplicaMetrics := make(map[string][]domain.ReplicaMetrics)
	// modelScaleTargets carries each model's scale targets into stage 3, where
	// applyScaleToZeroEnforcement needs them to gate the enforcer. Captured here
	// because data.scaleTargets is only in scope during this collection loop.
	modelScaleTargets := make(map[string]map[string]scaletarget.ScaleTargetAccessor)

	for groupKey, modelVAs := range modelGroups {
		modelID := modelVAs[0].Spec.ModelID
		namespace := modelVAs[0].Namespace
		logger.Info("Processing model (V2)",
			"modelID", modelID,
			"namespace", namespace,
			"variantCount", len(modelVAs),
			"groupKey", groupKey)

		// Get namespace-aware saturation config
		saturationConfigMap := e.Config.ScalingPolicyConfigForNamespace(namespace)
		if len(saturationConfigMap) == 0 {
			logger.Info("Saturation scaling config not loaded yet for namespace, skipping model",
				"namespace", namespace, "modelID", modelID)
			continue
		}

		scalingPolicyConfig := e.resolveModelPolicy(ctx, saturationConfigMap, modelID, namespace, modelVAs)
		data, err := e.prepareModelData(ctx, modelID, modelVAs, e.client)
		if err != nil {
			msg := "Model data preparation failed"
			logger.Error(err, msg, "modelID", modelID)
			e.recordOptimizationFailedEvent(modelVAs, msg)
			e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations, nil)
			continue
		}
		if data == nil {
			logger.V(logging.DEBUG).Info("Skipping model: no metrics available", "modelID", modelID)
			continue
		}

		req, err := e.collectV2ModelRequest(ctx, modelID, namespace,
			data.replicaMetrics, scalingPolicyConfig, data.variantStates, data.variantMetadata,
			data.scaleTargets, data.variantAutoscalings, data.schedulerQueue, data.arrivalRate)
		if err != nil {
			msg := "V2 analysis failed"
			logger.Error(err, msg, "modelID", modelID)
			e.recordOptimizationFailedEvent(modelVAs, msg)
			e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations, data.scaleTargets)
			continue
		}

		requests = append(requests, *req)
		modelReplicaMetrics[modelID] = data.replicaMetrics
		modelScaleTargets[utils.GetNamespacedKey(namespace, modelID)] = data.scaleTargets
	}

	if len(requests) == 0 {
		return nil
	}

	// The PHYSICAL usage snapshot is NOT published from here any more. It is
	// discovered independently by internal/gpuusage, which is the sole producer, so
	// this path no longer decides what the rest of WVA believes about the cluster —
	// a cycle where every model failed collection can no longer withhold the
	// picture, and a fleet parked at zero (which returns long before this) no longer
	// leaves scale-from-zero with no picture at all.
	//
	// The MANAGED view is published from here, because this population is the only
	// place it exists: it is what WVA's own variants hold, which is the sole
	// consumption an operator-declared quota may be charged for (see
	// decision.DefaultManagedGPUUsage). It is shared for the scale-from-zero
	// engine's benefit — that engine has no population of its own to sum, and a
	// wake still has to be judged against the allowance.
	//
	// Publishing only past the len(requests) == 0 guard above is deliberate. An
	// empty population there means collection FAILED, not that nothing is running,
	// and publishing zeros for it would hand every quota its full allowance back at
	// exactly the moment WVA cannot see what it is holding. The genuinely-empty
	// case is published separately, from the path that can tell the difference.
	managedByType := computeCurrentGPUUsage(requests)
	managedByNamespace := computeCurrentGPUUsageByNamespace(requests)
	// WVA's own warm pools are charged here too. A pool Pod is not a variant, so
	// it is absent from `requests` -- but a quota is an allowance granted to WVA,
	// and a pool exists because WVA asked for it and is sized by what WVA
	// publishes. Left out, a namespace with a 4-GPU quota and a 3-GPU pool placed
	// four more replicas and consumed seven.
	//
	// Folded in HERE rather than published separately, because this store has one
	// producer on purpose: two components summing "what WVA holds" their own way
	// is how the optimizer and the wake path come to disagree about the same
	// cluster.
	addWarmPoolGPUs(managedByType, managedByNamespace)
	decision.PublishManagedGPUUsage(managedByType, managedByNamespace)

	// Report unattributed GPUs from the MANAGED view, which is the only one that
	// can have any: the physical picture attributes usage by the node a pod runs
	// on, so every GPU it counts lands under a resolved type, whereas a variant
	// whose own accelerator cannot be resolved is charged here to the "unknown"
	// placeholder and then to no pool at all. That silently under-states what WVA
	// holds, which is a quota reporting more allowance free than there is.
	//
	// It stays HERE rather than inside selectV2Optimizer, which returns early
	// unless the optimizer is GreedyByScore — reporting from behind that guard is
	// how it went silent on default deployments before.
	reportUnattributedGPUs(ctx, managedByType)

	// Stage 2: Compute GPU constraints and call optimizer
	optimizer, constraints := e.selectV2Optimizer(ctx, requests)
	// Scope-coupled rescale enablement (cluster + per-namespace) is resolved from
	// config and handed to the GPU-aware optimizer for this cycle.
	if g, ok := optimizer.(*allocation.GreedyByScoreOptimizer); ok {
		g.Rescale = e.resolveRescaleFlags(requests)
	}
	allDecisions := optimizer.Optimize(ctx, requests, constraints)
	logScalingDecisions(ctx, requests, allDecisions)

	logger.Info("V2 optimizer produced decisions",
		"optimizer", optimizer.Name(),
		"decisionCount", len(allDecisions),
		"modelCount", len(requests))

	// Stage 3: Apply enforcer per-model (directly on decisions)
	for _, req := range requests {
		e.applyScaleToZeroEnforcement(
			ctx, req.ModelID, req.Namespace, optimizer.Name(),
			allDecisions,
			modelScaleTargets[utils.GetNamespacedKey(req.Namespace, req.ModelID)],
			req.VariantStates,
		)
	}

	// Stage 4: Enrich decisions with KV cache token data from replicaMetrics.
	// Utilization, RequiredCapacity, and SpareCapacity are already set by
	// buildDecisionsWithOptimizer from AnalyzerResult.
	enrichDecisionsWithKvTokenData(allDecisions, modelReplicaMetrics)

	return allDecisions
}

// observeAccelerators reports whether this deployment should resolve an
// unconstrained variant's accelerator by reading the nodes its pods run on.
//
// Only a PHYSICAL limiter charges a variant to an accelerator pool, so only a
// physical limiter makes the answer matter. Without one, an unresolved accelerator
// is permissive — FitsGPUBudget asks whether any pool has room — and the node read
// would buy an attribution nothing consumes, at the cost of the only cluster-scoped
// permission WVA needs in the normal path.
//
// This is what lets a namespace-scoped install run with no cluster-scoped RBAC at
// all when no physical limiter is configured.
func observeAccelerators(cfg *config.Config) variantmeta.ObserveAccelerators {
	// ALWAYS FromNodes, for now, and the reasoning that said otherwise was wrong.
	//
	// The argument was: only a physical limiter charges a variant to an accelerator
	// pool, so with no limiter the observation buys nothing and the cluster-scoped
	// node read can be skipped. The budgeting half of that is true. The rest is not
	// — accelerator identity is also how the V2 capacity store keys learned
	// capacity (saturation_v2/capacity_store.go:171 matches on AcceleratorName
	// before reusing a record), so leaving it unresolved denies a variant its own
	// prior capacity knowledge and any compatible variant's.
	//
	// Gating it on the limiter made the e2e "per-model override" spec run a
	// workload to maxReplicas instead of settling at 1: with no capacity record the
	// demand-to-capacity ratio has no sane denominator. 53/0 before, 51/1 after.
	//
	// The reduction is still worth having — it is the difference between a
	// namespace-scoped install needing cluster-scoped RBAC and not — but it needs
	// the capacity store to key on something the workload declares, not on an
	// observation of where its pods landed. Until then, correctness wins.
	_ = cfg
	return variantmeta.FromNodes
}

// BuildVariantStates extracts current and desired replica counts from VAs for capacity analysis.
func (e *Engine) BuildVariantStates(
	ctx context.Context,
	vas []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	k8sClient client.Client,
) []domain.VariantReplicaState {
	// Variant identity + replica/GPU state is resolved by the discovery step,
	// the single producer of per-variant metadata. BuildVariantStates projects
	// that onto the legacy VariantReplicaState the analyzers consume today; the
	// identity fields discovery additionally resolves (Cost, AcceleratorName,
	// ModelID, ...) are read directly from VariantMetadata by the consumers that
	// need them.
	metas := variantmeta.Discover(ctx, vas, scaleTargets, k8sClient, observeAccelerators(e.Config))
	states := make([]domain.VariantReplicaState, 0, len(metas))
	for _, m := range metas {
		states = append(states, m.ToReplicaState())
	}
	return states
}

// getRoleFromScaleTarget extracts the P/D role from a scale target's pod template labels.
// Returns "prefill", "decode", or "both" (default when no role label is present).
// The resolution now lives in the discovery package (the owner of variant metadata);
// this wrapper is retained for in-package callers and tests.
func getRoleFromScaleTarget(scaleTarget scaletarget.ScaleTargetAccessor) string {
	return variantmeta.RoleFromScaleTarget(scaleTarget)
}

// enrichDecisionsWithKvTokenData sets KvCacheTokensUsed, KvCacheTokensCapacity, and
// RequiredCapacityUnit on decisions from replica metrics aggregated per (model, variant).
// Used by V2 path where Utilization and RequiredCapacity are already set from
// AnalyzerResult.
//
// Aggregation is keyed by (modelID, variantName) — not just variantName — because
// variant names can collide across different models in the same reconcile cycle.
func enrichDecisionsWithKvTokenData(decisions []domain.VariantDecision, modelReplicaMetrics map[string][]domain.ReplicaMetrics) {
	type kvAgg struct {
		kvUsed  int64
		kvTotal int64
	}
	type variantKey struct {
		modelID string
		variant string
	}
	agg := make(map[variantKey]*kvAgg)
	for modelID, metrics := range modelReplicaMetrics {
		for _, rm := range metrics {
			k := variantKey{modelID: modelID, variant: rm.VariantName}
			a, ok := agg[k]
			if !ok {
				a = &kvAgg{}
				agg[k] = a
			}
			a.kvUsed += rm.TokensInUse
			a.kvTotal += rm.TotalKvCapacityTokens
		}
	}

	for i := range decisions {
		d := &decisions[i]
		d.RequiredCapacityUnit = constants.UnitContinuous
		if a, ok := agg[variantKey{modelID: d.ModelID, variant: d.VariantName}]; ok {
			d.KvCacheTokensUsed = a.kvUsed
			d.KvCacheTokensCapacity = a.kvTotal
		}
	}
}

// hasMinReplicasAboveZero returns true if any variant in the states has MinReplicas > 0.
func hasMinReplicasAboveZero(states []domain.VariantReplicaState) bool {
	for _, state := range states {
		if state.MinReplicas != nil && *state.MinReplicas > 0 {
			return true
		}
	}
	return false
}

// soleEngineFor returns the one engine a model's scale targets run, and whether
// there is exactly one.
//
// Idleness is measured with a single counter, and the counter is engine-specific:
// vllm:request_success_total or sglang:num_requests_total. Either alone is fine —
// ask for the matching one. A model running BOTH would need both counters summed,
// and asking for one of them would see only part of its traffic and could park a
// model that is still serving through the other engine. That is a real
// (if unusual) topology, so it is refused rather than guessed at.
//
// Present() returns vLLM for an empty set, so a model with no resolvable targets
// yields a single engine and the absent-series guard downstream keeps it safe.
func soleEngineFor(scaleTargets map[string]scaletarget.ScaleTargetAccessor) (inferenceengine.Engine, bool) {
	engines := inferenceengine.Present(scaleTargets)
	if len(engines) != 1 {
		return inferenceengine.EngineVLLM, false
	}
	return engines[0], true
}

// applyScaleToZeroEnforcement runs scale-to-zero / minimum-replica enforcement for a
// single model's decisions, unless a safety gate skips it:
//   - the model runs MORE THAN ONE engine, so no single request counter measures
//     its idleness (see soleEngineFor); or
//   - any variant declares minReplicas > 0 (hasMinReplicasAboveZero).
//
// A single non-vLLM engine is now supported. It used to be refused outright,
// because the enforcer asked for vllm:request_success_total whatever the model
// ran, and an SGLang model has no such series — reading as idle and getting
// zeroed. The engine-specific query already existed
// (sglang:num_requests_total, registered beside the vLLM one); it simply was not
// reached. The engine is detected here and passed to the enforcer, which asks for
// the counter that matches.
//
// Decisions are mutated in place; returns true if the model was scaled to zero.
//
// Both optimize paths (saturation, queueing-model) funnel their enforcement through
// this one method so the gate lives in a single place — a caller cannot accidentally
// invoke the enforcer ungated, and one test (engine_scale_to_zero_enforce_test.go)
// locks the gate down for every path.
func (e *Engine) applyScaleToZeroEnforcement(
	ctx context.Context,
	modelID, namespace, optimizerName string,
	decisions []domain.VariantDecision,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantStates []domain.VariantReplicaState,
) bool {
	logger := ctrl.LoggerFrom(ctx)

	// The resolved scaling entry carries the whole scale-to-zero policy: enabled
	// and retention, tiered namespace-local → global and merged with this model's
	// override. Resolved before the gates below, not after, because the gates are
	// half of what decides whether this model can ever park and the policy is the
	// other half — reporting one without the other cannot say which is binding.
	satConfig := config.ResolveScalingPolicy(e.Config.ScalingPolicyConfigForNamespace(namespace), modelID, namespace)
	scaleToZeroEnabled := config.ResolveScaleToZeroEnabled(e.Config, &satConfig)
	modelEngine, engineSupported := soleEngineFor(scaleTargets)

	// Reported before the empty-decision return below, and unconditionally,
	// including with no reasons. This call is what CLEARS a reason that no longer
	// holds, so any path that skips it pins the last bad answer — and a cycle that
	// produced no decisions at all is precisely when a stale "will never park"
	// series is most misleading. The reasons themselves are configuration
	// reconciled against discovered bounds; they do not depend on there being a
	// decision to enforce.
	// Resolved before the reasons are published, not with the gate below, so the
	// hold appears on wva_model_scaling_blocked rather than only in a V(DEBUG)
	// line. A model held here is otherwise indistinguishable from one that will
	// never park.
	retention := config.ResolveScaleToZeroRetention(&satConfig)
	recentlyWoken := decision.WithinActivationRetention(namespace, modelID, retention)

	blockedReasons := scaleToZeroBlockReasons(scaleToZeroEnabled, engineSupported, recentlyWoken, variantStates)
	metrics.SetModelScalingBlockedReasons(namespace, modelID,
		constants.ScalingBlockedReasonsPolicy, blockedReasons)
	if e.recordBlockedModel(namespace, modelID, blockedReasons) {
		logBlockedTransition(ctx, namespace, modelID, blockedReasons)
	}

	if len(decisions) == 0 {
		return false
	}

	if !engineSupported {
		logger.V(logging.DEBUG).Info("Skipping scale-to-zero enforcement: model runs more than one inference engine, so no single request counter measures its idleness",
			"modelID", modelID, "optimizer", optimizerName,
			"engines", inferenceengine.Present(scaleTargets))
		return false
	}
	if hasMinReplicasAboveZero(variantStates) {
		logger.V(logging.DEBUG).Info("Skipping scale-to-zero enforcement: variant has minReplicas > 0",
			"modelID", modelID, "optimizer", optimizerName)
		return false
	}

	// A model just woken from zero has served nothing yet: the request that woke
	// it is still queued in the EPP while the pod pulls and loads. The enforcer's
	// idle signal is a request counter over the retention window, which reads
	// zero for precisely that model — so without this gate the wake is undone
	// before it can serve the request that asked for it. Hold the model for the
	// same retention period the operator already configures for idleness.
	if recentlyWoken {
		logger.V(logging.DEBUG).Info("Skipping scale-to-zero enforcement: model was recently woken from zero and is inside its retention period",
			"modelID", modelID, "namespace", namespace, "retention", retention, "optimizer", optimizerName)
		return false
	}

	scaledToZero := e.ScaleToZeroEnforcer.EnforcePolicyOnDecisions(
		ctx, modelID, namespace, decisions, &satConfig, optimizerName, modelEngine,
	)
	if scaledToZero {
		logger.Info("Scale-to-zero enforcement applied",
			"modelID", modelID, "optimizer", optimizerName)
	}
	return scaledToZero
}

// normalizeRole maps empty and "both" roles to the same canonical key so that
// non-disaggregated variants are grouped together regardless of whether the
// deployment carries a role label.
func normalizeRole(role string) string {
	if role == "" {
		return domain.RoleBoth
	}
	return role
}

// groupByRole sub-groups variant states by their P/D role.
// Returns a map keyed by normalized role ("both", "prefill", "decode").
// For non-disaggregated models (all "both"/""), the map contains a single entry.
func groupByRole(states []domain.VariantReplicaState) map[string][]domain.VariantReplicaState {
	groups := make(map[string][]domain.VariantReplicaState)
	for _, s := range states {
		key := normalizeRole(s.Role)
		groups[key] = append(groups[key], s)
	}
	return groups
}

// filterReplicaMetricsByVariants returns only the replica metrics whose VariantName
// appears in the given variant state slice. Used to split per-model metrics into
// per-role subsets without re-querying Prometheus.
func filterReplicaMetricsByVariants(metrics []domain.ReplicaMetrics, states []domain.VariantReplicaState) []domain.ReplicaMetrics {
	allowed := make(map[string]struct{}, len(states))
	for _, s := range states {
		allowed[s.VariantName] = struct{}{}
	}
	filtered := make([]domain.ReplicaMetrics, 0, len(metrics))
	for _, m := range metrics {
		if _, ok := allowed[m.VariantName]; ok {
			filtered = append(filtered, m)
		}
	}
	return filtered
}

// filterVAsByVariantStates returns the subset of VAs whose Name appears in the
// given variant states. Used to map a role group back to its source VAs for
// safety-net metric emission.
func filterVAsByVariantStates(
	vas []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	states []domain.VariantReplicaState,
) []llmdVariantAutoscalingV1alpha1.VariantAutoscaling {
	allowed := make(map[string]struct{}, len(states))
	for _, s := range states {
		allowed[s.VariantName] = struct{}{}
	}
	filtered := make([]llmdVariantAutoscalingV1alpha1.VariantAutoscaling, 0, len(states))
	for _, va := range vas {
		if _, ok := allowed[va.Name]; ok {
			filtered = append(filtered, va)
		}
	}
	return filtered
}

// variantNames returns variant names from states for logging.
func variantNames(states []domain.VariantReplicaState) []string {
	names := make([]string, len(states))
	for i, s := range states {
		names[i] = s.VariantName
	}
	return names
}

// modelData holds the pre-processed data for a model, shared across optimize paths.
type modelData struct {
	modelID             string
	namespace           string
	replicaMetrics      []domain.ReplicaMetrics
	scaleTargets        map[string]scaletarget.ScaleTargetAccessor
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling
	variantStates       []domain.VariantReplicaState
	variantMetadata     []domain.VariantMetadata
	schedulerQueue      *domain.SchedulerQueueMetrics
	arrivalRate         float64
}

// prepareModelData collects metrics and builds lookup maps for a model's VAs.
// This is shared by the saturation path and the Queueing Model Analyzer engine.
// Returns nil modelData (not error) when no metrics are available — caller should skip the model.
func (e *Engine) prepareModelData(
	ctx context.Context,
	modelID string,
	modelVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	k8sClient client.Client,
) (*modelData, error) {
	if len(modelVAs) == 0 {
		return nil, fmt.Errorf("no VAs provided for model %s", modelID)
	}

	logger := ctrl.LoggerFrom(ctx)
	namespace := modelVAs[0].Namespace

	scaleTargets := make(map[string]scaletarget.ScaleTargetAccessor)
	variantAutoscalings := make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)

	for i := range modelVAs {
		va := &modelVAs[i]
		scaleTarget, err := scaletarget.FetchScaleTarget(ctx, k8sClient, va.Name, va.Spec.ScaleTargetRef.Kind, va.GetScaleTargetName(), va.Namespace)
		if err != nil {
			logger.V(logging.DEBUG).Info("Could not get scale target for VA",
				"variant", va.Name,
				"scaleTarget", va.GetScaleTargetName(),
				"error", err)
			continue
		}

		key := utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())
		scaleTargets[key] = scaleTarget

		variantKey := utils.GetNamespacedKey(va.Namespace, va.Name)
		variantAutoscalings[variantKey] = va
	}

	logger.V(logging.DEBUG).Info("Using source infrastructure for replica metrics",
		"modelID", modelID,
		"namespace", namespace)
	replicaMetrics, err := e.ReplicaMetricsCollector.CollectReplicaMetrics(ctx, modelID, namespace, scaleTargets, variantAutoscalings, e.vaEventTracker)
	if err != nil {
		return nil, fmt.Errorf("failed to collect Saturation metrics for model %s: %w", modelID, err)
	}

	logger.V(logging.DEBUG).Info("Collected saturation metrics",
		"modelID", modelID,
		"namespace", namespace,
		"metricsCount", len(replicaMetrics))

	if len(replicaMetrics) == 0 {
		// Two very different situations reach this line, and they used to produce
		// the same message.
		//
		// A model scaled to zero with nothing queued is idle, and skipping is the
		// whole point. A model with requests waiting in the scheduler is serving
		// traffic through pods this controller cannot attribute — an FMA topology
		// whose launchers nothing scrapes, a PodMonitor naming a port the pods do
		// not declare, a workload whose ownerReferences reach no scale target. The
		// second is an emergency and was indistinguishable from the first.
		//
		// The scheduler queue answers it, and it can be asked here precisely
		// because it is model-level and comes from EPP: it does not depend on any
		// engine pod being scraped, which is the thing that has gone wrong.
		queued := 0
		if sq := e.ReplicaMetricsCollector.CollectSchedulerQueueMetrics(ctx, modelID); sq != nil {
			queued = int(sq.QueueSize)
		}
		metrics.SetUnmeasuredQueue(namespace, modelID, queued)

		if queued > 0 {
			logger.Info("Model is serving but no replica could be attributed: requests are queued and nothing will scale",
				"modelID", modelID,
				"namespace", namespace,
				"queuedRequests", queued,
				"hint", "check that the serving pods are scraped and that their ownerReferences reach a scale target; for FMA, that the launcher pods have a PodMonitor (see docs/deployment/operations.md, 'FMA launcher pods')")
		} else {
			logger.Info("No saturation metrics available for model, skipping analysis",
				"modelID", modelID,
				"namespace", namespace)
		}
		return nil, nil // nil modelData signals skip
	}
	// Measurable this cycle. Reset the gauge rather than leaving the last bad
	// value to expire on Prometheus' staleness clock.
	metrics.SetUnmeasuredQueue(namespace, modelID, 0)

	// Discover the authoritative per-variant metadata once, then project it onto
	// the VariantReplicaState the analyzers consume. The full metadata is also
	// threaded to the optimizer (via ModelScalingRequest.Variants) as the source
	// of truth for variant identity/cost/accelerator.
	variantMetadata := variantmeta.Discover(ctx, modelVAs, scaleTargets, k8sClient, observeAccelerators(e.Config))
	variantStates := make([]domain.VariantReplicaState, 0, len(variantMetadata))
	for _, m := range variantMetadata {
		variantStates = append(variantStates, m.ToReplicaState())
	}
	schedulerQueue := e.ReplicaMetricsCollector.CollectSchedulerQueueMetrics(ctx, modelID)
	arrivalRate := e.ReplicaMetricsCollector.CollectModelArrivalRate(ctx, modelID, namespace)

	return &modelData{
		modelID:             modelID,
		namespace:           namespace,
		replicaMetrics:      replicaMetrics,
		scaleTargets:        scaleTargets,
		variantAutoscalings: variantAutoscalings,
		variantStates:       variantStates,
		variantMetadata:     variantMetadata,
		schedulerQueue:      schedulerQueue,
		arrivalRate:         arrivalRate,
	}, nil
}

// applySaturationDecisions updates VA status and emits metrics based on Saturation decisions.
func (e *Engine) applySaturationDecisions(
	ctx context.Context,
	decisions []domain.VariantDecision,
	vaMap map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	currentAllocations map[string]*domain.Allocation,
) {
	logger := ctrl.LoggerFrom(ctx)
	// Create a map of decisions for O(1) lookup
	// Use namespace/variantName as key to match vaMap and avoid collisions
	decisionMap := make(map[string]domain.VariantDecision)
	for _, d := range decisions {
		decisionMap[utils.GetNamespacedKey(d.Namespace, d.VariantName)] = d
	}

	// Iterate over ALL active VAs to ensure we update status and trigger reconciliation for everyone
	for vaName, va := range vaMap {
		decision, hasDecision := decisionMap[vaName]

		if hasDecision {
			logger.Info("Processing decision for VA",
				"variant", vaName,
				"action", decision.Action,
				"current", decision.CurrentReplicas,
				"target", decision.TargetReplicas)
		} else {
			logger.V(logging.DEBUG).Info("No scaling decision for VA, but updating status to trigger reconcile",
				"variant", vaName)
		}

		// Variants are synthesized in-memory from annotated HPAs/ScaledObjects;
		// there is no API-server object to fetch, so work on a copy.
		updateVa := *va.DeepCopy()

		// Update CurrentAlloc from local analysis (which has the latest metrics)
		// We use currentAllocations map instead of Status.CurrentAlloc
		if currentAlloc, ok := currentAllocations[vaName]; ok {
			// If we have a decision, attach current alloc to it for cache
			// If we have a decision, attach current alloc to it for cache
			// (Future logic if needed)
			_ = currentAlloc // Used for something?
			// Previously we updated va.Status.CurrentAlloc = currentAlloc
			// Now we just don't update status with it.
		}

		// Determine target replicas and accelerator
		var targetReplicas int
		var acceleratorName string
		var reason string

		if hasDecision {
			targetReplicas = decision.TargetReplicas
			acceleratorName = decision.AcceleratorName
			reason = decision.Reason()
		} else {
			// No change/decision: Keep current target or default to current replicas
			// We effectively explicitly "decide" to keep things as they are if no decision was made
			if updateVa.Status.DesiredOptimizedAlloc.NumReplicas != nil && *updateVa.Status.DesiredOptimizedAlloc.NumReplicas > 0 {
				targetReplicas = int(*updateVa.Status.DesiredOptimizedAlloc.NumReplicas)
			} else if curr, ok := currentAllocations[vaName]; ok {
				targetReplicas = curr.NumReplicas
			}
			// Keep existing accelerator or use current (skip sentinel values)
			if acc := updateVa.Status.DesiredOptimizedAlloc.Accelerator; constants.IsAcceleratorResolved(acc) {
				acceleratorName = acc
			} else if curr, ok := currentAllocations[vaName]; ok && constants.IsAcceleratorResolved(curr.Accelerator) {
				acceleratorName = curr.Accelerator
			}

			// Fallback for new VAs without prior status or collected metrics:
			// resolve accelerator from deployment nodeSelector/nodeAffinity or VA label,
			// and use current deployment replicas as target to avoid unintended scaling.
			if !constants.IsAcceleratorResolved(acceleratorName) {
				scaleTargetName := updateVa.GetScaleTargetName()
				if scaleTargetName != "" {
					var scaleTarget scaletarget.ScaleTargetAccessor
					var err error
					if scaleTarget, err = scaletarget.FetchScaleTarget(ctx, e.client, va.Name, va.Spec.ScaleTargetRef.Kind, scaleTargetName, va.Namespace); err == nil {
						acceleratorName = accel.GetAcceleratorNameFromScaleTarget(&updateVa, scaleTarget)
						if targetReplicas == 0 && scaleTarget.GetReplicas() != nil {
							targetReplicas = int(*scaleTarget.GetReplicas())
						}
					} else {
						// If scaleTarget fetch fails, try VA label directly
						acceleratorName = accel.GetAcceleratorNameFromScaleTarget(&updateVa, nil)
					}
				}
			}

			reason = "No scaling decision (optimization loop)"
		}

		// If we still don't have an accelerator name (e.g. new VA, no decision, no current alloc), we can't update status sensibly
		// But we still need to set MetricsAvailable condition via the cache
		if acceleratorName == "" {
			logger.Info("Skipping status update for variant without accelerator info",
				"variant", vaName, "cacheKey.name", va.Name, "cacheKey.namespace", va.Namespace)
		}

		// Emit a K8s event when accelerator cannot be resolved so operators
		// can see the problem without digging through controller logs.
		// The message is a constant string (not built per-cycle via Eventf with
		// formatted args), so each emission produces an identical
		// (involvedObject, source, type, reason, message) tuple — which the K8s
		// API server's event aggregator collapses into a single Event entry with
		// an updated count, rather than creating a new entry each optimization
		// cycle.
		if !constants.IsAcceleratorResolved(acceleratorName) {
			e.emitAcceleratorNotResolvedEvent(&updateVa)
			e.policies.reportUnresolvedAccelerator(ctx, va.Namespace, vaName, string(e.Config.EffectiveLimiterMode()))
		}

		// Stage the just-computed decision on the in-memory VA so that
		// act.EmitMetrics below — which reads
		// Status.DesiredOptimizedAlloc.{NumReplicas,Accelerator}
		// (see actuator.EmitMetrics) — publishes the fresh target rather than
		// whatever was last persisted by the controller. This object is local
		// to the engine; CRD persistence happens later, via the cache write
		// (DecisionCache.Set) → controller patch path.
		// Sanitize the sentinel out of the staged value so neither EmitMetrics
		// nor the cache (which reuses statusAccelerator) ever sees it.
		numReplicas := int32(targetReplicas)
		statusAccelerator := acceleratorName
		if !constants.IsAcceleratorResolved(statusAccelerator) {
			statusAccelerator = ""
		}
		updateVa.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
			NumReplicas: &numReplicas,
			Accelerator: statusAccelerator,
			LastRunTime: metav1.Now(),
		}
		updateVa.Status.Actuation.Applied = false // Reset applied status until Actuator handles it (if needed)

		// Set condition based on decision characteristics (or lack thereof)
		if hasDecision {
			switch {
			case decision.SafetyOverride:
				llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
					llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
					metav1.ConditionTrue,
					"SaturationSafetyOverride",
					"saturation safety override: "+reason)
			case decision.SaturationOnly:
				llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
					llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
					metav1.ConditionTrue,
					"SaturationOnlyMode",
					fmt.Sprintf("saturation-only decision: %s (target: %d replicas)", reason, targetReplicas))
			default:
				llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
					llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
					metav1.ConditionTrue,
					llmdVariantAutoscalingV1alpha1.ReasonOptimizationSucceeded,
					fmt.Sprintf("Hybrid mode: %s (target: %d replicas)", reason, targetReplicas))
			}
		} else {
			// No active decision (just refreshing)
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
				llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
				metav1.ConditionTrue,
				llmdVariantAutoscalingV1alpha1.ReasonOptimizationSucceeded,
				"Optimization loop ran (no scaling change needed)")
		}

		// Emit metrics for external autoscalers (Important: Actuator emits these)
		// We should emit metrics even if no decision changed, to keep HPA alive
		act := actuator.NewActuator(e.client)
		/*
		   NOTE: emitSafetyNetMetrics handles cases where optimization FAILS.
		   Here we are in the success path (optimization ran, even if no change).
		   We should ensure metrics are emitted for the External Scaler.
		*/

		// Ensure we have a valid SAT/Model decision "SaturationOnly" flag for metric emission context if needed
		// For now we assume if no decision, it's not saturation-only forced override, just normal op.
		// isSaturationOnly := false
		// if hasDecision {
		// 	isSaturationOnly = decision.SaturationOnly
		// }

		// Always emit the replica scaling signal (the HPA/KEDA external metric).
		// EmitReplicaMetrics labels an unresolved accelerator as the bounded
		// "unresolved" value (never the internal sentinel), so the scaling signal
		// is never withheld; the scaler matches on variant/namespace rather than
		// accelerator_type, so it keeps working regardless. Accelerator-dimensioned
		// metrics (saturation/capacity) remain gated on resolution below.
		if err := act.EmitMetrics(ctx, &updateVa); err != nil {
			msg := "Failed to emit metrics for external autoscalers"
			// K8s best practice: events should reference the current resource version
			e.recordOptimizationFailedEvent([]llmdVariantAutoscalingV1alpha1.VariantAutoscaling{updateVa}, msg)
			logger.Error(err, msg, "variant", updateVa.Name)
		} else {
			// Only log detail if we had a decision or periodically (to avoid spamming logs on every loop for no-ops)
			if hasDecision {
				// Emit Kubernetes event for observability
				e.recordScalingEvent(&updateVa, decision.Action, decision.TargetReplicas, decision.Reason())
				if decision.WasLimited {
					e.recordEvent(va, corev1.EventTypeWarning, constants.K8SEventResourceConstrained, decision.Reason())
				}

				logger.Info("Successfully emitted metrics",
					"variant", updateVa.Name,
					"target", targetReplicas,
					"accelerator", acceleratorName)
			}
			updateVa.Status.Actuation.Applied = true
		}

		// Record saturation and capacity metrics when this cycle produced a
		// fresh decision for the variant. These accelerator-dimensioned series
		// carry the accelerator_type label, so they are emitted only when the
		// type is resolved — otherwise the internal sentinel would leak into a
		// label. When there is no fresh decision the existing series persist with
		// their last-recorded values until Prometheus' staleness marker fires.
		if hasDecision && constants.IsAcceleratorResolved(acceleratorName) {
			act.RecordSaturationMetrics(ctx, decision)
		}

		// The wva_saturation_metrics_up freshness gauge carries only
		// {variant_name, namespace} (no accelerator_type), so it is emitted on
		// every cycle independent of accelerator resolution: 1.0 when a fresh
		// decision was produced for the variant, 0.0 otherwise. Dashboards gate
		// alerts on this gauge instead of relying on Prometheus' 5-minute
		// implicit staleness marker.
		if hasDecision {
			act.RecordSaturationFreshness(ctx, decision.VariantName, decision.Namespace, true)
		} else {
			act.RecordSaturationFreshness(ctx, va.Name, va.Namespace, false)
		}

		// Metric emission above (act.EmitMetrics) is the sole output for
		// annotation-sourced variants: KEDA/HPA reads wva_desired_replicas
		// directly. There is no CRD status to patch, so no cache write or
		// reconcile trigger is needed.

		if hasDecision {
			if decision.Action != domain.ActionNoChange {
				if err := e.metricsEmitter.EmitReplicaScalingMetrics(ctx, &updateVa, decision.Action, decision.ReasonCategory()); err != nil {
					logger.Error(err, "Failed to emit replica scaling metrics")
				}
			}
			logger.Info("Applied saturation decision via shared cache",
				"variant", vaName,
				"namespace", updateVa.Namespace,
				"action", decision.Action,
				"target", targetReplicas,
				"reason", reason)
		}
	}
}

// emitAcceleratorNotResolvedEvent records a Warning event on the given
// VariantAutoscaling so operators see at-a-glance that the optimization
// loop ran but could not resolve an accelerator type for it. The message
// is a constant string so the API server's event aggregator collapses
// repeated emissions into a single Event entry with an updated count
// rather than creating a new entry each optimization cycle.
func (e *Engine) emitAcceleratorNotResolvedEvent(va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling) {
	// A placement constraint is the only thing that resolves this now: the
	// acceleratorName label was removed because a workload that does not constrain
	// placement can be scheduled onto any GPU node, so the label asserted a type
	// nothing enforced. Telling an operator to set one would be telling them to
	// write down a guess.
	e.recordEvent(va, corev1.EventTypeWarning, "AcceleratorNotResolved",
		"The workload constrains no accelerator (no GPU product key in its nodeSelector or "+
			"nodeAffinity), so WVA cannot tell which accelerator it runs on. It is treated as "+
			"placeable on any of them: on a single-accelerator cluster the type is deduced, but "+
			"elsewhere its GPUs are charged to no accelerator pool (see wva_unattributed_gpus) and "+
			"accelerator-specific saturation/capacity metrics are withheld. Replica scaling metrics "+
			"are still emitted with accelerator_type=\"unresolved\" so HPA/KEDA can scale. "+
			"Set a GPU product nodeSelector or nodeAffinity on the workload to resolve it.")
}

// emitSafetyNetMetrics emits fallback metrics when saturation analysis fails.
func (e *Engine) emitSafetyNetMetrics(
	ctx context.Context,
	modelVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	currentAllocations map[string]*domain.Allocation,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
) {
	logger := ctrl.LoggerFrom(ctx)
	act := actuator.NewActuator(e.client)

	for _, va := range modelVAs {
		// Determine desired replicas
		var desiredReplicas, currentReplicas int32
		var fallbackSource string
		var scaleTarget scaletarget.ScaleTargetAccessor
		var err error
		if scaleTargets != nil {
			if target, ok := scaleTargets[utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())]; ok {
				scaleTarget = target
				// Get current replicas for metric emission.
				currentReplicas, err = act.GetCurrentScaleTargetReplicasFromScaleTarget(&va, scaleTarget)
				if err != nil {
					logger.Error(err, "Safety net: failed to get current replicas from scale target for metrics, using cached allocation",
						"variant", va.Name)
					if curr, ok := currentAllocations[utils.GetNamespacedKey(va.Namespace, va.Name)]; ok {
						currentReplicas = int32(curr.NumReplicas)
					}
				}
			}
		}

		// Strategy 1: Use previous desired replicas if available
		if va.Status.DesiredOptimizedAlloc.NumReplicas != nil && *va.Status.DesiredOptimizedAlloc.NumReplicas > 0 {
			desiredReplicas = *va.Status.DesiredOptimizedAlloc.NumReplicas
			fallbackSource = "previous-desired"
		} else {
			desiredReplicas = currentReplicas
			fallbackSource = "current-replicas"
		}

		// Determine accelerator - try status first, then labels, skip if unavailable
		// TODO: remove this checks when we will move to a new version of the CRD
		// with required accelerator field
		accelerator := va.Status.DesiredOptimizedAlloc.Accelerator
		if accelerator == "" {
			if curr, ok := currentAllocations[utils.GetNamespacedKey(va.Namespace, va.Name)]; ok {
				accelerator = curr.Accelerator
			}
		}
		if !constants.IsAcceleratorResolved(accelerator) {
			// Try to get accelerator name from scale target nodeSelector/nodeAffinity or VA labels
			if scaleTarget == nil {
				logger.V(logging.DEBUG).Info("Safety net: no scale target found for VA",
					"variant", va.Name)
			} else {
				accelerator = accel.GetAcceleratorNameFromScaleTarget(&va, scaleTarget)
			}
		}
		if !constants.IsAcceleratorResolved(accelerator) {
			// Do NOT withhold the scaling signal: EmitReplicaMetrics labels an
			// unresolved accelerator with the bounded "unresolved" value, so the
			// safety-net signal still reaches HPA/KEDA (which match on
			// variant/namespace, not accelerator_type). Mirrors the main path.
			logger.V(logging.DEBUG).Info("Safety net: accelerator unresolved, emitting scaling signal with 'unresolved' accelerator_type",
				"variant", va.Name)
		}

		// Emit safety net metrics
		if err := act.MetricsEmitter.EmitReplicaMetrics(
			ctx,
			&va,
			currentReplicas,
			desiredReplicas,
			accelerator,
		); err != nil {
			logger.Error(err, "Safety net: failed to emit metrics",
				"variant", va.Name)
			continue
		}

		logger.Info("Safety net activated: emitted fallback metrics",
			"variant", va.Name,
			"currentReplicas", currentReplicas,
			"desiredReplicas", desiredReplicas,
			"accelerator", accelerator,
			"fallbackSource", fallbackSource)
	}
}
