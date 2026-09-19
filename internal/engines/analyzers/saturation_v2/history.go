package saturation_v2

import (
	"slices"
	"time"
)

// rollingAverage maintains a fixed-size sliding window of float64 values
// and computes their arithmetic mean. Used to smooth noisy compute-capacity
// (k2) observations over time.
type rollingAverage struct {
	values      []float64
	maxSize     int
	lastUpdated time.Time
}

// newRollingAverage creates a rollingAverage with the given window size.
func newRollingAverage(maxSize int) *rollingAverage {
	return &rollingAverage{
		values:      make([]float64, 0, maxSize),
		maxSize:     maxSize,
		lastUpdated: time.Now(),
	}
}

// Add appends a value, evicting the oldest entry if the window is full.
func (r *rollingAverage) Add(value float64) {
	if len(r.values) >= r.maxSize {
		r.values = r.values[1:]
	}
	r.values = append(r.values, value)
	r.lastUpdated = time.Now()
}

// Average returns the arithmetic mean of all stored values, or 0 if empty.
func (r *rollingAverage) Average() float64 {
	if len(r.values) == 0 {
		return 0
	}
	var sum float64
	for _, v := range r.values {
		sum += v
	}
	return sum / float64(len(r.values))
}

// Contains reports whether value is already in the window, exactly.
func (r *rollingAverage) Contains(value float64) bool {
	return slices.Contains(r.values, value)
}

// Touch marks the window as used now without adding to it: a reading seen
// again is still the window being observed, and Stale asks about that.
func (r *rollingAverage) Touch() {
	r.lastUpdated = time.Now()
}

// Len returns the number of values currently stored.
func (r *rollingAverage) Len() int {
	return len(r.values)
}

// Max returns the largest stored value, or 0 if empty. The saturated
// throughput window reads this rather than Average: see
// recordSaturatedThroughput for why a completion rate under saturation can
// only under-read.
func (r *rollingAverage) Max() float64 {
	if len(r.values) == 0 {
		return 0
	}
	return slices.Max(r.values)
}

// Stale reports whether nothing has been added within the timeout.
//
// EvictStaleHistory sweeps whole entries on the same measure, but it has no
// caller on the reconcile path, so a window can outlive the behaviour it
// describes. Callers that fold a fresh observation in check this first: an
// average carried over a long gap is worse than no average, because it looks
// like data and is weighted like data.
func (r *rollingAverage) Stale(timeout time.Duration) bool {
	return time.Since(r.lastUpdated) > timeout
}
