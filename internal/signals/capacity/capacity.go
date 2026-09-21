// Package capacity is what a replica can hold and how that is learned: the
// KV-cache capacity a variant's engines report or its deployment implies
// (k1, memory-bound), the compute-bound capacity observed under saturation
// (k2), the window that smooths those observations, and the store that
// keeps a record per variant so a variant with no live replica can still be
// sized.
package capacity

import (
	"time"
)

const (
	// RollingAverageWindowSize is the number of samples retained for compute
	// capacity (k2) history per workload bucket.
	RollingAverageWindowSize = 10

	// StalenessTimeout is the duration after which a stored capacity
	// record is considered stale and should be refreshed from live data.
	StalenessTimeout = 30 * time.Minute

	// EvictionTimeout is the duration after which unused capacity
	// store records are eligible for removal. This is intentionally long
	// because historical capacity knowledge is valuable for zero-replica
	// estimation and cross-variant matching (e.g., a variant may be at
	// zero replicas over a weekend and scale back up Monday).
	EvictionTimeout = 7 * 24 * time.Hour

	// HistoryEvictionTimeout is the duration after which unused k2 history
	// entries are eligible for removal. Shorter than capacity eviction
	// because workload patterns shift and stale k2 observations from a
	// very different workload can mislead scaling decisions.
	HistoryEvictionTimeout = 24 * time.Hour
)

// LearnedFromLive indicates a capacity record was derived from live metrics.
const LearnedFromLive = "live"

// K2Source identifies which priority level produced the compute-bound capacity
// estimate for a replica.
type K2Source int

const (
	K2SrcObserved   K2Source = iota + 1 // queue saturated: tokensInUse
	K2SrcHistorical                     // rolling average from prior observations
	K2SrcDerived                        // estimated from deployment args
	K2SrcFallback                       // fallback to k1 (memory-bound)
)

var K2Labels = map[K2Source]string{
	K2SrcObserved:   "P1-obs",
	K2SrcHistorical: "P2-hist",
	K2SrcDerived:    "P3-k2",
	K2SrcFallback:   "P4-k1",
}
