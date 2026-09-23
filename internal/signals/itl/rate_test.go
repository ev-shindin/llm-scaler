package itl

import (
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Sequences", func() {
	It("is the share of the cache one request does not occupy", func() {
		// Half a 1,000,000-token cache, 4,000 tokens a request.
		Expect(Sequences(0.5, 1_000_000, 4_000)).To(BeNumerically("~", 125, 1e-9))
	})

	It("scales with utilization and against footprint", func() {
		base := Sequences(0.4, 1_000_000, 4_000)
		Expect(Sequences(0.8, 1_000_000, 4_000)).To(BeNumerically("~", 2*base, 1e-9))
		Expect(Sequences(0.4, 1_000_000, 8_000)).To(BeNumerically("~", base/2, 1e-9))
	})

	It("declines on any non-positive input rather than returning an infinity", func() {
		Expect(Sequences(0, 1_000_000, 4_000)).To(BeZero())
		Expect(Sequences(0.5, 0, 4_000)).To(BeZero())
		Expect(Sequences(0.5, 1_000_000, 0)).To(BeZero())
		Expect(Sequences(-0.5, 1_000_000, 4_000)).To(BeZero())
	})
})

var _ = Describe("TokenRate", func() {
	// The fit the 2026-09-23 shape-swap runs were taken on:
	// ITL(k) = 30.7 ms*k + 1.6 ms, mean residual 3-6 % over 70 intervals.
	model := Model{A: 0.0307, B: 0.0016}

	It("reproduces the token rate the fleet measured for itself", func() {
		// The trace's first shape on that card: 1000-token prompts, 6000-token
		// generations, so KVreq = 1000 + 3000, over a 1,163,136-token cache.
		// Saturated, the fleet recorded 1.4292 and 1.5429 requests a second
		// across two runs -- 8,575 and 9,258 tokens a second.
		got := TokenRate(model, DefaultKSat, 1_163_136, 4_000)
		Expect(got).To(BeNumerically("~", 8900, 700),
			"the model has to land where the hardware did, or it is not describing it")
	})

	It("reads the same replica differently at a different load", func() {
		// Same replica, same shape, a third as full: fewer sequences resident,
		// and each of them faster because contention is lower.
		busy := TokenRate(model, 0.85, 1_163_136, 4_000)
		idle := TokenRate(model, 0.30, 1_163_136, 4_000)
		Expect(idle).To(BeNumerically("<", busy), "fewer sequences resident")
		Expect(model.ITLAt(0.30)).To(BeNumerically("<", model.ITLAt(0.85)),
			"but each one is advancing faster")
	})

	It("is not a constant across shapes, which is the point of the model", func() {
		// The trace's second shape -- 8000-token prompts, 1000-token
		// generations -- leaves room for less than half as many sequences, so
		// a plain tokens-per-second invariant would have been wrong.
		first := TokenRate(model, DefaultKSat, 1_163_136, 1_000+6_000/2)
		second := TokenRate(model, DefaultKSat, 1_163_136, 8_000+1_000/2)
		Expect(second).To(BeNumerically("<", first/2))
	})

	It("declines rather than guessing", func() {
		Expect(TokenRate(Model{}, DefaultKSat, 1_163_136, 4_000)).To(BeZero(),
			"a zero model has no reading at any k")
		Expect(TokenRate(model, DefaultKSat, 0, 4_000)).To(BeZero())
		Expect(TokenRate(model, DefaultKSat, 1_163_136, 0)).To(BeZero())
	})

	It("refuses a model whose reading at k is not a positive number", func() {
		// A fit on degenerate input can put ITL(k) at or below zero, or at NaN;
		// dividing by either yields an infinity or a NaN that would travel
		// silently into a replica count.
		Expect(TokenRate(Model{A: -1, B: 0.5}, 0.85, 1_163_136, 4_000)).To(BeZero())
		nan := Model{A: math.NaN(), B: math.NaN()}
		Expect(TokenRate(nan, 0.85, 1_163_136, 4_000)).To(BeZero())
	})
})
