// Package policy decides what the warm pool should do, and nothing about how.
//
// Everything here is a pure function of what was observed: the memberships the
// pool reports, what each variant wants, and the clock. That is deliberate --
// the mechanism was the easy half, and this is where a pool becomes either
// insurance or an expensive way to hold GPUs idle. Pure functions let those
// decisions be argued with in tests rather than on a cluster.
package policy

import (
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

// Config is the pool's operating policy.
type Config struct {
	// SleepMinSize is the floor on FREE Pods: the reserve kept for the next
	// spike. Per pool, not per model -- a per-model reserve would be N GPUs for
	// N models, which is the per-model headroom a pool exists to beat.
	SleepMinSize int
	// MaxHold bounds a borrow. A bridge whose ordinary replicas never arrive is
	// returned anyway, because holding indefinitely turns insurance into
	// capacity for whichever variant spiked first.
	MaxHold time.Duration
	// AdmissionWindow and MinMissesToAdmit are the frequency filter: a model is
	// admitted on its Nth miss within the window, not its first, so models that
	// spike once do not evict models that spike often.
	AdmissionWindow  time.Duration
	MinMissesToAdmit int
	// PreloadTop admits the N most-requested variants regardless of misses.
	// Prefetch beats demand-filling when the distribution is known.
	PreloadTop int
	// MaxInstancesPerPod bounds a Pod's warm set.
	MaxInstancesPerPod int
}

// VariantDemand is what WVA has decided about one variant, plus what the pool
// needs to know to serve it.
type VariantDemand struct {
	Model pool.ModelRef
	// Desired and Ready are ORDINARY replicas -- the scale target's, never the
	// pool's. Counting lent Pods as capacity here is what would let the pool
	// suppress the scale-up it exists to cover.
	Desired int
	Ready   int
	// Parked means the variant is at zero replicas by choice. Its wake is the
	// case with no alternative: without a warm copy it pays a full cold start.
	Parked bool
	// Share is this variant's fraction of requests, used for preloading.
	Share float64
}

// Borrow identifies one lent Pod.
type Borrow struct {
	Pod     types.NamespacedName
	Variant string
}

// Input is everything Decide is allowed to look at.
type Input struct {
	Memberships []pool.Membership
	Variants    []VariantDemand
	// BorrowedAt is when each lend began, for the hold timeout. Absent entries
	// are treated as starting now, which is the safe direction after a restart:
	// a bridge is held a little too long rather than cut mid-flight.
	BorrowedAt map[Borrow]time.Time
	// MissesAt records recent misses per variant, for the frequency filter.
	MissesAt map[string][]time.Time
	Now      time.Time
}

// Action is one thing to do to one model in one Pod.
type Action struct {
	Pod   types.NamespacedName
	Model pool.ModelRef
}

// Plan is what to do this cycle. Ordering between the lists matters: returns
// free Pods that borrows may then use, and admissions are decided last because
// they must not spend the reserve.
type Plan struct {
	Return []Action
	Borrow []Action
	Admit  []Action
	Evict  []Action
	// GrowBy is how many Pods short of the reserve the pool is, after this
	// plan. The caller decides whether that shortfall has lasted long enough to
	// act on -- growth costs a full model load, so it must not chase a
	// transient borrow.
	GrowBy int
	// Blocked and Missed name the variants the pool could not help, and why.
	// They are different faults: blocked means the reserve was too small,
	// missed means the warm set was wrong.
	Blocked []string
	Missed  []string
}

// Decide produces the plan. It never mutates its input.
func Decide(in Input, cfg Config) Plan {
	plan := Plan{}
	byPod := pool.ByPod(in.Memberships)
	lent := lentByVariant(in.Memberships)

	// 1. Returns first: a handed-back Pod is reserve for every model, and may
	//    serve a borrow decided below.
	returned := map[types.NamespacedName]bool{}
	for _, v := range in.Variants {
		for _, action := range returnsFor(v, lent[v.Model.Variant], in, cfg) {
			plan.Return = append(plan.Return, action)
			returned[action.Pod] = true
		}
	}

	// 2. Borrows: cover what the ordinary replicas cannot yet.
	free := freePods(byPod, returned)
	for _, v := range in.Variants {
		shortfall := v.Desired - v.Ready - len(lent[v.Model.Variant])
		if shortfall <= 0 {
			continue
		}
		candidates := holding(byPod, free, v.Model.Variant)
		if len(candidates) == 0 {
			plan.Missed = append(plan.Missed, v.Model.Variant)
			continue
		}
		borrowed := 0
		for _, p := range candidates {
			if borrowed == shortfall {
				break
			}
			plan.Borrow = append(plan.Borrow, Action{Pod: p, Model: v.Model})
			delete(free, p)
			borrowed++
		}
		if borrowed < shortfall {
			// The reserve ran out with the model resident: more Pods would have
			// helped, unlike a miss.
			plan.Blocked = append(plan.Blocked, v.Model.Variant)
		}
	}

	// 3. Admissions, last and most cautiously: never spend the reserve to fill
	//    the cache, and never during a burst.
	if len(plan.Blocked) == 0 {
		plan.Admit = admissions(in, cfg, byPod, free)
	}

	// 4. What the reserve will be once this plan is carried out.
	if short := cfg.SleepMinSize - (len(free) - len(plan.Admit)); short > 0 {
		plan.GrowBy = short
	}
	return plan
}

// returnsFor decides which of a variant's lent Pods go back.
//
// One formula covers both cases the design distinguishes: handover, when the
// ordinary replicas have arrived, and scale-down, where borrowed capacity is
// given up BEFORE the steady state shrinks. In both, what is returned is the
// lending beyond what the ordinary replicas still fail to cover.
func returnsFor(v VariantDemand, lent []pool.Membership, in Input, cfg Config) []Action {
	stillNeeded := v.Desired - v.Ready
	if stillNeeded < 0 {
		stillNeeded = 0
	}
	excess := len(lent) - stillNeeded

	// Oldest first: the longest-held bridge is the one most likely to be stuck,
	// and the one whose Pod has been out of the reserve longest.
	sort.Slice(lent, func(i, j int) bool {
		return borrowedAt(in, lent[i]).Before(borrowedAt(in, lent[j]))
	})

	var out []Action
	for i, m := range lent {
		expired := in.Now.Sub(borrowedAt(in, m)) >= cfg.MaxHold
		if i < excess || expired {
			out = append(out, Action{Pod: m.Pod, Model: m.Model})
		}
	}
	return out
}

// admissions chooses what to make resident, in descending order of confidence:
// parked variants, then the most popular, then anything that has missed often
// enough to look like a pattern rather than an accident.
func admissions(in Input, cfg Config, byPod map[types.NamespacedName][]pool.Membership, free map[types.NamespacedName]bool) []Action {
	var out []Action
	budget := len(free) - cfg.SleepMinSize // never take the reserve below its floor
	if budget <= 0 {
		return nil
	}

	for _, v := range candidatesForAdmission(in, cfg) {
		if budget == 0 {
			break
		}
		if residentSomewhere(byPod, v.Model.Variant) {
			continue
		}
		target, ok := roomiestFreePod(byPod, free, cfg)
		if !ok {
			break
		}
		out = append(out, Action{Pod: target, Model: v.Model})
		delete(free, target)
		budget--
	}
	return out
}

// candidatesForAdmission ranks the variants worth warming.
func candidatesForAdmission(in Input, cfg Config) []VariantDemand {
	byShare := append([]VariantDemand(nil), in.Variants...)
	sort.SliceStable(byShare, func(i, j int) bool { return byShare[i].Share > byShare[j].Share })

	popular := map[string]bool{}
	for i, v := range byShare {
		if i >= cfg.PreloadTop {
			break
		}
		popular[v.Model.Variant] = true
	}

	var out []VariantDemand
	for _, v := range byShare {
		switch {
		case v.Parked:
			// No ordinary replicas exist, so the pool is this variant's only
			// fast path, and the alternative is a guaranteed cold start.
			out = append(out, v)
		case popular[v.Model.Variant]:
			out = append(out, v)
		case missesInWindow(in, cfg, v.Model.Variant) >= cfg.MinMissesToAdmit && cfg.MinMissesToAdmit > 0:
			out = append(out, v)
		}
	}
	// Parked variants first, then by share, so a small budget is spent where
	// the alternative is worst.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Parked != out[j].Parked {
			return out[i].Parked
		}
		return out[i].Share > out[j].Share
	})
	return out
}

func missesInWindow(in Input, cfg Config, variant string) int {
	count := 0
	for _, at := range in.MissesAt[variant] {
		if in.Now.Sub(at) <= cfg.AdmissionWindow {
			count++
		}
	}
	return count
}

func lentByVariant(all []pool.Membership) map[string][]pool.Membership {
	out := map[string][]pool.Membership{}
	for _, m := range all {
		if m.State == pool.Serving || m.State == pool.Waking {
			out[m.Model.Variant] = append(out[m.Model.Variant], m)
		}
	}
	return out
}

func borrowedAt(in Input, m pool.Membership) time.Time {
	if at, ok := in.BorrowedAt[Borrow{Pod: m.Pod, Variant: m.Model.Variant}]; ok {
		return at
	}
	return in.Now
}

// freePods is the reserve: Pods that can serve a wake now, plus those this plan
// is about to hand back.
func freePods(byPod map[types.NamespacedName][]pool.Membership, returning map[types.NamespacedName]bool) map[types.NamespacedName]bool {
	free := map[types.NamespacedName]bool{}
	for p, inPod := range byPod {
		if pool.Free(inPod) || returning[p] {
			free[p] = true
		}
	}
	return free
}

func holding(byPod map[types.NamespacedName][]pool.Membership, free map[types.NamespacedName]bool, variant string) []types.NamespacedName {
	var out []types.NamespacedName
	for p := range free {
		for _, m := range byPod[p] {
			if m.Model.Variant == variant {
				out = append(out, p)
				break
			}
		}
	}
	// Deterministic, so a plan is reproducible and testable.
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func residentSomewhere(byPod map[types.NamespacedName][]pool.Membership, variant string) bool {
	for _, inPod := range byPod {
		for _, m := range inPod {
			if m.Model.Variant == variant {
				return true
			}
		}
	}
	return false
}

// roomiestFreePod spreads the warm set: the Pod holding fewest models takes the
// next one, so copies of popular models do not pile into one Pod that can serve
// only one wake at a time.
func roomiestFreePod(byPod map[types.NamespacedName][]pool.Membership, free map[types.NamespacedName]bool, cfg Config) (types.NamespacedName, bool) {
	best, found := types.NamespacedName{}, false
	for p := range free {
		if cfg.MaxInstancesPerPod > 0 && len(byPod[p]) >= cfg.MaxInstancesPerPod {
			continue
		}
		if !found || len(byPod[p]) < len(byPod[best]) ||
			(len(byPod[p]) == len(byPod[best]) && p.String() < best.String()) {
			best, found = p, true
		}
	}
	return best, found
}
