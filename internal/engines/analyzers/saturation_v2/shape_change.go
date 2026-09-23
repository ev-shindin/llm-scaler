package saturation_v2

import (
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/shape"
)

// The fleet's shape is (I, O): the prompt length arriving and the generation
// length in flight. Every capacity figure this analyzer learns is keyed by a
// shape bucket, and each is a reading of the shape the fleet was serving when
// it was taken. When the shape moves, those figures describe a workload that
// is no longer running -- and this analyzer's only shape input was each
// replica's own average output length over its recent completions, a LAGGING
// signal: a completion ends O x ITL after the request arrived, ~60 s at 6000
// tokens.
//
// Measured on run biran-20260921-235843-225 (1000/6000 -> 8000/1000 at 6
// req/s, 2026-09-21), whose trace switches shape at t = 1100 s = 18.3 min:
//
//	signal                                    first reads the new shape
//	arriving prompt length (this file)        18.7 min  (+22 s)
//	the decode target actually moving         19.8 min  (+1.5 min)
//	saturatedThroughput for the new bucket    19.9 min  (+1.6 min, and wrong)
//
// The arriving prompt length is available within a scrape of the switch
// because it waits for nothing to complete. On that run it read 5838
// bytes/request through phase 1 and 45626 through phase 2 -- a 7.8x step
// against the 8x the trace applies, on the cycle after the switch, with no
// ambiguity anywhere in between.
//
// What the event buys is not the order -- occupancy still orders, and sooner
// than any of these -- but the two errors that come of acting on a figure the
// switch has made stale: ordering on the old shape's mu (the same run asked
// for 142 replicas against 5.68 req/s arriving, on a mu of 0.039928 req/s
// carried over from a bucket the fleet had left), and RELEASING on the old
// shape's occupancy before the new one has been measured.
//
// The detector is signals/shape's Tracker, which the throughput analyzer
// already drives over its own (IL, OL): a fractional tolerance on either axis,
// not a bucket crossing, so it does not inherit the bucket table that item 1
// of the traffic shape-shift proposal removes. This is item 2 of that
// proposal, less the per-role split -- the hold is the fleet's, as the
// proposal states it, and fleetOutputLength is already model-level.
//
// That proposal is not on main yet; it is under review, and the path is left
// out here deliberately so hack/check-doc-links.py does not report a mention
// of a file the tree does not have.

// shapeMemo is one model's shape tracker and the state of its outstanding
// change. The tracker is not safe for concurrent use; a.mu serialises it.
type shapeMemo struct {
	tracker *shape.Tracker
	// stable is the shape the throughput keys are built from. It follows the
	// tracker only when the tracker reports a CHANGE, which is what keeps a
	// fleet off a bucket boundary.
	//
	// Measured on the rerun of the 1000/6000 -> 8000/1000 trace (2026-09-22,
	// biran-pd, on the build that carries #85): phase 1 generates exactly
	// 6000-token outputs, which is the xxlong/huge boundary, and the fleet's
	// rate-weighted average wobbled either side of it. The whole fleet shares
	// one throughput key since #85, so the wobble moved all of it at once --
	// mu alternated 2.36 and 0.82 req/s cycle to cycle, a 2.9x swing, and the
	// decode target with it, 10 -> 4 -> 10 -> 4 every 30-45 s for the length
	// of the phase. Only KEDA's scale-down stabilization window kept the
	// fleet itself from following. Before #85 the replicas straddled the
	// boundary independently and the median smoothed it; one key per fleet
	// made the edge total, which is the cost of that fix and this is its
	// other half.
	//
	// A tolerance, not hysteresis on the boundary: the tracker already says
	// when the shape has genuinely moved, and any shape that has not moved by
	// DefaultChangeTolerance keeps the key it was learned under, wherever the
	// boundaries happen to fall. Item 1 of the proposal removes the
	// boundaries; until then this removes their edge.
	stable shape.Shape
	// lastArriving is the previous queue-derived prompt length, kept so that
	// reading is compared against itself rather than against the replicas'.
	lastArriving float64
	// changedAt is when the outstanding change was raised, zero when none is.
	changedAt time.Time
	// lastSeen is when this model was last analyzed, so the memo can be swept
	// with the rest of the analyzer's per-model state (EvictStaleHistory).
	lastSeen time.Time
}

// arrivingPromptLength is the prompt length, in tokens, of what is
// ARRIVING, from the scheduler queue; ok is false when the queue is empty,
// which it is on most cycles of a fleet that is keeping up (141 of the
// run's 171).
//
// It is NOT on the same scale as servedPromptLength below: BytesPerToken is
// 4 and the run measured ~5.8 bytes per prompt token, so this reads about
// 46 % high -- 1459 tokens for the trace's 1000. The two are therefore each
// compared against their own previous value and never against each other;
// feeding one tracker from whichever was available made the queue merely
// emptying look like a shape change, and held the fleet for it.
//
// The scheduler's queue is the early source: the EPP reports the queue's size
// and its bytes, and bytes / size is the prompt length of what has not been
// dispatched yet. It leads every other signal because a queued request has
// been measured but not yet served.
//
// The queue is empty on most cycles of a fleet that is keeping up -- 141 of
// 171 on the run above -- and empty is not a reading, so the replicas'
// AvgInputTokens carries it in between, weighted by request rate exactly as
// fleetOutputLength weights the output half, so a fresh replica with a handful
// of completions barely moves it. That fallback lags, being an average over
// completed requests; the cycles where the queue is NOT empty are the cycles
// where the shape is moving or the fleet is behind, which is when the lead
// matters.
//
// Note BytesPerToken is 4 and the run measured ~5.8 bytes per prompt token, so
// the queue estimate reads about 45 % high in absolute terms. It is compared
// against the previous cycle's figure through a fractional tolerance, so a
// constant bias cancels; correcting the constant would move the demand
// estimates that also divide by it, and is not this change's business.
func arrivingPromptLength(sq *domain.SchedulerQueueMetrics) (float64, bool) {
	if sq == nil || sq.QueueSize <= 0 || sq.QueueBytes <= 0 {
		return 0, false
	}
	return float64(sq.QueueBytes) / float64(sq.QueueSize) / BytesPerToken, true
}

// servedPromptLength is the prompt length of what the replicas have been
// SERVING: the other axis of fleetAverage, so a fresh replica with a handful
// of completions barely moves it. It lags -- it is an average over completed
// requests -- and it is on the engines' scale, not the queue's.
func servedPromptLength(replicas []domain.ReplicaMetrics) float64 {
	return fleetAverage(replicas,
		func(rm domain.ReplicaMetrics) float64 { return rm.AvgInputTokens },
		func(rm domain.ReplicaMetrics) bool { return !rm.FromWarmPool })
}

// saturatedCompletionRate is what one replica completes per second while
// saturated. For a role that generates, it is priced from the tokens it is
// GENERATING rather than the requests it is finishing.
//
// RequestRate is rate(vllm:request_generation_tokens_count), a count of
// COMPLETIONS, and completions are bursty in exactly the way that breaks a max
// window. Sequences admitted together finish together, so when a batch drains
// the completion rate spikes far above anything the replica sustains, the
// queue gate is a one-minute max and is still up while the burst is fresh, and
// the window -- which keeps a max and never lowers a reading -- carries the
// burst for the rest of the phase. throughput_floor.go's own header records
// this as open and names this fix.
//
// Measured on the 2026-09-22 A/B, phase 1 (1000/6000 at 6 req/s). The window
// settled on 1.7 req/s per replica; the fleet sustained 6 req/s across 6.7
// replicas, which is 0.9. Sized on 1.7 the floor asked for 3.5 replicas where
// the shape needs about 8, and the decode queue ran at a mean of 16.2 against
// a threshold of 5 for the whole phase -- sustained saturation. The run before
// it, whose mu alternated across a bucket boundary, happened to substitute
// 0.82 on half its cycles and so kept a large enough fleet by accident; that
// accident is what removing the boundary flip took away.
//
// Generated tokens do not burst on a drain: a replica emits them at the rate
// its batch allows whether or not any sequence happens to finish. Tokens per
// second over the shape's output length is therefore the same figure the
// completion rate is trying to be, measured where it is steady:
//
//	mu_req = GenerationTokenRate / O
//
// O is the FLEET's output length, the same figure the window is keyed by, so
// the rate and the key describe one shape.
//
// Returns false when either term is missing, and the caller records nothing:
// a window holding one definition of mu and then another would take the max of
// the two, which is the burst again. A role with no window gets no floor and
// answers to occupancy, which is the documented behaviour for a fleet that has
// never been seen saturated.
func saturatedCompletionRate(rm domain.ReplicaMetrics, role string, fleetOutput float64) (float64, bool) {
	// Prefill emits about one token per request -- its work is the prompt, not
	// the generation -- so tokens over an output length is not its completion
	// rate and would read three orders of magnitude low. It keeps the
	// completion rate, which is also where the burst this replaces does not
	// arise: a drain burst is a batch of long GENERATIONS finishing together,
	// and prefill holds nothing that long. Prefill's own counterpart is prompt
	// tokens per second, from prompt_tokens_total keyed by input length, which
	// the collector does not gather today (item 1 of the proposal).
	if canonicalRole(role) == domain.RolePrefill {
		return rm.RequestRate, rm.RequestRate > 0
	}
	if rm.GenerationTokenRate <= 0 || fleetOutput <= 0 {
		return 0, false
	}
	return rm.GenerationTokenRate / fleetOutput, true
}

// noteFleetShape folds this cycle's (I, O) into the model's tracker and
// reports whether a change is outstanding: raised on this cycle or an earlier
// one and not yet settled by settleFleetShape.
//
// A cycle that can measure neither axis leaves the tracker alone rather than
// feeding it a zero, which Tracker.Within would read as a change away from
// every non-zero shape and then a change back. The prefix hit rate is passed
// as zero because Within compares IL and OL only; nothing here reads the
// derived ILeff or KVreq.
//
// Logged once per change, at INFO: the event is rare and it explains every
// held decision that follows, which is the first thing a reader of those
// decisions will ask.
func (a *SaturationAnalyzer) noteFleetShape(namespace, modelID string, in, out float64,
	arriving float64, arrivingOK bool, logger logr.Logger) (float64, bool) {
	if !(in > 0) && !(out > 0) && !arrivingOK {
		stable, outstanding := a.fleetShapeState(namespace, modelID)
		if stable <= 0 {
			stable = out
		}
		return stable, outstanding
	}
	key := namespace + "|" + modelID

	a.mu.Lock()
	defer a.mu.Unlock()
	memo, ok := a.fleetShape[key]
	if !ok {
		memo = &shapeMemo{tracker: shape.NewTracker(shape.DefaultChangeTolerance)}
		a.fleetShape[key] = memo
	}
	was, hadShape := memo.tracker.Current()
	now := a.now()
	memo.lastSeen = now
	// An axis nobody reported this cycle is MISSING, not zero, and Within
	// measures any stored non-zero value against a zero as a change of a
	// hundred per cent. A scrape gap, a rolling restart, or simply a cycle in
	// which no ready replica has completed anything would otherwise raise a
	// change and hold the fleet from release for ShapeChangeHoldMax on a
	// workload that never moved. Carrying the last known value forward says
	// "no new information on this axis", which is what the cycle actually is.
	//
	// The reset below handles the other direction, an axis being learned for
	// the first time; between them every transition through zero is covered.
	if hadShape {
		if out <= 0 && was.AvgOutputTokens > 0 {
			out = was.AvgOutputTokens
		}
		if in <= 0 && was.AvgInputTokens > 0 {
			in = was.AvgInputTokens
		}
	}
	// An axis reading zero is a fleet that has nothing to report on it yet,
	// not a workload of zero-length generations, and Within treats any
	// non-zero value as outside a stored zero. So the first cycle whose
	// completions give an output length would otherwise read as a change away
	// from a shape that was never measured. Observed on the 2026-09-22 rerun:
	// `outputTokensWas: 0, outputTokensNow: 6000` at 19:20:37, 70 s into the
	// run, one cycle after the first replica completed anything. It cost
	// nothing there -- the fleet was ramping, so its demand was above the
	// no-release floor and no hold applied -- but it would fire on every
	// controller restart, and a restart during a steady phase is exactly when
	// a spurious hold would suppress a real scale-down.
	//
	// Reset rather than suppress: the stored shape is half-measured, and the
	// tracker's own contract for a first reading (set it, report no change) is
	// what this cycle actually is.
	if hadShape && ((was.AvgOutputTokens == 0 && out > 0) || (was.AvgInputTokens == 0 && in > 0)) {
		memo.tracker.Reset()
		hadShape = false
	}
	next, changed := memo.tracker.Observe(in, out, 0)
	// The arriving prompt is the early half, and the only one that moves
	// before anything completes. Compared against the last reading from
	// the SAME source, so the scale difference cannot raise anything.
	if arrivingOK {
		if prev := memo.lastArriving; prev > 0 {
			if delta := arriving - prev; delta > prev*shape.DefaultChangeTolerance ||
				-delta > prev*shape.DefaultChangeTolerance {
				changed = true
			}
		}
		memo.lastArriving = arriving
	}
	// The keys move only on a change, so a shape that merely wobbles keeps
	// the window it was learned under.
	if changed || memo.stable.IsZero() {
		memo.stable = next
	}
	stableOut := memo.stable.AvgOutputTokens
	if stableOut <= 0 {
		stableOut = out
	}

	switch {
	case changed:
		memo.changedAt = now
	case !memo.changedAt.IsZero() && now.Sub(memo.changedAt) >= ShapeChangeHoldMax:
		// The backstop. A fleet that is over-provisioned for the new shape
		// never saturates under it, so it never records the reading that
		// settles the hold: on the run above, phase 2 ran 8-21 requests
		// across 11 replicas with nothing queued, and an unbounded hold would
		// have pinned all 11 for its remaining 16 minutes. After this the
		// fleet answers to occupancy again, which by then is a reading of the
		// new shape whether or not it ever saturated under it.
		memo.changedAt = time.Time{}
	}
	if !changed {
		return stableOut, !memo.changedAt.IsZero()
	}
	logger.Info("fleet-shape-change",
		"modelID", modelID, "namespace", namespace,
		"inputTokensWas", was.AvgInputTokens, "inputTokensNow", next.AvgInputTokens,
		"outputTokensWas", was.AvgOutputTokens, "outputTokensNow", next.AvgOutputTokens,
		"arrivingPromptTokens", arriving, "arrivingRead", arrivingOK,
		"hadShape", hadShape, "tolerance", shape.DefaultChangeTolerance,
		"reason", "the shape the capacity figures were learned under is no longer the one arriving; the fleet is not released until the new shape has a reading of its own")
	return stableOut, true
}

// settleFleetShape clears an outstanding change once the fleet has MEASURED
// itself under the new shape.
//
// One replica reading its own bucket was the whole condition and it was too
// weak: computeReplicaCapacity records a sample and reads the window back in
// the same call, so the first saturated cycle after a switch both wrote the
// new bucket's first sample and satisfied the test -- the hold lasted one
// cycle. Worse, that first sample is the one most likely to be wrong: on an
// O-down switch the replicas are still draining generations of the OLD length
// while the new, shorter O is already the divisor, so the reading is inflated
// by the ratio of the two. The hold existed to reject exactly that reading and
// was being cleared by it.
//
// The condition is now the one the floor itself uses to trust a window with an
// order: MinThroughputSamplesToOrder readings of its own, which are a
// ThroughputSampleSpacing apart by construction, so the second cannot come
// from the same drain as the first.
func (a *SaturationAnalyzer) settleFleetShape(namespace, modelID string, ownReading bool) {
	if !ownReading {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if memo, ok := a.fleetShape[namespace+"|"+modelID]; ok {
		memo.changedAt = time.Time{}
	}
}

// fleetShapeState reports the stable output length the keys are built from
// and whether a change is outstanding, without observing anything.
func (a *SaturationAnalyzer) fleetShapeState(namespace, modelID string) (float64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	memo, ok := a.fleetShape[namespace+"|"+modelID]
	if !ok {
		return 0, false
	}
	return memo.stable.AvgOutputTokens, !memo.changedAt.IsZero()
}

// holdFleetFloor raises every role's demand to the bottom of the band where
// the engine does not release -- scaleDown x the role's own supply, the demand
// at which applyUniversalThreshold's SC is zero -- and returns how much the
// model-level total must move to follow, with the roles it raised.
//
// A floor, not the clamp holdPrefillDemand applies: a shape change says the
// figures are stale, not that they are high, and an I-up switch genuinely
// needs more capacity. Occupancy and the throughput floor order as they always
// did; only the release is withheld, and only until the new shape has been
// measured.
//
// Each role is floored against its OWN supply, so a fleet already above the
// band in one role and below it in another is left correct in both.
func holdFleetFloor(roleDemand map[string]float64, variants []domain.VariantCapacity, scaleDown float64) (float64, map[string]float64) {
	if roleDemand == nil || scaleDown <= 0 {
		return 0, nil
	}
	byRole := aggregation.AggregateByRole(variants)
	var moved float64
	raised := make(map[string]float64)
	for role, demand := range roleDemand {
		rc, ok := byRole[role]
		if !ok || rc.TotalSupply <= 0 {
			continue
		}
		if floorTo := scaleDown * rc.TotalSupply; demand < floorTo {
			roleDemand[role] = floorTo
			moved += floorTo - demand
			raised[role] = floorTo
		}
	}
	if len(raised) == 0 {
		return 0, nil
	}
	// The variants' own figures follow the role's, for the reason
	// holdPrefillDemand gives: the optimizer prices each variant by its share
	// of the role demand, and a raw variant figure would leave the hold to be
	// split by the measurement the hold has just declared stale.
	for i := range variants {
		vc := &variants[i]
		held, ok := raised[canonicalRole(vc.Role)]
		if !ok {
			continue
		}
		rc := byRole[canonicalRole(vc.Role)]
		supply := float64(vc.ReplicaCount) * vc.PerReplicaCapacity
		if supply <= 0 || rc.TotalSupply <= 0 {
			continue
		}
		if share := held * supply / rc.TotalSupply; share > vc.TotalDemand {
			vc.TotalDemand = share
			vc.Utilization = share / supply
		}
	}
	return moved, raised
}

// logShapeHold reports a hold that changed a decision. Silent when it did not:
// a fleet already above the band is the normal case for the cycles right after
// an I-up switch, and a line every cycle would bury the ones that matter.
func logShapeHold(logger logr.Logger, modelID, namespace string, moved float64, raised map[string]float64) {
	if len(raised) == 0 {
		return
	}
	logger.V(logging.DEFAULT).Info("fleet-shape-hold",
		"modelID", modelID, "namespace", namespace,
		"raisedByRole", raised, "totalDemandMoved", moved,
		"reason", "a shape change is outstanding: the fleet is not released on figures learned under the shape it has left")
}
