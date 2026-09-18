package allocation

import (
	"context"
	"fmt"
	"math"
	"sort"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// CostAwareOptimizer is a per-model optimizer that minimizes total cost while
// meeting capacity requirements. It processes each model independently:
//
//   - Scale-up: adds replicas to the most cost-efficient variant (lowest cost / perReplicaCapacity)
//   - Scale-down: removes replicas from the most expensive variant (highest absolute cost)
//   - Only the cheapest variant is protected at >=1 replica; others can scale to 0
//   - Variants with pending replicas are skipped for scale-up
//
// This optimizer ignores ResourceConstraints (unlimited mode). For GPU-limited
// environments, use GreedyByScoreOptimizer instead.
type CostAwareOptimizer struct{}

// NewCostAwareOptimizer creates a new CostAwareOptimizer.
func NewCostAwareOptimizer() *CostAwareOptimizer {
	return &CostAwareOptimizer{}
}

// Name returns the optimizer identifier.
func (o *CostAwareOptimizer) Name() string {
	return "cost-aware"
}

// Optimize produces VariantDecisions for all models.
// Constraints are ignored in unlimited mode (CostAwareOptimizer).
func (o *CostAwareOptimizer) Optimize(
	ctx context.Context,
	requests []ModelScalingRequest,
	constraints []*ResourceConstraints,
) []domain.VariantDecision {
	logger := ctrl.LoggerFrom(ctx).WithName(o.Name())
	var allDecisions []domain.VariantDecision

	for _, req := range requests {
		records := recordsForRequest(req)
		if records == nil {
			continue
		}

		stateMap := buildStateMap(req.VariantStates)
		vcMap := buildCapacityMap(records)
		targets := initTargets(req.VariantStates)

		// Unified dispatch: one path for all models via (model, role) math.
		// Non-disaggregated uses synthetic "both" role; disaggregated uses actual roles.
		e := req.CompositeSignal
		roles, ps := initRoleState(&e)
		if anyRoleNeedsScaleUp(ps, roles) {
			allocateForModelPaired(ctx, &e, records, stateMap, nil, targets,
				costGreedyRolePick, ps, roles)
		} else {
			scaleDownRoleIterated(ctx, &e, records, targets, stateMap)
		}

		decisions := buildDecisionsWithOptimizer(req, stateMap, vcMap, targets, "cost-aware")
		logger.V(logging.DEBUG).Info("Cost-aware optimizer decisions",
			"modelID", req.ModelID,
			"decisions", len(decisions))
		allDecisions = append(allDecisions, decisions...)
	}

	return allDecisions
}

// costGreedyRolePick is a RolePickFn that picks the cheapest-by-cost-efficiency
// variant in the given role. For "both" (non-disaggregated), all variants are
// eligible. For role-tagged roles, only variants with a matching Role are picked.
func costGreedyRolePick(
	role string,
	variants []variantRecord,
	stateMap map[string]domain.VariantReplicaState,
	_ map[string]int,
	targets map[string]int,
) (string, int) {
	roleVCs := variantsForRole(variants, role)
	for _, vc := range sortByCostEfficiencyAsc(roleVCs) {
		if vc.PerReplicaCapacity <= 0 {
			continue
		}
		state := stateMap[vc.VariantName]
		if state.MaxReplicas != nil && *state.MaxReplicas > 0 {
			headroom := *state.MaxReplicas - targets[vc.VariantName]
			if headroom <= 0 {
				continue
			}
			return vc.VariantName, headroom
		}
		return vc.VariantName, math.MaxInt
	}
	return "", 0
}

// scaleDownVariantSet sheds replicas from sortedVariants (PRE-SORTED cost-desc,
// cheapest last). minReplicas floor and cheapest-at-1 protection are enforced
// here. maxRemovable returns how many replicas of vc the caller permits to remove;
// onRemove is invoked after committing n so the caller can update its spare bookkeeping.
func scaleDownVariantSet(
	ctx context.Context,
	sortedVariants []variantRecord,
	targets map[string]int,
	states map[string]domain.VariantReplicaState,
	maxRemovable func(vc variantRecord) int,
	onRemove func(vc variantRecord, n int),
) {
	logger := ctrl.LoggerFrom(ctx)
	for i, vc := range sortedVariants {
		if vc.PerReplicaCapacity <= 0 {
			continue
		}
		current := targets[vc.VariantName]
		minReplicas := 0
		if states != nil {
			if st, ok := states[vc.VariantName]; ok && st.MinReplicas != nil {
				minReplicas = *st.MinReplicas
			}
		}
		removable := current - minReplicas
		if removable <= 0 {
			continue
		}
		n := maxRemovable(vc)
		if n > removable {
			n = removable
		}
		// cheapest-at-1: the last (cheapest) variant is protected at 1 only when no
		// more-expensive variant still holds replicas (#1237's positional rule).
		if i == len(sortedVariants)-1 && current-n < 1 && !anyHasReplicas(sortedVariants[:i], targets) {
			n = current - 1
		}
		if n <= 0 {
			continue
		}
		targets[vc.VariantName] = current - n
		onRemove(vc, n)
		logger.V(logging.DEBUG).Info("scale-down: removed replicas",
			"variant", vc.VariantName, "removed", n, "cost", vc.Cost)
	}
}

// sortVariantsForScaleDown orders a role's variants for cost-greedy scale-down:
//  1. Cost descending — shed the most expensive first.
//  2. Tie: score × PRC ascending — Score·PRC[v] (single entry).
//  3. Tie: variant name ascending — full determinism.
func sortVariantsForScaleDown(e NamedAnalyzerResult, roleVCs []variantRecord) []variantRecord {
	weighted := func(name string) float64 {
		if e.Result == nil {
			return 0
		}
		return e.Score * prcForVariant(e.Result, name)
	}
	out := append([]variantRecord(nil), roleVCs...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		wi, wj := weighted(out[i].VariantName), weighted(out[j].VariantName)
		if wi != wj {
			return wi < wj
		}
		return out[i].VariantName < out[j].VariantName
	})
	return out
}

// anyHasReplicas reports whether any of the given variants has a positive target.
func anyHasReplicas(variants []variantRecord, targets map[string]int) bool {
	for _, vc := range variants {
		if targets[vc.VariantName] > 0 {
			return true
		}
	}
	return false
}

// buildStateMap creates a lookup map from variant name to VariantReplicaState.
func buildStateMap(states []domain.VariantReplicaState) map[string]domain.VariantReplicaState {
	m := make(map[string]domain.VariantReplicaState, len(states))
	for _, s := range states {
		m[s.VariantName] = s
	}
	return m
}

// buildCapacityMap creates a lookup map from variant name to VariantCapacity.
func buildCapacityMap(capacities []variantRecord) map[string]variantRecord {
	m := make(map[string]variantRecord, len(capacities))
	for _, vc := range capacities {
		m[vc.VariantName] = vc
	}
	return m
}

// initTargets creates initial targets from current replica counts.
func initTargets(states []domain.VariantReplicaState) map[string]int {
	targets := make(map[string]int, len(states))
	for _, s := range states {
		targets[s.VariantName] = s.CurrentReplicas
	}
	return targets
}

// sortByCostEfficiencyAsc returns variants sorted by cost/perReplicaCapacity ascending.
func sortByCostEfficiencyAsc(capacities []variantRecord) []variantRecord {
	sorted := make([]variantRecord, len(capacities))
	copy(sorted, capacities)
	sort.Slice(sorted, func(i, j int) bool {
		return costEfficiency(sorted[i]) < costEfficiency(sorted[j])
	})
	return sorted
}

// costEfficiency returns the cost per unit of capacity.
func costEfficiency(vc variantRecord) float64 {
	if vc.PerReplicaCapacity <= 0 {
		return math.MaxFloat64
	}
	return vc.Cost / vc.PerReplicaCapacity
}

// buildDecisionsWithOptimizer converts targets map into VariantDecision slice.
// optimizerName is included in reason strings for observability.
func buildDecisionsWithOptimizer(
	req ModelScalingRequest,
	stateMap map[string]domain.VariantReplicaState,
	vcMap map[string]variantRecord,
	targets map[string]int,
	optimizerName string,
) []domain.VariantDecision {
	decisions := make([]domain.VariantDecision, 0, len(targets))
	// satNamed carries the model-level RequiredCapacity/SpareCapacity computed by
	// applyUniversalThreshold; per-variant Utilization is on each VariantCapacity.
	// These feed the saturation gauges (utilization/required/spare).
	satNamed := req.CompositeSignal
	for name, target := range targets {
		state := stateMap[name]
		vc := vcMap[name]

		var action domain.SaturationAction
		var decisionReason domain.DecisionReason
		var detailedReason string
		switch {
		case target > state.CurrentReplicas:
			action = domain.ActionScaleUp
			decisionReason = domain.DecisionReasonV2
			detailedReason = fmt.Sprintf("%s (optimizer: %s)", string(decisionReason), optimizerName)
		case target < state.CurrentReplicas:
			action = domain.ActionScaleDown
			decisionReason = domain.DecisionReasonV2
			detailedReason = fmt.Sprintf("%s (optimizer: %s)", string(decisionReason), optimizerName)
		default:
			action = domain.ActionNoChange
			decisionReason = domain.DecisionReasonV2
			detailedReason = string(decisionReason)
		}

		// GPUs the target commits for this variant. Unlike the replica counts it
		// is a resource figure, so it reports the WHOLE footprint at the target,
		// not the delta. An unset GPUsPerReplica reads as the 1-GPU default that
		// the rest of the optimizer assumes (gpusPerReplicaFromState).
		gpusPerReplica := state.GPUsPerReplica
		if gpusPerReplica <= 0 {
			gpusPerReplica = 1
		}

		decision := domain.VariantDecision{
			VariantName:     name,
			ModelID:         req.ModelID,
			Namespace:       req.Namespace,
			AcceleratorName: vc.AcceleratorName,
			Cost:            vc.Cost,
			Role:            state.Role,
			CurrentReplicas: state.CurrentReplicas,
			TargetReplicas:  target,
			GPUsPerReplica:  gpusPerReplica,
			GPUsAllocated:   target * gpusPerReplica,
			MinReplicas:     state.MinReplicas,
			MaxReplicas:     state.MaxReplicas,
		}
		// SetDecisionReason is the single place that sets d.Action (avoids a
		// redundant Action assignment in the struct literal above).
		decision.SetDecisionReason(action, decisionReason, detailedReason)

		// Observability fields consumed by RecordSaturationMetrics
		// (wva_saturation_utilization / wva_required_capacity / wva_spare_capacity).
		// Without these the three V2 gauges read zero. RequiredCapacity and
		// SpareCapacity are the matched scale-up/scale-down token signals from
		// applyUniversalThreshold; Utilization is the per-variant demand/capacity ratio.
		// For P/D-disaggregated models use the variant's per-role capacity; otherwise
		// fall back to the model-level totals.
		decision.Utilization = vc.Utilization
		reqCap, spareCap := satNamed.RequiredCapacity, satNamed.SpareCapacity
		role := state.Role
		if role == "" {
			role = domain.RoleBoth
		}
		if rc, ok := satNamed.RoleCapacities[role]; ok {
			reqCap, spareCap = rc.RequiredCapacity, rc.SpareCapacity
		}
		decision.RequiredCapacity = reqCap
		decision.SpareCapacity = spareCap
		// The demand that PRICED the target, so a later stage re-pricing a
		// different replica count uses the same figure: model-level (or per-role
		// for a disaggregated model), which carries the scheduler-queue estimate
		// the per-variant engine-local sum does not -- taken at THIS variant's
		// share of it, or the other variants' traffic would be priced against
		// this one's replicas and no hold could ever survive on a model with
		// two variants of the same role.
		demand := satNamed.Result.TotalDemand
		if rc, ok := satNamed.RoleCapacities[role]; ok {
			demand = rc.TotalDemand
		}
		if share, ok := variantDemandShare(satNamed.Result.VariantCapacities, vc.VariantName, role); ok {
			decision.TotalDemand = demand * share
		} else {
			decision.TotalDemand = domain.DemandUnpriced
		}
		decision.PerReplicaCapacity = vc.PerReplicaCapacity
		decision.ScaleUpThreshold = satNamed.ScaleUpThreshold

		decisions = append(decisions, decision)
	}
	return decisions
}

// variantDemandShare is the fraction of a model's (or role's) demand that
// belongs to the named variant: its engine-local demand over the engine-local
// demand of every variant of that role. One variant means 1. When nothing
// reported, the share is even, so a variant is never priced at zero for want
// of rows. When its siblings reported and it did not -- a rolling restart,
// or the router pinning the traffic elsewhere -- its share is unknown, not
// zero: a zero share would price any replica count at zero utilization and
// nothing could ever release a hold on it. Reports ok=false in that case.
func variantDemandShare(vcs []domain.VariantCapacity, variantName, role string) (float64, bool) {
	var mine, all float64
	var n int
	for _, vc := range vcs {
		r := vc.Role
		if r == "" {
			r = domain.RoleBoth
		}
		if r != role {
			continue
		}
		n++
		all += vc.TotalDemand
		if vc.VariantName == variantName {
			mine = vc.TotalDemand
		}
	}
	switch {
	case n <= 1:
		return 1, true
	case all == 0:
		return 1 / float64(n), true
	case mine == 0:
		return 0, false
	default:
		return mine / all, true
	}
}

// mergeConstraints combines GPU budget constraints from multiple providers.
// Used by GreedyByScoreOptimizer; lives here since CostAwareOptimizer owns the shared helpers.
func mergeConstraints(constraints []*ResourceConstraints) map[string]int {
	merged := make(map[string]int)
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for accType, pool := range c.Pools {
			if pool.Limit < 0 {
				// Unlimited sentinel: no finite cap. Represent it as an
				// unbounded budget (math.MaxInt) so the optimizer allocates the
				// type up to the model's fair-share demand. Leaving it absent
				// would let fairShareRolePick read a 0 budget and silently deny
				// the type — inverting the -1 = unlimited semantic. A finite
				// pool from any provider still wins via the min() below, since
				// Available() < math.MaxInt; only set the sentinel when no finite
				// budget is present yet.
				if _, ok := merged[accType]; !ok {
					merged[accType] = math.MaxInt
				}
				continue
			}
			if existing, ok := merged[accType]; !ok || pool.Available() < existing {
				merged[accType] = pool.Available()
			}
		}
	}
	return merged
}

// mergeNamespaceConstraints merges the per-(namespace, accelerator type) pools
// across providers into available-GPU budgets, taking the most restrictive
// (minimum available) where providers overlap. Returns nil when no provider
// carries namespace pools, so the per-type-only path is unaffected.
//
// A namespace present in any provider's NamespacePools is materialized in the
// result even when its inner map is empty — its presence marks a CLOSED
// allowlist (deny-all when empty) that the optimizer enforces by allocating
// only the listed types. An unlimited (-1 sentinel) pool is carried through as
// a negative budget so the optimizer can distinguish "unlimited" from a finite
// cap; tighterBudget treats it as +infinity when taking the minimum.
func mergeNamespaceConstraints(constraints []*ResourceConstraints) map[string]map[string]int {
	var merged map[string]map[string]int
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for ns, perType := range c.NamespacePools {
			if merged == nil {
				merged = make(map[string]map[string]int)
			}
			inner, ok := merged[ns]
			if !ok {
				inner = make(map[string]int)
				merged[ns] = inner // present (even if empty) marks a closed namespace
			}
			for accType, pool := range perType {
				budget := nsPoolBudget(pool)
				if existing, ok := inner[accType]; ok {
					inner[accType] = tighterBudget(existing, budget)
				} else {
					inner[accType] = budget
				}
			}
		}
	}
	return merged
}

// nsPoolBudget returns a namespace ResourcePool's remaining budget for the
// optimizer — the available (not total) GPU count. A negative Limit is the
// "unlimited" sentinel and is preserved as -1; any other Limit yields the
// finite available count (Limit - Used, clamped to 0).
func nsPoolBudget(pool ResourcePool) int {
	if pool.Limit < 0 {
		return -1
	}
	return pool.Available()
}

// tighterBudget returns the more restrictive of two namespace budgets, treating
// a negative (unlimited) budget as +infinity. The result is unlimited only when
// both inputs are unlimited.
func tighterBudget(a, b int) int {
	switch {
	case a < 0:
		return b
	case b < 0:
		return a
	case b < a:
		return b
	default:
		return a
	}
}

// scaleDownRoleIterated removes replicas role-by-role using the generalized
// scaleDownVariantSet primitive. Roles are sorted for determinism.
// Arity-1 (roles=["both"]) handles non-disaggregated models.
func scaleDownRoleIterated(
	ctx context.Context,
	e *NamedAnalyzerResult,
	variants []variantRecord,
	targets map[string]int,
	stateMap ...map[string]domain.VariantReplicaState,
) {
	var states map[string]domain.VariantReplicaState
	if len(stateMap) > 0 {
		states = stateMap[0]
	}
	roles := rolesOf(variants)

	for _, role := range roles {
		if !needsScaleDownForRole(*e, role) {
			continue
		}
		roleVCs := variantsForRole(variants, role)
		if len(roleVCs) == 0 {
			continue
		}
		sorted := sortVariantsForScaleDown(*e, roleVCs)
		scaleDownVariantSet(ctx, sorted, targets, states,
			func(vc variantRecord) int {
				return safeRemovalReplicasForRole(*e, vc.VariantName, role)
			},
			func(vc variantRecord, n int) {
				applyDeallocationForRole(e, vc.VariantName, role, n)
			},
		)
	}
}

// Ensure CostAwareOptimizer implements ScalingOptimizer
var _ ScalingOptimizer = (*CostAwareOptimizer)(nil)
