package registry

import (
	"fmt"
	"strconv"
)

// Trigger metadata keys. These are the whole per-variant configuration surface:
// KEDA delivers the trigger's `metadata` map verbatim as `scalerMetadata` on
// every RPC, so a value put there reaches WVA without WVA reading anything.
//
// They replace the llm-d.ai/* annotations. The `managed` annotation has no
// successor and needs none — KEDA only calls a scaler its trigger names, so
// being called is what being managed means.
const (
	// ModelIDKey identifies the model the variant serves. Required: it is the
	// grouping key for every multi-variant decision, and a variant that cannot be
	// attributed to a model cannot be optimized against one.
	ModelIDKey = "modelID"

	// VariantCostKey is the per-replica cost used by cost-aware optimization.
	// Optional; a non-negative decimal string.
	VariantCostKey = "variantCost"

	// ScalingPolicyKey names the scaling policy — a reusable TIER such as
	// "interactive", "standard" or "batch" — whose thresholds, analyzer selection
	// and scale-to-zero settings this variant scales under. Optional; when absent
	// the cluster default entry's defaultPolicy applies, and failing that the
	// default entry alone.
	//
	// The policy is named, not described, precisely so it is reusable: policies
	// carry no model identity, so one tier serves many models and changing the tier
	// changes all of them at once. That is what per-(model, namespace) config keys
	// cannot express — they bind settings to identity, so a fleet-wide change is an
	// edit per model.
	//
	// It rides trigger metadata because the trigger is already the registration:
	// selecting by policy name needs nothing watched, listed, or matched, unlike
	// resolving policy from an InferencePool's selector.
	ScalingPolicyKey = "scalingPolicy"

	// WarmPoolKey names the warm pool this variant may borrow from, when the
	// namespace holds more than one. Optional, and deliberately NOT boilerplate:
	// a namespace with a single pool needs no selection at all, because there is
	// nothing to disambiguate. Writing a pool name on every ScaledObject would
	// be ceremony in the case that is by far the most common.
	//
	// More than one pool is a real configuration rather than a hypothetical: a
	// warm copy is only reusable on the accelerator it was loaded on, so a
	// cluster with two GPU types needs a pool per type, and a tensor-parallel
	// variant needs Pods holding that many devices.
	//
	// When a namespace holds several pools and a variant names none, it gets no
	// warm copy and WVA says so. Picking one for it would be a guess with a cost:
	// the wrong pool means a ~35 s load that can never serve.
	WarmPoolKey = "warmPool"

	// ScalerAddressKey is KEDA's own key naming this scaler's address. It is
	// consumed by KEDA, never by WVA; named here only so it is not mistaken for a
	// WVA key when reading a trigger.
	ScalerAddressKey = "scalerAddress"
)

// DefaultVariantCost matches the default the VariantAutoscaling type carries, so
// a trigger that omits variantCost ranks exactly where an unset one always has.
const DefaultVariantCost = "10.0"

// Meta is the validated trigger metadata for one variant.
type Meta struct {
	// ModelID is the model this variant serves.
	ModelID string
	// VariantCost is the per-replica cost, as a decimal string. Kept in string
	// form because that is what VariantAutoscalingConfigSpec holds; validated
	// here so no consumer has to.
	VariantCost string
	// ScalingPolicy names the reusable policy tier this variant scales under, or
	// is empty to take the cluster default.
	ScalingPolicy string
	// WarmPool names the warm pool this variant may borrow from, or is empty to
	// take the namespace's only pool.
	WarmPool string
}

// ParseMeta validates a trigger's metadata.
//
// Errors name the key and the value, because the operator's only view of this is
// WVA's log: KEDA does not surface a scaler's opinion of its own trigger metadata
// anywhere on the ScaledObject, so an unhelpful message here is the whole
// diagnostic.
func ParseMeta(metadata map[string]string) (Meta, error) {
	modelID := metadata[ModelIDKey]
	if modelID == "" {
		return Meta{}, fmt.Errorf("trigger metadata %q is required and must not be empty", ModelIDKey)
	}

	cost := metadata[VariantCostKey]
	if cost == "" {
		cost = DefaultVariantCost
	}
	costVal, err := strconv.ParseFloat(cost, 64)
	if err != nil {
		return Meta{}, fmt.Errorf("trigger metadata %q must be a number, got %q: %w", VariantCostKey, cost, err)
	}
	if costVal < 0 {
		return Meta{}, fmt.Errorf("trigger metadata %q must be non-negative, got %v", VariantCostKey, costVal)
	}

	return Meta{
		ModelID:       modelID,
		VariantCost:   cost,
		ScalingPolicy: metadata[ScalingPolicyKey],
		WarmPool:      metadata[WarmPoolKey],
	}, nil
}
