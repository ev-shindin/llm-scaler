package steadystate

import (
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
)

// A warm pool's GPUs have to reach the CONSTRAINT PROVIDERS, not merely the
// published usage figure.
//
// These are two different consumers of the managed view. Publishing feeds
// scale-from-zero, which asks "may a wake be placed?". gpuUsageViews feeds the
// limiters, which decide what the optimizer may allocate. Charging the pool in
// one and not the other is the shape this bug took the first time: the published
// figure said the pool cost something while the quota enforcing it carried on as
// though it did not.
func TestWarmPoolGPUsReachTheConstraintViews(t *testing.T) {
	decision.ResetWarmPoolGPUs()
	t.Cleanup(decision.ResetWarmPoolGPUs)

	decision.PublishWarmPoolGPUs("tenant", map[string]int{"H100": 3})

	views := gpuUsageViews(nil)

	if got := views.ManagedByType["H100"]; got != 3 {
		t.Errorf("per-type view charged %d GPUs for the pool, want 3", got)
	}
	if got := views.ManagedByNamespace["tenant"]["H100"]; got != 3 {
		t.Errorf("per-namespace view charged %d, want 3", got)
	}
}

// A namespace whose ONLY WVA consumption is a pool has no scaling requests, so
// it would otherwise be absent from the view entirely -- and a namespace absent
// from the managed figure reads to a namespace-scoped quota as untouched, while
// the pool sits inside it holding GPUs.
func TestAPoolOnlyNamespaceStillAppears(t *testing.T) {
	decision.ResetWarmPoolGPUs()
	t.Cleanup(decision.ResetWarmPoolGPUs)

	decision.PublishWarmPoolGPUs("pool-only", map[string]int{"A100": 2})

	views := gpuUsageViews(nil)

	if _, present := views.ManagedByNamespace["pool-only"]; !present {
		t.Fatal("a namespace holding only a pool must still appear in the managed view")
	}
	if got := views.ManagedByNamespace["pool-only"]["A100"]; got != 2 {
		t.Errorf("charged %d, want 2", got)
	}
}

// No pools is not the same as a pool of zero: nothing is added, and no namespace
// is invented.
func TestNoPoolsAddNothing(t *testing.T) {
	decision.ResetWarmPoolGPUs()
	t.Cleanup(decision.ResetWarmPoolGPUs)

	views := gpuUsageViews(nil)

	if len(views.ManagedByType) != 0 {
		t.Errorf("per-type view is %v, want empty", views.ManagedByType)
	}
	if len(views.ManagedByNamespace) != 0 {
		t.Errorf("per-namespace view is %v, want empty", views.ManagedByNamespace)
	}
}

// Headroom is what the pool reads to decide whether it may grow. Nothing here
// exercises the limiter itself; this pins the contract the reconciler depends
// on -- that "no limiter bounds this namespace" is distinguishable from "no
// allowance left", because the pool must grow freely in the first case.
func TestHeadroomDistinguishesUnboundedFromExhausted(t *testing.T) {
	decision.DefaultHeadroom.Reset()
	t.Cleanup(decision.DefaultHeadroom.Reset)

	now := time.Now()
	decision.PublishHeadroom(map[string]map[string]int{"capped": {"H100": 0}}, now)

	if _, known := decision.GPUHeadroom("unbounded", "H100", time.Minute, now); known {
		t.Error("a namespace no limiter bounds must read as unknown, not exhausted")
	}
	free, known := decision.GPUHeadroom("capped", "H100", time.Minute, now)
	if !known || free != 0 {
		t.Errorf("an exhausted allowance read (%d, %v), want (0, true)", free, known)
	}
}
