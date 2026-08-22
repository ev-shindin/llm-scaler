// Package gpuusage keeps the cluster's GPU usage picture up to date, independently
// of any scaling engine.
//
// It exists because the picture used to be a by-product of optimization: the
// saturation engine summed its own population each cycle and published that. Two
// things follow from deriving it that way, and both are load-bearing:
//
//   - it only exists once something is already running. The saturation engine
//     returns before publishing when no variant is active, so a fleet parked at
//     zero — the state scale-from-zero exists to serve — produced no picture at
//     all, and the placement check silently degraded to "unknown", which is
//     permissive. Wakes were published onto accelerators nobody had checked;
//   - it could only ever see what WVA manages. GPUs held by other namespaces,
//     system pods, or another autoscaler were invisible, so every pool reported
//     more free capacity than the cluster had.
//
// Discovering usage from the pods actually occupying GPU nodes fixes both at
// once, and does so before the first scaling decision rather than after it.
package gpuusage

import (
	"context"
	"fmt"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/gpunodes"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// DefaultInterval is how often the picture is refreshed.
//
// Matched to the saturation engine's own cadence rather than to the
// scale-from-zero loop's 10Hz: usage changes when pods start and stop, which is
// the same timescale scaling decisions happen on. It must also stay well inside
// the consumer's staleness bound (scalefromzero.gpuUsageMaxAge), or the picture
// ages out between refreshes and the placement check degrades to "unknown"
// exactly as it did before this existed.
const DefaultInterval = 15 * time.Second

// Refresher publishes the cluster's PHYSICAL GPU usage — every GPU held on a GPU
// node, whoever holds it — on a fixed interval and on demand.
//
// It is the SOLE producer of that view: both the saturation optimizer and the
// scale-from-zero placement check read what it writes. A second producer summing
// it a different way is what this replaces — two views of "what is in use" are
// free to disagree, and the two engines would then place decisions against
// different pictures of the same cluster.
//
// It is not the only MEASURE of usage. An operator-declared quota is charged for
// WVA's own variants only, and that figure is published separately by the
// saturation engine (see decision.DefaultManagedGPUUsage and
// pipeline.UsageBasis). A deployment whose only limiter is a quota therefore
// consumes nothing this produces, which is what Periodic exists to notice.
type Refresher struct {
	// Discovery observes the cluster. Required.
	Discovery gpunodes.NamespacedUsageDiscovery
	// Store receives each observation. Defaults to decision.DefaultGPUUsage.
	Store *decision.GPUUsageStore
	// Interval between refreshes. Defaults to DefaultInterval.
	Interval time.Duration
	// Periodic reports whether the PERIODIC observation is worth taking right now.
	// Consulted on every tick, not once at startup, because the configuration it
	// answers from is live: an operator switching limiter modes must not need a
	// restart for this to follow. Nil means always.
	//
	// It does NOT gate EnsureFresh. An explicit on-demand ask comes from a caller
	// that is about to decide something and has already established it needs this
	// view, so a predicate saying "nobody needs it" would be answering a question
	// the caller has just answered better.
	Periodic func() bool

	// lastPeriodic records what Periodic last said, so the transition is logged
	// once rather than every tick.
	lastPeriodic *bool

	// freshMu serialises on-demand refreshes so concurrent callers collapse onto
	// one observation. See EnsureFresh.
	freshMu sync.Mutex
}

// DecisionMaxAge is how stale an observation may be at the moment a placement
// decision is actually taken.
//
// The ticker alone is not enough. A wake is decided the instant demand appears,
// which is routinely within a second of the cluster changing — a workload that
// has just become Ready is exactly the thing a placement must account for, and on
// a 15s tick it is invisible for up to 15s after it starts holding GPUs. That
// window is not rare: the scale-from-zero e2e loses it more often than it wins,
// waking onto an accelerator that a just-started workload had already filled.
//
// Small enough that a decision effectively sees the current cluster, large enough
// to bound the work: the observation walks the pod informer's cache, so a refresh
// costs no API call, and this caps it at one walk every two seconds however hard
// the 10Hz loop asks.
const DecisionMaxAge = 2 * time.Second

// EnsureFresh takes a new observation if the published one is older than maxAge,
// so a caller about to make a decision is not reasoning about a cluster that has
// since changed. A maxAge of zero or less forces an observation outright. A nil Refresher is a no-op, which is what makes it safe to leave
// uninjected in tests.
//
// Concurrent callers collapse onto one observation rather than stampeding: the
// 10Hz loop asks once per model per tick, and they would otherwise each walk the
// cache with nothing to gain — the second walk would see what the first did.
//
// A failed observation is not reported to the caller. The previous one stands and
// the consumer's own staleness bound still applies, so the decision degrades the
// same way it would have without this call; failing the placement instead would
// turn a transient blip into a refusal to wake.
func (r *Refresher) EnsureFresh(ctx context.Context, maxAge time.Duration) {
	if r == nil {
		return
	}
	// A maxAge of zero or less means "observe now", and must not be satisfied by
	// an existing snapshot. Without this guard it usually IS: time.Since can
	// return exactly zero when the monotonic clock has not ticked between the
	// observation and the check, and `0 <= 0` then reports a stale view as
	// fresh. How often that happens is a property of the platform's timer
	// granularity rather than of anything the caller did -- on Windows, whose
	// tick is ~15 ms, it is the common case.
	if maxAge > 0 {
		if snap, ok := r.store().Get(); ok && time.Since(snap.TakenAt) <= maxAge {
			return
		}
	}

	r.freshMu.Lock()
	defer r.freshMu.Unlock()
	// Re-check under the lock: whoever held it may have just refreshed. Skipped
	// for a forced refresh, which is a caller saying it needs an observation of
	// its own rather than someone else's.
	if maxAge > 0 {
		if snap, ok := r.store().Get(); ok && time.Since(snap.TakenAt) <= maxAge {
			return
		}
	}
	if err := r.Refresh(ctx); err != nil {
		log.FromContext(ctx).V(logging.DEBUG).Info(
			"On-demand GPU usage refresh failed; deciding against the previous observation",
			"error", err.Error())
	}
}

// Refresh takes one observation and publishes it.
//
// A failed observation deliberately leaves the previous snapshot in place rather
// than publishing an empty one. Publishing zeros for a failed look would say "the
// cluster is idle" at precisely the moment WVA cannot see it — and unlike a
// missing snapshot, which consumers treat as unknown, a zeroed one is believed.
// Consumers bound how stale a snapshot may be, so a sustained outage still
// degrades to unknown; it just does not lie on the way there.
func (r *Refresher) Refresh(ctx context.Context) error {
	byType, byNamespace, err := r.Discovery.DiscoverUsageByNamespace(ctx)
	if err != nil {
		return fmt.Errorf("discovering GPU usage: %w", err)
	}
	r.store().Publish(byType, byNamespace)
	log.FromContext(ctx).V(logging.DEBUG).Info("Refreshed cluster GPU usage",
		"gpusInUse", byType, "namespaces", len(byNamespace))
	return nil
}

// Start refreshes once and then on every tick until ctx is done. It satisfies
// controller-runtime's manager.Runnable.
//
// The first refresh happens BEFORE the ticker, and that ordering is the point:
// the engines consume this, and a picture that arrives one interval late is a
// full interval in which every decision is taken with no capacity evidence. A
// failed first attempt is logged, not fatal — the cluster may simply not be
// reachable yet, and the next tick retries.
func (r *Refresher) Start(ctx context.Context) error {
	logger := log.FromContext(ctx)
	if r.periodicWanted(ctx) {
		if err := r.Refresh(ctx); err != nil {
			logger.Error(err, "Initial GPU usage discovery failed; scaling decisions have no capacity "+
				"evidence until a refresh succeeds")
		}
	}

	ticker := time.NewTicker(r.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if !r.periodicWanted(ctx) {
				continue
			}
			if err := r.Refresh(ctx); err != nil {
				// Info, not Error: a blip is expected and the previous snapshot
				// still stands. The condition that actually harms a decision is
				// the snapshot going stale, which the consumers report.
				logger.V(logging.DEBUG).Info("GPU usage refresh failed; keeping the previous snapshot",
					"error", err.Error())
			}
		}
	}
}

// periodicWanted reports whether to take this tick's observation, logging the
// answer once per change.
//
// Skipping is a real saving, not a micro-optimisation: an observation lists nodes
// and walks every pod in the cluster, so a deployment whose only limiter is a
// quota — which is charged for WVA's own variants and never consults the physical
// picture — would otherwise pay that every interval, and hold the Node RBAC to do
// it, for a number nothing reads.
//
// The previous snapshot is deliberately left in place rather than cleared. It is
// stale, but nothing consults it in this state, and consumers bound staleness
// themselves; clearing would only matter if something did read it, in which case
// skipping was wrong to begin with.
func (r *Refresher) periodicWanted(ctx context.Context) bool {
	if r.Periodic == nil {
		return true
	}
	want := r.Periodic()
	if r.lastPeriodic == nil || *r.lastPeriodic != want {
		r.lastPeriodic = &want
		if want {
			log.FromContext(ctx).Info("Observing cluster GPU usage on a timer: a configured limiter " +
				"consumes the physical view")
		} else {
			log.FromContext(ctx).Info("Not observing cluster GPU usage on a timer: no configured " +
				"limiter consumes the physical view. A scale-from-zero placement still observes " +
				"on demand at the moment it decides.")
		}
	}
	return want
}

func (r *Refresher) store() *decision.GPUUsageStore {
	if r.Store != nil {
		return r.Store
	}
	return decision.DefaultGPUUsage
}

func (r *Refresher) interval() time.Duration {
	if r.Interval > 0 {
		return r.Interval
	}
	return DefaultInterval
}
