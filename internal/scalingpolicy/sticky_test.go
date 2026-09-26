package scalingpolicy

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// The numbers are the two-model benchmark's, sparse shape, nopool arm, Qwen
// at 3 rps after its first burst: per-replica capacity 28 482 tokens, the
// shipped thresholds, and the demand the analyzer reported cycle by cycle
// (wva_analyzer_demand, t=495..705 s). Against that capacity the stateless
// target is 1 for the first four cycles and 2 for every one after -- the
// controller itself published one more "1" at t=600 s, where the capacity
// estimate came from a different bucket (35 330) for one cycle -- and the
// fleet held two replicas for the whole quiet band.
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

// hold calls HoldPublishedScaleDown with a value published just now.
func hold(d domain.VariantDecision, published int, have bool) (domain.VariantDecision, bool) {
	now := time.Now()
	return HoldPublishedScaleDown(d, published, now.Add(-time.Second), have, DefaultMaxAge, now)
}

// carry calls CarryPublished on the first missed cycle, with the running
// count read.
func carry(resolved, published int, publishedAt time.Time, have bool, floor *int, now time.Time) int {
	return CarryPublished(resolved, true, published, publishedAt, have, floor, 1, now)
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
		d, _ := hold(decisionFor(2, fresh, demand), published, have)
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
	d, held := hold(decisionFor(2, 2, 24300), 1, true)
	assert.False(t, held)
	assert.Equal(t, 2, d.TargetReplicas)

	// Just under it, the hold stands.
	d, held = hold(decisionFor(2, 2, 24100), 1, true)
	assert.True(t, held)
	assert.Equal(t, 1, d.TargetReplicas)
}
func TestHoldPublishedScaleDown_ABurstMidDescentIsNotHeldDown(t *testing.T) {
	// Descending 4 -> 1 when a burst arrives: the fresh target is 3, demand
	// at the published 1 would be far past the threshold. The burst wins.
	d, held := hold(decisionFor(4, 3, 60000), 1, true)
	assert.False(t, held)
	assert.Equal(t, 3, d.TargetReplicas)
	assert.Equal(t, domain.ActionScaleDown, d.Action, "4 -> 3 is still a scale-down; the action is the optimizer's")
}
func TestHoldPublishedScaleDown_DescendingFurtherIsTakenAsIs(t *testing.T) {
	// Published 2 while running 4; the fresh target says 1. The descent
	// continues -- the hold only stops targets that crept UP.
	d, held := hold(decisionFor(4, 1, 18000), 2, true)
	assert.False(t, held)
	assert.Equal(t, 1, d.TargetReplicas)
}
func TestHoldPublishedScaleDown_InertWithoutADescent(t *testing.T) {
	// Nothing published yet.
	d, held := hold(decisionFor(2, 2, 20000), 0, false)
	assert.False(t, held)
	assert.Equal(t, 2, d.TargetReplicas)

	// Published equals the running count: no descent in flight.
	d, held = hold(decisionFor(2, 2, 20000), 2, true)
	assert.False(t, held)

	// Published above the running count (a scale-up in flight) is not this
	// stage's business either.
	d, held = hold(decisionFor(2, 3, 50000), 3, true)
	assert.False(t, held)
	assert.Equal(t, 3, d.TargetReplicas)

	// Same answer as published: nothing to hold.
	d, held = hold(decisionFor(2, 1, 18000), 1, true)
	assert.False(t, held)
	assert.Equal(t, 1, d.TargetReplicas)
}
func TestHoldPublishedScaleDown_InertWithoutCapacity(t *testing.T) {
	d := decisionFor(2, 2, 20000)
	d.PerReplicaCapacity = 0
	out, held := hold(d, 1, true)
	assert.False(t, held)
	assert.Equal(t, 2, out.TargetReplicas)

	d = decisionFor(2, 2, 20000)
	d.ScaleUpThreshold = 0
	_, held = hold(d, 1, true)
	assert.False(t, held)
}
func TestHoldPublishedScaleDown_RecordsItsStep(t *testing.T) {
	d, held := hold(decisionFor(2, 2, 20676), 1, true)
	require.True(t, held)
	step := d.LastStep()
	require.NotNil(t, step)
	assert.Equal(t, stickyStepName, step.Name)
	assert.True(t, step.WasConstrained)
	assert.Equal(t, 1, step.TargetReplicas)
	assert.Equal(t, domain.ActionScaleDown, d.Action)
	assert.Contains(t, step.Reason, "held the published 1 against a fresh target of 2")
	assert.Equal(t, stickyReason, d.Reason(), "the event and the condition carry the hold's reason, not the optimizer's")
	assert.NotContains(t, d.Reason(), "0.", "the event message carries no per-cycle number, so the API server can aggregate it")
}
func TestCarryPublished_ANoDecisionCycleKeepsTheHeldValue(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-time.Second)
	// The no-decision path resolved the running count (2); 1 was published.
	assert.Equal(t, 1, carry(2, 1, fresh, true, nil, now))
	// Nothing published, or published is not lower: the resolved value stands.
	assert.Equal(t, 2, carry(2, 0, fresh, false, nil, now))
	assert.Equal(t, 2, carry(2, 2, fresh, true, nil, now))
	assert.Equal(t, 2, carry(2, 3, fresh, true, nil, now))
	// A floor raised above the published value stands, as it does for the hold.
	two := 2
	assert.Equal(t, 2, carry(2, 1, fresh, true, &two, now))
	one := 1
	assert.Equal(t, 1, carry(2, 1, fresh, true, &one, now))
	// A publish whose deciding cycle is stale is a previous incarnation's, or
	// an outage's: the resolved value stands, so a manual scale-up during a
	// long metrics gap is not undone by a carry.
	assert.Equal(t, 2, carry(2, 1, now.Add(-carryMaxAge-time.Minute), true, nil, now))
	// The carry stops strictly inside KEDA's 300 s window, by age and by cycles,
	// so an operator's hand-scaled fleet during an outage is what fills the
	// window, not a held value that would undo it when the carry stopped.
	assert.Less(t, carryMaxAge, 5*time.Minute)
	assert.Equal(t, 2, CarryPublished(2, true, 1, fresh, true, nil, carryMaxCycles+1, now))
	assert.Equal(t, 1, CarryPublished(2, true, 1, fresh, true, nil, carryMaxCycles, now))
	// Two zeros. UNREAD: the path could not read the scale target, and
	// publishing 0 would read as "park", so the fresh held value stands in.
	assert.Equal(t, 1, CarryPublished(0, false, 1, fresh, true, nil, 1, now))
	assert.Equal(t, 0, CarryPublished(0, false, 1, now.Add(-carryMaxAge-time.Minute), true, nil, 1, now), "unless it is stale")
	// READ: somebody scaled the fleet to zero by hand. That is what is
	// published; a carried 1 here would wake what the operator just parked.
	assert.Equal(t, 0, carry(0, 1, fresh, true, nil, now))
}
func TestHoldPublishedScaleDown_LeavesALimitedDecisionAlone(t *testing.T) {
	// A GPU limiter bound this target; its reason is what the constrained
	// event carries, and the hold must not overwrite it or lower the count.
	d := decisionFor(2, 2, 20676)
	d.WasLimited = true
	out, held := hold(d, 1, true)
	assert.False(t, held)
	assert.Equal(t, 2, out.TargetReplicas)
}
func TestHoldPublishedScaleDown_InertWhenTheShareIsUnknown(t *testing.T) {
	// The variant reported no rows while a sibling did: its demand cannot be
	// priced, and 0 would read as "never saturated".
	d := decisionFor(2, 2, domain.DemandUnpriced)
	out, held := hold(d, 1, true)
	assert.False(t, held)
	assert.Equal(t, 2, out.TargetReplicas)
}
func TestHoldPublishedScaleDown_NeverHoldsAScaleUp(t *testing.T) {
	// Two variants: this one is the cheapest per capacity, so the optimizer
	// adds the model's replicas here, but its own share of the demand is small.
	// A fresh target ABOVE the running count is a scale-up and stands, share or
	// no share.
	d := decisionFor(2, 3, 7500) // 7500/(1 x 28482) = 0.26 at the published 1
	out, held := hold(d, 1, true)
	assert.False(t, held)
	assert.Equal(t, 3, out.TargetReplicas)
}
func TestHoldPublishedScaleDown_RespectsARaisedFloor(t *testing.T) {
	// 2 running, 1 published, and the operator has since raised minReplicaCount
	// to 2: the optimizer's floored target of 2 must stand, or the hold would
	// publish 1 below the floor forever -- KEDA never applies it, so the
	// published value never catches up with the running count.
	d := decisionFor(2, 2, 20000)
	two := 2
	d.MinReplicas = &two
	out, held := hold(d, 1, true)
	assert.False(t, held)
	assert.Equal(t, 2, out.TargetReplicas)
}
func TestHoldPublishedScaleDown_IgnoresAStalePublishedValue(t *testing.T) {
	// The store never evicts. A value published for a previous incarnation of
	// the same scale target -- deleted and re-created under the same name --
	// must not arm a hold on the new one.
	now := time.Now()
	d, held := HoldPublishedScaleDown(decisionFor(3, 2, 20000), 1, now.Add(-DefaultMaxAge-time.Minute), true, DefaultMaxAge, now)
	assert.False(t, held)
	assert.Equal(t, 2, d.TargetReplicas)

	// The same decision with the value published just now IS held -- so it is
	// the age, and nothing else, that made the difference above.
	_, held = HoldPublishedScaleDown(decisionFor(3, 2, 20000), 1, now.Add(-time.Second), true, DefaultMaxAge, now)
	assert.True(t, held)

	// And the same stale value under a bound that covers it is held too: the
	// bound follows the optimize interval, so a slow loop is not stale to itself.
	_, held = HoldPublishedScaleDown(decisionFor(3, 2, 20000), 1, now.Add(-DefaultMaxAge-time.Minute), true, 2*DefaultMaxAge, now)
	assert.True(t, held)
}
