// Limiter interfaces for resource constraint discovery.
//
// # Design Rationale
//
// Limiters supply CONSTRAINTS ONLY. They discover what resources exist and what
// caps apply to them; they never decide how those resources are distributed.
// Distribution is the optimizer's job, and it reads the constraints through
// ConstraintProvider:
//
//   - ConstraintProvider: "Here are the limits" (hard facts)
//   - ScalingOptimizer: "Here's what to do given those limits" (decision logic)
//
// The split keeps a single decision-maker. An earlier design also let limiters
// mutate decisions in place through an allocation algorithm; that half has been
// removed, so a limiter can no longer contradict — or silently pre-empt — the
// optimizer's allocation.
//
// # Interface Hierarchy
//
//	Limiter (the configured limiter slot; see NewLimiterFromConfig)
//	   │
//	   ├── Inventory (resource granularity: cluster pool, per-type, per-node)
//	   │
//	   └── ConstraintProvider (what the optimizer consumes)
//
// DefaultLimiter implements all three: it wraps an Inventory and exposes its
// availability as ResourceConstraints.
//
// The Inventory interface here is separate from collector's inventory because:
//   - Collector inventory: knows how to collect metrics, track staleness, emit events
//   - Limiter inventory: knows resource availability for scaling decisions
//   - Different responsibilities, different lifecycles
package allocation

import (
	"context"
)

// Limiter is the configured limiter slot: whatever NewLimiterFromConfig built
// for this deployment, held by the engine and handed to the optimizer path.
//
// A limiter supplies constraints only — it never mutates decisions. The
// optimizer reaches its constraints by type-asserting to ConstraintProvider
// (see gpuConstraintProviders), which is why this interface carries just an
// identity: a limiter shape that provides no constraints (NoOpLimiter) is a
// valid, inert value of the slot.
type Limiter interface {
	// Name returns limiter identifier for logging/metrics.
	Name() string
}

// ResourcePool represents available resources for one accelerator type.
type ResourcePool struct {
	Limit int // total capacity (from cluster discovery)
	Used  int // currently in use
}

// Available returns the number of allocatable resources (Limit - Used),
// clamped to a minimum of 0.
//
// Available is only meaningful for a FINITE pool (Limit >= 0). A pool carrying
// the unlimited sentinel (Limit < 0, emitted per-(namespace, type) by
// NamespaceResourcePools) would clamp to 0 here, which reads as "deny" —
// the opposite of "unlimited". Callers that may see the sentinel must branch on
// Limit < 0 first (see nsPoolBudget and mergeConstraints); do not call
// Available on such a pool.
func (p ResourcePool) Available() int {
	avail := p.Limit - p.Used
	if avail < 0 {
		return 0
	}
	return avail
}

// ResourceConstraints represents hard resource constraints from a single provider.
// Multiple providers can each contribute constraints; the optimizer respects all of them.
type ResourceConstraints struct {
	ProviderName string                  // e.g., "gpu-limiter", "quota-limiter"
	Pools        map[string]ResourcePool // accelerator type → pool
	// NamespacePools carries per-(namespace, accelerator type) caps for
	// namespace-scoped providers (e.g. a namespace-scoped quota limiter).
	// Outer key: namespace; inner key: accelerator type. Nil for providers
	// that only impose cluster/per-type constraints. The optimizer caps a
	// model's allocation at min(per-type pool, this namespace's pool).
	NamespacePools map[string]map[string]ResourcePool
	TotalLimit     int
	TotalUsed      int
	TotalAvail     int
	// PoolsVersion is the version of the warm-pool figure
	// (decision.WarmPoolGPUsVersion) that the usage these constraints were
	// computed against included. Set by the engine that built the usage views,
	// not by the provider, which never sees where its usage came from. The
	// headroom published from these constraints carries it on, so a warm pool
	// can tell a snapshot that has charged its Pods from one that has not.
	PoolsVersion uint64
}

// ConstraintProvider exposes hard constraints for the optimizer.
// Implementations discover resource availability without making allocation decisions.
//
// This separates constraint discovery from decision-making:
//   - ConstraintProvider: "Here are the limits" (hard facts)
//   - ScalingOptimizer: "Here's what to do given those limits" (decision logic)
type ConstraintProvider interface {
	// Name returns provider identifier, used as ProviderName in ResourceConstraints.
	Name() string

	// ComputeConstraints refreshes resource data and returns hard constraints.
	// usageByType maps accelerator type → GPUs currently in use (cluster-wide).
	// usageByNamespace maps namespace → accelerator type → GPUs in use, and its
	// keys also define the set of active namespaces for which namespace-scoped
	// caps are materialized (so a namespace with a quota but zero current usage
	// is still constrained). It may be nil for providers that ignore namespaces.
	ComputeConstraints(ctx context.Context, usageByType map[string]int, usageByNamespace map[string]map[string]int) (*ResourceConstraints, error)
}

// Inventory provides resource availability.
//
// Implementations define the granularity of resource tracking:
//   - TypeInventory: separate pools per accelerator type (H100, A100, etc.)
//   - QuotaInventory: operator-declared caps, cluster- or namespace-scoped
//
// The Inventory tracks three values per resource pool:
//   - Limit: total capacity (discovered from cluster, or operator-declared)
//   - Used: currently allocated/in-use GPUs
//   - Available: Limit - Used (what can still be allocated)
//
// The Inventory is responsible for:
//   - Refreshing capacity data (limits) from the cluster
//   - Tracking current usage
//
// Note: This is separate from collector.Inventory which handles metrics collection.
// The limiter Inventory focuses solely on reporting resource availability.
type Inventory interface {
	// Name returns inventory identifier for logging/metrics.
	Name() string

	// Refresh updates inventory limits from the cluster.
	// Should be called before reading pools to ensure fresh data.
	Refresh(ctx context.Context) error

	// SetUsed updates the used GPU counts.
	// This should be called with current usage before reading pools.
	// The usedByType map contains accelerator type -> used GPU count.
	SetUsed(usedByType map[string]int)

	// TotalLimit returns total GPU capacity across all resources.
	TotalLimit() int

	// TotalUsed returns total GPUs currently in use across all resources.
	TotalUsed() int

	// TotalAvailable returns total available GPUs (Limit - Used).
	TotalAvailable() int

	// GetResourcePools returns per-type resource availability.
	// Each key is an accelerator type (e.g., "A100", "H100").
	GetResourcePools() map[string]ResourcePool
}

// NamespaceAwareInventory is an Inventory whose constraints depend on the
// namespace of the decision, not just the accelerator type. Implementations
// bucket usage by namespace in addition to type; the chain orchestrator is
// expected to feed them the per-namespace usage map alongside the per-type
// map.
//
// Use feature detection at the chain level:
//
//	if nai, ok := inv.(NamespaceAwareInventory); ok {
//	    nai.SetUsedByNamespace(usedByNS)
//	}
//	inv.SetUsed(usedByType) // always safe; namespace-only inventories ignore it
//
// Implementations should remain valid Inventory values: SetUsed must not
// panic when called on a namespace-scoped inventory (it may simply be a
// no-op).
type NamespaceAwareInventory interface {
	Inventory

	// SetUsedByNamespace updates the per-namespace usage map.
	// Outer key: namespace; inner key: accelerator type.
	// Should be called before reading pools, alongside SetUsed.
	SetUsedByNamespace(usedByNS map[string]map[string]int)

	// NamespaceResourcePools returns per-(namespace, accelerator type) pools
	// for the given active namespaces as a CLOSED allowlist, resolving each
	// namespace's caps via the inventory's lookup rules (e.g. explicit entry →
	// default fall-through → exclude). Outer key: namespace; inner key:
	// accelerator type. A namespace that is unconstrained (excluded) is omitted,
	// signalling "open" — bound only by the cluster/per-type constraint. Every
	// other active namespace is present (a closed allowlist; an empty inner map
	// is a deny-all). A listed type with an unlimited cap is emitted as a
	// sentinel pool with Limit < 0 (so it stays distinct from a type the
	// namespace does not list, which the optimizer must deny). SetUsedByNamespace
	// should be called first so the returned pools carry current usage.
	NamespaceResourcePools(activeNamespaces []string) map[string]map[string]ResourcePool
}

// ConstraintProvidersFrom unwraps a configured limiter into the providers that
// can supply constraints: a limiter that is itself a ConstraintProvider, or each
// constituent of a CompositeLimiter that is one (so a multi-entry quota config is
// fully consulted). Returns nil for a nil limiter, or one that provides no
// constraints — NoOpLimiter is a valid, inert value of the slot.
//
// Shared by every constraint consumer so they cannot drift in what they consider
// a provider.
func ConstraintProvidersFrom(l Limiter) []ConstraintProvider {
	switch lim := l.(type) {
	case nil:
		return nil
	case *CompositeLimiter:
		var providers []ConstraintProvider
		for _, c := range lim.Constituents() {
			if cp, ok := c.(ConstraintProvider); ok {
				providers = append(providers, cp)
			}
		}
		return providers
	case ConstraintProvider:
		return []ConstraintProvider{lim}
	}
	return nil
}
