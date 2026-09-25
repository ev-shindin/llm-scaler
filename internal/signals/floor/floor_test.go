package floor

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Figures from the shape-swap P/D run (biran-20260915-102548-571), H200,
// Qwen3-0.6B, 6 req/s throughout. In its first phase (6000 in / 1000 out) one
// decode replica saturated at ~5.4 completions/s with 1.15M tokens resident;
// at six replicas the same load occupied ~100k tokens in total and the
// controller sized the fleet to one, which then re-saturated within a minute.
const (
	runLambda = 6.0
	runMu     = 5.4
	runK1     = int64(929_792) // 1,162,240 x 0.8
)

var _ = Describe("Estimate", func() {
	variants := func(role string, ready int) []domain.VariantCapacity {
		return []domain.VariantCapacity{{
			VariantName: "v", Role: role, ReplicaCount: ready, PerReplicaCapacity: float64(runK1),
		}}
	}
	// Readings from the variant's own bucket with enough samples to order;
	// the specs on holding build their own.
	replicas := func(n int) []capacity.ReplicaCapacity {
		out := make([]capacity.ReplicaCapacity, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, capacity.ReplicaCapacity{VariantName: "v", SaturatedThroughput: runMu,
				SaturatedThroughputSamples: MinThroughputSamplesToOrder})
		}
		return out
	}

	It("sizes the fleet to lambda over mu, in the variant's own tokens", func() {
		// 6 / 5.4 = 1.11 replicas' worth of demand. Through the engine's
		// RC = D / 0.85 - supply that is 1.31 replicas, so a two-replica fleet
		// holds and a one-replica fleet is (correctly) short.
		f := Estimate(runLambda, replicas(6), variants(domain.RoleDecode, 6), nil, BacklogDrainSeconds, 0.85, false, 0)
		Expect(f.ByRole).To(HaveKey(domain.RoleDecode))
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", runLambda/runMu*float64(runK1), 1e-6))
		Expect(f.Terms[domain.RoleDecode].Replicas).To(BeNumerically("~", 1.111, 1e-3))
		Expect(f.Terms[domain.RoleDecode].Backlog).To(BeZero())
	})

	It("holds a role whose shape has changed, however many readings it has", func() {
		// The gate this covers is the one a shape change adds, and it only
		// shows on readings that would otherwise ORDER: the role's own bucket,
		// with enough samples. Without staleShape a fleet of one orders its second replica;
		// with it the same readings may hold what it has and no
		// more, because every reading on record was taken under a shape the
		// fleet has left.
		ordering := Estimate(runLambda, replicas(1), variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0.85, false, 0)
		Expect(ordering.Terms[domain.RoleDecode].Held).To(BeFalse(),
			"the same readings order when the shape is steady")

		held := Estimate(runLambda, replicas(1), variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0.85, true, 0)
		Expect(held.Terms[domain.RoleDecode].Held).To(BeTrue())
		Expect(held.Terms[domain.RoleDecode].HeldWhy).To(Equal("shape-change"),
			"named for the reason, not for the sample count it would otherwise report")
		Expect(held.ByRole[domain.RoleDecode]).To(BeNumerically("<=", ordering.ByRole[domain.RoleDecode]),
			"a hold never asks for more than the order it replaces")
		Expect(held.Terms[domain.RoleDecode].Replicas).To(Equal(ordering.Terms[domain.RoleDecode].Replicas),
			"the uncapped figure is still reported, as it is for the other holds")
	})

	It("does not label a term shape-change when nothing was capped", func() {
		// A fleet whose floor is already under the hold cap is not held at
		// all, and must not be labelled as though it were.
		f := Estimate(runLambda, replicas(60), variants(domain.RoleDecode, 60), nil, BacklogDrainSeconds, 0.85, true, 0)
		Expect(f.Terms[domain.RoleDecode].Held).To(BeFalse())
		Expect(f.Terms[domain.RoleDecode].HeldWhy).To(BeEmpty())
	})

	It("does not move when replicas are added or removed", func() {
		// The property the arrival floor was supposed to have and did not:
		// mu is a per-replica constant, so the floor is the same at one
		// replica as at six.
		one := Estimate(runLambda, replicas(1), variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0.85, false, 0)
		six := Estimate(runLambda, replicas(6), variants(domain.RoleDecode, 6), nil, BacklogDrainSeconds, 0.85, false, 0)
		Expect(six.ByRole[domain.RoleDecode]).To(BeNumerically("~", one.ByRole[domain.RoleDecode], 1e-6))
	})

	It("asks for the replicas the load needs, whether the fleet has them or not", func() {
		// One replica, lambda / mu = 1.11: through the engine's RC = D / 0.85
		// - supply that orders the second replica in the first cycle lambda
		// is measured. The first version capped this at the fleet's size and
		// the order came 50 s later, from occupancy, after the replica had
		// tipped into preemption (file header).
		f := Estimate(runLambda, replicas(1), variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0.85, false, 0)
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", runLambda/runMu*float64(runK1), 1e-6))
		Expect(f.ByRole[domain.RoleDecode]/0.85).To(BeNumerically(">", float64(runK1)),
			"RC = D / scaleUp - one replica's supply is positive: the second replica is ordered")

		By("and not one more as replicas arrive: the figure is the load's, not the fleet's")
		g := Estimate(runLambda, replicas(2), variants(domain.RoleDecode, 2), nil, BacklogDrainSeconds, 0.85, false, 0)
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
		borrowed := capacity.ReplicaCapacity{VariantName: "v", SaturatedThroughput: 2.67, SaturatedThroughputSamples: 10, SaturatedThroughputBorrowed: true}
		f := Estimate(runLambda, []capacity.ReplicaCapacity{borrowed}, variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0.85, false, 0)
		Expect(f.Terms[domain.RoleDecode].Replicas).To(BeNumerically("~", runLambda/2.67, 1e-6), "the uncapped figure is reported")
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", 0.85*float64(runK1), 1e-6), "capped at scaleUp x one replica")
		Expect(f.Terms[domain.RoleDecode].Held).To(BeTrue())
		Expect(f.Terms[domain.RoleDecode].HeldWhy).To(Equal("borrowed"))

		By("ordering once one replica has a reading of its own")
		own := capacity.ReplicaCapacity{VariantName: "v", SaturatedThroughput: 2.67, SaturatedThroughputSamples: MinThroughputSamplesToOrder}
		g := Estimate(runLambda, []capacity.ReplicaCapacity{borrowed, own}, variants(domain.RoleDecode, 2), nil, BacklogDrainSeconds, 0.85, false, 0)
		Expect(g.Terms[domain.RoleDecode].Held).To(BeFalse())
		Expect(g.ByRole[domain.RoleDecode]).To(BeNumerically("~", runLambda/2.67*float64(runK1), 1e-6))
	})

	It("never publishes a negative floor, whatever the anticipated supply says", func() {
		// The hold cap is scaleUp x the role's anticipated supply, and that
		// is (ReplicaCount + PendingReplicas) x P. Unguarded, a negative
		// product caps a real floor BELOW zero -- every floor is above a
		// negative hold, so the branch takes it -- and the package's one
		// promise, that it only ever raises demand, inverts. Producers clamp
		// PendingReplicas today; this package cannot see that they do.
		one := []capacity.ReplicaCapacity{{VariantName: "v", SaturatedThroughput: runMu, SaturatedThroughputSamples: 1}}
		bad := []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode,
			ReplicaCount: 1, PendingReplicas: -3, PerReplicaCapacity: float64(runK1)}}
		f := Estimate(runLambda, one, bad, nil, BacklogDrainSeconds, 0.85, false, 0)
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically(">=", 0),
			"a negative anticipated supply must hold the floor at zero, not below it")
		Expect(f.ByRole[domain.RoleDecode]).To(BeZero(),
			"held at zero: the cap is what the engine's RC turns into nothing")
		Expect(f.Terms[domain.RoleDecode].Held).To(BeTrue())
	})

	It("lets a single reading order while the scheduler holds a queue", func() {
		// The hold exists because an order on a first under-read
		// over-provisions AND the over-provisioned fleet then never saturates
		// again to record the correcting reading. The second half fails while
		// the scheduler is holding work: the queue keeps the fleet saturated
		// until it drains, so the second reading arrives either way.
		one := []capacity.ReplicaCapacity{{VariantName: "v", SaturatedThroughput: runMu, SaturatedThroughputSamples: 1}}
		v := variants(domain.RoleDecode, 1)

		By("holding when the scheduler queue is only a transient")
		f := Estimate(runLambda, one, v, nil, BacklogDrainSeconds, 0.85, false, runLambda)
		Expect(f.Terms[domain.RoleDecode].Held).To(BeTrue(), "one second of arrivals is not a standing queue")
		Expect(f.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeFalse())

		By("ordering once it holds more than a second of arrivals")
		g := Estimate(runLambda, one, v, nil, BacklogDrainSeconds, 0.85, false, runLambda*10)
		Expect(g.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeTrue())
		Expect(g.Terms[domain.RoleDecode].Held).To(BeFalse(), "released, so the floor is its own figure")
		Expect(g.ByRole[domain.RoleDecode]).To(BeNumerically(">", f.ByRole[domain.RoleDecode]),
			"the whole point is that it may ask for more than the fleet it has")

		By("asking for one replica beyond the fleet, and no more")
		// The release skips the scaleUp cap, so it carries its own bound: a
		// thin window is exactly the reading that under-reads mu, and the
		// floor is rate x P/mu, so an unbounded release doubles the order
		// when mu is halved. One replica beyond anticipated is what a queue
		// justifies.
		//
		// scaleUp x (anticipated + P) leaves the engine RC = D/scaleUp -
		// anticipated at one replica, so assert the tokens directly.
		// The bound only BINDS when the floor exceeds it; with an accurate
		// reading the floor sits under it and is left alone.
		Expect(g.ByRole[domain.RoleDecode]).To(BeNumerically("<=", 0.85*(float64(runK1)+g.Terms[domain.RoleDecode].PerReplica)+1e-6),
			"never more than one replica beyond the one already there")

		By("and an under-read mu cannot inflate that")
		// Half the reading: the unbounded floor would double. The bound holds.
		thinHalf := []capacity.ReplicaCapacity{{VariantName: "v", SaturatedThroughput: runMu / 2, SaturatedThroughputSamples: 1}}
		half := Estimate(runLambda, thinHalf, v, nil, BacklogDrainSeconds, 0.85, false, runLambda*10)
		Expect(half.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeTrue())
		Expect(half.ByRole[domain.RoleDecode]).To(BeNumerically("~", 0.85*(float64(runK1)+half.Terms[domain.RoleDecode].PerReplica), 1e-6),
			"still one replica, though the reading is half what it should be")

		By("still holding when the fleet's shape has changed under it")
		st := Estimate(runLambda, one, v, nil, BacklogDrainSeconds, 0.85, true, runLambda*10)
		Expect(st.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeFalse(),
			"every reading on record was taken under a shape the fleet has left; a queue does not make it right")
		Expect(st.Terms[domain.RoleDecode].Held).To(BeTrue())

		By("still holding when the only reading is borrowed from another bucket")
		bor := []capacity.ReplicaCapacity{{VariantName: "v", SaturatedThroughput: runMu,
			SaturatedThroughputSamples: 10, SaturatedThroughputBorrowed: true}}
		bq := Estimate(runLambda, bor, v, nil, BacklogDrainSeconds, 0.85, false, runLambda*10)
		Expect(bq.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeFalse(),
			"a borrowed reading is wrong in a known direction, queue or no queue")
		Expect(bq.Terms[domain.RoleDecode].Held).To(BeTrue())
		Expect(bq.Terms[domain.RoleDecode].HeldWhy).To(Equal("borrowed"))

		By("not releasing on jitter when the load is light")
		// One stray request is ten seconds of arrivals at 0.1 req/s, so a test
		// against lambda alone would call it a standing queue. Against mu it
		// is a fraction of one replica-second of work, which is what it is.
		light := Estimate(0.1, one, v, nil, BacklogDrainSeconds, 0.85, false, 1)
		Expect(light.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeFalse(),
			"a single queued request on a lightly loaded fleet is jitter, not a queue")

		By("ignoring the engines' own queues, which may be waiting on a KV transfer")
		busy := Estimate(runLambda, one, v, map[string]float64{domain.RoleDecode: 500},
			BacklogDrainSeconds, 0.85, false, 0)
		Expect(busy.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeFalse(),
			"a request awaiting a remote KV transfer is not work another replica drains")
		Expect(busy.Terms[domain.RoleDecode].Held).To(BeTrue())
	})

	It("climbs to a figure that does not move, one start at a time", func() {
		// The pending test paces the climb; it does not prevent it. What makes
		// the climb safe is that the figure does not move: (lambda + backlog /
		// drain) / mu is fixed by the load and the queue, not by the fleet. So
		// a thin reading behind a standing queue converges on it instead of
		// ratcheting past it -- which is the property the rule rests on, and
		// the one a reader will want pinned.
		thin := []capacity.ReplicaCapacity{{VariantName: "v", SaturatedThroughput: runMu, SaturatedThroughputSamples: 1}}
		fleet := func(ready, pending int) []domain.VariantCapacity {
			return []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode,
				PerReplicaCapacity: float64(runK1), ReplicaCount: ready, PendingReplicas: pending}}
		}
		const queue = 60.0
		at := func(ready, pending int) Floor {
			return Estimate(runLambda, thin, fleet(ready, pending), nil, BacklogDrainSeconds, 0.85, false, queue)
		}

		first := at(2, 0)
		Expect(first.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeTrue())

		By("the figure is offered while the start is outstanding too")
		Expect(at(2, 1).Terms[domain.RoleDecode].OrderedBehindQueue).To(BeTrue())
		Expect(at(2, 1).ByRole[domain.RoleDecode]).To(BeNumerically("~", first.ByRole[domain.RoleDecode], 1e-6),
			"and it is the same figure, so the engine's own anticipated-supply subtraction does the pacing")

		By("and the figure is the same once it lands, and after the next, and the next")
		for _, ready := range []int{3, 4, 5} {
			f := at(ready, 0)
			Expect(f.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeTrue())
			Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", first.ByRole[domain.RoleDecode], 1e-6),
				"the floor must not grow as the fleet does, or this would ratchet")
		}

		By("and it stops asking once the queue is no longer a queue")
		Expect(at(5, 0).Terms[domain.RoleDecode].OrderedBehindQueue).To(BeTrue())
		quiet := Estimate(runLambda, thin, fleet(5, 0), nil, BacklogDrainSeconds, 0.85, false, runMu)
		Expect(quiet.Terms[domain.RoleDecode].OrderedBehindQueue).To(BeFalse())
	})

	It("holds but does not order on a single reading", func() {
		// The first reading at a saturation under-reads (3.67 against a true
		// 7.13 on the run); an order on it over-provisions, and the
		// over-provisioned fleet never saturates again to correct it.
		// Letting one reading order one replica was tried and dropped: a
		// ratchet across starts, and one cycle's worth of benefit measured.
		one := []capacity.ReplicaCapacity{{VariantName: "v", SaturatedThroughput: runMu / 2, SaturatedThroughputSamples: 1}}
		f := Estimate(runLambda, one, variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0.85, false, 0)
		Expect(f.Terms[domain.RoleDecode].Replicas).To(BeNumerically("~", 2.22, 0.01))
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", 0.85*float64(runK1), 1e-6),
			"capped at scaleUp x the one replica: RC = 0 exactly")
		Expect(f.Terms[domain.RoleDecode].HeldWhy).To(Equal("single-sample"))

		By("holding at the anticipated size when a replica is already on its way")
		pending := []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode, ReplicaCount: 2, PendingReplicas: 1, PerReplicaCapacity: 100}}
		pend := Estimate(100, one, pending, nil, BacklogDrainSeconds, 0.85, false, 0)
		Expect(pend.Terms[domain.RoleDecode].Held).To(BeTrue())
		Expect(pend.ByRole[domain.RoleDecode]).To(BeNumerically("~", 0.85*300, 1e-6))

		By("ordering from the second reading on")
		two := []capacity.ReplicaCapacity{{VariantName: "v", SaturatedThroughput: runMu / 2, SaturatedThroughputSamples: 2}}
		g := Estimate(runLambda, two, variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0.85, false, 0)
		Expect(g.Terms[domain.RoleDecode].Held).To(BeFalse())
		Expect(g.ByRole[domain.RoleDecode]).To(BeNumerically("~", 2.22*float64(runK1), 0.01*float64(runK1)))

		By("with no scale-up threshold there is nothing to cap against, and the figure stands")
		z := Estimate(runLambda, one, variants(domain.RoleDecode, 1), nil, BacklogDrainSeconds, 0, false, 0)
		Expect(z.Terms[domain.RoleDecode].Held).To(BeFalse())
	})

	It("prices a backlog as arrivals to clear within the drain target", func() {
		// 350 queued requests on the run's first ramp. Charged as residency
		// they were 2.45M tokens -- five replicas' worth on top of the load.
		// As throughput: 350 / 60 s = 5.8 extra req/s, (6 + 5.8) / 5.4 = 2.19
		// replicas in all, the load included.
		backlog := map[string]float64{domain.RoleDecode: 350}
		f := Estimate(runLambda, replicas(1), variants(domain.RoleDecode, 1), backlog, 60, 0.85, false, 0)
		Expect(f.Terms[domain.RoleDecode].Backlog).To(Equal(350.0))
		Expect(f.Terms[domain.RoleDecode].Replicas).To(BeNumerically("~", (runLambda+350.0/60)/runMu, 1e-6))
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", (runLambda+350.0/60)/runMu*float64(runK1), 1e-6))

		By("a longer drain target asks for less")
		g := Estimate(runLambda, replicas(1), variants(domain.RoleDecode, 1), backlog, 120, 0.85, false, 0)
		Expect(g.ByRole[domain.RoleDecode]).To(BeNumerically("<", f.ByRole[domain.RoleDecode]))

		By("another role's backlog is not this role's")
		h := Estimate(runLambda, replicas(1), variants(domain.RoleDecode, 1),
			map[string]float64{domain.RolePrefill: 350}, 60, 0.85, false, 0)
		Expect(h.Terms[domain.RoleDecode].Backlog).To(BeZero())

		By("a non-positive drain target disables the backlog term rather than dividing by it")
		z := Estimate(runLambda, replicas(1), variants(domain.RoleDecode, 1), backlog, 0, 0.85, false, 0)
		Expect(z.Terms[domain.RoleDecode].Backlog).To(BeZero())
		Expect(z.ByRole[domain.RoleDecode]).To(BeNumerically("~", runLambda/runMu*float64(runK1), 1e-6))
	})

	It("has no opinion for a role that has never been seen saturated", func() {
		rcs := append(replicas(2), capacity.ReplicaCapacity{VariantName: "p", SaturatedThroughput: 0})
		vcs := append(variants(domain.RoleDecode, 2),
			domain.VariantCapacity{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: 1, PerReplicaCapacity: 919_449})
		f := Estimate(runLambda, rcs, vcs, nil, BacklogDrainSeconds, 0.85, false, 0)
		Expect(f.ByRole).To(HaveKey(domain.RoleDecode))
		Expect(f.ByRole).NotTo(HaveKey(domain.RolePrefill))
	})

	It("leaves a bridge's throughput out", func() {
		// A warm-pool bridge runs its engine on different terms (lower
		// --gpu-memory-utilization, and it is going home); its rate is not
		// this variant's.
		rcs := []capacity.ReplicaCapacity{{VariantName: "v", SaturatedThroughput: 1, FromWarmPool: true}}
		f := Estimate(runLambda, rcs, variants(domain.RoleBoth, 0), nil, BacklogDrainSeconds, 0.85, false, 0)
		Expect(f.ByRole).To(BeEmpty())
	})

	It("leaves out a replica whose variant has no per-replica capacity", func() {
		// A throughput reading can outlive the capacity it was recorded
		// beside -- the window persists while a variant's P reads zero for a
		// cycle. P / mu is then 0, and a zero cost in the median would drag
		// the role's floor toward nothing for the replicas that are priced.
		vcs := append(variants(domain.RoleDecode, 1),
			domain.VariantCapacity{VariantName: "unpriced", Role: domain.RoleDecode, ReplicaCount: 1, PerReplicaCapacity: 0})
		rcs := append(replicas(1), capacity.ReplicaCapacity{VariantName: "unpriced", SaturatedThroughput: runMu})
		f := Estimate(runLambda, rcs, vcs, nil, BacklogDrainSeconds, 0.85, false, 0)
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", runLambda/runMu*float64(runK1), 1e-6),
			"the priced replica alone decides the floor")
	})

	It("says nothing without an arrival rate", func() {
		f := Estimate(0, replicas(2), variants(domain.RoleBoth, 2), nil, BacklogDrainSeconds, 0.85, false, 0)
		Expect(f.ByRole).To(BeEmpty())
	})
})

var _ = Describe("Estimate with mixed readings", func() {
	It("lets no borrowed reading outvote a replica's own", func() {
		// Replayed from the 1000/6000 shape-swap trace (2026-09-20, cycle
		// 11:46:37): the replica that had been saturated under the new shape
		// reads its own xxlong bucket at 1.74 req/s, two fresh replicas read
		// an output length of 0 (no completions yet, or the short ones that
		// finish first), land in a bucket with no reading and borrow the
		// previous shape's 4.38. The median of the three was 4.38 -- the
		// backlog of 441 requests read as two replicas' worth instead of five,
		// and the target went from 10 to 4 while the backlog grew.
		variants := []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode, ReplicaCount: 3, PerReplicaCapacity: 930_000}}
		own := capacity.ReplicaCapacity{VariantName: "v", SaturatedThroughput: 1.74, SaturatedThroughputSamples: MinThroughputSamplesToOrder}
		fresh := capacity.ReplicaCapacity{VariantName: "v", SaturatedThroughput: 4.38, SaturatedThroughputSamples: 10, SaturatedThroughputBorrowed: true}
		backlog := map[string]float64{domain.RoleDecode: 441}
		f := Estimate(1.68, []capacity.ReplicaCapacity{fresh, own, fresh}, variants, backlog, BacklogDrainSeconds, 0.85, false, 0)
		term := f.Terms[domain.RoleDecode]
		Expect(term.Mu).To(Equal(1.74), "the own reading, however many replicas borrow")
		Expect(term.Replicas).To(BeNumerically("~", (1.68+441/BacklogDrainSeconds)/1.74, 1e-6))
		Expect(term.Held).To(BeFalse(), "an own reading with enough samples still orders")
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", (1.68+441/BacklogDrainSeconds)/1.74*930_000, 1e-6))

		By("taking the borrowed readings when no replica reads its own")
		g := Estimate(1.68, []capacity.ReplicaCapacity{fresh, fresh}, variants, backlog, BacklogDrainSeconds, 0.85, false, 0)
		Expect(g.Terms[domain.RoleDecode].Mu).To(Equal(4.38))
		Expect(g.Terms[domain.RoleDecode].Replicas).To(BeNumerically("~", (1.68+441/BacklogDrainSeconds)/4.38, 1e-6))
		// Not held: two replicas' worth is under the fleet's cap (0.85 x 3 x
		// 930k), so the cap has nothing to do -- the borrowed figure
		// under-holds, which is the direction the header accepts.
		Expect(g.Terms[domain.RoleDecode].Held).To(BeFalse())
		Expect(g.ByRole[domain.RoleDecode]).To(BeNumerically("<", 0.85*3*930_000))

		By("keeping the own readings' median when they disagree among themselves")
		own2 := capacity.ReplicaCapacity{VariantName: "v", SaturatedThroughput: 1.26, SaturatedThroughputSamples: 1}
		h := Estimate(1.68, []capacity.ReplicaCapacity{fresh, own, own2, fresh, fresh}, variants, backlog, BacklogDrainSeconds, 0.85, false, 0)
		Expect(h.Terms[domain.RoleDecode].Mu).To(BeNumerically("~", (1.74+1.26)/2, 1e-9), "the central pair of the two own readings, three borrowed ones ignored")
		Expect(h.Terms[domain.RoleDecode].Held).To(BeFalse(), "one own window has enough samples")
	})

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
		replicas := []capacity.ReplicaCapacity{
			{VariantName: "fast", SaturatedThroughput: 5.4},
			{VariantName: "fast", SaturatedThroughput: 5.4},
			{VariantName: "fast", SaturatedThroughput: 5.4},
			{VariantName: "slow", SaturatedThroughput: 2.0},
			{VariantName: "slow", SaturatedThroughput: 2.0},
		}
		f := Estimate(6, replicas, variants, nil, BacklogDrainSeconds, 0.85, false, 0)
		fastCost := 930_000 / 5.4
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", 6*fastCost, 1e-6),
			"five readings, three of them the fast card's: the median is the fast card's cost")
		Expect(f.Terms[domain.RoleDecode].Mu).To(Equal(5.4))

		By("averaging the central pair on an even count")
		f = Estimate(6, replicas[1:], variants, nil, BacklogDrainSeconds, 0.85, false, 0)
		slowCost := 600_000 / 2.0
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", 6*(fastCost+slowCost)/2, 1e-6))
	})
})
