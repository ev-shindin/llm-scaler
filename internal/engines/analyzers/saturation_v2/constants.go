package saturation_v2

import "time"

const (
	// DecodeSaturationMemory is how long after the last cycle a decode
	// replica was seen full and queued the analyzer keeps treating decode as
	// saturated for prefill's sake (Analyze, roleSaturated). It is the
	// collector's row window: every row is a max_over_time[1m], so a prefill
	// row can carry a reading taken up to a minute before the cycle, and
	// decode's rows can have moved on -- measured on the shape-swap P/D
	// benchmark, decode's occupancy had dropped under k1 on the fourth
	// cycle of an episode while prefill's row still repeated the saturated
	// reading to the token. Without the memory that row would have
	// recorded.
	DecodeSaturationMemory = time.Minute

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

	// ThroughputSampleSpacing is how far apart two saturated completion-rate
	// readings must be for the second to count as a sample of its own toward
	// MinThroughputSamplesToOrder. The rate is rate(...[RequestRateWindow]) evaluated afresh
	// every cycle, so a cycle 15 s after the last reads mostly the same
	// window -- at 30 s scrapes, exactly the same two samples -- and a
	// reading a minute later is from a window that shares none of them.
	// Readings inside the spacing are folded into the last sample (its
	// window is still being read; the sample is the max of it). Equal to the
	// collector's rate window, registration.RequestRateWindow; a test holds
	// the two together. See recordSaturatedThroughput.
	ThroughputSampleSpacing = time.Minute

	// VeryLongOutputThreshold is the upper bound (exclusive) for the "xxlong"
	// output-length bucket; anything at or above it is "huge".
	VeryLongOutputThreshold = 6000
)
