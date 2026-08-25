package warmpool

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
