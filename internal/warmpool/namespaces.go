package warmpool

import (
	"context"
	"maps"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
)

// Multiplexer runs one pool reconciler per namespace that DECLARES a pool, and
// keeps that set in step with what WVA has discovered.
//
// It exists for the cluster-scoped install, where the namespace holding the pool
// cannot be derived. A namespace-scoped controller watches exactly one namespace
// and the pool warms the workloads in it, so there is nothing to multiplex; a
// cluster-scoped one has no such namespace, and until now the answer was a flag
// naming a single namespace, set at startup. That has two consequences an
// operator has no way to see: a pool created in any OTHER namespace is never
// used, and a pool created after the controller started needs a restart before
// it is. Both look identical from outside -- Pods running, accelerators held,
// and WVA saying nothing about them.
//
// A pool is still opted into, and the opt-in is unchanged: somebody has to
// create the pool workload and the ScaledObject whose trigger declares it. What
// changes is that WVA now acts on that declaration wherever it appears, rather
// than only where a flag pointed. Holding accelerators WVA refuses to use is not
// the safe side of this decision.
type Multiplexer struct {
	// Snapshot is what WVA has been called about. Injected rather than taken
	// from the package default so a test needs no global.
	Snapshot func() []registry.Entry

	// Start begins reconciling one namespace. It returns the func that stops it
	// and a channel closed when that reconciler has finished, however it
	// finished. Called once per namespace that declares a pool, and never twice
	// for the same one while it is running.
	//
	// The DONE channel is what keeps a namespace from being written off. A
	// reconciler can exit on its own -- the clearest case is the RBAC review
	// refusing borrows, which returns immediately -- and a Multiplexer holding
	// only the stop func cannot tell that apart from one still running. It would
	// then skip the namespace on every later pass, so an operator who granted the
	// missing permission would need a restart before WVA used the pool, which is
	// the exact silence this type exists to remove.
	Start func(ctx context.Context, namespace string) (context.CancelFunc, <-chan struct{})

	// Every bounds how often the set is recomputed. Discovery is call-driven, so
	// a namespace appears when KEDA first asks about a pool in it.
	Every time.Duration

	// RetryAfter is how long to leave a namespace alone after its reconciler
	// exited before starting it again. Defaults to two minutes.
	//
	// Retrying is the point, but retrying every tick would restate an unfixable
	// complaint -- denied RBAC does not resolve itself -- twice a minute forever.
	// A wait long enough to be quiet and short enough that a granted permission
	// takes effect on its own is the whole requirement.
	RetryAfter time.Duration

	// Now is the clock, injectable so a test need not sleep.
	Now func() time.Time

	running map[string]child
	// quiet holds, per namespace, the time before which its reconciler is not
	// started again.
	quiet map[string]time.Time
}

// child is one namespace's running reconciler.
type child struct {
	stop context.CancelFunc
	done <-chan struct{}
}

// Run keeps a reconciler running for every namespace that declares a pool, until
// ctx ends.
func (m *Multiplexer) Run(ctx context.Context) error {
	every := m.Every
	if every <= 0 {
		every = 30 * time.Second
	}
	m.running = map[string]child{}
	m.quiet = map[string]time.Time{}
	// Stopping every child on the way out, rather than relying on ctx alone:
	// the children are started from ctx, so cancellation does reach them, but a
	// Multiplexer that leaves its map populated cannot be run twice and would
	// leak the goroutines on the second Run.
	defer m.stopAll()

	wait.UntilWithContext(ctx, m.sync, every)
	return nil
}

// sync starts what is newly declared and stops what no longer is.
func (m *Multiplexer) sync(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("warmpool")
	// Lazily, so sync is callable on a zero Multiplexer -- Run seeds these too,
	// and a nil map here would panic on the first namespace rather than at the
	// point the omission was made.
	if m.running == nil {
		m.running = map[string]child{}
	}
	if m.quiet == nil {
		m.quiet = map[string]time.Time{}
	}
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	retryAfter := m.RetryAfter
	if retryAfter <= 0 {
		retryAfter = 2 * time.Minute
	}

	// Reconcilers that have finished on their own. Forgetting them is what makes
	// the next pass start them again; keeping them would mark the namespace as
	// handled by a goroutine that is not there.
	for _, ns := range sortedKeys(m.running) {
		c := m.running[ns]
		select {
		case <-c.done:
			logger.V(logging.DEFAULT).Info(
				"the warm pool reconciler for this namespace exited; it will be started "+
					"again shortly. If this repeats, the reason is in the lines it logged "+
					"as it stopped -- denied permission to patch Pods is the usual one",
				"namespace", ns, "retryAfter", retryAfter)
			c.stop() // releases the child context; the goroutine is already gone
			delete(m.running, ns)
			m.quiet[ns] = now().Add(retryAfter)
		default:
		}
	}

	declared := map[string]bool{}
	for _, entry := range m.Snapshot() {
		if entry.Namespace == "" || !registry.ScalesAWarmPool(entry.Metadata) {
			continue
		}
		declared[entry.Namespace] = true
	}

	// Sorted so the log reads the same way twice for the same set. Namespaces
	// arrive from a map, and an operator comparing two lines should not have to
	// wonder whether the order means anything.
	for _, ns := range sortedKeys(declared) {
		if _, running := m.running[ns]; running {
			continue
		}
		if until, waiting := m.quiet[ns]; waiting && now().Before(until) {
			continue
		}
		delete(m.quiet, ns)
		logger.V(logging.DEFAULT).Info(
			"warm pool declared in a namespace this controller was not reconciling; starting one",
			"namespace", ns)
		stop, done := m.Start(ctx, ns)
		m.running[ns] = child{stop: stop, done: done}
	}

	for _, ns := range sortedKeys(m.running) {
		if declared[ns] {
			continue
		}
		// The LAST pool in the namespace is gone. Stopping is right and is not
		// destructive: the Pods are the operator's, and WVA has simply stopped
		// having an opinion about them. Nothing is deleted here.
		logger.V(logging.DEFAULT).Info(
			"no ScaledObject declares a warm pool in this namespace any more; "+
				"stopping its reconciler. Any pool Pods still running hold their "+
				"accelerators and WVA will not use them until a trigger declares them again",
			"namespace", ns)
		m.running[ns].stop()
		delete(m.running, ns)
		delete(m.quiet, ns)
	}
}

func (m *Multiplexer) stopAll() {
	for ns, c := range m.running {
		c.stop()
		delete(m.running, ns)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
