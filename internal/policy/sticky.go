// Package policy is what happens between a plan and an order.
//
// The optimizer says how many replicas the load implies; these are the rules
// that decide what is actually published: a scale-down held against the
// chatter of a fleet that is still settling, the reasons a model may not be
// scaled to zero, whether a cluster-wide inventory read is worth its cost,
// and the throttling of the policy lines an operator reads. Each is a
// function of its inputs, with no engine state behind it, so each can be
// read and tested on its own.
//
// Not to be confused with internal/warmpool/policy, which decides what the
// warm pool does with the Pods it holds. This package is the scaling
// pipeline's; a file that needs both imports one under an alias.
package policy

import (
	"fmt"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// DefaultMaxAge is the floor on how long ago the cycle that DECIDED a
// published value may have run for the value still to be held or carried;
// the engine's stickyAge raises it with the optimize interval. A carry that
// republishes through a metrics outage must stop somewhere, or an operator's
// manual scale-up during the outage would be undone by the HPA when its
// window closed. The age is the last deciding cycle's (the engine's
// lastDecided), not the store's write time, which the carry itself
// refreshes. Past this, the no-decision path publishes the running count as
// it always did.
const DefaultMaxAge = 5 * time.Minute

// stickyStepName is the pipeline step HoldPublishedScaleDown records.
const stickyStepName = "sticky-scale-down"

// carryMaxCycles and carryMaxAge bound the carry: a held value is
// republished through at most this many consecutive no-decision cycles and
// for at most this long after the deciding cycle -- STRICTLY under KEDA's
// default 300 s scale-down window. The carry covers a scrape gap, not an
// outage: past it the no-decision path publishes the running count as it
// always did, so an operator who scaled the fleet by hand while metrics were
// gone finds the HPA's window filled with the running count, not with a held
// value that would undo the change the moment the carry stopped.
//
// The age bound is fixed on purpose, where the hold's follows the optimize
// interval: it is the HPA's window that sets it, not the loop's cadence. So
// the carry is cadence-limited -- it covers floor(4 min / interval) cycles,
// four at the shipped 15 s, one at 4 min, none above that -- and a loop
// slower than the window cannot be carried through a gap at all. That is
// the physics of a max-window actuator, and a longer carry would only trade
// it for undoing the operator's changes.
const (
	carryMaxCycles = 4
	carryMaxAge    = 4 * time.Minute
)

// stickyReason is the decision reason a held scale-down carries. It reaches
// the ScaledDown event and the OptimizationReady condition, and it is
// constant on purpose: the API server aggregates only identical event
// messages, and a hold that lasts KEDA's window at a 15 s cycle would
// otherwise leave ~20 distinct events per descent. The numbers go in the
// pipeline step and the log line.
const stickyReason = "held the published scale-down: the fresh target crept back up while utilization at the published count stays under the scale-up threshold"

// HoldPublishedScaleDown keeps a scale-down that WVA has already published
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
// not have that effect: CarryPublished republishes the held value there.
//
// On by default (WVA_STICKY_SCALE_DOWN); the switch exists to turn it off for
// a comparison, since off is the behaviour this replaces.
//
// Reports whether it changed the decision. Inert without a published value,
// with one older than maxAge (the engine's stickyAge) or below the variant's own
// floor, without a descent in flight, or when the decision carries no
// capacity (a path that did not go through the optimizer's decision builder).
func HoldPublishedScaleDown(d domain.VariantDecision, published int, publishedAt time.Time, havePublished bool, maxAge time.Duration, now time.Time) (domain.VariantDecision, bool) {
	if !havePublished || published <= 0 {
		return d, false
	}
	if now.Sub(publishedAt) > maxAge {
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
	if d.TargetReplicas > d.CurrentReplicas {
		// A scale-up is never held. On a single variant the release test
		// below would let it through anyway (a target above the running count
		// means demand above the scale-up threshold of the running supply, let
		// alone of the published one); on a model with several variants the
		// optimizer adds replicas where they are cheapest per capacity, and
		// that variant's own share of the demand can sit under the threshold
		// while the model as a whole is short -- so the share cannot be the
		// judge of a scale-up, and the optimizer's answer stands.
		return d, false
	}
	if d.PerReplicaCapacity <= 0 || d.ScaleUpThreshold <= 0 || d.TotalDemand < 0 {
		return d, false // nothing to price the published count with
	}
	if d.WasLimited {
		// A target a GPU limiter bound is the limiter's answer, and its reason
		// is what the ResourceConstrained event has to carry. The hold would
		// overwrite that reason with its own and lower a count the limiter
		// already priced; it stands down.
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

// CarryPublished is the no-decision counterpart of the hold: a cycle with
// nothing to say for a variant republishes what it would otherwise have
// resolved -- the previous desired from status, or the running count when
// the variant has no status, which every variant synthesized from a
// ScaledObject lacks -- and the running count is exactly the value that
// re-arms KEDA's window mid-descent. When a fresh published value is lower,
// it is republished instead: a cycle with no metrics cannot justify raising
// what the last cycle with metrics lowered. A floor the variant has since
// been given stands above the carried value, as it does above the hold's.
//
// read says whether resolved was actually read -- from status, the
// allocations or the scale target -- rather than left at the 0 it starts as
// when every read failed. The two zeros must not be confused: a READ 0 is a
// fleet somebody scaled to zero by hand, which must be published as 0 or the
// external scaler reports it active and KEDA wakes what the operator just
// parked; an UNREAD 0 is a transient API error, and publishing it would park a
// live model, so a fresh held value is republished over it. missed is how
// many consecutive cycles have had no decision, counting this one. Returns
// the value to publish.
func CarryPublished(resolved int, read bool, published int, publishedAt time.Time, havePublished bool, floor *int, missed int, now time.Time) int {
	if !havePublished || published <= 0 {
		return resolved
	}
	if missed > carryMaxCycles || now.Sub(publishedAt) > carryMaxAge {
		return resolved
	}
	if floor != nil && published < *floor {
		return resolved
	}
	if !read || published < resolved {
		return published
	}
	return resolved
}
