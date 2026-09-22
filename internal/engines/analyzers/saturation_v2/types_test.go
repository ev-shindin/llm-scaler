package saturation_v2

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("classifyOutputLength", func() {
	It("should classify short output (< 100)", func() {
		Expect(classifyOutputLength(50)).To(Equal("short"))
		Expect(classifyOutputLength(0)).To(Equal("short"))
		Expect(classifyOutputLength(99.9)).To(Equal("short"))
	})

	It("should classify medium output (100-500)", func() {
		Expect(classifyOutputLength(100)).To(Equal("medium"))
		Expect(classifyOutputLength(300)).To(Equal("medium"))
		Expect(classifyOutputLength(499.9)).To(Equal("medium"))
	})

	It("should classify long output (500-1500)", func() {
		Expect(classifyOutputLength(500)).To(Equal("long"))
		Expect(classifyOutputLength(1000)).To(Equal("long"))
		Expect(classifyOutputLength(1499.9)).To(Equal("long"))
	})

	It("keeps a 1000-token and a 4000-token shape in different buckets", func() {
		// The shape-swap benchmark's two shapes. In one bucket they shared a
		// throughput window whose max was the shorter shape's, and the floor
		// held the fleet at the shorter shape's size while the longer one was
		// served (docs/proposals/backlog-sizing.md).
		Expect(classifyOutputLength(1000)).NotTo(Equal(classifyOutputLength(4000)))
		Expect(classifyOutputLength(1500)).To(Equal("xlong"))
		Expect(classifyOutputLength(2000)).To(Equal("xlong"))
		Expect(classifyOutputLength(3000)).To(Equal("xxlong"))
		Expect(classifyOutputLength(4000)).To(Equal("xxlong"))
		Expect(classifyOutputLength(6000)).To(Equal("huge"))
		Expect(classifyOutputLength(32000)).To(Equal("huge"))
	})
})
