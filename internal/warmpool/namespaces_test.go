package warmpool

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
)

// poolEntry is a registered ScaledObject that DECLARES a pool.
func poolEntry(namespace, name string) registry.Entry {
	return registry.Entry{
		Name:      name,
		Namespace: namespace,
		Metadata:  map[string]string{registry.WarmPoolNameKey: name},
	}
}

// modelEntry is a registered ScaledObject that scales a MODEL. It must never
// start a reconciler: a namespace full of models and no pool has nothing to
// reconcile, and starting one there would poll for pool Pods that do not exist.
func modelEntry(namespace, name string) registry.Entry {
	return registry.Entry{
		Name:      name,
		Namespace: namespace,
		Metadata:  map[string]string{registry.ModelIDKey: "some/model"},
	}
}

// starts records which namespaces were started and stopped, in order.
type starts struct {
	mu      sync.Mutex
	started []string
	stopped []string
	// done is the channel handed back for each namespace's most recent start.
	// Closing one stands for that reconciler returning on its own.
	done map[string]chan struct{}
}

func (s *starts) start(_ context.Context, ns string) (context.CancelFunc, <-chan struct{}) {
	s.mu.Lock()
	s.started = append(s.started, ns)
	if s.done == nil {
		s.done = map[string]chan struct{}{}
	}
	done := make(chan struct{})
	s.done[ns] = done
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.stopped = append(s.stopped, ns)
		s.mu.Unlock()
	}, done
}

// finish stands for the reconciler of ns returning of its own accord.
func (s *starts) finish(ns string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.done[ns])
}

func (s *starts) snapshot() ([]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.started...), append([]string(nil), s.stopped...)
}

// A pool declared in a namespace nobody named must be reconciled.
//
// This is the cluster-scoped case the flag could not cover: the namespace is not
// derivable, so before this the pool was simply never used -- Pods running,
// accelerators held, and WVA silent about them.
func TestAPoolIsReconciledWhereverItIsDeclared(t *testing.T) {
	rec := &starts{}
	m := &Multiplexer{
		Snapshot: func() []registry.Entry {
			return []registry.Entry{poolEntry("tenant-a", "default"), modelEntry("tenant-b", "some-model")}
		},
		Start: rec.start,
	}
	m.sync(context.Background())

	started, _ := rec.snapshot()
	if len(started) != 1 || started[0] != "tenant-a" {
		t.Fatalf("started %v, want exactly [tenant-a]", started)
	}
}

// A namespace with models and no pool starts nothing. Reconciling it would poll
// for Pods that do not exist, on every pass, forever.
func TestAModelOnlyNamespaceStartsNothing(t *testing.T) {
	rec := &starts{}
	m := &Multiplexer{
		Snapshot: func() []registry.Entry { return []registry.Entry{modelEntry("tenant-b", "some-model")} },
		Start:    rec.start,
	}
	m.sync(context.Background())

	if started, _ := rec.snapshot(); len(started) != 0 {
		t.Errorf("started %v, want none", started)
	}
}

// The same namespace is started ONCE however many pools it declares, and however
// many times the set is recomputed. Two reconcilers on one namespace would both
// observe the same Pods and both act on them.
func TestANamespaceIsStartedOnce(t *testing.T) {
	rec := &starts{}
	m := &Multiplexer{
		Snapshot: func() []registry.Entry {
			return []registry.Entry{poolEntry("tenant-a", "h100"), poolEntry("tenant-a", "a100")}
		},
		Start: rec.start,
	}
	m.sync(context.Background())
	m.sync(context.Background())
	m.sync(context.Background())

	if started, _ := rec.snapshot(); len(started) != 1 {
		t.Errorf("started %v, want one reconciler for the namespace", started)
	}
}

// A pool that appears AFTER the controller started is picked up, which is the
// whole point: discovery is call-driven, so a namespace becomes known when KEDA
// first asks about a pool in it, which can be at any time.
func TestAPoolDeclaredLaterIsPickedUp(t *testing.T) {
	rec := &starts{}
	var mu sync.Mutex
	entries := make([]registry.Entry, 0, 1)
	m := &Multiplexer{
		Snapshot: func() []registry.Entry {
			mu.Lock()
			defer mu.Unlock()
			return append([]registry.Entry(nil), entries...)
		},
		Start: rec.start,
	}

	m.sync(context.Background())
	if started, _ := rec.snapshot(); len(started) != 0 {
		t.Fatalf("started %v before any pool was declared", started)
	}

	mu.Lock()
	entries = append(entries, poolEntry("tenant-c", "default"))
	mu.Unlock()

	m.sync(context.Background())
	started, _ := rec.snapshot()
	if len(started) != 1 || started[0] != "tenant-c" {
		t.Errorf("started %v, want [tenant-c] once the pool was declared", started)
	}
}

// When the last pool in a namespace is undeclared, its reconciler stops. Left
// running it would poll a namespace that has nothing to reconcile for as long as
// the controller lives.
func TestTheLastPoolLeavingStopsTheReconciler(t *testing.T) {
	rec := &starts{}
	var mu sync.Mutex
	entries := []registry.Entry{poolEntry("tenant-a", "default")}
	m := &Multiplexer{
		Snapshot: func() []registry.Entry {
			mu.Lock()
			defer mu.Unlock()
			return append([]registry.Entry(nil), entries...)
		},
		Start: rec.start,
	}

	m.sync(context.Background())
	mu.Lock()
	entries = nil
	mu.Unlock()
	m.sync(context.Background())

	_, stopped := rec.snapshot()
	if len(stopped) != 1 || stopped[0] != "tenant-a" {
		t.Errorf("stopped %v, want [tenant-a]", stopped)
	}
}

// Run stops every child when its context ends, rather than leaving goroutines
// behind for whoever runs it next.
func TestRunStopsEveryReconcilerOnShutdown(t *testing.T) {
	rec := &starts{}
	m := &Multiplexer{
		Snapshot: func() []registry.Entry { return []registry.Entry{poolEntry("tenant-a", "default")} },
		Start:    rec.start,
		Every:    10 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := m.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, stopped := rec.snapshot(); len(stopped) != 1 {
		t.Errorf("stopped %v on shutdown, want the one running reconciler", stopped)
	}
}

// A reconciler that exits on its own is started again.
//
// The clearest way one exits is the RBAC review denying borrows, which returns
// immediately and logs why. Before this, the Multiplexer kept that namespace's
// stop func and read it as still running, so it skipped the namespace on every
// later pass: an operator who granted the missing permission would see nothing
// change until the controller was restarted. That is the same "Pods running,
// accelerators held, WVA silent" failure this type was written to remove, just
// arrived at from the other side.
func TestANamespaceWhoseReconcilerExitedIsStartedAgain(t *testing.T) {
	rec := &starts{}
	now := time.Now()
	m := &Multiplexer{
		Snapshot: func() []registry.Entry {
			return []registry.Entry{poolEntry("tenant-a", "default")}
		},
		Start:      rec.start,
		RetryAfter: time.Minute,
		Now:        func() time.Time { return now },
	}

	m.sync(context.Background())
	rec.finish("tenant-a") // the reconciler returned, e.g. borrows are denied

	// Noticed, but not retried yet: an unfixable complaint restated twice a
	// minute forever is its own kind of silence.
	m.sync(context.Background())
	if started, _ := rec.snapshot(); len(started) != 1 {
		t.Fatalf("started %v, want no restart inside the quiet window", started)
	}

	now = now.Add(2 * time.Minute)
	m.sync(context.Background())
	started, _ := rec.snapshot()
	if len(started) != 2 || started[1] != "tenant-a" {
		t.Errorf("started %v, want tenant-a started again once RetryAfter had passed", started)
	}
}

// A namespace whose reconciler is still running is not started a second time,
// even long after RetryAfter would have elapsed. Two reconcilers on one
// namespace would both observe the same Pods and both act on them.
func TestARunningReconcilerIsNeverStartedTwice(t *testing.T) {
	rec := &starts{}
	now := time.Now()
	m := &Multiplexer{
		Snapshot: func() []registry.Entry {
			return []registry.Entry{poolEntry("tenant-a", "default")}
		},
		Start:      rec.start,
		RetryAfter: time.Minute,
		Now:        func() time.Time { return now },
	}

	m.sync(context.Background())
	for i := 0; i < 3; i++ {
		now = now.Add(time.Hour)
		m.sync(context.Background())
	}

	if started, _ := rec.snapshot(); len(started) != 1 {
		t.Errorf("started %v, want exactly one reconciler for a namespace still running", started)
	}
}
