package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

func TestRecordAnalyzerDemandAndTarget(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}
	emitter := NewMetricsEmitter()

	emitter.RecordAnalyzerDemand("saturation", "ns", "m", "", 42)
	emitter.RecordAnalyzerDemand("ttft-slo", "ns", "m", "prefill", 7)
	emitter.RecordAnalyzerTarget("ttft-slo", "ns", "m", "v1", 0.5)

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	var gotDemand, gotTarget bool
	for _, mf := range mfs {
		switch mf.GetName() {
		case constants.WVAAnalyzerDemand:
			for _, m := range mf.GetMetric() {
				if getLabelValue(m, constants.LabelAnalyzerName) == "saturation" &&
					getLabelValue(m, constants.LabelRole) == "" {
					gotDemand = true
					if v := m.GetGauge().GetValue(); v != 42 {
						t.Errorf("wva_analyzer_demand{analyzer=saturation} = %v, want 42", v)
					}
				}
			}
		case constants.WVAAnalyzerTarget:
			for _, m := range mf.GetMetric() {
				if getLabelValue(m, constants.LabelVariantName) == "v1" {
					gotTarget = true
					if v := m.GetGauge().GetValue(); v != 0.5 {
						t.Errorf("wva_analyzer_target{variant_name=v1} = %v, want 0.5", v)
					}
				}
			}
		}
	}
	if !gotDemand {
		t.Errorf("metric %s (analyzer=saturation) not found", constants.WVAAnalyzerDemand)
	}
	if !gotTarget {
		t.Errorf("metric %s (variant_name=v1) not found", constants.WVAAnalyzerTarget)
	}
}

// TestRecordAnalyzerMetrics_NilGuard ensures the Record methods are a no-op when
// InitMetrics has not run (the gauges are nil), matching the other emitters.
func TestRecordAnalyzerMetrics_NilGuard(t *testing.T) {
	analyzerDemand = nil
	analyzerTarget = nil
	emitter := NewMetricsEmitter()
	// Must not panic.
	emitter.RecordAnalyzerDemand("a", "ns", "m", "", 1)
	emitter.RecordAnalyzerTarget("a", "ns", "m", "v", 1)
}

// countSeries returns how many series the named metric family currently holds.
// Absence is meaningful for the analyzer metrics, so the deletion tests assert
// on series counts rather than values: a series that stops being emitted must
// disappear from the registry, not freeze at its last value.
func countSeries(t *testing.T, registry *prometheus.Registry, name string) int {
	t.Helper()
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return len(mf.GetMetric())
		}
	}
	return 0
}

// hasSeries reports whether the named metric family holds a series whose
// labelName label equals labelValue.
func hasSeries(t *testing.T, registry *prometheus.Registry, name, labelName, labelValue string) bool {
	t.Helper()
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if getLabelValue(m, labelName) == labelValue {
				return true
			}
		}
	}
	return false
}

func TestDeleteAnalyzerDemandRemovesOnlyThatSeries(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}
	emitter := NewMetricsEmitter()

	emitter.RecordAnalyzerDemand("saturation", "ns", "m", "prefill", 10)
	emitter.RecordAnalyzerDemand("saturation", "ns", "m", "decode", 20)
	if got := countSeries(t, registry, constants.WVAAnalyzerDemand); got != 2 {
		t.Fatalf("setup: %d demand series, want 2", got)
	}

	emitter.DeleteAnalyzerDemand("saturation", "ns", "m", "prefill")

	if got := countSeries(t, registry, constants.WVAAnalyzerDemand); got != 1 {
		t.Errorf("after delete: %d demand series, want 1", got)
	}
	if hasSeries(t, registry, constants.WVAAnalyzerDemand, constants.LabelRole, "prefill") {
		t.Error("prefill demand series still present after delete")
	}
	if !hasSeries(t, registry, constants.WVAAnalyzerDemand, constants.LabelRole, "decode") {
		t.Error("decode demand series was wrongly removed")
	}
}

func TestDeleteAnalyzerTargetRemovesOnlyThatSeries(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}
	emitter := NewMetricsEmitter()

	emitter.RecordAnalyzerTarget("saturation", "ns", "m", "v1", 1)
	emitter.RecordAnalyzerTarget("saturation", "ns", "m", "v2", 2)

	emitter.DeleteAnalyzerTarget("saturation", "ns", "m", "v1")

	if got := countSeries(t, registry, constants.WVAAnalyzerTarget); got != 1 {
		t.Errorf("after delete: %d target series, want 1", got)
	}
	if hasSeries(t, registry, constants.WVAAnalyzerTarget, constants.LabelVariantName, "v1") {
		t.Error("v1 target series still present after delete")
	}
}

func TestDeleteAnalyzerDemandWithEmptyRole(t *testing.T) {
	// The non-disaggregated series carries role="". Delete must match it, since
	// an empty label value is a real label value, not a wildcard.
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}
	emitter := NewMetricsEmitter()

	emitter.RecordAnalyzerDemand("saturation", "ns", "m", "", 42)
	emitter.DeleteAnalyzerDemand("saturation", "ns", "m", "")

	if got := countSeries(t, registry, constants.WVAAnalyzerDemand); got != 0 {
		t.Errorf("after delete: %d demand series, want 0", got)
	}
}

func TestDeleteAnalyzerSeriesForModelRemovesAllAnalyzersRolesAndVariants(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}
	emitter := NewMetricsEmitter()

	// Two analyzers, two roles and two variants for the departing model...
	emitter.RecordAnalyzerDemand("saturation", "ns", "gone", "prefill", 1)
	emitter.RecordAnalyzerDemand("saturation", "ns", "gone", "decode", 2)
	emitter.RecordAnalyzerDemand("ttft-slo", "ns", "gone", "", 3)
	emitter.RecordAnalyzerTarget("saturation", "ns", "gone", "v1", 4)
	emitter.RecordAnalyzerTarget("ttft-slo", "ns", "gone", "v2", 5)
	// ...plus a model that stays, including one in another namespace that shares
	// the departing model's name.
	emitter.RecordAnalyzerDemand("saturation", "ns", "stays", "", 6)
	emitter.RecordAnalyzerTarget("saturation", "ns", "stays", "v1", 7)
	emitter.RecordAnalyzerDemand("saturation", "other-ns", "gone", "", 8)

	emitter.DeleteAnalyzerSeriesForModel("ns", "gone")

	if got := countSeries(t, registry, constants.WVAAnalyzerDemand); got != 2 {
		t.Errorf("after delete: %d demand series, want 2 (ns/stays and other-ns/gone)", got)
	}
	if got := countSeries(t, registry, constants.WVAAnalyzerTarget); got != 1 {
		t.Errorf("after delete: %d target series, want 1 (ns/stays)", got)
	}
	if !hasSeries(t, registry, constants.WVAAnalyzerDemand, constants.LabelModelName, "stays") {
		t.Error("the surviving model's demand series was wrongly removed")
	}
	if !hasSeries(t, registry, constants.WVAAnalyzerDemand, constants.LabelNamespace, "other-ns") {
		t.Error("a same-named model in another namespace was wrongly removed")
	}
}

// TestDeleteAnalyzerMetrics_NilGuard mirrors TestRecordAnalyzerMetrics_NilGuard:
// the Delete methods must be a no-op when InitMetrics has not run, so callers
// need not know whether the gauges exist.
func TestDeleteAnalyzerMetrics_NilGuard(t *testing.T) {
	savedDemand := analyzerDemand
	savedTarget := analyzerTarget
	defer func() {
		analyzerDemand = savedDemand
		analyzerTarget = savedTarget
	}()

	analyzerDemand = nil
	analyzerTarget = nil
	emitter := NewMetricsEmitter()
	// Must not panic.
	emitter.DeleteAnalyzerDemand("a", "ns", "m", "r")
	emitter.DeleteAnalyzerTarget("a", "ns", "m", "v")
	emitter.DeleteAnalyzerSeriesForModel("ns", "m")
}

// TestDeleteAnalyzerSeries_WithControllerInstance exercises the exact-match
// requirement that motivated centralizing label construction: Prometheus's
// Delete matches on the FULL label set, and controller_instance is present only
// when CONTROLLER_INSTANCE is set. A Delete that built its labels independently
// of Record would silently fail to match here — leaving precisely the stale
// series this eviction exists to remove — while still passing every test that
// runs without the label.
func TestDeleteAnalyzerSeries_WithControllerInstance(t *testing.T) {
	savedInstance := controllerInstance
	savedDemand := analyzerDemand
	savedTarget := analyzerTarget
	defer func() {
		controllerInstance = savedInstance
		analyzerDemand = savedDemand
		analyzerTarget = savedTarget
	}()

	// Must be set before InitMetrics so the label is part of the gauge schema.
	t.Setenv(ControllerInstanceEnvVar, "controller-1")

	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}
	emitter := NewMetricsEmitter()

	emitter.RecordAnalyzerDemand("saturation", "ns", "m", "prefill", 10)
	emitter.RecordAnalyzerTarget("saturation", "ns", "m", "v1", 1)

	// The label really is on the series, so the delete below is a genuine
	// exact-match test rather than a no-op.
	if !hasSeries(t, registry, constants.WVAAnalyzerDemand, constants.LabelControllerInstance, "controller-1") {
		t.Fatal("setup: demand series is missing the controller_instance label")
	}
	if !hasSeries(t, registry, constants.WVAAnalyzerTarget, constants.LabelControllerInstance, "controller-1") {
		t.Fatal("setup: target series is missing the controller_instance label")
	}

	emitter.DeleteAnalyzerDemand("saturation", "ns", "m", "prefill")
	emitter.DeleteAnalyzerTarget("saturation", "ns", "m", "v1")

	if got := countSeries(t, registry, constants.WVAAnalyzerDemand); got != 0 {
		t.Errorf("after delete: %d demand series, want 0 (label set did not match)", got)
	}
	if got := countSeries(t, registry, constants.WVAAnalyzerTarget); got != 0 {
		t.Errorf("after delete: %d target series, want 0 (label set did not match)", got)
	}
}

// TestDeleteAnalyzerSeriesForModel_WithControllerInstance covers the partial-match
// path with the optional label present. DeletePartialMatch matches on a label
// subset, so it must still evict the model's series when controller_instance is
// set and not named in the match.
func TestDeleteAnalyzerSeriesForModel_WithControllerInstance(t *testing.T) {
	savedInstance := controllerInstance
	savedDemand := analyzerDemand
	savedTarget := analyzerTarget
	defer func() {
		controllerInstance = savedInstance
		analyzerDemand = savedDemand
		analyzerTarget = savedTarget
	}()

	t.Setenv(ControllerInstanceEnvVar, "controller-1")

	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}
	emitter := NewMetricsEmitter()

	emitter.RecordAnalyzerDemand("saturation", "ns", "gone", "", 1)
	emitter.RecordAnalyzerTarget("saturation", "ns", "gone", "v1", 2)
	emitter.RecordAnalyzerDemand("saturation", "ns", "stays", "", 3)

	emitter.DeleteAnalyzerSeriesForModel("ns", "gone")

	if got := countSeries(t, registry, constants.WVAAnalyzerDemand); got != 1 {
		t.Errorf("after delete: %d demand series, want 1 (only ns/stays)", got)
	}
	if got := countSeries(t, registry, constants.WVAAnalyzerTarget); got != 0 {
		t.Errorf("after delete: %d target series, want 0", got)
	}
	if !hasSeries(t, registry, constants.WVAAnalyzerDemand, constants.LabelModelName, "stays") {
		t.Error("the surviving model's series was wrongly removed")
	}
}

// TestRecordAnalyzerObservedReplicas covers the case the series exists for: an
// analyzer that attributed MORE replicas than the scale target owns. The gauge
// has to carry the pre-clamp number, because that difference is what tells a
// half-scraped cycle apart from a fleet with something extra serving in it.
func TestRecordAnalyzerObservedReplicas(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}
	emitter := NewMetricsEmitter()

	emitter.RecordAnalyzerObservedReplicas("saturation", "ns", "m", "v1", 3)

	if got := countSeries(t, registry, constants.WVAAnalyzerObservedReplicas); got != 1 {
		t.Fatalf("%d observed-replica series, want 1", got)
	}
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var found bool
	for _, mf := range mfs {
		if mf.GetName() != constants.WVAAnalyzerObservedReplicas {
			continue
		}
		// gaugeValue is the shared helper in metrics_test.go.
		v, ok := gaugeValue(mf, map[string]string{constants.LabelVariantName: "v1"})
		if !ok {
			t.Fatal("no observed-replica series for variant v1")
		}
		if v != 3 {
			t.Errorf("observed replicas = %v, want 3", v)
		}
		found = true
	}
	if !found {
		t.Fatalf("metric %s not found", constants.WVAAnalyzerObservedReplicas)
	}
}

func TestDeleteAnalyzerObservedReplicasRemovesOnlyThatSeries(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}
	emitter := NewMetricsEmitter()

	emitter.RecordAnalyzerObservedReplicas("saturation", "ns", "m", "v1", 3)
	emitter.RecordAnalyzerObservedReplicas("saturation", "ns", "m", "v2", 2)
	emitter.DeleteAnalyzerObservedReplicas("saturation", "ns", "m", "v1")

	if got := countSeries(t, registry, constants.WVAAnalyzerObservedReplicas); got != 1 {
		t.Errorf("after delete: %d series, want 1", got)
	}
	if hasSeries(t, registry, constants.WVAAnalyzerObservedReplicas, constants.LabelVariantName, "v1") {
		t.Error("v1 series still present after delete")
	}
	if !hasSeries(t, registry, constants.WVAAnalyzerObservedReplicas, constants.LabelVariantName, "v2") {
		t.Error("v2 series was wrongly removed")
	}
}
