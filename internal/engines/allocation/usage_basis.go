package allocation

import "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"

// Usage basis: which measure of "GPUs in use" a constraint provider must be fed.
//
// There are two, they answer different questions, and feeding one where the
// other belongs is silently wrong — the constraint still computes, it just
// computes an answer to a question nobody asked:
//
//   - a PHYSICAL inventory asks "how many GPUs are actually free on the nodes?"
//     Every GPU-requesting pod counts, whoever owns it, because the scheduler
//     will not place a variant on a device something else already holds. A
//     training job in another namespace is as real an obstacle as one of WVA's
//     own replicas;
//   - a QUOTA asks "how much of the operator's declared allowance has WVA
//     consumed?" A quota governs WVA-managed variants and nothing else. Charging
//     it for pods WVA does not govern exhausts the allowance with workloads the
//     operator never meant to bill against it: a namespace with a 4-GPU WVA quota
//     and an unrelated 4-GPU training job reads as fully spent while WVA has used
//     none of it, and every scale-up is refused.
//
// Providers therefore declare which basis they need, and the caller hands each
// one the matching view (see GPUUsageViews.For). Broadcasting a single figure to
// every provider is what this replaces.
type UsageBasis int

const (
	// PhysicalUsage counts every GPU held on the cluster's GPU nodes, attributed
	// by the node a pod runs on and independent of whether WVA manages it. This
	// is the default: a provider that does not say otherwise is describing real
	// hardware.
	PhysicalUsage UsageBasis = iota

	// ManagedUsage counts only what WVA's own variants hold, summed from the
	// population it is optimizing. It is deliberately blind to everything else,
	// which is the property a quota needs.
	ManagedUsage
)

// String renders the basis for logs.
func (b UsageBasis) String() string {
	if b == ManagedUsage {
		return "managed"
	}
	return "physical"
}

// UsageBasisReporter is the optional interface a provider or inventory
// implements to ask for something other than PhysicalUsage. Optional on purpose:
// physical is both the common case and the safe default for a provider that has
// not thought about it, since a physical figure is a superset — it over-states
// consumption for a quota, which errs toward refusing, rather than under-stating
// free hardware, which would place a variant onto a device that is taken.
type UsageBasisReporter interface {
	UsageBasis() UsageBasis
}

// UsageBasisOf reports the basis v needs, defaulting to PhysicalUsage for
// anything that does not declare one.
func UsageBasisOf(v any) UsageBasis {
	if r, ok := v.(UsageBasisReporter); ok {
		return r.UsageBasis()
	}
	return PhysicalUsage
}

// PhysicalUsageConfigured reports whether the limiter cfg selects contains any
// provider that consumes the physical view.
//
// It answers from the mode rather than from a built limiter because callers need
// it before (and independently of) constructing one — notably the usage observer,
// which starts before either engine. The two must agree, which
// TestPhysicalUsageConfiguredMatchesTheBuiltLimiter checks by building the
// limiter for each mode and comparing the bases its providers declare.
//
// Unknown modes answer true: observing when nothing needs it costs a walk of the
// pod cache, while not observing when something does turns the capacity check off
// silently.
func PhysicalUsageConfigured(cfg *config.Config) bool {
	switch cfg.EffectiveLimiterMode() {
	case config.LimiterTypeNone, config.LimiterTypeQuota:
		return false
	default:
		return true
	}
}

// GPUUsageViews carries both measures of current GPU usage so a caller can serve
// a mixed set of providers from one place.
//
// Each view is the (cluster per-type, per-namespace per-type) pair that
// ComputeConstraints consumes. A view may be nil, meaning "not observed": callers
// must check NeedsMissingView before computing constraints, because a nil map is
// indistinguishable at the provider from a confident claim that nothing is in
// use, and a provider told nothing is in use reports its whole capacity free.
type GPUUsageViews struct {
	// PhysicalByType / PhysicalByNamespace: every GPU held on the cluster's GPU
	// nodes (see internal/gpuusage).
	PhysicalByType      map[string]int
	PhysicalByNamespace map[string]map[string]int

	// ManagedByType / ManagedByNamespace: only what WVA's own variants hold.
	ManagedByType      map[string]int
	ManagedByNamespace map[string]map[string]int

	// PoolsVersion is the version of the warm-pool figure folded into the
	// managed views (decision.WarmPoolGPUsWithVersion), stamped onto every
	// constraint computed from them. See ResourceConstraints.PoolsVersion.
	PoolsVersion uint64
}

// Stamp records the views' pools version on constraints computed from them.
func (v GPUUsageViews) Stamp(c *ResourceConstraints) *ResourceConstraints {
	if c != nil {
		c.PoolsVersion = v.PoolsVersion
	}
	return c
}

// For returns the usage pair the given provider must be fed.
func (v GPUUsageViews) For(cp ConstraintProvider) (map[string]int, map[string]map[string]int) {
	if UsageBasisOf(cp) == ManagedUsage {
		return v.ManagedByType, v.ManagedByNamespace
	}
	return v.PhysicalByType, v.PhysicalByNamespace
}

// Has reports whether the given basis has been observed.
func (v GPUUsageViews) Has(basis UsageBasis) bool {
	if basis == ManagedUsage {
		return v.ManagedByType != nil
	}
	return v.PhysicalByType != nil
}

// MissingBasis returns the first basis some provider needs that has not been
// observed, and whether there is one. Callers use it to decide between computing
// constraints and falling back to "unknown" — a decision that must be taken
// before any provider is called, since asking a provider with a nil view yields a
// confident-looking constraint built on no evidence.
func (v GPUUsageViews) MissingBasis(providers []ConstraintProvider) (UsageBasis, bool) {
	for _, cp := range providers {
		if basis := UsageBasisOf(cp); !v.Has(basis) {
			return basis, true
		}
	}
	return PhysicalUsage, false
}
