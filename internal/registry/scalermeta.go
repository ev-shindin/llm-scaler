package registry

import (
	"fmt"
	"strconv"
	"time"
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

	// WarmPoolCopiesKey is how many warm copies of this variant the pool should
	// hold. Optional; a non-negative integer.
	//
	// Absent means AUTOMATIC, which is the default and the right answer for
	// almost everything: the pool decides from parking, popularity and miss
	// frequency, and holds at most one copy. Setting it takes that judgement
	// away, and is worth doing in two cases automatic mode cannot express.
	//
	//	"0"  never warm this variant -- it opts out of a pool it shares, freeing
	//	     the slot for models that gain more from it
	//	"1"  always keep one warm, whatever the pool's own ranking thinks
	//	"N"  keep N warm, so N scale-ups of this variant can bridge AT ONCE
	//
	// The last is the one automatic mode cannot do at all. A single warm copy
	// bridges a single scale-up, so a variant that scales twice in quick
	// succession takes a cold start for the second with free Pods sitting
	// beside it. It is also the only way to weight a shared pool toward one
	// model, since automatic mode holds one copy of each and no more.
	//
	// "1" is not the same as absent. Absent lets a quiet variant lose its slot
	// to a busier one; "1" pins it, which is what a low-traffic but
	// latency-critical model needs and what popularity ranking can never give.
	WarmPoolCopiesKey = "warmPoolCopies"

	// WarmPoolNameKey marks a ScaledObject as scaling a warm POOL rather than a
	// model, and names which pool it is.
	//
	// A pool is scaled the same way every other workload here is -- WVA computes
	// a size, KEDA writes it -- which is what lets the controller stay read-only.
	// The alternative was a write permission on the pool Deployment, and
	// internal/controller/rbac.go is explicit that a licence to resize workloads
	// is the single permission a cluster admin is most right to refuse.
	//
	// Its presence is also what excuses this trigger from carrying a modelID: a
	// pool serves no model in particular, and every consumer that builds
	// variants skips these entries rather than inventing one.
	WarmPoolNameKey = "warmPoolName"

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
	// WarmPoolName is set when this trigger scales a warm POOL rather than a
	// model. When it is set, nothing else here is meaningful.
	WarmPoolName string
	// WarmPoolCopies is how many warm copies to hold, or nil for automatic. A
	// POINTER because zero is a real setting -- never warm this -- and has to be
	// distinguishable from "not specified".
	WarmPoolCopies *int
}

// ParseMeta validates a trigger's metadata.
//
// Errors name the key and the value, because the operator's only view of this is
// WVA's log: KEDA does not surface a scaler's opinion of its own trigger metadata
// anywhere on the ScaledObject, so an unhelpful message here is the whole
// diagnostic.
func ParseMeta(metadata map[string]string) (Meta, error) {
	// A pool's trigger names no model because it serves none. Returned early so
	// the model-shaped validation below does not reject it, and so callers have
	// one field to test rather than a rule to remember.
	if poolName := metadata[WarmPoolNameKey]; poolName != "" {
		return Meta{WarmPoolName: poolName}, nil
	}

	modelID := metadata[ModelIDKey]
	if modelID == "" {
		return Meta{}, fmt.Errorf("trigger metadata %q is required and must not be empty; "+
			"a ScaledObject that scales a warm pool sets %q instead", ModelIDKey, WarmPoolNameKey)
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

	// Parsed here rather than at the point of use so a bad value is rejected
	// with the trigger that carried it, where the operator can act on it.
	var copies *int
	if raw := metadata[WarmPoolCopiesKey]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return Meta{}, fmt.Errorf("trigger metadata %q must be a non-negative whole number, got %q",
				WarmPoolCopiesKey, raw)
		}
		copies = &n
	}

	return Meta{
		ModelID:        modelID,
		VariantCost:    cost,
		ScalingPolicy:  metadata[ScalingPolicyKey],
		WarmPool:       metadata[WarmPoolKey],
		WarmPoolCopies: copies,
	}, nil
}

// ScalesAWarmPool reports whether a trigger's metadata scales a warm pool rather
// than a model. Callers that build variants must skip these: a pool has no
// model, no engine options and nothing to autoscale on demand for.
func ScalesAWarmPool(metadata map[string]string) bool {
	return metadata[WarmPoolNameKey] != ""
}

// Trigger metadata keys that tune a warm POOL. They appear only on a trigger
// that carries WarmPoolNameKey.
//
// The pool's configuration lives with its ScaledObject rather than on the
// Deployment that supplies its Pods, because a warm pool is a WVA concept that
// happens to have Pods rather than a workload WVA happens to manage. Nothing
// outside WVA reads it, nothing outside WVA creates it, and there is no sensible
// way to operate one except through WVA -- so its declaration belongs with
// everything else WVA is told, and the invariant stays whole: WVA manages what
// it is CALLED about, with no exceptions.
//
// It also puts the reserve beside the envelope it has to fit inside. The rule
// that decides whether a pool can warm anything at all -- maxReplicaCount must
// exceed the reserve -- is now checkable within one object.
const (
	// WarmPoolSleepMinSizeKey is the floor on FREE Pods: the reserve kept for
	// the next spike. Admission draws on free-minus-reserve, so a reserve that
	// reaches maxReplicaCount leaves a budget of zero forever.
	WarmPoolSleepMinSizeKey = "warmPoolSleepMinSize"
	// WarmPoolMaxHoldKey bounds how long a borrowed Pod may serve before it is
	// returned regardless, so a scale-up that never arrives cannot turn the
	// reserve into permanent capacity for one variant. A Go duration.
	WarmPoolMaxHoldKey = "warmPoolMaxHold"
	// WarmPoolPreloadTopKey warms this many of the busiest variants without
	// waiting for each to miss first.
	WarmPoolPreloadTopKey = "warmPoolPreloadTop"
	// WarmPoolGPUMemoryUtilizationKey overrides --gpu-memory-utilization for
	// warm copies, trading KV cache for warm-set size.
	WarmPoolGPUMemoryUtilizationKey = "warmPoolGPUMemoryUtilization"
)

// PoolMeta is a warm pool's tuning, as its trigger declares it. Every field is
// optional; an absent one leaves the controller's own default in place.
type PoolMeta struct {
	Name                 string
	SleepMinSize         *int
	MaxHold              *time.Duration
	PreloadTop           *int
	GPUMemoryUtilization float64
}

// ParsePoolMeta reads a pool trigger's tuning.
//
// A value that does not parse is REFUSED rather than defaulted, and the whole
// trigger with it. A pool's knobs decide how many GPUs it holds and whether it
// can warm anything; silently ignoring one and carrying on would leave an
// operator looking at a number that is not in force anywhere.
func ParsePoolMeta(metadata map[string]string) (PoolMeta, error) {
	name := metadata[WarmPoolNameKey]
	if name == "" {
		return PoolMeta{}, fmt.Errorf("trigger metadata %q is required on a warm pool trigger", WarmPoolNameKey)
	}
	out := PoolMeta{Name: name}

	if raw := metadata[WarmPoolSleepMinSizeKey]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return PoolMeta{}, fmt.Errorf("trigger metadata %q must be a non-negative whole number, got %q",
				WarmPoolSleepMinSizeKey, raw)
		}
		out.SleepMinSize = &n
	}
	if raw := metadata[WarmPoolMaxHoldKey]; raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return PoolMeta{}, fmt.Errorf("trigger metadata %q must be a positive duration such as 2m, got %q",
				WarmPoolMaxHoldKey, raw)
		}
		out.MaxHold = &d
	}
	if raw := metadata[WarmPoolPreloadTopKey]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return PoolMeta{}, fmt.Errorf("trigger metadata %q must be a non-negative whole number, got %q",
				WarmPoolPreloadTopKey, raw)
		}
		out.PreloadTop = &n
	}
	if raw := metadata[WarmPoolGPUMemoryUtilizationKey]; raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v <= 0 || v > 1 {
			return PoolMeta{}, fmt.Errorf("trigger metadata %q must be a fraction above 0 and at most 1, got %q",
				WarmPoolGPUMemoryUtilizationKey, raw)
		}
		out.GPUMemoryUtilization = v
	}
	return out, nil
}
