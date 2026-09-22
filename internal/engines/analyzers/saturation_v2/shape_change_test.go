package saturation_v2

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

var _ = Describe("the two prompt-length sources", func() {
	It("reads the scheduler queue, and says when there is none", func() {
		// Run biran-20260921-235843-225, cycle at 20.7 min: 681 requests
		// queued, 31,071,039 bytes, the trace's 8000-token phase-2 prompt.
		sq := &domain.SchedulerQueueMetrics{QueueSize: 681, QueueBytes: 31_071_039}
		got, ok := arrivingPromptLength(sq)
		Expect(ok).To(BeTrue())
		Expect(got).To(BeNumerically("~", 31_071_039.0/681/BytesPerToken, 1e-6))

		// One phase earlier: 692 requests, 4,039,809 bytes, the 1000-token
		// prompt. The two are 7.8x apart, which is the step that raises the
		// event; the absolute scale never matters because this reading is
		// only ever compared against another of its own.
		was, ok := arrivingPromptLength(&domain.SchedulerQueueMetrics{QueueSize: 692, QueueBytes: 4_039_809})
		Expect(ok).To(BeTrue())
		Expect(got / was).To(BeNumerically("~", 7.8, 0.1))

		By("reporting no reading when the queue is empty, which it is on 141 of the run's 171 cycles")
		_, ok = arrivingPromptLength(nil)
		Expect(ok).To(BeFalse())
		_, ok = arrivingPromptLength(&domain.SchedulerQueueMetrics{})
		Expect(ok).To(BeFalse())
	})

	It("reads the replicas weighted by rate", func() {
		rms := []domain.ReplicaMetrics{
			{VariantName: "d", AvgInputTokens: 8000, RequestRate: 1.0},
			{VariantName: "d", AvgInputTokens: 8000, RequestRate: 1.0},
			{VariantName: "d", AvgInputTokens: 1000, RequestRate: 0.1},
		}
		got := servedPromptLength(rms)
		Expect(got).To(BeNumerically("~", (2*1.0*8000+0.1*1000)/(2*1.0+0.1), 1e-9))
		Expect(got).To(BeNumerically(">", 7000), "the replica still draining the old shape must not drag it down")

		By("the plain mean when no replica reports a rate")
		Expect(servedPromptLength([]domain.ReplicaMetrics{
			{VariantName: "d", AvgInputTokens: 1000}, {VariantName: "d", AvgInputTokens: 3000},
		})).To(Equal(2000.0))

		By("and nothing at all when there is nothing to read")
		Expect(servedPromptLength(nil)).To(Equal(0.0))
	})

	It("ignores a warm-pool bridge, whose prompts are not this variant's", func() {
		rms := []domain.ReplicaMetrics{
			{VariantName: "d", AvgInputTokens: 1000, RequestRate: 1.0},
			{VariantName: "d", AvgInputTokens: 60000, RequestRate: 1.0, FromWarmPool: true},
		}
		Expect(servedPromptLength(rms)).To(Equal(1000.0))
	})
})

var _ = Describe("the fleet-shape change, through Analyze", func() {
	const (
		decodeV  = "decode-v"
		prefillV = "prefill-v"
		kvCap    = int64(1_162_240)
	)
	var (
		analyzer *SaturationAnalyzer
		ctx      context.Context
		clock    time.Time
	)
	BeforeEach(func() {
		analyzer = NewSaturationAnalyzer(capacity.NewStore())
		clock = time.Date(2026, 9, 21, 20, 59, 38, 0, time.UTC)
		analyzer.now = func() time.Time { return clock }
		ctx = context.Background()
	})

	// The hold's state, read the way the production path reads it.
	outstanding := func() bool {
		_, held := analyzer.fleetShapeState("test-ns", "test-model")
		return held
	}
	states := func(decodeN int) []domain.VariantReplicaState {
		return []domain.VariantReplicaState{
			{VariantName: decodeV, Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: decodeN, GPUsPerReplica: 1},
			{VariantName: prefillV, Role: domain.RolePrefill, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1},
		}
	}
	// A cycle of the run: `n` decode replicas each holding `tokens`, the
	// given arriving prompt length, and the shape their completions average.
	cycle := func(n int, tokens int64, queue int, in, out float64, sq *domain.SchedulerQueueMetrics) *domain.AnalyzerResult {
		rms := make([]domain.ReplicaMetrics, 0, n+1)
		for i := 0; i < n; i++ {
			rm := makeReplicaMetrics(fmt.Sprintf("d%d", i), decodeV, tokens, kvCap, queue, in, out)
			rm.RequestRate = 0.6
			rm.Ready = true
			rms = append(rms, rm)
		}
		p := makeReplicaMetrics("p0", prefillV, 8_065, 1_149_312, 0, in, 1)
		p.RequestRate = 6
		p.Ready = true
		rms = append(rms, p)

		input := makeAnalyzerInput(rms, states(n))
		input.ArrivalRate = 5.68
		input.SchedulerQueue = sq
		out2, err := analyzer.Analyze(ctx, input)
		Expect(err).NotTo(HaveOccurred())
		return out2
	}

	It("does not release the fleet on the shape it has just left", func() {
		// Phase 1 of run biran-20260921-235843-225: 1000-token prompts,
		// 6000-token generations, ten decode replicas carrying it.
		cycle(10, 900_000, 0, 1000, 6000, nil)
		clock = clock.Add(15 * time.Second)

		// 18.7 min: the trace switched at 18.3 and the queue is the first
		// thing to say so -- 20 requests, 912,037 bytes, 7811 tokens each.
		// The generations in flight are still the old shape's; only the
		// prompt has moved, which is exactly the I-up case the proposal
		// names as the one the analyzer reads late.
		swap := cycle(10, 900_000, 0, 8000, 6000,
			&domain.SchedulerQueueMetrics{QueueSize: 20, QueueBytes: 912_037})
		clock = clock.Add(15 * time.Second)

		// The switch's batch drains: the 6000-token generations finish, the
		// 1000-token ones that replace them hold almost nothing, and
		// occupancy collapses. On the run the fleet went to 8-21 running
		// requests across 11 replicas. Occupancy alone would release here.
		drained := cycle(10, 17_282, 0, 8000, 1000, nil)

		var decodeSupply float64
		for _, vc := range drained.VariantCapacities {
			if vc.VariantName == decodeV {
				decodeSupply = float64(vc.ReplicaCount) * vc.PerReplicaCapacity
			}
		}
		Expect(decodeSupply).To(BeNumerically(">", 0))
		// scaleDown is 0.70 in makeAnalyzerInput: the bottom of the band
		// where the engine neither orders nor releases.
		Expect(drained.RoleDemand[domain.RoleDecode]).To(BeNumerically(">=", 0.70*decodeSupply),
			"a fleet whose shape has just changed is not released on the old shape's occupancy")
		Expect(swap).NotTo(BeNil())
	})

	It("releases the fleet once the new shape has a reading of its own", func() {
		// The same three cycles, then the fleet saturates under the new
		// shape: a replica full and queued records a throughput window of
		// its OWN, which is what the hold was waiting for.
		cycle(10, 900_000, 0, 1000, 6000, nil)
		clock = clock.Add(15 * time.Second)
		cycle(10, 900_000, 0, 8000, 6000, &domain.SchedulerQueueMetrics{QueueSize: 20, QueueBytes: 912_037})
		clock = clock.Add(15 * time.Second)
		Expect(outstanding()).To(BeTrue())

		// Two saturated cycles a window apart under the new shape.
		for _, rate := range []float64{5.3, 5.4} {
			rms := makeReplicaMetrics("d0", decodeV, 1_100_000, kvCap, 10, 8000, 1000)
			rms.RequestRate = rate
			rms.Ready = true
			input := makeAnalyzerInput([]domain.ReplicaMetrics{rms}, states(1))
			input.ArrivalRate = 5.68
			_, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			clock = clock.Add(ThroughputSampleSpacing + time.Second)
		}
		Expect(outstanding()).To(BeFalse(),
			"a replica reading its own window under the new shape settles the hold")
	})

	It("releases the fleet after the backstop when it never saturates again", func() {
		cycle(10, 900_000, 0, 1000, 6000, nil)
		clock = clock.Add(15 * time.Second)
		cycle(10, 900_000, 0, 8000, 6000, &domain.SchedulerQueueMetrics{QueueSize: 20, QueueBytes: 912_037})
		Expect(outstanding()).To(BeTrue())

		// The generations turn over to the new shape a minute later, which is
		// a second change on the output axis -- the I-up event and the O-down
		// event are distinct on this trace, and the hold spans both.
		clock = clock.Add(time.Minute)
		cycle(10, 17_282, 0, 8000, 1000, nil)
		Expect(outstanding()).To(BeTrue())

		// The run's phase 2 from here: idle at eleven replicas, nothing
		// queued, the shape steady, so no window is ever recorded under it.
		// Without the backstop the hold would stand for the rest of the run.
		clock = clock.Add(ShapeChangeHoldMax + time.Second)
		cycle(10, 17_282, 0, 8000, 1000, nil)
		Expect(outstanding()).To(BeFalse(),
			"ShapeChangeHoldMax releases a fleet that will never measure itself")
	})

	It("keeps one throughput window when the shape sits on a bucket boundary", func() {
		// Measured on the rerun of 2026-09-22 (biran-pd, the build carrying
		// #85): phase 1 generates exactly 6000-token outputs, which is the
		// xxlong/huge boundary, so the fleet's rate-weighted average wobbled
		// either side of it. Every replica shares one throughput key since
		// #85, so the wobble moved all of them together -- the bucket read
		// `huge` at 2.36 req/s on one cycle and `xxlong` at 0.82 on the next,
		// and the decode target went 10 -> 4 -> 10 -> 4 every 30-45 s under a
		// shape that never actually changed.
		//
		// The keys follow the TRACKED shape, which does not move until the
		// tolerance is exceeded, so a wobble of a few tokens keeps one window.
		for i, out := range []float64{6000, 5990, 6010, 5995, 6005, 6000} {
			rm := makeReplicaMetrics("d0", decodeV, 1_100_000, kvCap, 10, 1000, out)
			rm.RequestRate = 5.4 - 0.01*float64(i)
			rm.Ready = true
			input := makeAnalyzerInput([]domain.ReplicaMetrics{rm}, states(1))
			input.ArrivalRate = 5.68
			_, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			clock = clock.Add(ThroughputSampleSpacing + time.Second)
		}

		decodeKeys := make([]string, 0, 2)
		for key := range analyzer.saturatedThroughput {
			if strings.Contains(key, "|"+domain.RoleDecode+"|") {
				decodeKeys = append(decodeKeys, key)
			}
		}
		Expect(decodeKeys).To(HaveLen(1),
			"a shape that wobbles across a boundary must not split the fleet's window in two: %v", decodeKeys)
		Expect(outstanding()).To(BeFalse(),
			"and it is not a shape change either")
	})

	It("does not read the queue merely emptying as a change of shape", func() {
		// The two sources are on different scales: BytesPerToken is 4 and the
		// run measures ~5.8 bytes per prompt token, so the queue reads 1459
		// tokens for the same 1000-token prompt the replicas report. Fed to
		// one tracker from whichever source happened to be available, that
		// 46 % step raised a change every time the queue filled or emptied --
		// and held the fleet for five minutes on each. The run's queue is
		// empty on 141 of 171 cycles, so this is the common case, not an edge.
		queued := &domain.SchedulerQueueMetrics{QueueSize: 692, QueueBytes: 4_039_809}
		for i, sq := range []*domain.SchedulerQueueMetrics{queued, nil, queued, nil, nil, queued} {
			cycle(10, 17_282, 0, 1000, 6000, sq)
			Expect(outstanding()).To(BeFalse(),
				"cycle %d: the queue coming and going is not the workload changing", i)
			clock = clock.Add(15 * time.Second)
		}
	})

	It("raises nothing while the shape holds still", func() {
		// The negative control for the whole mechanism: the same load, cycle
		// after cycle, must never raise an event or hold anything.
		for i := 0; i < 5; i++ {
			cycle(10, 17_282, 0, 1000, 6000, nil)
			Expect(outstanding()).To(BeFalse(),
				"a fleet serving one shape has nothing to hold for")
			clock = clock.Add(15 * time.Second)
		}
	})
})
