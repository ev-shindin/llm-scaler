package shape

import (
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Tracker", func() {
	var tracker *Tracker

	BeforeEach(func() {
		tracker = NewTracker(DefaultChangeTolerance)
	})

	Describe("first Observe call", func() {
		It("sets the shape without reporting a change", func() {
			shape, changed := tracker.Observe(5000, 200, 0.1)

			Expect(changed).To(BeFalse())
			Expect(shape.AvgInputTokens).To(Equal(5000.0))
			Expect(shape.AvgOutputTokens).To(Equal(200.0))
			Expect(shape.PrefixHitRate).To(Equal(0.1))
		})

		It("computes ILeff and KVreq correctly", func() {
			shape, _ := tracker.Observe(5000, 200, 0.3)

			// ILeff = 5000 × (1 - 0.3) = 3500
			Expect(shape.ILeff).To(BeNumerically("~", 3500.0, 0.01))
			// KVreq = 3500 + 200/2 = 3600
			Expect(shape.KVreq).To(BeNumerically("~", 3600.0, 0.01))
		})

		It("reports no shape before first call", func() {
			_, hasShape := tracker.Current()
			Expect(hasShape).To(BeFalse())
		})
	})

	Describe("subsequent calls within tolerance", func() {
		BeforeEach(func() {
			tracker.Observe(5000, 200, 0.0)
		})

		It("does not report a change for identical values", func() {
			_, changed := tracker.Observe(5000, 200, 0.0)
			Expect(changed).To(BeFalse())
		})

		It("does not report a change for 15% IL shift (within 20% tolerance)", func() {
			_, changed := tracker.Observe(5750, 200, 0.0) // +15% IL
			Expect(changed).To(BeFalse())
		})

		It("does not report a change for 15% OL shift (within 20% tolerance)", func() {
			_, changed := tracker.Observe(5000, 230, 0.0) // +15% OL
			Expect(changed).To(BeFalse())
		})

		It("does not report a change at exactly the tolerance boundary", func() {
			_, changed := tracker.Observe(6000, 200, 0.0) // exactly +20% IL
			Expect(changed).To(BeFalse())
		})
	})

	Describe("shape change detection", func() {
		BeforeEach(func() {
			tracker.Observe(5000, 200, 0.0)
		})

		It("reports a change for >20% IL increase", func() {
			_, changed := tracker.Observe(6100, 200, 0.0) // +22% IL
			Expect(changed).To(BeTrue())
		})

		It("reports a change for >20% IL decrease", func() {
			_, changed := tracker.Observe(3900, 200, 0.0) // -22% IL
			Expect(changed).To(BeTrue())
		})

		It("reports a change for >20% OL increase", func() {
			_, changed := tracker.Observe(5000, 245, 0.0) // +22.5% OL
			Expect(changed).To(BeTrue())
		})

		It("updates the stored shape after a change", func() {
			tracker.Observe(6500, 300, 0.0)
			shape, hasShape := tracker.Current()
			Expect(hasShape).To(BeTrue())
			Expect(shape.AvgInputTokens).To(Equal(6500.0))
			Expect(shape.AvgOutputTokens).To(Equal(300.0))
		})
	})

	Describe("a workload that drifts rather than jumps", func() {
		// The 1k/6000 -> 8k/1000 swap measured on 2026-09-23: generations
		// already in flight keep the fleet average high, so the served output
		// length walks down over minutes instead of stepping. Every 15-second
		// cycle moves a few percent; the whole move is 83%.
		It("declares a change once the cumulative move leaves the band", func() {
			tracker.Observe(1000, 5900, 0.0)

			declared := 0
			last := 5900.0
			for ol := 5900.0; ol > 1000; ol *= 0.95 { // -5% a cycle, never -20%
				if _, changed := tracker.Observe(1000, ol, 0.0); changed {
					declared++
				}
				last = ol
			}
			Expect(last).To(BeNumerically("<", 1100))
			Expect(declared).To(BeNumerically(">", 0),
				"a slide of 83%% must be declared: measuring the band from the "+
					"previous reading makes the tolerance a per-cycle rate limit")
		})

		It("settles on the shape the workload arrived at", func() {
			tracker.Observe(1000, 5900, 0.0)
			for ol := 5900.0; ol > 1000; ol *= 0.95 {
				tracker.Observe(1000, ol, 0.0)
			}
			// The slide is over; the workload now holds 1000. The anchor has
			// come down with it, so it takes at most one more declaration to
			// land on the new shape -- against a fixed 5900 anchor it would
			// declare on every single cycle from here to the end of the run.
			declared := 0
			for i := 0; i < 10; i++ {
				if _, changed := tracker.Observe(1000, 1000, 0.0); changed {
					declared++
				}
			}
			Expect(declared).To(BeNumerically("<=", 1))
		})

		It("reports the latest reading from Current, not the band's centre", func() {
			// The anchor and the latest reading are separate fields now, and
			// only a reading INSIDE the band tells them apart: on a declared
			// change Observe writes the same value to both. The throughput
			// analyzer asks Current what the fleet is serving, so it has to be
			// the measurement.
			tracker.Observe(5000, 200, 0.0)

			_, changed := tracker.Observe(5750, 200, 0.0) // +15%, inside the band
			Expect(changed).To(BeFalse())

			shape, hasShape := tracker.Current()
			Expect(hasShape).To(BeTrue())
			Expect(shape.AvgInputTokens).To(Equal(5750.0))
		})

		It("does not re-declare while the workload holds its new shape", func() {
			tracker.Observe(1000, 5900, 0.0)
			tracker.Observe(1000, 1000, 0.0) // the change

			for i := 0; i < 20; i++ {
				_, changed := tracker.Observe(1000, 1000, 0.0)
				Expect(changed).To(BeFalse())
			}
		})
	})

	Describe("Reset", func() {
		It("clears the stored shape", func() {
			tracker.Observe(5000, 200, 0.0)
			tracker.Reset()

			_, hasShape := tracker.Current()
			Expect(hasShape).To(BeFalse())
		})

		It("treats the next call as a fresh first call after Reset", func() {
			tracker.Observe(5000, 200, 0.0)
			tracker.Reset()

			_, changed := tracker.Observe(9999, 999, 0.0)
			Expect(changed).To(BeFalse())
		})
	})

	Describe("NaN and edge case hit rates", func() {
		It("treats NaN hit rate as 0.0", func() {
			shape, _ := tracker.Observe(5000, 200, float64NaN())
			Expect(shape.PrefixHitRate).To(Equal(0.0))
			Expect(shape.ILeff).To(BeNumerically("~", 5000.0, 0.01))
		})

		It("clamps hit rate above 1.0 to 1.0", func() {
			shape, _ := tracker.Observe(5000, 200, 1.5)
			Expect(shape.PrefixHitRate).To(Equal(1.0))
			Expect(shape.ILeff).To(BeNumerically("~", 0.0, 0.01))
		})

		It("clamps negative hit rate to 0.0", func() {
			shape, _ := tracker.Observe(5000, 200, -0.3)
			Expect(shape.PrefixHitRate).To(Equal(0.0))
			Expect(shape.ILeff).To(BeNumerically("~", 5000.0, 0.01))
		})
	})
})

// float64NaN returns a NaN value for use in tests.
func float64NaN() float64 {
	return math.NaN()
}
