package config

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// typed builds a namespace snapshot of purely typed grants.
func typed(ns map[string]map[string]int) ExternalQuotas {
	out := ExternalQuotas{Namespace: make(map[string]ExternalCaps, len(ns))}
	for name, caps := range ns {
		out.Namespace[name] = ExternalCaps{ByType: caps}
	}
	return out
}

// untyped builds a namespace snapshot of one untyped grant.
func untyped(ns string, n int) ExternalQuotas {
	return ExternalQuotas{Namespace: map[string]ExternalCaps{ns: {Untyped: n, HasUntyped: true}}}
}

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
			out := entry.BoundBy(typed(map[string]map[string]int{
				"team-a": {"H100": 2, "A100": 16},
			}))
			Expect(out.NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 2, "A100": 4}))
		})

		It("denies a type only one side lists (closed allowlists on both sides)", func() {
			entry := nsEntry(map[string]map[string]int{"team-a": {"H100": 8}})
			out := entry.BoundBy(typed(map[string]map[string]int{
				"team-a": {"A100": 4},
			}))
			Expect(out.NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 0, "A100": 0}))
		})

		It("lets an unlimited static cap defer to the external figure", func() {
			entry := nsEntry(map[string]map[string]int{"team-a": {"H100": QuotaUnlimited}})
			out := entry.BoundBy(typed(map[string]map[string]int{
				"team-a": {"H100": 6},
			}))
			Expect(out.NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 6}))
		})

		It("matches type keys by accelerator identity and keeps the static spelling", func() {
			entry := nsEntry(map[string]map[string]int{"team-a": {"H100": 8}})
			out := entry.BoundBy(typed(map[string]map[string]int{
				"team-a": {"NVIDIA-H100-80GB-HBM3": 3},
			}))
			Expect(out.NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 3}))

			// And case-insensitively, for a GKE-style lower-case product.
			out = entry.BoundBy(typed(map[string]map[string]int{
				"team-a": {"h100": 5},
			}))
			Expect(out.NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 5}))
		})

		It("bounds a short static key by every grant of that family, and a long one by its own", func() {
			// The reviewer's probe. Kueue: PCIe flavor 4, SXM flavor 0. A static
			// entry naming both products must see 4 and 0 -- collapsing the
			// family to "A100" would have granted 4 SXM GPUs Kueue admits none of.
			entry := nsEntry(map[string]map[string]int{"team-a": {
				"NVIDIA-A100-PCIE-40GB": 4, "NVIDIA-A100-SXM4-80GB": 8,
			}})
			out := entry.BoundBy(typed(map[string]map[string]int{
				"team-a": {"NVIDIA-A100-PCIE-40GB": 4, "NVIDIA-A100-SXM4-80GB": 0},
			}))
			Expect(out.NamespaceQuotas["team-a"]).To(Equal(map[string]int{
				"NVIDIA-A100-PCIE-40GB": 4, "NVIDIA-A100-SXM4-80GB": 0,
			}))

			// A static "A100" means either product, so it is bounded by both
			// grants together.
			entry = nsEntry(map[string]map[string]int{"team-a": {"A100": 16}})
			out = entry.BoundBy(typed(map[string]map[string]int{
				"team-a": {"NVIDIA-A100-PCIE-40GB": 4, "NVIDIA-A100-SXM4-80GB": 8},
			}))
			Expect(out.NamespaceQuotas["team-a"]).To(Equal(map[string]int{"A100": 12}))
		})

		It("bounds an unlisted namespace at min(default, external) and lists it explicitly", func() {
			entry := nsEntry(map[string]map[string]int{
				"team-a":  {"H100": 8},
				"default": {"H100": 2, "L40S": 1},
			})
			out := entry.BoundBy(typed(map[string]map[string]int{
				"team-b": {"H100": 10},
			}))
			Expect(out.NamespaceQuotas["team-b"]).To(Equal(map[string]int{"H100": 2, "L40S": 0}))
			// The default itself is untouched for namespaces Kueue does not know.
			Expect(out.NamespaceQuotas["default"]).To(Equal(map[string]int{"H100": 2, "L40S": 1}))
			Expect(out.NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 8}))
		})

		It("takes the external typed map as-is when the entry declares no static quotas", func() {
			entry := nsEntry(nil)
			out := entry.BoundBy(typed(map[string]map[string]int{
				"team-a": {"H100": 4},
			}))
			Expect(out.NamespaceQuotas).To(Equal(map[string]map[string]int{"team-a": {"H100": 4}}))
		})

		It("does not let the external source open a namespace a strict static allowlist denies", func() {
			entry := nsEntry(map[string]map[string]int{"team-a": {"H100": 8}})
			out := entry.BoundBy(typed(map[string]map[string]int{
				"team-b": {"H100": 4},
			}))
			Expect(out.NamespaceQuotas).NotTo(HaveKey("team-b"))
			quotas, excluded := out.QuotaForNamespace("team-b")
			Expect(excluded).To(BeFalse())
			Expect(quotas).To(BeEmpty())
		})

		It("skips excluded namespaces and the reserved default key", func() {
			entry := nsEntry(map[string]map[string]int{"team-a": {"H100": 8}}, "kube-system")
			out := entry.BoundBy(typed(map[string]map[string]int{
				"kube-system": {"H100": 1},
				"default":     {"H100": 1},
			}))
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

	Context("untyped grants (a Kueue flavor with no product label)", func() {
		nsEntry := func(quotas map[string]map[string]int) QuotaLimiterConfig {
			return QuotaLimiterConfig{Name: "ns", Type: "quota", Scope: QuotaScopeNamespace, NamespaceQuotas: quotas}
		}

		It("bounds every static type instead of zeroing them", func() {
			// A working static entry plus Kueue's quickstart `default-flavor`:
			// each static type is bounded by the untyped figure, none is denied.
			entry := nsEntry(map[string]map[string]int{"team-a": {"H100": 8, "A100": 4}})
			out := entry.BoundBy(untyped("team-a", 6))
			Expect(out.NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 6, "A100": 4}))
			Expect(entry.UnappliedUntyped(untyped("team-a", 6))).To(BeEmpty())
		})

		It("denies everything for a namespace whose queue Kueue will not admit from", func() {
			// The reader hands an inactive queue over as an untyped grant of 0:
			// governed, nothing granted. Against the recommended `default: {H100: -1}`
			// that is a real zero, not a fall-through to unlimited.
			entry := nsEntry(map[string]map[string]int{"default": {"H100": QuotaUnlimited, "A100": 4}})
			out := entry.BoundBy(untyped("team-a", 0))
			Expect(out.NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 0, "A100": 0}))
		})

		It("supplies the figure for types the entry names with -1", func() {
			entry := nsEntry(map[string]map[string]int{"default": {"H100": QuotaUnlimited}})
			out := entry.BoundBy(untyped("team-b", 16))
			Expect(out.NamespaceQuotas["team-b"]).To(Equal(map[string]int{"H100": 16}))
		})

		It("applies to static types the typed grants do not name", func() {
			entry := nsEntry(map[string]map[string]int{"team-a": {"H100": 8, "A100": 4}})
			out := entry.BoundBy(ExternalQuotas{Namespace: map[string]ExternalCaps{
				"team-a": {ByType: map[string]int{"H100": 2}, Untyped: 3, HasUntyped: true},
			}})
			Expect(out.NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 2, "A100": 3}))
		})

		It("cannot open a namespace on its own and is reported as unapplied", func() {
			entry := nsEntry(nil)
			ext := untyped("team-a", 16)
			out := entry.BoundBy(ext)
			Expect(out.NamespaceQuotas).To(BeEmpty())
			Expect(entry.UnappliedUntyped(ext)).To(Equal([]string{"team-a"}))

			cluster := QuotaLimiterConfig{Name: "c", Type: "quota", Scope: QuotaScopeCluster}
			cext := ExternalQuotas{Cluster: ExternalCaps{Untyped: 16, HasUntyped: true}}
			Expect(cluster.BoundBy(cext).ClusterQuotas).To(BeEmpty())
			Expect(cluster.UnappliedUntyped(cext)).To(Equal([]string{"cluster"}))
		})
	})

	Context("cluster scope", func() {
		clusterEntry := func(quotas map[string]int) QuotaLimiterConfig {
			return QuotaLimiterConfig{Name: "c", Type: "quota", Scope: QuotaScopeCluster, ClusterQuotas: quotas}
		}
		clusterTyped := func(caps map[string]int) ExternalQuotas {
			return ExternalQuotas{Cluster: ExternalCaps{ByType: caps}}
		}

		It("takes the smaller cap per type over the union of keys", func() {
			out := clusterEntry(map[string]int{"H100": 16, "A100": QuotaUnlimited}).
				BoundBy(clusterTyped(map[string]int{"H100": 24, "A100": 8, "L40S": 2}))
			Expect(out.ClusterQuotas).To(Equal(map[string]int{"H100": 16, "A100": 8, "L40S": 0}))
		})

		It("takes the external typed map as-is when no static quotas are declared", func() {
			out := clusterEntry(nil).BoundBy(clusterTyped(map[string]int{"H100": 24}))
			Expect(out.ClusterQuotas).To(Equal(map[string]int{"H100": 24}))
		})

		It("changes nothing when the source declares no GPU budget", func() {
			out := clusterEntry(map[string]int{"H100": 16}).BoundBy(ExternalQuotas{})
			Expect(out.ClusterQuotas).To(Equal(map[string]int{"H100": 16}))
		})

		It("bounds every static type by an untyped cluster grant", func() {
			out := clusterEntry(map[string]int{"H100": 16, "A100": 2}).BoundBy(ExternalQuotas{
				Cluster: ExternalCaps{Untyped: 8, HasUntyped: true},
			})
			Expect(out.ClusterQuotas).To(Equal(map[string]int{"H100": 8, "A100": 2}))
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
