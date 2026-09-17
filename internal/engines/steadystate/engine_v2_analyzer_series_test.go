package steadystate

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// analyzerSeriesEngine returns an Engine wired to a fresh Prometheus registry so
// the wva_analyzer_* series it emits can be inspected directly.
func analyzerSeriesEngine(t *testing.T) (*Engine, *prometheus.Registry) {
	t.Helper()
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	return &Engine{
		metricsEmitter:     metrics.NewMetricsEmitter(),
		lastAnalyzerSeries: make(map[string]analyzerSeries),
	}, registry
}

// seriesLabels returns, for each series of the named metric family, the value of
// the given label. Sorted-insensitive: callers assert with ElementsMatch.
func seriesLabels(t *testing.T, registry *prometheus.Registry, metricName, labelName string) []string {
	t.Helper()
	mfs, err := registry.Gather()
	require.NoError(t, err)
	var out []string
	for _, mf := range mfs {
		if mf.GetName() != metricName {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == labelName {
					out = append(out, l.GetValue())
				}
			}
		}
	}
	return out
}

func namedResult(name string, roleDemand map[string]float64, total float64, variants ...string) allocation.NamedAnalyzerResult {
	r := &domain.AnalyzerResult{AnalyzerName: name, TotalDemand: total}
	for _, v := range variants {
		r.VariantCapacities = append(r.VariantCapacities, domain.VariantCapacity{VariantName: v, PerReplicaCapacity: 1})
	}
	nr := allocation.NamedAnalyzerResult{Name: name, Result: r}
	if len(roleDemand) > 0 {
		nr.RoleCapacities = make(map[string]domain.RoleCapacity, len(roleDemand))
		for role, d := range roleDemand {
			nr.RoleCapacities[role] = domain.RoleCapacity{Role: role, TotalDemand: d}
		}
	}
	return nr
}

// Absence is meaningful for the analyzer metrics: a series that stops being
// emitted must disappear rather than freeze at its last value, or a consumer
// cannot tell a stale reading from a live one. These tests pin the eviction of
// each way a series can stop being emitted.

func TestRecordAnalyzerMetrics_EvictsRoleThatDisappears(t *testing.T) {
	e, registry := analyzerSeriesEngine(t)

	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		namedResult("saturation", map[string]float64{"prefill": 10, "decode": 20}, 30, "v1"),
	})
	require.ElementsMatch(t, []string{"prefill", "decode"},
		seriesLabels(t, registry, constants.WVAAnalyzerDemand, constants.LabelRole))

	// The fleet stops being disaggregated: prefill/decode give way to the
	// model-level series (role="").
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 30, "v1"),
	})

	assert.ElementsMatch(t, []string{""},
		seriesLabels(t, registry, constants.WVAAnalyzerDemand, constants.LabelRole),
		"the retired per-role series must not linger")
}

func TestRecordAnalyzerMetrics_EvictsVariantThatDisappears(t *testing.T) {
	e, registry := analyzerSeriesEngine(t)

	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 5, "v1", "v2"),
	})
	require.ElementsMatch(t, []string{"v1", "v2"},
		seriesLabels(t, registry, constants.WVAAnalyzerTarget, constants.LabelVariantName))

	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 5, "v1"),
	})

	assert.ElementsMatch(t, []string{"v1"},
		seriesLabels(t, registry, constants.WVAAnalyzerTarget, constants.LabelVariantName),
		"the removed variant's target series must not linger")
}

func TestRecordAnalyzerMetrics_EvictsAnalyzerThatStopsRunning(t *testing.T) {
	e, registry := analyzerSeriesEngine(t)

	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 5, "v1"),
		namedResult("ttft-slo", nil, 7, "v1"),
	})
	require.ElementsMatch(t, []string{"saturation", "ttft-slo"},
		seriesLabels(t, registry, constants.WVAAnalyzerDemand, constants.LabelAnalyzerName))

	// ttft-slo is disabled in config, so it no longer reports for this model.
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 5, "v1"),
	})

	assert.ElementsMatch(t, []string{"saturation"},
		seriesLabels(t, registry, constants.WVAAnalyzerDemand, constants.LabelAnalyzerName))
	assert.ElementsMatch(t, []string{"saturation"},
		seriesLabels(t, registry, constants.WVAAnalyzerTarget, constants.LabelAnalyzerName))
}

func TestRecordAnalyzerMetrics_KeepsSeriesThatSurvive(t *testing.T) {
	e, registry := analyzerSeriesEngine(t)

	emit := func() {
		e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
			namedResult("saturation", map[string]float64{"prefill": 10, "decode": 20}, 30, "v1", "v2"),
		})
	}
	emit()
	emit()
	emit()

	assert.ElementsMatch(t, []string{"prefill", "decode"},
		seriesLabels(t, registry, constants.WVAAnalyzerDemand, constants.LabelRole),
		"a steady-state cycle must not churn its own series")
	assert.ElementsMatch(t, []string{"v1", "v2"},
		seriesLabels(t, registry, constants.WVAAnalyzerTarget, constants.LabelVariantName))
}

func TestRecordAnalyzerMetrics_IgnoresNilResults(t *testing.T) {
	e, registry := analyzerSeriesEngine(t)

	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 5, "v1"),
		{Name: "skipped", Result: nil},
	})

	assert.ElementsMatch(t, []string{"saturation"},
		seriesLabels(t, registry, constants.WVAAnalyzerDemand, constants.LabelAnalyzerName))
}

func TestRecordAnalyzerMetrics_KeepsModelsIndependent(t *testing.T) {
	// One model's eviction must not touch another's series, including a model of
	// the same name in a different namespace.
	e, registry := analyzerSeriesEngine(t)

	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 1, "v1", "v2"),
	})
	e.recordAnalyzerMetrics("other-ns", "m", []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 1, "v9"),
	})

	// ns/m drops v2; other-ns/m must be untouched.
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 1, "v1"),
	})

	assert.ElementsMatch(t, []string{"v1", "v9"},
		seriesLabels(t, registry, constants.WVAAnalyzerTarget, constants.LabelVariantName))
}

func TestPruneAnalyzerSeries_EvictsDepartedModel(t *testing.T) {
	e, registry := analyzerSeriesEngine(t)

	e.recordAnalyzerMetrics("ns", "stays", []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 1, "v1"),
	})
	e.recordAnalyzerMetrics("ns", "gone", []allocation.NamedAnalyzerResult{
		namedResult("saturation", map[string]float64{"prefill": 1, "decode": 2}, 3, "v2", "v3"),
	})

	// Build the active-key the same way the production call site does
	// (engine.go, from the model group's namespace + modelID) rather than
	// hardcoding the format, so the two cannot silently diverge.
	e.pruneAnalyzerSeries(map[string]bool{utils.GetNamespacedKey("ns", "stays"): true})

	assert.ElementsMatch(t, []string{"stays"},
		seriesLabels(t, registry, constants.WVAAnalyzerDemand, constants.LabelModelName))
	assert.ElementsMatch(t, []string{"v1"},
		seriesLabels(t, registry, constants.WVAAnalyzerTarget, constants.LabelVariantName))
	assert.NotContains(t, e.lastAnalyzerSeries, utils.GetNamespacedKey("ns", "gone"),
		"bookkeeping for the departed model must be dropped")
}

func TestPruneAnalyzerSeries_EmptyActiveSetIsNoOp(t *testing.T) {
	// Mirrors pruneLastGoodAnalysis: a transient cycle that enumerates no models
	// (collector hiccup, config not loaded yet) must not wipe live series.
	e, registry := analyzerSeriesEngine(t)

	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 1, "v1"),
	})

	e.pruneAnalyzerSeries(map[string]bool{})
	e.pruneAnalyzerSeries(nil)

	assert.ElementsMatch(t, []string{"m"},
		seriesLabels(t, registry, constants.WVAAnalyzerDemand, constants.LabelModelName))
	assert.Contains(t, e.lastAnalyzerSeries, utils.GetNamespacedKey("ns", "m"))
}

func TestPruneAnalyzerSeries_NilMapDoesNotPanic(t *testing.T) {
	e, _ := analyzerSeriesEngine(t)
	e.lastAnalyzerSeries = nil
	assert.NotPanics(t, func() { e.pruneAnalyzerSeries(map[string]bool{utils.GetNamespacedKey("ns", "m"): true}) })
}

// A cycle with no active models never reaches the per-model prune, so it evicts
// everything instead. Without this, a scale-to-zero fleet that idles overnight
// would keep publishing the demand and targets it saw at peak, with no other
// series left to contradict the stale reading.

func TestEvictAllAnalyzerSeries_ClearsEveryModel(t *testing.T) {
	e, registry := analyzerSeriesEngine(t)

	e.recordAnalyzerMetrics("ns", "m1", []allocation.NamedAnalyzerResult{
		namedResult("saturation", map[string]float64{"prefill": 1, "decode": 2}, 3, "v1", "v2"),
	})
	e.recordAnalyzerMetrics("other-ns", "m2", []allocation.NamedAnalyzerResult{
		namedResult("ttft-slo", nil, 4, "v3"),
	})
	require.NotEmpty(t, seriesLabels(t, registry, constants.WVAAnalyzerDemand, constants.LabelModelName))

	e.evictAllAnalyzerSeries()

	assert.Empty(t, seriesLabels(t, registry, constants.WVAAnalyzerDemand, constants.LabelModelName))
	assert.Empty(t, seriesLabels(t, registry, constants.WVAAnalyzerTarget, constants.LabelVariantName))
	assert.Empty(t, e.lastAnalyzerSeries, "bookkeeping must be cleared too")
}

func TestEvictAllAnalyzerSeries_IsIdempotent(t *testing.T) {
	// Idle cycles repeat every reconcile; the second and later ones must be no-ops
	// rather than re-deleting or panicking.
	e, registry := analyzerSeriesEngine(t)

	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 1, "v1"),
	})

	assert.NotPanics(t, func() {
		e.evictAllAnalyzerSeries()
		e.evictAllAnalyzerSeries()
		e.evictAllAnalyzerSeries()
	})
	assert.Empty(t, seriesLabels(t, registry, constants.WVAAnalyzerDemand, constants.LabelModelName))
}

func TestEvictAllAnalyzerSeries_EmptyBookkeepingDoesNotPanic(t *testing.T) {
	e, _ := analyzerSeriesEngine(t)
	assert.NotPanics(t, func() { e.evictAllAnalyzerSeries() })
	e.lastAnalyzerSeries = nil
	assert.NotPanics(t, func() { e.evictAllAnalyzerSeries() })
}

func TestRecordAnalyzerMetrics_RepublishesAfterEvictAll(t *testing.T) {
	// The fleet comes back after an idle stretch: series must reappear, and the
	// eviction must not have left bookkeeping that suppresses them.
	e, registry := analyzerSeriesEngine(t)

	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 1, "v1"),
	})
	e.evictAllAnalyzerSeries()
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{
		namedResult("saturation", nil, 1, "v1"),
	})

	assert.ElementsMatch(t, []string{"m"},
		seriesLabels(t, registry, constants.WVAAnalyzerDemand, constants.LabelModelName))
	assert.ElementsMatch(t, []string{"v1"},
		seriesLabels(t, registry, constants.WVAAnalyzerTarget, constants.LabelVariantName))
}

// gaugeValues returns, for the named metric family, each series' value keyed
// by the given label.
func gaugeValues(t *testing.T, registry *prometheus.Registry, metricName, labelName string) map[string]float64 {
	t.Helper()
	mfs, err := registry.Gather()
	require.NoError(t, err)
	out := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != metricName {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == labelName {
					out[l.GetValue()] = m.GetGauge().GetValue()
				}
			}
		}
	}
	return out
}

// The observed-replicas series is the one analyzer series whose VALUE has to
// change on a cycle that skips analysis: nothing was observed, and a stale
// count would read as a fully scraped fleet in exactly the case the series
// exists to expose. The series itself is kept, like its siblings.
func TestZeroObservedReplicas_ZeroesWhatTheModelPublished(t *testing.T) {
	e, registry := analyzerSeriesEngine(t)

	nr := namedResult("saturation", nil, 5, "v1", "v2")
	nr.Result.VariantCapacities[0].ObservedReplicas = 3
	nr.Result.VariantCapacities[1].ObservedReplicas = 2
	e.recordAnalyzerMetrics("ns", "m", []allocation.NamedAnalyzerResult{nr})
	require.Equal(t, map[string]float64{"v1": 3, "v2": 2},
		gaugeValues(t, registry, constants.WVAAnalyzerObservedReplicas, constants.LabelVariantName))

	e.zeroObservedReplicas("ns", "m")

	assert.Equal(t, map[string]float64{"v1": 0, "v2": 0},
		gaugeValues(t, registry, constants.WVAAnalyzerObservedReplicas, constants.LabelVariantName),
		"a skipped cycle must read as nothing observed, with the series kept")
	assert.ElementsMatch(t, []string{"v1", "v2"},
		seriesLabels(t, registry, constants.WVAAnalyzerTarget, constants.LabelVariantName),
		"the sibling series are untouched")
}

func TestZeroObservedReplicas_UnknownModelIsNoOp(t *testing.T) {
	e, _ := analyzerSeriesEngine(t)
	e.zeroObservedReplicas("ns", "never-published")
}
