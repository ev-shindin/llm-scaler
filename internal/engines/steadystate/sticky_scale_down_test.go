package steadystate

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/policy"
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

// decisionFor builds the decision a cycle would carry at this demand: the
// optimizer's target, and the action that follows from it.
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

func TestPruneLastDecided_DropsWhatCannotBeTrusted(t *testing.T) {
	now := time.Now()
	e := &Engine{lastDecided: map[string]decidedMark{
		"ns/fresh": {at: now.Add(-time.Minute), uid: "a"},
		"ns/stale": {at: now.Add(-policy.DefaultMaxAge - time.Minute), uid: "b"},
	}}
	e.pruneLastDecided(policy.DefaultMaxAge, now)
	assert.Contains(t, e.lastDecided, "ns/fresh")
	assert.NotContains(t, e.lastDecided, "ns/stale")
}

// The identity gate end to end, as the engine wires it: a value decided for
// one incarnation of a target is not trusted for another, and a target the
// cycle did not read keeps the trust its mark earned.
func TestPublishedValueIsTrustedForOneIncarnationOnly(t *testing.T) {
	e := &Engine{
		lastDecided:     map[string]decidedMark{"ns/dep": {at: time.Now(), uid: "old"}},
		scaleTargetUIDs: map[string]types.UID{"ns/dep": "new"},
	}
	// Mirrors the published() closure in applySaturationDecisions.
	trusted := func(key string) bool {
		mark, decided := e.lastDecided[key]
		uid, known := e.scaleTargetUIDs[key]
		return decided && (!known || mark.uid == uid)
	}
	assert.False(t, trusted("ns/dep"), "read this cycle under a different UID: a different fleet")
	e.noteScaleTargetUID("ns/dep", "old")
	assert.True(t, trusted("ns/dep"), "the same fleet")
	e.scaleTargetUIDs = map[string]types.UID{}
	assert.True(t, trusted("ns/dep"), "not read this cycle: the mark stands, bounded by its age")
}

func TestStickyAge_FollowsALongOptimizeInterval(t *testing.T) {
	e := &Engine{}
	assert.Equal(t, policy.DefaultMaxAge, e.stickyAge(), "no config: the floor")
	e.Config = config.NewTestConfig()
	config.SetOptimizationIntervalForTest(e.Config, 2*time.Minute)
	assert.Equal(t, stickyAgeCycles*2*time.Minute, e.stickyAge(), "four cycles of a 2 m loop outrun the floor")
	config.SetOptimizationIntervalForTest(e.Config, 15*time.Second)
	assert.Equal(t, policy.DefaultMaxAge, e.stickyAge(), "four cycles of a 15 s loop do not")
}
