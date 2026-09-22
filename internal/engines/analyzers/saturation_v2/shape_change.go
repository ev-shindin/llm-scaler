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
// of docs/proposals/shape-shift-treatment.md removes. This is item 2 of that
// proposal, less the per-role split -- the hold is the fleet's, as the
// proposal states it, and fleetOutputLength is already model-level.

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
	// changedAt is when the outstanding change was raised, zero when none is.
	changedAt time.Time
}

// fleetInputLength is the prompt length, in tokens, of what is ARRIVING.
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
func fleetInputLength(sq *domain.SchedulerQueueMetrics, replicas []domain.ReplicaMetrics) float64 {
	if sq != nil && sq.QueueSize > 0 && sq.QueueBytes > 0 {
		return float64(sq.QueueBytes) / float64(sq.QueueSize) / BytesPerToken
	}
	var weighted, weights, plain float64
	var n int
	for _, rm := range replicas {
		if rm.AvgInputTokens <= 0 || rm.FromWarmPool {
			continue
		}
		plain += rm.AvgInputTokens
		n++
		if rm.RequestRate > 0 {
			weighted += rm.AvgInputTokens * rm.RequestRate
			weights += rm.RequestRate
		}
	}
	if weights > 0 {
		return weighted / weights
	}
	if n > 0 {
		return plain / float64(n)
	}
	return 0
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
func (a *SaturationAnalyzer) noteFleetShape(namespace, modelID string, in, out float64, logger logr.Logger) (float64, bool) {
	if !(in > 0) && !(out > 0) {
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
	next, changed := memo.tracker.Observe(in, out, 0)
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
		"hadShape", hadShape, "tolerance", shape.DefaultChangeTolerance,
		"reason", "the shape the capacity figures were learned under is no longer the one arriving; the fleet is not released until the new shape has a reading of its own")
	return stableOut, true
}

// settleFleetShape clears an outstanding change once the fleet has a
// throughput reading taken under the NEW shape. One replica reading its own
// bucket rather than borrowing a neighbour's is the whole condition, and
// floor.Estimate already distinguishes the two, so nothing here scans windows.
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
