package allocation

import (
	"math"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// gpuLimitTracker records the variants of ONE model whose scale-up the optimizer
// had to cut short because a GPU budget ran out. It is the bridge between the
// allocation loops — which are the only place that knows a target was clamped —
// and the decisions built afterwards, which carry the operator-facing signal.
//
// Only genuine scarcity is recorded. A variant held back by its own MaxReplicas
// ceiling is honouring user intent, not competing for GPUs, and marking it would
// raise a spurious ResourceConstrained warning; the loops therefore distinguish
// the two bounds before marking.
//
// Keyed by variant name, which is unique within a model. Trackers are per-model
// so names never collide across models or namespaces.
type gpuLimitTracker map[string]bool

// mark records that variantName wanted more replicas than the GPU budget allowed.
// Repeated marks are idempotent — a variant clamped in several fair-share
// iterations is still one constrained decision.
func (t gpuLimitTracker) mark(variantName string) {
	if t != nil {
		t[variantName] = true
	}
}

// applyGPULimitAttribution stamps the resource-constraint signal onto a model's
// decisions: WasLimited for every variant the tracker recorded, and LimitedBy
// naming the provider whose pool bound it. It also emits the
// decisions_limited_total metric, once per constrained decision.
//
// Decisions of every action carry the signal, not just scale-ups: a variant that
// wanted to grow and was denied every GPU ends up at ActionNoChange, and that is
// precisely the case an operator needs to see.
func applyGPULimitAttribution(
	decisions []domain.VariantDecision,
	limited gpuLimitTracker,
	constraints []*ResourceConstraints,
) {
	// Published on EVERY pass, including the one where nothing was limited.
	// An empty snapshot is a real answer -- it says this pass denied nobody --
	// and publishing only the non-empty ones would leave the last contended
	// reading standing forever, holding warm pools down long after the burst.
	contended := map[string]map[string]bool{}
	defer func() { decision.PublishGPUContention(contended, time.Now()) }()

	if len(limited) == 0 {
		return
	}
	emitter := metrics.NewMetricsEmitter()
	for i := range decisions {
		d := &decisions[i]
		if !limited[d.VariantName] {
			continue
		}
		d.WasLimited = true
		d.LimitedBy = bindingProvider(constraints, d.Namespace, d.AcceleratorName)
		emitter.RecordDecisionsLimitedTotalMetric(d.VariantName, d.Namespace, d.LimitedBy)

		// A warm pool of this accelerator type, in this namespace, should stop
		// competing for GPUs a model replica is being denied. See
		// decision.GPUContention.
		if d.AcceleratorName != "" {
			if contended[d.Namespace] == nil {
				contended[d.Namespace] = map[string]bool{}
			}
			contended[d.Namespace][d.AcceleratorName] = true
		}
	}
}

// bindingProvider names the constraint provider whose pool most tightly bounds
// (namespace, accType) — the one to credit when a GPU budget clamps a scale-up.
//
// Cluster and namespace pools are weighed together, mirroring how
// effectiveAvailable combines them: the smallest availability wins. A provider
// that carries a closed allowlist for this namespace is judged on that allowlist
// alone (its per-type Pools are the aggregate *derived* from it, so counting
// both would weigh one provider twice), and a type the namespace does not list
// is a hard deny — availability 0, the tightest bound there is.
//
// Unlimited sentinels (Limit < 0) impose no bound and are skipped. Returns ""
// when no provider finitely bounds the type, which leaves LimitedBy empty rather
// than crediting a limiter that did not constrain anything.
func bindingProvider(constraints []*ResourceConstraints, namespace, accType string) string {
	name := ""
	tightest := math.MaxInt
	consider := func(avail int, provider string) {
		if avail < tightest {
			tightest, name = avail, provider
		}
	}
	for _, c := range constraints {
		if c == nil {
			continue
		}
		if perType, ok := c.NamespacePools[namespace]; ok {
			pool, listed := perType[accType]
			switch {
			case !listed:
				consider(0, c.ProviderName)
			case pool.Limit >= 0:
				consider(pool.Available(), c.ProviderName)
			}
			continue
		}
		if pool, ok := c.Pools[accType]; ok && pool.Limit >= 0 {
			consider(pool.Available(), c.ProviderName)
		}
	}
	return name
}
