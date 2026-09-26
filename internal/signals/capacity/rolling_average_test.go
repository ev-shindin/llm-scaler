package capacity

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Rolling Average", func() {

	Describe("Add and Average", func() {
		It("should compute the mean of added values", func() {
			ra := NewRollingAverage(5)
			ra.Add(10)
			ra.Add(20)
			ra.Add(30)

			Expect(ra.Average()).To(Equal(20.0))
			Expect(ra.Len()).To(Equal(3))
		})
	})

	Describe("Window eviction", func() {
		It("should evict oldest values when window is full", func() {
			ra := NewRollingAverage(3)
			ra.Add(1)
			ra.Add(2)
			ra.Add(3)
			ra.Add(4) // evicts 1
			ra.Add(5) // evicts 2

			Expect(ra.Average()).To(Equal(4.0)) // (3+4+5)/3
			Expect(ra.Len()).To(Equal(3))
		})

		It("raises the last value, never a lower one, and reports it", func() {
			ra := NewRollingAverage(3)
			Expect(ra.Last()).To(Equal(0.0), "empty")
			ra.Add(1.5)
			ra.RaiseLast(1.2)
			Expect(ra.Last()).To(Equal(1.5), "a lower reading leaves it")
			ra.RaiseLast(1.8)
			Expect(ra.Last()).To(Equal(1.8))
			Expect(ra.Len()).To(Equal(1), "raising adds nothing")
			ra.Add(2)
			Expect(ra.Last()).To(Equal(2.0))
			Expect(ra.Max()).To(Equal(2.0))
		})
	})

	Describe("Empty average", func() {
		It("should return 0 for an empty rolling average", func() {
			ra := NewRollingAverage(5)
			Expect(ra.Average()).To(Equal(0.0))
			Expect(ra.Len()).To(Equal(0))
		})
	})

	Describe("Single value", func() {
		It("should return the value itself as the average", func() {
			ra := NewRollingAverage(5)
			ra.Add(42)

			Expect(ra.Average()).To(Equal(42.0))
			Expect(ra.Len()).To(Equal(1))
		})
	})

	Describe("Large window with overflow", func() {
		It("should retain only the last maxSize values", func() {
			ra := NewRollingAverage(RollingAverageWindowSize)
			for i := 1; i <= 15; i++ {
				ra.Add(float64(i))
			}

			Expect(ra.Len()).To(Equal(RollingAverageWindowSize))
			// Window contains 6..15, average = 10.5
			Expect(ra.Average()).To(BeNumerically("~", 10.5, 0.001))
		})
	})
})

var _ = Describe("RollingAverage.Median", func() {
	It("takes the middle value, and the lower of two on an even count", func() {
		r := NewRollingAverage(10)
		for _, v := range []float64{5.4, 3.3, 3.5} {
			r.Add(v)
		}
		Expect(r.Median()).To(Equal(3.5))
		Expect(r.Max()).To(Equal(5.4), "the read this replaced")

		r2 := NewRollingAverage(10)
		r2.Add(4.0)
		r2.Add(4.01)
		Expect(r2.Median()).To(Equal(4.0), "the lower of the two middle values")
	})

	It("is not moved by a single burst, which is what Max was", func() {
		// The generation-token rate this window holds bursts rather than
		// under-reads: measured 2026-09-22, a per-replica rate of 4028 min,
		// 7548 median, 11663 max. Read with Max the window ratcheted 0.92 ->
		// 3.60 req/s and the demand floor asked for 1.6 replicas where about
		// eight were needed.
		r := NewRollingAverage(10)
		for _, v := range []float64{0.90, 0.92, 0.88, 0.91, 3.60} {
			r.Add(v)
		}
		Expect(r.Median()).To(BeNumerically("~", 0.91, 0.001))
		Expect(r.Max()).To(Equal(3.60))
	})

	It("is zero on an empty window", func() {
		Expect(NewRollingAverage(10).Median()).To(BeZero())
	})
})
