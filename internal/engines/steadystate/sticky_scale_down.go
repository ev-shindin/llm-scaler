package steadystate

import (
	"fmt"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// stickyStepName is the pipeline step holdPublishedScaleDown records.
const stickyStepName = "sticky-scale-down"

// stickyReason is the decision reason a held scale-down carries. It reaches
// the ScaledDown event and the OptimizationReady condition, and it is
// constant on purpose: the API server aggregates only identical event
// messages, and a hold that lasts KEDA's window at a 15 s cycle would
// otherwise leave ~20 distinct events per descent. The numbers go in the
// pipeline step and the log line.
const stickyReason = "held the published scale-down: the fresh target crept back up while utilization at the published count stays under the scale-up threshold"

// stickyMaxAge is how old a published value may be and still be held. The
// decision store never evicts, so a Deployment deleted and re-created under
// the same name would otherwise inherit a value published for a fleet that
// no longer exists. The optimize loop republishes every cycle it decides, so
// anything older than this belongs to a previous incarnation or to a
// controller that has been silent long enough for KEDA's window to have
// closed anyway.
const stickyMaxAge = 5 * time.Minute

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
// What it cannot survive: the published value lives in memory, so a
// controller restart forgets it, and the safety net that runs while the
// engine has nothing to say publishes the previous desired from the
// variant's status when there is one and the current count when there is
// not -- a restart mid-descent therefore re-arms KEDA's window once. That is
// the pre-existing behaviour on that path, and one window per restart is
// what it costs. A no-decision CYCLE (a scrape gap, a skipped model) does
// not have that effect: carryPublished republishes the held value there.
//
// Reports whether it changed the decision. Inert without a published value,
// with one older than stickyMaxAge or below the variant's own floor, without
// a descent in flight, or when the decision carries no capacity (a path that
// did not go through the optimizer's decision builder).
func holdPublishedScaleDown(d domain.VariantDecision, published int, publishedAt time.Time, havePublished bool, now time.Time) (domain.VariantDecision, bool) {
	if !havePublished || published <= 0 {
		return d, false
	}
	if now.Sub(publishedAt) > stickyMaxAge {
		return d, false // published for a fleet this cycle cannot vouch for
	}
	if d.MinReplicas != nil && published < *d.MinReplicas {
		return d, false // the floor has moved above it; the floored target stands
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
	// Through SetDecisionReason, the one writer of Action, so the event and the
	// condition that carry Reason() say what happened rather than repeating the
	// optimizer's text under a different action.
	d.SetDecisionReason(domain.ActionScaleDown, d.ReasonCategory(), stickyReason)
	d.AddDecisionStep(stickyStepName, fmt.Sprintf(
		"held the published %d against a fresh target of %d: utilization at %d would be %.2f, under the scale-up threshold %.2f",
		published, crept, published, utilAtPublished, d.ScaleUpThreshold), true)
	return d, true
}

// carryPublished is the no-decision counterpart of the hold: a cycle with
// nothing to say for a variant republishes what it would otherwise have
// resolved -- the previous desired from status, or the running count when
// the variant has no status, which every variant synthesized from a
// ScaledObject lacks -- and the running count is exactly the value that
// re-arms KEDA's window mid-descent. When a fresh published value is lower,
// it is republished instead: a cycle with no metrics cannot justify raising
// what the last cycle with metrics lowered. Returns the value to publish.
func carryPublished(resolved, published int, publishedAt time.Time, havePublished bool, now time.Time) int {
	if !havePublished || published <= 0 || now.Sub(publishedAt) > stickyMaxAge {
		return resolved
	}
	if published < resolved {
		return published
	}
	return resolved
}
