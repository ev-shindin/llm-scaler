package warmpool

import (
	"sync"

	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
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
}

// NewDecisionTrigger watches the given scale targets.
func NewDecisionTrigger(store *decision.Store, targets []types.NamespacedName) *DecisionTrigger {
	t := &DecisionTrigger{
		store: store,
		// Depth one, and never blocking: several decisions landing together
		// need one reconcile, not one each. A full buffer already means "go
		// round again", which is exactly what a second signal would say.
		fired: make(chan struct{}, 1),
	}
	for _, target := range targets {
		t.watch(target)
	}
	return t
}

// Notify returns the channel the reconciler selects on.
func (t *DecisionTrigger) Notify() <-chan struct{} { return t.fired }

// Watch adds a target after construction, for variants discovered later.
func (t *DecisionTrigger) Watch(target types.NamespacedName) { t.watch(target) }

func (t *DecisionTrigger) watch(target types.NamespacedName) {
	updates, cancel := t.store.Subscribe(target.Namespace, target.Name)

	t.mu.Lock()
	t.unsubscribe = append(t.unsubscribe, cancel)
	t.mu.Unlock()

	go func() {
		for range updates {
			select {
			case t.fired <- struct{}{}:
			default: // a reconcile is already pending; one is enough
			}
		}
	}()
}

// Close releases every subscription.
func (t *DecisionTrigger) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, cancel := range t.unsubscribe {
		cancel()
	}
	t.unsubscribe = nil
}
