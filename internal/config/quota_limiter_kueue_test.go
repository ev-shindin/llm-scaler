package config

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("QuotaLimiterConfig.BoundBy", func() {

	Context("namespace scope", func() {
		nsEntry := func(quotas map[string]map[string]int, exclude ...string) QuotaLimiterConfig {
			return QuotaLimiterConfig{
				Name: "ns", Type: "quota", Scope: QuotaScopeNamespace,
				NamespaceQuotas: quotas, Exclude: exclude,
			}
		}

		It("takes the smaller cap per type for a namespace both sources list", func() {
			entry := nsEntry(map[string]map[string]int{"team-a": {"H100": 8, "A100": 4}})
			out := entry.BoundBy(ExternalQuotas{Namespace: map[string]map[string]int{
				"team-a": {"H100": 2, "A100": 16},
			}})
			Expect(out.NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 2, "A100": 4}))
		})

		It("denies a type only one side lists (closed allowlists on both sides)", func() {
			entry := nsEntry(map[string]map[string]int{"team-a": {"H100": 8}})
			out := entry.BoundBy(ExternalQuotas{Namespace: map[string]map[string]int{
				"team-a": {"A100": 4},
			}})
			Expect(out.NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 0, "A100": 0}))
		})

		It("lets an unlimited static cap defer to the external figure", func() {
			entry := nsEntry(map[string]map[string]int{"team-a": {"H100": QuotaUnlimited}})
			out := entry.BoundBy(ExternalQuotas{Namespace: map[string]map[string]int{
				"team-a": {"H100": 6},
			}})
			Expect(out.NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 6}))
		})

		It("matches type keys by accelerator identity and keeps the static spelling", func() {
			entry := nsEntry(map[string]map[string]int{"team-a": {"H100": 8}})
			out := entry.BoundBy(ExternalQuotas{Namespace: map[string]map[string]int{
				"team-a": {"NVIDIA-H100-80GB-HBM3": 3},
			}})
			Expect(out.NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 3}))

			// And case-insensitively, for a flavor simply named after the model.
			out = entry.BoundBy(ExternalQuotas{Namespace: map[string]map[string]int{
				"team-a": {"h100": 5},
			}})
			Expect(out.NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 5}))
		})

		It("bounds an unlisted namespace at min(default, external) and lists it explicitly", func() {
			entry := nsEntry(map[string]map[string]int{
				"team-a":  {"H100": 8},
				"default": {"H100": 2, "L40S": 1},
			})
			out := entry.BoundBy(ExternalQuotas{Namespace: map[string]map[string]int{
				"team-b": {"H100": 10},
			}})
			Expect(out.NamespaceQuotas["team-b"]).To(Equal(map[string]int{"H100": 2, "L40S": 0}))
			// The default itself is untouched for namespaces Kueue does not know.
			Expect(out.NamespaceQuotas["default"]).To(Equal(map[string]int{"H100": 2, "L40S": 1}))
			Expect(out.NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 8}))
		})

		It("takes the external map as-is when the entry declares no static quotas", func() {
			entry := nsEntry(nil)
			out := entry.BoundBy(ExternalQuotas{Namespace: map[string]map[string]int{
				"team-a": {"H100": 4},
			}})
			Expect(out.NamespaceQuotas).To(Equal(map[string]map[string]int{"team-a": {"H100": 4}}))
		})

		It("does not let the external source open a namespace a strict static allowlist denies", func() {
			entry := nsEntry(map[string]map[string]int{"team-a": {"H100": 8}})
			out := entry.BoundBy(ExternalQuotas{Namespace: map[string]map[string]int{
				"team-b": {"H100": 4},
			}})
			Expect(out.NamespaceQuotas).NotTo(HaveKey("team-b"))
			quotas, excluded := out.QuotaForNamespace("team-b")
			Expect(excluded).To(BeFalse())
			Expect(quotas).To(BeEmpty())
		})

		It("skips excluded namespaces and the reserved default key", func() {
			entry := nsEntry(map[string]map[string]int{"team-a": {"H100": 8}}, "kube-system")
			out := entry.BoundBy(ExternalQuotas{Namespace: map[string]map[string]int{
				"kube-system": {"H100": 1},
				"default":     {"H100": 1},
			}})
			Expect(out.NamespaceQuotas).To(Equal(map[string]map[string]int{"team-a": {"H100": 8}}))
			Expect(out.Exclude).To(Equal([]string{"kube-system"}))
		})

		It("leaves the entry unchanged for an empty snapshot and never aliases the original", func() {
			static := map[string]map[string]int{"team-a": {"H100": 8}}
			entry := nsEntry(static)
			out := entry.BoundBy(ExternalQuotas{})
			Expect(out.NamespaceQuotas).To(Equal(static))
			out.NamespaceQuotas["team-a"]["H100"] = 1
			Expect(static["team-a"]["H100"]).To(Equal(8))
		})
	})

	Context("cluster scope", func() {
		clusterEntry := func(quotas map[string]int) QuotaLimiterConfig {
			return QuotaLimiterConfig{Name: "c", Type: "quota", Scope: QuotaScopeCluster, ClusterQuotas: quotas}
		}

		It("takes the smaller cap per type over the union of keys", func() {
			out := clusterEntry(map[string]int{"H100": 16, "A100": QuotaUnlimited}).
				BoundBy(ExternalQuotas{Cluster: map[string]int{"H100": 24, "A100": 8, "L40S": 2}})
			Expect(out.ClusterQuotas).To(Equal(map[string]int{"H100": 16, "A100": 8, "L40S": 0}))
		})

		It("takes the external map as-is when no static quotas are declared", func() {
			out := clusterEntry(nil).BoundBy(ExternalQuotas{Cluster: map[string]int{"H100": 24}})
			Expect(out.ClusterQuotas).To(Equal(map[string]int{"H100": 24}))
		})

		It("changes nothing when the source declares no GPU budget", func() {
			out := clusterEntry(map[string]int{"H100": 16}).BoundBy(ExternalQuotas{})
			Expect(out.ClusterQuotas).To(Equal(map[string]int{"H100": 16}))
		})
	})
})

var _ = Describe("KueueQuotaSource", func() {
	It("is inert when nil or disabled", func() {
		Expect(QuotaLimiterConfig{}.KueueEnabled()).To(BeFalse())
		Expect(QuotaLimiterConfig{Kueue: &KueueQuotaSource{}}.KueueEnabled()).To(BeFalse())
		Expect(QuotaLimiterConfig{Kueue: &KueueQuotaSource{Enabled: true}}.KueueEnabled()).To(BeTrue())
	})

	It("defaults the refresh interval and honors a configured one", func() {
		Expect(QuotaLimiterConfig{}.KueueRefreshInterval()).To(Equal(DefaultKueueRefreshInterval))
		entry := QuotaLimiterConfig{Kueue: &KueueQuotaSource{Enabled: true, RefreshInterval: "2m"}}
		Expect(entry.KueueRefreshInterval()).To(Equal(2 * time.Minute))
	})

	It("is deep-copied by clone", func() {
		entry := QuotaLimiterConfig{Kueue: &KueueQuotaSource{Enabled: true, Resources: []string{"nvidia.com/gpu"}}}
		out := entry.clone()
		out.Kueue.Resources[0] = "amd.com/gpu"
		out.Kueue.Enabled = false
		Expect(entry.Kueue.Resources).To(Equal([]string{"nvidia.com/gpu"}))
		Expect(entry.Kueue.Enabled).To(BeTrue())
	})

	Context("validation", func() {
		validate := func(k *KueueQuotaSource) error {
			e := QuotaLimiterEntries{Limiters: []QuotaLimiterConfig{{
				Name: "ns", Type: "quota", Scope: QuotaScopeNamespace, Kueue: k,
			}}}
			_, err := e.Validate()
			return err
		}

		It("accepts an enabled block with defaults", func() {
			Expect(validate(&KueueQuotaSource{Enabled: true})).To(Succeed())
		})

		It("rejects an empty resource name", func() {
			err := validate(&KueueQuotaSource{Enabled: true, Resources: []string{"nvidia.com/gpu", " "}})
			Expect(err).To(MatchError(ContainSubstring("kueue.resources[1]")))
		})

		It("rejects a malformed or non-positive refresh interval", func() {
			Expect(validate(&KueueQuotaSource{Enabled: true, RefreshInterval: "soon"})).
				To(MatchError(ContainSubstring("is not a duration")))
			Expect(validate(&KueueQuotaSource{Enabled: true, RefreshInterval: "0s"})).
				To(MatchError(ContainSubstring("must be positive")))
		})

		It("is rejected on a gpu-inventory limiter entry", func() {
			policy := ScalingPolicy{Limiters: []QuotaLimiterConfig{{
				Type: limiterTypeGPUInventory, Kueue: &KueueQuotaSource{Enabled: true},
			}}}
			Expect(policy.validateLimiters()).To(MatchError(ContainSubstring("must not set quota fields")))
		})
	})
})
