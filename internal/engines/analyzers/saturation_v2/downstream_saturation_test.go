package saturation_v2

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// The cycle this replays is 15:01:33Z on the shape-swap P/D benchmark's cold
// pass (2026-09-18): two decode replicas over the queue threshold at about
// their k1, one prefill replica showing queue 30 and 357 800 resident tokens
// because decode could not admit what it had prefilled. Before the gate the
// analyzer recorded that as prefill's k2 (against a k1 of 919 449 here) and
// a prefill mu of 4.77-5.57 req/s, ordered a second prefill replica on the
// spot, and held both for the rest of the run on lambda / mu = 1.08 while
// their resident KV read zero.
var _ = Describe("a prefill saturation under a saturated decode", func() {
	var (
		analyzer *SaturationAnalyzer
		ctx      context.Context
		clock    time.Time
	)
	BeforeEach(func() {
		analyzer = NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		clock = time.Date(2026, 9, 18, 15, 1, 33, 0, time.UTC)
		analyzer.now = func() time.Time { return clock }
		ctx = context.Background()
	})
	// forget moves the clock past DecodeSaturationMemory, for the specs that
	// want the cycle after an episode to read decode as recovered.
	forget := func() { clock = clock.Add(DecodeSaturationMemory + time.Second) }

	const (
		prefillVariant = "prefill-v"
		prefillKv      = int64(1_149_312)
		prefillK1      = 919_449.0 // prefillKv x 0.8, truncated
		prefillKey     = "test-model|H200|1|prefill|short|q5"
		decodeKey      = "test-model|H200|1|decode|long|q5"
	)
	decode := func(pod string, tokensInUse int64, queue int) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, decodeVariant, tokensInUse, runKvCapacity, queue, 6000, 1000)
		rm.RequestRate = 4.73
		rm.Ready = true
		return rm
	}
	// The prefill row as the run showed it: queue 30, 357 800 resident.
	prefill := func() domain.ReplicaMetrics {
		rm := makeReplicaMetrics("prefill-0", prefillVariant, 357_800, prefillKv, 30, 6000, 1)
		rm.RequestRate = 4.77
		rm.Ready = true
		return rm
	}
	states := []domain.VariantReplicaState{
		{VariantName: decodeVariant, Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 2, GPUsPerReplica: 1},
		{VariantName: prefillVariant, Role: domain.RolePrefill, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	prefillP := func(result *domain.AnalyzerResult) float64 {
		for _, vc := range result.VariantCapacities {
			if vc.VariantName == prefillVariant {
				return vc.PerReplicaCapacity
			}
		}
		Fail("no capacity for " + prefillVariant)
		return 0
	}

	It("records neither prefill's k2 nor its throughput, and prices prefill at k1", func() {
		in := makeAnalyzerInput([]domain.ReplicaMetrics{
			decode("decode-0", 970_475, 36),
			decode("decode-1", 1_039_474, 81),
			prefill(),
		}, states)
		in.ArrivalRate = runLambda
		var result *domain.AnalyzerResult
		for i := 0; i < 4; i++ { // the four cycles the run showed it for
			var err error
			result, err = analyzer.Analyze(ctx, in)
			Expect(err).NotTo(HaveOccurred())
		}

		Expect(prefillP(result)).To(Equal(prefillK1),
			"prefill stays memory-bound: 357 800 resident tokens while decode is full is decode's backlog, not prefill's capacity")
		Expect(analyzer.computeCapacityHistory).NotTo(HaveKey(prefillKey), "no k2 history for prefill")
		Expect(analyzer.saturatedThroughput).NotTo(HaveKey(prefillKey), "no mu for prefill")
		// Its demand -- 357 800 held plus 30 x 6000 queued at input-only
		// footprint, 537 800, as for any role without a mu -- is 58 % of the
		// one replica: under the 0.70 release boundary, so without the hold
		// a two-replica prefill fleet would have been cut to one here.
		Expect(result.RoleDemand[domain.RolePrefill]).To(BeNumerically("~", 0.7*prefillK1, 1),
			"held at the release boundary: prefill is neither ordered nor released on decode's backlog")

		By("recording decode's saturation as decode's, in the same cycles")
		Expect(analyzer.computeCapacityHistory).To(HaveKey(decodeKey))
		Expect(analyzer.saturatedThroughput).To(HaveKey(decodeKey))
		Expect(analyzer.saturatedThroughput[decodeKey].Max()).To(Equal(4.73))
	})

	It("records prefill's saturation once decode is not saturated", func() {
		// The same prefill row on a cycle decode is keeping up in: prefill
		// is the bottleneck now, and its reading is its own.
		in := makeAnalyzerInput([]domain.ReplicaMetrics{
			decode("decode-0", 300_000, 0),
			decode("decode-1", 280_000, 2),
			prefill(),
		}, states)
		in.ArrivalRate = runLambda
		result, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())

		Expect(prefillP(result)).To(Equal(357_800.0))
		Expect(analyzer.computeCapacityHistory).To(HaveKey(prefillKey))
		Expect(analyzer.saturatedThroughput).To(HaveKey(prefillKey))
		Expect(analyzer.saturatedThroughput[prefillKey].Max()).To(Equal(4.77))
	})

	It("holds prefill in the band where the engine neither orders nor releases", func() {
		// The reviewer's arithmetic: a prefill k2 learned on a genuine
		// saturation is small, as a compute bound is (one step's prompts
		// plus the hand-off). Against 80 000, the 537 800 that decode's
		// backlog puts on prefill is RC = 537 800 / 0.85 - 80 000 = 552 706,
		// seven extra prefill replicas -- ordered on every decode saturation
		// of a fleet whose prefill was ever the bottleneck, and released
		// through the scale-down window once decode recovers.
		seedP := makeReplicaMetrics("prefill-0", prefillVariant, 80_000, prefillKv, 30, 6000, 1)
		seedP.RequestRate, seedP.Ready = 30, true
		_, err := analyzer.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{
			decode("decode-0", 300_000, 0), decode("decode-1", 280_000, 0), seedP}, states))
		Expect(err).NotTo(HaveOccurred())

		By("capping the demand at the scale-up threshold of the anticipated supply")
		result, err := analyzer.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{
			decode("decode-0", 970_475, 36), decode("decode-1", 1_039_474, 81), prefill()}, states))
		Expect(err).NotTo(HaveOccurred())
		Expect(prefillP(result)).To(Equal(80_000.0), "P2-hist")
		Expect(result.RoleDemand[domain.RolePrefill]).To(BeNumerically("~", 0.85*80_000, 1),
			"RC = 0: the held KV orders nothing")

		By("flooring it at the release boundary of the supply, on a fleet of two")
		two := []domain.VariantReplicaState{states[0],
			{VariantName: prefillVariant, Role: domain.RolePrefill, AcceleratorName: "H200", CurrentReplicas: 2, GPUsPerReplica: 1}}
		p1 := prefill()
		p1.PodName = "prefill-1"
		p1.TokensInUse, p1.QueueLength = 0, 0
		fresh := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		fresh.now = analyzer.now
		result, err = fresh.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{
			decode("decode-0", 970_475, 36), decode("decode-1", 1_039_474, 81), prefill(), p1}, two))
		Expect(err).NotTo(HaveOccurred())
		// 537 800 on a supply of 2 x 919 449 = 1 838 898 is 29 %: SC would
		// have been 1 838 898 - 537 800 / 0.7 = 1 070 612, one replica removed while
		// decode is saturated, then re-ordered when it recovers.
		Expect(result.RoleDemand[domain.RolePrefill]).To(BeNumerically("~", 0.7*2*prefillK1, 1),
			"SC = 0: nothing is released on decode's backlog")
		Expect(result.TotalDemand).To(BeNumerically("~",
			result.RoleDemand[domain.RoleDecode]+result.RoleDemand[domain.RolePrefill], 1),
			"the model-level total moved with the role")

		By("leaving prefill alone the cycle decode is not saturated")
		// The same held KV, no queue behind it, decode keeping up: the
		// measured demand stands, and with it the release.
		forget()
		p0 := prefill()
		p0.QueueLength = 0
		result, err = fresh.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{
			decode("decode-0", 300_000, 0), decode("decode-1", 280_000, 2), p0, p1}, two))
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RoleDemand[domain.RolePrefill]).To(BeNumerically("~", 357_800, 1))
		Expect(result.RoleDemand[domain.RolePrefill]).To(BeNumerically("<", 0.7*2*prefillK1))
	})

	It("holds prefill against a floor that may order, not only against occupancy", func() {
		// Two genuine prefill saturations (decode keeping up) give prefill a
		// mu the floor may ORDER on (MinThroughputSamplesToOrder), so the
		// floor is no longer self-capped at the fleet's size. A mu of 4 at
		// 6 req/s offered is 1.5 replicas of one; against the one replica's
		// P that is an order every cycle -- and this is the shape a mu
		// learned under decode's metering has, below the offered rate.
		for i := 0; i < MinThroughputSamplesToOrder; i++ {
			slow := prefill()
			slow.RequestRate = 4
			in := makeAnalyzerInput([]domain.ReplicaMetrics{
				decode("decode-0", 300_000, 0), decode("decode-1", 280_000, 0), slow}, states)
			in.ArrivalRate = runLambda
			_, err := analyzer.Analyze(ctx, in)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(analyzer.saturatedThroughput[prefillKey].Len()).To(Equal(MinThroughputSamplesToOrder))

		By("the floor ordering on it while decode is not saturated")
		idle := prefill()
		idle.QueueLength, idle.TokensInUse = 0, 10_000
		in := makeAnalyzerInput([]domain.ReplicaMetrics{
			decode("decode-0", 300_000, 0), decode("decode-1", 280_000, 0), idle}, states)
		in.ArrivalRate = runLambda
		result, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		p := prefillP(result)
		Expect(result.RoleDemand[domain.RolePrefill]).To(BeNumerically("~", runLambda/4*p, 1),
			"lambda / mu x P, uncapped: 1.5 replicas of demand on a fleet of one")
		Expect(result.RoleDemand[domain.RolePrefill]).To(BeNumerically(">", 0.85*p))

		By("and held the cycle decode is saturated, floor and all")
		in = makeAnalyzerInput([]domain.ReplicaMetrics{
			decode("decode-0", 970_475, 36), decode("decode-1", 1_039_474, 81), idle}, states)
		in.ArrivalRate = runLambda
		result, err = analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RoleDemand[domain.RolePrefill]).To(BeNumerically("~", 0.85*p, 1),
			"the floor's order is capped at the scale-up threshold of the anticipated supply")
	})

	It("keeps decode saturated for the row window after its last full, queued reading", func() {
		// The run's fourth cycle: decode's occupancy had dropped under k1
		// (589k and 825k against 929 792) while its queues (65, 81) and
		// prefill's row -- a one-minute max, repeated to the token -- had
		// not moved. Without the memory that row records.
		full := []domain.ReplicaMetrics{decode("decode-0", 970_475, 36), decode("decode-1", 1_039_474, 81), prefill()}
		for i := 0; i < 3; i++ {
			_, err := analyzer.Analyze(ctx, makeAnalyzerInput(full, states))
			Expect(err).NotTo(HaveOccurred())
			clock = clock.Add(15 * time.Second)
		}
		drained := []domain.ReplicaMetrics{decode("decode-0", 824_667, 65), decode("decode-1", 589_249, 81), prefill()}
		_, err := analyzer.Analyze(ctx, makeAnalyzerInput(drained, states))
		Expect(err).NotTo(HaveOccurred())
		Expect(analyzer.computeCapacityHistory).NotTo(HaveKey(prefillKey), "the stale row is still decode's")
		Expect(analyzer.saturatedThroughput).NotTo(HaveKey(prefillKey))

		By("and letting the same row record once the window has passed")
		forget()
		_, err = analyzer.Analyze(ctx, makeAnalyzerInput(drained, states))
		Expect(err).NotTo(HaveOccurred())
		Expect(analyzer.computeCapacityHistory).To(HaveKey(prefillKey))
	})

	It("forgets at exactly the row window, and not a moment before", func() {
		full := []domain.ReplicaMetrics{decode("decode-0", 970_475, 36), decode("decode-1", 1_039_474, 81), prefill()}
		_, err := analyzer.Analyze(ctx, makeAnalyzerInput(full, states))
		Expect(err).NotTo(HaveOccurred())
		last := clock
		drained := []domain.ReplicaMetrics{decode("decode-0", 300_000, 0), decode("decode-1", 280_000, 0), prefill()}

		clock = last.Add(DecodeSaturationMemory - time.Nanosecond)
		_, err = analyzer.Analyze(ctx, makeAnalyzerInput(drained, states))
		Expect(err).NotTo(HaveOccurred())
		Expect(analyzer.computeCapacityHistory).NotTo(HaveKey(prefillKey), "inside the window, still decode's")

		clock = last.Add(DecodeSaturationMemory)
		_, err = analyzer.Analyze(ctx, makeAnalyzerInput(drained, states))
		Expect(err).NotTo(HaveOccurred())
		Expect(analyzer.computeCapacityHistory).To(HaveKey(prefillKey), "a full window later, prefill's own")
	})

	It("remembers per model: one model's saturated decode says nothing about another's prefill", func() {
		full := []domain.ReplicaMetrics{decode("decode-0", 970_475, 36), decode("decode-1", 1_039_474, 81), prefill()}
		_, err := analyzer.Analyze(ctx, makeAnalyzerInput(full, states))
		Expect(err).NotTo(HaveOccurred())

		// The same rows under another model and namespace, decode idle: its
		// prefill records, and its demand is not held.
		other := makeAnalyzerInput([]domain.ReplicaMetrics{
			decode("decode-0", 300_000, 0), decode("decode-1", 280_000, 0), prefill()}, states)
		other.ModelID, other.Namespace = "other-model", "other-ns"
		result, err := analyzer.Analyze(ctx, other)
		Expect(err).NotTo(HaveOccurred())
		Expect(analyzer.computeCapacityHistory).To(HaveKey("other-model|H200|1|prefill|short|q5"))
		Expect(analyzer.computeCapacityHistory).NotTo(HaveKey(prefillKey), "the first model is still gated")
		Expect(analyzer.decodeSaturatedAt).To(HaveLen(1))
		Expect(analyzer.decodeSaturatedAt).To(HaveKey("test-ns|test-model"))
		Expect(result.RoleDemand[domain.RolePrefill]).To(BeNumerically(">", 0.85*prefillP(result)), "not held")
	})

	It("ages the memory out with the history it sits beside", func() {
		full := []domain.ReplicaMetrics{decode("decode-0", 970_475, 36), decode("decode-1", 1_039_474, 81), prefill()}
		_, err := analyzer.Analyze(ctx, makeAnalyzerInput(full, states))
		Expect(err).NotTo(HaveOccurred())
		Expect(analyzer.decodeSaturatedAt).To(HaveLen(1))
		// EvictStaleHistory reads the wall clock, as the history does; an
		// entry written under the test clock (2026-09-18) is older than an
		// hour of wall time and younger than a century of it.
		analyzer.EvictStaleHistory(100 * 365 * 24 * time.Hour)
		Expect(analyzer.decodeSaturatedAt).To(HaveLen(1), "a fresh entry survives")
		analyzer.EvictStaleHistory(time.Hour)
		Expect(analyzer.decodeSaturatedAt).To(BeEmpty(), "a stale one is swept")
	})

	It("does not read a decode queue that is only KV transfers in flight as saturation", func() {
		// vLLM counts a request waiting for its remote KV in
		// num_requests_waiting: a large model over a slow link keeps five or
		// more there at all times while decode admits fine. Prefill's
		// reading is its own then -- and, with the hold on its demand,
		// prefill could otherwise never be ordered on such a fleet.
		inFlight := []domain.ReplicaMetrics{decode("decode-0", 300_000, 8), decode("decode-1", 280_000, 6), prefill()}
		result, err := analyzer.Analyze(ctx, makeAnalyzerInput(inFlight, states))
		Expect(err).NotTo(HaveOccurred())
		Expect(analyzer.computeCapacityHistory).To(HaveKey(prefillKey))
		Expect(analyzer.saturatedThroughput).To(HaveKey(prefillKey))
		// Priced by its own reading: k2 = 357 800, the queue as a backlog
		// against its mu, the resident KV standing -- 100 % of the replica,
		// above the band's cap of 85 %, so an order, and not held.
		Expect(result.RoleDemand[domain.RolePrefill]).To(BeNumerically("~", 357_800, 1), "not held")
		Expect(result.RoleDemand[domain.RolePrefill]).To(BeNumerically(">", 0.85*prefillP(result)))
	})

	It("shares the held figure out across a role's variants by their size", func() {
		// Two prefill variants: the one whose replica shows decode's backlog
		// (357 800 held, 30 queued) and one idling on a genuine reading. The
		// optimizer prices each variant by its share of the role demand,
		// read off the variants' own TotalDemand, and the sticky scale-down
		// tests a release against that -- so the held figure has to reach
		// the variants, and by size, not by who happened to show the backlog.
		idle := makeReplicaMetrics("prefill-1", "prefill-w", 20_000, prefillKv, 0, 6000, 1)
		idle.Ready = true
		two := []domain.VariantReplicaState{states[0], states[1],
			{VariantName: "prefill-w", Role: domain.RolePrefill, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1}}
		result, err := analyzer.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{
			decode("decode-0", 970_475, 36), decode("decode-1", 1_039_474, 81), prefill(), idle}, two))
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RoleDemand[domain.RolePrefill]).To(BeNumerically("~", 0.7*2*prefillK1, 1), "the role, at the band floor")
		var byName = map[string]domain.VariantCapacity{}
		for _, vc := range result.VariantCapacities {
			byName[vc.VariantName] = vc
		}
		Expect(byName[prefillVariant].TotalDemand).To(BeNumerically("~", 0.7*prefillK1, 1), "half each: same size")
		Expect(byName["prefill-w"].TotalDemand).To(BeNumerically("~", 0.7*prefillK1, 1))
		Expect(byName[prefillVariant].Utilization).To(BeNumerically("~", 0.7, 0.001))
		Expect(byName["prefill-w"].Utilization).To(BeNumerically("~", 0.7, 0.001))
		Expect(byName[decodeVariant].TotalDemand).To(BeNumerically(">", 2_000_000), "decode's own figures untouched")
	})

	It("falls through to prefill's own history once it has one, not to k1", func() {
		// A history seeded on a cycle decode was keeping up in. The gate only
		// declines the fresh reading; the priorities below it still apply,
		// so a gated cycle reads P2-hist -- returning k1 from the gate
		// directly would forget what prefill was measured at.
		seed := makeAnalyzerInput([]domain.ReplicaMetrics{
			decode("decode-0", 300_000, 0),
			decode("decode-1", 280_000, 0),
			prefill(),
		}, states)
		_, err := analyzer.Analyze(ctx, seed)
		Expect(err).NotTo(HaveOccurred())
		Expect(analyzer.computeCapacityHistory).To(HaveKey(prefillKey))

		gated := makeAnalyzerInput([]domain.ReplicaMetrics{
			decode("decode-0", 970_475, 36),
			decode("decode-1", 1_039_474, 81),
			prefill(),
		}, states)
		result, err := analyzer.Analyze(ctx, gated)
		Expect(err).NotTo(HaveOccurred())
		Expect(prefillP(result)).To(Equal(357_800.0), "the seeded history, at P2-hist")
		Expect(analyzer.computeCapacityHistory[prefillKey].Len()).To(Equal(1), "the gated reading was not added to it")
		Expect(analyzer.saturatedThroughput[prefillKey].Len()).To(Equal(1), "nor to the throughput window")
	})

	It("does not gate decode on prefill, nor a non-disaggregated fleet on anything", func() {
		By("a saturated decode records while prefill is saturated too")
		in := makeAnalyzerInput([]domain.ReplicaMetrics{
			decode("decode-0", 970_475, 36),
			prefill(),
		}, states)
		_, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(analyzer.computeCapacityHistory).To(HaveKey(decodeKey))
		Expect(analyzer.computeCapacityHistory).NotTo(HaveKey(prefillKey))

		By("a 'both' replica has no downstream, and records as before")
		both := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		rm := makeReplicaMetrics("both-0", "both-v", 900_000, runKvCapacity, 40, 6000, 1000)
		rm.Ready = true
		_, err = both.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{rm},
			[]domain.VariantReplicaState{{VariantName: "both-v", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1}}))
		Expect(err).NotTo(HaveOccurred())
		Expect(both.computeCapacityHistory).To(HaveKey("test-model|H200|1|both|long|q5"))
	})

	It("decides decode's saturation over every decode row, whatever order the rows come in", func() {
		// The prefill row first, then the saturated decode: the gate must not
		// depend on having seen decode before pricing prefill.
		in := makeAnalyzerInput([]domain.ReplicaMetrics{
			prefill(),
			decode("decode-0", 300_000, 0),
			decode("decode-1", 1_039_474, 81),
		}, states)
		_, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(analyzer.computeCapacityHistory).NotTo(HaveKey(prefillKey))
	})
})

var _ = Describe("holdPrefillDemand", func() {
	variants := []domain.VariantCapacity{
		{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: 2, PendingReplicas: 1, PerReplicaCapacity: 100},
		{VariantName: "d", Role: domain.RoleDecode, ReplicaCount: 1, PerReplicaCapacity: 100},
	}
	It("clamps into [scaleDown x supply, scaleUp x anticipated] and says what it did", func() {
		// supply 200, anticipated 300: the band is [140, 255].
		rd := map[string]float64{domain.RolePrefill: 900, domain.RoleDecode: 50}
		h, held := holdPrefillDemand(rd, variants, 0.85, 0.7)
		Expect(held).To(BeTrue())
		Expect(h).To(Equal(roleHold{before: 900, after: 255, lo: 140, hi: 255}))
		Expect(rd[domain.RolePrefill]).To(Equal(255.0))
		Expect(rd[domain.RoleDecode]).To(Equal(50.0), "another role is not touched")

		rd[domain.RolePrefill] = 10
		h, held = holdPrefillDemand(rd, variants, 0.85, 0.7)
		Expect(held).To(BeTrue())
		Expect(h.after).To(Equal(140.0))

		rd[domain.RolePrefill] = 200
		_, held = holdPrefillDemand(rd, variants, 0.85, 0.7)
		Expect(held).To(BeFalse(), "inside the band: nothing to do")
		Expect(rd[domain.RolePrefill]).To(Equal(200.0))
	})
	It("does nothing for a role with no demand entry, no supply, or no thresholds", func() {
		_, held := holdPrefillDemand(map[string]float64{domain.RoleDecode: 50}, variants, 0.85, 0.7)
		Expect(held).To(BeFalse())
		noSupply := []domain.VariantCapacity{{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: 0, PerReplicaCapacity: 100}}
		_, held = holdPrefillDemand(map[string]float64{domain.RolePrefill: 900}, noSupply, 0.85, 0.7)
		Expect(held).To(BeFalse())
		_, held = holdPrefillDemand(map[string]float64{domain.RolePrefill: 900}, variants, 0, 0.7)
		Expect(held).To(BeFalse())
	})
	It("lets the cap win when the band is empty", func() {
		// An in-flight scale-down: 3 running, target 2 -- anticipated below
		// supply, so 0.7 x 300 = 210 is above 0.85 x 200 = 170.
		down := []domain.VariantCapacity{{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: 3, PendingReplicas: -1, PerReplicaCapacity: 100}}
		rd := map[string]float64{domain.RolePrefill: 900}
		h, held := holdPrefillDemand(rd, down, 0.85, 0.7)
		Expect(held).To(BeTrue())
		Expect(h.after).To(Equal(170.0), "never an order")
		rd[domain.RolePrefill] = 10
		h, held = holdPrefillDemand(rd, down, 0.85, 0.7)
		Expect(held).To(BeFalse(), "and no floor to raise to, so the release stands")
		Expect(h.after).To(Equal(10.0))
	})
})

var _ = Describe("roleSaturated", func() {
	roles := map[string]string{"d": domain.RoleDecode, "p": domain.RolePrefill, "": ""}
	const cache = int64(1_162_240) // k1 at 0.8 = 929 792
	row := func(variant string, queue int, tokens int64) []domain.ReplicaMetrics {
		return []domain.ReplicaMetrics{{VariantName: variant, QueueLength: queue, TokensInUse: tokens, TotalKvCapacityTokens: cache}}
	}
	It("is full AND queued: the P1-obs admission test with the KV at its bound", func() {
		Expect(roleSaturated(row("d", 5, 929_792), roles, domain.RoleDecode, 5, 0.8)).To(BeTrue(), "at the threshold, at k1")
		Expect(roleSaturated(row("d", 4, 1_100_000), roles, domain.RoleDecode, 5, 0.8)).To(BeFalse(), "under the queue threshold")
		Expect(roleSaturated(row("d", 50, 929_791), roles, domain.RoleDecode, 5, 0.8)).To(BeFalse(),
			"queued but under k1: a decode that is admitting -- the queue a slow KV transfer keeps looks like this")
		Expect(roleSaturated(row("d", 50, 0), roles, domain.RoleDecode, 5, 0.8)).To(BeFalse(), "a queue on a replica holding nothing")
		Expect(roleSaturated(row("d", 50, 2_000_000), roles, domain.RoleDecode, 5, 0.8)).To(BeFalse(),
			"resident tokens above the physical ceiling are the scrape artifact P1 discards as invalid")
		Expect(roleSaturated(row("d", 50, 1_100_000), roles, domain.RoleDecode, 5, 0.8)).To(BeTrue(),
			"between k1 and the ceiling is the most informative reading there is")
		Expect(roleSaturated([]domain.ReplicaMetrics{{VariantName: "d", QueueLength: 50, TokensInUse: 1_000_000}},
			roles, domain.RoleDecode, 5, 0.8)).To(BeFalse(), "no cache size: cannot be judged, does not count")
		Expect(roleSaturated(row("p", 50, 1_100_000), roles, domain.RoleDecode, 5, 0.8)).To(BeFalse(), "another role's saturation is not this one's")
		Expect(roleSaturated(row("", 50, 1_100_000), roles, domain.RoleBoth, 5, 0.8)).To(BeTrue(), "an empty role is 'both'")
		bridge := row("d", 50, 1_100_000)
		bridge[0].FromWarmPool = true
		Expect(roleSaturated(bridge, roles, domain.RoleDecode, 5, 0.8)).To(BeTrue(), "a full, queued bridge lent to decode is decode saturated")
		Expect(roleSaturated(nil, roles, domain.RoleDecode, 5, 0.8)).To(BeFalse())
	})
})

// The gate's log line joins the k2-decision contract: dump_k2_decisions.py
// reads the priority field, and a reader of the run has to be able to find
// why a saturated prefill replica stayed at P4-k1.
func TestLogContract_PrefillSaturationUnderDecodeIsLabelled(t *testing.T) {
	ctx, logs := observedCtx(t)
	analyzer := NewSaturationAnalyzer(NewCapacityKnowledgeStore())

	prefill := makeReplicaMetrics("pod-p", "variant-p", 357_800, 1_149_312, 30, 6000, 1)
	decode := makeReplicaMetrics("pod-d", "variant-d", 1_039_474, 1_162_240, 81, 6000, 1000)
	prefill.Ready, decode.Ready = true, true
	_, err := analyzer.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{prefill, decode},
		[]domain.VariantReplicaState{
			{VariantName: "variant-p", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RolePrefill},
			{VariantName: "variant-d", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RoleDecode},
		}))
	require.NoError(t, err)

	var seen []string
	for _, e := range logs.FilterMessage("k2-decision").All() {
		f := e.ContextMap()
		if f["variant"] != "variant-p" {
			continue
		}
		seen = append(seen, stringField(t, f, "priority"))
		if f["priority"] == k2ReasonObsDownstream {
			for _, key := range logContract["k2-decision"] {
				assert.Contains(t, f, key)
			}
			assert.Contains(t, stringField(t, f, "reason"), "decode")
			assert.EqualValues(t, 30, f["queueLength"])
		}
	}
	assert.Equal(t, []string{k2ReasonObsDownstream, "P4-k1"}, seen,
		"the gate line, then the fallback the analyzer fell through to")
	assert.Equal(t, "P1-obs", func() string {
		for _, e := range logs.FilterMessage("k2-decision").All() {
			if f := e.ContextMap(); f["variant"] == "variant-d" {
				return stringField(t, f, "priority")
			}
		}
		return ""
	}(), "decode's own saturation is recorded in the same cycle")

	// And the hold on prefill's demand, which changes the decision, says
	// what it found and what it left.
	held := requireLogged(t, logs, "prefill-demand-held")
	assert.InDelta(t, 357_800+30*6000, held["demandBefore"], 1)
	assert.InDelta(t, 0.7*919_449, held["demandHeld"], 1)
	assert.InDelta(t, 0.7*919_449, held["holdFloor"], 1)
	assert.InDelta(t, 0.85*919_449, held["holdCap"], 1)
}
