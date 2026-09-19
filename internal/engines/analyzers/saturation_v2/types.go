package saturation_v2

import "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"

// learnedFromLive indicates a capacity record was derived from live metrics.
const learnedFromLive = "live"

// k2Source identifies which priority level produced the compute-bound capacity
// estimate for a replica.
type k2Source int

const (
	k2SrcObserved   k2Source = iota + 1 // queue saturated: tokensInUse
	k2SrcHistorical                     // rolling average from prior observations
	k2SrcDerived                        // estimated from deployment args
	k2SrcFallback                       // fallback to k1 (memory-bound)
)

var k2Labels = map[k2Source]string{
	k2SrcObserved:   "P1-obs",
	k2SrcHistorical: "P2-hist",
	k2SrcDerived:    "P3-k2",
	k2SrcFallback:   "P4-k1",
}

// k2ReasonObsImplausible labels the diagnostic emitted when an observation is
// discarded for exceeding the KV cache's physical ceiling. It is deliberately
// not a k2Source: no capacity comes from it -- the analyzer falls through to
// the next priority -- so it must not appear where a k2Source label is
// expected. It shares the P-prefix vocabulary because it is read from the same
// k2-decision log line.
const k2ReasonObsImplausible = "P1-obs-invalid"

// k2ReasonObsDownstream labels the diagnostic emitted when a prefill
// replica's saturated queue is left unrecorded because the decode role is
// saturated in the same cycle: a prefill request completes only when decode
// admits it, so what prefill shows then is decode's saturation, not its own
// (computeK2). Like k2ReasonObsImplausible it is not a k2Source -- the
// analyzer falls through to the next priority.
const k2ReasonObsDownstream = "P1-obs-downstream"

const (
	satReasonP0Store = "P0-store" // capacity from store or compatible-variant record; no live replicas
	// satReasonNoData marks a variant with no live replicas and no store record.
	// It aliases the shared pipeline sentinel so this producer and the engine's
	// liveness gate (allocation.ResultIsInformative) cannot drift apart.
	satReasonNoData = allocation.ReasonNoData
)

// ReplicaCapacity holds the per-replica capacity breakdown computed by
// the V2 saturation analyzer. It is internal to the analyzer and not
// part of the public interfaces package.
type ReplicaCapacity struct {
	PodName               string
	VariantName           string
	AcceleratorName       string
	TokensInUse           int64
	TotalKvCapacityTokens int64
	MemoryBoundCapacity   int64    // k1: KV-cache-limited capacity
	ComputeBoundCapacity  int64    // k2: compute/scheduling-limited capacity
	K2Priority            k2Source // how k2 was computed
	EffectiveCapacity     int64    // min(k1, k2)
	// ReplicaDemand is the replica's resident KV tokens — TokensInUse on the main
	// path, kvCacheUsage * effectiveCapacity on the fallback path — plus the
	// role-aware waiting-queue footprint: queueLength * avgInputTokens for
	// prefill replicas, and queueLength * (avgInputTokens + avgOutputTokens) for
	// decode/"both". See waitingQueueDemand.
	ReplicaDemand int64
	// QueueLength is the number of requests waiting in this replica's engine
	// queue, and LocalQueueDemand the residency charge waitingQueueDemand put
	// on them, which ReplicaDemand includes. Carried separately so the
	// throughput model (throughput_floor.go) can take the residency charge
	// back out and price the same requests as work to be done instead.
	QueueLength      int
	LocalQueueDemand int64

	// FromWarmPool marks a BRIDGE: a warm pool Pod lent to this variant rather
	// than one of its own replicas. Carried through from the collector so
	// aggregation can put its demand in and keep its capacity out. See
	// domain.ReplicaMetrics.FromWarmPool.
	FromWarmPool bool

	// SaturatedThroughput is the completion rate (requests/s) one replica of
	// this bucket sustains when its queue is saturated, from the history
	// recorded beside k2; 0 when no saturation has been observed for the
	// bucket. Read by the throughput floor (throughput_floor.go).
	SaturatedThroughput float64
	// SaturatedThroughputSamples is how many readings the window that
	// produced SaturatedThroughput holds, and SaturatedThroughputBorrowed
	// whether that window is a neighbouring bucket's rather than the
	// replica's own (see nearestSaturatedThroughput). The floor orders a
	// replica only on an own window of MinThroughputSamplesToOrder readings;
	// anything less holds.
	SaturatedThroughputSamples  int
	SaturatedThroughputBorrowed bool
}

// outputBuckets lists the output-length buckets in ascending order of length.
// The order is what the throughput floor walks when a bucket has no reading
// of its own (see nearestSaturatedThroughput).
var outputBuckets = []string{"short", "medium", "long", "xlong", "xxlong", "huge"}

// classifyOutputLength returns a workload bucket name based on average
// output token length. The buckets are used to key compute-capacity (k2)
// history, since k2 depends heavily on generation length.
//
// Buckets (the thresholds and why they sit where they do are in constants.go):
//
//	"short"  — avgOutput in [0, 100)
//	"medium" — avgOutput in [100, 500)
//	"long"   — avgOutput in [500, 1500)
//	"xlong"  — avgOutput in [1500, 3000)
//	"xxlong" — avgOutput in [3000, 6000)
//	"huge"   — avgOutput >= 6000
func classifyOutputLength(avgOutputTokens float64) string {
	switch {
	case avgOutputTokens < ShortOutputThreshold:
		return "short"
	case avgOutputTokens < MediumOutputThreshold:
		return "medium"
	case avgOutputTokens < LongOutputThreshold:
		return "long"
	case avgOutputTokens < ExtraLongOutputThreshold:
		return "xlong"
	case avgOutputTokens < VeryLongOutputThreshold:
		return "xxlong"
	default:
		return "huge"
	}
}
