package warmpool

import (
	"context"
	"sort"
	"time"

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

	// Start begins reconciling one namespace and returns the func that stops it.
	// Called once per namespace that declares a pool, and never twice for the
	// same one while it is running.
	Start func(ctx context.Context, namespace string) context.CancelFunc

	// Every bounds how often the set is recomputed. Discovery is call-driven, so
	// a namespace appears when KEDA first asks about a pool in it.
	Every time.Duration

	running map[string]context.CancelFunc
}

// Run keeps a reconciler running for every namespace that declares a pool, until
// ctx ends.
func (m *Multiplexer) Run(ctx context.Context) error {
	every := m.Every
	if every <= 0 {
		every = 30 * time.Second
	}
	m.running = map[string]context.CancelFunc{}
	// Stopping every child on the way out, rather than relying on ctx alone:
	// the children are started from ctx, so cancellation does reach them, but a
	// Multiplexer that leaves its map populated cannot be run twice and would
	// leak the goroutines on the second Run.
	defer m.stopAll()

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		m.sync(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// sync starts what is newly declared and stops what no longer is.
func (m *Multiplexer) sync(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("warmpool")

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
		if m.running[ns] != nil {
			continue
		}
		logger.V(logging.DEFAULT).Info(
			"warm pool declared in a namespace this controller was not reconciling; starting one",
			"namespace", ns)
		m.running[ns] = m.Start(ctx, ns)
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
		m.running[ns]()
		delete(m.running, ns)
	}
}

func (m *Multiplexer) stopAll() {
	for ns, stop := range m.running {
		stop()
		delete(m.running, ns)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
