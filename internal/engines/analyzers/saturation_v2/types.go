package saturation_v2

import "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"

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
	satReasonNoData = domain.ReasonNoData
)

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
