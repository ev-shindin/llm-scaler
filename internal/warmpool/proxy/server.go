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
// It is an HTTP reverse proxy rather than a TCP one, which matters for two
// reasons found in review. The upstream is resolved PER REQUEST, so a client
// holding a keep-alive connection across a wake reaches the model that is awake
// now rather than the one that was awake when it connected. And responses are
// not truncated: a raw byte pump that stops when either direction closes cuts
// off a streamed completion the moment the client half-closes its request side,
// which is the ordinary shape of LLM traffic.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
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

// Server forwards HTTP requests to the currently awake instance.
//
// The zero upstream is meaningful: it means no model is awake, which is a normal
// state for an idle pool Pod. Requests arriving then are refused rather than
// queued, because the readiness probe should already be holding traffic away and
// a queued request would hide that it is not.
type Server struct {
	cfg      Config
	upstream atomic.Pointer[string]
	listener atomic.Pointer[net.Listener]
	proxy    *httputil.ReverseProxy
}

// New creates a Server. Nothing listens until Run is called.
func New(cfg Config) *Server {
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = DefaultConfig.DialTimeout
	}
	s := &Server{cfg: cfg}
	s.proxy = &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			// Resolved per request, so a wake takes effect on the next request
			// rather than the next connection.
			r.URL.Scheme = "http"
			r.URL.Host = s.Upstream()
		},
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: cfg.DialTimeout}).DialContext,
			MaxIdleConnsPerHost: 32,
		},
		// Stream token by token rather than buffering: a completion is the
		// output this proxy exists to carry, and buffering it would undo the
		// latency the pool is built for.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			klog.FromContext(r.Context()).V(2).Info("warm-pool proxy could not reach the awake instance",
				"upstream", s.Upstream(), "err", err)
			http.Error(w, "the awake instance did not answer", http.StatusBadGateway)
		},
	}
	return s
}

// ErrUpstreamNotLocal rejects an upstream outside this Pod.
var ErrUpstreamNotLocal = errors.New("upstream must be loopback: a pool Pod may only serve its own engines")

// SetUpstream points the proxy at addr ("host:port"), which must be loopback.
//
// Loopback-only is a security boundary, not a convenience. This proxy sits in a
// Pod carrying a tenant's InferencePool labels and receiving their traffic, so an
// upstream pointing anywhere else would send prompts and completions there --
// and would mark the Pod Ready while doing it. The only legitimate value is an
// engine in this same Pod, which shares its network namespace.
//
// An empty addr means no model is awake.
func (s *Server) SetUpstream(addr string) error {
	if addr == "" {
		s.upstream.Store(nil)
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("address must be host:port: %w", err)
	}
	if !isLoopback(host) {
		return fmt.Errorf("%w: got %q", ErrUpstreamNotLocal, host)
	}
	s.upstream.Store(&addr)
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
// arrives is readiness, not whether this socket is open.
func (s *Server) Run(ctx context.Context) error {
	logger := klog.FromContext(ctx).WithName("warmpool-proxy")

	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", s.cfg.Port))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", s.cfg.Port, err)
	}
	s.listener.Store(&listener)
	logger.Info("Warm-pool proxy listening", "addr", listener.Addr().String())

	server := &http.Server{
		Handler:           http.HandlerFunc(s.serve),
		ReadHeaderTimeout: 30 * time.Second,
		// No write timeout: a completion may stream for minutes, and cutting it
		// off would be the truncation this design exists to avoid.
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	logger.V(2).Info("Warm-pool proxy stopped")
	return nil
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if s.Upstream() == "" {
		http.Error(w, "no model is awake in this Pod", http.StatusServiceUnavailable)
		return
	}
	s.proxy.ServeHTTP(w, r)
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

// UpstreamHandler serves the control endpoint: GET reports the current target,
// PUT re-points the proxy, DELETE clears it.
//
// Unlike the FMA proxy this was modelled on, PUT may be called repeatedly: the
// upstream changes every time a different model is woken in this Pod, which is
// the normal case rather than an error. It is refused for any address outside
// this Pod -- see SetUpstream.
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
		if err := s.SetUpstream(body.Address); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, upstreamBody{Address: s.Upstream()})
	case http.MethodDelete:
		_ = s.SetUpstream("")
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
