package itl

import (
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Model", func() {
	Describe("IsZero", func() {
		It("returns true for zero-value model", func() {
			Expect(Model{}.IsZero()).To(BeTrue())
		})

		It("returns false when A is set", func() {
			Expect(Model{A: 0.073, B: 0.006}.IsZero()).To(BeFalse())
		})
	})

	Describe("ITLAt", func() {
		It("computes A·k + B correctly", func() {
			m := Model{A: 0.073, B: 0.006}
			Expect(m.ITLAt(0.85)).To(BeNumerically("~", 0.073*0.85+0.006, 1e-9))
		})

		It("returns B at k=0", func() {
			m := Model{A: 0.073, B: 0.006}
			Expect(m.ITLAt(0)).To(BeNumerically("~", 0.006, 1e-9))
		})
	})
})

var _ = Describe("Fit", func() {
	// makeObs builds a slice of observations that lie exactly on A*k + B.
	makeObs := func(kValues []float64, A, B float64) []Observation {
		obs := make([]Observation, len(kValues))
		for i, k := range kValues {
			obs[i] = Observation{K: k, ITLSec: A*k + B, Timestamp: time.Now()}
		}
		return obs
	}

	Describe("successful fit", func() {
		It("recovers A and B from a perfect linear dataset", func() {
			kValues := []float64{0.20, 0.30, 0.40, 0.50, 0.60, 0.70}
			m, ok := Fit(makeObs(kValues, 0.073, 0.006))
			Expect(ok).To(BeTrue())
			Expect(m.A).To(BeNumerically("~", 0.073, 1e-6))
			Expect(m.B).To(BeNumerically("~", 0.006, 1e-6))
		})

		It("recovers A and B with only 2 observations", func() {
			obs := makeObs([]float64{0.20, 0.80}, 0.05, 0.008)
			m, ok := Fit(obs)
			Expect(ok).To(BeTrue())
			Expect(m.A).To(BeNumerically("~", 0.05, 1e-6))
			Expect(m.B).To(BeNumerically("~", 0.008, 1e-6))
		})

		It("produces a positive slope for monotonically increasing ITL", func() {
			kValues := []float64{0.20, 0.40, 0.60, 0.80}
			m, ok := Fit(makeObs(kValues, 0.10, 0.002))
			Expect(ok).To(BeTrue())
			Expect(m.A).To(BeNumerically(">", 0))
		})
	})

	Describe("degenerate inputs", func() {
		It("returns false for fewer than 2 observations", func() {
			_, ok := Fit(nil)
			Expect(ok).To(BeFalse())

			_, ok = Fit(makeObs([]float64{0.50}, 0.073, 0.006))
			Expect(ok).To(BeFalse())
		})

		It("returns false when all k values are identical (zero k-spread)", func() {
			obs := makeObs([]float64{0.50, 0.50, 0.50, 0.50}, 0.073, 0.006)
			_, ok := Fit(obs)
			Expect(ok).To(BeFalse())
		})

		It("returns false when fitted slope A is zero (flat line)", func() {
			// Constant ITL across varying k → A = 0.
			obs := []Observation{
				{K: 0.20, ITLSec: 0.030},
				{K: 0.50, ITLSec: 0.030},
				{K: 0.80, ITLSec: 0.030},
			}
			_, ok := Fit(obs)
			Expect(ok).To(BeFalse())
		})

		It("returns false when fitted slope A is negative (inverted)", func() {
			// ITL decreasing with k — physically implausible.
			obs := makeObs([]float64{0.20, 0.50, 0.80}, -0.05, 0.10)
			_, ok := Fit(obs)
			Expect(ok).To(BeFalse())
		})
	})

	Describe("prediction accuracy", func() {
		It("ITLAt matches the fitted model's prediction", func() {
			kValues := []float64{0.20, 0.35, 0.50, 0.65, 0.80}
			m, ok := Fit(makeObs(kValues, 0.073, 0.006))
			Expect(ok).To(BeTrue())
			Expect(m.ITLAt(0.85)).To(BeNumerically("~", 0.073*0.85+0.006, 1e-6))
		})
	})
})

var _ = Describe("ValidModel", func() {
	It("accepts a finite, positive-slope model with positive ITL at saturation", func() {
		Expect(ValidModel(0.073, 0.006)).To(BeTrue())
	})

	It("rejects NaN A", func() {
		Expect(ValidModel(math.NaN(), 0.006)).To(BeFalse())
	})

	It("rejects Inf A", func() {
		Expect(ValidModel(math.Inf(1), 0.006)).To(BeFalse())
	})

	It("rejects a flat slope (A <= slopeEpsilon)", func() {
		Expect(ValidModel(0, 0.006)).To(BeFalse())
	})

	It("rejects NaN B", func() {
		Expect(ValidModel(0.073, math.NaN())).To(BeFalse())
	})

	It("rejects Inf B", func() {
		Expect(ValidModel(0.073, math.Inf(1))).To(BeFalse())
	})

	It("rejects a non-positive ITL at saturation", func() {
		// A*DefaultKSat + B <= 0 with a valid positive A.
		Expect(ValidModel(0.01, -1.0)).To(BeFalse())
	})
})
