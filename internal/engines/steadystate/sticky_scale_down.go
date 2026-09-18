package steadystate

import (
	"fmt"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// stickyStepName is the pipeline step holdPublishedScaleDown records.
const stickyStepName = "sticky-scale-down"

// holdPublishedScaleDown keeps a scale-down that WVA has already published
// from being cancelled by demand noise, so the fleet actually descends.
//
// The scale-down rule is stateless: every cycle, from scratch, the target is
// the smallest replica count whose utilization stays under the scale-down
// boundary (0.7 by default). Nothing in that asks what WVA said LAST cycle.
// When demand sits near the boundary -- a model idling at a few requests per
// second, whose per-cycle demand moves 15 % either way -- the target flips
// between N and N-1 from one cycle to the next. KEDA's HPA then takes the
// maximum of the published values over its stabilization window (300 s), so
// one "N" in any 300 s keeps the N-th replica for good: the fleet never gets
// to N-1, the scale-UP threshold (0.85) never gets to judge N-1, and the
// hysteresis the two thresholds are meant to give never engages.
//
// Measured on the two-model benchmark, both runs: a model at 3 rps on two
// replicas published 1,2,1,2,... every 15-45 s for a 900 s quiet band
// (demand 18.8k-23.9k tokens against a boundary of 0.7 x 28 482 = 19 937),
// and held its second replica for the whole band -- 1 GPU x ~3000 s -- in
// the arm meant to show what autoscaling alone costs.
//
// The hold: while the count last published is below the count the target is
// running (a descent is in flight) and the fresh target has crept back above
// it, keep publishing the lower count unless the demand at THAT count would
// reach the scale-up threshold. That is the same hysteresis the two
// thresholds already express, applied to the published value instead of the
// running one -- the published value is what KEDA acts on. A fresh target
// that is lower still is taken as is (the descent continues), and a fresh
// target the demand justifies at the published count wins outright, so a
// burst arriving mid-descent is not held down: at 0.85 of the published
// count's capacity the hold releases and the fresh target stands.
//
// ONE-SIDED ON PURPOSE. The up direction already has its stickiness, from
// the actuator: KEDA's HPA acts on a scale-up at its next sync (its scale-up
// stabilization window is 0) and treats a published value that has dipped
// back as a scale-DOWN request, which enters the same 300 s max-window the
// higher value is still in -- so a target that chatters N, N+1, N, ... goes
// to N+1 once and stays, and the measured runs show exactly that (3, 4, 3,
// 4 during a burst; the fleet went to 4 once). The down direction is the one
// where the published value falls through, because there the window IS the
// stickiness and the chatter defeats it. A direct actuator with no window
// behind it would need the mirror of this hold -- keep a published N+1 until
// demand at N+1 drops under the scale-down boundary -- and that is the
// release test with the other threshold, to be added behind the same switch
// when such an actuator exists and can be measured.
//
// Reports whether it changed the decision. Inert without a published value,
// without a descent in flight, or when the decision carries no capacity (a
// path that did not go through the optimizer's decision builder).
func holdPublishedScaleDown(d domain.VariantDecision, published int, havePublished bool) (domain.VariantDecision, bool) {
	if !havePublished || published <= 0 {
		return d, false
	}
	if published >= d.CurrentReplicas {
		return d, false // nothing is descending
	}
	if d.TargetReplicas <= published {
		return d, false // descending further, or the same answer
	}
	if d.PerReplicaCapacity <= 0 || d.ScaleUpThreshold <= 0 {
		return d, false
	}
	utilAtPublished := d.TotalDemand / (float64(published) * d.PerReplicaCapacity)
	if utilAtPublished >= d.ScaleUpThreshold {
		return d, false // the published count would be saturated: let the fresh target stand
	}
	crept := d.TargetReplicas
	d.TargetReplicas = published
	d.Action = domain.ActionScaleDown
	d.AddDecisionStep(stickyStepName, fmt.Sprintf(
		"held the published %d against a fresh target of %d: utilization at %d would be %.2f, under the scale-up threshold %.2f",
		published, crept, published, utilAtPublished, d.ScaleUpThreshold), true)
	return d, true
}
