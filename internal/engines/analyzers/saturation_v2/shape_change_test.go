package saturation_v2

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/floor"
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
			rm.GenerationTokenRate = rm.RequestRate * rm.AvgOutputTokens
			rm.Ready = true
			rms = append(rms, rm)
		}
		p := makeReplicaMetrics("p0", prefillV, 8_065, 1_149_312, 0, in, 1)
		p.RequestRate = 6
		p.GenerationTokenRate = p.RequestRate * p.AvgOutputTokens
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
			rms.GenerationTokenRate = rms.RequestRate * rms.AvgOutputTokens
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

	It("bounds the hold by the transition, not by each step of it", func() {
		// A shape swap is not one change event. Generations already in flight
		// keep the fleet average high, so the served output length walks down
		// over minutes, and the tracker declares a change each time the walk
		// has covered a further tolerance. Restarting the clock on each of
		// those would hold the fleet for the whole of a slide TOWARDS shorter
		// work -- the transition that needs fewer replicas, not more.
		cycle(10, 900_000, 0, 1000, 6000, nil)
		clock = clock.Add(15 * time.Second)
		cycle(10, 900_000, 0, 8000, 6000, nil)
		Expect(outstanding()).To(BeTrue())
		raisedAt := clock

		// The walk down, a declaration roughly every 90 s, never settling: no
		// cycle saturates, so nothing ever measures the new shape.
		for _, out := range []float64{4500, 3300, 2400, 1700, 1200} {
			clock = clock.Add(90 * time.Second)
			cycle(10, 17_282, 0, 8000, out, nil)
		}
		Expect(clock.Sub(raisedAt)).To(BeNumerically(">", ShapeChangeHoldMax),
			"the walk has to outlast the backstop or this proves nothing")

		clock = clock.Add(15 * time.Second)
		cycle(10, 17_282, 0, 8000, 1000, nil)
		Expect(outstanding()).To(BeFalse(),
			"the backstop runs from the first change of the transition; a later "+
				"step of the same slide must not extend it")
	})

	It("re-arms once the hold has cleared", func() {
		// The clock starting only when it is not already running is the whole
		// of the change above, and the risk it carries is the opposite one:
		// a clock that is never rearmed. A fleet whose shape moves again --
		// hours later, or back to where it started -- must be held for the
		// second transition as it was for the first.
		cycle(10, 900_000, 0, 1000, 6000, nil)
		clock = clock.Add(15 * time.Second)
		cycle(10, 900_000, 0, 8000, 6000, nil)
		Expect(outstanding()).To(BeTrue())

		clock = clock.Add(ShapeChangeHoldMax + time.Second)
		cycle(10, 17_282, 0, 8000, 6000, nil)
		Expect(outstanding()).To(BeFalse(), "the backstop clears the first hold")

		// A genuinely new transition, well after the first has been let go.
		clock = clock.Add(10 * time.Minute)
		cycle(10, 17_282, 0, 1000, 1000, nil)
		Expect(outstanding()).To(BeTrue(),
			"a change arriving after the hold cleared raises a hold of its own")

		// And it is bounded like any other, from its own first change.
		clock = clock.Add(ShapeChangeHoldMax + time.Second)
		cycle(10, 17_282, 0, 1000, 1000, nil)
		Expect(outstanding()).To(BeFalse())
	})

	It("runs the backstop on a cycle that also declares a change", func() {
		// These two were a switch, and its cases were mutually exclusive only
		// while the arm fired on every change, which kept the elapsed time at
		// zero. Once the clock stopped being restarted, a cycle could both
		// declare a change and be past the backstop, and the switch took the
		// first case and skipped the clear -- holding the fleet past
		// ShapeChangeHoldMax for as long as changes kept arriving.
		cycle(10, 900_000, 0, 1000, 6000, nil)
		clock = clock.Add(15 * time.Second)
		cycle(10, 900_000, 0, 8000, 6000, nil)
		Expect(outstanding()).To(BeTrue())

		// Past the backstop, and a change on the very same cycle.
		clock = clock.Add(ShapeChangeHoldMax + time.Second)
		cycle(10, 17_282, 0, 8000, 1000, nil)
		Expect(outstanding()).To(BeFalse(),
			"the backstop is not behind another condition; a change arriving on "+
				"the cycle it expires must not keep the hold alive")
	})

	It("does not chain one hold onto the next for a fleet that never settles", func() {
		// A workload that drifts without ever settling crosses the band every
		// few cycles. One fresh hold per crossing is a fleet that is never
		// released at all -- worse than the stale figures the hold guards
		// against, because those at least let it scale.
		cycle(10, 900_000, 0, 1000, 6000, nil)
		clock = clock.Add(15 * time.Second)
		cycle(10, 900_000, 0, 8000, 6000, nil)
		Expect(outstanding()).To(BeTrue())

		clock = clock.Add(ShapeChangeHoldMax + time.Second)
		cycle(10, 17_282, 0, 8000, 5000, nil)
		Expect(outstanding()).To(BeFalse(), "the backstop gives up on the first hold")

		// The slide continues, declaring a change every 90 s and never
		// saturating, so nothing ever settles. Through the cooldown the fleet
		// stays released even though every one of those cycles declares.
		gaveUp := clock
		for _, out := range []float64{4000, 3200, 2500} {
			clock = clock.Add(90 * time.Second)
			cycle(10, 17_282, 0, 8000, out, nil)
			Expect(clock.Sub(gaveUp)).To(BeNumerically("<", ShapeChangeHoldMax),
				"these cycles have to fall inside the cooldown or this proves nothing")
			Expect(outstanding()).To(BeFalse(),
				"a hold given up on is not re-raised by the next crossing of the same slide")
		}

		// Past the cooldown it may hold again -- the bound is a duty cycle, not
		// a permanent disarm, so a drift that genuinely continues still gets
		// looked at, and a fleet is released for at least half of it.
		clock = clock.Add(ShapeChangeHoldMax)
		cycle(10, 17_282, 0, 8000, 1800, nil)
		Expect(outstanding()).To(BeTrue())
	})

	It("declares a drift in the arriving prompt length, not just a jump", func() {
		// The queue-derived length is the early axis -- it moves before
		// anything completes -- so it is the one that most needs to notice a
		// drift. It was compared against the previous reading, where a few
		// percent a cycle never crosses the tolerance.
		q := func(tokens float64) *domain.SchedulerQueueMetrics {
			return &domain.SchedulerQueueMetrics{
				QueueSize: 20, QueueBytes: int64(tokens * 20 * BytesPerToken),
			}
		}
		// The replicas' own shape is held still, so only the arriving axis can
		// declare anything.
		cycle(10, 17_282, 0, 8000, 1000, q(1000))
		Expect(outstanding()).To(BeFalse())

		declared := false
		for tokens := 1000.0; tokens < 2600; tokens *= 1.05 {
			clock = clock.Add(15 * time.Second)
			cycle(10, 17_282, 0, 8000, 1000, q(tokens))
			if outstanding() {
				declared = true
				break
			}
		}
		Expect(declared).To(BeTrue(),
			"a prompt length that doubles 5% at a time is a shape change")
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
			rm.GenerationTokenRate = rm.RequestRate * rm.AvgOutputTokens
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

	It("does not read the first measurable output length as a change", func() {
		// Observed on the 2026-09-22 rerun at 19:20:37, 70 s in: before any
		// request completes fleetOutputLength is 0, and Within treats any
		// non-zero value as outside a stored zero, so the first completions
		// read as `outputTokensWas: 0, outputTokensNow: 6000`. Harmless
		// during a ramp, wrong on a controller restart in a steady phase.
		rm := makeReplicaMetrics("d0", decodeV, 17_282, kvCap, 0, 1000, 0)
		rm.RequestRate = 0.6
		rm.GenerationTokenRate = rm.RequestRate * rm.AvgOutputTokens
		rm.Ready = true
		input := makeAnalyzerInput([]domain.ReplicaMetrics{rm}, states(1))
		input.ArrivalRate = 5.68
		_, err := analyzer.Analyze(ctx, input)
		Expect(err).NotTo(HaveOccurred())
		Expect(outstanding()).To(BeFalse())
		clock = clock.Add(15 * time.Second)

		// The first completions land: an output length becomes measurable.
		cycle(1, 17_282, 0, 1000, 6000, nil)
		Expect(outstanding()).To(BeFalse(),
			"learning an axis for the first time is not the workload changing")

		// And a genuine change after that is still caught.
		clock = clock.Add(15 * time.Second)
		cycle(1, 8000, 0, 8000, 1000, nil)
		Expect(outstanding()).To(BeTrue(), "a real shape change must still raise")
	})

	It("does not take a drain burst as the fleet's throughput", func() {
		// throughput_floor.go's header records this as the open failure of a
		// completion-rate mu: sequences admitted together finish together, so
		// when a batch drains the completion rate spikes far above anything
		// the replica sustains, the queue gate is a one-minute max and is
		// still up while the burst is fresh, and the window -- which keeps a
		// max and never lowers a reading -- carries the burst for the rest of
		// the phase. Measured on the cold pass of 2026-09-19: 6.67 req/s on
		// the last saturated cycle against ~5.0 sustained.
		//
		// Generated tokens do not burst on a drain, so pricing mu from them
		// leaves the window on the sustained figure.
		const (
			out       = 1000.0
			sustained = 5.0
			burst     = 6.67
		)
		saturated := func(completions, genTokens float64) {
			rm := makeReplicaMetrics("d0", decodeV, 1_100_000, kvCap, 10, 1000, out)
			rm.RequestRate = completions
			rm.GenerationTokenRate = genTokens
			rm.Ready = true
			input := makeAnalyzerInput([]domain.ReplicaMetrics{rm}, states(1))
			input.ArrivalRate = 5.68
			_, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			clock = clock.Add(ThroughputSampleSpacing + time.Second)
		}

		// Two sustained cycles a window apart: the fleet is completing 5.0/s
		// and generating 5000 tokens/s to do it.
		saturated(sustained-0.1, (sustained-0.1)*out)
		saturated(sustained, sustained*out)

		// The drain: the batch finishes together, so completions spike while
		// the token rate does not -- there are no more tokens to emit than
		// the batch was already emitting.
		saturated(burst, sustained*out)

		var mu float64
		for key, window := range analyzer.saturatedThroughput {
			if strings.Contains(key, "|"+domain.RoleDecode+"|") {
				mu = window.Max()
			}
		}
		Expect(mu).To(BeNumerically("~", sustained, 0.05),
			"the window must hold what the replica sustains, not the drain's burst")
		Expect(mu).To(BeNumerically("<", burst*0.95),
			"a burst of %v must not become the fleet's mu", burst)
	})

	It("does not read a metric gap as a change of shape", func() {
		// The mirror of the spec above, and the case its guard missed. Once an
		// axis has been learned, a cycle in which no replica reports it -- a
		// scrape gap, a rolling restart, a moment when no ready replica has
		// completed anything -- reads as an average of 0, and Within measures
		// any non-zero stored value against it as a change of 100%. The fleet
		// would then be held from release for ShapeChangeHoldMax on a workload
		// that never moved.
		cycle(10, 17_282, 0, 1000, 6000, nil)
		clock = clock.Add(15 * time.Second)
		cycle(10, 17_282, 0, 1000, 6000, nil)
		Expect(outstanding()).To(BeFalse())
		clock = clock.Add(15 * time.Second)

		// The gap: the replicas are there and serving, but none reports an
		// output length this cycle.
		gap := makeReplicaMetrics("d0", decodeV, 17_282, kvCap, 0, 1000, 0)
		gap.RequestRate = 0.6
		gap.GenerationTokenRate = gap.RequestRate * 6000
		gap.Ready = true
		input := makeAnalyzerInput([]domain.ReplicaMetrics{gap}, states(10))
		input.ArrivalRate = 5.68
		_, err := analyzer.Analyze(ctx, input)
		Expect(err).NotTo(HaveOccurred())
		Expect(outstanding()).To(BeFalse(),
			"an axis nobody reported this cycle is missing, not zero")

		// The same on the other axis. Only the output half was driven above,
		// so the input carry-forward was never exercised: a scrape that keeps
		// the output field and loses the input one is just as ordinary.
		clock = clock.Add(15 * time.Second)
		lostInput := makeReplicaMetrics("d0", decodeV, 17_282, kvCap, 0, 0, 6000)
		lostInput.RequestRate = 0.6
		lostInput.GenerationTokenRate = lostInput.RequestRate * 6000
		lostInput.Ready = true
		in2 := makeAnalyzerInput([]domain.ReplicaMetrics{lostInput}, states(10))
		in2.ArrivalRate = 5.68
		_, err = analyzer.Analyze(ctx, in2)
		Expect(err).NotTo(HaveOccurred())
		Expect(outstanding()).To(BeFalse(),
			"the input axis is carried forward too")

		// And both at once, with a queue reading present so the cycle is not
		// simply skipped as unmeasurable.
		clock = clock.Add(15 * time.Second)
		bothGone := makeReplicaMetrics("d0", decodeV, 17_282, kvCap, 0, 0, 0)
		bothGone.RequestRate = 0.6
		bothGone.GenerationTokenRate = bothGone.RequestRate * 6000
		bothGone.Ready = true
		in3 := makeAnalyzerInput([]domain.ReplicaMetrics{bothGone}, states(10))
		in3.ArrivalRate = 5.68
		in3.SchedulerQueue = &domain.SchedulerQueueMetrics{QueueSize: 692, QueueBytes: 4_039_809}
		_, err = analyzer.Analyze(ctx, in3)
		Expect(err).NotTo(HaveOccurred())
		Expect(outstanding()).To(BeFalse(),
			"and a cycle that lost both axes is still not a change of shape")

		// And the shape is still the one it was, so the axis coming back is
		// not a change either.
		clock = clock.Add(15 * time.Second)
		cycle(10, 17_282, 0, 1000, 6000, nil)
		Expect(outstanding()).To(BeFalse(), "nor is it a change when the reading returns")
	})

	It("composes with the prefill hold over the same demand map", func() {
		// analyzer.go applies holdPrefillDemand and then holdFleetFloor, and
		// both write the same roleDemand entries and the same variant figures.
		// They are asserted here rather than through Analyze because they
		// cannot in fact both apply in one cycle: a saturated decode replica
		// records a window under the new shape's key in that same cycle, the
		// reading is its own rather than borrowed, and settleFleetShape clears
		// the change before holdFleetFloor is reached. The composition still
		// has to be right -- the order is unconditional in the engine, and a
		// future settle rule could let both through.
		const p = 900_000.0
		vcs := []domain.VariantCapacity{
			{VariantName: "d", Role: domain.RoleDecode, ReplicaCount: 4, PerReplicaCapacity: p},
			{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: 2, PerReplicaCapacity: p},
		}
		demand := map[string]float64{
			domain.RoleDecode:  100_000,
			domain.RolePrefill: 50_000,
		}

		// The prefill hold first, as the engine runs it: it clamps prefill
		// into the band, which raises it off 50_000.
		_, held := holdPrefillDemand(demand, vcs, 0.85, 0.70)
		Expect(held).To(BeTrue())
		afterPrefill := demand[domain.RolePrefill]
		Expect(afterPrefill).To(BeNumerically(">", 50_000))

		// Then the shape hold. It must raise decode, which is far below its
		// band, and must not undo what the prefill hold just decided.
		moved, raised := holdFleetFloor(demand, vcs, 0.70)
		Expect(raised).To(HaveKey(domain.RoleDecode))
		Expect(demand[domain.RoleDecode]).To(BeNumerically("~", 0.70*4*p, 1e-6))
		Expect(demand[domain.RolePrefill]).To(BeNumerically("~", afterPrefill, 1e-6),
			"prefill keeps the figure the prefill hold left it at")
		Expect(moved).To(BeNumerically("~", 0.70*4*p-100_000, 1e-6),
			"and the model total moves only by what this hold changed")

		for _, vc := range vcs {
			Expect(vc.TotalDemand).To(BeNumerically(">=", 0), "%s", vc.VariantName)
			Expect(vc.Utilization).To(BeNumerically(">=", 0), "%s", vc.VariantName)
		}
	})

	It("is not settled by the first sample taken under the new shape", func() {
		// computeReplicaCapacity records a sample and reads the window back in
		// the same call, so one saturated cycle after a switch used to both
		// write the new bucket's first sample and satisfy the settle test --
		// the hold lasted a single cycle. That first sample is also the one
		// most likely to be wrong: the replicas are still draining generations
		// of the OLD length while the new, shorter O is already the divisor.
		cycle(4, 17_282, 0, 1000, 6000, nil)
		clock = clock.Add(15 * time.Second)

		// The switch, and a replica saturated in the very same cycle.
		saturated := makeReplicaMetrics("d0", decodeV, 1_100_000, kvCap, 20, 8000, 1000)
		saturated.RequestRate = 0.6
		saturated.GenerationTokenRate = saturated.RequestRate * 1000
		saturated.Ready = true
		in := makeAnalyzerInput([]domain.ReplicaMetrics{saturated}, states(4))
		in.ArrivalRate = 5.68
		_, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(outstanding()).To(BeTrue(),
			"one sample is the drain, not a measurement of the new shape")

		// A second reading a window later is: it cannot have come from the
		// same drain as the first.
		clock = clock.Add(ThroughputSampleSpacing + time.Second)
		saturated.RequestRate = 0.7
		saturated.GenerationTokenRate = saturated.RequestRate * 1000
		in2 := makeAnalyzerInput([]domain.ReplicaMetrics{saturated}, states(4))
		in2.ArrivalRate = 5.68
		_, err = analyzer.Analyze(ctx, in2)
		Expect(err).NotTo(HaveOccurred())
		Expect(outstanding()).To(BeFalse(), "two spaced readings settle it")
	})

	It("prices mu against the output length the fleet is serving now", func() {
		// The bucket LABEL is hysteretic so it does not flip on a wobble; the
		// DIVISOR must not be, because it is a physical quantity. Freezing
		// both meant that under a drift too slow to trip the tracker -- a
		// percent a cycle, an hour to double -- mu was divided by a stale O
		// for the whole drift and the floor silently stopped binding.
		//
		// Here the fleet's output length drifts 1000 -> 1180 in steps of 6%,
		// none of which trips the 20% tolerance, so the tracked shape never
		// moves. mu must still follow the live figure.
		record := func(out float64) float64 {
			rm := makeReplicaMetrics("d0", decodeV, 1_100_000, kvCap, 20, 1000, out)
			rm.RequestRate = 1.0
			rm.GenerationTokenRate = 6000 // held constant: only O moves
			rm.Ready = true
			in := makeAnalyzerInput([]domain.ReplicaMetrics{rm}, states(1))
			in.ArrivalRate = 5.68
			_, err := analyzer.Analyze(ctx, in)
			Expect(err).NotTo(HaveOccurred())
			clock = clock.Add(ThroughputSampleSpacing + time.Second)
			var mu float64
			for key, w := range analyzer.saturatedThroughput {
				if strings.Contains(key, "|"+domain.RoleDecode+"|") {
					mu = w.Max() // the largest reading the window has taken
				}
			}
			return mu
		}
		record(1000)
		Expect(outstanding()).To(BeFalse(), "the drift never trips the tracker")
		windowMax := record(1180)
		Expect(outstanding()).To(BeFalse())

		// 6000 tokens/s over 1000 is 6.0; over 1180 it is 5.08. A frozen
		// divisor would have recorded 6.0 twice and the window's max would
		// still be 6.0.
		Expect(windowMax).To(BeNumerically("~", 6.0, 0.001),
			"the first reading, at O = 1000")
		var readings int
		for key, w := range analyzer.saturatedThroughput {
			if strings.Contains(key, "|"+domain.RoleDecode+"|") {
				readings = w.Len()
			}
		}
		Expect(readings).To(Equal(2), "both cycles recorded into the one bucket")
		var lowest float64
		for key, w := range analyzer.saturatedThroughput {
			if strings.Contains(key, "|"+domain.RoleDecode+"|") {
				lowest = w.Median()
			}
		}
		Expect(lowest).To(BeNumerically("~", 6000.0/1180.0, 0.01),
			"and the second was priced against the length the fleet had drifted to")
	})

	It("raises no hold at all when the policy disables it", func() {
		// The switch an operator reaches for when the hold misbehaves. The
		// shape is still tracked -- the throughput keys still follow it -- but
		// a change stops withholding release.
		cycle(4, 17_282, 0, 1000, 6000, nil)
		clock = clock.Add(15 * time.Second)

		rm := makeReplicaMetrics("d0", decodeV, 17_282, kvCap, 0, 8000, 1000)
		rm.RequestRate = 0.6
		rm.GenerationTokenRate = rm.RequestRate * 1000
		rm.Ready = true
		in := makeAnalyzerInput([]domain.ReplicaMetrics{rm}, states(4))
		in.ArrivalRate = 5.68
		in.Config.(*config.ScalingPolicy).DisableShapeChangeHold = true
		_, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(outstanding()).To(BeFalse(),
			"the change is tracked but holds nothing")
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

var _ = Describe("holdFleetFloor", func() {
	const p = 900_000.0
	variants := func(spec ...[2]int) []domain.VariantCapacity {
		out := make([]domain.VariantCapacity, 0, len(spec))
		for i, s := range spec {
			role := domain.RoleDecode
			if s[1] == 1 {
				role = domain.RolePrefill
			}
			out = append(out, domain.VariantCapacity{
				VariantName: fmt.Sprintf("v%d", i), Role: role,
				ReplicaCount: s[0], PerReplicaCapacity: p,
			})
		}
		return out
	}

	It("raises a role below the band and leaves one above it alone", func() {
		// The function's own claim: each role is floored against its OWN
		// supply, so a fleet short in one role and comfortable in another is
		// left correct in both.
		vcs := variants([2]int{4, 0}, [2]int{2, 1})
		demand := map[string]float64{
			domain.RoleDecode:  100_000,     // far below 0.7 x 4p
			domain.RolePrefill: 0.9 * 2 * p, // already above 0.7 x 2p
		}
		moved, raised := holdFleetFloor(demand, vcs, 0.70)

		Expect(raised).To(HaveKey(domain.RoleDecode))
		Expect(raised).NotTo(HaveKey(domain.RolePrefill), "a role already above the band is not touched")
		Expect(demand[domain.RoleDecode]).To(BeNumerically("~", 0.70*4*p, 1e-6))
		Expect(demand[domain.RolePrefill]).To(BeNumerically("~", 0.9*2*p, 1e-6))
		Expect(moved).To(BeNumerically("~", 0.70*4*p-100_000, 1e-6),
			"the model total moves by exactly what the roles did")
	})

	It("shares a raised role across its variants by supply", func() {
		// The optimizer prices each variant by its share of the role demand,
		// so the variant figures have to follow the role's or the hold does
		// not survive the split.
		vcs := variants([2]int{3, 0}, [2]int{1, 0})
		demand := map[string]float64{domain.RoleDecode: 10_000}
		_, raised := holdFleetFloor(demand, vcs, 0.70)
		Expect(raised).To(HaveKey(domain.RoleDecode))

		held := 0.70 * 4 * p
		Expect(vcs[0].TotalDemand).To(BeNumerically("~", held*3/4, 1e-6), "three quarters of the supply")
		Expect(vcs[1].TotalDemand).To(BeNumerically("~", held*1/4, 1e-6), "one quarter")
		Expect(vcs[0].TotalDemand + vcs[1].TotalDemand).To(BeNumerically("~", held, 1e-6))
		Expect(vcs[0].Utilization).To(BeNumerically("~", 0.70, 1e-6),
			"and utilization follows, since the warm pool reads it")
	})

	It("does nothing without a demand map, a threshold, or any supply", func() {
		vcs := variants([2]int{2, 0})
		moved, raised := holdFleetFloor(nil, vcs, 0.70)
		Expect(moved).To(BeZero())
		Expect(raised).To(BeNil())

		moved, raised = holdFleetFloor(map[string]float64{domain.RoleDecode: 1}, vcs, 0)
		Expect(moved).To(BeZero())
		Expect(raised).To(BeNil())

		idle := []domain.VariantCapacity{{VariantName: "v", Role: domain.RoleDecode, ReplicaCount: 0, PerReplicaCapacity: p}}
		moved, raised = holdFleetFloor(map[string]float64{domain.RoleDecode: 1}, idle, 0.70)
		Expect(moved).To(BeZero(), "a role with no supply has no band to be held in")
		Expect(raised).To(BeNil())
	})
})

var _ = Describe("a saturated replica that cannot be priced", func() {
	It("records no window, so the role gets no floor", func() {
		// The failure this guards is the one that cost a whole benchmark run
		// on 2026-09-22: the generation-token rate was not being collected,
		// saturatedCompletionRate returned false on every cycle, and the
		// demand floor silently had no mu for any role. The contract is that
		// nothing is recorded -- a half-priced window would take the max of
		// two different definitions of mu.
		analyzer := NewSaturationAnalyzer(capacity.NewStore())
		clock := time.Date(2026, 9, 22, 21, 0, 0, 0, time.UTC)
		analyzer.now = func() time.Time { return clock }

		rm := makeReplicaMetrics("d0", "decode-v", 1_100_000, 1_162_240, 10, 1000, 6000)
		rm.RequestRate = 5.4
		rm.GenerationTokenRate = 0 // the metric nobody was collecting
		rm.Ready = true
		in := makeAnalyzerInput([]domain.ReplicaMetrics{rm}, []domain.VariantReplicaState{
			{VariantName: "decode-v", Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1},
		})
		in.ArrivalRate = 5.68
		result, err := analyzer.Analyze(context.Background(), in)
		Expect(err).NotTo(HaveOccurred())

		for key := range analyzer.saturatedThroughput {
			Expect(key).NotTo(ContainSubstring("|"+domain.RoleDecode+"|"),
				"a rate that cannot be priced must not be recorded under some other definition")
		}
		// With no window there is no floor, so the demand is occupancy and
		// nothing else: the resident KV plus the queue priced at its own
		// per-request footprint, 1,100,000 + 10 x (1000 + 6000).
		Expect(result.RoleDemand[domain.RoleDecode]).To(BeNumerically("~", 1_100_000+10*7000, 1),
			"with no window the role answers to occupancy, not to a floor")
	})
})

var _ = Describe("fleetHasMeasuredItself", func() {
	own := func(variant string) capacity.ReplicaCapacity {
		return capacity.ReplicaCapacity{
			VariantName: variant, SaturatedThroughput: 1.4,
			SaturatedThroughputSamples: floor.MinThroughputSamplesToOrder,
		}
	}
	borrowed := func(variant string) capacity.ReplicaCapacity {
		rc := own(variant)
		rc.SaturatedThroughputBorrowed = true
		return rc
	}
	oneSample := func(variant string) capacity.ReplicaCapacity {
		rc := own(variant)
		rc.SaturatedThroughputSamples = floor.MinThroughputSamplesToOrder - 1
		return rc
	}

	It("requires every variant, not the first one to qualify", func() {
		// The blast radius this guards: windows are keyed per variant, so one
		// variant's readings say nothing about another's, and the hold they
		// would clear belongs to the whole model.
		Expect(fleetHasMeasuredItself([]capacity.ReplicaCapacity{
			own("decode-a"), borrowed("decode-b"),
		})).To(BeFalse(), "decode-b is still reading a neighbour's bucket")

		Expect(fleetHasMeasuredItself([]capacity.ReplicaCapacity{
			own("decode-a"), oneSample("decode-b"),
		})).To(BeFalse(), "one reading is the drain, not a measurement")

		Expect(fleetHasMeasuredItself([]capacity.ReplicaCapacity{
			own("decode-a"), own("decode-b"),
		})).To(BeTrue(), "both measured under the new shape")
	})

	It("counts a variant measured when any of its replicas is", func() {
		Expect(fleetHasMeasuredItself([]capacity.ReplicaCapacity{
			borrowed("decode-a"), own("decode-a"),
		})).To(BeTrue(), "one replica speaks for its variant's window")
	})

	It("is false for a fleet with nothing in it", func() {
		Expect(fleetHasMeasuredItself(nil)).To(BeFalse())
		Expect(fleetHasMeasuredItself([]capacity.ReplicaCapacity{})).To(BeFalse())
	})
})
