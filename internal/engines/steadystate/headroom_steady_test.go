package steadystate

import (
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// A fleet of STEADY models must still publish headroom every cycle.
//
// The optimizer publishes it from the attribution pass, and that pass runs only
// for a model that is scaling up. A namespace whose models all sit at no-change
// took neither that branch nor the idle-fleet one, so nothing was published --
// and the warm pool, the only reader, was left without an answer for as long
// as the fleet stayed calm. Measured on kind: one steady variant beside a pool
// on a one-GPU quota, and the pool grew to three Pods with no cap ever logged.
func TestASteadyFleetStillPublishesHeadroom(t *testing.T) {
	const (
		ns    = "llm-d-sim"
		accel = "NVIDIA-A100-PCIE-80GB"
	)
	decision.ResetWarmPoolGPUs()
	decision.DefaultHeadroom.Reset()
	t.Cleanup(func() {
		decision.ResetWarmPoolGPUs()
		decision.DefaultHeadroom.Reset()
	})

	// A pool holds one of the two GPUs allowed; the model holds the other.
	v := decision.PublishWarmPoolGPUs(ns, map[string]int{accel: 1})

	e := &Engine{GPULimiter: quotaLimiterFor(ns, accel, 2), optimizer: allocation.NewGreedyByScoreOptimizer()}

	// A request with no demand: nothing here scales up, so the optimizer's own
	// attribution pass will not run for it.
	_, constraints := e.selectV2Optimizer(t.Context(), []allocation.ModelScalingRequest{{
		ModelID:   "test-model",
		Namespace: ns,
		CompositeSignal: allocation.NamedAnalyzerResult{
			Name: domain.SaturationAnalyzerName, Result: &domain.AnalyzerResult{},
		},
		Variants:      []domain.VariantMetadata{{VariantName: "v", AcceleratorName: accel}},
		VariantStates: []domain.VariantReplicaState{{VariantName: "v", CurrentReplicas: 1, GPUsPerReplica: 1}},
	}})
	if len(constraints) == 0 {
		t.Fatal("no constraints were computed; the test is about what happens once they are")
	}

	free, state := decision.GPUHeadroomReading(ns, accel, v, time.Minute, time.Now())
	if state != decision.HeadroomBounded {
		t.Fatalf("a steady fleet under a quota must still publish headroom the pool can read; read %v", state)
	}
	// Two allowed, one held by the model and one by the pool: nothing left.
	if free != 0 {
		t.Errorf("headroom is %d, want 0 (model 1 + pool 1 of an allowance of 2)", free)
	}
	// And the snapshot is stamped with the pool figure it charged, so a pool
	// that has charged more since would not grow on it.
	if _, state := decision.GPUHeadroomReading(ns, accel, v+1, time.Minute, time.Now()); state != decision.HeadroomUnknown {
		t.Errorf("a snapshot built before a later pool charge must read unknown for it, read %v", state)
	}
}

// A limiter-less install with an ACTIVE fleet must tell the pool it is
// unbounded, every cycle. optimize() installs the cost-aware optimizer when no
// limiter is declared, and the constraints path returns early for it -- so a
// publish placed after that guard never ran, and a pool beside any registered
// model would have been held at its size for good. The idle path alone does not
// cover this: a fleet with a model in it is not idle.
func TestANoLimiterActiveFleetPublishesUnbounded(t *testing.T) {
	decision.ResetWarmPoolGPUs()
	decision.DefaultHeadroom.Reset()
	t.Cleanup(func() {
		decision.ResetWarmPoolGPUs()
		decision.DefaultHeadroom.Reset()
	})

	v := decision.PublishWarmPoolGPUs("tenant", map[string]int{"H100": 2})

	// No GPULimiter, and the optimizer optimize() installs for LimiterTypeNone.
	e := &Engine{optimizer: allocation.NewCostAwareOptimizer()}

	_, constraints := e.selectV2Optimizer(t.Context(), []allocation.ModelScalingRequest{{
		ModelID: "m", Namespace: "tenant",
		Variants:      []domain.VariantMetadata{{VariantName: "v", AcceleratorName: "H100"}},
		VariantStates: []domain.VariantReplicaState{{VariantName: "v", CurrentReplicas: 1, GPUsPerReplica: 1}},
	}})
	if constraints != nil {
		t.Fatalf("no limiter must yield no constraints, got %d", len(constraints))
	}
	if _, state := decision.GPUHeadroomReading("tenant", "H100", v, time.Minute, time.Now()); state != decision.HeadroomUnbounded {
		t.Fatalf("with no limiter and an active fleet the pool must read unbounded at its charge version %d, read %v", v, state)
	}
}

// Providers present but the optimizer not GPU-aware -- a limiter edit to none
// whose rebuild failed, leaving the old limiter under the cost-aware optimizer.
// The models scale unbounded on that path; the pool must be told the same.
func TestANonGPUAwareOptimizerWithProvidersPublishesUnbounded(t *testing.T) {
	decision.ResetWarmPoolGPUs()
	decision.DefaultHeadroom.Reset()
	t.Cleanup(func() {
		decision.ResetWarmPoolGPUs()
		decision.DefaultHeadroom.Reset()
	})
	v := decision.PublishWarmPoolGPUs("tenant", map[string]int{"H100": 1})

	e := &Engine{GPULimiter: quotaLimiterFor("tenant", "H100", 4), optimizer: allocation.NewCostAwareOptimizer()}
	_, constraints := e.selectV2Optimizer(t.Context(), []allocation.ModelScalingRequest{{
		ModelID: "m", Namespace: "tenant",
		Variants:      []domain.VariantMetadata{{VariantName: "v", AcceleratorName: "H100"}},
		VariantStates: []domain.VariantReplicaState{{VariantName: "v", CurrentReplicas: 1, GPUsPerReplica: 1}},
	}})
	if constraints != nil {
		t.Fatalf("a non-GPU-aware optimizer takes no constraints, got %d", len(constraints))
	}
	if _, state := decision.GPUHeadroomReading("tenant", "H100", v, time.Minute, time.Now()); state != decision.HeadroomUnbounded {
		t.Fatalf("the pool must read unbounded on the path where the models are, read %v", state)
	}
}
