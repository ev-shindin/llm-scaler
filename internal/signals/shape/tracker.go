package shape

// Tracker maintains the current workload shape bucket for one variant and
// detects when the workload has shifted enough that a model fitted on the old
// shape should be refitted.
//
// The shape is characterised by the variant-average (IL, OL) across all replicas.
// A shape change is declared when either IL or OL deviates from the ANCHOR — the
// reading the current tolerance band is measured from — by more than the
// configured tolerance fraction.
//
// The anchor is why there are two fields. Measuring the band from the previous
// READING instead makes the tolerance a per-cycle rate limit rather than a band:
// a workload that slides from 5900-token outputs to 1000 over ten minutes moves
// only a few percent between two 15-second cycles, so no cycle ever exceeds 20%
// and the change is never declared, though the cumulative move is 83%. That is
// not hypothetical — it is what a 1k/6000 → 8k/1000 swap did on 2026-09-23,
// leaving the throughput window keyed to the outputs the fleet had stopped
// serving for the remaining nineteen minutes of the run, and the demand floor
// dividing the arrival rate by a μ worth roughly a third of the truth.
//
// Not safe for concurrent use: the owner serialises access.
type Tracker struct {
	current Shape
	// anchor is the shape the tolerance band is measured from. It moves only
	// when a change is declared, so successive small moves accumulate against
	// a fixed reference instead of resetting it.
	anchor    Shape
	hasShape  bool
	tolerance float64
}

// NewTracker creates a Tracker with the given fractional tolerance.
// For example, tolerance=0.20 means a ≥20% change in IL or OL triggers a reset.
func NewTracker(tolerance float64) *Tracker {
	return &Tracker{tolerance: tolerance}
}

// Observe updates the tracker with the variant-averaged (il, ol, hitRate) for
// this reconcile cycle and reports whether the shape bucket changed.
//
// On the first call (no prior shape), the shape is set and changed=false is
// returned — there is nothing to refit against yet.
//
// On subsequent calls, changed=true is returned when the new shape falls outside
// the tolerance band of the ANCHOR, and the anchor then moves to this reading —
// so a workload that drifts declares a change each time it has moved a further
// tolerance from where it was last declared, rather than never declaring one.
// The most recent reading is stored either way and is what Current returns.
func (t *Tracker) Observe(il, ol, hitRate float64) (shape Shape, changed bool) {
	next := New(il, ol, hitRate)

	if !t.hasShape {
		t.current = next
		t.anchor = next
		t.hasShape = true
		return next, false
	}

	changed = !next.Within(t.anchor, t.tolerance)
	if changed {
		t.anchor = next
	}
	t.current = next
	return next, changed
}

// Current returns the most recently observed shape and whether any shape has
// been observed yet. Returns (zero Shape, false) before the first Observe call.
//
// This is the latest reading, not the anchor: a caller asking what the fleet is
// serving wants the measurement, and a caller asking whether that has changed
// calls Observe, which answers against the anchor.
func (t *Tracker) Current() (Shape, bool) {
	return t.current, t.hasShape
}

// Reset clears the stored shape, as if the tracker had just been created.
// The next Observe call will set a fresh shape without triggering a change event.
func (t *Tracker) Reset() {
	t.current = Shape{}
	t.anchor = Shape{}
	t.hasShape = false
}
