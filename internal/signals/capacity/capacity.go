// Package capacity is what a replica can hold and how that is learned: the
// KV-cache capacity a variant's engines report or its deployment implies
// (k1, memory-bound), the compute-bound capacity observed under saturation
// (k2), the window that smooths those observations, and the store that
// keeps a record per variant so a variant with no live replica can still be
// sized.
package capacity

// LearnedFromLive indicates a capacity record was derived from live metrics.
const LearnedFromLive = "live"

// K2Source identifies which priority level produced the compute-bound capacity
// estimate for a replica.
type K2Source int

// The K2 sources, in priority order: the analyzer takes the first that yields.
const (
	K2SrcObserved   K2Source = iota + 1 // queue saturated: tokensInUse
	K2SrcHistorical                     // rolling average from prior observations
	K2SrcDerived                        // estimated from deployment args
	K2SrcFallback                       // fallback to k1 (memory-bound)
)

// K2Labels is the log label of each K2Source; the labels are what the k2
// decision log line and the analyzer result's Reason carry.
var K2Labels = map[K2Source]string{
	K2SrcObserved:   "P1-obs",
	K2SrcHistorical: "P2-hist",
	K2SrcDerived:    "P3-k2",
	K2SrcFallback:   "P4-k1",
}
