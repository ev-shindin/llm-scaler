package scalingpolicy

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

var _ = Describe("ShouldCollectClusterInventory", func() {
	// withLimiters returns a test Config whose global "default" saturation entry
	// declares the given inline limiters.
	withLimiters := func(limiters ...config.QuotaLimiterConfig) *config.Config {
		cfg := config.NewTestConfig()
		cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
			"default": {Limiters: limiters},
		})
		return cfg
	}

	// No limiter declared means nothing bounds scaling, so there is no
	// physical-capacity path to log for either — and no reason to list Nodes. This
	// used to collect, because an undeclared list implied an inventory limiter.
	It("skips inventory collection when no limiters are configured", func() {
		Expect(ShouldCollectClusterInventory(config.NewTestConfig())).To(BeFalse())
	})

	It("collects inventory when a gpu-inventory limiter is configured", func() {
		cfg := withLimiters(config.QuotaLimiterConfig{Type: "gpu-inventory"})
		Expect(ShouldCollectClusterInventory(cfg)).To(BeTrue())
	})

	It("skips inventory collection when an inline quota limiter is configured", func() {
		cfg := withLimiters(config.QuotaLimiterConfig{
			Type: "quota", Name: "cluster", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{"H100": 8},
		})
		Expect(ShouldCollectClusterInventory(cfg)).To(BeFalse())
	})
})
