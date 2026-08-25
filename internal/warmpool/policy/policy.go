// Package policy decides what the warm pool should do, and nothing about how.
//
// Everything here is a pure function of what was observed: the memberships the
// pool reports, what each variant wants, and the clock. That is deliberate --
// the mechanism was the easy half, and this is where a pool becomes either
// insurance or an expensive way to hold GPUs idle. Pure functions let those
// decisions be argued with in tests rather than on a cluster.
package policy

import (
	"fmt"
	"slices"
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

	// PodMemoryBytes is how much HOST memory one Pod may commit to sleeping
	// models, and it is a hard wall rather than a target.
	//
	// A level-1 sleeper's weights go to SHARED memory, which the container's
	// cgroup is charged for -- measured at 2.6 GiB + 1.4x the weights. So
	// admitting one model too many does not fail that admission: it OOM-kills
	// the launcher and destroys every model already resident in the Pod.
	//
	// Clamped to the container's own limit at the point of use, so a budget
	// larger than the limit cannot pretend to be a wall. Zero means "use the
	// container's limit", which is the honest default.
	PodMemoryBytes int64
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
	// WarmPool is the pool this variant selected in its trigger metadata, or
	// empty to take the namespace's only pool. Selection, not identity, which is
	// why it sits here rather than on ModelRef.
	WarmPool string
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
	// Admitting are Pods with an admission already in flight, which the caller
	// knows about and an observation cannot.
	//
	// An admission is asynchronous and takes ~35 s, and the instance it creates
	// does not appear in the supervisor's listing until its create call has
	// landed. So a Pod chosen for one variant still reads as EMPTY to the next
	// pass, which picks the roomiest empty Pod, finds the same one, and admits
	// a second model into it against a budget that counts nothing yet. Two
	// loads then run in a Pod sized for the arithmetic of one, which is the OOM
	// that budget exists to prevent.
	Admitting map[types.NamespacedName]bool

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
	// Blocked and Missed are the variants the pool could not help, and why.
	// They are different faults: blocked means the reserve was too small,
	// missed means the warm set was wrong.
	//
	// Carried as models rather than names because a caller reporting them needs
	// the namespace too, and a variant that missed appears in no action list to
	// recover it from.
	Blocked []pool.ModelRef
	Missed  []pool.ModelRef

	// Declined are variants the pool will not warm at all, with the reason.
	// Distinct from Missed: a miss is a model the pool could have held and did
	// not, and says raise the warm set. A decline says the warm set could never
	// have held it, and no amount of raising anything will change that.
	Declined []Declined
}

// Declined is one variant the pool refused to warm.
type Declined struct {
	Model  pool.ModelRef
	Reason string
}

// Decide produces the plan. It never mutates its input.
func Decide(in Input, cfg Config) Plan {
	plan := Plan{}
	byPod := pool.ByPod(in.Memberships)
	lent := lentByVariant(in.Memberships)
	// What each variant's model REALLY looks like, including the InferencePool
	// selector. An observation cannot supply that: ListWarm learns a variant's
	// name and options from the supervisor, and the selector comes from the
	// tenant's InferencePool, which only demand has read.
	known := knownModels(in.Variants)

	// 1. Returns first: a handed-back Pod is reserve for every model, and may
	//    serve a borrow decided below.
	returned := map[types.NamespacedName]bool{}
	for _, v := range in.Variants {
		for _, action := range returnsFor(v, lent[v.Model.Variant], in, cfg) {
			plan.Return = append(plan.Return, withKnownModel(action, known))
			returned[action.Pod] = true
		}
	}
	// And every engine that is awake with nothing using it, whether or not any
	// variant asked for it back. These are not bridges anyone is holding; they
	// are GPUs being burned by a sequence that did not finish.
	orphans := orphanReturns(in.Memberships)
	for i, action := range orphans {
		orphans[i] = withKnownModel(action, known)
		plan.Return = append(plan.Return, orphans[i])
		returned[action.Pod] = true
	}

	// 2. Borrows: cover what the ordinary replicas cannot yet.
	free := freePods(byPod, returned, in.Admitting)
	for _, v := range in.Variants {
		shortfall := v.Desired - v.Ready - len(lent[v.Model.Variant])
		if shortfall <= 0 {
			continue
		}
		candidates := holding(byPod, free, v.Model.Variant)
		if len(candidates) == 0 {
			// Which fault this is decides which knob an operator turns, so the
			// two must not be conflated. `holding` searches FREE Pods only, so
			// a variant resident in Pods that are all lent or loading yields no
			// candidates -- and reporting that as a miss would say "the warm
			// set was wrong" when the truth is "the reserve was too small".
			if residentSomewhere(byPod, v.Model.Variant) {
				plan.Blocked = append(plan.Blocked, v.Model)
			} else {
				plan.Missed = append(plan.Missed, v.Model)
			}
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
			plan.Blocked = append(plan.Blocked, v.Model)
		}
	}

	// An orphan whose variant turned out to be short is not garbage: it is
	// already awake, for the right model. Sleeping it and waking it again
	// within one plan is pure cost -- a drain, a sleep, and a wake -- so the
	// return is dropped and the borrow finishes the activation the interrupted
	// sequence began.
	//
	// ORPHANS ONLY. A return that a variant actually asked for must survive
	// being re-borrowed in the same cycle, because the hold timeout is exactly
	// that case: a bridge whose ordinary replicas never arrived is returned
	// while the variant is still short, and the shortfall then takes it again.
	// Dropping that return would leave the bridge silently in place, and the
	// observation of how long it lasted -- the signal that a scale-up is
	// failing behind the pool -- would never be made.
	plan.Return = dropOrphanReturnsSupersededByBorrows(plan.Return, orphans, plan.Borrow)

	// 3. Admissions, last and most cautiously: never spend the reserve to fill
	//    the cache, and never during a burst.
	if len(plan.Blocked) == 0 {
		plan.Admit, plan.Declined = admissions(in, cfg, byPod, free)
	}

	// 4. What the reserve will be once this plan is carried out.
	//
	// `free` has already had borrows and admissions removed from it in place, so
	// it IS the post-plan reserve. Subtracting the admissions again would count
	// them twice and report a shortfall that does not exist.
	if short := cfg.SleepMinSize - len(free); short > 0 {
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
func admissions(in Input, cfg Config, byPod map[types.NamespacedName][]pool.Membership, free map[types.NamespacedName]bool) ([]Action, []Declined) {
	var out []Action
	var declined []Declined
	budget := len(free) - cfg.SleepMinSize // never take the reserve below its floor
	if budget <= 0 {
		return nil, nil
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
		if reason := doesNotFit(v.Model, byPod[target], cfg); reason != "" {
			// Skipped rather than breaking the loop: this variant does not fit,
			// but a smaller one further down the list still might.
			declined = append(declined, Declined{Model: v.Model, Reason: reason})
			continue
		}
		out = append(out, Action{Pod: target, Model: v.Model})
		delete(free, target)
		budget--
	}
	return out, declined
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

// lentByVariant is what the pool is currently covering: engines that are AWAKE
// AND BEING USED.
//
// Serving only. Waking used to count here too, on the reasoning that a Pod
// mid-activation is spoken for and must not be handed to someone else. That
// reasoning is sound but the state cannot occur at this point: a pass observes
// before it acts, passes are serialised, and Activate runs to completion inside
// the pass that decided it. So every Waking membership a decision ever sees is
// an engine that is awake and serving nothing -- see orphanReturns.
func lentByVariant(all []pool.Membership) map[string][]pool.Membership {
	out := map[string][]pool.Membership{}
	for _, m := range all {
		if m.State == pool.Serving {
			out[m.Model.Variant] = append(out[m.Model.Variant], m)
		}
	}
	return out
}

// doesNotFit reports why a model cannot be admitted into this Pod, or "" if it
// can.
//
// Two questions, and the second is the expensive one to get wrong. A Pod holds a
// fixed number of GPUs, so a tensor-parallel engine simply cannot start in it --
// that costs a wasted load. And a Pod has a hard memory limit that a level-1
// sleeper's weights count against, so one model too many does not fail its own
// admission, it takes the whole Pod down with every model already in it.
func doesNotFit(model pool.ModelRef, resident []pool.Membership, cfg Config) string {
	shape := pool.ShapeOf(model.EngineOptions)
	capacity := capacityOf(resident)

	// The POD is the only source. There used to be a --warm-pool-gpus-per-pod
	// flag beside this whose own help said it "must match the pool Deployment's
	// nvidia.com/gpu request" -- a second copy of a fact the Pod already states,
	// which could only ever agree or cause a silent decline. Every pool Pod
	// carries its declared capacity, including an EMPTY one (the placeholder
	// membership sets it), so there is nothing left for a fallback to cover.
	gpus := capacity.GPUs
	if gpus < 1 {
		gpus = 1
	}
	if shape.GPUs > gpus {
		return fmt.Sprintf("needs %d GPUs, this Pod holds %d", shape.GPUs, gpus)
	}

	// The right NUMBER of the wrong GPU is still the wrong GPU. A warm copy is
	// only reusable on the accelerator it was loaded on, so a pool of one type
	// cannot serve a model pinned to another: the bridge would place traffic on
	// hardware the workload's own affinity refuses, and the load that produced
	// it is spent for nothing.
	if why, bad := pool.AcceleratorMismatch(model.Accelerator, capacity.Accelerator); bad {
		return why
	}

	budget := cfg.PodMemoryBytes
	// Clamped to the container's own limit, never above it. A budget larger
	// than the limit is not a wall -- the kubelet enforces the limit whatever
	// the flag says -- so admitting against it would OOM the Pod and take every
	// model resident in it with it.
	if capacity.MemoryLimitBytes > 0 && (budget <= 0 || budget > capacity.MemoryLimitBytes) {
		budget = capacity.MemoryLimitBytes
	}
	if budget <= 0 {
		return "" // nothing to check against; the check is off
	}
	if !shape.Known() {
		// Declining is the conservative direction and the consistent one: the
		// engine-flag allowlist already refuses to guess, and a warm copy the
		// pool declines to make costs a cold start, where a warm copy that
		// does not fit costs every model in the Pod.
		return "its size cannot be worked out from its name, so it cannot be shown to fit"
	}

	committed := int64(0)
	for _, m := range resident {
		if held := pool.ShapeOf(m.Model.EngineOptions); held.Known() {
			committed += held.HostBytes
		}
	}
	if committed+shape.HostBytes > budget {
		return fmt.Sprintf("needs %s on top of the %s already resident, over the %s budget",
			gib(shape.HostBytes), gib(committed), gib(budget))
	}
	return ""
}

// capacityOf is what the Pod these memberships belong to declares. Every
// membership from one Pod carries the same value, so the first is enough.
func capacityOf(resident []pool.Membership) pool.PodCapacity {
	for _, m := range resident {
		if m.Capacity.GPUs > 0 || m.Capacity.MemoryLimitBytes > 0 {
			return m.Capacity
		}
	}
	return pool.PodCapacity{}
}

func gib(bytes int64) string {
	return fmt.Sprintf("%.1fGiB", float64(bytes)/(1<<30))
}

// knownModels indexes what demand knows about each variant.
func knownModels(variants []VariantDemand) map[string]pool.ModelRef {
	out := make(map[string]pool.ModelRef, len(variants))
	for _, v := range variants {
		out[v.Model.Variant] = v.Model
	}
	return out
}

// withKnownModel replaces an action's model with demand's fuller version.
//
// This is what makes a return able to UNDO its borrow. An action built from an
// observation carries no PoolLabels -- there is nowhere for ListWarm to learn
// them -- and Deactivate removes exactly the labels it is handed, so a return
// built that way silently removed nothing at all. The Pod kept every
// InferencePool selector it had ever served, and the next model woken in it
// went Ready as an endpoint of BOTH pools: the first model's traffic, answered
// by the second model's engine.
//
// A variant that demand no longer knows about keeps the observation's model.
// Nothing better is available, and the labels are then left behind -- which is
// worth knowing about, but is confined to variants that have been deregistered
// while lent.
func withKnownModel(action Action, known map[string]pool.ModelRef) Action {
	full, ok := known[action.Model.Variant]
	if !ok || len(full.PoolLabels) == 0 {
		return action
	}
	action.Model = full
	return action
}

// dropOrphanReturnsSupersededByBorrows removes an ORPHAN return whose Pod and
// variant are borrowed again in the same plan. Returns a variant asked for are
// left alone -- see the call site.
func dropOrphanReturnsSupersededByBorrows(returns, orphans, borrows []Action) []Action {
	if len(orphans) == 0 || len(borrows) == 0 {
		return returns
	}
	key := func(a Action) Borrow { return Borrow{Pod: a.Pod, Variant: a.Model.Variant} }

	redundant := make(map[Borrow]bool, len(orphans))
	for _, o := range orphans {
		redundant[key(o)] = true
	}
	borrowed := make(map[Borrow]bool, len(borrows))
	for _, b := range borrows {
		borrowed[key(b)] = true
	}
	return slices.DeleteFunc(returns, func(a Action) bool {
		return redundant[key(a)] && borrowed[key(a)]
	})
}

// orphanReturns hands back every engine that is awake with nothing using it.
//
// An engine reaches this state whenever the sequence that woke it did not
// finish: the controller restarted between a model load and the sleep that
// should have followed it, an Activate timed out between the wake and the
// re-point, or a Deactivate failed at its final step. In each case the engine
// holds its GPU, is in no InferencePool, and has no traffic pointed at it.
//
// Counting these as lent -- which is what the code did before -- makes the
// damage worse rather than better. The variant they are charged to reads as
// already covered, so a REAL borrow for it is suppressed, and nothing forces
// the situation open until the hold timeout expires. Since a restart also loses
// the borrow times, that timeout restarts from zero, so a controller restart
// that lands mid-admission could idle a GPU for the full hold period and
// silently withhold capacity from the variant that needed it.
//
// Returning them instead is both the smaller statement and the correct one: an
// engine nobody is using should be asleep, and the Pod it sits in should be back
// in the reserve. The return sleeps the model rather than evicting it, so the
// warm set survives.
func orphanReturns(all []pool.Membership) []Action {
	var out []Action
	for _, m := range all {
		if m.State == pool.Waking {
			out = append(out, Action{Pod: m.Pod, Model: m.Model})
		}
	}
	// Deterministic, so a plan is reproducible and testable.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pod != out[j].Pod {
			return out[i].Pod.String() < out[j].Pod.String()
		}
		return out[i].Model.Variant < out[j].Model.Variant
	})
	return out
}

func borrowedAt(in Input, m pool.Membership) time.Time {
	if at, ok := in.BorrowedAt[Borrow{Pod: m.Pod, Variant: m.Model.Variant}]; ok {
		return at
	}
	return in.Now
}

// freePods is the reserve: Pods that can serve a wake now, plus those this plan
// is about to hand back, minus any with an admission still in flight.
func freePods(byPod map[types.NamespacedName][]pool.Membership, returning, admitting map[types.NamespacedName]bool) map[types.NamespacedName]bool {
	free := map[types.NamespacedName]bool{}
	for p, inPod := range byPod {
		if admitting[p] {
			// Loading, even though the listing does not show it yet.
			continue
		}
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
	if variant == "" {
		return false // the empty variant marks an idle Pod, not a model
	}
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
		if cfg.MaxInstancesPerPod > 0 && pool.Resident(byPod[p]) >= cfg.MaxInstancesPerPod {
			continue
		}
		if !found || pool.Resident(byPod[p]) < pool.Resident(byPod[best]) ||
			(pool.Resident(byPod[p]) == pool.Resident(byPod[best]) && p.String() < best.String()) {
			best, found = p, true
		}
	}
	return best, found
}
