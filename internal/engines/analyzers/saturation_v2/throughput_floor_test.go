package saturation_v2

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// Figures from the shape-swap P/D run (biran-20260915-102548-571), H200,
// Qwen3-0.6B, 6 req/s throughout. In its first phase (6000 in / 1000 out) one
// decode replica saturated at ~5.4 completions/s with 1.15M tokens resident;
// at six replicas the same load occupied ~100k tokens in total and the
// controller sized the fleet to one, which then re-saturated within a minute.
const (
	runLambda     = 6.0
	runMu         = 5.4
	runK1         = int64(929_792) // 1,162,240 x 0.8
	runKvCapacity = int64(1_162_240)

	decodeVariant = "decode-v"
)

var _ = Describe("estimateThroughputDemand", func() {
	variants := func(role string, ready int) []domain.VariantCapacity {
		return []domain.VariantCapacity{{
			VariantName: "v", Role: role, ReplicaCount: ready, PerReplicaCapacity: float64(runK1),
		}}
	}
	// Readings from the variant's own bucket with enough samples to order;
	// the specs on holding build their own.
	replicas := func(n int) []ReplicaCapacity {
		out := make([]ReplicaCapacity, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, ReplicaCapacity{VariantName: "v", SaturatedThroughput: runMu,
				SaturatedThroughputSamples: MinThroughputSamplesToOrder})
		}
		return out
	}

	It("sizes the fleet to lambda over mu, in the variant's own tokens", func() {
		// 6 / 5.4 = 1.11 replicas' worth of demand. Through the engine's
		// RC = D / 0.85 - supply that is 1.31 replicas, so a two-replica fleet
		// holds and a one-replica fleet is (correctly) short.
		f := estimateThroughputDemand(runLambda, replicas(6), variants(domain.RoleDecode, 6), nil, BacklogDrainSeconds, 0.85)
		Expect(f.ByRole).To(HaveKey(domain.RoleDecode))
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", runLambda/runMu*float64(runK1), 1e-6))
		Expect(f.Terms[domain.RoleDecode].Replicas).To(BeNumerically("~", 1.111, 1e-3))
		Expect(f.Terms[domain.RoleDecode].Backlog).To(BeZero())
	})

	It("does not move when replicas are added or removed", func() {
		// The property the arrival floor was supposed to have and did not:
		// mu is a per-replica constant, so the floor is the same at one
		// replica as at six.
		one := estimateThroughputDemand(runLambda, replicas(1), variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0.85)
		six := estimateThroughputDemand(runLambda, replicas(6), variants(domain.RoleDecode, 6), nil, BacklogDrainSeconds, 0.85)
		Expect(six.ByRole[domain.RoleDecode]).To(BeNumerically("~", one.ByRole[domain.RoleDecode], 1e-6))
	})

	It("asks for the replicas the load needs, whether the fleet has them or not", func() {
		// One replica, lambda / mu = 1.11: through the engine's RC = D / 0.85
		// - supply that orders the second replica in the first cycle lambda
		// is measured. The first version capped this at the fleet's size and
		// the order came 50 s later, from occupancy, after the replica had
		// tipped into preemption (file header).
		f := estimateThroughputDemand(runLambda, replicas(1), variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0.85)
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", runLambda/runMu*float64(runK1), 1e-6))
		Expect(f.ByRole[domain.RoleDecode]/0.85).To(BeNumerically(">", float64(runK1)),
			"RC = D / scaleUp - one replica's supply is positive: the second replica is ordered")

		By("and not one more as replicas arrive: the figure is the load's, not the fleet's")
		g := estimateThroughputDemand(runLambda, replicas(2), variants(domain.RoleDecode, 2), nil, BacklogDrainSeconds, 0.85)
		Expect(g.ByRole[domain.RoleDecode]).To(BeNumerically("~", f.ByRole[domain.RoleDecode], 1e-6))
		Expect(g.ByRole[domain.RoleDecode]/0.85).To(BeNumerically("<", 2*float64(runK1)),
			"at two replicas RC is negative: nothing more is ordered")
	})

	It("holds but does not order on a borrowed reading", func() {
		// A mu borrowed from a neighbouring bucket is wrong in a known
		// direction; from a longer shape it is too low and would over-order.
		// The shape-swap benchmark's phase 2 starts exactly so: the 4000-token
		// shape reads the 1000-token shape's mu until it has its own. Borrowed
		// readings hold the fleet at its size and no more.
		borrowed := ReplicaCapacity{VariantName: "v", SaturatedThroughput: 2.67, SaturatedThroughputSamples: 10, SaturatedThroughputBorrowed: true}
		f := estimateThroughputDemand(runLambda, []ReplicaCapacity{borrowed}, variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0.85)
		Expect(f.Terms[domain.RoleDecode].Replicas).To(BeNumerically("~", runLambda/2.67, 1e-6), "the uncapped figure is reported")
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", 0.85*float64(runK1), 1e-6), "capped at scaleUp x one replica")
		Expect(f.Terms[domain.RoleDecode].Held).To(BeTrue())
		Expect(f.Terms[domain.RoleDecode].HeldWhy).To(Equal("borrowed"))

		By("ordering once one replica has a reading of its own")
		own := ReplicaCapacity{VariantName: "v", SaturatedThroughput: 2.67, SaturatedThroughputSamples: MinThroughputSamplesToOrder}
		g := estimateThroughputDemand(runLambda, []ReplicaCapacity{borrowed, own}, variants(domain.RoleDecode, 2), nil, BacklogDrainSeconds, 0.85)
		Expect(g.Terms[domain.RoleDecode].Held).To(BeFalse())
		Expect(g.ByRole[domain.RoleDecode]).To(BeNumerically("~", runLambda/2.67*float64(runK1), 1e-6))
	})

	It("orders at most one replica on a single reading", func() {
		// The first reading at a saturation under-reads (3.67 against a true
		// 7.13 on the run); an order on it over-provisions, and the
		// over-provisioned fleet never saturates again to correct it. One
		// replica bounds that, and keeps the early order the floor exists
		// for: the second reading is a rate window away, and a fleet held at
		// its size for that minute, its queues priced nowhere, landed the
		// cold ramp's next replica 15-75 s later than occupancy alone.
		one := []ReplicaCapacity{{VariantName: "v", SaturatedThroughput: runMu / 2, SaturatedThroughputSamples: 1}}
		f := estimateThroughputDemand(runLambda, one, variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0.85)
		Expect(f.Terms[domain.RoleDecode].Replicas).To(BeNumerically("~", 2.22, 0.01))
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", 0.85*2*float64(runK1), 1e-6),
			"2.22 replicas' worth capped at scaleUp x (the one running + one): RC is exactly one replica")
		Expect(f.Terms[domain.RoleDecode].HeldWhy).To(Equal("single-sample"))

		By("not capping a single reading that asks for one replica or less")
		mild := []ReplicaCapacity{{VariantName: "v", SaturatedThroughput: runMu, SaturatedThroughputSamples: 1}}
		m := estimateThroughputDemand(runLambda, mild, variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0.85)
		Expect(m.Terms[domain.RoleDecode].Held).To(BeFalse())
		Expect(m.ByRole[domain.RoleDecode]).To(BeNumerically("~", runLambda/runMu*float64(runK1), 1))

		By("ordering from the second reading on")
		two := []ReplicaCapacity{{VariantName: "v", SaturatedThroughput: runMu / 2, SaturatedThroughputSamples: 2}}
		g := estimateThroughputDemand(runLambda, two, variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0.85)
		Expect(g.Terms[domain.RoleDecode].Held).To(BeFalse())
		Expect(g.ByRole[domain.RoleDecode]).To(BeNumerically("~", 2.22*float64(runK1), 0.01*float64(runK1)))

		By("with no scale-up threshold there is nothing to cap against, and the figure stands")
		z := estimateThroughputDemand(runLambda, one, variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0)
		Expect(z.Terms[domain.RoleDecode].Held).To(BeFalse())
	})

	It("prices a backlog as arrivals to clear within the drain target", func() {
		// 350 queued requests on the run's first ramp. Charged as residency
		// they were 2.45M tokens -- five replicas' worth on top of the load.
		// As throughput: 350 / 60 s = 5.8 extra req/s, (6 + 5.8) / 5.4 = 2.19
		// replicas in all, the load included.
		backlog := map[string]float64{domain.RoleDecode: 350}
		f := estimateThroughputDemand(runLambda, replicas(1), variants(domain.RoleDecode, 1), backlog, 60, 0.85)
		Expect(f.Terms[domain.RoleDecode].Backlog).To(Equal(350.0))
		Expect(f.Terms[domain.RoleDecode].Replicas).To(BeNumerically("~", (runLambda+350.0/60)/runMu, 1e-6))
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", (runLambda+350.0/60)/runMu*float64(runK1), 1e-6))

		By("a longer drain target asks for less")
		g := estimateThroughputDemand(runLambda, replicas(1), variants(domain.RoleDecode, 1), backlog, 120, 0.85)
		Expect(g.ByRole[domain.RoleDecode]).To(BeNumerically("<", f.ByRole[domain.RoleDecode]))

		By("another role's backlog is not this role's")
		h := estimateThroughputDemand(runLambda, replicas(1), variants(domain.RoleDecode, 1),
			map[string]float64{domain.RolePrefill: 350}, 60, 0.85)
		Expect(h.Terms[domain.RoleDecode].Backlog).To(BeZero())

		By("a non-positive drain target disables the backlog term rather than dividing by it")
		z := estimateThroughputDemand(runLambda, replicas(1), variants(domain.RoleDecode, 1), backlog, 0, 0.85)
		Expect(z.Terms[domain.RoleDecode].Backlog).To(BeZero())
		Expect(z.ByRole[domain.RoleDecode]).To(BeNumerically("~", runLambda/runMu*float64(runK1), 1e-6))
	})

	It("has no opinion for a role that has never been seen saturated", func() {
		rcs := append(replicas(2), ReplicaCapacity{VariantName: "p", SaturatedThroughput: 0})
		vcs := append(variants(domain.RoleDecode, 2),
			domain.VariantCapacity{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: 1, PerReplicaCapacity: 919_449})
		f := estimateThroughputDemand(runLambda, rcs, vcs, nil, BacklogDrainSeconds, 0.85)
		Expect(f.ByRole).To(HaveKey(domain.RoleDecode))
		Expect(f.ByRole).NotTo(HaveKey(domain.RolePrefill))
	})

	It("leaves a bridge's throughput out", func() {
		// A warm-pool bridge runs its engine on different terms (lower
		// --gpu-memory-utilization, and it is going home); its rate is not
		// this variant's.
		rcs := []ReplicaCapacity{{VariantName: "v", SaturatedThroughput: 1, FromWarmPool: true}}
		f := estimateThroughputDemand(runLambda, rcs, variants(domain.RoleBoth, 0), nil, BacklogDrainSeconds, 0.85)
		Expect(f.ByRole).To(BeEmpty())
	})

	It("leaves out a replica whose variant has no per-replica capacity", func() {
		// A throughput reading can outlive the capacity it was recorded
		// beside -- the window persists while a variant's P reads zero for a
		// cycle. P / mu is then 0, and a zero cost in the median would drag
		// the role's floor toward nothing for the replicas that are priced.
		vcs := append(variants(domain.RoleDecode, 1),
			domain.VariantCapacity{VariantName: "unpriced", Role: domain.RoleDecode, ReplicaCount: 1, PerReplicaCapacity: 0})
		rcs := append(replicas(1), ReplicaCapacity{VariantName: "unpriced", SaturatedThroughput: runMu})
		f := estimateThroughputDemand(runLambda, rcs, vcs, nil, BacklogDrainSeconds, 0.85)
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", runLambda/runMu*float64(runK1), 1e-6),
			"the priced replica alone decides the floor")
	})

	It("says nothing without an arrival rate", func() {
		f := estimateThroughputDemand(0, replicas(2), variants(domain.RoleBoth, 2), nil, BacklogDrainSeconds, 0.85)
		Expect(f.ByRole).To(BeEmpty())
	})
})

var _ = Describe("the saturated-throughput window", func() {
	It("keeps the max, because a saturated completion rate can only under-read", func() {
		// The readings a saturated decode replica produced on the run, in
		// order: 5.4 (a full minute saturated), then 3.3 and 3.5 under KV
		// pressure with preemptions. The mean would be 4.07 and imply 1.47
		// replicas; the replica was demonstrably completing 5.4.
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		k := "m|H200|1|decode|long|q5"
		a.recordSaturatedThroughput(k, 5.4)
		a.recordSaturatedThroughput(k, 3.3)
		a.recordSaturatedThroughput(k, 3.5)
		mu, bucket := a.saturatedThroughputFor(k)
		Expect(mu).To(Equal(5.4))
		Expect(bucket).To(Equal("long"))
		mu, bucket = a.saturatedThroughputFor("other|H200|1|decode|long|q5")
		Expect(mu).To(BeZero())
		Expect(bucket).To(BeEmpty())
	})

	It("borrows the nearest output-length bucket's reading until it has its own", func() {
		// The shape-swap benchmark's second phase: 4000-token outputs land in
		// "xxlong", which has never been seen saturated, while "long" holds the
		// 1000-token shape's 4.8. Without the borrow the floor vanished on the
		// first cycle of the new shape and a three-replica fleet was sized to
		// one from 400k tokens of occupancy.
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		a.recordSaturatedThroughput("m|H200|1|decode|long|q5", 4.8)
		mu, bucket := a.saturatedThroughputFor("m|H200|1|decode|xxlong|q5")
		Expect(mu).To(Equal(4.8))
		Expect(bucket).To(Equal("long"), "the figure is borrowed, and the log has to say from where")

		By("preferring the nearer bucket, and the shorter on a tie")
		a.recordSaturatedThroughput("m|H200|1|decode|huge|q5", 1.0)
		mu, bucket = a.saturatedThroughputFor("m|H200|1|decode|xxlong|q5")
		Expect(bucket).To(Equal("huge"), "huge is one step away, long is two")
		Expect(mu).To(Equal(1.0))
		a.recordSaturatedThroughput("m|H200|1|decode|xlong|q5", 3.0)
		mu, bucket = a.saturatedThroughputFor("m|H200|1|decode|xxlong|q5")
		Expect(bucket).To(Equal("xlong"), "equally distant: the shorter shape's under-hold wins")
		Expect(mu).To(Equal(3.0))

		By("and the bucket's own reading takes over the moment it exists, unmasked")
		a.recordSaturatedThroughput("m|H200|1|decode|xxlong|q5", 2.75)
		mu, bucket = a.saturatedThroughputFor("m|H200|1|decode|xxlong|q5")
		Expect(mu).To(Equal(2.75))
		Expect(bucket).To(Equal("xxlong"))

		By("never crossing a role, accelerator or threshold boundary")
		mu, _ = a.saturatedThroughputFor("m|H200|1|prefill|xxlong|q5")
		Expect(mu).To(BeZero())
		mu, _ = a.saturatedThroughputFor("m|H100|1|decode|xxlong|q5")
		Expect(mu).To(BeZero())
		mu, _ = a.saturatedThroughputFor("m|H200|1|decode|xxlong|q100")
		Expect(mu).To(BeZero())
	})

	It("does not count the same sample twice", func() {
		// The run's rows: one decode replica's saturated sample re-read on
		// four consecutive cycles, 3.43 req/s each time. Four samples would
		// clear MinThroughputSamplesToOrder and let the floor order at
		// lambda / 3.43 = 1.75 replicas' worth on one under-read.
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		t0 := time.Date(2026, 9, 19, 7, 44, 41, 0, time.UTC)
		now := t0
		a.now = func() time.Time { return now }
		for i := 0; i < 4; i++ {
			now = t0.Add(time.Duration(i) * 15 * time.Second)
			a.recordSaturatedThroughput("k", 3.433333333333333)
		}
		Expect(a.saturatedThroughput["k"].Len()).To(Equal(1), "one reading, however often the row repeats")
		Expect(a.saturatedThroughputReading("k").samples).To(Equal(1))

		By("still counting as observed: a re-read row does not let the window go stale")
		a.saturatedThroughput["k"].lastUpdated = time.Now().Add(-2 * time.Hour)
		a.recordSaturatedThroughput("k", 3.433333333333333)
		Expect(a.saturatedThroughput["k"].Stale(time.Hour)).To(BeFalse())
		Expect(a.saturatedThroughput["k"].Len()).To(Equal(1))

		By("folding a higher reading inside the spacing into the sample: two replicas, one moment, the max")
		a.recordSaturatedThroughput("k", 3.6)
		Expect(a.saturatedThroughput["k"].Len()).To(Equal(1))
		Expect(a.saturatedThroughput["k"].Max()).To(Equal(3.6), "the sample is the max of its window")
		a.recordSaturatedThroughput("k", 3.5)
		Expect(a.saturatedThroughput["k"].Max()).To(Equal(3.6), "and a lower one leaves it")

		By("not counting the same value at the boundary: the same pair straddling it, or a repeat")
		now = t0.Add(ThroughputSampleSpacing)
		a.recordSaturatedThroughput("k", 3.6)
		Expect(a.saturatedThroughput["k"].Len()).To(Equal(1))

		By("counting a reading that differs and lands a rate window after the last counted one")
		a.recordSaturatedThroughput("k", 3.4333333333333336)
		Expect(a.saturatedThroughput["k"].Len()).To(Equal(2))
		Expect(a.saturatedThroughput["k"].Max()).To(Equal(3.6))

		By("and a value seen before, a window later, is a reading of its own: a different window")
		now = t0.Add(2 * ThroughputSampleSpacing)
		a.recordSaturatedThroughput("k", 3.6)
		Expect(a.saturatedThroughput["k"].Len()).To(Equal(3))
	})

	It("spaces its samples by the window the collector takes the rate over", func() {
		// The spacing is a sample-independence argument about the rate's
		// window; widen the query to [2m] without widening the spacing and
		// two readings a minute apart share half their samples again.
		window, err := time.ParseDuration(registration.RequestRateWindow)
		Expect(err).NotTo(HaveOccurred())
		Expect(ThroughputSampleSpacing).To(Equal(window))
	})

	It("ignores a non-positive reading and is evicted with the k2 history", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		a.recordSaturatedThroughput("k", 0)
		Expect(a.saturatedThroughput).To(BeEmpty())
		a.recordSaturatedThroughput("k", 2)
		// Age the window explicitly rather than evicting with a zero timeout:
		// a zero timeout asks whether any time at all has passed, which on a
		// coarse clock it may not have.
		a.saturatedThroughput["k"].lastUpdated = time.Now().Add(-2 * time.Hour)
		a.EvictStaleHistory(time.Hour)
		Expect(a.saturatedThroughput).To(BeEmpty())
	})

	It("takes a history key apart around its bucket, and refuses one it cannot", func() {
		prefix, bucket, suffix, ok := splitHistoryKey("org/model|H200|2|decode|xlong|q5")
		Expect(ok).To(BeTrue())
		Expect(prefix).To(Equal("org/model|H200|2|decode|"))
		Expect(bucket).To(Equal("xlong"))
		Expect(suffix).To(Equal("|q5"))
		Expect(bucketOf("org/model|H200|2|decode|xlong|q5")).To(Equal("xlong"))

		// A key with fewer than two separators has no bucket field to find.
		// Both the borrow and bucketOf must say so rather than guess.
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		a.recordSaturatedThroughput("m|H200|1|decode|long|q5", 5.4)
		for _, bad := range []string{"", "no-separators", "one|separator"} {
			_, _, _, ok := splitHistoryKey(bad)
			Expect(ok).To(BeFalse(), bad)
			Expect(bucketOf(bad)).To(BeEmpty(), bad)
			mu, from := a.saturatedThroughputFor(bad)
			Expect(mu).To(BeZero(), bad)
			Expect(from).To(BeEmpty(), bad)
		}
		// A well-formed key whose bucket is not one the analyzer knows has
		// nothing to borrow from either.
		mu, from := a.saturatedThroughputFor("m|H200|1|decode|enormous|q5")
		Expect(mu).To(BeZero())
		Expect(from).To(BeEmpty())
	})
})

var _ = Describe("estimateThroughputDemand with mixed readings", func() {
	It("takes the median cost across a role's replicas, not the mean or an extreme", func() {
		// Two variants of one role priced differently: an H200 at 930k tokens
		// completing 5.4/s and a slower card at 600k completing 2.0/s. Costs
		// (P/mu) are 172k and 300k tokens per req/s; with four replicas split
		// two and two the median averages the central pair, 236k. The mean of
		// costs is the same here by symmetry, so the third reading breaks it.
		variants := []domain.VariantCapacity{
			{VariantName: "fast", Role: domain.RoleDecode, ReplicaCount: 3, PerReplicaCapacity: 930_000},
			{VariantName: "slow", Role: domain.RoleDecode, ReplicaCount: 2, PerReplicaCapacity: 600_000},
		}
		replicas := []ReplicaCapacity{
			{VariantName: "fast", SaturatedThroughput: 5.4},
			{VariantName: "fast", SaturatedThroughput: 5.4},
			{VariantName: "fast", SaturatedThroughput: 5.4},
			{VariantName: "slow", SaturatedThroughput: 2.0},
			{VariantName: "slow", SaturatedThroughput: 2.0},
		}
		f := estimateThroughputDemand(6, replicas, variants, nil, BacklogDrainSeconds, 0.85)
		fastCost := 930_000 / 5.4
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", 6*fastCost, 1e-6),
			"five readings, three of them the fast card's: the median is the fast card's cost")
		Expect(f.Terms[domain.RoleDecode].Mu).To(Equal(5.4))

		By("averaging the central pair on an even count")
		f = estimateThroughputDemand(6, replicas[1:], variants, nil, BacklogDrainSeconds, 0.85)
		slowCost := 600_000 / 2.0
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", 6*(fastCost+slowCost)/2, 1e-6))
	})
})

// Through Analyze, replaying the run: a decode replica saturates (P1 fires,
// mu is recorded), then the fleet grows to six and occupancy collapses.
var _ = Describe("the throughput floor, through Analyze", func() {
	var (
		analyzer *SaturationAnalyzer
		ctx      context.Context
		clock    time.Time
	)
	BeforeEach(func() {
		analyzer = NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		// The saturating cycles below are decode full and queued, which
		// the analyzer remembers for DecodeSaturationMemory and holds
		// prefill's demand through (holdPrefillDemand); the specs here are
		// about the floor, so each saturation moves the clock past it.
		clock = time.Date(2026, 9, 17, 10, 48, 9, 0, time.UTC)
		analyzer.now = func() time.Time { return clock }
		ctx = context.Background()
	})

	decode := func(pod string, tokensInUse int64, queue int, rate float64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, decodeVariant, tokensInUse, runKvCapacity, queue, 6000, 1000)
		rm.RequestRate = rate
		rm.Ready = true
		return rm
	}
	prefill := func(pod string, tokensInUse int64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, "prefill-v", tokensInUse, 1_149_312, 0, 6000, 1)
		rm.RequestRate = runLambda
		rm.Ready = true
		return rm
	}
	states := func(decodeN, prefillN int) []domain.VariantReplicaState {
		return []domain.VariantReplicaState{
			{VariantName: decodeVariant, Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: decodeN, GPUsPerReplica: 1},
			{VariantName: "prefill-v", Role: domain.RolePrefill, AcceleratorName: "H200", CurrentReplicas: prefillN, GPUsPerReplica: 1},
		}
	}

	// Two saturated cycles: 07:29:08 on the run, queue 10 over threshold 5,
	// 1.15M resident, completing 5.4/s -- and the cycle after it, which is
	// what makes the window one the floor may order on
	// (MinThroughputSamplesToOrder).
	saturateOnce := func(rate float64) {
		in := makeAnalyzerInput(
			[]domain.ReplicaMetrics{decode("decode-0", 1_158_912, 10, rate), prefill("prefill-0", 66_183)},
			states(1, 1))
		in.ArrivalRate = runLambda
		_, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
	}
	// Each cycle a reading of its own, a rate window apart: the window counts
	// a reading as a sample only when it is a new value that lands
	// ThroughputSampleSpacing after the last one (recordSaturatedThroughput).
	// The first is the under-read, the last is runMu, which the max keeps.
	// The last step also moves the clock past DecodeSaturationMemory.
	saturate := func() {
		for i := MinThroughputSamplesToOrder - 1; i >= 0; i-- {
			saturateOnce(runMu - 0.01*float64(i))
			clock = clock.Add(ThroughputSampleSpacing + time.Second)
		}
	}

	It("holds the decode role at lambda / mu once the fleet has caught up", func() {
		saturate()

		// 07:33:23 on the run: six decode replicas, ~20k tokens each, no
		// queue anywhere. Occupancy said one replica; the load needed two.
		rm := make([]domain.ReplicaMetrics, 0, 9)
		for i := 0; i < 6; i++ {
			rm = append(rm, decode(string(rune('a'+i)), 20_000, 0, 1.0))
		}
		for i := 0; i < 3; i++ {
			rm = append(rm, prefill(string(rune('p'+i)), 0))
		}
		in := makeAnalyzerInput(rm, states(6, 3))
		in.ArrivalRate = runLambda
		result, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())

		var decodeP float64
		for _, vc := range result.VariantCapacities {
			if vc.VariantName == decodeVariant {
				decodeP = vc.PerReplicaCapacity
			}
		}
		Expect(decodeP).To(BeNumerically(">", 0))
		want := runLambda / runMu * decodeP
		Expect(result.RoleDemand[domain.RoleDecode]).To(BeNumerically("~", want, 1),
			"decode demand must be held at lambda / mu replicas' worth")
		Expect(result.RoleDemand[domain.RoleDecode] / decodeP).To(BeNumerically("~", 1.11, 0.01))
		Expect(result.RoleDemand[domain.RolePrefill]).To(BeNumerically("<", 0.5*919_449),
			"prefill was never seen saturated and keeps its measured demand")
		Expect(result.TotalDemand).To(BeNumerically(">=", result.RoleDemand[domain.RoleDecode]))

		// Negative control, on the same fixtures: an analyzer that never saw
		// the saturation has no mu and reports occupancy, one tenth of that.
		fresh := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		bare, err := fresh.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(bare.RoleDemand[domain.RoleDecode]).To(BeNumerically("<", want/5))
	})

	It("orders one replica at most on one saturated sample, however many cycles the row shows it", func() {
		// The measured failure: one decode replica's saturated sample --
		// 1 150 207 resident, queue 20, 3.43 req/s against a true ~5.4 --
		// re-read on four consecutive cycles. Counted four times it cleared
		// MinThroughputSamplesToOrder, and the floor ordered on lambda / 3.43
		// = 1.75 replicas' worth. One sample orders one replica and no more;
		// the second sample, a window on, may order the rest.
		for i := 0; i < 4; i++ {
			in := makeAnalyzerInput(
				[]domain.ReplicaMetrics{decode("decode-0", 1_150_207, 20, 3.433333333333333), prefill("prefill-0", 66_183)},
				states(1, 1))
			in.ArrivalRate = runLambda
			_, err := analyzer.Analyze(ctx, in)
			Expect(err).NotTo(HaveOccurred())
			clock = clock.Add(15 * time.Second)
		}
		Expect(analyzer.saturatedThroughputReading("test-model|H200|1|decode|long|q5").samples).To(Equal(1))

		// The one replica, occupancy a fraction of it, lambda / mu = 1.75:
		// the one reading orders the second replica and no more, though
		// 1.75 through the 0.85 headroom would be two.
		in := makeAnalyzerInput(
			[]domain.ReplicaMetrics{decode("decode-0", 200_000, 0, runLambda), prefill("prefill-0", 0)},
			states(1, 1))
		in.ArrivalRate = runLambda
		result, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		var decodeP float64
		for _, vc := range result.VariantCapacities {
			if vc.VariantName == decodeVariant {
				decodeP = vc.PerReplicaCapacity
			}
		}
		Expect(result.RoleDemand[domain.RoleDecode]).To(BeNumerically("~", 0.85*2*decodeP, 1),
			"capped at scaleUp x (one running + one): RC is one replica exactly")

		By("ordering the full figure once a second reading of its own is on record, a rate window later")
		clock = clock.Add(ThroughputSampleSpacing)
		sat := makeAnalyzerInput(
			[]domain.ReplicaMetrics{decode("decode-0", 1_150_207, 20, 3.5), prefill("prefill-0", 66_183)},
			states(1, 1))
		sat.ArrivalRate = runLambda
		_, err = analyzer.Analyze(ctx, sat)
		Expect(err).NotTo(HaveOccurred())
		result, err = analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RoleDemand[domain.RoleDecode]).To(BeNumerically("~", runLambda/3.5*decodeP, 1),
			"lambda / mu, uncapped: 1.71 replicas' worth, two through the headroom")
	})

	It("counts two replicas saturated in one cycle as one reading of one moment", func() {
		// Two decode replicas over the threshold in the same cycle read two
		// values -- two integer counts over the same scrape interval -- of
		// one moment; before the spacing that was two samples and the guard
		// cleared on the first window. The fleet of two, priced on that one
		// reading, is held at what it has.
		in := makeAnalyzerInput(
			[]domain.ReplicaMetrics{decode("decode-0", 1_150_207, 20, 3.433333333333333), decode("decode-1", 1_130_876, 14, 3.6), prefill("prefill-0", 66_183)},
			states(2, 1))
		in.ArrivalRate = runLambda
		_, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		r := analyzer.saturatedThroughputReading("test-model|H200|1|decode|long|q5")
		Expect(r.samples).To(Equal(1), "one moment, one sample")
		Expect(r.rate).To(Equal(3.6), "at the higher of the two, whichever row came first")

		idle := makeAnalyzerInput(
			[]domain.ReplicaMetrics{decode("decode-0", 200_000, 0, runLambda/2), decode("decode-1", 200_000, 0, runLambda/2), prefill("prefill-0", 0)},
			states(2, 1))
		idle.ArrivalRate = runLambda
		result, err := analyzer.Analyze(ctx, idle)
		Expect(err).NotTo(HaveOccurred())
		var decodeP float64
		for _, vc := range result.VariantCapacities {
			if vc.VariantName == decodeVariant {
				decodeP = vc.PerReplicaCapacity
			}
		}
		Expect(result.RoleDemand[domain.RoleDecode]).To(BeNumerically("~", runLambda/3.6*decodeP, 1),
			"lambda / mu = 1.67 replicas' worth on two: under the one-replica cap, and RC = 0 on a fleet of two")

		By("and the next window, a minute on, is the second reading")
		clock = clock.Add(ThroughputSampleSpacing)
		_, err = analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(analyzer.saturatedThroughputReading("test-model|H200|1|decode|long|q5").samples).To(Equal(2))
	})

	It("orders the second replica from lambda / mu on a fleet of one", func() {
		// After ONE saturated cycle the window holds a single reading, and
		// that already orders the second replica -- lambda / mu = 1.11 on a
		// fleet of one is within the one replica a single reading may order.
		saturateOnce(runMu - 0.01)
		in0 := makeAnalyzerInput(
			[]domain.ReplicaMetrics{decode("decode-0", 200_000, 0, runLambda), prefill("prefill-0", 0)},
			states(1, 1))
		in0.ArrivalRate = runLambda
		first, err := analyzer.Analyze(ctx, in0)
		Expect(err).NotTo(HaveOccurred())
		var firstP float64
		for _, vc := range first.VariantCapacities {
			if vc.VariantName == decodeVariant {
				firstP = vc.PerReplicaCapacity
			}
		}
		Expect(first.RoleDemand[domain.RoleDecode]).To(BeNumerically("~", runLambda/(runMu-0.01)*firstP, 1),
			"one reading, 1.11 replicas' worth: the second replica is ordered on the first saturated cycle")

		clock = clock.Add(ThroughputSampleSpacing)
		saturateOnce(runMu)
		// One decode replica at a fifth of its KV, no queue, mu on record:
		// occupancy says nothing; the load says 1.11 replicas. RC through the
		// engine's headroom is D / 0.85 - P > 0, so the second replica is
		// ordered now rather than after the queue forms.
		in := makeAnalyzerInput(
			[]domain.ReplicaMetrics{decode("decode-0", 200_000, 0, runLambda), prefill("prefill-0", 0)},
			states(1, 1))
		in.ArrivalRate = runLambda
		result, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		var decodeP float64
		for _, vc := range result.VariantCapacities {
			if vc.VariantName == decodeVariant {
				decodeP = vc.PerReplicaCapacity
			}
		}
		Expect(result.RoleDemand[domain.RoleDecode]).To(BeNumerically("~", runLambda/runMu*decodeP, 1))
		Expect(result.RoleDemand[domain.RoleDecode]/0.85).To(BeNumerically(">", decodeP),
			"RC = D / scaleUp - supply is positive on a fleet of one")

		// Negative control: without the saturation on record the same input
		// is priced at its occupancy, and a fleet of one is not asked to grow.
		fresh := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		bare, err := fresh.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(bare.RoleDemand[domain.RoleDecode] / 0.85).To(BeNumerically("<", decodeP))
	})

	It("prices a queue as a backlog to drain, not as KV that must be resident at once", func() {
		saturate()
		// The first ramp of the run: one decode replica full, 180 requests
		// waiting in its engine, 200 more at the scheduler. Charged as
		// residency that was 380 x 7000 = 2.66M tokens on top of the resident
		// 1.15M -- 4.1 replicas' worth, and the run ordered seven. As a
		// backlog to drain in 60 s: (6 + 380/60) / 5.4 = 2.28 replicas' worth.
		in := makeAnalyzerInput(
			[]domain.ReplicaMetrics{decode("decode-0", 1_158_912, 180, runMu), prefill("prefill-0", 66_183)},
			states(1, 1))
		in.ArrivalRate = runLambda
		in.SchedulerQueue = &domain.SchedulerQueueMetrics{QueueSize: 200, QueueBytes: 200 * 6000 * 4}
		result, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		var decodeP float64
		for _, vc := range result.VariantCapacities {
			if vc.VariantName == decodeVariant {
				decodeP = vc.PerReplicaCapacity
			}
		}
		want := (runLambda + 380.0/BacklogDrainSeconds) / runMu * decodeP
		Expect(result.RoleDemand[domain.RoleDecode]).To(BeNumerically("~", want, 1))
		Expect(result.RoleDemand[domain.RoleDecode] / decodeP).To(BeNumerically("~", 2.28, 0.01))
		Expect(result.TotalDemand).To(BeNumerically("~", result.RoleDemand[domain.RoleDecode]+result.RoleDemand[domain.RolePrefill], 1),
			"the total moved with decode, and prefill's dropped share was never in it")

		// Negative control: with no mu on record the queues are still charged
		// as residency, and the same cycle is priced at more than four
		// replicas. A saturated replica records its mu in the cycle its queue
		// appears, so the control is one whose completion rate is not
		// reported (no rate, no reading) rather than a fresh analyzer.
		fresh := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		ctl := makeAnalyzerInput(
			[]domain.ReplicaMetrics{decode("decode-0", 1_158_912, 180, 0), prefill("prefill-0", 66_183)},
			states(1, 1))
		ctl.ArrivalRate = runLambda
		ctl.SchedulerQueue = in.SchedulerQueue
		bare, err := fresh.Analyze(ctx, ctl)
		Expect(err).NotTo(HaveOccurred())
		Expect(bare.RoleDemand[domain.RoleDecode] / decodeP).To(BeNumerically(">", 4))
	})

	It("keeps the resident KV when the backlog term is smaller than it", func() {
		saturate()
		// Three replicas nearly full with a small queue: the resident KV is
		// the larger figure and stays; the floor never lowers demand.
		rm := []domain.ReplicaMetrics{
			decode("a", 1_000_000, 2, 2.0), decode("b", 1_000_000, 2, 2.0), decode("c", 1_000_000, 2, 2.0),
			prefill("p", 0),
		}
		in := makeAnalyzerInput(rm, states(3, 1))
		in.ArrivalRate = runLambda
		result, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RoleDemand[domain.RoleDecode]).To(BeNumerically("~", 3_000_000, 1),
			"the six queued requests come out of the residency charge and the resident 3M stays")
	})

	It("drops prefill's share of the scheduler queue while prefill has no mu, and prices it once it has", func() {
		saturate()
		// 200 prompts at the scheduler were charged to prefill as 200 x 6000
		// tokens of residency -- more than a prefill replica -- and ordered
		// prefill replicas that had nothing to prefill. A prefill replica holds
		// a prompt for its prefill time plus the hand-off; the queue is decode's
		// to drain.
		// Decode is not over its queue threshold here: with decode saturated,
		// prefill's whole demand is held in the no-order/no-release band
		// (holdPrefillDemand), which would mask what this spec is about.
		in := makeAnalyzerInput(
			[]domain.ReplicaMetrics{decode("decode-0", 900_000, 0, runMu), prefill("prefill-0", 66_183)},
			states(1, 1))
		in.ArrivalRate = runLambda
		in.SchedulerQueue = &domain.SchedulerQueueMetrics{QueueSize: 200, QueueBytes: 200 * 6000 * 4}
		result, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RoleDemand[domain.RolePrefill]).To(BeNumerically("~", 66_183, 1),
			"prefill keeps only its own resident KV")

		By("pricing it as a backlog once prefill has a saturated throughput of its own")
		// Learned on a cycle decode is NOT saturated in: a prefill saturation
		// under a saturated decode is decode's, and is not recorded
		// (computeK2).
		satP := prefill("prefill-0", 900_000)
		satP.QueueLength = 10
		satP.RequestRate = 30
		in2 := makeAnalyzerInput([]domain.ReplicaMetrics{decode("decode-0", 300_000, 0, runMu), satP}, states(1, 1))
		in2.ArrivalRate = runLambda
		_, err = analyzer.Analyze(ctx, in2)
		Expect(err).NotTo(HaveOccurred())
		result, err = analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		var prefillP float64
		for _, vc := range result.VariantCapacities {
			if vc.VariantName == "prefill-v" {
				prefillP = vc.PerReplicaCapacity
			}
		}
		Expect(result.RoleDemand[domain.RolePrefill]).To(BeNumerically("~", (runLambda+200.0/BacklogDrainSeconds)/30*prefillP, 1),
			"the 200 queued prompts are 3.3 extra req/s against a prefill mu of 30")
	})

	It("leaves a bridge's queue out of the backlog and out of the residency it takes back", func() {
		saturate()
		// A warm-pool bridge lent to the variant carries a queue of its own;
		// aggregation counts its demand toward the variant, but it is not
		// this variant's backlog to size for and its residency charge is not
		// one the floor put there.
		own := decode("decode-0", 300_000, 0, runMu)
		bridge := decode("bridge-0", 300_000, 600, runMu)
		bridge.FromWarmPool = true
		in := makeAnalyzerInput([]domain.ReplicaMetrics{own, bridge, prefill("prefill-0", 0)}, states(1, 1))
		in.ArrivalRate = runLambda
		result, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		// The bridge's 600 queued requests are charged once, by aggregation,
		// as 600 x 7000 = 4.2M of residency on top of 0.6M resident. The floor
		// neither takes that charge out (it is not one it put there) nor
		// re-prices it as a backlog: either mistake would land at
		// (6 + 600/60) / 5.4 = 2.96 replicas' worth (2.75M) instead.
		Expect(result.RoleDemand[domain.RoleDecode]).To(BeNumerically("~", 600_000+600*7000, 1),
			"the bridge's queue stays as the residency aggregation charged, and enters no backlog")
	})

	It("keeps prefill's own engine queue as residency while dropping its scheduler-queue share", func() {
		saturate()
		// Only the share of the scheduler queue goes: requests waiting in a
		// prefill engine's own queue are work prefill has accepted, and stay
		// charged until prefill has a mu to price them by.
		p := prefill("prefill-0", 66_183)
		p.QueueLength = 4 // under the threshold: no saturation, no mu for prefill
		in := makeAnalyzerInput([]domain.ReplicaMetrics{decode("decode-0", 300_000, 0, runMu), p}, states(1, 1))
		in.ArrivalRate = runLambda
		in.SchedulerQueue = &domain.SchedulerQueueMetrics{QueueSize: 200, QueueBytes: 200 * 6000 * 4}
		result, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RoleDemand[domain.RolePrefill]).To(BeNumerically("~", 66_183+4*6000, 1),
			"resident KV plus the own queue at prefill's input-only footprint; the 200 x 6000 scheduler share is gone")
	})

	It("floors the resident KV at zero when the residency it takes back exceeds what was measured", func() {
		// Reachable only if a residency charge outlives the demand it was
		// folded into; the arithmetic must not hand the engine a negative
		// demand. Exercised at the function level with an inconsistent pair.
		rcs := []ReplicaCapacity{{VariantName: decodeVariant, SaturatedThroughput: runMu,
			SaturatedThroughputSamples: MinThroughputSamplesToOrder, QueueLength: 10, LocalQueueDemand: 500_000}}
		vcs := []domain.VariantCapacity{{VariantName: decodeVariant, Role: domain.RoleDecode, ReplicaCount: 1, PerReplicaCapacity: float64(runK1)}}
		roleDemand := map[string]float64{domain.RoleDecode: 100_000}
		in := makeAnalyzerInput(nil, states(1, 1))
		in.ArrivalRate = runLambda
		total := analyzer.applyThroughputFloor(in, in.Config.(*config.ScalingPolicy), rcs, vcs, 100_000, roleDemand, nil, 0, GinkgoLogr)
		want := (runLambda + 10.0/BacklogDrainSeconds) / runMu * float64(runK1)
		Expect(roleDemand[domain.RoleDecode]).To(BeNumerically("~", want, 1))
		Expect(total).To(BeNumerically("~", want, 1))
		Expect(total).To(BeNumerically(">", 0))
	})

	It("does not record a bridge's throughput under the variant it is lent to", func() {
		// A warm-pool bridge is recorded with the borrowing variant's name, so
		// it lands on the same history key as the variant's own replicas. Its
		// engine runs on the pool's terms; its saturated rate must not price
		// the variant -- and the window is a max, so one reading would.
		in := makeAnalyzerInput(
			[]domain.ReplicaMetrics{decode("bridge-0", 1_158_912, 10, 9.9)},
			states(1, 0)[:1])
		in.ReplicaMetrics[0].FromWarmPool = true
		_, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(analyzer.saturatedThroughput).To(BeEmpty())

		By("while the variant's own replica on the same key is recorded")
		in = makeAnalyzerInput(
			[]domain.ReplicaMetrics{decode("decode-0", 1_158_912, 10, runMu)},
			states(1, 0)[:1])
		_, err = analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(analyzer.saturatedThroughput).To(HaveLen(1))
		for _, ra := range analyzer.saturatedThroughput {
			Expect(ra.Max()).To(Equal(runMu), "the bridge's 9.9 must not be in the window")
		}
	})

	It("keeps TotalDemand and the RoleDemand sum moving together when it binds", func() {
		saturate()
		// Six replicas at a fraction of a replica's occupancy each. The floor
		// raises decode; TotalDemand must move by the same amount, since the
		// optimizer reads the per-role figure for a P/D fleet and the
		// model-level one everywhere else.
		rm := make([]domain.ReplicaMetrics, 0, 7)
		for i := 0; i < 6; i++ {
			rm = append(rm, decode(string(rune('a'+i)), 3_000, 0, 1.0))
		}
		rm = append(rm, prefill("p", 0))
		in := makeAnalyzerInput(rm, states(6, 1))
		in.ArrivalRate = runLambda
		result, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())

		var decodeP float64
		for _, vc := range result.VariantCapacities {
			if vc.VariantName == decodeVariant {
				decodeP = vc.PerReplicaCapacity
			}
		}
		want := runLambda / runMu * decodeP
		Expect(result.RoleDemand[domain.RoleDecode]).To(BeNumerically("~", want, 1))
		var sum float64
		for _, v := range result.RoleDemand {
			sum += v
		}
		Expect(result.TotalDemand).To(BeNumerically("~", sum, 1),
			"TotalDemand and RoleDemand must move together after the floor")
	})

	It("counts each request once in the arrival-rate fallback on a P/D fleet", func() {
		// A request completes on its prefill replica and again on its decode
		// replica. Summing every replica's completion rate would report 2λ.
		rm := []domain.ReplicaMetrics{
			{VariantName: "prefill-v", AvgInputTokens: 6000, AvgOutputTokens: 1, RequestRate: 6},
			{VariantName: decodeVariant, AvgInputTokens: 6000, AvgOutputTokens: 1000, RequestRate: 6},
		}
		in := domain.AnalyzerInput{ReplicaMetrics: rm, VariantStates: states(1, 1)}
		Expect(offeredArrivalRate(in)).To(Equal(6.0))

		By("and prefers the scheduler's figure when there is one")
		in.ArrivalRate = 9
		Expect(offeredArrivalRate(in)).To(Equal(9.0))

		By("and falls back to every replica when the decode side reports nothing")
		in = domain.AnalyzerInput{ReplicaMetrics: rm[:1], VariantStates: states(1, 1)}
		Expect(offeredArrivalRate(in)).To(Equal(6.0), "a poor estimate beats silently declining")
	})

	It("does not record a throughput from a pod that is not Ready", func() {
		in := makeAnalyzerInput(
			[]domain.ReplicaMetrics{decode("decode-0", 1_158_912, 10, runMu)},
			states(1, 0)[:1])
		in.ReplicaMetrics[0].Ready = false
		_, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(analyzer.saturatedThroughput).To(BeEmpty())
	})

	It("floors a non-disaggregated fleet on its total", func() {
		both := func(pod string, tokensInUse int64, queue int, rate float64) domain.ReplicaMetrics {
			rm := makeReplicaMetrics(pod, "v", tokensInUse, runKvCapacity, queue, 6000, 1000)
			rm.RequestRate = rate
			rm.Ready = true
			return rm
		}
		st := []domain.VariantReplicaState{{VariantName: "v", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1}}
		in := makeAnalyzerInput([]domain.ReplicaMetrics{both("a", 1_158_912, 10, runMu)}, st)
		in.ArrivalRate = runLambda
		_, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())

		in.ReplicaMetrics[0].RequestRate = runMu - 0.01 // a second reading, a rate window later
		clock = clock.Add(ThroughputSampleSpacing)
		_, err = analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred(), "the second saturated cycle, so the window may order")

		st[0].CurrentReplicas = 4
		in = makeAnalyzerInput([]domain.ReplicaMetrics{
			both("a", 20_000, 0, 1.5), both("b", 20_000, 0, 1.5), both("c", 20_000, 0, 1.5), both("d", 20_000, 0, 1.5)}, st)
		in.ArrivalRate = runLambda
		result, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RoleDemand).To(BeNil())
		Expect(result.TotalDemand).To(BeNumerically("~", runLambda/runMu*result.VariantCapacities[0].PerReplicaCapacity, 1))

		By("pricing a queue as a backlog on the total, with the residency charge taken out")
		// One replica full with 60 waiting and 40 at the scheduler: residency
		// would charge 100 x 7000 = 700k on top of 1.16M resident; the model
		// prices (6 + 100/60) / 5.4 = 1.42 replicas' worth.
		st[0].CurrentReplicas = 1
		in = makeAnalyzerInput([]domain.ReplicaMetrics{both("a", 1_158_912, 60, runMu)}, st)
		in.ArrivalRate = runLambda
		in.SchedulerQueue = &domain.SchedulerQueueMetrics{QueueSize: 40, QueueBytes: 40 * 6000 * 4}
		result, err = analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		P := result.VariantCapacities[0].PerReplicaCapacity
		Expect(result.TotalDemand).To(BeNumerically("~", (runLambda+100.0/BacklogDrainSeconds)/runMu*P, 1))
		Expect(result.TotalDemand / P).To(BeNumerically("~", 1.42, 0.01))
	})
})
