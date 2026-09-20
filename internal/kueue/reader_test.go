package kueue

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

func TestKueue(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Kueue Reader Suite")
}

// The served API version is whatever the cluster's RESTMapper says; the fake
// mapper serves v1beta2 here and v1beta1 in one spec, and the Reader must not care.
func kueueMapper(version string) meta.RESTMapper {
	gv := schema.GroupVersion{Group: Group, Version: version}
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	m.Add(gv.WithKind(KindClusterQueue), meta.RESTScopeRoot)
	m.Add(gv.WithKind(KindResourceFlavor), meta.RESTScopeRoot)
	m.Add(gv.WithKind(KindLocalQueue), meta.RESTScopeNamespace)
	return m
}

func obj(version, kind, ns, name string, spec map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(Group + "/" + version)
	u.SetKind(kind)
	u.SetName(name)
	if ns != "" {
		u.SetNamespace(ns)
	}
	u.Object["spec"] = spec
	return u
}

func flavor(name string, nodeLabels map[string]interface{}) *unstructured.Unstructured {
	spec := map[string]interface{}{}
	if nodeLabels != nil {
		spec["nodeLabels"] = nodeLabels
	}
	return obj("v1beta2", KindResourceFlavor, "", name, spec)
}

// quota is one {name, nominalQuota} resource line; nominalQuota is a string, as
// the API server stores a Quantity.
func quota(resName string, nominal interface{}) map[string]interface{} {
	return map[string]interface{}{"name": resName, "nominalQuota": nominal}
}

func flavorQuotas(name string, resources ...map[string]interface{}) map[string]interface{} {
	rs := make([]interface{}, 0, len(resources))
	for _, r := range resources {
		rs = append(rs, r)
	}
	return map[string]interface{}{"name": name, "resources": rs}
}

func clusterQueue(version, name string, flavors ...map[string]interface{}) *unstructured.Unstructured {
	fs := make([]interface{}, 0, len(flavors))
	covered := map[string]bool{}
	for _, f := range flavors {
		fs = append(fs, f)
		for _, r := range f["resources"].([]interface{}) {
			covered[r.(map[string]interface{})["name"].(string)] = true
		}
	}
	cr := make([]interface{}, 0, len(covered))
	for r := range covered {
		cr = append(cr, r)
	}
	return obj(version, KindClusterQueue, "", name, map[string]interface{}{
		"resourceGroups": []interface{}{
			map[string]interface{}{"coveredResources": cr, "flavors": fs},
		},
	})
}

func localQueue(version, ns, name, cq string) *unstructured.Unstructured {
	return obj(version, KindLocalQueue, ns, name, map[string]interface{}{"clusterQueue": cq})
}

func newFakeClient(version string, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(runtime.NewScheme()).
		WithRESTMapper(kueueMapper(version)).
		WithObjects(objs...).
		Build()
}

var _ = Describe("Reader", func() {
	const v = "v1beta2"
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	It("derives per-namespace and cluster caps from flavors, ClusterQueues and LocalQueues", func() {
		c := newFakeClient(v,
			flavor("h100-sxm", map[string]interface{}{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"}),
			flavor("h100-pcie", map[string]interface{}{"nvidia.com/gpu.product": "NVIDIA-H100-PCIE-80GB"}),
			flavor("gke-l4", map[string]interface{}{"cloud.google.com/gke-accelerator": "nvidia-l4"}),
			flavor("cpu-only", map[string]interface{}{"node.kubernetes.io/instance-type": "m5.large"}),
			clusterQueue(v, "team-a-cq",
				flavorQuotas("h100-sxm", quota("nvidia.com/gpu", "8"), quota("cpu", "64")),
				flavorQuotas("h100-pcie", quota("nvidia.com/gpu", "2")),
			),
			clusterQueue(v, "team-b-cq",
				flavorQuotas("gke-l4", quota("nvidia.com/gpu", "4")),
				flavorQuotas("cpu-only", quota("cpu", "128"), quota("memory", "512Gi")),
			),
			clusterQueue(v, "batch-cq", flavorQuotas("mi300x", quota("amd.com/gpu", "16"))),
			localQueue(v, "team-a", "lq", "team-a-cq"),
			localQueue(v, "team-a", "lq-2", "team-a-cq"), // same queue twice: counted once
			localQueue(v, "team-b", "lq", "team-b-cq"),
			localQueue(v, "team-b", "batch", "batch-cq"),
			localQueue(v, "team-c", "lq", "cpu-cq"),      // dangling: no GPU opinion
			localQueue(v, "team-d", "lq", "cpu-only-cq"), // CPU queue: no GPU opinion
			clusterQueue(v, "cpu-only-cq", flavorQuotas("cpu-only", quota("cpu", "8"))),
			clusterQueue(v, "zero-cq", flavorQuotas("h100-sxm", quota("nvidia.com/gpu", "0"))),
			localQueue(v, "team-e", "lq", "zero-cq"), // explicit 0: governed, granted nothing
		)
		snap, err := NewReader(c, Options{}).Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.ObservedAt).NotTo(BeZero())
		Expect(snap.Namespace).To(Equal(map[string]map[string]int{
			"team-a": {"H100": 10},
			"team-b": {"l4": 4, "mi300x": 16},
			"team-e": {"H100": 0},
		}))
		Expect(snap.Cluster).To(Equal(map[string]int{"H100": 10, "l4": 4, "mi300x": 16}))
	})

	It("names a flavor without a product label after the flavor itself", func() {
		c := newFakeClient(v,
			flavor("h100", nil),
			clusterQueue(v, "cq", flavorQuotas("h100", quota("nvidia.com/gpu", "6"))),
			localQueue(v, "team-a", "lq", "cq"),
		)
		snap, err := NewReader(c, Options{}).Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Namespace["team-a"]).To(Equal(map[string]int{"h100": 6}))
	})

	It("reads the API version the cluster serves", func() {
		const old = "v1beta1"
		c := newFakeClient(old,
			clusterQueue(old, "cq", flavorQuotas("a100", quota("nvidia.com/gpu", "3"))),
			localQueue(old, "team-a", "lq", "cq"),
		)
		snap, err := NewReader(c, Options{}).Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Namespace["team-a"]).To(Equal(map[string]int{"a100": 3}))
	})

	It("counts only the configured resource names", func() {
		c := newFakeClient(v,
			clusterQueue(v, "cq",
				flavorQuotas("h100", quota("nvidia.com/gpu", "6")),
				flavorQuotas("mi300x", quota("amd.com/gpu", "8")),
			),
			localQueue(v, "team-a", "lq", "cq"),
		)
		snap, err := NewReader(c, Options{Resources: []string{"amd.com/gpu"}}).Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Namespace["team-a"]).To(Equal(map[string]int{"mi300x": 8}))
	})

	It("lists LocalQueues in one namespace only when told to", func() {
		c := newFakeClient(v,
			clusterQueue(v, "cq", flavorQuotas("h100", quota("nvidia.com/gpu", "6"))),
			localQueue(v, "team-a", "lq", "cq"),
			localQueue(v, "team-b", "lq", "cq"),
		)
		snap, err := NewReader(c, Options{Namespace: "team-b"}).Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Namespace).To(HaveLen(1))
		Expect(snap.Namespace).To(HaveKey("team-b"))
		// Cluster-scoped queues are still the whole cluster's figure.
		Expect(snap.Cluster).To(Equal(map[string]int{"h100": 6}))
	})

	It("clamps a huge nominalQuota and accepts numeric quantities", func() {
		c := newFakeClient(v,
			clusterQueue(v, "cq",
				flavorQuotas("h100", quota("nvidia.com/gpu", "1G")),
				flavorQuotas("a100", quota("nvidia.com/gpu", int64(2))),
			),
			localQueue(v, "team-a", "lq", "cq"),
		)
		snap, err := NewReader(c, Options{}).Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Namespace["team-a"]).To(Equal(map[string]int{"h100": config.MaxQuotaValue, "a100": 2}))
	})

	It("reports a cluster without Kueue as an error, not as no quota", func() {
		c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
		snap, err := NewReader(c, Options{}).Quotas(ctx)
		Expect(err).To(MatchError(ContainSubstring("is Kueue installed?")))
		Expect(snap.ObservedAt).To(BeZero())
		Expect(snap.Namespace).To(BeEmpty())
	})

	It("serves the cached snapshot inside the refresh interval and re-reads after it", func() {
		now := time.Unix(1000, 0)
		lists := 0
		c := fake.NewClientBuilder().
			WithScheme(runtime.NewScheme()).
			WithRESTMapper(kueueMapper(v)).
			WithObjects(
				clusterQueue(v, "cq", flavorQuotas("h100", quota("nvidia.com/gpu", "6"))),
				localQueue(v, "team-a", "lq", "cq"),
			).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					lists++
					return cl.List(ctx, list, opts...)
				},
			}).
			Build()
		r := NewReader(c, Options{RefreshInterval: 30 * time.Second, now: func() time.Time { return now }})

		_, err := r.Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(lists).To(Equal(3), "one list per kind")

		now = now.Add(10 * time.Second)
		snap, err := r.Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(lists).To(Equal(3), "inside the interval nothing is re-read")
		Expect(snap.ObservedAt).To(Equal(time.Unix(1000, 0)))

		now = now.Add(30 * time.Second)
		snap, err = r.Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(lists).To(Equal(6))
		Expect(snap.ObservedAt).To(Equal(now))
	})

	It("keeps the last good snapshot when a read fails, and says how old it is", func() {
		now := time.Unix(1000, 0)
		fail := false
		c := fake.NewClientBuilder().
			WithScheme(runtime.NewScheme()).
			WithRESTMapper(kueueMapper(v)).
			WithObjects(
				clusterQueue(v, "cq", flavorQuotas("h100", quota("nvidia.com/gpu", "6"))),
				localQueue(v, "team-a", "lq", "cq"),
			).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if fail {
						return errors.New("forbidden: boom")
					}
					return cl.List(ctx, list, opts...)
				},
			}).
			Build()
		r := NewReader(c, Options{RefreshInterval: time.Second, now: func() time.Time { return now }})

		first, err := r.Quotas(ctx)
		Expect(err).NotTo(HaveOccurred())

		fail = true
		now = now.Add(time.Minute)
		snap, err := r.Quotas(ctx)
		Expect(err).To(MatchError(ContainSubstring("forbidden")))
		Expect(snap).To(Equal(first))
		Expect(snap.ObservedAt).To(Equal(time.Unix(1000, 0)))

		// The failure is remembered for the rest of the interval, not hidden by
		// the cache.
		now = now.Add(100 * time.Millisecond)
		_, err = r.Quotas(ctx)
		Expect(err).To(HaveOccurred())
	})
})
