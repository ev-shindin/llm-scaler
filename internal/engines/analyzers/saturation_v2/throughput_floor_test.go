package saturation_v2

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/floor"
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

// A second run, the other direction: biran-20260921-235843-225 (2026-09-21),
// same cluster and model, 1000/6000 for 1100s then 8000/1000 for 1100s. Its
// figures are not the ones above and must not be read as a phase of them.
const (
	// shortenSwapLambda is the arrival rate measured over that run's second
	// phase, where the floor divided it by a mu of 0.039928 and asked for
	// 142 replicas.
	shortenSwapLambda = 5.68
)

var _ = Describe("the saturated-throughput window", func() {
	It("keeps the max, because a saturated completion rate under-reads while the replica is full", func() {
		// The readings a saturated decode replica produced on the run, in
		// order: 5.4 (a full minute saturated), then 3.3 and 3.5 under KV
		// pressure with preemptions, each a rate window apart so each is
		// a sample (inside the spacing the fold would make them one).
		//
		// The window reads its MEDIAN, and this spec used to assert its max.
		// That is a deliberate reversal, and the reason is that what the window
		// holds changed: a saturated COMPLETION rate errs one way -- it
		// under-reads while a replica is filling -- so the largest reading was
		// the best estimate of what the replica sustains. A generation-token
		// rate, which is what the window holds now, errs the other way: it
		// bursts. Measured on the 2026-09-22 rerun, phase 1, one shape
		// throughout: the per-replica rate ran 4028 min, 7548 median, 11663
		// max, the fleet's own total sat at 33,175 tokens/s against a demanded
		// 36,000, and read with the max the window ratcheted 0.92 -> 3.60 req/s
		// and left the floor asking for 1.6 replicas where about 8 were needed.
		// The middle reading is the typical replica; the peak is a burst.
		a := NewSaturationAnalyzer(capacity.NewStore())
		now := time.Date(2026, 9, 17, 10, 50, 20, 0, time.UTC)
		a.now = func() time.Time { return now }
		k := "m|H200|1|decode|long|q5"
		for _, rate := range []float64{5.4, 3.3, 3.5} {
			a.recordSaturatedThroughput(k, rate)
			now = now.Add(ThroughputSampleSpacing)
		}
		Expect(a.saturatedThroughput[k].Len()).To(Equal(3))
		Expect(a.saturatedThroughput[k].Average()).To(BeNumerically("~", 4.07, 0.01), "the mean the window does not use")
		Expect(a.saturatedThroughput[k].Max()).To(Equal(5.4), "nor the max it used to")
		mu, bucket := a.saturatedThroughputFor(k)
		Expect(mu).To(Equal(3.5), "the middle reading of 3.3, 3.5, 5.4")
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
		a := NewSaturationAnalyzer(capacity.NewStore())
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
		a := NewSaturationAnalyzer(capacity.NewStore())
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
		a.saturatedThroughput["k"].TouchAt(time.Now().Add(-2 * time.Hour))
		a.recordSaturatedThroughput("k", 3.433333333333333)
		Expect(a.saturatedThroughput["k"].Stale(time.Hour)).To(BeFalse())
		Expect(a.saturatedThroughput["k"].Len()).To(Equal(1))

		By("folding a higher reading inside the spacing into the sample: two replicas, one moment, the max")
		a.recordSaturatedThroughput("k", 3.6)
		Expect(a.saturatedThroughput["k"].Len()).To(Equal(1))
		Expect(a.saturatedThroughput["k"].Max()).To(Equal(3.6), "the sample is the max of its window")
		a.recordSaturatedThroughput("k", 3.5)
		Expect(a.saturatedThroughput["k"].Max()).To(Equal(3.6), "and a lower one leaves it")

		By("not counting the last reading again at the boundary: the same pair straddling it, or a repeat")
		// The last READING was 3.5, not the folded 3.6 the sample now holds.
		now = t0.Add(ThroughputSampleSpacing)
		a.recordSaturatedThroughput("k", 3.5)
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
		a := NewSaturationAnalyzer(capacity.NewStore())
		a.recordSaturatedThroughput("k", 0)
		Expect(a.saturatedThroughput).To(BeEmpty())
		a.recordSaturatedThroughput("k", 2)
		// Age the window explicitly rather than evicting with a zero timeout:
		// a zero timeout asks whether any time at all has passed, which on a
		// coarse clock it may not have.
		a.saturatedThroughput["k"].TouchAt(time.Now().Add(-2 * time.Hour))
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
		a := NewSaturationAnalyzer(capacity.NewStore())
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

// Through Analyze, replaying the run: a decode replica saturates (P1 fires,
// mu is recorded), then the fleet grows to six and occupancy collapses.
var _ = Describe("the throughput floor, through Analyze", func() {
	var (
		analyzer *SaturationAnalyzer
		ctx      context.Context
		clock    time.Time
	)
	BeforeEach(func() {
		analyzer = NewSaturationAnalyzer(capacity.NewStore())
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
		rm.GenerationTokenRate = rm.RequestRate * rm.AvgOutputTokens
		rm.Ready = true
		return rm
	}
	prefill := func(pod string, tokensInUse int64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, "prefill-v", tokensInUse, 1_149_312, 0, 6000, 1)
		rm.RequestRate = runLambda
		rm.GenerationTokenRate = rm.RequestRate * rm.AvgOutputTokens
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
	// The readings straddle runMu so the window's MEDIAN is runMu: with an
	// even count the lower of the two middle values is taken, so the lowest
	// reading fed is the one the specs below divide by. The window read its
	// max until the 2026-09-22 rerun showed a token rate ratcheting on its
	// bursts (RollingAverage.Median).
	// The last step also moves the clock past DecodeSaturationMemory.
	saturate := func() {
		for i := floor.MinThroughputSamplesToOrder - 1; i >= 0; i-- {
			saturateOnce(runMu + 0.01*float64(i))
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
		fresh := NewSaturationAnalyzer(capacity.NewStore())
		bare, err := fresh.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(bare.RoleDemand[domain.RoleDecode]).To(BeNumerically("<", want/5))
	})

	It("prices every replica of a role under the fleet's shape, not each replica's own", func() {
		// Replayed from the 1000/6000 shape-swap trace (2026-09-20, cycles
		// 17:32-17:34). Phase 1, 1000-token outputs: one decode replica
		// saturates twice a window apart at 3.08 -- the `long` bucket's own
		// reading. Phase 2, ~5500-token outputs: two replicas saturate at
		// 1.40 under `xxlong`; three fresh ones have completed a handful of
		// short stragglers each (average 900, `long` by their own reading,
		// at 0.2 req/s). Then the switch's batch drains: no queue, little
		// resident KV, 6 req/s arriving. The floor's mu must be the shape
		// the fleet serves, 1.40 -- four-and-a-bit replicas' worth -- and
		// not the median of three `long` readings and two `xxlong` ones,
		// 3.08, which released the fleet to 3 on the run.
		long := func(pod string, tokens int64, queue int, rate float64) domain.ReplicaMetrics {
			rm := makeReplicaMetrics(pod, decodeVariant, tokens, runKvCapacity, queue, 1000, 900)
			rm.RequestRate = rate
			rm.GenerationTokenRate = rm.RequestRate * rm.AvgOutputTokens
			rm.Ready = true
			return rm
		}
		xxlong := func(pod string, tokens int64, queue int, rate float64) domain.ReplicaMetrics {
			rm := makeReplicaMetrics(pod, decodeVariant, tokens, runKvCapacity, queue, 1000, 5500)
			rm.RequestRate = rate
			rm.GenerationTokenRate = rm.RequestRate * rm.AvgOutputTokens
			rm.Ready = true
			return rm
		}
		cycle := func(rms []domain.ReplicaMetrics, decodeN int) *domain.AnalyzerResult {
			in := makeAnalyzerInput(append(rms, prefill("prefill-0", 66_183)), states(decodeN, 1))
			in.ArrivalRate = runLambda
			out, err := analyzer.Analyze(ctx, in)
			Expect(err).NotTo(HaveOccurred())
			return out
		}
		// phase 1: `long` learns 3.08 over two spaced samples
		for _, rate := range []float64{3.07, 3.08} {
			cycle([]domain.ReplicaMetrics{long("d0", 1_158_912, 10, rate)}, 1)
			clock = clock.Add(ThroughputSampleSpacing + time.Second)
		}
		// phase 2: `xxlong` learns 1.40 over two spaced samples, with three
		// fresh replicas beside the saturated pair
		for _, rate := range []float64{1.39, 1.40} {
			cycle([]domain.ReplicaMetrics{
				xxlong("d0", 1_158_912, 10, rate), xxlong("d1", 1_158_912, 10, rate),
				long("d2", 30_000, 0, 0.2), long("d3", 30_000, 0, 0.2), long("d4", 30_000, 0, 0.2),
			}, 5)
			clock = clock.Add(ThroughputSampleSpacing + time.Second)
		}
		// the drain: nothing queued, little resident, 6 req/s still arriving
		result := cycle([]domain.ReplicaMetrics{
			xxlong("d0", 30_000, 0, 1.2), xxlong("d1", 30_000, 0, 1.2),
			long("d2", 30_000, 0, 0.2), long("d3", 30_000, 0, 0.2), long("d4", 30_000, 0, 0.2),
		}, 5)
		var decodeP float64
		for _, vc := range result.VariantCapacities {
			if vc.VariantName == decodeVariant {
				decodeP = vc.PerReplicaCapacity
			}
		}
		Expect(decodeP).To(BeNumerically(">", 0))
		// The figure is read off the window the analyzer actually holds rather
		// than re-derived here. A hand derivation was tried and was wrong in
		// two ways at once, each too small for a loose tolerance to catch:
		// mu is priced over the TRACKED shape, which is frozen until the
		// tracker reports a change, not the raw per-cycle average; and the
		// median of an even-sized window is the LOWER of the two middle
		// values, not the later one. Asserting against the window makes the
		// spec's arithmetic the code's arithmetic by construction.
		// Named by bucket, not by whichever decode key map iteration yields
		// last: the fleet has more than one window open here and Go randomises
		// that order, so the earlier form asserted against a different figure
		// from run to run.
		var mu float64
		for key, window := range analyzer.saturatedThroughput {
			if strings.Contains(key, "|"+domain.RoleDecode+"|xxlong|") {
				mu = window.Median()
			}
		}
		Expect(mu).To(BeNumerically(">", 0), "the fleet's own shape must have a window")
		Expect(result.RoleDemand[domain.RoleDecode]/decodeP).To(BeNumerically("~", runLambda/mu, 0.01),
			"the floor is lambda over the mu the fleet's own shape priced")

		// And the claim this spec exists for: that mu belongs to the shape the
		// FLEET is serving, ~5500-token outputs, not to the `long` bucket the
		// three fresh replicas' own recent completions would have chosen. The
		// two are far apart -- the wrong one would price the drained fleet at
		// about two replicas and release it.
		Expect(mu).To(BeNumerically("<", 2.5),
			"a mu from the fleet's 5500-token shape, not the 3.08 the 900-token one carried")
		Expect(result.RoleDemand[domain.RoleDecode]/decodeP).To(BeNumerically(">", 3),
			"so the floor holds more than the two replicas the wrong bucket implies")
	})

	It("does not order on one saturated sample, however many cycles the row shows it", func() {
		// The measured failure: one decode replica's saturated sample --
		// 1 150 207 resident, queue 20, 3.43 req/s against a true ~5.4 --
		// re-read on four consecutive cycles. Counted four times it cleared
		// MinThroughputSamplesToOrder, and the floor ordered on lambda / 3.43
		// = 1.75 replicas' worth. One sample holds; the second, a window on,
		// may order.
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
		// held at what it has.
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
		Expect(result.RoleDemand[domain.RoleDecode]).To(BeNumerically("~", 0.85*decodeP, 1),
			"capped at scaleUp x the one replica: RC = 0, nothing ordered")

		By("ordering once a second reading of its own is on record, a rate window later")
		clock = clock.Add(ThroughputSampleSpacing)
		sat := makeAnalyzerInput(
			[]domain.ReplicaMetrics{decode("decode-0", 1_150_207, 20, 3.5), prefill("prefill-0", 66_183)},
			states(1, 1))
		sat.ArrivalRate = runLambda
		_, err = analyzer.Analyze(ctx, sat)
		Expect(err).NotTo(HaveOccurred())
		result, err = analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		// The window holds the two replayed readings, 103/30 and 105/30, and
		// reads their median -- the lower of two middle values -- so the floor
		// divides by 3.4333 where it divided by the max, 3.5. The claim this
		// spec makes is the sample COUNT, not the figure: one reading holds the
		// fleet, a second orders.
		Expect(result.RoleDemand[domain.RoleDecode]).To(BeNumerically("~", runLambda/3.433333333333333*decodeP, 1),
			"lambda / mu, uncapped: 1.74 replicas' worth, the second replica through the headroom")
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
			"lambda / mu = 1.67 replicas' worth on two: under the hold at 0.85 x two, and RC = 0 on a fleet of two")

		By("and the next window, a minute on, is the second reading")
		clock = clock.Add(ThroughputSampleSpacing)
		_, err = analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(analyzer.saturatedThroughputReading("test-model|H200|1|decode|long|q5").samples).To(Equal(2))
	})

	It("holds a fleet with a replica on its way at that replica, on one reading", func() {
		// Run 11's phase switch, +1434 s: two decode replicas full and queued
		// at the new shape (804 185 / q17 at 2.23 req/s, 791 127 / q11 at
		// 2.27), a third ordered by occupancy a minute earlier and still
		// starting, 6.2 req/s arriving, 28 queued. One reading; lambda / mu
		// = 2.96 replicas' worth. Letting one reading order one replica
		// ("anticipated plus one") made that four, a replica the run never
		// needed; a hold at the anticipated three is a hold.
		row := func(pod string, tokens int64, queue int, rate float64) domain.ReplicaMetrics {
			rm := makeReplicaMetrics(pod, decodeVariant, tokens, runKvCapacity, queue, 1000, 4000)
			rm.RequestRate = rate
			rm.GenerationTokenRate = rm.RequestRate * rm.AvgOutputTokens
			rm.Ready = true
			return rm
		}
		st := []domain.VariantReplicaState{
			{VariantName: decodeVariant, Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 3, GPUsPerReplica: 1},
			{VariantName: "prefill-v", Role: domain.RolePrefill, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1},
		}
		in := makeAnalyzerInput([]domain.ReplicaMetrics{
			row("decode-0", 804_185, 17, 2.233333333333334), row("decode-1", 791_127, 11, 2.2666666666666666), prefill("prefill-0", 0)}, st)
		in.ArrivalRate = 6.2
		result, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		var decodeP float64
		var pending int
		for _, vc := range result.VariantCapacities {
			if vc.VariantName == decodeVariant {
				decodeP, pending = vc.PerReplicaCapacity, vc.PendingReplicas
			}
		}
		Expect(pending).To(Equal(1), "the third is on its way")
		Expect(analyzer.saturatedThroughputReading("test-model|H200|1|decode|xxlong|q5").samples).To(Equal(1))
		Expect(result.RoleDemand[domain.RoleDecode]).To(BeNumerically("~", 0.85*3*decodeP, 1),
			"held at the anticipated three: RC = 0, no fourth on one reading")
	})

	It("orders the second replica from lambda / mu on a fleet of one", func() {
		// After ONE saturated cycle the window holds a single reading and the
		// floor holds the fleet where it is (RC = 0 exactly); after the second
		// -- a reading of its own, a rate window later -- it orders.
		// Straddles runMu so the pair's median is runMu (RollingAverage.Median).
		saturateOnce(runMu + 0.01)
		in0 := makeAnalyzerInput(
			[]domain.ReplicaMetrics{decode("decode-0", 200_000, 0, runLambda), prefill("prefill-0", 0)},
			states(1, 1))
		in0.ArrivalRate = runLambda
		held, err := analyzer.Analyze(ctx, in0)
		Expect(err).NotTo(HaveOccurred())
		var heldP float64
		for _, vc := range held.VariantCapacities {
			if vc.VariantName == decodeVariant {
				heldP = vc.PerReplicaCapacity
			}
		}
		Expect(held.RoleDemand[domain.RoleDecode]).To(BeNumerically("~", 0.85*heldP, 1),
			"one reading: capped at scaleUp x the one replica, so nothing is ordered")

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
		fresh := NewSaturationAnalyzer(capacity.NewStore())
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
		want := (runLambda + 380.0/floor.BacklogDrainSeconds) / runMu * decodeP
		Expect(result.RoleDemand[domain.RoleDecode]).To(BeNumerically("~", want, 1))
		Expect(result.RoleDemand[domain.RoleDecode] / decodeP).To(BeNumerically("~", 2.28, 0.01))
		Expect(result.TotalDemand).To(BeNumerically("~", result.RoleDemand[domain.RoleDecode]+result.RoleDemand[domain.RolePrefill], 1),
			"the total moved with decode, and prefill's dropped share was never in it")

		// Negative control: with no mu on record the queues are still charged
		// as residency, and the same cycle is priced at more than four
		// replicas. A saturated replica records its mu in the cycle its queue
		// appears, so the control is one whose completion rate is not
		// reported (no rate, no reading) rather than a fresh analyzer.
		fresh := NewSaturationAnalyzer(capacity.NewStore())
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
		satP.GenerationTokenRate = satP.RequestRate * satP.AvgOutputTokens
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
		Expect(result.RoleDemand[domain.RolePrefill]).To(BeNumerically("~", (runLambda+200.0/floor.BacklogDrainSeconds)/30*prefillP, 1),
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
		rcs := []capacity.ReplicaCapacity{{VariantName: decodeVariant, SaturatedThroughput: runMu,
			SaturatedThroughputSamples: floor.MinThroughputSamplesToOrder, QueueLength: 10, LocalQueueDemand: 500_000}}
		vcs := []domain.VariantCapacity{{VariantName: decodeVariant, Role: domain.RoleDecode, ReplicaCount: 1, PerReplicaCapacity: float64(runK1)}}
		roleDemand := map[string]float64{domain.RoleDecode: 100_000}
		in := makeAnalyzerInput(nil, states(1, 1))
		in.ArrivalRate = runLambda
		total := analyzer.applyThroughputFloor(in, in.Config.(*config.ScalingPolicy), rcs, vcs, 100_000, roleDemand, nil, 0, false, GinkgoLogr)
		want := (runLambda + 10.0/floor.BacklogDrainSeconds) / runMu * float64(runK1)
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
			rm.GenerationTokenRate = rm.RequestRate * rm.AvgOutputTokens
			rm.Ready = true
			return rm
		}
		st := []domain.VariantReplicaState{{VariantName: "v", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1}}
		in := makeAnalyzerInput([]domain.ReplicaMetrics{both("a", 1_158_912, 10, runMu)}, st)
		in.ArrivalRate = runLambda
		_, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())

		in.ReplicaMetrics[0].RequestRate = runMu + 0.01 // a second reading, a rate window later
		in.ReplicaMetrics[0].GenerationTokenRate = in.ReplicaMetrics[0].RequestRate * in.ReplicaMetrics[0].AvgOutputTokens
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
		Expect(result.TotalDemand).To(BeNumerically("~", (runLambda+100.0/floor.BacklogDrainSeconds)/runMu*P, 1))
		Expect(result.TotalDemand / P).To(BeNumerically("~", 1.42, 0.01))
	})

	It("bounds the floor when the outputs shorten under a longer prompt", func() {
		// Replayed from the 1000/6000 -> 8000/1000 shape-swap trace
		// (2026-09-21, run biran-20260921-235843-225, cycles 21:19-21:27).
		// The other direction from the spec above, and the one the fleet
		// reads worst: phase 1 generates 6000-token outputs, phase 2 takes
		// 8000-token prompts and generates 1000.
		//
		// On the run the decode replicas kept their own output-length
		// buckets across the switch, and the fleet split: six replicas had
		// completed enough of the new short requests to classify `long`,
		// where the only reading was one fresh replica's first, extrapolated
		// rate window -- 0.039928 req/s -- and four still read `xxlong` at
		// 3.012173. Six of ten put the under-read at the median, and the
		// four `xxlong` replicas' own two-sample window was what let the
		// role order on it: mu = 0.039928 against 5.68 arriving asked for
		// 142 replicas (logged replicasImplied 134-181, demand 134-213 M
		// tokens, utilization 12.98 rising to 27.49). Only maxReplicas = 10
		// and, from 21:27, the single-sample hold stopped it.
		//
		// Priced under the fleet's shape there is one bucket per cycle, the
		// replicas that serve the load fill it, and the floor stays within
		// a replica or two of what the fleet is running.
		phase1 := func(pod string, tokens int64, queue int, rate float64) domain.ReplicaMetrics {
			rm := makeReplicaMetrics(pod, decodeVariant, tokens, runKvCapacity, queue, 1000, 5000)
			rm.RequestRate = rate
			rm.Ready = true
			return rm
		}
		// After the switch a replica's own average slides from the old shape
		// to the new one as its long requests finish: the ones that have
		// turned over read ~900 output tokens, the ones still draining a
		// 6000-token batch read ~5000.
		turned := func(pod string, tokens int64, queue int, rate float64) domain.ReplicaMetrics {
			rm := makeReplicaMetrics(pod, decodeVariant, tokens, runKvCapacity, queue, 8000, 900)
			rm.RequestRate = rate
			rm.Ready = true
			return rm
		}
		draining := func(pod string, tokens int64, queue int, rate float64) domain.ReplicaMetrics {
			rm := makeReplicaMetrics(pod, decodeVariant, tokens, runKvCapacity, queue, 8000, 5000)
			rm.RequestRate = rate
			rm.Ready = true
			return rm
		}
		cycle := func(rms []domain.ReplicaMetrics, decodeN int) *domain.AnalyzerResult {
			in := makeAnalyzerInput(append(rms, prefill("prefill-0", 8_065)), states(decodeN, 1))
			in.ArrivalRate = shortenSwapLambda
			out, err := analyzer.Analyze(ctx, in)
			Expect(err).NotTo(HaveOccurred())
			return out
		}

		// Phase 1, saturated on the 6000-token output shape -- the replicas
		// average ~5000 over their recent completions, which is what buckets
		// (both land in `xxlong`): the fleet's bucket
		// learns 3.012173, the figure the four `xxlong` replicas carried
		// into the switch on the run.
		for _, rate := range []float64{3.01, 3.012173} {
			cycle([]domain.ReplicaMetrics{phase1("d0", 1_158_912, 10, rate)}, 1)
			clock = clock.Add(ThroughputSampleSpacing + time.Second)
		}
		// A replica that has just joined and is queued reports its first,
		// extrapolated rate window -- the 0.039928 that became the whole
		// `long` bucket on the run. Under the fleet's shape it lands in the
		// bucket the fleet is serving, beside the readings of replicas that
		// are keeping up, and the window's max discards it.
		cycle([]domain.ReplicaMetrics{
			turned("d0", 266_654, 10, 0.039928),
			draining("d1", 1_158_912, 10, 3.012173),
		}, 2)
		clock = clock.Add(ThroughputSampleSpacing + time.Second)

		// 21:23:34-21:26:04: ten decode replicas, ~17k resident each, no
		// queue anywhere, 5.68 req/s still arriving. Six have turned over
		// to the short shape, four are still draining the long one.
		rms := make([]domain.ReplicaMetrics, 0, 10)
		for i := 0; i < 6; i++ {
			rms = append(rms, turned(fmt.Sprintf("turned-%d", i), 17_282, 0, 0.61))
		}
		for i := 0; i < 4; i++ {
			rms = append(rms, draining(fmt.Sprintf("draining-%d", i), 17_538, 0, 0.61))
		}
		result := cycle(rms, 10)

		var decodeP float64
		for _, vc := range result.VariantCapacities {
			if vc.VariantName == decodeVariant {
				decodeP = vc.PerReplicaCapacity
			}
		}
		Expect(decodeP).To(BeNumerically(">", 0))
		implied := result.RoleDemand[domain.RoleDecode] / decodeP
		Expect(implied).To(BeNumerically("<", 14),
			"the floor must stay within reach of the ten replicas serving the load; on the run it asked for 142")
		Expect(implied).To(BeNumerically(">", 1),
			"and it is still a floor: 5.68 req/s is not one replica's work")
	})
})
