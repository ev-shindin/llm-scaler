package scaler

import (
	"context"
	"errors"
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
// NeedLeaderElection -- in two phases. Until this replica holds the lease a
// standby server answers GetMetricSpec, refuses everything that needs a
// decision, and cycles each client connection every few seconds so a client
// that landed on a standby re-dials and can reach the leader. On election the
// full server binds the same port alongside it (SO_REUSEPORT, see listen), the
// standby is stopped, and the full server serves for as long as it runs.
// Multi-replica HA remains what docs/reference/configuration.md says it is:
// standbys wait on the lease; the only thing they now do for KEDA is tell it
// which metric to carry.
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
	// (manager.Elected()). Nil means always elected. Pass nil when leader
	// election is off: the manager's channel does close in that case too, but
	// only after it has started this runnable, so the standby phase would run
	// for the moments in between and hand off for nothing.
	Elected <-chan struct{}

	// listening is set once a listener is bound and cleared when Start returns.
	// Where the platform allows it the hand-off binds the full server before
	// stopping the standby, so there is no moment in between at which it would
	// be false; elsewhere the gap is sub-millisecond and left as is.
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

// stopGrace bounds a graceful stop. Unary calls finish in milliseconds, but a
// StreamIsActive is held open by KEDA for as long as KEDA likes, and a
// GracefulStop waits for it -- through the manager's whole shutdown budget,
// with the leader lease released only after that, which is precisely the
// window the standby phase exists to cover. After this long the stop is
// forced; KEDA re-opens a dropped stream on its own.
const stopGrace = 2 * time.Second

// neverElected gates the standby server's handler: whichever way the lease
// goes, a call that reached the standby is answered as a standby. Gating it
// on the real lease instead let a stream that arrived after election but
// before the standby drained pass the check, subscribe, and hold the
// standby's stop open.
var neverElected = make(chan struct{})

// Start listens and serves until ctx is cancelled, then stops gracefully.
// It implements manager.Runnable.
func (s *Server) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("external-scaler")
	// On every exit, including a cancellation that lands during the hand-off.
	defer s.listening.Store(false)

	full := NewHandler(s.Client, nil, s.Registry).WithElected(s.Elected)
	if full.leader() {
		logger.Info("KEDA external scaler listening", "addr", s.Addr)
		srv, err := s.bind(ctx, full)
		if err != nil {
			return err
		}
		return s.run(ctx, srv)
	}

	logger.Info("KEDA external scaler listening before the leader lease is held; decisions refused until then", "addr", s.Addr)
	standby, err := s.bind(ctx, NewHandler(s.Client, nil, s.Registry).WithElected(neverElected),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionAge:      standbyConnectionAge,
			MaxConnectionAgeGrace: standbyConnectionAge,
		}))
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		standby.stop()
		return nil
	case err := <-standby.err:
		return err
	case <-s.Elected:
	}

	// Bind the full server BEFORE stopping the standby where the platform
	// allows it (overlapBind), so the port is never unbound in between: a
	// GetMetricSpec refused in that gap would leave an HPA on the CPU default
	// with nothing left to flip it back, since the full server refuses
	// nothing. Where it does not, the standby has to go first.
	var srv *served
	if overlapBind {
		srv, err = s.bind(ctx, full)
		if err != nil {
			standby.stop()
			return err
		}
		standby.stop()
	} else {
		standby.stop()
		if srv, err = s.bind(ctx, full); err != nil {
			return err
		}
	}
	logger.Info("leader lease held; KEDA external scaler serving decisions", "addr", s.Addr)
	return s.run(ctx, srv)
}

// served is one gRPC server and the channel its Serve goroutine reports on.
type served struct {
	srv *grpc.Server
	err chan error
}

// bind listens on Addr and starts serving handler; the caller owns the stop.
func (s *Server) bind(ctx context.Context, handler pb.ExternalScalerServer, opts ...grpc.ServerOption) (*served, error) {
	lis, err := listen(ctx, s.Addr)
	if err != nil {
		return nil, err
	}
	s.listening.Store(true)
	r := &served{srv: grpc.NewServer(opts...), err: make(chan error, 1)}
	pb.RegisterExternalScalerServer(r.srv, handler)
	go func() { r.err <- r.srv.Serve(lis) }()
	return r, nil
}

// run waits for ctx or for Serve to fail, and stops the server on the former.
func (s *Server) run(ctx context.Context, r *served) error {
	logger := log.FromContext(ctx).WithName("external-scaler")
	select {
	case <-ctx.Done():
		r.stop()
		logger.Info("KEDA external scaler stopped")
		return nil
	case err := <-r.err:
		return err
	}
}

// stop drains the server for at most stopGrace, then forces it.
func (r *served) stop() {
	done := make(chan struct{})
	go func() {
		r.srv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(stopGrace):
		r.srv.Stop()
		<-done
	}
}
