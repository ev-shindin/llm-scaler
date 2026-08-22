package warmpool

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/policy"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

// These cover defects found in review. Each one failed before its fix.

func TestAFailedBorrowIsReturnedRatherThanLeftHalfLent(t *testing.T) {
	// The chain this prevents: Activate fails after labelling and waking, the
	// Pod reads as Waking (which counts as lent), so nothing retries the variant
	// elsewhere -- and with no recorded start time its age is recomputed as zero
	// every cycle, so the hold timeout never reclaims it either. One transient
	// failure removed a Pod from the reserve permanently.
	p := &fakePool{
		memberships: []pool.Membership{{Model: model("qwen"), Pod: podA(), State: pool.Asleep}},
		wakeErr:     errors.New("CUDA error: out of memory"),
	}
	d := &staticDemand{variants: []policy.VariantDemand{{Model: model("qwen"), Desired: 1, Ready: 0}}}
	r := New(p, d, testConfig())

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if !p.did("deactivate qwen@a") {
		t.Fatalf("a failed borrow must be returned: %v", p.seen())
	}
	if len(r.Lent()) != 0 {
		t.Fatalf("and not remembered as lent: %v", r.Lent())
	}
}

func TestAFailedBorrowThatCannotBeReturnedStillAges(t *testing.T) {
	// If even the rollback fails, the borrow must remain recorded -- that is the
	// only thing that lets the hold timeout reclaim the Pod later.
	p := &fakePool{
		memberships:   []pool.Membership{{Model: model("qwen"), Pod: podA(), State: pool.Asleep}},
		wakeErr:       errors.New("wake failed"),
		deactivateErr: errors.New("and the rollback failed too"),
	}
	d := &staticDemand{variants: []policy.VariantDemand{{Model: model("qwen"), Desired: 1, Ready: 0}}}
	r := New(p, d, testConfig())

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(r.Lent()) != 1 {
		t.Fatalf("the borrow must stay recorded so the hold timeout can reclaim it: %v", r.Lent())
	}
}

func TestReturnsHappenBeforeBorrowsInOneCycle(t *testing.T) {
	// A Pod handed back is reserve for every model, including one waiting in the
	// same cycle. Asserted here at the RECONCILER, not just in the plan: the
	// two loops in apply could be swapped and no other test would notice.
	p := &fakePool{memberships: []pool.Membership{
		{Model: model("done"), Pod: podA(), State: pool.Serving},
		{Model: model("next"), Pod: podA(), State: pool.Asleep},
	}}
	d := &staticDemand{variants: []policy.VariantDemand{
		{Model: model("done"), Desired: 1, Ready: 1}, // handover due
		{Model: model("next"), Desired: 1, Ready: 0}, // wants capacity
	}}
	r := New(p, d, testConfig())
	r.borrowedAt[policy.Borrow{Pod: podA(), Variant: "done"}] = time.Now()

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	seen := p.seen()
	deactivate, activate := indexOf(seen, "deactivate done@a"), indexOf(seen, "activate next@a")
	if deactivate < 0 || activate < 0 {
		t.Fatalf("want both a return and a borrow: %v", seen)
	}
	if deactivate > activate {
		t.Fatalf("the return must come first: %v", seen)
	}
}

func TestAnExpiredHoldIsReturnedThroughTheReconciler(t *testing.T) {
	// Proven at the policy level already; this pins the WIRING -- borrowedAt and
	// the clock reaching policy.Input, and the plan reaching Pool.Deactivate.
	p := &fakePool{memberships: []pool.Membership{
		{Model: model("qwen"), Pod: podA(), State: pool.Serving},
	}}
	// Still short, so only the timeout can justify the return.
	d := &staticDemand{variants: []policy.VariantDemand{{Model: model("qwen"), Desired: 3, Ready: 0}}}
	r := New(p, d, testConfig())

	base := time.Now()
	r.now = func() time.Time { return base }
	r.borrowedAt[policy.Borrow{Pod: podA(), Variant: "qwen"}] = base.Add(-5 * time.Minute)

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if !p.did("deactivate qwen@a") {
		t.Fatalf("an expired bridge must be returned: %v", p.seen())
	}
}

func TestTheReconcilerForwardsTheWholeModel(t *testing.T) {
	// A ModelRef missing PoolLabels or EngineOptions is refused deep in the
	// adapter, where a fake that records only the variant name would not notice.
	p := &fakePool{memberships: []pool.Membership{
		{Model: model("qwen"), Pod: podA(), State: pool.Asleep},
	}}
	d := &staticDemand{variants: []policy.VariantDemand{{Model: model("qwen"), Desired: 1, Ready: 0}}}
	r := New(p, d, testConfig())

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(p.activated) != 1 {
		t.Fatalf("want one activation, got %d", len(p.activated))
	}
	got := p.activated[0]
	if len(got.PoolLabels) == 0 {
		t.Error("PoolLabels must survive: Activate refuses without them")
	}
	if got.Namespace == "" {
		t.Error("the namespace must survive: the metrics are labelled with it")
	}
}

func TestClosingTheTriggerStopsItsGoroutines(t *testing.T) {
	// decision.Store's cancel removes the registration but never closes the
	// channel, so a goroutine ranging over it parked forever: one leaked per
	// watched target, every time the trigger was rebuilt.
	store := decision.NewStore()
	before := runtime.NumGoroutine()

	trigger := NewDecisionTrigger(store, nil)
	for i := 0; i < 20; i++ {
		trigger.Watch(types.NamespacedName{Namespace: "ns", Name: name(i)})
	}
	trigger.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 { // slack for the runtime's own
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutines leaked: %d before, %d after Close", before, runtime.NumGoroutine())
}

func TestWatchingATargetTwiceSubscribesOnce(t *testing.T) {
	// Discovery is call-driven and re-offers the same targets on every sync.
	store := decision.NewStore()
	trigger := NewDecisionTrigger(store, nil)
	defer trigger.Close()

	target := types.NamespacedName{Namespace: "ns", Name: "qwen-decode"}
	before := runtime.NumGoroutine()
	for i := 0; i < 10; i++ {
		trigger.Watch(target)
	}
	if after := runtime.NumGoroutine(); after > before+3 {
		t.Fatalf("watching one target ten times started %d goroutines", after-before)
	}

	// And it still fires.
	store.Set(target.Namespace, target.Name, 2)
	select {
	case <-trigger.Notify():
	case <-time.After(2 * time.Second):
		t.Fatal("a decision did not fire the trigger")
	}
}

func indexOf(events []string, want string) int {
	for i, e := range events {
		if e == want {
			return i
		}
	}
	return -1
}

func name(i int) string { return string(rune('a'+i%26)) + "-decode" }

func TestTheTriggerPicksUpVariantsDiscoveredAfterItStarted(t *testing.T) {
	// Discovery is call-driven: a workload becomes known when KEDA first asks
	// about it. A trigger built once at startup would therefore never fire for
	// anything registered later -- every variant, on a fresh controller -- and
	// each of those bridges would wait for KEDA's next poll instead of starting
	// at decision time.
	store := decision.NewStore()
	reg := registry.New(0)
	trigger := NewDecisionTrigger(store, nil)
	defer trigger.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go trigger.WatchRegistry(ctx, reg, time.Millisecond)

	// Registered only now, after WatchRegistry is already running.
	reg.Observe("tenant", "qwen", nil)
	reg.SetTarget("tenant", "qwen", registry.Target{Kind: "Deployment", Name: "qwen-decode"})

	// The subscription lands on a tick, so keep writing until it does rather
	// than sleeping for a guessed interval.
	deadline := time.After(5 * time.Second)
	for {
		store.Set("tenant", "qwen-decode", 3)
		select {
		case <-trigger.Notify():
			return
		case <-deadline:
			t.Fatal("a decision for a later-discovered variant never woke the loop")
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func TestTheTriggerIgnoresEntriesWhoseScaleTargetIsNotKnownYet(t *testing.T) {
	// The decision store is keyed by the SCALE TARGET's name, which is only
	// known after a ScaledObject read. Subscribing on the ScaledObject's name
	// would silently watch a key nothing ever writes.
	store := decision.NewStore()
	reg := registry.New(0)
	reg.Observe("tenant", "qwen", nil) // observed, never enriched

	trigger := NewDecisionTrigger(store, nil)
	defer trigger.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go trigger.WatchRegistry(ctx, reg, time.Millisecond)

	time.Sleep(20 * time.Millisecond)
	store.Set("tenant", "qwen", 3) // the ScaledObject's name, not a scale target

	select {
	case <-trigger.Notify():
		t.Fatal("an unenriched entry must not be watched under the wrong key")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestWatchRegistryEndsWithItsContext(t *testing.T) {
	// It runs for the life of the manager; on shutdown it must return rather
	// than hold the run group open.
	trigger := NewDecisionTrigger(decision.NewStore(), nil)
	defer trigger.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		trigger.WatchRegistry(ctx, registry.New(0), time.Hour)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WatchRegistry did not return when its context ended")
	}
}
