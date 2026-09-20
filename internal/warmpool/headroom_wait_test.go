package warmpool

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/policy"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

// The pool must not size itself against an allowance that has not yet charged
// the Pods it already holds.
//
// The race this closes, as it was measured: a pool with a one-GPU quota saw its
// first Pod, charged it, and on the SAME pass asked whether it could grow. The
// only headroom snapshot was from before the charge and said one GPU was free,
// so the pool asked for its reserve and KEDA created two more Pods -- three on a
// quota of one, and no cap was ever logged because once grown the pool never
// wanted more than it had. The existing e2e for the static quota fails this way
// on any cluster fast enough for the pool's pass to beat the optimize cycle.
//
// Three answers, three behaviours. The reconciler passes the version of the
// charge it just made; the store answers Unknown for a snapshot older than that.

// onePodPool is a pool holding one A100 Pod with a reserve of one, so it wants
// SizeFor(1, 0) = 2 and is at 1.
func onePodPool(t *testing.T) (*Reconciler, *int32) {
	t.Helper()
	held := withAccel(pool.Membership{Pod: podA(), State: pool.Absent, Pool: "p"}, "A100")
	held.Capacity.GPUs = 1
	p := &fakePool{memberships: []pool.Membership{held}}
	cfg := testConfig()
	cfg.SleepMinSize = 1

	var published int32
	r := New(p, &staticDemand{}, cfg)
	r.Namespace = poolNamespace
	r.Pools = fakePools{{Name: "p", Config: cfg, Replicas: 1, Deployment: "p"}}
	r.PublishSize = func(_, _ string, replicas int32) { published = replicas }
	return r, &published
}

func TestAPoolHoldsUntilTheAllowanceHasSeenItsPods(t *testing.T) {
	decision.ResetWarmPoolGPUs()
	t.Cleanup(decision.ResetWarmPoolGPUs)
	r, published := onePodPool(t)

	var askedVersion uint64
	r.Headroom = func(_, _ string, poolsVersion uint64) (int, decision.HeadroomState) {
		askedVersion = poolsVersion
		return 0, decision.HeadroomUnknown
	}

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if *published != 1 {
		t.Fatalf("with no usable headroom the pool must hold at its current 1, asked for %d", *published)
	}
	// The bar it set is this pass's own charge, not some earlier one.
	if askedVersion == 0 || askedVersion != decision.WarmPoolGPUsVersion() {
		t.Errorf("the pool asked for a snapshot at version %d, want the version of its own charge %d",
			askedVersion, decision.WarmPoolGPUsVersion())
	}
}

func TestAPoolGrowsOnceTheAllowanceIsKnownAndSufficient(t *testing.T) {
	decision.ResetWarmPoolGPUs()
	t.Cleanup(decision.ResetWarmPoolGPUs)
	r, published := onePodPool(t)
	r.Headroom = func(_, _ string, _ uint64) (int, decision.HeadroomState) {
		return 1, decision.HeadroomBounded // one more GPU is allowed
	}

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if *published != 2 {
		t.Fatalf("one GPU of headroom lets the pool reach 2, got %d", *published)
	}
}

func TestAPoolIsCappedAtWhatTheAllowanceLeaves(t *testing.T) {
	decision.ResetWarmPoolGPUs()
	t.Cleanup(decision.ResetWarmPoolGPUs)
	r, published := onePodPool(t)
	r.Headroom = func(_, _ string, _ uint64) (int, decision.HeadroomState) {
		return 0, decision.HeadroomBounded // the allowance is spent
	}

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if *published != 1 {
		t.Fatalf("a spent allowance caps the pool at its current 1, got %d", *published)
	}
}

func TestAPoolNoLimiterBoundsGrowsFreely(t *testing.T) {
	decision.ResetWarmPoolGPUs()
	t.Cleanup(decision.ResetWarmPoolGPUs)
	r, published := onePodPool(t)
	r.Headroom = func(_, _ string, _ uint64) (int, decision.HeadroomState) {
		return 0, decision.HeadroomUnbounded
	}

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if *published != 2 {
		t.Fatalf("an unbounded namespace must not hold the pool, got %d", *published)
	}
}

func TestAHeldPoolStillShrinksToWhatItNeeds(t *testing.T) {
	// Unknown headroom stops GROWTH. A pool above its need must still hand
	// Pods back -- the same rule contention follows, and for the same reason:
	// shrinking is the one thing a pool can do that never needs an allowance.
	decision.ResetWarmPoolGPUs()
	t.Cleanup(decision.ResetWarmPoolGPUs)
	idle := withAccel(pool.Membership{Pod: podA(), State: pool.Absent, Pool: "p"}, "H100")
	p := &fakePool{memberships: []pool.Membership{idle}}
	cfg := testConfig()
	cfg.SleepMinSize = 1

	var published int32
	r := New(p, &staticDemand{}, cfg)
	r.Namespace = poolNamespace
	r.Pools = fakePools{{Name: "p", Config: cfg, Replicas: 3, Deployment: "p"}}
	r.PublishSize = func(_, _ string, replicas int32) { published = replicas }
	r.Headroom = func(_, _ string, _ uint64) (int, decision.HeadroomState) { return 0, decision.HeadroomUnknown }

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if published != 2 {
		t.Fatalf("a held pool still releases what it does not need: got %d, want 2", published)
	}
}

func TestTheWaitIsStatedOncePerCharge(t *testing.T) {
	decision.ResetWarmPoolGPUs()
	t.Cleanup(decision.ResetWarmPoolGPUs)
	r, _ := onePodPool(t)
	r.Headroom = func(_, _ string, _ uint64) (int, decision.HeadroomState) { return 0, decision.HeadroomUnknown }

	for i := 0; i < 3; i++ {
		if _, err := r.Once(context.Background()); err != nil {
			t.Fatalf("Once: %v", err)
		}
	}
	// Three passes with the same charge is one wait; the dedupe key is the
	// version, and the version did not move because the figure did not change.
	if got := r.lastAwaiting["p"]; got != decision.WarmPoolGPUsVersion() {
		t.Errorf("the wait recorded version %d, want %d", got, decision.WarmPoolGPUsVersion())
	}
	if len(r.lastAwaiting) != 1 {
		t.Errorf("one pool waiting, %d recorded", len(r.lastAwaiting))
	}
}

// threePodLendingPool is a pool at THREE Pods above a floor of one: two idle,
// one lent to "qwen". Reserve one, so it wants SizeFor(1, 1) = 3 -- exactly
// what it holds. PoolSpec.Replicas is the ScaledObject floor, not the size, and
// every hold and cap has to know the difference: computed from the floor they
// would tell KEDA to take two Pods away, the bridge among them.
func threePodLendingPool(t *testing.T) (*Reconciler, *int32) {
	t.Helper()
	pods := []types.NamespacedName{
		{Namespace: poolNamespace, Name: "p-0"},
		{Namespace: poolNamespace, Name: "p-1"},
		{Namespace: poolNamespace, Name: "p-2"},
	}
	lent := withAccel(pool.Membership{Pod: pods[0], State: pool.Serving, Pool: "p", Model: model("qwen")}, "A100")
	lent.Capacity.GPUs = 1
	idle1 := withAccel(pool.Membership{Pod: pods[1], State: pool.Absent, Pool: "p"}, "A100")
	idle1.Capacity.GPUs = 1
	idle2 := withAccel(pool.Membership{Pod: pods[2], State: pool.Absent, Pool: "p"}, "A100")
	idle2.Capacity.GPUs = 1
	p := &fakePool{memberships: []pool.Membership{lent, idle1, idle2}}
	cfg := testConfig()
	cfg.SleepMinSize = 1

	var published int32
	r := New(p, &staticDemand{}, cfg)
	r.Namespace = poolNamespace
	r.Pools = fakePools{{Name: "p", Config: cfg, Replicas: 1, Deployment: "p"}}
	r.PublishSize = func(_, _ string, replicas int32) { published = replicas }
	r.borrowedAt[policy.Borrow{Pod: pods[0], Variant: "qwen"}] = time.Now()
	return r, &published
}

func TestAHoldIsAtThePodsHeldNotTheScaledObjectFloor(t *testing.T) {
	decision.ResetWarmPoolGPUs()
	t.Cleanup(decision.ResetWarmPoolGPUs)
	r, published := threePodLendingPool(t)
	r.Headroom = func(_, _ string, _ uint64) (int, decision.HeadroomState) { return 0, decision.HeadroomUnknown }

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if *published != 3 {
		t.Fatalf("a pool at 3 held on an unknown allowance must stay at 3, not fall to the floor: got %d", *published)
	}
}

func TestACapIsWhatThePoolHoldsPlusWhatIsLeft(t *testing.T) {
	// Quota exactly spent by this pool: three GPUs, three Pods. Free is zero,
	// and the pool is entitled to every one of the three. Capping it at
	// floor+free = 1 would shrink it, free two GPUs, and grow it back -- the
	// sawtooth the quota e2e cannot see because it runs with floor == held.
	decision.ResetWarmPoolGPUs()
	t.Cleanup(decision.ResetWarmPoolGPUs)
	r, published := threePodLendingPool(t)
	r.Headroom = func(_, _ string, _ uint64) (int, decision.HeadroomState) { return 0, decision.HeadroomBounded }

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if *published != 3 {
		t.Fatalf("a pool holding exactly its quota keeps it: got %d, want 3", *published)
	}
}

func TestAWaitThatRecursIsReportedAgain(t *testing.T) {
	// The snapshot arrives, the pool grows; later the snapshot ages out with
	// the figure unchanged. That is a new wait and must be said again, or a
	// pool that quietly stopped growing reads as one that is simply small.
	decision.ResetWarmPoolGPUs()
	t.Cleanup(decision.ResetWarmPoolGPUs)
	r, _ := onePodPool(t)
	state := decision.HeadroomUnknown
	r.Headroom = func(_, _ string, _ uint64) (int, decision.HeadroomState) { return 1, state }

	once := func() {
		if _, err := r.Once(context.Background()); err != nil {
			t.Fatalf("Once: %v", err)
		}
	}
	once() // waits: recorded
	if len(r.lastAwaiting) != 1 {
		t.Fatalf("the first wait must be recorded, have %v", r.lastAwaiting)
	}
	state = decision.HeadroomBounded
	once() // answered: the record is cleared
	if len(r.lastAwaiting) != 0 {
		t.Fatalf("an answered wait must clear its record, have %v", r.lastAwaiting)
	}
	state = decision.HeadroomUnknown
	once() // waits again at the same version: recorded again, so it was logged again
	if len(r.lastAwaiting) != 1 {
		t.Fatalf("a recurring wait must be recorded (and so reported) again, have %v", r.lastAwaiting)
	}
}

func TestAWaitEndedByWantingLessIsAlsoForgotten(t *testing.T) {
	// The wait ends because the pool stopped wanting more (its reserve was
	// lowered), not because the allowance answered. The record must clear all
	// the same, or the next genuine wait at this version is silent.
	decision.ResetWarmPoolGPUs()
	t.Cleanup(decision.ResetWarmPoolGPUs)
	r, _ := onePodPool(t)
	r.Headroom = func(_, _ string, _ uint64) (int, decision.HeadroomState) { return 0, decision.HeadroomUnknown }

	once := func() {
		if _, err := r.Once(context.Background()); err != nil {
			t.Fatalf("Once: %v", err)
		}
	}
	once()
	if len(r.lastAwaiting) != 1 {
		t.Fatalf("the wait must be recorded, have %v", r.lastAwaiting)
	}
	// Reserve 0: wants SizeFor(0, 0) = 1, which it has. No wait this pass.
	cfg := testConfig()
	cfg.SleepMinSize = 0
	r.Pools = fakePools{{Name: "p", Config: cfg, Replicas: 1, Deployment: "p"}}
	once()
	if len(r.lastAwaiting) != 0 {
		t.Fatalf("a pass without a wait must clear the record, have %v", r.lastAwaiting)
	}
}

// A pool with no readable Pod cannot name an accelerator. Under a bounded
// namespace it holds until it can; under no limiter it grows. This was the last
// overshoot: the window before the first Pod is readable let the pool ask for
// its reserve with no check, and once up it never wanted more than it held, so
// the extra Pod was permanent.
func TestAPoolWithNoReadablePodHoldsUnderAQuota(t *testing.T) {
	decision.ResetWarmPoolGPUs()
	t.Cleanup(decision.ResetWarmPoolGPUs)
	p := &fakePool{memberships: nil}
	cfg := testConfig()
	cfg.SleepMinSize = 1

	var published int32
	var asked string
	r := New(p, &staticDemand{}, cfg)
	r.Namespace = poolNamespace
	r.Pools = fakePools{{Name: "p", Config: cfg, Replicas: 1, Deployment: "p"}}
	r.PublishSize = func(_, _ string, replicas int32) { published = replicas }
	r.Headroom = func(_, accel string, _ uint64) (int, decision.HeadroomState) {
		asked = accel
		return 0, decision.HeadroomBounded // the namespace is bounded; "" is a type it does not name
	}

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if asked != "" {
		t.Errorf("with no Pod the pool has no accelerator to ask about, asked %q", asked)
	}
	if published != 1 {
		t.Fatalf("a bounded namespace holds a pool that cannot yet name its accelerator: got %d, want 1", published)
	}

	r.Headroom = func(_, _ string, _ uint64) (int, decision.HeadroomState) { return 0, decision.HeadroomUnbounded }
	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if published != 2 {
		t.Fatalf("no limiter: the pool grows without a Pod to read, got %d, want 2", published)
	}
}

func TestAPoolWhosePodsCostNoGPUsIsNotCapped(t *testing.T) {
	// An emulated pool declares no devices. It costs the allowance nothing, so
	// a bounded namespace with nothing left must not hold it.
	decision.ResetWarmPoolGPUs()
	t.Cleanup(decision.ResetWarmPoolGPUs)
	idle := withAccel(pool.Membership{Pod: podA(), State: pool.Absent, Pool: "p"}, "A100") // Capacity.GPUs 0
	p := &fakePool{memberships: []pool.Membership{idle}}
	cfg := testConfig()
	cfg.SleepMinSize = 1

	var published int32
	r := New(p, &staticDemand{}, cfg)
	r.Namespace = poolNamespace
	r.Pools = fakePools{{Name: "p", Config: cfg, Replicas: 1, Deployment: "p"}}
	r.PublishSize = func(_, _ string, replicas int32) { published = replicas }
	r.Headroom = func(_, _ string, _ uint64) (int, decision.HeadroomState) { return 0, decision.HeadroomBounded }

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if published != 2 {
		t.Fatalf("a pool costing no GPUs grows under a spent allowance: got %d, want 2", published)
	}
}
