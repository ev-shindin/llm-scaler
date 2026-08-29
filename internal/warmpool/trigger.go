package warmpool

import (
	"context"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
)

// DecisionTrigger fires the reconciler when WVA writes a scale decision.
//
// It subscribes to the same store the external scaler's StreamIsActive watches,
// for the same reason: a decision is known here BEFORE KEDA is told about it,
// and a bridge that starts at decision time is up before the ordinary replicas
// have finished being asked for. Waiting for KEDA's poll would add its interval
// to a wake measured in hundreds of milliseconds.
type DecisionTrigger struct {
	store *decision.Store

	mu          sync.Mutex
	fired       chan struct{}
	unsubscribe []func()
	watched     map[types.NamespacedName]bool
	// stop ends the per-target goroutines. The store's cancel only removes the
	// registration and never closes the channel, so a goroutine ranging over it
	// would park forever -- one leaked per watched target, every time this is
	// rebuilt.
	stop chan struct{}
}

// NewDecisionTrigger watches the given scale targets.
func NewDecisionTrigger(store *decision.Store, targets []types.NamespacedName) *DecisionTrigger {
	t := &DecisionTrigger{
		store:   store,
		watched: map[types.NamespacedName]bool{},
		// Depth one, and never blocking: several decisions landing together
		// need one reconcile, not one each. A full buffer already means "go
		// round again", which is exactly what a second signal would say.
		fired: make(chan struct{}, 1),
		stop:  make(chan struct{}),
	}
	for _, target := range targets {
		t.watch(target)
	}
	return t
}

// Notify returns the channel the reconciler selects on.
func (t *DecisionTrigger) Notify() <-chan struct{} { return t.fired }

// Watch adds a target after construction, for variants discovered later.
// Watching one twice is a no-op: WVA discovers workloads from KEDA calls, so the
// same target is offered on every sync.
func (t *DecisionTrigger) Watch(target types.NamespacedName) { t.watch(target) }

func (t *DecisionTrigger) watch(target types.NamespacedName) {
	t.mu.Lock()
	if t.watched[target] {
		t.mu.Unlock()
		return
	}
	t.watched[target] = true
	t.mu.Unlock()

	updates, cancel := t.store.Subscribe(target.Namespace, target.Name)

	t.mu.Lock()
	t.unsubscribe = append(t.unsubscribe, cancel)
	t.mu.Unlock()

	go func() {
		for {
			select {
			case <-t.stop:
				return
			case _, open := <-updates:
				if !open {
					return
				}
				select {
				case t.fired <- struct{}{}:
				default: // a reconcile is already pending; one is enough
				}
			}
		}
	}()
}

// WatchRegistry keeps the trigger's subscriptions in step with what WVA has
// discovered, until ctx ends.
//
// Necessary because discovery is call-driven: a workload becomes known when KEDA
// first asks about it, so the set of scale targets grows during a run and a
// trigger built once at startup would never fire for anything registered later.
func (t *DecisionTrigger) WatchRegistry(ctx context.Context, reg *registry.Registry, every time.Duration) {
	if every <= 0 {
		every = 30 * time.Second
	}
	wait.UntilWithContext(ctx, func(context.Context) {
		for _, entry := range reg.Snapshot() {
			if entry.Target.Name == "" {
				continue // not enriched yet; the decision store is keyed by the
				// scale target's name, which is only known after a read
			}
			t.Watch(types.NamespacedName{Namespace: entry.Namespace, Name: entry.Target.Name})
		}
	}, every)
}

// Close releases every subscription and stops the goroutines reading them.
//
// Both halves are needed: the store's cancel removes the registration but never
// closes the channel, so without the stop signal each goroutine parks on a
// receive that can no longer fire.
func (t *DecisionTrigger) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, cancel := range t.unsubscribe {
		cancel()
	}
	t.unsubscribe = nil
	t.watched = map[types.NamespacedName]bool{}
	select {
	case <-t.stop: // Close is idempotent
	default:
		close(t.stop)
	}
}
