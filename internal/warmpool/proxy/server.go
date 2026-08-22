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
	"strconv"
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
	// MinUpstreamPort is the lowest port an engine may listen on, and so the
	// lowest this proxy will forward to. It exists to keep the supervisor and
	// this proxy's own control endpoint -- both loopback -- out of reach.
	MinUpstreamPort int
}

// DefaultConfig is a usable configuration for a pool Pod.
var DefaultConfig = Config{
	Port:        8000,
	DialTimeout: 10 * time.Second,
	// Matches pool.BasePort. Not imported from there: this binary ships in the
	// pool Pod and must not depend on the controller's packages.
	MinUpstreamPort: 9001,
}

// Check asks a proxy in this container whether it is ready or alive, and is how
// the Pod's probes run.
//
// An exec probe rather than an httpGet one, because a kubelet probe originates
// from the NODE and a NetworkPolicy `from:` selector cannot name a node -- so
// any port this pool restricts is a port the kubelet may not be able to reach,
// and whether it can is a property of the CNI rather than of the manifest.
// Running the check inside the container sidesteps the question entirely: the
// request never leaves the Pod's network namespace.
func Check(ctx context.Context, port uint16, path string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", url, resp.StatusCode)
	}
	return nil
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

	// degraded records that a request to the upstream failed. It makes
	// readiness reflect the engine rather than the pointer at it -- see
	// ReadyHandler -- and is cleared by the next response that works.
	degraded atomic.Bool
	// health is a separate client so a readiness check cannot be starved by
	// the streaming connections the proxy transport is holding.
	health *http.Client
}

// New creates a Server. Nothing listens until Run is called.
func New(cfg Config) *Server {
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = DefaultConfig.DialTimeout
	}
	if cfg.MinUpstreamPort <= 0 {
		cfg.MinUpstreamPort = DefaultConfig.MinUpstreamPort
	}
	s := &Server{cfg: cfg, health: &http.Client{Timeout: cfg.DialTimeout}}
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
		ModifyResponse: func(*http.Response) error {
			// A response that arrived is proof the engine is there.
			s.degraded.Store(false)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Recorded, not just logged: this is how readiness learns that the
			// engine behind a perfectly valid upstream has gone.
			s.degraded.Store(true)
			klog.FromContext(r.Context()).V(2).Info("warm-pool proxy could not reach the awake instance",
				"upstream", s.Upstream(), "err", err)
			http.Error(w, "the awake instance did not answer", http.StatusBadGateway)
		},
	}
	return s
}

// ErrUpstreamNotLocal rejects an upstream outside this Pod.
var ErrUpstreamNotLocal = errors.New("upstream must be loopback: a pool Pod may only serve its own engines")

// ErrUpstreamNotAnEngine rejects a loopback address that is not an engine --
// the supervisor and this proxy's own control endpoint are loopback too.
var ErrUpstreamNotAnEngine = errors.New("upstream must be an engine port")

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
	if err := validUpstream(addr, s.cfg.MinUpstreamPort); err != nil {
		return err
	}
	s.upstream.Store(&addr)
	return nil
}

// validUpstream reports whether addr is an engine in this Pod. Separated from
// SetUpstream so the control endpoint can check the form before spending a
// request on the engine.
func validUpstream(addr string, minPort int) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("address must be host:port: %w", err)
	}
	if !isLoopback(host) {
		return fmt.Errorf("%w: got %q", ErrUpstreamNotLocal, host)
	}
	// Loopback alone is not enough. The dangerous ports in a pool Pod are ALSO
	// loopback: the supervisor spawns processes with caller-supplied argv, and
	// this proxy's own control endpoint re-points it. An upstream aimed at
	// either turns this port -- the one every Pod in the namespace may dial --
	// into a way to reach them, straight through the NetworkPolicy.
	//
	// Engines listen in the pool's own range and nothing else legitimately does,
	// so that is what is accepted.
	number, err := strconv.Atoi(port)
	if err != nil || number < minPort {
		return fmt.Errorf("%w: port %q is not in the pool's engine range (>= %d)",
			ErrUpstreamNotAnEngine, port, minPort)
	}
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
	// Nothing is intercepted here. An earlier version answered the probes on
	// this port, on the reasoning that a kubelet probe comes from the node and
	// the NetworkPolicy admits this port -- but a policy rule carrying ANY
	// `from:` restricts its sources, and this port's rule carries one, so the
	// node matches it no better than it matches the control port's. The probes
	// run `warmpool-proxy --check` inside the container instead, over loopback,
	// where no policy applies. See Check.
	if s.Upstream() == "" {
		http.Error(w, "no model is awake in this Pod", http.StatusServiceUnavailable)
		return
	}
	s.proxy.ServeHTTP(w, r)
}

// HealthHandler reports that the proxy process is up. Deliberately says nothing
// about whether a model is awake: that is ReadyHandler's job, and a liveness
// probe that failed while a Pod slept would restart it for doing what it is for.
func (s *Server) HealthHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// UpstreamPath is where the control endpoint lives.
const UpstreamPath = "/upstream"

// ReadyPath is the Pod's readiness probe.
const ReadyPath = "/readyz"

// HealthPath is the Pod's liveness probe. It reports that the proxy is up, NOT
// that a model is awake -- conflating the two is how a Pod comes to be Ready
// while asleep, which is the 503 condition this design exists to avoid.
const HealthPath = "/healthz"

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
func (s *Server) ReadyHandler(w http.ResponseWriter, r *http.Request) {
	if s.Upstream() == "" {
		http.Error(w, "no model is awake in this Pod", http.StatusServiceUnavailable)
		return
	}
	// An upstream that is set is not the same as an engine that answers. A vLLM
	// process that crashed or was OOM-killed leaves the upstream exactly as it
	// was, so readiness on the pointer alone keeps the Pod in its InferencePool
	// and every dispatched request becomes a 502 -- indefinitely, because
	// nothing else notices. Worse, the controller's eventual Deactivate cannot
	// put a dead engine to sleep, so the Pod stays lent and its GPU is lost.
	//
	// Checked only after a failure, not on every probe: the flag is set by the
	// proxy's own error handler and cleared by the next response that works, so
	// the healthy path stays a pointer read.
	if s.degraded.Load() {
		if err := s.upstreamAnswers(r.Context(), s.Upstream()); err != nil {
			http.Error(w, "the awake instance is not answering: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		s.degraded.Store(false)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

// upstreamAnswers asks the engine at addr directly, which is the only thing
// that settles whether this Pod can serve.
func (s *Server) upstreamAnswers(ctx context.Context, upstream string) error {
	if upstream == "" {
		return errors.New("no model is awake")
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.DialTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+upstream+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := s.health.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("engine answered %d", resp.StatusCode)
	}
	return nil
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
		if err := validUpstream(body.Address, s.cfg.MinUpstreamPort); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// The engine is vetted BEFORE the pointer is stored, so that "an
		// upstream is set" means "an engine answered", by construction.
		//
		// Found on a cluster: pointing at an address with nothing behind it
		// left the Pod READY, because degraded is only raised once a request
		// has actually failed. Readiness would then have admitted traffic to
		// nothing until the first request 502'd. The wake path already waits
		// for /health before it points, so this costs the ~8 ms measured
		// between /wake_up returning and the engine answering.
		if body.Address != "" {
			if err := s.upstreamAnswers(r.Context(), body.Address); err != nil {
				http.Error(w, "refusing to point at an engine that does not answer: "+err.Error(),
					http.StatusBadGateway)
				return
			}
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
