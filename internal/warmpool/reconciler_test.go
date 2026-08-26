package warmpool

import (
	"bytes"
	"context"
	"errors"
	stdlog "log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/stdr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

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
	// The POD is part of the record, as it is for activate and deactivate. It
	// was dropped here, so no test could say which Pod a model was admitted
	// into -- which is exactly the question once a namespace holds more than
	// one pool.
	f.record("warm " + m.Variant + "@" + p.Name)
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

// didPrefix is did for a call whose Pod half is not the point of the assertion.
func (f *fakePool) didPrefix(prefix string) bool {
	for _, c := range f.seen() {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
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

// logTo returns a context whose logger writes into buf at a verbosity that
// includes V(1). stdr defaults to 0, which silently drops the state line these
// tests exist to assert on -- a test that passes because nothing was logged
// would agree with the bug.
func logTo(t *testing.T, buf *bytes.Buffer) context.Context {
	t.Helper()
	old := stdr.SetVerbosity(1)
	t.Cleanup(func() { stdr.SetVerbosity(old) })
	return log.IntoContext(context.Background(), stdr.New(stdlog.New(buf, "", 0)))
}

// fakePools is a fixed set of declared pools.
type fakePools []PoolSpec

func (f fakePools) Pools(context.Context) ([]PoolSpec, error) { return []PoolSpec(f), nil }

func TestEachPoolKeepsItsOwnReserve(t *testing.T) {
	// The point of naming pools: the reserve is per pool, so a pool with room
	// admits while one whose every Pod is reserve does not -- on the SAME pass,
	// for the SAME model. Before the split both Pods were one flat pool under
	// one config, so the reserve was a single number spanning accelerators that
	// cannot substitute for each other.
	//
	// The reserve gates ADMISSION, not borrowing: lending a Pod that already
	// holds the model warm is what the pool is for.
	podRoomy := types.NamespacedName{Namespace: "pool", Name: "roomy-0"}
	podStrict := types.NamespacedName{Namespace: "pool", Name: "strict-0"}
	p := &fakePool{
		memberships: []pool.Membership{
			{Pod: podRoomy, State: pool.Absent, Pool: "zzz-roomy"},
			{Pod: podStrict, State: pool.Absent, Pool: "aaa-strict"},
		},
		warmStarted: make(chan struct{}, 2),
	}
	// Each variant SELECTS its pool, which is required once a namespace holds
	// more than one -- with two pools of different accelerators, guessing is
	// wrong half the time and costs a ~35 s load that can never serve.
	d := &staticDemand{variants: []policy.VariantDemand{
		{Model: model("wants-roomy"), Parked: true, WarmPool: "zzz-roomy"},
		{Model: model("wants-strict"), Parked: true, WarmPool: "aaa-strict"},
	}}

	roomy, strict := testConfig(), testConfig()
	roomy.SleepMinSize = 0
	strict.SleepMinSize = 1

	r := New(p, d, testConfig())
	// The STRICT pool is named so it sorts FIRST. With the roomy one first, a
	// bug that ignores per-pool config still produces the right Pod: the roomy
	// pool admits, and the variant-level dedup then suppresses the strict pool's
	// attempt, so the outcome is identical either way. Ordering the strict pool
	// first makes the two cases diverge.
	r.Pools = fakePools{
		{Name: "aaa-strict", Config: strict, Replicas: 1},
		{Name: "zzz-roomy", Config: roomy, Replicas: 1},
	}

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	// Admission runs in its own goroutine so a ~35 s load cannot hold the loop.
	select {
	case <-p.warmStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("no admission was started at all")
	}
	if !p.did("warm wants-roomy@roomy-0") {
		t.Fatalf("the pool with room must admit its variant: %v", p.seen())
	}
	if p.didPrefix("warm wants-strict@") {
		t.Fatalf("a pool whose every Pod is reserve must not admit: %v", p.seen())
	}
	if p.did("warm wants-roomy@strict-0") {
		t.Fatalf("a variant must never be admitted into a pool it did not select: %v", p.seen())
	}
}

func TestAPodInNoDeclaredPoolIsNeverLent(t *testing.T) {
	// A Pod whose label matches no pool holds a GPU nothing will lend. Folding
	// it into the first pool would be worse than ignoring it: that pool would
	// admit models into a Pod sized for something else.
	stray := types.NamespacedName{Namespace: "pool", Name: "stray-0"}
	p := &fakePool{memberships: []pool.Membership{
		{Model: model("qwen"), Pod: stray, State: pool.Asleep, Pool: "typo"},
	}}
	d := &staticDemand{variants: []policy.VariantDemand{{Model: model("qwen"), Desired: 2, Ready: 1}}}

	open := testConfig()
	open.SleepMinSize = 0
	r := New(p, d, testConfig())
	r.Pools = fakePools{{Name: "declared", Config: open, Replicas: 2}}

	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(r.Lent()) != 0 {
		t.Fatalf("a Pod in no declared pool must not be lent: %v", r.Lent())
	}
}

func TestAPoolWhoseEveryPodIsReserveSaysSo(t *testing.T) {
	// The state the obvious first deployment lands in: one Pod, the default
	// reserve of one. policy.Decide computes free-minus-reserve, so the budget is
	// zero forever and nothing is ever admitted -- silently, while the Pod holds
	// a GPU. Confirmed on a cluster: adding a second Pod made admission fire on
	// the next pass, with no other change.
	p := &fakePool{memberships: []pool.Membership{
		{Model: pool.ModelRef{}, Pod: podA(), State: pool.Asleep},
	}}
	d := &staticDemand{variants: []policy.VariantDemand{{Model: model("qwen"), Desired: 1, Ready: 1}}}
	cfg := testConfig()
	cfg.SleepMinSize = 1
	r := New(p, d, cfg)

	var buf bytes.Buffer
	ctx := logTo(t, &buf)
	if _, err := r.Once(ctx); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if !strings.Contains(buf.String(), "every Pod is reserve") {
		t.Fatalf("an inert pool must say so; got %q", buf.String())
	}
}

func TestAnInertPoolIsReportedOnceNotTwice(t *testing.T) {
	// Two checks describe this condition from different evidence: the declared
	// replica count, and the Pods that exist. Both firing reads as two separate
	// faults, and the reader has to work out they are one. Seen on a cluster --
	// raising the reserve to equal replicas produced two lines, one after the
	// other, saying the same thing.
	p := &fakePool{memberships: []pool.Membership{
		{Pod: podA(), State: pool.Absent, Pool: "sized"},
	}}
	d := &staticDemand{variants: []policy.VariantDemand{{Model: model("qwen"), Desired: 1, Ready: 1}}}
	cfg := testConfig()
	cfg.SleepMinSize = 1
	r := New(p, d, cfg)
	r.Pools = fakePools{{Name: "sized", Config: cfg, Replicas: 1}}

	var buf bytes.Buffer
	ctx := logTo(t, &buf)
	if _, err := r.Once(ctx); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := strings.Count(buf.String(), "can never admit"); got != 1 {
		t.Fatalf("the Deployment-based reason must be the only one, got %d:\n%s", got, buf.String())
	}
	if strings.Contains(buf.String(), "every Pod is reserve") {
		t.Errorf("the Pod-count check must stand down where the Deployment answered: %s", buf.String())
	}
}

func TestAPoolWithNoDeploymentStillReportsBeingInert(t *testing.T) {
	// The other half: with no Deployment to read, Replicas is unknown and Inert
	// cannot answer, so the Pod-count check is the only thing that can say it.
	p := &fakePool{memberships: []pool.Membership{
		{Pod: podA(), State: pool.Absent},
	}}
	d := &staticDemand{variants: []policy.VariantDemand{{Model: model("qwen"), Desired: 1, Ready: 1}}}
	cfg := testConfig()
	cfg.SleepMinSize = 1
	r := New(p, d, cfg) // no r.Pools: the single unnamed pool, Replicas unknown

	var buf bytes.Buffer
	ctx := logTo(t, &buf)
	if _, err := r.Once(ctx); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if !strings.Contains(buf.String(), "every Pod is reserve") {
		t.Fatalf("an inert pool with no Deployment must still say so: %s", buf.String())
	}
}

func TestASteadyPoolDoesNotRepeatItself(t *testing.T) {
	// The state line is deduplicated, not periodic: a pool that is not changing
	// should be quiet, or the log becomes something operators filter out and
	// then miss the one line that mattered.
	p := &fakePool{memberships: []pool.Membership{
		{Model: model("qwen"), Pod: podA(), State: pool.Asleep},
	}}
	d := &staticDemand{variants: []policy.VariantDemand{{Model: model("qwen"), Desired: 1, Ready: 1}}}
	r := New(p, d, testConfig())

	var buf bytes.Buffer
	ctx := logTo(t, &buf)
	for i := 0; i < 3; i++ {
		if _, err := r.Once(ctx); err != nil {
			t.Fatalf("Once: %v", err)
		}
	}
	if got := strings.Count(buf.String(), "warm pool state"); got != 1 {
		t.Fatalf("an unchanging pool must log its state once, got %d:\n%s", got, buf.String())
	}
}

func TestAChangedPoolReportsAgain(t *testing.T) {
	// The other half: dedup must not swallow a real change, or the feature
	// trades one silence for another.
	p := &fakePool{memberships: []pool.Membership{
		{Model: model("qwen"), Pod: podA(), State: pool.Asleep},
	}}
	d := &staticDemand{variants: []policy.VariantDemand{{Model: model("qwen"), Desired: 1, Ready: 1}}}
	r := New(p, d, testConfig())

	var buf bytes.Buffer
	ctx := logTo(t, &buf)
	if _, err := r.Once(ctx); err != nil {
		t.Fatalf("Once: %v", err)
	}
	p.memberships = nil // every Pod went away
	if _, err := r.Once(ctx); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := strings.Count(buf.String(), "warm pool state"); got != 2 {
		t.Fatalf("a changed pool must report again, got %d:\n%s", got, buf.String())
	}
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
		if strings.HasPrefix(c, "warm parked@") {
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

// hangingPool blocks in whichever call the test selects, until its context ends.
// A Pod that accepts a connection and never answers is the realistic version:
// the HTTP clients have their own timeouts, but they are measured in minutes.
type hangingPool struct {
	fakePool
	hangList     bool
	hangActivate bool
	listEnded    chan error

	mu           sync.Mutex
	listDeadline bool
}

func (h *hangingPool) ListWarm(ctx context.Context) ([]pool.Membership, error) {
	h.mu.Lock()
	_, h.listDeadline = ctx.Deadline()
	h.mu.Unlock()

	if !h.hangList {
		return h.fakePool.ListWarm(ctx)
	}
	<-ctx.Done()
	if h.listEnded != nil {
		h.listEnded <- ctx.Err()
	}
	return nil, ctx.Err()
}

func (h *hangingPool) sawDeadline() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.listDeadline
}

func (h *hangingPool) Activate(ctx context.Context, p types.NamespacedName, m pool.ModelRef) (pool.Endpoint, error) {
	if !h.hangActivate {
		return h.fakePool.Activate(ctx, p, m)
	}
	<-ctx.Done()
	return pool.Endpoint{}, ctx.Err()
}

func TestAPoolPodThatWillNotAnswerDoesNotHoldTheLoop(t *testing.T) {
	// The reconcile interval is 5 s and the engine client's default timeout is
	// 120 s. Without a deadline of its own, one Pod that accepts a connection
	// and never answers holds the pass for two minutes per instance behind it --
	// and every borrow decided in that window arrives long after the cold start
	// it was meant to beat.
	p := &hangingPool{hangList: true, listEnded: make(chan error, 1)}
	r := New(p, &staticDemand{}, testConfig())
	r.ObserveTimeout = 20 * time.Millisecond

	start := time.Now()
	if _, err := r.Once(context.Background()); err == nil {
		t.Fatal("an observation that timed out must be reported, not treated as an empty pool")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Once took %s: the observation is not bounded", elapsed)
	}

	select {
	case err := <-p.listEnded:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("the pool saw %v, want a deadline", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the call was abandoned without its context being cancelled")
	}
}

func TestABorrowThatHangsCostsOneActionRatherThanThePass(t *testing.T) {
	// Each act gets its own deadline, so a Pod that will not wake does not stop
	// the pool from returning bridges or admitting models in the same pass.
	p := &hangingPool{hangActivate: true}
	p.memberships = []pool.Membership{{Model: model("qwen"), Pod: podA(), State: pool.Asleep}}

	r := New(p, &staticDemand{variants: []policy.VariantDemand{
		{Model: model("qwen"), Desired: 2, Ready: 0},
	}}, testConfig())
	r.ActTimeout = 20 * time.Millisecond

	start := time.Now()
	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Once took %s: the borrow is not bounded", elapsed)
	}
	// And it is treated as what it is: a warm copy that cannot wake is not warm.
	r.mu.Lock()
	misses := len(r.missesAt["qwen"])
	r.mu.Unlock()
	if misses != 1 {
		t.Errorf("misses = %d, want the failed borrow counted as one", misses)
	}
}

func TestAnUnsetTimeoutIsBoundedAnyway(t *testing.T) {
	// A Reconciler built as a struct literal rather than through New must still
	// be bounded: an unset deadline is a mistake, not a request for none, and a
	// zero duration passed straight to context.WithTimeout would be worse than
	// either -- every call would fail immediately.
	p := &hangingPool{}
	r := &Reconciler{
		Pool:       p,
		Demand:     &staticDemand{},
		Config:     testConfig(),
		borrowedAt: map[policy.Borrow]time.Time{},
		missesAt:   map[string][]time.Time{},
		admitting:  map[string]bool{},
		now:        time.Now,
	}
	if _, err := r.Once(context.Background()); err != nil {
		t.Fatalf("a zero ObserveTimeout must not fail the pass: %v", err)
	}
	if !p.sawDeadline() {
		t.Error("an unset ObserveTimeout must fall back to a default, not run unbounded")
	}
}

func TestADeclinedModelIsSaidOutLoud(t *testing.T) {
	// Found in review: plan.Declined was built with a reason and then consumed
	// by nothing. A model needing more GPUs than a Pod holds, or an accelerator
	// this pool is not on, was refused in total silence -- the variant scaled
	// normally and never ran warm, which from outside is indistinguishable from
	// a pool that is merely too small.
	big := pool.ModelRef{
		Namespace:     "workload",
		Variant:       "too-wide",
		EngineOptions: "--model Qwen/Qwen3-0.6B --tensor-parallel-size 8",
	}
	p := &fakePool{memberships: []pool.Membership{
		{Pod: podA(), State: pool.Absent, Capacity: pool.PodCapacity{GPUs: 1}},
	}}
	d := &staticDemand{variants: []policy.VariantDemand{{Model: big, Parked: true}}}
	cfg := testConfig()
	cfg.SleepMinSize = 0
	cfg.PodMemoryBytes = 100 << 30
	r := New(p, d, cfg)

	var buf bytes.Buffer
	ctx := logTo(t, &buf)
	if _, err := r.Once(ctx); err != nil {
		t.Fatalf("Once: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "will not warm this model") {
		t.Fatalf("a decline must be reported: %s", out)
	}
	// The reason has to name both figures, or an operator cannot act on it.
	if !strings.Contains(out, "8") || !strings.Contains(out, "too-wide") {
		t.Errorf("the reason must name the variant and what it needs: %s", out)
	}
}

func TestAStandingDeclineIsNotRepeatedEveryPass(t *testing.T) {
	// A mismatch is re-decided on every pass. Reported each time it would be the
	// only thing in the log, which is how operators learn to filter the log.
	big := pool.ModelRef{Variant: "too-wide", EngineOptions: "--model Qwen/Qwen3-0.6B --tensor-parallel-size 8"}
	p := &fakePool{memberships: []pool.Membership{
		{Pod: podA(), State: pool.Absent, Capacity: pool.PodCapacity{GPUs: 1}},
	}}
	d := &staticDemand{variants: []policy.VariantDemand{{Model: big, Parked: true}}}
	cfg := testConfig()
	cfg.SleepMinSize = 0
	cfg.PodMemoryBytes = 100 << 30
	r := New(p, d, cfg)

	var buf bytes.Buffer
	ctx := logTo(t, &buf)
	for i := 0; i < 3; i++ {
		if _, err := r.Once(ctx); err != nil {
			t.Fatalf("Once: %v", err)
		}
	}
	if got := strings.Count(buf.String(), "will not warm this model"); got != 1 {
		t.Fatalf("a standing decline must be said once, got %d", got)
	}
}

func TestResidentCountsModelsNotEmptyPods(t *testing.T) {
	// Found in review. An EMPTY Pod contributes a placeholder membership so idle
	// Pods are visible as reserve, so len(memberships) reports a pool holding
	// NOTHING as resident=2 -- which an operator reads as two models warm, and
	// concludes the pool is working when it has never warmed anything.
	p := &fakePool{memberships: []pool.Membership{
		{Pod: podA(), State: pool.Absent},
		{Pod: types.NamespacedName{Namespace: "pool", Name: "b"}, State: pool.Absent},
	}}
	r := New(p, &staticDemand{}, testConfig())

	var buf bytes.Buffer
	ctx := logTo(t, &buf)
	if _, err := r.Once(ctx); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if !strings.Contains(buf.String(), "resident=0") {
		t.Fatalf("an empty pool holds no models: %s", buf.String())
	}
}
