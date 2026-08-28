package steadystate

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// A namespace with NO MODELS still has to be told what its allowance leaves.
//
// Headroom is published from inside the optimizer, which runs only when there is
// something to optimize. A namespace whose only WVA consumption is a warm pool
// has no variants, so no pass ran, so no headroom was published -- and the pool
// is the only reader of that figure, treating its absence as "nobody bounds this
// namespace". Measured on kind before this: a one-GPU quota, no models, and a
// pool that grew to three Pods.
//
// The test is about the empty case specifically. The populated case has always
// worked and is covered where attribution is.
func TestIdleFleetStillPublishesHeadroom(t *testing.T) {
	decision.ResetWarmPoolGPUs()
	decision.DefaultHeadroom.Reset()
	t.Cleanup(func() {
		decision.ResetWarmPoolGPUs()
		decision.DefaultHeadroom.Reset()
	})

	// Two GPUs allowed, one already held by a pool. One left.
	decision.PublishWarmPoolGPUs("tenant", map[string]int{"H100": 1})

	e := &Engine{GPULimiter: quotaLimiterFor("tenant", "H100", 2)}

	e.publishHeadroomForIdleFleet(context.Background())

	free, known := decision.GPUHeadroom("tenant", "H100", time.Minute, time.Now())
	if !known {
		t.Fatal("an idle namespace under a quota must still get an answer; " +
			"absent reads as unbounded to the warm pool")
	}
	if free != 1 {
		t.Errorf("headroom is %d, want 1: the pool's own GPU must be charged against the allowance", free)
	}
}

// A pool that has spent the whole allowance reads as ZERO left, not as absent.
// The two are the same number of GPUs and opposite instructions.
func TestIdleFleetReportsAnExhaustedAllowance(t *testing.T) {
	decision.ResetWarmPoolGPUs()
	decision.DefaultHeadroom.Reset()
	t.Cleanup(func() {
		decision.ResetWarmPoolGPUs()
		decision.DefaultHeadroom.Reset()
	})

	decision.PublishWarmPoolGPUs("tenant", map[string]int{"H100": 2})

	e := &Engine{GPULimiter: quotaLimiterFor("tenant", "H100", 2)}

	e.publishHeadroomForIdleFleet(context.Background())

	free, known := decision.GPUHeadroom("tenant", "H100", time.Minute, time.Now())
	if !known || free != 0 {
		t.Errorf("a spent allowance read (%d, %v), want (0, true)", free, known)
	}
}

// No limiter is not an empty allowance: nothing is published, so the pool goes
// on growing. Publishing zeros here would cap every pool on every cluster that
// has not configured a quota at all.
func TestIdleFleetWithNoLimiterPublishesNothing(t *testing.T) {
	decision.ResetWarmPoolGPUs()
	decision.DefaultHeadroom.Reset()
	t.Cleanup(func() {
		decision.ResetWarmPoolGPUs()
		decision.DefaultHeadroom.Reset()
	})

	decision.PublishWarmPoolGPUs("tenant", map[string]int{"H100": 2})

	e := &Engine{}

	e.publishHeadroomForIdleFleet(context.Background())

	if _, known := decision.GPUHeadroom("tenant", "H100", time.Minute, time.Now()); known {
		t.Error("with no limiter configured the namespace must stay unbounded")
	}
}

// quotaLimiterFor builds a namespace-scoped quota limiter, the shape an operator
// declares in the ConfigMap's default entry.
func quotaLimiterFor(namespace, accelerator string, gpus int) allocation.Limiter {
	return allocation.NewDefaultLimiter("test-quota", allocation.NewQuotaInventory(config.QuotaLimiterConfig{
		Name:  "test-quota",
		Type:  "quota",
		Scope: config.QuotaScopeNamespace,
		NamespaceQuotas: map[string]map[string]int{
			namespace: {accelerator: gpus},
		},
	}))
}
