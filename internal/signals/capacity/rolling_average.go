package capacity

import (
	"slices"
	"time"
)

const (
	// RollingAverageWindowSize is the number of samples retained for compute
	// capacity (k2) history per workload bucket, and for the saturated
	// completion-rate (mu) windows recorded beside it.
	RollingAverageWindowSize = 10

	// HistoryEvictionTimeout is the duration after which unused k2 history
	// entries -- and the mu windows keyed beside them -- are eligible for
	// removal. Shorter than the capacity store's EvictionTimeout because
	// workload patterns shift and stale k2 observations from a very
	// different workload can mislead scaling decisions.
	HistoryEvictionTimeout = 24 * time.Hour
)

// RollingAverage maintains a fixed-size sliding window of float64 values.
// The saturation analyzer keeps one per history key for two readings: the
// compute-bound capacity (k2), read through Average, and the saturated
// completion rate (mu) the throughput floor prices from, read through Max
// (see Max for why the two differ).
//
// Not safe for concurrent use: the owner serialises access (the saturation
// analyzer holds its own mutex around every window).
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

// Last returns the most recent value, or 0 if empty. No production caller;
// the tests read it.
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

// Stale reports whether nothing has touched the window within the timeout:
// creation, Add, RaiseLast and Touch all count, so a window that was
// created and never added to is fresh for one timeout.
//
// The saturation analyzer's EvictStaleHistory sweeps whole entries on the
// same measure, but it has no caller on the reconcile path, so a window can outlive the behaviour it
// describes. Callers that fold a fresh observation in check this first: an
// average carried over a long gap is worse than no average, because it looks
// like data and is weighted like data.
func (r *RollingAverage) Stale(timeout time.Duration) bool {
	return time.Since(r.lastUpdated) > timeout
}
