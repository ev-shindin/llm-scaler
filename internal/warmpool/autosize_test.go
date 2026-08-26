package warmpool

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/policy"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

// poolNamespace is where these fixtures put the pool.
const poolNamespace = "pool-ns"

func TestAPoolIsSizedForItsReservePlusRoomToWarm(t *testing.T) {
	// The +1 is the whole point. A pool sized exactly to its reserve is the
	// inert state: admission draws on free-minus-reserve, so the budget is zero
	// forever and the pool holds accelerators while warming nothing.
	if got := SizeFor(1, 0); got != 2 {
		t.Errorf("a reserve of 1 with nothing lent needs 2 Pods, got %d", got)
	}
	if got := SizeFor(2, 0); got != 3 {
		t.Errorf("a reserve of 2 needs 3, got %d", got)
	}
}

func TestLentPodsAreAddedOnTop(t *testing.T) {
	// A lent Pod cannot be counted toward the reserve: it is serving live
	// traffic and cannot be taken back for the next spike.
	if got := SizeFor(1, 2); got != 4 {
		t.Fatalf("two bridges above a reserve of 1 needs 4 Pods, got %d", got)
	}
}

func TestSizeIsNeverNegative(t *testing.T) {
	if got := SizeFor(-5, -5); got != 1 {
		t.Fatalf("nonsense inputs must still yield a usable size, got %d", got)
	}
}

func TestThePoolSizeIsPublishedNotWritten(t *testing.T) {
	// WVA computes, KEDA actuates. The controller holds no permission to resize
	// anything, so a size that is not published is a size nothing can act on.
	var gotNS, gotName string
	var gotReplicas int32
	p := &fakePool{memberships: []pool.Membership{
		{Pod: podA(), State: pool.Absent, Pool: "sized"},
	}}
	d := &staticDemand{}
	cfg := testConfig()
	cfg.SleepMinSize = 1

	r := New(p, d, cfg)
	r.Namespace = poolNamespace
	r.Pools = fakePools{{Name: "sized", Config: cfg, Replicas: 1, Deployment: "wva-warm-pool"}}
	r.PublishSize = func(ns, name string, replicas int32) {
		gotNS, gotName, gotReplicas = ns, name, replicas
	}

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if gotNS != poolNamespace || gotName != "wva-warm-pool" {
		t.Fatalf("published against the wrong object: %s/%s", gotNS, gotName)
	}
	if gotReplicas != 2 {
		t.Errorf("a reserve of 1 with nothing lent publishes 2, got %d", gotReplicas)
	}
}

func TestAPoolWithNoScaledObjectIsLeftAlone(t *testing.T) {
	// No publisher means the pool stays exactly the size its Deployment says,
	// which is what an install without a pool ScaledObject wants. It must not
	// panic or silently resize by another route.
	p := &fakePool{memberships: []pool.Membership{{Pod: podA(), State: pool.Absent}}}
	r := New(p, &staticDemand{}, testConfig())
	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("a pool with no publisher must still reconcile: %v", err)
	}
}

func TestAPoolNotDiscoveredFromADeploymentPublishesNothing(t *testing.T) {
	// There is no object to scale, so a published size would name nothing.
	called := false
	p := &fakePool{memberships: []pool.Membership{{Pod: podA(), State: pool.Absent}}}
	r := New(p, &staticDemand{}, testConfig())
	r.Namespace = poolNamespace
	r.PublishSize = func(string, string, int32) { called = true }
	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if called {
		t.Fatal("nothing to scale, so nothing to publish")
	}
}

func TestAPoolIsSizedForItsOwnBridgesOnly(t *testing.T) {
	// Found in review. The published size was computed from the process-wide
	// borrow record, so with two pools each was told to carry the OTHER's
	// bridges: a quiet pool beside a busy one is sized for Pods it has no use
	// for, permanently, on an accelerator the busy pool cannot even run on. And
	// in the mirror case a busy pool is told it needs fewer Pods than it is
	// currently lending, which invites KEDA to scale it below its own floor.
	busyPod := types.NamespacedName{Namespace: poolNamespace, Name: "busy-0"}
	quietPod := types.NamespacedName{Namespace: poolNamespace, Name: "quiet-0"}
	p := &fakePool{memberships: []pool.Membership{
		{Model: model("qwen"), Pod: busyPod, State: pool.Serving, Pool: "busy"},
		{Pod: quietPod, State: pool.Absent, Pool: "quiet"},
	}}

	cfg := testConfig()
	cfg.SleepMinSize = 1
	sizes := map[string]int32{}
	r := New(p, &staticDemand{}, cfg)
	r.Namespace = poolNamespace
	r.Pools = fakePools{
		{Name: "busy", Config: cfg, Replicas: 2, Deployment: "busy"},
		{Name: "quiet", Config: cfg, Replicas: 2, Deployment: "quiet"},
	}
	r.PublishSize = func(_, name string, replicas int32) { sizes[name] = replicas }
	// The borrow record is deliberately populated for the BUSY pool only, which
	// is what the buggy version summed across both.
	r.borrowedAt[policy.Borrow{Pod: busyPod, Variant: "qwen"}] = time.Now()

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := sizes["busy"]; got != 3 {
		t.Errorf("the lending pool needs its bridge plus reserve plus one: got %d, want 3", got)
	}
	if got := sizes["quiet"]; got != 2 {
		t.Errorf("the idle pool must not be sized for another pool's bridge: got %d, want 2", got)
	}
}

func TestAPoolStopsGrowingWhileReplicasAreDeniedGPUs(t *testing.T) {
	// The arbitration. Nothing bounds a pool's growth -- the optimizer's budgets
	// govern what model REPLICAS may have, and a pool is not a model -- so
	// without this a burst blocks borrows, the pool asks for another Pod to
	// relieve the block, and that Pod is the GPU a starved replica needed. The
	// pool would compete with the very scale-up it exists to bridge.
	held := withAccel(pool.Membership{Pod: podA(), State: pool.Serving, Pool: "p"}, "H100")
	held.Model = model("qwen")
	p := &fakePool{memberships: []pool.Membership{held}}
	cfg := testConfig()
	cfg.SleepMinSize = 1

	var published int32
	r := New(p, &staticDemand{}, cfg)
	r.Namespace = poolNamespace
	r.Pools = fakePools{{Name: "p", Config: cfg, Replicas: 2, Deployment: "p"}}
	r.PublishSize = func(_, _ string, replicas int32) { published = replicas }
	r.borrowedAt[policy.Borrow{Pod: podA(), Variant: "qwen"}] = time.Now()
	r.Contended = func(ns, accel string) bool { return ns == poolNamespace && accel == "H100" }

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	// It WANTED lent(1) + reserve(1) + 1 = 3, and holds at its current 2.
	if published != 2 {
		t.Fatalf("a contended pool must hold at its current size, got %d", published)
	}
}

func TestAContendedPoolStillReleasesPodsItDoesNotNeed(t *testing.T) {
	// Contention stops the pool GROWING. It must not stop it shrinking to what
	// it needs -- that shrink hands accelerators back, which is the most useful
	// thing the pool can do while replicas are starved. An earlier version of
	// this test asserted the opposite and would have pinned an oversized pool
	// exactly when GPUs were scarce.
	idle := withAccel(pool.Membership{Pod: podA(), State: pool.Absent, Pool: "p"}, "H100")
	p := &fakePool{memberships: []pool.Membership{idle}}
	cfg := testConfig()
	cfg.SleepMinSize = 1

	var published int32
	r := New(p, &staticDemand{}, cfg)
	r.Namespace = poolNamespace
	// At 3 Pods, but only reserve+1 = 2 are needed.
	r.Pools = fakePools{{Name: "p", Config: cfg, Replicas: 3, Deployment: "p"}}
	r.PublishSize = func(_, _ string, replicas int32) { published = replicas }
	r.Contended = func(string, string) bool { return true }

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if published != 2 {
		t.Fatalf("a contended pool still releases what it does not need: got %d, want 2", published)
	}
}

func TestContentionOnAnotherAcceleratorDoesNotHoldThisPool(t *testing.T) {
	// Contention for A100s says nothing about an H100 pool. Holding every pool
	// on any contention anywhere would make one starved variant freeze the whole
	// cluster's insurance.
	idle := withAccel(pool.Membership{Pod: podA(), State: pool.Absent, Pool: "p"}, "H100")
	p := &fakePool{memberships: []pool.Membership{idle}}
	cfg := testConfig()
	cfg.SleepMinSize = 1

	var published int32
	r := New(p, &staticDemand{}, cfg)
	r.Namespace = poolNamespace
	r.Pools = fakePools{{Name: "p", Config: cfg, Replicas: 1, Deployment: "p"}}
	r.PublishSize = func(_, _ string, replicas int32) { published = replicas }
	r.Contended = func(_, accel string) bool { return accel == "A100" }

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if published != 2 {
		t.Fatalf("another accelerator's contention must not hold this pool: got %d, want 2", published)
	}
}

func TestAPoolWhoseAcceleratorIsUnknownGrowsAsBefore(t *testing.T) {
	// An install without node read cannot name its accelerator, so it can never
	// match a contention record. It must grow as it did before rather than be
	// frozen by a check it cannot participate in.
	idle := pool.Membership{Pod: podA(), State: pool.Absent, Pool: "p"} // no Accelerator
	p := &fakePool{memberships: []pool.Membership{idle}}
	cfg := testConfig()
	cfg.SleepMinSize = 1

	var published int32
	r := New(p, &staticDemand{}, cfg)
	r.Namespace = poolNamespace
	r.Pools = fakePools{{Name: "p", Config: cfg, Replicas: 1, Deployment: "p"}}
	r.PublishSize = func(_, _ string, replicas int32) { published = replicas }
	r.Contended = func(string, string) bool { return true }

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if published != 2 {
		t.Fatalf("an unknown accelerator must not freeze the pool: got %d, want 2", published)
	}
}

// withAccel gives a membership a declared accelerator.
func withAccel(m pool.Membership, accelerator string) pool.Membership {
	m.Capacity.Accelerator = accelerator
	return m
}
