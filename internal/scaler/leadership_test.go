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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/scaler"
)

// The server listens before this replica holds the leader lease, so that
// KEDA's one-time GetMetricSpec at ScaledObject creation is answered even in
// the window between a pod becoming Ready and its winning the lease. These
// specs hold the contract that makes that safe: the static answer is always
// given, and anything that would read a decision store nobody is feeding is
// refused rather than answered with "no decision" -- which HPA would act on.
var _ = Describe("Answering before the leader lease is held", func() {
	var (
		ctx     context.Context
		store   *decision.Store
		reg     *registry.Registry
		elected chan struct{}
	)

	newHandler := func(objs ...client.Object) *scaler.Handler {
		s := runtime.NewScheme()
		Expect(kedav1alpha1.AddToScheme(s)).To(Succeed())
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
		return scaler.NewHandler(c, store, reg).WithElected(elected)
	}

	BeforeEach(func() {
		ctx = context.Background()
		store = decision.NewStore()
		reg = registry.New(time.Minute)
		elected = make(chan struct{})
	})

	It("advertises the metric spec, and registers the caller, without the lease", func() {
		h := newHandler()
		resp, err := h.GetMetricSpec(ctx, ref("chat-decode", map[string]string{registry.ModelIDKey: "default/default"}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.MetricSpecs).To(HaveLen(1))
		Expect(resp.MetricSpecs[0].MetricName).To(Equal(scaler.MetricName))

		// The engines start on election and read the registry; a workload seen
		// before that must already be there.
		_, ok := reg.Get(testNamespace, "chat-decode")
		Expect(ok).To(BeTrue(), "GetMetricSpec before the lease did not register the workload")
	})

	It("refuses GetMetrics with Unavailable rather than answering 0", func() {
		// A decision exists in this replica's store only because the spec put
		// it there; a real standby's store is empty, and 0 from an empty store
		// would read to HPA as "scale to minReplicas".
		store.Set(testNamespace, "chat-decode-deploy", 3)
		h := newHandler(scaledObjectFor("chat-decode", "chat-decode-deploy"))

		_, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{ScaledObjectRef: ref("chat-decode", nil)})
		Expect(status.Code(err)).To(Equal(codes.Unavailable), "got %v", err)
	})

	It("refuses IsActive with Unavailable", func() {
		h := newHandler(scaledObjectFor("chat-decode", "chat-decode-deploy"))
		_, err := h.IsActive(ctx, ref("chat-decode", nil))
		Expect(status.Code(err)).To(Equal(codes.Unavailable), "got %v", err)
	})

	It("answers both once the lease is held", func() {
		store.Set(testNamespace, "chat-decode-deploy", 3)
		h := newHandler(scaledObjectFor("chat-decode", "chat-decode-deploy"))
		close(elected)

		m, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{ScaledObjectRef: ref("chat-decode", nil)})
		Expect(err).NotTo(HaveOccurred())
		Expect(m.MetricValues[0].MetricValue).To(Equal(int64(3)))

		a, err := h.IsActive(ctx, ref("chat-decode", nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(a.Result).To(BeTrue())
	})

	It("refuses StreamIsActive with Unavailable, having registered the caller", func() {
		h := newHandler(scaledObjectFor("chat-decode", "chat-decode-deploy"))
		stream := newFakeStream(ctx)
		err := h.StreamIsActive(ref("chat-decode", map[string]string{registry.ModelIDKey: "default/default"}), stream)
		Expect(status.Code(err)).To(Equal(codes.Unavailable), "got %v", err)
		stream.expectNoPush()
	})

	It("serves a StreamIsActive once the lease is held", func() {
		store.Set(testNamespace, "chat-decode-deploy", 1)
		h := newHandler(scaledObjectFor("chat-decode", "chat-decode-deploy"))
		close(elected)

		streamCtx, cancel := context.WithCancel(ctx)
		stream := newFakeStream(streamCtx)
		done := make(chan error, 1)
		go func() { done <- h.StreamIsActive(ref("chat-decode", nil), stream) }()
		Expect(stream.nextPush()).To(BeTrue())
		cancel()
		Eventually(done).Should(Receive(BeNil()))
	})

	It("treats a nil channel as always elected", func() {
		elected = nil
		store.Set(testNamespace, "chat-decode-deploy", 2)
		h := newHandler(scaledObjectFor("chat-decode", "chat-decode-deploy"))
		m, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{ScaledObjectRef: ref("chat-decode", nil)})
		Expect(err).NotTo(HaveOccurred())
		Expect(m.MetricValues[0].MetricValue).To(Equal(int64(2)))
	})
})
