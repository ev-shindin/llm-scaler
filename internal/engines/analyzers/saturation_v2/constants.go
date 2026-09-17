package saturation_v2

import "time"

const (
	// RollingAverageWindowSize is the number of samples retained for compute
	// capacity (k2) history per workload bucket.
	RollingAverageWindowSize = 10

	// CapacityStalenessTimeout is the duration after which a stored capacity
	// record is considered stale and should be refreshed from live data.
	CapacityStalenessTimeout = 30 * time.Minute

	// CapacityEvictionTimeout is the duration after which unused capacity
	// store records are eligible for removal. This is intentionally long
	// because historical capacity knowledge is valuable for zero-replica
	// estimation and cross-variant matching (e.g., a variant may be at
	// zero replicas over a weekend and scale back up Monday).
	CapacityEvictionTimeout = 7 * 24 * time.Hour

	// HistoryEvictionTimeout is the duration after which unused k2 history
	// entries are eligible for removal. Shorter than capacity eviction
	// because workload patterns shift and stale k2 observations from a
	// very different workload can mislead scaling decisions.
	HistoryEvictionTimeout = 24 * time.Hour

	// BytesPerToken is the approximate number of bytes per LLM token.
	// Used to convert scheduler queue bytes to estimated token count.
	// Based on the OpenAI tiktoken observation that each token corresponds
	// to roughly 4 bytes of text. This is conservative (yields an upper
	// bound on token count) since modern tokenizers achieve 5-6 chars/token.
	BytesPerToken = 4

	// ShortOutputThreshold is the upper bound (exclusive) for the "short"
	// output-length bucket used for k2 history keying.
	ShortOutputThreshold = 100

	// MediumOutputThreshold is the upper bound (exclusive) for the "medium"
	// output-length bucket used for k2 history keying.
	MediumOutputThreshold = 500

	// LongOutputThreshold is the upper bound (exclusive) for the "long"
	// output-length bucket. Above 500 the buckets are a factor of two wide,
	// because what the key protects is a factor-of-two quantity: the
	// completion rate a saturated replica sustains falls roughly with output
	// length, so a 1000-token and a 4000-token shape sharing one bucket share
	// one throughput window, and the max the window keeps is the shorter
	// shape's -- which then holds a fleet at the shorter shape's size while
	// the longer one is served. Measured on the shape-swap benchmark
	// (docs/proposals/backlog-sizing.md), where the two shared "long".
	//
	// The boundaries deliberately avoid the round numbers benchmarks use as
	// fixed output lengths (1000, 2000, 4000): a mean that sits on a boundary
	// would move between two buckets on measurement noise and split its
	// history in half.
	LongOutputThreshold = 1500

	// ExtraLongOutputThreshold is the upper bound (exclusive) for the "xlong"
	// output-length bucket.
	ExtraLongOutputThreshold = 3000

	// BacklogDrainSeconds is how long a queued request may wait for capacity
	// that is not yet running: the throughput model prices a backlog of B
	// requests as B / BacklogDrainSeconds extra arrivals per second, so the
	// fleet it asks for clears the backlog in about this long while keeping up
	// with the load. It should not be shorter than a replica's start time --
	// capacity ordered to drain a backlog faster than it can start drains
	// nothing -- and a replica on the benchmark clusters takes 60-100 s. Sixty
	// keeps the order within one start. See throughput_floor.go.
	BacklogDrainSeconds = 60.0

	// MinThroughputSamplesToOrder is how many saturated readings a role's own
	// output-length bucket must hold before the throughput floor may ORDER a
	// replica from it; with fewer it holds the fleet and no more. The first
	// reading at a saturation under-reads (a 1m rate on a replica that has
	// been full for 20 s counts a third of a minute's completions), and an
	// order on an under-read over-provisions in a way that removes the
	// saturation which would have corrected it. Measured on the shape-swap
	// trace: 3.67, then 5.23, then 7.13 req/s on three consecutive saturated
	// cycles of one replica. Two readings is the second cycle.
	MinThroughputSamplesToOrder = 2

	// VeryLongOutputThreshold is the upper bound (exclusive) for the "xxlong"
	// output-length bucket; anything at or above it is "huge".
	VeryLongOutputThreshold = 6000
)
