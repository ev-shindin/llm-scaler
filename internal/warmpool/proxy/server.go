// Package proxy forwards a pool Pod's serving port to whichever engine in that
// Pod is currently awake.
//
// A pool Pod holds several models resident and **at most one awake**. The models
// are separate vLLM processes, each binding its own port for its whole life, so
// they cannot take turns binding the port the InferencePool dials. A sleeping
// process keeps its socket.
//
// One indirection resolves it: the Pod's serving port belongs to this proxy, and
// the proxy points at the awake instance. Waking model B after model A means
// re-pointing, not rebinding.
//
// The shape is taken from Fast Model Actuation's requester proxy
// (pkg/server/requester/proxy), which solves the adjacent problem of a requester
// forwarding to a launcher's instance. The difference is the one that matters
// here: that proxy is configured ONCE and refuses a second target, because a
// requester serves one backend for its lifetime. A pool's upstream changes on
// every wake, so this one is switchable, and it forwards with the standard
// library rather than adding a dependency to the controller's module.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"k8s.io/klog/v2"
)

// Config holds the proxy's runtime parameters.
type Config struct {
	// Port is the port the proxy listens on: the one the InferencePool dials.
	Port uint16
	// DialTimeout bounds a connection to the awake instance.
	DialTimeout time.Duration
}

// DefaultConfig is a usable configuration for a pool Pod.
var DefaultConfig = Config{
	Port:        8000,
	DialTimeout: 10 * time.Second,
}

// Server forwards TCP connections to the currently awake instance.
//
// The zero upstream is meaningful: it means no model is awake, which is a normal
// state for an idle pool Pod. Connections arriving then are closed rather than
// queued, because the Pod's readiness gate should already be holding traffic away
// and a queued connection would hide that it is not.
type Server struct {
	cfg      Config
	upstream atomic.Pointer[string]
	listener atomic.Pointer[net.Listener]
}

// New creates a Server. Nothing listens until Run is called.
func New(cfg Config) *Server {
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = DefaultConfig.DialTimeout
	}
	return &Server{cfg: cfg}
}

// SetUpstream points the proxy at addr ("host:port"). An empty addr means no
// model is awake and connections should be refused.
//
// Existing connections are left alone: they are already proxied to the instance
// that was awake when they arrived, and a wake is always preceded by draining
// the previous model, so there is nothing to cut short.
func (s *Server) SetUpstream(addr string) {
	if addr == "" {
		s.upstream.Store(nil)
		return
	}
	s.upstream.Store(&addr)
}

// Upstream reports the current target, or "" when no model is awake.
func (s *Server) Upstream() string {
	if p := s.upstream.Load(); p != nil {
		return *p
	}
	return ""
}

// Addr reports the address the proxy is listening on, or "" before Run has
// bound it. Useful in tests, where the port is chosen by the kernel.
func (s *Server) Addr() string {
	if l := s.listener.Load(); l != nil {
		return (*l).Addr().String()
	}
	return ""
}

// Run listens and forwards until ctx is cancelled.
//
// It binds immediately rather than waiting for an upstream, so that a Pod's
// serving port exists from the moment the Pod does. What decides whether traffic
// arrives is the readiness gate, not whether this socket is open.
func (s *Server) Run(ctx context.Context) error {
	logger := klog.FromContext(ctx).WithName("warmpool-proxy")

	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", s.cfg.Port))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", s.cfg.Port, err)
	}
	s.listener.Store(&listener)
	logger.Info("Warm-pool proxy listening", "addr", listener.Addr().String())

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				logger.V(2).Info("Warm-pool proxy stopped")
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go s.forward(ctx, conn)
	}
}

// forward copies between the caller and the awake instance in both directions,
// half-closing each side as it finishes so that a client which has sent its
// whole request still receives the whole response.
func (s *Server) forward(ctx context.Context, client net.Conn) {
	defer func() { _ = client.Close() }()
	logger := klog.FromContext(ctx).WithName("warmpool-proxy")

	target := s.Upstream()
	if target == "" {
		logger.V(4).Info("Refusing connection: no model is awake in this Pod")
		return
	}

	dialer := net.Dialer{Timeout: s.cfg.DialTimeout}
	backend, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		logger.V(2).Info("Cannot reach the awake instance", "target", target, "err", err)
		return
	}
	defer func() { _ = backend.Close() }()

	done := make(chan struct{}, 2)
	go copyThenHalfClose(backend, client, done)
	go copyThenHalfClose(client, backend, done)
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// halfCloser is implemented by *net.TCPConn: it lets one direction finish while
// the other is still in flight, which a plain Close would cut off.
type halfCloser interface{ CloseWrite() error }

func copyThenHalfClose(dst, src net.Conn, done chan<- struct{}) {
	_, _ = io.Copy(dst, src)
	if hc, ok := dst.(halfCloser); ok {
		_ = hc.CloseWrite()
	}
	done <- struct{}{}
}

// UpstreamPath is where the control endpoint lives.
const UpstreamPath = "/upstream"

// ReadyPath is the Pod's readiness probe.
const ReadyPath = "/readyz"

// ReadyHandler reports whether this Pod should receive traffic: it is ready
// exactly when a model is awake in it.
//
// This is how the Pod is kept out of its InferencePool while asleep, and it is
// deliberately an ordinary readinessProbe rather than a readiness GATE that WVA
// patches. Fast Model Actuation reaches the same conclusion from the other side:
// it never writes Pod status, and derives the serving port from the requester's
// own readinessProbe, leaving readiness to the kubelet. Doing the same here
// means the controller needs no `pods/status` permission, and a Pod whose
// controller has died still stops taking traffic when its model sleeps.
func (s *Server) ReadyHandler(w http.ResponseWriter, _ *http.Request) {
	if s.Upstream() == "" {
		http.Error(w, "no model is awake in this Pod", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

type upstreamBody struct {
	Address string `json:"address"`
}

// Upstreamhandler serves the control endpoint: GET reports the current target,
// PUT re-points the proxy, DELETE clears it.
//
// Unlike the FMA proxy this was modelled on, PUT may be called repeatedly: the
// upstream changes every time a different model is woken in this Pod, which is
// the normal case rather than an error.
func (s *Server) UpstreamHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, upstreamBody{Address: s.Upstream()})
	case http.MethodPut:
		var body upstreamBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, fmt.Sprintf("decode body: %v", err), http.StatusBadRequest)
			return
		}
		if body.Address != "" {
			if _, _, err := net.SplitHostPort(body.Address); err != nil {
				http.Error(w, fmt.Sprintf("address must be host:port: %v", err), http.StatusBadRequest)
				return
			}
		}
		s.SetUpstream(body.Address)
		writeJSON(w, http.StatusOK, upstreamBody{Address: s.Upstream()})
	case http.MethodDelete:
		s.SetUpstream("")
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
