package allocation

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// fakeQuotaSource is a scripted QuotaSource: each Quotas call returns the next
// scripted result, the last one repeating.
type fakeQuotaSource struct {
	results []struct {
		q   config.ExternalQuotas
		err error
	}
	calls int
}

func (f *fakeQuotaSource) push(q config.ExternalQuotas, err error) {
	f.results = append(f.results, struct {
		q   config.ExternalQuotas
		err error
	}{q, err})
}

func (f *fakeQuotaSource) Quotas(context.Context) (config.ExternalQuotas, error) {
	f.calls++
	i := min(f.calls-1, len(f.results)-1)
	return f.results[i].q, f.results[i].err
}

var _ = Describe("QuotaInventory with an external QuotaSource", func() {
	var ctx context.Context
	observed := time.Unix(1000, 0)

	BeforeEach(func() { ctx = context.Background() })

	nsEntry := config.QuotaLimiterConfig{
		Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
		NamespaceQuotas: map[string]map[string]int{
			"team-a":  {"H100": 8},
			"default": {"H100": 2},
		},
	}

	It("enforces the static entry until the source has been read once", func() {
		src := &fakeQuotaSource{}
		src.push(config.ExternalQuotas{}, errors.New("forbidden"))
		inv := NewQuotaInventoryWithSource(nsEntry, src)

		Expect(inv.Refresh(ctx)).To(Succeed(), "a source failure must not lift the cap by erroring")
		inv.SetUsedByNamespace(map[string]map[string]int{"team-a": {}})
		pools := inv.NamespaceResourcePools([]string{"team-a"})
		Expect(pools["team-a"]["H100"].Limit).To(Equal(8))
	})

	It("installs min(static, external) as the effective entry on Refresh", func() {
		src := &fakeQuotaSource{}
		src.push(config.ExternalQuotas{
			Namespace: map[string]config.ExternalCaps{
				"team-a": {ByType: map[string]int{"NVIDIA-H100-80GB-HBM3": 3}},
				"team-b": {ByType: map[string]int{"H100": 10}},
			},
			ObservedAt: observed,
		}, nil)
		inv := NewQuotaInventoryWithSource(nsEntry, src)
		Expect(inv.Refresh(ctx)).To(Succeed())

		// Usage arrives keyed by the raw product label and must still land on
		// the effective (static-spelled) key.
		inv.SetUsedByNamespace(map[string]map[string]int{
			"team-a": {"NVIDIA-H100-80GB-HBM3": 2},
			"team-b": {"H100": 1},
		})
		pools := inv.NamespaceResourcePools([]string{"team-a", "team-b"})
		Expect(pools["team-a"]).To(Equal(map[string]ResourcePool{"H100": {Limit: 3, Used: 2}}))
		Expect(pools["team-b"]).To(Equal(map[string]ResourcePool{"H100": {Limit: 2, Used: 1}}),
			"an unlisted namespace is bounded at min(default, external)")
		Expect(inv.TotalLimit()).To(Equal(5))
		Expect(inv.TotalUsed()).To(Equal(3))
		Expect(inv.EffectiveConfig().NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 3}))
	})

	It("keeps a stale snapshot in force when a later read fails", func() {
		src := &fakeQuotaSource{}
		good := config.ExternalQuotas{
			Namespace:  map[string]config.ExternalCaps{"team-a": {ByType: map[string]int{"H100": 3}}},
			ObservedAt: observed,
		}
		src.push(good, nil)
		src.push(good, errors.New("api down")) // the source hands back its last good snapshot
		inv := NewQuotaInventoryWithSource(nsEntry, src)

		Expect(inv.Refresh(ctx)).To(Succeed())
		Expect(inv.Refresh(ctx)).To(Succeed())
		Expect(inv.EffectiveConfig().NamespaceQuotas["team-a"]).To(Equal(map[string]int{"H100": 3}))
	})

	It("follows the source when its figures change", func() {
		src := &fakeQuotaSource{}
		src.push(config.ExternalQuotas{Namespace: map[string]config.ExternalCaps{"team-a": {ByType: map[string]int{"H100": 3}}}, ObservedAt: observed}, nil)
		src.push(config.ExternalQuotas{Namespace: map[string]config.ExternalCaps{"team-a": {ByType: map[string]int{"H100": 6}}}, ObservedAt: observed.Add(time.Minute)}, nil)
		inv := NewQuotaInventoryWithSource(nsEntry, src)

		Expect(inv.Refresh(ctx)).To(Succeed())
		Expect(inv.EffectiveConfig().NamespaceQuotas["team-a"]["H100"]).To(Equal(3))
		Expect(inv.Refresh(ctx)).To(Succeed())
		Expect(inv.EffectiveConfig().NamespaceQuotas["team-a"]["H100"]).To(Equal(6))
	})

	It("bounds a cluster-scoped entry the same way", func() {
		src := &fakeQuotaSource{}
		src.push(config.ExternalQuotas{Cluster: config.ExternalCaps{ByType: map[string]int{"H100": 12, "A100": 4}}, ObservedAt: observed}, nil)
		inv := NewQuotaInventoryWithSource(config.QuotaLimiterConfig{
			Name: "cluster-quota", Type: "quota", Scope: config.QuotaScopeCluster,
			ClusterQuotas: map[string]int{"H100": 16},
		}, src)
		Expect(inv.Refresh(ctx)).To(Succeed())
		inv.SetUsed(map[string]int{"H100": 5})
		Expect(inv.GetResourcePools()).To(Equal(map[string]ResourcePool{
			"H100": {Limit: 12, Used: 5},
			"A100": {Limit: 0},
		}))
	})

	It("is a no-op without a source", func() {
		inv := NewQuotaInventory(nsEntry)
		Expect(inv.Refresh(ctx)).To(Succeed())
		Expect(inv.EffectiveConfig()).To(Equal(nsEntry))
	})

	It("reaches the optimizer's constraints through DefaultLimiter.ComputeConstraints", func() {
		src := &fakeQuotaSource{}
		src.push(config.ExternalQuotas{
			Namespace:  map[string]config.ExternalCaps{"team-a": {ByType: map[string]int{"H100": 3}}},
			ObservedAt: observed,
		}, nil)
		limiter := NewDefaultLimiter("namespace-quota", NewQuotaInventoryWithSource(nsEntry, src))

		rc, err := limiter.ComputeConstraints(ctx, nil, map[string]map[string]int{"team-a": {"H100": 1}})
		Expect(err).NotTo(HaveOccurred())
		Expect(rc.NamespacePools["team-a"]["H100"]).To(Equal(ResourcePool{Limit: 3, Used: 1}))
		Expect(rc.Pools["H100"]).To(Equal(ResourcePool{Limit: 3, Used: 1}))
		Expect(rc.TotalAvail).To(Equal(2))
	})
})

var _ = Describe("NewLimiterFromConfig with a kueue-enabled quota entry", func() {
	entry := config.QuotaLimiterConfig{
		Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
		NamespaceQuotas: map[string]map[string]int{"team-a": {"H100": 8}},
		Kueue:           &config.KueueQuotaSource{Enabled: true},
	}

	It("builds a Kueue-backed quota limiter over the given client", func() {
		c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
		l, err := NewLimiterFromConfig(configWithLimiters(entry), c)
		Expect(err).NotTo(HaveOccurred())
		dl, ok := l.(*DefaultLimiter)
		Expect(ok).To(BeTrue())
		inv, ok := dl.inventory.(*QuotaInventory)
		Expect(ok).To(BeTrue())
		Expect(inv.source).NotTo(BeNil())
	})

	It("refuses to build without a client rather than silently dropping the source", func() {
		_, err := NewLimiterFromConfig(configWithLimiters(entry), nil)
		Expect(err).To(MatchError(ContainSubstring("enables kueue but no Kubernetes client")))
	})

	It("shares one reader between limiters built for the same entry", func() {
		c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
		cfg := configWithLimiters(entry)
		first, err := NewLimiterFromConfig(cfg, c)
		Expect(err).NotTo(HaveOccurred())
		second, err := NewLimiterFromConfig(cfg, c)
		Expect(err).NotTo(HaveOccurred())
		src := func(l Limiter) QuotaSource {
			return l.(*DefaultLimiter).inventory.(*QuotaInventory).source
		}
		Expect(src(first)).To(BeIdenticalTo(src(second)),
			"both engines build a limiter from the same config; one Kueue reader must serve both")

		changed := entry
		changed.Kueue = &config.KueueQuotaSource{Enabled: true, RefreshInterval: "1m"}
		third, err := NewLimiterFromConfig(configWithLimiters(changed), c)
		Expect(err).NotTo(HaveOccurred())
		Expect(src(third)).NotTo(BeIdenticalTo(src(first)), "different reader options need a new reader")
	})

	It("changes the limiter signature when the kueue block changes", func() {
		without := entry
		without.Kueue = nil
		Expect(LimiterSignature(configWithLimiters(entry))).
			NotTo(Equal(LimiterSignature(configWithLimiters(without))))
	})
})
