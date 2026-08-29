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
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
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
	// Namespace is where the pool's Pods and its Deployment live. Needed to
	// publish a size against the right object.
	Namespace string

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
	// admittingPods is the same claim keyed by POD. Keying only by variant is
	// not enough: two DIFFERENT variants can be sent to the same Pod on
	// consecutive passes, because the first one's instance is not visible to
	// the supervisor's listing until its create call has landed.
	admittingPods map[types.NamespacedName]bool

	// now exists so tests do not sleep.
	now func() time.Time

	// Pools reports the pools that exist. Nil means one unnamed pool running
	// under Config, which is what every install had before pools were named.
	Pools PoolSource

	// Undeclared reports warm pool Deployments that no pool declares. Nil skips
	// the check. It exists so that deleting a pool's ScaledObject and leaving
	// its Deployment behind cannot quietly become a GPU leak.
	Undeclared func(ctx context.Context, declared []PoolSpec) ([]string, error)

	// lastUndeclared is the last set reported, so a standing orphan is named
	// once rather than every pass.
	lastUndeclared string

	// Headroom reports how many GPUs of this accelerator the namespace may still
	// take, and whether that figure is usable at all. Optional; nil means the
	// pool grows without a capacity check, which is what an install with no
	// limiter declared should do.
	//
	// FALSE means "no answer" -- no limiter bounds this namespace, or none has
	// published yet -- and must not be read as zero. A pool held down because
	// nobody has published a limit would never grow on a cluster that has no
	// limiter at all.
	Headroom func(namespace, accelerator string) (int, bool)

	// Contended reports whether a model replica in this namespace is being
	// denied GPUs of a given accelerator. Nil disables the arbitration, which
	// is what a pool with no optimizer running should do.
	Contended func(namespace, accelerator string) bool

	// WantAwake reports which model the optimizer says should hold a pool's
	// GPUs, and whether it has said anything at all.
	//
	// Optional, and absent means "no opinion" rather than "sleep everything": a
	// pool without one decides from demand exactly as it did before this
	// existed, which is what every bridge pool should keep doing.
	//
	// Injected rather than read from the package default so a test needs no
	// global, and so the staleness rule lives with the caller that knows how
	// often the optimizer runs.
	WantAwake func(namespace, pool string) (string, bool)

	// lastHeld is the last reason each pool was held at its current size.
	lastHeld map[string]string

	// PublishSize records the size a pool should be, for KEDA to act on. Nil
	// leaves the pool exactly the size its Deployment says, which is what an
	// install without a pool ScaledObject wants.
	PublishSize func(namespace, name string, replicas int32)

	// lastDeclined remembers variant+reason pairs already reported, so a
	// standing mismatch is stated once rather than every pass.
	lastDeclined map[declineKey]bool

	// lastUnassignable is the last reason given for each variant that could not
	// be matched to a pool, so a standing misconfiguration is stated once rather
	// than every Interval.
	lastUnassignable map[string]string

	// lastShort is the previous pass's reserve shortfall PER POOL. Zero is a
	// real value: it is how a recovered pool re-arms, so the next shortfall is
	// reported instead of being mistaken for the one already logged.
	lastShort map[string]int
	// lastCapped dedupes the capacity-capped line, keyed by pool.
	lastCapped map[string]string
	// lastSummary is the previous pass's one-line state PER POOL, so a steady
	// pool logs once rather than every Interval. Guarded by passMu, which
	// already serialises the whole pass.
	lastSummary map[string]string

	// MinGap is the floor between passes when the TRIGGER is what woke us.
	//
	// The trigger exists so a borrow starts at decision time rather than at the
	// next tick, and that is worth keeping -- but it says "go round again", not
	// "go round again immediately, forever". Whenever it is ready the loop runs
	// Once() back to back, and a pass is cheap: measured on a two-node pool on
	// H100s, the controller made 19,992 supervisor calls in 64 seconds, roughly
	// 310 a second against a launcher that is also trying to start an engine.
	// It buried the engine's own logs, which is what made a real fan-out failure
	// hard to read.
	//
	// A floor bounds that without giving up the latency the trigger buys: a
	// bridge still starts within MinGap of the decision, which at 250ms is
	// invisible next to a wake measured in hundreds of milliseconds.
	//
	// Zero takes the default. Ticks are NOT gated by this -- Interval is already
	// a floor of its own, and gating them would silently double it.
	MinGap time.Duration
}

// defaultMinGap is the floor between trigger-driven passes. See Reconciler.MinGap.
const defaultMinGap = 250 * time.Millisecond

// New returns a Reconciler ready to Start.
func New(p pool.Pool, demand DemandSource, cfg policy.Config) *Reconciler {
	return &Reconciler{
		Pool:           p,
		Demand:         demand,
		Config:         cfg,
		Interval:       5 * time.Second,
		MinGap:         defaultMinGap,
		ObserveTimeout: defaultObserveTimeout,
		ActTimeout:     defaultActTimeout,
		AdmitTimeout:   defaultAdmitTimeout,

		borrowedAt:       map[policy.Borrow]time.Time{},
		missesAt:         map[string][]time.Time{},
		admitting:        map[string]bool{},
		admittingPods:    map[types.NamespacedName]bool{},
		lastShort:        map[string]int{},
		lastSummary:      map[string]string{},
		lastUnassignable: map[string]string{},
		lastDeclined:     map[declineKey]bool{},
		lastHeld:         map[string]string{},
		now:              time.Now,
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

	gap := r.MinGap
	if gap <= 0 {
		gap = defaultMinGap
	}

	for {
		started := r.now()
		if _, err := r.Once(ctx); err != nil {
			log.FromContext(ctx).V(1).Info("warm pool reconcile failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-decided:
			// A decision landed: go round rather than waiting out the tick.
			// This is the whole reason a bridge can be up before the ordinary
			// replicas have finished being asked for.
			//
			// Held to MinGap, though. The trigger is ready far more often than
			// there is new work, and without a floor the loop spins as fast as a
			// pass completes -- see MinGap for what that measured.
			if rest := gap - r.now().Sub(started); rest > 0 {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(rest):
				}
			}
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
	// What the pools hold, for the quota to charge WVA for. Published on every
	// pass INCLUDING an empty one: a pool that shrank or was deleted has to stop
	// being charged, and only a fresh figure says so.
	//
	// Not published when observation FAILED -- that returns above -- because an
	// empty reading there means "could not see", and zero would hand the quota
	// its allowance back at the moment WVA cannot tell what it is holding.
	decision.PublishWarmPoolGPUs(r.Namespace, warmPoolGPUsByAccelerator(memberships))

	pools, err := r.poolSpecs(ctx)
	if err != nil {
		return policy.Plan{}, err
	}
	r.reportUndeclaredPools(ctx, pools)
	if orphans := Orphaned(memberships, pools); len(orphans) > 0 {
		// A Pod labelled for a pool nothing declares holds a GPU that no pool
		// will ever lend. Silence here reads as a pool that is simply small.
		log.FromContext(ctx).WithName("warmpool").Info(
			"warm pool Pods belong to no declared pool and are holding GPUs unusably; "+
				"check their llm-d.ai/warm-pool label against the pool Deployments",
			"pods", orphans)
	}

	// One decision per pool. The per-variant state below -- borrows, misses,
	// admissions in flight -- is keyed by Pod and variant, both already unique
	// across pools, so it needs no splitting.
	// A variant that selected no resolvable pool gets no warm copy. Said once
	// per variant, because the alternative -- a variant that scales normally and
	// silently never runs warm -- is indistinguishable from a pool too small to
	// hold it.
	for variant, why := range Unassignable(variants, pools) {
		if r.lastUnassignable[variant] == why {
			continue
		}
		if r.lastUnassignable == nil {
			r.lastUnassignable = map[string]string{}
		}
		r.lastUnassignable[variant] = why
		log.FromContext(ctx).WithName("warmpool").Info(
			"variant will get no warm copy", "variant", variant, "reason", why)
	}

	var merged policy.Plan
	for _, spec := range pools {
		mine := MembershipsIn(memberships, spec.Name)
		theirs := VariantsFor(variants, spec, pools)

		r.mu.Lock()
		in := policy.Input{
			Memberships: mine,
			Variants:    theirs,
			BorrowedAt:  copyBorrows(r.borrowedAt),
			WantAwake:   r.wantAwake(spec),
			MissesAt:    copyMisses(r.missesAt),
			Admitting:   maps.Clone(r.admittingPods),
			Now:         r.now(),
		}
		r.mu.Unlock()

		plan := policy.Decide(in, spec.Config)
		free := pool.FreePods(mine)
		metrics.SetWarmPoolFreePods(r.metricName(spec), free)
		// This pool's own bridges, not the process-wide total. r.Lent() spans
		// every pool, and feeding it to a per-pool size tells each pool to
		// carry every other pool's borrows.
		lent := lentPods(mine)
		r.report(ctx, spec, mine, theirs, free, lent)
		r.apply(ctx, spec, plan)
		r.publishSize(ctx, spec, mine, lent)
		merged = mergePlans(merged, plan)
	}
	r.forgetVanishedVariants(variants)
	return merged, nil
}

// forgetVanishedVariants drops per-variant bookkeeping for variants that are no
// longer being called about.
//
// Two reasons, and the second is the one that matters. Discovery is call-driven
// and the registry ages entries out, so variants come and go over a controller's
// life; maps keyed by variant name would otherwise only ever grow.
//
// More importantly, the report-once maps would keep a variant's silence FOREVER.
// A model declined for a reason, removed, and redeployed with the same problem
// would never be reported again -- the operator investigating it would find
// nothing in the log, which is precisely the silence these maps were added to
// break. Forgetting a variant that went away means its return is news again.
//
// missesAt is pruned on the same pass but by AGE, not by liveness: a variant
// that flickers out and back should not have to earn its admission twice.
func (r *Reconciler) forgetVanishedVariants(variants []policy.VariantDemand) {
	live := make(map[string]bool, len(variants))
	for _, v := range variants {
		live[v.Model.Variant] = true
	}

	for variant := range r.lastUnassignable {
		if !live[variant] {
			delete(r.lastUnassignable, variant)
		}
	}
	for key := range r.lastDeclined {
		if !live[key.Variant] {
			delete(r.lastDeclined, key)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := r.now().Add(-orDefaultWindow(r.Config.AdmissionWindow))
	for variant, times := range r.missesAt {
		newest := time.Time{}
		for _, at := range times {
			if at.After(newest) {
				newest = at
			}
		}
		if newest.Before(cutoff) {
			delete(r.missesAt, variant)
		}
	}
}

// orDefaultWindow keeps a zero AdmissionWindow from pruning every miss the
// instant it is recorded.
func orDefaultWindow(window time.Duration) time.Duration {
	if window <= 0 {
		return time.Hour
	}
	return window
}

// poolSpecs is the declared pools, or the single unnamed one when nothing
// declares any.

// warmPoolGPUsByAccelerator sums the devices this namespace's pool Pods hold.
//
// PER POD, counted once. Memberships are per model per Pod, so a Pod holding
// three warm models appears three times and summing memberships would charge its
// GPUs three times over -- a pool would then read as larger the more useful it
// was, and the quota would bind on models it was successfully sharing.
//
// Capacity.GPUs is already the whole warm unit for a group: capacityOf
// multiplies the leader's devices by the group size, so a two-Pod group is
// charged once for both Pods' GPUs, and the worker Pods contribute nothing
// further because they are not members.
func warmPoolGPUsByAccelerator(memberships []pool.Membership) map[string]int {
	seen := make(map[types.NamespacedName]bool, len(memberships))
	out := map[string]int{}
	for _, m := range memberships {
		if seen[m.Pod] {
			continue
		}
		seen[m.Pod] = true
		if m.Capacity.GPUs <= 0 {
			continue
		}
		accelerator := m.Capacity.Accelerator
		if accelerator == "" {
			// Charged under a name that says why it cannot be attributed, rather
			// than dropped: a pool on an unlabelled node still holds its GPUs,
			// and silently omitting them is how a quota over-grants.
			accelerator = "unknown"
		}
		out[accelerator] += m.Capacity.GPUs
	}
	return out
}

func (r *Reconciler) poolSpecs(ctx context.Context) ([]PoolSpec, error) {
	if r.Pools == nil {
		return []PoolSpec{{Name: "", Config: r.Config}}, nil
	}
	return r.Pools.Pools(ctx)
}

// metricName keeps the pool label stable for the single unnamed pool, which
// reported under the namespace before pools had names. Renaming a live series
// loses its history for no gain.
func (r *Reconciler) metricName(spec PoolSpec) string {
	if spec.Name == "" {
		return r.Name
	}
	return spec.Name
}

// mergePlans concatenates two pools' plans so one pass still reports one Plan.
func mergePlans(a, b policy.Plan) policy.Plan {
	a.Return = append(a.Return, b.Return...)
	a.Borrow = append(a.Borrow, b.Borrow...)
	a.Admit = append(a.Admit, b.Admit...)
	a.Evict = append(a.Evict, b.Evict...)
	a.Blocked = append(a.Blocked, b.Blocked...)
	a.Missed = append(a.Missed, b.Missed...)
	a.Declined = append(a.Declined, b.Declined...)
	a.GrowBy += b.GrowBy
	return a
}

// acceleratorsIn summarises the GPU models a pool's Pods sit on.
func acceleratorsIn(memberships []pool.Membership) string {
	seen := map[string]bool{}
	var models []string
	for _, m := range memberships {
		name := m.Capacity.Accelerator
		if name == "" {
			name = "unknown"
		}
		if !seen[name] {
			seen[name] = true
			models = append(models, name)
		}
	}
	if len(models) == 0 {
		return "none"
	}
	sort.Strings(models)
	return strings.Join(models, ",")
}

// publishSize records how big this pool should be, for KEDA to act on.
//
// WVA never writes the pool's replica count itself. The pool has its own
// ScaledObject, so it is scaled by the same path as every model -- WVA decides,
// KEDA actuates -- and the controller keeps the read-only ClusterRole that
// makes it deployable in clusters whose admins will not grant a resize licence.
//
// Published on every pass, unconditionally. It is a level, not an edge: KEDA
// polls for it, and a size withheld because "nothing changed" would read as no
// decision at all the first time KEDA asked after a restart.
func (r *Reconciler) publishSize(ctx context.Context, spec PoolSpec, memberships []pool.Membership, lent int) {
	if r.PublishSize == nil || spec.Deployment == "" {
		return
	}
	want := SizeFor(spec.Config.SleepMinSize, lent)

	// Yield to model replicas. While a variant of this pool's accelerator is
	// being denied GPUs, the pool stops asking for more -- see HoldAt.
	if hold := HoldAt(spec.Replicas); r.Contended != nil && want > hold {
		if accelerator := soleAccelerator(memberships); accelerator != "" &&
			r.Contended(r.Namespace, accelerator) {
			r.reportHeld(ctx, spec, accelerator, want, hold)
			want = hold
		}
	}

	// DO NOT ASK FOR WHAT THE NAMESPACE CANNOT AFFORD.
	//
	// Growth here is a size published to KEDA, which creates Pods. Asking beyond
	// the allowance does not queue anything useful: the Pods are created and sit
	// Pending for want of a GPU, or a quota admission refuses them outright, and
	// the pool reports itself short forever while nothing can ever fill it. A
	// pool that stops at what it can have is the honest state, and the shortfall
	// is already reported by reportShort.
	//
	// Distinct from the contention hold above. That yields ground while a model
	// replica is actively being denied; this refuses to ask when the allowance is
	// spent. A namespace can be uncontended and still have nothing left.
	if r.Headroom != nil && want > spec.Replicas {
		if accelerator := soleAccelerator(memberships); accelerator != "" {
			if free, known := r.Headroom(r.Namespace, accelerator); known {
				// Headroom is in GPUs; the pool grows in Pods. A Pod costs the
				// devices one warm unit holds, and a group of two 8-GPU Pods
				// costs sixteen -- capacityOf already reports the unit's total.
				perPod := gpusPerUnit(memberships)
				if perPod > 0 {
					affordable := spec.Replicas + free/perPod
					if want > affordable {
						r.reportCapped(ctx, spec, accelerator, want, affordable, free)
						want = affordable
					}
				}
			}
		}
	}
	r.PublishSize(r.Namespace, spec.Deployment, int32(want)) //nolint:gosec // small counts
}

// gpusPerUnit is what one more Pod of this pool would cost in devices.
//
// Read from the Pods themselves rather than configured: the pool's own Deployment
// states it, and a second copy of that fact could only ever agree or silently
// disagree. Zero when no Pod declares a device count, which switches the capacity
// check off rather than dividing by it.
func gpusPerUnit(memberships []pool.Membership) int {
	for _, m := range memberships {
		if m.Capacity.GPUs > 0 {
			return m.Capacity.GPUs
		}
	}
	return 0
}

// reportCapped says the pool wanted to grow and the namespace could not afford
// it. Deduplicated like every other pool-state line: this is a STATE that lasts
// as long as the allowance is spent, and an undeduplicated line would repeat on
// every pass for exactly as long as somebody is reading the log to find out why
// the pool is not growing.
func (r *Reconciler) reportCapped(ctx context.Context, spec PoolSpec, accelerator string, want, capped, free int) {
	name := r.metricName(spec)
	summary := fmt.Sprintf("%s want=%d capped=%d free=%d", accelerator, want, capped, free)
	if r.lastCapped == nil {
		r.lastCapped = map[string]string{}
	}
	if r.lastCapped[name] == summary {
		return
	}
	r.lastCapped[name] = summary
	log.FromContext(ctx).WithName("warmpool").Info(
		"warm pool is not growing: the namespace has no GPU allowance left for it. "+
			"Pods asked for beyond this would stay Pending or be refused, so the pool "+
			"stops at what it can have",
		"pool", name, "accelerator", accelerator,
		"wanted", want, "cappedAt", capped, "gpusFree", free)
}

// soleAccelerator is the accelerator every Pod in this pool sits on, or "" if
// they differ or none is known.
//
// A pool spanning two accelerator types is a misconfiguration rather than a
// case to reason about -- a warm copy is only reusable on the GPU it was loaded
// on -- so rather than guess which type the contention applies to, this declines
// to answer and the pool grows as it otherwise would.
func soleAccelerator(memberships []pool.Membership) string {
	found := ""
	for _, m := range memberships {
		if m.Capacity.Accelerator == "" {
			continue
		}
		if found != "" && found != m.Capacity.Accelerator {
			return ""
		}
		found = m.Capacity.Accelerator
	}
	return found
}

// reportHeld says why a pool that wanted to grow did not, once per pool per
// reason. Silence here would read as a pool that had all the Pods it wanted.
func (r *Reconciler) reportHeld(ctx context.Context, spec PoolSpec, accelerator string, want, held int) {
	name := r.metricName(spec)
	summary := fmt.Sprintf("%s want=%d held=%d", accelerator, want, held)
	if r.lastHeld[name] == summary {
		return
	}
	if r.lastHeld == nil {
		r.lastHeld = map[string]string{}
	}
	r.lastHeld[name] = summary
	log.FromContext(ctx).WithName("warmpool").Info(
		"warm pool is not growing: a model replica on this accelerator is being denied GPUs. "+
			"The pool keeps what it holds and stops competing for more",
		"pool", name, "accelerator", accelerator, "wanted", want, "holdingAt", held)
}

// lentPods counts THIS pool's Pods that are serving a bridge.
//
// Distinct Pods, not memberships: a Pod holding several models is one Pod, and
// the size a pool needs is measured in Pods.
//
// Scoped to the pool on purpose. r.Lent() is the process-wide record and is the
// right answer for the exported diagnostic, but using it per pool made every
// pool's published size include every other pool's borrows -- so a quiet pool
// beside a busy one was told to hold Pods it had no use for, permanently, on an
// accelerator the busy pool cannot even run on.
func lentPods(memberships []pool.Membership) int {
	pods := map[types.NamespacedName]bool{}
	for _, m := range memberships {
		if m.State == pool.Serving {
			pods[m.Pod] = true
		}
	}
	return len(pods)
}

// declineKey identifies one reported decline. A pair rather than a joined
// string, so no separator can collide with a reason that contains it.
type declineKey struct {
	Variant string
	Reason  string
}

// residentModels counts the models actually held, across a pool's Pods.
//
// NOT len(memberships): an EMPTY Pod contributes one placeholder membership so
// that idle Pods are visible as reserve, which means a two-Pod pool holding
// nothing at all would report "resident=2". An operator reading that as "two
// models warm" would conclude the pool was working when it had never warmed
// anything.
func residentModels(memberships []pool.Membership) int {
	n := 0
	for _, inPod := range pool.ByPod(memberships) {
		n += pool.Resident(inPod)
	}
	return n
}

// reportUndeclaredPools names pool Deployments that no ScaledObject declares.
//
// Said once per distinct set: a standing orphan repeated every pass is noise, and
// noise is how a real one gets missed.
func (r *Reconciler) reportUndeclaredPools(ctx context.Context, pools []PoolSpec) {
	if r.Undeclared == nil {
		return
	}
	orphans, err := r.Undeclared(ctx, pools)
	if err != nil {
		log.FromContext(ctx).WithName("warmpool").V(1).Info(
			"could not check for undeclared warm pool Deployments", "err", err.Error())
		return
	}
	summary := strings.Join(orphans, ",")
	if summary == r.lastUndeclared {
		return
	}
	r.lastUndeclared = summary
	if len(orphans) == 0 {
		return
	}
	log.FromContext(ctx).WithName("warmpool").Info(
		"warm pool Deployments are holding accelerators but no ScaledObject declares them; "+
			"WVA will not use them. Give each a trigger carrying warmPoolName, or delete them",
		"deployments", orphans)
}

// report logs what the pass saw, but only when it differs from the last one.
//
// Everything else in this file logs on FAILURE only, which means a pool that is
// working and a pool that is dead produce identical output: nothing. That is the
// wrong default for a component that holds GPUs continuously -- the first
// question anyone asks is "is it doing anything", and the honest answer has to
// come from somewhere. Measured on a live cluster: a full borrow, bridge and
// return cycle -- the pool doing precisely its job -- emitted not one line.
//
// Deduplicated rather than rate-limited so a steady pool is quiet and a change
// is visible immediately, which is the opposite of what a periodic dump gives.
func (r *Reconciler) report(ctx context.Context, spec PoolSpec, memberships []pool.Membership, variants []policy.VariantDemand, free, lent int) {
	logger := log.FromContext(ctx).WithName("warmpool")
	if r.lastSummary == nil {
		// A Reconciler built as a struct literal rather than through New has no
		// map here, and writing to a nil one panics. An unset field is a
		// mistake, not a request to crash the pass.
		r.lastSummary = map[string]string{}
	}
	name := r.metricName(spec)
	pods := len(pool.ByPod(memberships))
	// The accelerator is in the summary because its ABSENCE is the interesting
	// case: an install that cannot read nodes matches nothing, and the fit check
	// then treats every model as portable. That is the right failure direction,
	// but a silent one, and this line is deduplicated so saying it costs nothing.
	summary := fmt.Sprintf("pods=%d free=%d resident=%d variants=%d lent=%d accelerator=%s",
		pods, free, residentModels(memberships), len(variants), lent, acceleratorsIn(memberships))
	if summary != r.lastSummary[name] {
		logger.V(1).Info("warm pool state", "pool", name, "state", summary)
		r.lastSummary[name] = summary
	}

	// The reserve cannot fit inside the pool. Read from the Deployment, so it is
	// reported whether or not any model has asked for anything yet -- the check
	// below only fires once there is demand, which is exactly when it is too
	// late to be useful.
	why, inert := spec.Inert()
	if inert {
		logger.Info("warm pool is configured so that it can never admit a model",
			"pool", name, "reason", why)
	}

	// A pool whose every Pod is reserve can never admit anything: the budget in
	// policy.Decide is free-minus-reserve, so at equality it is zero forever and
	// nothing is ever warmed. It is not an error -- the reserve is doing exactly
	// what it was told -- but it is indistinguishable from a working pool from
	// the outside, and it is the state the obvious first deployment lands in:
	// one Pod, the default reserve of one. Proven on a cluster by adding a
	// second Pod, after which admission fired on the next pass.
	// Only when the Deployment could not answer it. Both checks describe the
	// same condition from different evidence -- Inert reads the declared replica
	// count, this one counts Pods that exist -- and saying it twice reads as two
	// faults. Inert is preferred where it applies because it names the
	// annotation to change; this one is the answer for a pool with no Deployment
	// to read, where Replicas is unknown.
	if !inert && pods > 0 && pods <= spec.Config.SleepMinSize && len(variants) > 0 {
		logger.Info("warm pool cannot admit any model: every Pod is reserve. "+
			"Add Pods or lower the reserve, or the pool will hold GPUs and never warm anything",
			"pool", name, "pods", pods, "sleepMinSize", spec.Config.SleepMinSize)
	}
}

// apply carries out a plan. Returns come first, as the policy decided them:
// a Pod handed back is reserve for every model, including the borrows below.
func (r *Reconciler) apply(ctx context.Context, spec PoolSpec, plan policy.Plan) {
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
		if !r.beginAdmission(action.Model.Variant, action.Pod) {
			continue
		}
		go func(action policy.Action) {
			defer r.endAdmission(action.Model.Variant, action.Pod)
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

	// A DECLINE is the pool refusing a model outright: it needs more GPUs than a
	// Pod holds, or an accelerator this pool is not on, or its size cannot be
	// worked out. Nothing consumed this, so the whole class was silent -- the
	// variant scaled normally and simply never ran warm, which from outside is
	// indistinguishable from a pool that is merely too small. That is the exact
	// failure this configuration work exists to remove, reintroduced by the code
	// that reports everything else.
	//
	// Deduplicated per variant+reason, because a standing mismatch is decided
	// again on every pass and would otherwise be the only thing in the log.
	for _, declined := range plan.Declined {
		if !declined.Permanent {
			// TRANSIENT: the Pod is full right now. Not a fault and not
			// actionable as configuration -- it resolves when something is
			// evicted or the pool grows, and the pressure it represents is
			// already visible as blocked borrows and the reserve shortfall.
			// Reported quietly so it can be found when investigating, and not
			// at a level that buries the permanent ones.
			logger.V(1).Info("warm pool has no room for this model right now",
				"variant", declined.Model.Variant, "reason", declined.Reason)
			continue
		}
		// PERMANENT: this pool can never hold this model -- wrong accelerator,
		// more GPUs than a Pod has, a size that cannot be read. Nothing in the
		// pool will change it, so it is said once and it names what to fix.
		key := declineKey{Variant: declined.Model.Variant, Reason: declined.Reason}
		if r.lastDeclined[key] {
			continue
		}
		if r.lastDeclined == nil {
			r.lastDeclined = map[declineKey]bool{}
		}
		r.lastDeclined[key] = true
		logger.Info("warm pool will never warm this model; it is pointed at a pool that cannot hold it",
			"variant", declined.Model.Variant, "namespace", declined.Model.Namespace,
			"reason", declined.Reason)
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
	// Reported, not acted on: growing costs a model load per Pod, and a
	// shortfall that lasts one cycle is a borrow doing its job.
	//
	// Deduplicated per pool, like every other report here. Being below the
	// reserve is a STATE, not an event: an undeduplicated line repeats on every
	// pass for as long as the shortfall lasts, which is precisely as long as
	// someone is trying to read the log to find out why. Observed burying every
	// other message in the controller's output, including the pool state line
	// the condition should be diagnosed from.
	if r.lastShort == nil {
		r.lastShort = map[string]int{}
	}
	if plan.GrowBy != r.lastShort[spec.Name] {
		r.lastShort[spec.Name] = plan.GrowBy
		if plan.GrowBy > 0 {
			logger.V(2).Info("warm pool is below its reserve",
				"pool", spec.Name, "short", plan.GrowBy)
		}
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
func (r *Reconciler) beginAdmission(variant string, pod types.NamespacedName) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.admitting[variant] || r.admittingPods[pod] {
		return false
	}
	r.admitting[variant] = true
	r.admittingPods[pod] = true
	return true
}

func (r *Reconciler) endAdmission(variant string, pod types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.admitting, variant)
	delete(r.admittingPods, pod)
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

// wantAwake asks the optimizer which model should hold this pool's GPUs.
//
// Empty whenever nothing is wired or nothing has been said, which is the
// ordinary case: the pool then decides from demand as it always has.
func (r *Reconciler) wantAwake(spec PoolSpec) string {
	if r.WantAwake == nil {
		return ""
	}
	variant, ok := r.WantAwake(r.Namespace, r.metricName(spec))
	if !ok {
		return ""
	}
	return variant
}
