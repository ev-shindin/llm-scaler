package steadystate

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// The numbers are the two-model benchmark's, sparse shape, nopool arm, Qwen
// at 3 rps after its first burst: per-replica capacity 28 482 tokens, the
// shipped thresholds, and the demand the analyzer reported cycle by cycle
// (wva_analyzer_demand, t=495..705 s). The stateless target flipped 1,1,1,1,
// 2,2,2,1,2,2,2,2,2,2,2 over those cycles and the fleet held two replicas
// for the whole quiet band.
const (
	qwenCapacity = 28482.0
	scaleUp      = 0.85
	scaleDown    = 0.70
)

var qwenDemand = []float64{18770, 19063, 18770, 19503, 20676, 22876, 23903, 21136, 20237, 21996, 20090, 23023, 22729, 23609, 22583}

// statelessTarget is the scale-down rule as the optimizer applies it: the
// smallest replica count whose utilization stays under the boundary.
func statelessTarget(demand float64) int {
	return int(math.Ceil(demand / (scaleDown * qwenCapacity)))
}

func decisionFor(current, target int, demand float64) domain.VariantDecision {
	d := domain.VariantDecision{
		VariantName:        "qwen",
		CurrentReplicas:    current,
		TargetReplicas:     target,
		TotalDemand:        demand,
		PerReplicaCapacity: qwenCapacity,
		ScaleUpThreshold:   scaleUp,
	}
	switch {
	case target < current:
		d.Action = domain.ActionScaleDown
	case target > current:
		d.Action = domain.ActionScaleUp
	default:
		d.Action = domain.ActionNoChange
	}
	return d
}

func TestHoldPublishedScaleDown_TheMeasuredChatterSettlesAtOne(t *testing.T) {
	// The measured sequence, with the fleet still at two replicas (KEDA has
	// not acted yet) and the target recomputed from scratch each cycle.
	stateless := make([]int, 0, len(qwenDemand))
	sticky := make([]int, 0, len(qwenDemand))
	published, have := 0, false
	for _, demand := range qwenDemand {
		fresh := statelessTarget(demand)
		stateless = append(stateless, fresh)
		d, _ := holdPublishedScaleDown(decisionFor(2, fresh, demand), published, have)
		sticky = append(sticky, d.TargetReplicas)
		published, have = d.TargetReplicas, true
	}
	require.Equal(t, []int{1, 1, 1, 1, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2}, stateless,
		"the stateless rule chatters on this demand (the first cycles say 1, the noise says 2)")
	assert.Equal(t, []int{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}, sticky,
		"once 1 is published it holds: no demand in the sequence reaches 0.85 of one replica")
}

func TestHoldPublishedScaleDown_ReleasesWhenThePublishedCountWouldSaturate(t *testing.T) {
	// 0.85 x 28 482 = 24 210 tokens. Demand at that level on one replica is
	// the scale-up case, and the hold must not stand in its way.
	d, held := holdPublishedScaleDown(decisionFor(2, 2, 24300), 1, true)
	assert.False(t, held)
	assert.Equal(t, 2, d.TargetReplicas)

	// Just under it, the hold stands.
	d, held = holdPublishedScaleDown(decisionFor(2, 2, 24100), 1, true)
	assert.True(t, held)
	assert.Equal(t, 1, d.TargetReplicas)
}

func TestHoldPublishedScaleDown_ABurstMidDescentIsNotHeldDown(t *testing.T) {
	// Descending 4 -> 1 when a burst arrives: the fresh target is 3, demand
	// at the published 1 would be far past the threshold. The burst wins.
	d, held := holdPublishedScaleDown(decisionFor(4, 3, 60000), 1, true)
	assert.False(t, held)
	assert.Equal(t, 3, d.TargetReplicas)
	assert.Equal(t, domain.ActionScaleDown, d.Action, "4 -> 3 is still a scale-down; the action is the optimizer's")
}

func TestHoldPublishedScaleDown_DescendingFurtherIsTakenAsIs(t *testing.T) {
	// Published 2 while running 4; the fresh target says 1. The descent
	// continues -- the hold only stops targets that crept UP.
	d, held := holdPublishedScaleDown(decisionFor(4, 1, 18000), 2, true)
	assert.False(t, held)
	assert.Equal(t, 1, d.TargetReplicas)
}

func TestHoldPublishedScaleDown_InertWithoutADescent(t *testing.T) {
	// Nothing published yet.
	d, held := holdPublishedScaleDown(decisionFor(2, 2, 20000), 0, false)
	assert.False(t, held)
	assert.Equal(t, 2, d.TargetReplicas)

	// Published equals the running count: no descent in flight.
	d, held = holdPublishedScaleDown(decisionFor(2, 2, 20000), 2, true)
	assert.False(t, held)

	// Published above the running count (a scale-up in flight) is not this
	// stage's business either.
	d, held = holdPublishedScaleDown(decisionFor(2, 3, 50000), 3, true)
	assert.False(t, held)
	assert.Equal(t, 3, d.TargetReplicas)

	// Same answer as published: nothing to hold.
	d, held = holdPublishedScaleDown(decisionFor(2, 1, 18000), 1, true)
	assert.False(t, held)
	assert.Equal(t, 1, d.TargetReplicas)
}

func TestHoldPublishedScaleDown_InertWithoutCapacity(t *testing.T) {
	d := decisionFor(2, 2, 20000)
	d.PerReplicaCapacity = 0
	out, held := holdPublishedScaleDown(d, 1, true)
	assert.False(t, held)
	assert.Equal(t, 2, out.TargetReplicas)

	d = decisionFor(2, 2, 20000)
	d.ScaleUpThreshold = 0
	_, held = holdPublishedScaleDown(d, 1, true)
	assert.False(t, held)
}

func TestHoldPublishedScaleDown_RecordsItsStep(t *testing.T) {
	d, held := holdPublishedScaleDown(decisionFor(2, 2, 20676), 1, true)
	require.True(t, held)
	step := d.LastStep()
	require.NotNil(t, step)
	assert.Equal(t, stickyStepName, step.Name)
	assert.True(t, step.WasConstrained)
	assert.Equal(t, 1, step.TargetReplicas)
	assert.Equal(t, domain.ActionScaleDown, d.Action)
	assert.Contains(t, step.Reason, "held the published 1 against a fresh target of 2")
}
