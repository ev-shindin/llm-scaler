package allocation

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// variantRecord is the optimizer's per-variant view of one model: authoritative
// identity from the discovery step, joined with the capacity signal the
// saturation analyzer measured for that variant.
//
// The two halves have different owners, and that is the point. Identity (name,
// role, cost, accelerator, GPUs per replica, bounds) is discovery's — it comes
// straight from domain.VariantMetadata, embedded here so it cannot drift from
// what discovery resolved. PerReplicaCapacity and Utilization are the analyzer's
// measured signal, looked up from its output. Before this type existed the
// optimizer read both halves off domain.VariantCapacity, which meant identity
// reached the optimizer only because the capacity builder had overlaid it back
// onto the analyzer's copies.
//
// The embedded replica counts are in *scale-target* units (pods, or LWS
// groups) — the same units the optimizer's targets and stateMap use, and the
// same units domain.VariantCapacity.ReplicaCount and PerReplicaCapacity are in,
// since the collector merges a pod's engine instances into one replica.
type variantRecord struct {
	domain.VariantMetadata

	// PerReplicaCapacity is the analyzer's P for this variant, in its own units.
	// Zero when the analyzer emitted no capacity for it, which every consumer
	// treats as "cannot size a replica off this variant" and skips.
	PerReplicaCapacity float64

	// Utilization is the analyzer's per-variant demand/capacity ratio, carried
	// through to the decision for the wva_saturation_utilization gauge.
	Utilization float64
}

// buildVariantRecords joins the request's discovery metadata with the capacity
// signal from satResult, producing the per-variant view the optimizer works on.
//
// Metadata drives the result set: one record per discovered variant, in
// discovery order. A variant the analyzer did not size gets a zero
// PerReplicaCapacity rather than being dropped, so the optimizer sees the whole
// fleet and skips what it cannot size — the same outcome as the variant being
// absent, but without the optimizer's view of the model silently depending on
// which variants an analyzer happened to emit.
//
// req.Variants is required: with no discovery metadata there is no cost and no
// accelerator to select variants by, and the analyzer's output no longer carries
// either. Returning nil for an empty one makes the caller skip the model rather
// than run the cost-aware math on zero costs, which would silently pick
// arbitrarily.
func buildVariantRecords(req ModelScalingRequest, satResult *domain.AnalyzerResult) []variantRecord {
	if satResult == nil || len(req.Variants) == 0 {
		return nil
	}
	capByVariant := make(map[string]domain.VariantCapacity, len(satResult.VariantCapacities))
	for _, vc := range satResult.VariantCapacities {
		capByVariant[vc.VariantName] = vc
	}
	out := make([]variantRecord, 0, len(req.Variants))
	for _, m := range req.Variants {
		vc := capByVariant[m.VariantName]
		out = append(out, variantRecord{
			VariantMetadata:    m,
			PerReplicaCapacity: vc.PerReplicaCapacity,
			Utilization:        vc.Utilization,
		})
	}
	return out
}

// recordsForRequest is the common entry point for the optimizers: it finds the
// saturation entry and builds the per-variant view from it. Returns nil when the
// model has no saturation result, which callers treat as "skip this model".
//
// Saturation is still the analyzer whose P sizes replicas — changing that is the
// coordination math and an explicit non-goal of this refactor.
func recordsForRequest(req ModelScalingRequest) []variantRecord {
	nr := req.CompositeSignal
	if nr.Result == nil {
		return nil
	}
	return buildVariantRecords(req, nr.Result)
}
