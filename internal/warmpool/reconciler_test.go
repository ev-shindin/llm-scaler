package warmpool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/policy"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

// fakePool records what the reconciler asked the data plane to do.
type fakePool struct {
	mu sync.Mutex

	memberships []pool.Membership
	calls       []string
	wakeErr     error
	// deactivateErr fails the rollback path, which must still leave the borrow
	// recorded so the hold timeout can reclaim the Pod.
	deactivateErr error
	warmDelay     time.Duration
	warmStarted   chan struct{}
	// activated keeps the WHOLE ModelRef, because a reconciler forwarding an
	// incomplete one (no PoolLabels, no EngineOptions) is refused deep in the
	// adapter, where a name-only record would not notice.
	activated []pool.ModelRef
}

func (f *fakePool) ListWarm(context.Context) ([]pool.Membership, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pool.Membership(nil), f.memberships...), nil
}

func (f *fakePool) Warm(ctx context.Context, p types.NamespacedName, m pool.ModelRef, _ pool.Tier) error {
	f.record("warm " + m.Variant)
	if f.warmStarted != nil {
		f.warmStarted <- struct{}{}
	}
	if f.warmDelay > 0 {
		select {
		case <-time.After(f.warmDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (f *fakePool) Activate(_ context.Context, p types.NamespacedName, m pool.ModelRef) (pool.Endpoint, error) {
	f.record("activate " + m.Variant + "@" + p.Name)
	f.mu.Lock()
	f.activated = append(f.activated, m)
	f.mu.Unlock()
	if f.wakeErr != nil {
		return pool.Endpoint{}, f.wakeErr
	}
	return pool.Endpoint{PodIP: "10.0.0.1", Port: 9001}, nil
}

func (f *fakePool) Deactivate(_ context.Context, p types.NamespacedName, m pool.ModelRef) error {
	f.record("deactivate " + m.Variant + "@" + p.Name)
	return f.deactivateErr
}

func (f *fakePool) Evict(_ context.Context, p types.NamespacedName, m pool.ModelRef) error {
	f.record("evict " + m.Variant + "@" + p.Name)
	return nil
}

func (f *fakePool) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakePool) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakePool) did(call string) bool {
	for _, c := range f.seen() {
		if c == call {
			return true
		}
	}
	return false
}

// staticDemand answers with a fixed view of what variants want.
type staticDemand struct {
	mu       sync.Mutex
	variants []policy.VariantDemand
	err      error
}

func (d *staticDemand) Variants(context.Context) ([]policy.VariantDemand, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]policy.VariantDemand(nil), d.variants...), d.err
}

func (d *staticDemand) set(v []policy.VariantDemand) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.variants = v
}

func testConfig() policy.Config {
	return policy.Config{
		SleepMinSize:       0,
		MaxHold:            time.Minute,
		AdmissionWindow:    time.Hour,
		MinMissesToAdmit:   2,
		MaxInstancesPerPod: 4,
	}
}

// podA names the single pool Pod these tests act on; the fake answers for
// whichever Pod is addressed, so a second name would prove nothing.
func podA() types.NamespacedName {
	return types.NamespacedName{Namespace: "pool", Name: "a"}
}

func model(variant string) pool.ModelRef {
	return pool.ModelRef{Namespace: "workload", Variant: variant, PoolLabels: map[string]string{"llm-d.ai/model": variant}}
}

func TestABorrowIsCarriedOutWhenAVariantIsShort(t *testing.T) {
	p := &fakePool{memberships: []pool.Membership{
		{Model: model("qwen"), Pod: podA(), State: pool.Asleep},
	}}
	d := &staticDemand{variants: []policy.VariantDemand{{Model: model("qwen"), Desired: 2, Ready: 1}}}
	r := New(p, d, testConfig())

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if !p.did("activate qwen@a") {
		t.Fatalf("expected a borrow, got %v", p.seen())
	}
	if len(r.Lent()) != 1 {
		t.Errorf("the borrow must be remembered for its hold timeout: %v", r.Lent())
	}
}

func TestAFinishedBridgeIsReturnedAndForgotten(t *testing.T) {
	p := &fakePool{memberships: []pool.Membership{
		{Model: model("qwen"), Pod: podA(), State: pool.Serving},
	}}
	d := &staticDemand{variants: []policy.VariantDemand{{Model: model("qwen"), Desired: 1, Ready: 1}}}
	r := New(p, d, testConfig())
	r.borrowedAt[policy.Borrow{Pod: podA(), Variant: "qwen"}] = time.Now()

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if !p.did("deactivate qwen@a") {
		t.Fatalf("expected the bridge to be returned, got %v", p.seen())
	}
	if len(r.Lent()) != 0 {
		t.Errorf("a returned bridge must be forgotten, else its hold timer runs forever: %v", r.Lent())
	}
}

func TestAFailedWakeCountsAsAMiss(t *testing.T) {
	// A warm copy that cannot wake is not warm. Counting it as a miss is what
	// eventually gets the model re-admitted somewhere that works.
	p := &fakePool{
		memberships: []pool.Membership{{Model: model("qwen"), Pod: podA(), State: pool.Asleep}},
		wakeErr:     errors.New("CUDA error: out of memory"),
	}
	d := &staticDemand{variants: []policy.VariantDemand{{Model: model("qwen"), Desired: 1, Ready: 0}}}
	r := New(p, d, testConfig())

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(r.Lent()) != 0 {
		t.Fatalf("a failed wake must leave nothing lent once rolled back: %v", r.Lent())
	}
	if !p.did("deactivate qwen@a") {
		t.Fatalf("and the Pod must be returned: %v", p.seen())
	}
	r.mu.Lock()
	misses := len(r.missesAt["qwen"])
	r.mu.Unlock()
	if misses != 1 {
		t.Fatalf("want the failure recorded as a miss, got %d", misses)
	}
}

func TestAdmissionDoesNotBlockTheLoop(t *testing.T) {
	// Admission is a ~35 s model load. A reconcile that waited for it would
	// stop returning finished bridges and stop borrowing for anyone else.
	p := &fakePool{
		memberships: []pool.Membership{{Model: model("other"), Pod: podA(), State: pool.Asleep}},
		warmDelay:   2 * time.Second,
		warmStarted: make(chan struct{}, 1),
	}
	d := &staticDemand{variants: []policy.VariantDemand{{Model: model("parked"), Parked: true}}}
	r := New(p, d, testConfig())

	start := time.Now()
	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the reconcile waited %s for an admission", elapsed)
	}
	select {
	case <-p.warmStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("admission never started")
	}
}

func TestTheSameAdmissionIsNotIssuedTwice(t *testing.T) {
	// The model is not resident until the load finishes, so without a claim the
	// same admission would be re-issued every cycle for ~35 s.
	p := &fakePool{
		memberships: []pool.Membership{{Model: model("other"), Pod: podA(), State: pool.Asleep}},
		warmDelay:   time.Second,
		warmStarted: make(chan struct{}, 4),
	}
	d := &staticDemand{variants: []policy.VariantDemand{{Model: model("parked"), Parked: true}}}
	r := New(p, d, testConfig())

	for i := 0; i < 3; i++ {
		if _, err := r.Once(context.Background()); err != nil {
			t.Fatalf("Once: %v", err)
		}
	}
	<-p.warmStarted // at least one started

	warms := 0
	for _, c := range p.seen() {
		if c == "warm parked" {
			warms++
		}
	}
	if warms != 1 {
		t.Fatalf("want exactly one admission in flight, got %d: %v", warms, p.seen())
	}
}

func TestStaleMissesAreForgotten(t *testing.T) {
	// Otherwise a model that missed twice last week is admitted today, which is
	// the frequency filter answering a question nobody asked.
	r := New(&fakePool{}, &staticDemand{}, testConfig())
	r.Config.AdmissionWindow = time.Minute
	base := time.Now()
	r.now = func() time.Time { return base }

	r.recordMiss("rare")
	r.now = func() time.Time { return base.Add(2 * time.Minute) }
	r.recordMiss("rare")

	r.mu.Lock()
	defer r.mu.Unlock()
	if got := len(r.missesAt["rare"]); got != 1 {
		t.Fatalf("want only the recent miss, got %d", got)
	}
}

func TestADecisionWakesTheLoopWithoutWaitingForTheTick(t *testing.T) {
	// The reason the trigger exists: a decision is known here before KEDA is
	// told, and waiting for the tick would add it to a wake measured in
	// hundreds of milliseconds.
	store := decision.NewStore()
	target := types.NamespacedName{Namespace: "workload", Name: "qwen-decode"}
	trigger := NewDecisionTrigger(store, []types.NamespacedName{target})
	defer trigger.Close()

	p := &fakePool{memberships: []pool.Membership{
		{Model: model("qwen"), Pod: podA(), State: pool.Asleep},
	}}
	d := &staticDemand{variants: []policy.VariantDemand{{Model: model("qwen"), Desired: 0, Ready: 0}}}
	r := New(p, d, testConfig())
	r.Trigger = trigger
	r.Interval = time.Hour // only the trigger can drive this test

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Start(ctx) }()

	// The first pass runs immediately and borrows nothing.
	time.Sleep(50 * time.Millisecond)
	if p.did("activate qwen@a") {
		t.Fatal("nothing was wanted yet")
	}

	// A scale-up decision lands.
	d.set([]policy.VariantDemand{{Model: model("qwen"), Desired: 2, Ready: 0}})
	store.Set(target.Namespace, target.Name, 2)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.did("activate qwen@a") {
			return // borrowed without waiting out the hour-long tick
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the decision did not drive a borrow: %v", p.seen())
}

func TestDemandFailureDoesNotActOnStaleState(t *testing.T) {
	// Acting on a half-read world is worse than doing nothing for a cycle: it
	// would return bridges that are still needed.
	p := &fakePool{memberships: []pool.Membership{
		{Model: model("qwen"), Pod: podA(), State: pool.Serving},
	}}
	d := &staticDemand{err: errors.New("datastore unavailable")}
	r := New(p, d, testConfig())

	if _, err := r.Once(context.Background()); err == nil {
		t.Fatal("want the error surfaced")
	}
	if len(p.seen()) != 0 {
		t.Fatalf("nothing should have been done: %v", p.seen())
	}
}
