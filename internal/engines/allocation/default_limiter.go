package allocation

import (
	"context"
	"fmt"
	"maps"
	"slices"
)

// DefaultLimiter exposes an Inventory's resource availability as hard
// constraints for the optimizer.
//
// It is a ConstraintProvider and nothing more — it reports what is available
// and never touches scaling decisions. ComputeConstraints is the whole API:
//  1. Refresh the inventory to get latest resource limits from the cluster
//  2. Record the caller-supplied current usage
//  3. Return the resulting per-type (and, for namespace-aware inventories,
//     per-namespace) pools
type DefaultLimiter struct {
	name      string
	inventory Inventory
}

// NewDefaultLimiter creates a limiter that exposes the given inventory's
// availability as constraints.
func NewDefaultLimiter(name string, inventory Inventory) *DefaultLimiter {
	return &DefaultLimiter{
		name:      name,
		inventory: inventory,
	}
}

// Name returns the limiter identifier for logging/metrics.
func (l *DefaultLimiter) Name() string {
	return l.name
}

// UsageBasis reports which measure of "GPUs in use" this limiter must be fed,
// which is entirely a property of the inventory it wraps: a physical inventory
// needs every GPU on the node, an operator-declared quota needs only what WVA
// itself holds. The same DefaultLimiter shape serves both, so it cannot answer
// this on its own — it delegates, and defaults to physical for an inventory that
// does not declare a basis.
func (l *DefaultLimiter) UsageBasis() UsageBasis {
	return UsageBasisOf(l.inventory)
}

// ComputeConstraints refreshes the inventory and returns its resource availability.
// It exposes constraints for the optimizer, which is the only component that
// decides how the available resources are distributed.
//
// It always exposes per-type (cluster) availability via Pools. When the
// underlying inventory is namespace-aware (e.g. a namespace-scoped quota), it
// additionally exposes per-(namespace, type) caps via NamespacePools for the
// active namespaces (the keys of usageByNamespace) as a closed allowlist; the
// optimizer caps a model's allocation at min(per-type pool, that namespace's
// pool) for listed types and denies types the namespace does not list (see
// NamespaceResourcePools). Multi-entry quotas are handled by the engine
// computing constraints from each constituent of a CompositeLimiter.
//
// The keys of usageByNamespace define the active-namespace set: a namespace with
// a quota but zero current usage must still appear here (with an empty inner
// map) for its caps to be materialized and enforced.
//
// Remaining gap (tracked by the limiter chain, sub-issue #1003): composing a
// physical-inventory limiter with a quota limiter as min(physical, quota) within
// a single ComputeConstraints. See docs/reference/quota-limiter.md.
func (l *DefaultLimiter) ComputeConstraints(ctx context.Context, usageByType map[string]int, usageByNamespace map[string]map[string]int) (*ResourceConstraints, error) {
	// Step 1: Refresh inventory to get latest limits from the cluster
	//
	// This is Refresh (limits only), so Limit is every GPU INSTALLED on every
	// node. Usage arrives from the caller, on the basis this limiter's inventory
	// asked for (see UsageBasis): a physical inventory is fed every GPU held on
	// the nodes — including by workloads WVA does not manage, which used to be
	// invisible here and made free capacity over-stated — while a quota is fed
	// only what WVA's own variants hold, since that is what the allowance
	// governs. See docs/concepts/gpu-capacity-accounting.md.
	//
	// Remaining gap: a node that is cordoned or NotReady still contributes its
	// GPUs to Limit.
	if err := l.inventory.Refresh(ctx); err != nil {
		return nil, fmt.Errorf("failed to refresh inventory: %w", err)
	}

	// Step 2: Record current usage
	//
	// Usage keyed to an unresolved accelerator ("unknown") never lands in a pool,
	// because GetResourcePools iterates the discovered types — another way free
	// capacity is over-stated. Same doc, Gap 1.
	l.inventory.SetUsed(usageByType)

	// Step 3: Expose per-type availability
	rc := &ResourceConstraints{
		ProviderName: l.name,
		Pools:        l.inventory.GetResourcePools(),
		TotalLimit:   l.inventory.TotalLimit(),
		TotalUsed:    l.inventory.TotalUsed(),
		TotalAvail:   l.inventory.TotalAvailable(),
	}

	// Step 4: For namespace-aware inventories carrying per-namespace caps for the
	// active namespaces, expose them via NamespacePools and derive the cluster
	// per-type aggregate (Pools/Totals) from the SAME active set. Deriving the
	// aggregate from the active namespaces — rather than the static
	// GetResourcePools sum — keeps it consistent with the per-namespace budgets
	// the optimizer partitions: it includes default fall-through namespaces and
	// never under-counts, so the optimizer's per-type budget cannot be driven
	// negative as namespaces draw against it. When the inventory carries no
	// namespace caps (e.g. a cluster-scoped quota), Pools is left as the
	// per-type GetResourcePools result above.
	if nai, ok := l.inventory.(NamespaceAwareInventory); ok {
		nai.SetUsedByNamespace(usageByNamespace)
		activeNamespaces := slices.Collect(maps.Keys(usageByNamespace))
		if nsPools := nai.NamespaceResourcePools(activeNamespaces); len(nsPools) > 0 {
			rc.NamespacePools = nsPools
			rc.Pools = aggregateNamespacePools(nsPools)
			rc.TotalLimit, rc.TotalUsed, rc.TotalAvail = poolTotals(rc.Pools)
		}
	}
	return rc, nil
}

// aggregateNamespacePools sums per-(namespace, type) pools into a per-type
// cluster aggregate — the total finite budget that the per-namespace caps
// partition. Unlimited (negative-Limit sentinel) pools are skipped: they impose
// no finite cap, so a type that is only ever unlimited across namespaces is
// absent from the aggregate (no cluster cap), consistent with GetResourcePools.
func aggregateNamespacePools(nsPools map[string]map[string]ResourcePool) map[string]ResourcePool {
	agg := make(map[string]ResourcePool)
	for _, perType := range nsPools {
		for accType, pool := range perType {
			if pool.Limit < 0 {
				continue
			}
			a := agg[accType]
			a.Limit += pool.Limit
			a.Used += pool.Used
			agg[accType] = a
		}
	}
	return agg
}

// poolTotals returns the summed Limit, Used, and available (clamped at 0)
// across the given pools.
func poolTotals(pools map[string]ResourcePool) (limit, used, avail int) {
	for _, p := range pools {
		limit += p.Limit
		used += p.Used
	}
	avail = limit - used
	if avail < 0 {
		avail = 0
	}
	return limit, used, avail
}

// Ensure DefaultLimiter implements Limiter interface
var _ Limiter = (*DefaultLimiter)(nil)

// Ensure DefaultLimiter implements ConstraintProvider interface
var _ ConstraintProvider = (*DefaultLimiter)(nil)

// Ensure DefaultLimiter reports the usage basis its inventory needs
var _ UsageBasisReporter = (*DefaultLimiter)(nil)
