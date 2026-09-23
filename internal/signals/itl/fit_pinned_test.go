package itl

import (
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("FitPinnedB", func() {
	It("recovers the slope when the points are on a line through the pinned B", func() {
		want := Model{A: 0.0307, B: DefaultBaselineSec}
		obs := make([]Observation, 0, 5)
		for _, k := range []float64{0.2, 0.35, 0.5, 0.65, 0.8} {
			obs = append(obs, Observation{K: k, ITLSec: want.ITLAt(k)})
		}
		got, ok := FitPinnedB(obs, DefaultBaselineSec)
		Expect(ok).To(BeTrue())
		Expect(got.A).To(BeNumerically("~", want.A, 1e-9))
		Expect(got.B).To(Equal(DefaultBaselineSec))
	})

	It("fits a fleet whose replicas are all at the same load", func() {
		// The case Fit refuses and Window.Ready is built to detect: no spread
		// in k at all. One parameter is still determined by it.
		want := Model{A: 0.04, B: DefaultBaselineSec}
		obs := []Observation{
			{K: 0.62, ITLSec: want.ITLAt(0.62)},
			{K: 0.62, ITLSec: want.ITLAt(0.62)},
			{K: 0.62, ITLSec: want.ITLAt(0.62)},
		}
		got, ok := FitPinnedB(obs, DefaultBaselineSec)
		Expect(ok).To(BeTrue())
		Expect(got.A).To(BeNumerically("~", want.A, 1e-9))

		By("where the two-parameter fit has nothing to work with")
		_, twoParam := Fit(obs)
		Expect(twoParam).To(BeFalse())
	})

	It("ignores readings that are missing either half of the pair", func() {
		want := Model{A: 0.03, B: DefaultBaselineSec}
		obs := []Observation{
			{K: 0.4, ITLSec: want.ITLAt(0.4)},
			{K: 0, ITLSec: 0.02},
			{K: 0.5, ITLSec: 0},
			{K: 0.6, ITLSec: want.ITLAt(0.6)},
		}
		got, ok := FitPinnedB(obs, DefaultBaselineSec)
		Expect(ok).To(BeTrue())
		Expect(got.A).To(BeNumerically("~", want.A, 1e-9))
	})

	It("refuses a fit that says load makes a replica faster", func() {
		// A negative slope is measurement noise, not a model: more cache in use
		// cannot speed a replica up, and a model that says so would price an
		// idle fleet as faster than a busy one.
		// The residual has to be negative where it is weighted most, i.e. the
		// busy replica must read FASTER than an idle one: 2 ms at k=0.8
		// against a 6 ms floor.
		obs := []Observation{
			{K: 0.2, ITLSec: 0.0065},
			{K: 0.8, ITLSec: 0.002},
		}
		_, ok := FitPinnedB(obs, DefaultBaselineSec)
		Expect(ok).To(BeFalse())
	})

	It("declines rather than guessing", func() {
		_, ok := FitPinnedB(nil, DefaultBaselineSec)
		Expect(ok).To(BeFalse(), "no observations")

		_, ok = FitPinnedB([]Observation{{K: 0.5, ITLSec: 0.02}}, 0)
		Expect(ok).To(BeFalse(), "no baseline to pin to")

		_, ok = FitPinnedB([]Observation{{K: 0, ITLSec: 0}}, DefaultBaselineSec)
		Expect(ok).To(BeFalse(), "nothing usable in the observations")

		_, ok = FitPinnedB([]Observation{{K: math.NaN(), ITLSec: 0.02}}, DefaultBaselineSec)
		Expect(ok).To(BeFalse(), "a NaN load places nothing")
	})
})
