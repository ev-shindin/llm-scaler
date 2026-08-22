// Package warmpool runs the pool: it observes what is resident, asks the policy
// what should happen, and carries that out through the port.
//
// It is driven from two clocks on purpose. Borrows are event-driven, off the
// same decision store the external scaler watches, so a bridge starts when WVA
// DECIDES rather than when KEDA next polls -- the difference between a
// sub-second bridge and one that begins a poll interval late. Everything else --
// refreshing what is resident, handing back finished bridges, admitting and
// growing -- runs on a slower reconcile, because it involves an HTTP fan-out
// across the pool and a model load costs ~35 s.
//
// The pool never tells KEDA anything. It borrows underneath the same decision
// KEDA is about to act on, and the ordinary scale-up proceeds untouched, which
// is what keeps lent capacity out of the metric KEDA reads.
package warmpool

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/policy"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

// DemandSource reports what each variant currently wants. It is a READ of
// decisions WVA has already made -- the desired count from the decision store,
// the ready count from the scale target -- never a second opinion about them.
type DemandSource interface {
	Variants(ctx context.Context) ([]policy.VariantDemand, error)
}

// Trigger fires when a scale decision lands. The decision store already offers
// exactly this, and the external scaler already uses it to push activation to
// KEDA rather than waiting for a poll; a borrow wants the same timing.
type Trigger interface {
	Notify() <-chan struct{}
}

// Reconciler drives the pool.
type Reconciler struct {
	Pool   pool.Pool
	Demand DemandSource
	Config policy.Config

	// Trigger is optional. Without it the pool still works, just at reconcile
	// latency: borrows would wait for the next tick instead of starting when
	// the decision lands.
	Trigger Trigger
	// Interval is the housekeeping cadence: returns, admissions, growth.
	Interval time.Duration
	// Name identifies this pool in its metrics.
	Name string

	// ObserveTimeout bounds one observation -- the HTTP fan-out across the pool
	// plus the read of what each variant wants.
	//
	// Bounded because the pool talks to Pods over the network and the default
	// client timeout is 120 s. One Pod that accepts a connection and never
	// answers would otherwise hold the whole loop for that long, per instance
	// behind it, and a reconcile that should take milliseconds becomes minutes.
	// A borrow that arrives after the cold start it was meant to beat is worse
	// than no pool at all, so giving up early and going round again is the
	// correct answer.
	ObserveTimeout time.Duration
	// ActTimeout bounds one borrow or one return. A return includes the drain
	// wait, so this must comfortably exceed the adapter's DrainWait.
	ActTimeout time.Duration
	// AdmitTimeout bounds one admission, which is a model load: ~33-37 s
	// measured for 0.6 B, and minutes for anything large. It runs in its own
	// goroutine, so it is generous where the others are tight.
	AdmitTimeout time.Duration

	// passMu serialises a whole observe-decide-act pass. The finer mu below
	// guards the bookkeeping maps, which is not the same thing: two passes
	// could each read the pool, each see the same Pod free, and each spend it,
	// without either touching a map the other had locked. Once is exported and
	// its doc invites an operator to call it, so this cannot be left to
	// convention.
	passMu sync.Mutex

	mu         sync.Mutex
	borrowedAt map[policy.Borrow]time.Time
	missesAt   map[string][]time.Time
	admitting  map[string]bool

	// now exists so tests do not sleep.
	now func() time.Time
}

// New returns a Reconciler ready to Start.
func New(p pool.Pool, demand DemandSource, cfg policy.Config) *Reconciler {
	return &Reconciler{
		Pool:           p,
		Demand:         demand,
		Config:         cfg,
		Interval:       5 * time.Second,
		ObserveTimeout: defaultObserveTimeout,
		ActTimeout:     defaultActTimeout,
		AdmitTimeout:   defaultAdmitTimeout,

		borrowedAt: map[policy.Borrow]time.Time{},
		missesAt:   map[string][]time.Time{},
		admitting:  map[string]bool{},
		now:        time.Now,
	}
}

// Start runs until ctx ends.
func (r *Reconciler) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()

	var decided <-chan struct{}
	if r.Trigger != nil {
		decided = r.Trigger.Notify()
	}

	for {
		if _, err := r.Once(ctx); err != nil {
			log.FromContext(ctx).V(1).Info("warm pool reconcile failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-decided:
			// A decision landed: go round immediately rather than waiting out
			// the tick. This is the whole reason a bridge can be up before the
			// ordinary replicas have finished being asked for.
		}
	}
}

// Once observes, decides and acts a single time. Exported because a single pass
// is exactly what a test wants, and what an operator debugging a pool wants.
//
// Admissions started by this pass OUTLIVE it -- a model load is ~35 s and must
// not hold the loop -- and they are bound to ctx, not to the pass. Start passes
// the loop's own context, which is what makes that work: an admission survives
// every tick and ends only at shutdown or AdmitTimeout. A caller that passes a
// short-lived context instead gets the other behaviour, and cancelling it
// cancels the load in flight. That is the right answer for a caller who asked
// for one pass and then walked away, but it is worth knowing before writing
//
//	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
//	defer cancel()
//	r.Once(ctx)
//
// which cancels a 35 s admission at 30 s, every time.
func (r *Reconciler) Once(ctx context.Context) (policy.Plan, error) {
	r.passMu.Lock()
	defer r.passMu.Unlock()

	memberships, variants, err := r.observe(ctx)
	if err != nil {
		return policy.Plan{}, err
	}

	r.mu.Lock()
	in := policy.Input{
		Memberships: memberships,
		Variants:    variants,
		BorrowedAt:  copyBorrows(r.borrowedAt),
		MissesAt:    copyMisses(r.missesAt),
		Now:         r.now(),
	}
	r.mu.Unlock()

	plan := policy.Decide(in, r.Config)
	metrics.SetWarmPoolFreePods(r.Name, pool.FreePods(memberships))
	r.apply(ctx, plan)
	return plan, nil
}

// apply carries out a plan. Returns come first, as the policy decided them:
// a Pod handed back is reserve for every model, including the borrows below.
func (r *Reconciler) apply(ctx context.Context, plan policy.Plan) {
	logger := log.FromContext(ctx).WithName("warmpool")

	// Returns are planned as though they will succeed, and the borrows and
	// admissions below were chosen from a free set that INCLUDES them -- see
	// policy.Decide, which folds returning Pods into `free` precisely so a
	// handed-back Pod can serve a borrow in the same cycle. When a return fails
	// that assumption is wrong, and acting on it anyway puts a second engine on
	// a GPU sized for one: the first model is still awake, and Activate would
	// label, wake and point a second on top of it.
	stillLent := map[types.NamespacedName]bool{}

	for _, action := range plan.Return {
		if err := r.act(ctx, func(ctx context.Context) error {
			return r.Pool.Deactivate(ctx, action.Pod, action.Model)
		}); err != nil {
			logger.V(1).Info("could not return a bridge", "pod", action.Pod, "variant", action.Model.Variant, "err", err)
			stillLent[action.Pod] = true
			continue
		}
		// Observed as the bridge ends, so the distribution answers the question
		// the pool exists for: a bridge should last about as long as an ordinary
		// replica takes to start, and one sitting at the hold timeout means the
		// scale-up it covers is failing while the pool hides it.
		if held, ok := r.heldFor(action); ok {
			metrics.ObserveWarmPoolBridge(action.Model.Namespace, action.Model.Variant, held.Seconds())
		}
		r.forget(action)
	}

	for _, action := range plan.Borrow {
		if stillLent[action.Pod] {
			// Counted as blocked, which is what it is: the model is resident
			// and the reserve could not produce a Pod for it.
			logger.V(1).Info("not borrowing a Pod whose return failed",
				"pod", action.Pod, "variant", action.Model.Variant)
			metrics.CountWarmPoolBorrow(action.Model.Namespace, action.Model.Variant, OutcomeBlocked)
			continue
		}

		// Recorded BEFORE the attempt, not after. A borrow that fails partway --
		// labelled and awake but never pointed at -- still reads as lent to the
		// next cycle, and with no start time its age is recomputed as zero
		// forever, so the hold timeout could never reclaim it. Recording first
		// means the timeout applies whatever the attempt does.
		r.recordBorrow(action)

		if err := r.act(ctx, func(ctx context.Context) error {
			_, err := r.Pool.Activate(ctx, action.Pod, action.Model)
			return err
		}); err != nil {
			logger.V(1).Info("wake failed; returning the Pod and falling through to the cold path",
				"pod", action.Pod, "variant", action.Model.Variant, "err", err)
			// Put it back rather than leave it half-borrowed. Without this one
			// transient failure removes a Pod from the reserve permanently: it
			// reads as lent, so nothing retries the variant elsewhere, and it
			// serves no traffic.
			back := r.act(ctx, func(ctx context.Context) error {
				return r.Pool.Deactivate(ctx, action.Pod, action.Model)
			})
			if back != nil {
				logger.V(1).Info("could not return a Pod after a failed wake; the hold timeout will reclaim it",
					"pod", action.Pod, "variant", action.Model.Variant, "err", back)
			} else {
				r.forget(action)
			}
			// A warm copy that cannot wake is not warm, so it counts as a miss
			// for the frequency filter.
			r.recordMiss(action.Model.Variant)
			metrics.CountWarmPoolBorrow(action.Model.Namespace, action.Model.Variant, OutcomeMiss)
			continue
		}
		metrics.CountWarmPoolBorrow(action.Model.Namespace, action.Model.Variant, OutcomeHit)
	}

	// Admission takes ~35 s, so it must not hold up the next cycle. The Pod
	// shows as Loading on the following pass, which is what keeps it out of the
	// reserve, so the state is discovered rather than remembered.
	for _, action := range plan.Admit {
		if stillLent[action.Pod] {
			// Worse here than for a borrow: admission starts a full model load
			// on a Pod whose previous engine never slept, and Warm only checks
			// whether THIS model is resident, not whether the Pod is occupied.
			logger.V(1).Info("not admitting into a Pod whose return failed",
				"pod", action.Pod, "variant", action.Model.Variant)
			continue
		}
		if !r.beginAdmission(action.Model.Variant) {
			continue
		}
		go func(action policy.Action) {
			defer r.endAdmission(action.Model.Variant)
			// From the loop's context, not the tick's: an admission outlives
			// the pass that decided it by design, and cancelling it at the next
			// tick would restart a ~35 s load every 5 s forever.
			admitCtx, cancel := context.WithTimeout(ctx, orDefault(r.AdmitTimeout, defaultAdmitTimeout))
			defer cancel()
			if err := r.Pool.Warm(admitCtx, action.Pod, action.Model, r.tierFor(action.Model)); err != nil {
				logger.V(1).Info("admission failed", "pod", action.Pod, "variant", action.Model.Variant, "err", err)
			}
		}(action)
	}

	for _, action := range plan.Evict {
		if err := r.act(ctx, func(ctx context.Context) error {
			return r.Pool.Evict(ctx, action.Pod, action.Model)
		}); err != nil {
			logger.V(1).Info("eviction failed", "pod", action.Pod, "variant", action.Model.Variant, "err", err)
		}
	}

	for _, model := range plan.Missed {
		r.recordMiss(model.Variant)
		metrics.CountWarmPoolBorrow(model.Namespace, model.Variant, OutcomeMiss)
	}
	for _, model := range plan.Blocked {
		// Counted apart from a miss on purpose: this one says the reserve was
		// too small, not that the warm set was wrong.
		metrics.CountWarmPoolBorrow(model.Namespace, model.Variant, OutcomeBlocked)
	}
	if plan.GrowBy > 0 {
		// Reported, not acted on: growing costs a model load per Pod, and a
		// shortfall that lasts one cycle is a borrow doing its job.
		logger.V(2).Info("warm pool is below its reserve", "short", plan.GrowBy)
	}
}

// Outcomes of an attempt to cover a scale-up from the pool. They partition the
// same event, which is why they are one metric with a label rather than three.
const (
	OutcomeHit     = "hit"
	OutcomeBlocked = "blocked"
	OutcomeMiss    = "miss"
)

// heldFor reports how long a bridge lasted, or false if this process never saw
// it begin -- after a restart the pool discovers lent Pods it did not lend, and
// reporting a made-up duration would be worse than reporting none.
func (r *Reconciler) heldFor(action policy.Action) (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.borrowedAt[policy.Borrow{Pod: action.Pod, Variant: action.Model.Variant}]
	if !ok {
		return 0, false
	}
	return r.now().Sub(at), true
}

func (r *Reconciler) tierFor(pool.ModelRef) pool.Tier {
	// One tier per pool today. When a pool spans tiers this reads it from the
	// model's policy rather than the pool's.
	return pool.Ram
}

func (r *Reconciler) recordBorrow(action policy.Action) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := policy.Borrow{Pod: action.Pod, Variant: action.Model.Variant}
	if _, known := r.borrowedAt[key]; !known {
		r.borrowedAt[key] = r.now()
	}
}

func (r *Reconciler) forget(action policy.Action) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.borrowedAt, policy.Borrow{Pod: action.Pod, Variant: action.Model.Variant})
}

// recordMiss feeds the frequency filter, and forgets misses older than the
// window so a model that missed twice last week is not admitted today.
func (r *Reconciler) recordMiss(variant string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	kept := []time.Time{now}
	for _, at := range r.missesAt[variant] {
		if now.Sub(at) <= r.Config.AdmissionWindow {
			kept = append(kept, at)
		}
	}
	r.missesAt[variant] = kept
}

// Defaults for the three deadlines. They are separate constants rather than one
// because they bound different things: a fan-out of small calls, a single wake,
// and a model load.
const (
	defaultObserveTimeout = 15 * time.Second
	defaultActTimeout     = 60 * time.Second
	defaultAdmitTimeout   = 10 * time.Minute
)

// observe reads what is resident and what each variant wants, under one
// deadline. Both halves are reads, and a pass that cannot complete them has
// nothing to decide from.
func (r *Reconciler) observe(ctx context.Context) ([]pool.Membership, []policy.VariantDemand, error) {
	observeCtx, cancel := context.WithTimeout(ctx, orDefault(r.ObserveTimeout, defaultObserveTimeout))
	defer cancel()

	memberships, err := r.Pool.ListWarm(observeCtx)
	if err != nil {
		return nil, nil, err
	}
	variants, err := r.Demand.Variants(observeCtx)
	if err != nil {
		return nil, nil, err
	}
	return memberships, variants, nil
}

// act runs one borrow, return or eviction under its own deadline, so that one
// Pod that will not answer costs a single action rather than the pass.
func (r *Reconciler) act(ctx context.Context, do func(context.Context) error) error {
	actCtx, cancel := context.WithTimeout(ctx, orDefault(r.ActTimeout, defaultActTimeout))
	defer cancel()
	return do(actCtx)
}

// orDefault lets a Reconciler built as a struct literal -- which is what tests
// and any future caller that does not use New will do -- still be bounded.
// An unset timeout is a mistake, not a request for none.
func orDefault(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

// beginAdmission reports whether this variant's admission should start, and
// claims it if so. Without this the same admission would be re-issued every
// cycle for the ~35 s it takes, since the model is not resident yet.
func (r *Reconciler) beginAdmission(variant string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.admitting[variant] {
		return false
	}
	r.admitting[variant] = true
	return true
}

func (r *Reconciler) endAdmission(variant string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.admitting, variant)
}

func copyBorrows(in map[policy.Borrow]time.Time) map[policy.Borrow]time.Time {
	return maps.Clone(in)
}

// copyMisses is NOT maps.Clone: the values are slices, and a shallow copy would
// alias their backing arrays between the live map and the snapshot handed to the
// policy.
func copyMisses(in map[string][]time.Time) map[string][]time.Time {
	out := make(map[string][]time.Time, len(in))
	for k, v := range in {
		out[k] = append([]time.Time(nil), v...)
	}
	return out
}

// Lent reports which Pods are currently lent, for diagnostics and for a caller
// that wants to know before shutting a pool down.
func (r *Reconciler) Lent() []policy.Borrow {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Collect(maps.Keys(r.borrowedAt))
}

// PodOf is a small convenience for callers assembling actions by hand.
func PodOf(namespace, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
}
