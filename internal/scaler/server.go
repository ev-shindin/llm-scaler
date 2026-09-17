package scaler

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
)

// Server is a controller-runtime manager.Runnable that serves WVA's KEDA
// external scaler over gRPC.
//
// It listens from process start on every replica, leader or not -- see
// NeedLeaderElection -- in two phases. Until this replica holds the lease it
// answers GetMetricSpec, refuses everything that needs a decision, and cycles
// each client connection every few seconds so a client that landed on a
// standby re-dials and can reach the leader. Once elected it serves in full,
// on a fresh listener, for as long as it runs. Multi-replica HA remains what
// docs/reference/configuration.md says it is: standbys wait on the lease; the
// only thing they now do for KEDA is tell it which metric to carry.
type Server struct {
	// Addr is the gRPC bind address, e.g. ":9090".
	Addr string
	// Client reads KEDA ScaledObjects to resolve scale targets. It MUST be an
	// uncached reader (manager.GetAPIReader): a cached Get of a ScaledObject
	// lazily starts a cluster-wide LIST+WATCH informer for the kind, which is the
	// watch this design removes.
	Client client.Reader
	// Registry is where incoming calls register the workloads they name — WVA's
	// discovery. Nil uses registry.Default, which is what the engines read.
	Registry *registry.Registry
	// Elected is closed once this replica holds the leader lease
	// (manager.Elected()). Nil means always elected, which is what a manager
	// without leader election reports too.
	Elected <-chan struct{}

	// listening is set once a listener is bound and cleared when the server
	// stops for good -- not across the standby-to-leader hand-off, which
	// re-binds within the same call.
	listening atomic.Bool
}

// errNotListening is what Ready reports until the scaler port is bound.
var errNotListening = errors.New("KEDA external scaler is not listening yet")

// Ready is a readiness check (healthz.Checker) that fails until the scaler is
// listening. A pod that is Ready is in the Service KEDA dials, so Ready has
// to mean "KEDA can be answered here" -- the same gate the KEDA HTTP add-on
// puts on its scaler pods with a gRPC readiness probe on the scaler port.
// Registered on the manager's /readyz so the shipped Deployment's probe
// carries it without a second probe.
func (s *Server) Ready(_ *http.Request) error {
	if !s.listening.Load() {
		return errNotListening
	}
	return nil
}

// NeedLeaderElection reports false: the server must be reachable BEFORE this
// replica is the leader.
//
// A pod is Ready, and so in the Service KEDA dials, as soon as its probes
// pass -- which on a rolling update is while the outgoing pod still holds the
// lease, and can be a minute before this one acquires it. A leader-gated
// server does not listen in that window, so KEDA's GetMetricSpec for a
// ScaledObject created then gets connection refused, and KEDA answers that by
// creating the HPA with an empty metrics list. Kubernetes defaults the empty
// list to Resource/cpu, and KEDA re-derives the HPA's metrics only when the
// ScaledObject changes -- so the HPA scales that workload on CPU, or on a
// missing metrics-server on nothing at all, until somebody edits the
// ScaledObject. Measured as a 600 s wait with WVA publishing a fresh target
// every cycle and the HPA parked on FailedGetResourceMetric.
//
// Listening early closes that: GetMetricSpec is static and is answered by any
// replica, so the HPA is wired correctly whichever pod KEDA reaches; the
// calls that need a decision are refused with Unavailable until the lease is
// held, which KEDA treats as a trigger error -- no scaling action -- and
// which, once the answers turn good, flips the ScaledObject's Ready condition
// and makes KEDA re-derive the HPA anyway.
func (s *Server) NeedLeaderElection() bool { return false }

// The manager only honours NeedLeaderElection through this interface.
var _ manager.LeaderElectionRunnable = (*Server)(nil)

// standbyConnectionAge bounds how long a client stays connected to a replica
// that is not the leader. A gRPC client keeps one connection for as long as
// it lives, and a Service picks the backend per connection, so without this
// a KEDA that dialled a standby would be refused on every poll until
// something dropped the connection. GOAWAY after this long makes the client
// re-dial on its next call -- on a single-replica rollout to the same pod,
// which is by then normally the leader; with standbys, to a fresh pick.
// In-flight calls get the grace period to finish; none takes that long.
const standbyConnectionAge = 5 * time.Second

// Start listens and serves until ctx is cancelled, then stops gracefully.
// It implements manager.Runnable.
func (s *Server) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("external-scaler")
	handler := NewHandler(s.Client, nil, s.Registry).WithElected(s.Elected)
	// On every exit, including a cancellation that lands during the hand-off.
	defer s.listening.Store(false)

	if !handler.leader() {
		logger.Info("KEDA external scaler listening before the leader lease is held; decisions refused until then", "addr", s.Addr)
		standby := []grpc.ServerOption{grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionAge:      standbyConnectionAge,
			MaxConnectionAgeGrace: standbyConnectionAge,
		})}
		if err := s.serve(ctx, handler, s.Elected, standby...); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		logger.Info("leader lease held; KEDA external scaler serving decisions", "addr", s.Addr)
	} else {
		logger.Info("KEDA external scaler listening", "addr", s.Addr)
	}
	err := s.serve(ctx, handler, nil)
	logger.Info("KEDA external scaler stopped")
	return err
}

// serve runs one gRPC server on Addr until ctx is cancelled or until closes,
// whichever first, and stops it gracefully. A nil until never closes.
func (s *Server) serve(ctx context.Context, handler pb.ExternalScalerServer, until <-chan struct{}, opts ...grpc.ServerOption) error {
	lis, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	s.listening.Store(true)
	grpcServer := grpc.NewServer(opts...)
	pb.RegisterExternalScalerServer(grpcServer, handler)
	serveErr := make(chan error, 1)
	go func() { serveErr <- grpcServer.Serve(lis) }()
	select {
	case <-ctx.Done():
		grpcServer.GracefulStop()
		return nil
	case <-until:
		// GracefulStop closes the listener and waits for in-flight calls, and
		// every call a standby accepts is short -- the handler refuses the
		// long-lived stream before the lease is held for exactly this reason.
		grpcServer.GracefulStop()
		return nil
	case err := <-serveErr:
		return err
	}
}
