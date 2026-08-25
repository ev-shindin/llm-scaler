package warmpool

import (
	"context"
	"testing"

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
