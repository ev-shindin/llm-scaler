package capacity

import (
	"slices"
	"time"
)

// RollingAverage maintains a fixed-size sliding window of float64 values
// and computes their arithmetic mean. Used to smooth noisy compute-capacity
// (k2) observations over time.
type RollingAverage struct {
	values      []float64
	maxSize     int
	lastUpdated time.Time
}

// NewRollingAverage creates a RollingAverage with the given window size.
func NewRollingAverage(maxSize int) *RollingAverage {
	return &RollingAverage{
		values:      make([]float64, 0, maxSize),
		maxSize:     maxSize,
		lastUpdated: time.Now(),
	}
}

// Add appends a value, evicting the oldest entry if the window is full.
func (r *RollingAverage) Add(value float64) {
	if len(r.values) >= r.maxSize {
		r.values = r.values[1:]
	}
	r.values = append(r.values, value)
	r.lastUpdated = time.Now()
}

// Average returns the arithmetic mean of all stored values, or 0 if empty.
func (r *RollingAverage) Average() float64 {
	if len(r.values) == 0 {
		return 0
	}
	var sum float64
	for _, v := range r.values {
		sum += v
	}
	return sum / float64(len(r.values))
}

// Touch marks the window as used now without adding to it: a reading seen
// again is still the window being observed, and Stale asks about that.
func (r *RollingAverage) Touch() {
	r.TouchAt(time.Now())
}

// TouchAt is Touch at a given time; Touch is TouchAt(time.Now()). A test
// ages a window by it.
func (r *RollingAverage) TouchAt(t time.Time) {
	r.lastUpdated = t
}

// RaiseLast lifts the most recent value to v when v is higher, and touches
// the window either way. A reading that belongs to the last sample's window
// is folded into that sample this way rather than counted or dropped.
func (r *RollingAverage) RaiseLast(v float64) {
	if n := len(r.values); n > 0 && v > r.values[n-1] {
		r.values[n-1] = v
	}
	r.lastUpdated = time.Now()
}

// Last returns the most recent value, or 0 if empty.
func (r *RollingAverage) Last() float64 {
	if n := len(r.values); n > 0 {
		return r.values[n-1]
	}
	return 0
}

// Len returns the number of values currently stored.
func (r *RollingAverage) Len() int {
	return len(r.values)
}

// Max returns the largest stored value, or 0 if empty. The saturated
// throughput window reads this rather than Average: see the saturation
// analyzer's recordSaturatedThroughput for why a completion rate under saturation
// under-reads while the replica is full -- and for the drain at an
// episode's end, which it does not.
func (r *RollingAverage) Max() float64 {
	if len(r.values) == 0 {
		return 0
	}
	return slices.Max(r.values)
}

// Stale reports whether nothing has been added within the timeout.
//
// The saturation analyzer's EvictStaleHistory sweeps whole entries on the
// same measure, but it has no caller on the reconcile path, so a window can outlive the behaviour it
// describes. Callers that fold a fresh observation in check this first: an
// average carried over a long gap is worse than no average, because it looks
// like data and is weighted like data.
func (r *RollingAverage) Stale(timeout time.Duration) bool {
	return time.Since(r.lastUpdated) > timeout
}
