package allocation

import (
	"context"
	"maps"
	"math"
	"slices"
	"sort"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// rolesOf returns the distinct roles among the given variants, sorted for
// determinism. A variant with no role is the synthetic RoleBoth.
func rolesOf(vcs []variantRecord) []string {
	set := make(map[string]struct{}, len(vcs))
	for _, vc := range vcs {
		r := vc.Role
		if r == "" {
			r = domain.RoleBoth
		}
		set[r] = struct{}{}
	}
	return slices.Sorted(maps.Keys(set))
}

// Sentinel VariantCapacity.Reason values that indicate a variant carries no
// usable capacity signal (see domain.VariantCapacity.Reason doc). Analyzers
// that skip a variant entirely on failure (e.g. throughput's ITL-model
// resolution) never emit these — the variant is simply absent from
// VariantCapacities, which ResultIsInformative also treats as uninformative.
//
// These are the single source of truth for the no-data/error sentinels:
// producer packages (e.g. saturation_v2) reference them rather than
// re-declaring the literals, so ResultIsInformative and the producers cannot
// drift apart.
const (
	// ReasonNoData marks a variant for which the analyzer had no usable input
	// (no live replicas and no store record).
	ReasonNoData = domain.ReasonNoData
	// ReasonError marks a variant whose capacity could not be resolved due to
	// an internal analyzer error.
	ReasonError = domain.ReasonError
)

// ResultIsInformative reports whether nr carries a usable capacity signal:
// a non-nil Result with at least one VariantCapacity whose Reason is not a
// no-data/error sentinel. Used by the engine to decide whether to refresh
// the analyzer's last-good-analysis timestamp for the liveness gate.
func ResultIsInformative(nr NamedAnalyzerResult) bool {
	if nr.Result == nil {
		return false
	}
	for _, vc := range nr.Result.VariantCapacities {
		if vc.Reason != ReasonNoData && vc.Reason != ReasonError {
			return true
		}
	}
	return false
}

// applyAllocation subtracts the capacity provided by n replicas of variant v
// from the entry's Remaining counter. Clamps to 0. Result.RequiredCapacity is
// never mutated.
//
// Contract: Remaining/Spare are engine-calibrated on entry (via the universal
// threshold post-step). Helpers do not read or mutate PendingReplicas.
func applyAllocation(e *NamedAnalyzerResult, v string, n int) {
	if e.Result == nil {
		return
	}
	prc := prcForVariant(e.Result, v)
	if prc <= 0 {
		return
	}
	e.Remaining -= float64(n) * prc
	if e.Remaining < 0 {
		e.Remaining = 0
	}
}

// prcForVariant returns the PerReplicaCapacity for variant v in result r.
// Returns 0 if the variant is not present.
func prcForVariant(r *domain.AnalyzerResult, v string) float64 {
	for _, vc := range r.VariantCapacities {
		if vc.VariantName == v {
			return vc.PerReplicaCapacity
		}
	}
	return 0
}

// =============================================================================
// Paired helpers — disaggregated (P/D) models
// =============================================================================

// initRoleState initialises picker-local role state for one model's allocation pass.
// It unifies disaggregated and non-disaggregated models into one (model, role) view:
//
//   - Disaggregated (RoleCapacities != nil): roles = sorted keys of RoleCapacities;
//     per-role RC → pickerState[role]; per-role SC → e.RoleSpare[role].
//   - Non-disaggregated (RoleCapacities == nil): one synthetic role "both" using
//     the engine-calibrated model-level RC/SC (via the Remaining/Spare working
//     copies). No re-aggregation — the engine already summed all variants into
//     those scalars.
//
// Returns the list of active roles and the picker-local RolePairedState.
// Remaining/Spare scalars on NamedAnalyzerResult are read-only after this call;
// all dynamic bookkeeping moves to pickerState (scale-up) and RoleSpare (scale-down).
//
// # Role-visibility contract
//
// Roles are derived exclusively from the composite result's own RoleCapacities map keys.
// A role that exists in discovery but has no analyzer-attributed capacity entry is
// permanently invisible to scale-up until the analyzer starts emitting demand for it.
//
// This is NOT a complete gap: for disaggregated models, saturation's
// estimateSchedulerQueueDemand (internal/engines/analyzers/saturation_v2/analyzer.go)
// provides a purpose-built demand estimate for zero-replica roles from EPP queue-depth
// signals, producing a real nonzero RequiredCapacity that does trigger scale-up for that
// role before any replica of it exists — covering the ordinary cold-start case.
//
// The remaining, narrower gap has two cases:
//   (a) No EPP queue signal (SchedulerQueue == nil or empty): the estimator returns
//       all-zeros, and scale-up for the missing role cannot be triggered.
//   (b) The role is absent from VariantStates/VariantCapacities entirely (discovery-side
//       omission): neither the capacity store nor the queue estimator can help, since both
//       are iterated from the same variant list that is already missing the entry.
//
// Mixed-P/D+"both" models (disaggregated roles AND a "both" role simultaneously) are not
// supported today: initRoleState assigns a model to exactly one of the two branches above.
// This is a shallow implementation choice — no deep type constraint prevents lifting it —
// and is explicitly deferred as a future requirement.

func initRoleState(e *NamedAnalyzerResult) (roles []string, pickerState RolePairedState) {
	pickerState = make(RolePairedState)
	roleSet := make(map[string]struct{})

	if e.Result == nil {
		return nil, pickerState
	}
	if e.RoleCapacities != nil {
		// Disaggregated: per-role RC/SC from engine-calibrated RoleCapacities.
		if e.RoleSpare == nil {
			e.RoleSpare = make(map[string]float64, len(e.RoleCapacities))
		}
		for role, rc := range e.RoleCapacities {
			pickerState[role] = rc.RequiredCapacity
			e.RoleSpare[role] = rc.SpareCapacity
			roleSet[role] = struct{}{}
		}
	} else {
		// Non-disaggregated: synthesize a single "both" role from model-level scalars.
		pickerState[domain.RoleBoth] = e.Remaining
		if e.RoleSpare == nil {
			e.RoleSpare = make(map[string]float64, 1)
		}
		e.RoleSpare[domain.RoleBoth] = e.Spare
		roleSet[domain.RoleBoth] = struct{}{}
	}

	roles = make([]string, 0, len(roleSet))
	for role := range roleSet {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles, pickerState
}

// =============================================================================
// Paired helpers — role-generic scale-up and scale-down
// =============================================================================
//
// Design § Architecture/D: (model, role) is the unit of allocation math.
// Per-role sizing is independent, scoped to each role's picker-local demand.
// The joint-commit step bounds by the min-util role (the coupling constraint).
//
// RolePairedState holds picker-local per-role demand tracked during one
// model's allocation pass. Maps role → remaining demand (in that role's own
// capacity units). Initialized from RoleCapacities[role].RC; decremented per
// joint commit. Lives only inside the allocation loop — not stored on
// NamedAnalyzerResult (per design A10).
type RolePairedState map[string]float64

// roleBottleneckReplicas returns ceil(state[role] / PRC[v]) for the single
// entry. Returns 0 if the entry has no result or PRC ≤ 0.
func roleBottleneckReplicas(e NamedAnalyzerResult, state RolePairedState, role, v string) int {
	if e.Result == nil {
		return 0
	}
	prc := prcForVariant(e.Result, v)
	if prc <= 0 {
		return 0
	}
	return int(math.Ceil(state[role] / prc))
}

// roleAggRemaining returns the remaining demand for role from the picker state.
func roleAggRemaining(state RolePairedState, role string) float64 {
	return state[role]
}

// anyRoleNeedsScaleUp is the per-role scale-up gate for the unified dispatcher.
// Returns true when any role has remaining demand > 0.
func anyRoleNeedsScaleUp(state RolePairedState, roles []string) bool {
	for _, role := range roles {
		if state[role] > 0 {
			return true
		}
	}
	return false
}

// variantsForRole returns the capacities whose role matches role exactly,
// canonicalizing an empty Role to domain.RoleBoth.
func variantsForRole(vcs []variantRecord, role string) []variantRecord {
	out := make([]variantRecord, 0, len(vcs))
	for _, vc := range vcs {
		r := vc.Role
		if r == "" {
			r = domain.RoleBoth
		}
		if r == role {
			out = append(out, vc)
		}
	}
	return out
}

// safeRemovalReplicasForRole returns the number of replicas of variant v that
// can safely be removed — floor(RoleSpare[role] / PRC[v]) for the entry if it
// is live, has a Result and RoleSpare, and PRC > 0. Returns 0 if the entry is
// not live, has no Result/RoleSpare, PRC ≤ 0, or RoleSpare[role] < 0.
func safeRemovalReplicasForRole(e NamedAnalyzerResult, v, role string) int {
	if !e.Live {
		return 0 // non-live: no current basis to constrain removal
	}
	if e.Result == nil || e.RoleSpare == nil {
		return 0
	}
	prc := prcForVariant(e.Result, v)
	if prc <= 0 {
		return 0
	}
	n := int(math.Floor(e.RoleSpare[role] / prc))
	if n < 0 {
		return 0
	}
	return n
}

// applyDeallocationForRole decrements the entry's RoleSpare[role] by
// n × PRC[v]. Clamps to 0. Never mutates Result.
func applyDeallocationForRole(e *NamedAnalyzerResult, v, role string, n int) {
	if e.Result == nil || e.RoleSpare == nil {
		return
	}
	prc := prcForVariant(e.Result, v)
	if prc <= 0 {
		return
	}
	e.RoleSpare[role] -= float64(n) * prc
	if e.RoleSpare[role] < 0 {
		e.RoleSpare[role] = 0
	}
}

// needsScaleDownForRole reports whether the single live entry agrees this role
// has spare capacity. Returns false if the entry is not live, has no
// Result/RoleSpare, or RoleSpare[role] ≤ 0. Safety floor: a non-live entry
// means no current basis to scale down.
func needsScaleDownForRole(e NamedAnalyzerResult, role string) bool {
	if !e.Live {
		return false // non-live: no current basis to scale down
	}
	if e.Result == nil || e.RoleSpare == nil || e.RoleSpare[role] <= 0 {
		return false
	}
	return true
}

// RolePickFn is the role-generic optimizer variant selector for the unified
// allocateForModelPaired loop. Called once per role per iteration; returns the
// chosen variant and its resource cap. Returning ("", 0) signals no variant
// is available for this role.
type RolePickFn func(
	role string,
	variants []variantRecord,
	stateMap map[string]domain.VariantReplicaState,
	available map[string]int,
	targets map[string]int,
) (variant string, capN int)

// allocateForModelPaired is the Phase-3 role-generic scale-up loop.
// Handles any set of roles (including the arity-1 "both" single-role case).
// Per iteration: pick one variant per role, size independently, compute
// Δ_util = min_role util_role, trim to matched joint commit.
// Arity-1 (roles = ["both"]) reduces to plain per-variant allocation.
func allocateForModelPaired(
	ctx context.Context,
	e *NamedAnalyzerResult,
	variants []variantRecord,
	stateMap map[string]domain.VariantReplicaState,
	available map[string]int,
	targets map[string]int,
	pick RolePickFn,
	pickerState RolePairedState,
	roles []string,
) {
	logger := ctrl.LoggerFrom(ctx)
	for anyRoleNeedsScaleUp(pickerState, roles) {
		variantByRole := make(map[string]string, len(roles))
		capByRole := make(map[string]int, len(roles))
		prcByRole := make(map[string]float64, len(roles))
		allPicked := true
		for _, role := range roles {
			v, capN := pick(role, variants, stateMap, available, targets)
			if v == "" {
				allPicked = false
				break
			}
			variantByRole[role] = v
			capByRole[role] = capN
			prcByRole[role] = prcFromVCs(variants, v)
		}
		if !allPicked {
			break
		}

		nByRole := make(map[string]int, len(roles))
		utilByRole := make(map[string]float64, len(roles))
		for _, role := range roles {
			prc := prcByRole[role]
			n := min(roleBottleneckReplicas(*e, pickerState, role, variantByRole[role]), capByRole[role])
			nByRole[role] = n
			demand := roleAggRemaining(pickerState, role)
			if demand <= 0 {
				utilByRole[role] = 1.0
			} else {
				utilByRole[role] = float64(n) * prc / demand
			}
		}

		deltaUtil := math.MaxFloat64
		for _, role := range roles {
			if utilByRole[role] < deltaUtil {
				deltaUtil = utilByRole[role]
			}
		}
		if deltaUtil <= 0 {
			break
		}

		kByRole := make(map[string]int, len(roles))
		anyPositive := false
		for _, role := range roles {
			demand := roleAggRemaining(pickerState, role)
			prc := prcByRole[role]
			n := nByRole[role]
			k := 0
			if prc > 0 && demand > 0 {
				k = max(int(math.Floor(deltaUtil*demand/prc)), min(1, n))
			}
			kByRole[role] = k
			if k > 0 {
				anyPositive = true
			}
		}
		if !anyPositive {
			break
		}

		for _, role := range roles {
			v := variantByRole[role]
			k := kByRole[role]
			prc := prcByRole[role]
			targets[v] += k
			pickerState[role] = math.Max(0, pickerState[role]-float64(k)*prc)
			if available != nil {
				available[accFromVCs(variants, v)] -= k * gpusPerReplicaFromState(stateMap, v)
			}
		}
		// Update model-level Remaining via the P-anchor role so fairShareValue
		// reflects committed capacity. For "both" (non-disaggregated) use the
		// single role; for P/D prefer "prefill".
		for _, anchor := range []string{"prefill", domain.RoleBoth} {
			if v, ok := variantByRole[anchor]; ok {
				applyAllocation(e, v, kByRole[anchor])
				break
			}
		}
		logger.V(logging.DEBUG).Info("scale-up: joint role commit", "deltaUtil", deltaUtil)
	}
}
