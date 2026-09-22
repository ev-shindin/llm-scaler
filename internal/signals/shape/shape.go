// Package shape is the workload shape a fleet serves -- average input and
// output length and the prefix-cache hit rate -- and a tracker that says
// when it has moved. Signals, not decisions: nothing here scales anything.
package shape

import (
	"math"
)

// DefaultChangeTolerance is the fractional change in IL or OL that
// makes Tracker.Observe report a change; the throughput analyzer then
// clears its observation window and refits its ITL model.
// A value of 0.20 means a 20% shift in either dimension resets calibration.
const DefaultChangeTolerance = 0.20

// Shape captures the stable (IL, OL) characterization for a calibration period.
//
// KVreq = ILeff + OL/2 is the time-averaged per-in-flight-request KV footprint.
// ILeff is used for both N(k*) (current fleet) and N(k_sat) (new replica capacity).
//
// Rationale for using ILeff for new replicas: with cache-aware scheduling (EPP
// prefix routing), a new replica warms up quickly and operates at approximately
// fleet-average ILeff in steady state. Using IL (cold-cache pessimism) would
// chronically over-estimate KV demand per request and lead to over-scaling.
// When prefix caching is disabled, PrefixHitRate=0 and ILeff=IL automatically,
// so the formula is correct in that case without special handling.
type Shape struct {
	// AvgInputTokens is the average prompt length per request in tokens (IL, tok/req).
	AvgInputTokens float64
	// AvgOutputTokens is the average generation length per request in tokens (OL, tok/req).
	AvgOutputTokens float64
	// PrefixHitRate is the prefix cache hit fraction (0.0–1.0).
	// Zero when prefix caching is disabled or hit-rate metrics are unavailable.
	PrefixHitRate float64
	// ILeff is the effective input length after prefix cache reduction:
	//   ILeff = AvgInputTokens × (1 − PrefixHitRate)
	ILeff float64
	// KVreq is the time-averaged KV token footprint per in-flight request:
	//   KVreq = ILeff + AvgOutputTokens/2
	KVreq float64
}

// New constructs a Shape and computes the derived fields.
// hitRate values outside [0, 1] are clamped; NaN is treated as 0.0. il and
// ol are taken as given: a NaN in either is never Within any shape, so a
// tracker fed one reports a change every cycle -- the caller's sanity check
// keeps them out.
func New(il, ol, hitRate float64) Shape {
	if math.IsNaN(hitRate) || hitRate < 0 {
		hitRate = 0
	}
	if hitRate > 1 {
		hitRate = 1
	}
	ileff := il * (1 - hitRate)
	return Shape{
		AvgInputTokens:  il,
		AvgOutputTokens: ol,
		PrefixHitRate:   hitRate,
		ILeff:           ileff,
		KVreq:           ileff + ol/2,
	}
}

// IsZero returns true when the shape has not been populated (zero IL and OL).
func (s Shape) IsZero() bool {
	return s.AvgInputTokens == 0 && s.AvgOutputTokens == 0
}

// Within returns true if both IL and OL of s are within ±tolerance of other.
// tolerance is fractional: 0.20 means ±20%.
func (s Shape) Within(other Shape, tolerance float64) bool {
	return withinTolerance(s.AvgInputTokens, other.AvgInputTokens, tolerance) &&
		withinTolerance(s.AvgOutputTokens, other.AvgOutputTokens, tolerance)
}

// withinTolerance returns true if |a - b| / b ≤ tolerance, or if both are zero.
func withinTolerance(a, b, tolerance float64) bool {
	if b == 0 {
		return a == 0
	}
	return math.Abs(a-b)/b <= tolerance
}
