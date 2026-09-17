package scaler_test

import (
	"context"
	"net"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/scaler"
)

// freePort asks the kernel for an unused loopback port and gives it back, so
// the Server under test can bind it by address the way the manager does.
func freePort() string {
	GinkgoHelper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	addr := l.Addr().String()
	Expect(l.Close()).To(Succeed())
	return addr
}

// The Server runs in two phases over one address: a standby phase until the
// lease is held, then the full server on a fresh listener. This spec drives a
// real gRPC client through both, because the hand-off -- stop one server,
// bind again, serve the other -- is the part a handler-level spec cannot see.
var _ = Describe("Server before and after the leader lease", func() {
	It("advertises the metric spec before the lease, refuses decisions, then serves them", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		s := runtime.NewScheme()
		Expect(kedav1alpha1.AddToScheme(s)).To(Succeed())
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(scaledObjectFor("chat-decode", "chat-decode-deploy")).Build()
		// The handler reads decision.Default when the Server builds it; seed
		// that rather than a private store so the post-election answer is real.
		decision.Default.Set(testNamespace, "chat-decode-deploy", 3)

		elected := make(chan struct{})
		addr := freePort()
		srv := &scaler.Server{Addr: addr, Client: c, Registry: registry.New(time.Minute), Elected: elected}
		Expect(srv.Ready(nil)).To(HaveOccurred(), "not listening yet, so not Ready")
		done := make(chan error, 1)
		go func() { done <- srv.Start(ctx) }()

		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		Expect(err).NotTo(HaveOccurred())
		defer func() { Expect(conn.Close()).To(Succeed()) }()
		cli := pb.NewExternalScalerClient(conn)

		By("standby: GetMetricSpec is answered, and the pod reads Ready")
		Eventually(func() error {
			_, err := cli.GetMetricSpec(ctx, ref("chat-decode", nil))
			return err
		}, 5*time.Second).Should(Succeed())
		Expect(srv.Ready(nil)).To(Succeed())

		By("standby: GetMetrics and IsActive are refused with Unavailable")
		_, err = cli.GetMetrics(ctx, &pb.GetMetricsRequest{ScaledObjectRef: ref("chat-decode", nil)})
		Expect(status.Code(err)).To(Equal(codes.Unavailable), "got %v", err)
		_, err = cli.IsActive(ctx, ref("chat-decode", nil))
		Expect(status.Code(err)).To(Equal(codes.Unavailable), "got %v", err)

		By("election: the same address serves decisions to the same client")
		close(elected)
		// The standby server stops and the full one binds the port again; the
		// client's connection is closed by the hand-off and re-dialled by its
		// next call, so the first attempts may see the gap.
		Eventually(func() (int64, error) {
			m, err := cli.GetMetrics(ctx, &pb.GetMetricsRequest{ScaledObjectRef: ref("chat-decode", nil)})
			if err != nil {
				return 0, err
			}
			return m.MetricValues[0].MetricValue, nil
		}, 5*time.Second).Should(Equal(int64(3)))

		Expect(srv.Ready(nil)).To(Succeed(), "still Ready across the hand-off")

		By("shutdown: cancelling the context stops the full server")
		cancel()
		Eventually(done, 5*time.Second).Should(Receive(BeNil()))
		Expect(srv.Ready(nil)).To(HaveOccurred(), "stopped, so not Ready")
	})
})
