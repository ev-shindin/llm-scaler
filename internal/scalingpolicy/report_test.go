package scalingpolicy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// The reporter's whole job is to say a thing once and then stay quiet until it
// is no longer the same thing, so what a test can pin is the key each method
// reports under and what counts as a change. The lines themselves go to the
// logger; these read the record the throttle keeps, which is what decides
// whether a line is printed at all.

func TestChangeReporter_SaysAThingOnceAndAgainWhenItChanges(t *testing.T) {
	r := NewChangeReporter()
	assert.True(t, r.changed("k", "a"), "the first time is always a change")
	assert.False(t, r.changed("k", "a"), "the same summary is not")
	assert.True(t, r.changed("k", "b"), "a different summary is")
	assert.False(t, r.changed("k", "b"))
	assert.True(t, r.changed("other", "a"), "another key keeps its own record")
}

func TestChangeReporter_NilReportsNothing(t *testing.T) {
	// An Engine built in a test leaves this field nil; production sets it in
	// NewEngine. Reporting must be inert rather than a panic.
	var r *ChangeReporter
	assert.False(t, r.changed("k", "a"))
	assert.NotPanics(t, func() {
		r.ReportUnknownPolicy(context.Background(), "ns", "v", "gold", nil)
		r.ReportPolicyConflict(context.Background(), "ns", "m", []string{"gold"}, "gold")
		r.ReportEffectivePolicy(context.Background(), "ns", "m", "gold", config.ScalingPolicy{})
		r.ReportUnresolvedAccelerator(context.Background(), "ns", "v", "")
	})
}

func TestChangeReporter_EachMethodKeepsItsOwnKey(t *testing.T) {
	ctx := context.Background()
	r := NewChangeReporter()
	r.ReportUnknownPolicy(ctx, "ns", "v", "gold", []string{"default"})
	r.ReportPolicyConflict(ctx, "ns", "m", []string{"gold", "silver"}, "gold")
	r.ReportEffectivePolicy(ctx, "ns", "m", "gold", config.ScalingPolicy{})
	r.ReportUnresolvedAccelerator(ctx, "ns", "v", "")

	// One record each: a variant's unknown tier and its unresolved accelerator
	// share a name but not a key, and the model's conflict and effective policy
	// likewise.
	assert.Len(t, r.seen, 4)
	assert.Equal(t, "unknown|gold", r.seen["ns/v"])
	assert.Equal(t, "conflict|gold|gold,silver", r.seen["ns|m"])
	assert.Equal(t, "gold|0.000|0.000|0.000|0.000", r.seen["effective|ns|m"])
	assert.Equal(t, "false", r.seen["accel|ns/v"])
}

func TestChangeReporter_AModelOnNoTierIsReportedAsTheDefaultEntry(t *testing.T) {
	r := NewChangeReporter()
	r.ReportEffectivePolicy(context.Background(), "ns", "m", "", config.ScalingPolicy{})
	assert.Equal(t, "(default entry)|0.000|0.000|0.000|0.000", r.seen["effective|ns|m"],
		"an empty tier name reads as the default entry, not as an empty string")
}

func TestChangeReporter_AConflictIsTheSameWhateverOrderTheTiersArriveIn(t *testing.T) {
	ctx := context.Background()
	r := NewChangeReporter()
	r.ReportPolicyConflict(ctx, "ns", "m", []string{"gold", "silver"}, "gold")
	before := r.seen["ns|m"]
	r.ReportPolicyConflict(ctx, "ns", "m", []string{"silver", "gold"}, "gold")
	assert.Equal(t, before, r.seen["ns|m"], "the tiers are sorted, so map order is not a change")
}

func TestChangeReporter_TheEffectivePolicyChangesWithTheBandItResolvedTo(t *testing.T) {
	ctx := context.Background()
	r := NewChangeReporter()
	cfg := config.ScalingPolicy{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.70}
	r.ReportEffectivePolicy(ctx, "ns", "m", "gold", cfg)
	first := r.seen["effective|ns|m"]
	r.ReportEffectivePolicy(ctx, "ns", "m", "gold", cfg)
	assert.Equal(t, first, r.seen["effective|ns|m"], "the same tier at the same thresholds is not a change")

	cfg.ScaleUpThreshold = 0.90
	r.ReportEffectivePolicy(ctx, "ns", "m", "gold", cfg)
	assert.NotEqual(t, first, r.seen["effective|ns|m"], "the same tier resolving to a different band is")
}

func TestChangeReporter_AnUnresolvedAcceleratorChangesWithWhatItCosts(t *testing.T) {
	ctx := context.Background()
	r := NewChangeReporter()
	// Without a limiter the variant still scales; with one it never scales up,
	// which is the difference the line exists to report.
	r.ReportUnresolvedAccelerator(ctx, "ns", "v", string(config.LimiterTypeNone))
	assert.Equal(t, "false", r.seen["accel|ns/v"])
	r.ReportUnresolvedAccelerator(ctx, "ns", "v", "gpu-budget")
	assert.Equal(t, "true", r.seen["accel|ns/v"])
}
