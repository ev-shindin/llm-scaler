package warmpool

import "time"

// ContentionMaxAge is how long a contention reading is believed.
//
// A pool held down by an observation nobody has refreshed is held down forever,
// and the failure is invisible -- it looks exactly like a pool that has all the
// Pods it wants. Two optimizer intervals is long enough to survive one slow
// pass and short enough that a stopped optimizer releases the pool quickly.
const ContentionMaxAge = 2 * time.Minute

// SizeFor is how many Pods a pool needs right now.
//
// PUBLISHED, not written. WVA computes this and KEDA scales the pool, exactly as
// it does for every model, which is what lets the controller stay read-only --
// internal/controller/rbac.go is explicit that a licence to resize workloads is
// the single permission a cluster admin is most right to refuse, and scaling
// through KEDA means the pool needs no write permission at all, not even a
// narrow one scoped to a single object.
//
// The formula is stateless on purpose:
//
//	lent + reserve + 1
//
// `lent` is the Pods currently bridging, which cannot be taken back without
// dropping live traffic. `reserve` is the floor of free Pods the pool keeps for
// the next spike. The `+1` is what makes the pool able to warm anything at all:
// admission draws on free-minus-reserve, so a pool sized exactly to its reserve
// sits at a budget of zero forever, holding accelerators and warming nothing.
//
// There is deliberately no hysteresis here. An earlier version counted
// consecutive short and idle passes to avoid growing on a single spike or
// shrinking on a lull -- which is exactly what an HPA's stabilization windows
// already do, better, in an object operators already read. Two copies of that
// logic are two things to tune that can disagree; the asymmetry a warm pool
// needs (grow promptly, shrink slowly) now lives in the ScaledObject's behavior
// block, where it is visible.
//
// Bounds are absent for the same reason: minReplicaCount and maxReplicaCount say
// it already. The one setting worth checking is a floor BELOW the reserve, which
// is the inert pool, and PoolSpec.Inert reports it.
func SizeFor(reserve, lent int) int {
	if reserve < 0 {
		reserve = 0
	}
	if lent < 0 {
		lent = 0
	}
	return lent + reserve + 1
}

// HoldAt is the size a pool publishes while a model replica of its accelerator
// type is being denied GPUs.
//
// The pool keeps what it holds and asks for nothing more. Growing is what has to
// stop: the pool takes accelerators by growing its own Deployment, and no
// limiter bounds that -- the optimizer's budgets govern what model REPLICAS may
// have, and a pool is not a model. Left alone, a burst blocks borrows, the pool
// asks for another Pod to relieve the block, and that Pod is a GPU a starved
// replica needed. The pool would be competing with the very scale-up it exists
// to bridge.
//
// It does not SHRINK, and that asymmetry is the point. A warm copy already
// loaded is worth more than the accelerator under it is worth to a replica that
// would need ~35 s to make use of it; yielding one would spend a real warm set
// to buy a slow start. Insurance stops buying more during a fire. It does not
// cancel itself.
//
// Publishing the current count is also safe against starving the pool: KEDA
// clamps to minReplicaCount, so the configured floor holds regardless of what
// this returns.
func HoldAt(current int) int {
	if current < 1 {
		return 1
	}
	return current
}
