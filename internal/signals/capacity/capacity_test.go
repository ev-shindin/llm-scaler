package capacity

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The labels are read by operators and matched by tests downstream: they are
// the analyzer result's Reason and the k2-decision log line's priority. Each
// one is pinned to its source here, by value, because nothing else in the
// tree pins P2-hist or P3-k2 -- the analyzer's own specs assert set
// membership or compare String() against itself, so a transposition between
// two sources would be silent.
var _ = Describe("K2Source.String", func() {
	It("gives each source its own label", func() {
		Expect(K2SrcObserved.String()).To(Equal("P1-obs"))
		Expect(K2SrcHistorical.String()).To(Equal("P2-hist"))
		Expect(K2SrcDerived.String()).To(Equal("P3-k2"))
		Expect(K2SrcFallback.String()).To(Equal("P4-k1"))
	})

	It("is empty for a source that is none of the four", func() {
		// What the label map this method replaced returned for a missing
		// key, and what the saturation analyzer's k2SourceLabel reads to
		// fall back to domain.ReasonError.
		Expect(K2Source(0).String()).To(BeEmpty())
		Expect(K2Source(99).String()).To(BeEmpty())
	})
})
