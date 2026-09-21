package shape

// Tracker maintains the current workload shape bucket for one variant and
// detects when the workload has shifted enough that a model fitted on the old
// shape should be refitted.
//
// The shape is characterised by the variant-average (IL, OL) across all replicas.
// A shape change is declared when either IL or OL deviates from the stored shape
// by more than the configured tolerance fraction.
type Tracker struct {
	current   Shape
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
// the tolerance band of the stored shape. The stored shape is updated to the new
// value regardless.
func (t *Tracker) Observe(il, ol, hitRate float64) (shape Shape, changed bool) {
	next := New(il, ol, hitRate)

	if !t.hasShape {
		t.current = next
		t.hasShape = true
		return next, false
	}

	changed = !next.Within(t.current, t.tolerance)
	t.current = next
	return next, changed
}

// Current returns the most recently stored shape and whether any shape has been
// observed yet. Returns (zero Shape, false) before the first Observe call.
func (t *Tracker) Current() (Shape, bool) {
	return t.current, t.hasShape
}

// Reset clears the stored shape, as if the tracker had just been created.
// The next Observe call will set a fresh shape without triggering a change event.
func (t *Tracker) Reset() {
	t.current = Shape{}
	t.hasShape = false
}
