package gpuusage

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
)

// fakeDiscovery returns a scripted observation, or an error once armed.
// fakeDiscovery is read from the test goroutine while the Refresher calls it
// from its own, so its counter is atomic. Ordinary ints raced under -race:
// Eventually/Consistently poll from the ginkgo goroutine while Start() is
// running, which is the whole point of those assertions.
type fakeDiscovery struct {
	byType      map[string]int
	byNamespace map[string]map[string]int
	err         error
	calls       atomic.Int64
}

// Calls reports how many observations have been made.
func (f *fakeDiscovery) Calls() int { return int(f.calls.Load()) }

func (f *fakeDiscovery) DiscoverUsageByNamespace(context.Context) (map[string]int, map[string]map[string]int, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.byType, f.byNamespace, nil
}

var _ = Describe("Refresher", func() {
	var (
		ctx   context.Context
		store *decision.GPUUsageStore
		disc  *fakeDiscovery
		r     *Refresher
	)

	BeforeEach(func() {
		ctx = context.Background()
		store = decision.NewGPUUsageStore()
		disc = &fakeDiscovery{}
		r = &Refresher{Store: store, Discovery: disc}
	})

	Describe("Refresh", func() {
		It("publishes the cluster and per-namespace views together", func() {
			disc.byType = map[string]int{"A100": 6}
			disc.byNamespace = map[string]map[string]int{"chat": {"A100": 4}, "batch": {"A100": 2}}

			Expect(r.Refresh(ctx)).To(Succeed())

			snap, ok := store.Get()
			Expect(ok).To(BeTrue())
			Expect(snap.ByType).To(HaveKeyWithValue("A100", 6))
			Expect(snap.ByNamespace).To(HaveKeyWithValue("chat", HaveKeyWithValue("A100", 4)))
			Expect(snap.ByNamespace).To(HaveKeyWithValue("batch", HaveKeyWithValue("A100", 2)))
		})

		It("keeps the last observation when a look fails", func() {
			// Consumers treat an ABSENT snapshot as unknown and bound how stale a
			// present one may be, so keeping the previous reading degrades safely.
			// Publishing zeros would not degrade at all — it would be BELIEVED, and
			// a capacity check would report the cluster empty at exactly the moment
			// WVA stopped being able to see it.
			disc.byType = map[string]int{"A100": 8}
			Expect(r.Refresh(ctx)).To(Succeed())

			disc.err = errors.New("the node API is unreachable")
			Expect(r.Refresh(ctx)).ToNot(Succeed(), "a failed observation must reach the caller")

			snap, ok := store.Get()
			Expect(ok).To(BeTrue(), "the previous observation was dropped")
			Expect(snap.ByType).To(HaveKeyWithValue("A100", 8), "zeros must not replace the last good reading")
		})
	})

	Describe("Start", func() {
		It("observes before the first tick", func() {
			// An interval of dead time at startup is an interval in which every
			// scaling decision is taken with no capacity evidence — the defect this
			// package was added to fix. The hour-long interval means a pass proves
			// the observation was not driven by the ticker.
			disc.byType = map[string]int{"H100": 2}
			r.Interval = time.Hour

			runCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- r.Start(runCtx) }()
			defer cancel()

			Eventually(func() bool {
				_, ok := store.Get()
				return ok
			}, 2*time.Second, 5*time.Millisecond).Should(BeTrue(), "nothing published before the first tick")

			cancel()
			Eventually(done).Should(Receive(BeNil()))
		})

		It("takes no observation at all when nothing consumes the physical view", func() {
			// A quota is charged for WVA's own variants and never reads this. Left
			// ungated, such a deployment lists nodes and walks every pod in the
			// cluster on every interval to produce a number nothing reads — and
			// needs the Node RBAC to do it.
			r.Interval = 5 * time.Millisecond
			r.Periodic = func() bool { return false }

			runCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- r.Start(runCtx) }()

			Consistently(func() int { return disc.Calls() }, 200*time.Millisecond, 10*time.Millisecond).
				Should(Equal(0), "the cluster was observed for a view nothing consumes")

			cancel()
			Eventually(done).Should(Receive(BeNil()))
		})

		It("follows the predicate without a restart when the configuration changes", func() {
			// Limiter mode is live-reloadable, so the answer is re-read every tick
			// rather than latched at startup.
			var wanted atomic.Bool
			r.Interval = 5 * time.Millisecond
			r.Periodic = wanted.Load

			runCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- r.Start(runCtx) }()
			defer cancel()

			Consistently(func() int { return disc.Calls() }, 100*time.Millisecond, 10*time.Millisecond).
				Should(Equal(0))

			wanted.Store(true)
			Eventually(func() int { return disc.Calls() }, 2*time.Second, 5*time.Millisecond).
				Should(BeNumerically(">", 0), "a limiter mode change must not need a restart")

			cancel()
			Eventually(done).Should(Receive(BeNil()))
		})

		It("survives a failed first observation", func() {
			// The cluster may not be reachable when the process starts; giving up
			// there would leave WVA with no capacity picture for its whole life.
			disc.err = errors.New("not ready")
			r.Interval = time.Hour

			runCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- r.Start(runCtx) }()

			Eventually(func() int { return disc.Calls() }, time.Second, 5*time.Millisecond).
				Should(BeNumerically(">", 0), "Start never attempted an observation")

			cancel()
			Eventually(done).Should(Receive(BeNil()), "a failed first observation must not be fatal")
		})
	})

	Describe("EnsureFresh", func() {
		It("observes even when the timer is off", func() {
			// This is what keeps the scale-from-zero capacity check working in a
			// deployment that does not run the timer: the engine asks at the moment
			// it decides, and it only asks when a provider that reads this view is
			// actually configured. A predicate saying "nobody needs it periodically"
			// must not override a caller that has established it needs it now.
			r.Periodic = func() bool { return false }
			disc.byType = map[string]int{"A100": 3}

			r.EnsureFresh(ctx, 0)

			snap, ok := store.Get()
			Expect(ok).To(BeTrue(), "an on-demand ask was refused because the timer is off")
			Expect(snap.ByType).To(HaveKeyWithValue("A100", 3))
		})

		It("observes again when the published view is stale", func() {
			// The reason this exists: a placement is decided the instant demand
			// appears, and the periodic observation can be a whole interval old by
			// then. The scale-from-zero e2e failed exactly here — a workload became
			// Ready, a wake was considered a second later, and the budget came from
			// an observation taken before that workload existed, so it was waved
			// onto an accelerator that was already full.
			disc.byType = map[string]int{"A100": 0}
			Expect(r.Refresh(ctx)).To(Succeed())

			disc.byType = map[string]int{"A100": 4} // a workload starts and takes the pool
			r.EnsureFresh(ctx, 0)

			snap, _ := store.Get()
			Expect(snap.ByType).To(HaveKeyWithValue("A100", 4),
				"the decision would have been taken against a cluster that no longer exists")
		})

		It("reuses an observation that is still current", func() {
			// The caller runs at 10Hz; re-walking the cache every tick would buy
			// nothing.
			disc.byType = map[string]int{"A100": 1}
			Expect(r.Refresh(ctx)).To(Succeed())
			before := disc.Calls()

			for range 20 {
				r.EnsureFresh(ctx, time.Minute)
			}
			Expect(disc.Calls()).To(Equal(before), "a current observation must be reused")
		})

		It("keeps the previous observation when the look fails", func() {
			// A blip must not become a refusal to wake.
			disc.byType = map[string]int{"A100": 2}
			Expect(r.Refresh(ctx)).To(Succeed())

			disc.err = errors.New("transient")
			r.EnsureFresh(ctx, 0)

			snap, ok := store.Get()
			Expect(ok).To(BeTrue())
			Expect(snap.ByType).To(HaveKeyWithValue("A100", 2))
		})

		It("is a no-op on a nil Refresher", func() {
			// The engine field is optional; a nil receiver must not panic the 10Hz
			// loop.
			var nilRefresher *Refresher
			Expect(func() { nilRefresher.EnsureFresh(ctx, 0) }).ToNot(Panic())
		})
	})
})
