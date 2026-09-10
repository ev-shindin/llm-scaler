package scaler_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/scaler"
)

// testNamespace is the single namespace these specs operate in. The handler
// resolves a ScaledObject and its decision by (namespace, name), so the refs,
// the seeded ScaledObjects and the decision-store entries must all agree on it —
// naming it keeps that pairing explicit rather than repeating a bare literal.
const testNamespace = "chat"

func ref(name string, metadata map[string]string) *pb.ScaledObjectRef {
	return &pb.ScaledObjectRef{Namespace: testNamespace, Name: name, ScalerMetadata: metadata}
}

func ptr(v int32) *int32 { return &v }

var _ = Describe("External scaler handler", func() {
	var (
		ctx   context.Context
		store *decision.Store
	)

	// newHandler builds a Handler backed by a fake client seeded with objs and
	// the fresh per-spec decision store.
	newHandler := func(objs ...client.Object) *scaler.Handler {
		s := runtime.NewScheme()
		Expect(kedav1alpha1.AddToScheme(s)).To(Succeed())
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
		return scaler.NewHandler(c, store, registry.New(0))
	}

	scaledObject := func(namespace, name, target string) *kedav1alpha1.ScaledObject {
		return &kedav1alpha1.ScaledObject{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
			Spec: kedav1alpha1.ScaledObjectSpec{
				ScaleTargetRef: &kedav1alpha1.ScaleTarget{Name: target},
			},
		}
	}

	BeforeEach(func() {
		ctx = context.Background()
		store = decision.NewStore()
	})

	Describe("GetMetricSpec", func() {
		It("advertises the WVA metric with a target of 1", func() {
			h := newHandler()
			resp, err := h.GetMetricSpec(ctx, ref("chat-decode", nil))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.MetricSpecs).To(HaveLen(1))
			Expect(resp.MetricSpecs[0].MetricName).To(Equal(scaler.MetricName))
			Expect(resp.MetricSpecs[0].TargetSize).To(Equal(int64(1)))
		})
	})

	Describe("GetMetrics", func() {
		It("returns the desired replicas resolved via the ScaledObject's scaleTargetRef", func() {
			h := newHandler(scaledObject(testNamespace, "chat-decode", "chat-decode-deploy"))
			store.Set(testNamespace, "chat-decode-deploy", 5)

			resp, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{
				ScaledObjectRef: ref("chat-decode", nil),
				MetricName:      scaler.MetricName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.MetricValues).To(HaveLen(1))
			Expect(resp.MetricValues[0].MetricName).To(Equal(scaler.MetricName))
			Expect(resp.MetricValues[0].MetricValue).To(Equal(int64(5)))
		})

		It("returns 0 before any optimization decision exists", func() {
			h := newHandler(scaledObject(testNamespace, "chat-decode", "chat-decode-deploy"))

			resp, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{
				ScaledObjectRef: ref("chat-decode", nil),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.MetricValues[0].MetricValue).To(Equal(int64(0)))
		})

		It("ignores a scale target named in trigger metadata; the ScaledObject decides", func() {
			// A second, hand-written copy of scaleTargetRef.name can disagree with
			// the spec that actually decides what KEDA scales, and then a variant's
			// metrics are attributed to another workload. Cloning a ScaledObject to
			// add a variant used to carry such a key across and point the new entry
			// at the original's Deployment.
			h := newHandler(scaledObject(testNamespace, "chat-decode", "chat-decode-deploy"))
			store.Set(testNamespace, "chat-decode-deploy", 7)
			store.Set(testNamespace, "impostor", 99)

			resp, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{
				ScaledObjectRef: ref("chat-decode", map[string]string{"variantName": "impostor"}),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.MetricValues[0].MetricValue).To(Equal(int64(7)),
				"the metric must come from the ScaledObject's own target")
		})

		It("declines to answer when WVA has no trusted view of the workload", func() {
			// The abstain path. WVA's guards reject inputs they cannot trust,
			// and rejecting enough of them turns "the scrape is broken" into
			// "this workload looks idle" -- which is scaled DOWN. Rather than
			// serve a number built on nothing, WVA errors and lets KEDA do what
			// it already does with an erroring scaler: propagate no metric, so
			// the HPA holds, then apply spec.fallback after failureThreshold.
			// Keyed by the SCALEDOBJECT ("chat-decode"), not the scale target
			// ("chat-decode-deploy"). Publishing under the target name is the
			// bug this replaced: the generator names ScaledObjects
			// "<target>-wva", so a lookup by target name missed every verdict
			// and the abstain could never fire. The two names differ here on
			// purpose so the spec fails if the key regresses.
			trust := decision.NewTrustStore()
			trust.Observe(testNamespace, "chat-decode", "m", true, "every replica's metrics are older than the unavailable threshold", time.Now())
			h := newHandler(scaledObject(testNamespace, "chat-decode", "chat-decode-deploy")).WithTrustStore(trust)
			store.Set(testNamespace, "chat-decode-deploy", 5)

			_, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{
				ScaledObjectRef: ref("chat-decode", nil),
			})
			Expect(err).To(HaveOccurred())
			Expect(status.Code(err)).To(Equal(codes.Unavailable),
				"Unavailable is retryable, so KEDA counts it toward failureThreshold and asks again")
			Expect(err.Error()).To(ContainSubstring("unavailable threshold"),
				"the reason must reach KEDA's error, which is where an operator sees it first")
		})

		It("keeps answering the 0<->1 gate even when it will not size the fleet", func() {
			// IsActive must NOT abstain. A silent scaler reads as INACTIVE
			// there, so KEDA would scale the workload to zero on exactly the
			// evidence that says WVA cannot see it. Declining to size a fleet
			// is safe; declining to say a fleet should exist is not.
			trust := decision.NewTrustStore()
			trust.Observe(testNamespace, "chat-decode", "m", true, "stopped", time.Now())
			h := newHandler(scaledObject(testNamespace, "chat-decode", "chat-decode-deploy")).WithTrustStore(trust)
			store.Set(testNamespace, "chat-decode-deploy", 3)

			resp, err := h.IsActive(ctx, ref("chat-decode", nil))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Result).To(BeTrue())
		})

		It("answers normally for a workload the trust store has no verdict on", func() {
			// The store's silence is not evidence of a failure: a workload at
			// zero replicas, or one seen before the collector's first pass, has
			// no verdict and must not be frozen by that.
			h := newHandler(scaledObject(testNamespace, "chat-decode", "chat-decode-deploy")).
				WithTrustStore(decision.NewTrustStore())
			store.Set(testNamespace, "chat-decode-deploy", 4)

			resp, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{
				ScaledObjectRef: ref("chat-decode", nil),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.MetricValues[0].MetricValue).To(Equal(int64(4)))
		})

		It("errors when the ScaledObject is missing", func() {
			h := newHandler()

			_, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{
				ScaledObjectRef: ref("missing", nil),
			})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("IsActive", func() {
		DescribeTable("gates on WVA's decision",
			func(seed *int32, wantActive bool) {
				h := newHandler(scaledObject(testNamespace, "chat-decode", "chat-decode-deploy"))
				if seed != nil {
					store.Set(testNamespace, "chat-decode-deploy", *seed)
				}
				resp, err := h.IsActive(ctx, ref("chat-decode", nil))
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.Result).To(Equal(wantActive))
			},
			Entry("desired > 0 -> active", ptr(3), true),
			Entry("desired == 0 -> inactive", ptr(0), false),
			Entry("no decision yet -> active", nil, true),
		)
	})
})
