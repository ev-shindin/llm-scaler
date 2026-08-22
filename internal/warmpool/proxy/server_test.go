package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const loopbackHost = "127.0.0.1"

// engine stands in for a vLLM instance: it answers with its own name so a test
// can tell which instance a request reached.
func engine(t *testing.T, name string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stream" {
			streamFrom(w, name)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_, _ = fmt.Fprintf(w, "%s:%s", name, strings.TrimSpace(string(body)))
	}))
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

// streamFrom writes a long response in chunks, which is what a completion looks
// like and what a truncating proxy cuts off.
func streamFrom(w http.ResponseWriter, name string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	for i := 0; i < 20; i++ {
		_, _ = fmt.Fprintf(w, "%s-%02d\n", name, i)
		flusher.Flush()
		time.Sleep(time.Millisecond)
	}
}

// startProxy runs a proxy on a kernel-chosen port and returns its address.
func startProxy(t *testing.T) (*Server, string) {
	t.Helper()
	s := New(Config{Port: 0, DialTimeout: 2 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for s.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.Addr() == "" {
		t.Fatal("proxy did not bind")
	}
	return s, "http://" + s.Addr()
}

func mustSetUpstream(t *testing.T, s *Server, addr string) {
	t.Helper()
	if err := s.SetUpstream(addr); err != nil {
		t.Fatalf("SetUpstream(%q): %v", addr, err)
	}
}

// ask sends one request and returns status and body.
func ask(t *testing.T, client *http.Client, base, body string) (int, string) {
	t.Helper()
	resp, err := client.Post(base+"/v1/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return resp.StatusCode, string(out)
}

func TestForwardsToTheAwakeInstance(t *testing.T) {
	a := engine(t, "modelA")
	s, base := startProxy(t)
	mustSetUpstream(t, s, a)

	code, body := ask(t, http.DefaultClient, base, "hello")
	if code != http.StatusOK || body != "modelA:hello" {
		t.Fatalf("got %d %q", code, body)
	}
}

func TestAKeepAliveClientFollowsTheWake(t *testing.T) {
	// The defect this rewrite fixes. A TCP proxy resolves its upstream once per
	// CONNECTION, so a client holding a keep-alive connection across a wake goes
	// on talking to the instance that was awake when it connected -- which by
	// then is asleep.
	a, b := engine(t, "modelA"), engine(t, "modelB")
	s, base := startProxy(t)
	// One connection, reused for every request, exactly as the EPP does.
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1}}

	mustSetUpstream(t, s, a)
	if _, body := ask(t, client, base, "one"); body != "modelA:one" {
		t.Fatalf("before the switch: %q", body)
	}

	mustSetUpstream(t, s, b)
	if code, body := ask(t, client, base, "two"); body != "modelB:two" {
		t.Fatalf("after the switch the SAME client must reach model B: %d %q", code, body)
	}
}

func TestAStreamedResponseIsNotTruncated(t *testing.T) {
	// A completion is long and the request that asked for it is short. A proxy
	// that stops when either direction finishes cuts the response off; this
	// asserts every chunk arrives.
	a := engine(t, "modelA")
	s, base := startProxy(t)
	mustSetUpstream(t, s, a)

	resp, err := http.Get(base + "/stream")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	lines := 0
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		lines++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading the stream failed: %v", err)
	}
	if lines != 20 {
		t.Fatalf("got %d chunks, want 20: the response was truncated", lines)
	}
}

func TestRequestsAreRefusedWhenEveryModelIsAsleep(t *testing.T) {
	// An idle pool Pod is a normal state. Refused, not queued: readiness is what
	// keeps traffic away, and queuing would hide that the Pod serves nothing.
	_, base := startProxy(t)
	code, _ := ask(t, http.DefaultClient, base, "anyone there")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 with nothing awake, got %d", code)
	}
}

func TestAnUnreachableUpstreamIsABadGatewayNotAHang(t *testing.T) {
	s, base := startProxy(t)
	mustSetUpstream(t, s, deadEngine(t))

	done := make(chan int, 1)
	go func() {
		code, _ := ask(t, http.DefaultClient, base, "hello")
		done <- code
	}()
	select {
	case code := <-done:
		if code != http.StatusBadGateway {
			t.Fatalf("want 502, got %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("caller hung on an unreachable upstream")
	}
}

func TestUpstreamMustBeLoopback(t *testing.T) {
	// A pool Pod carries a tenant's InferencePool labels and receives their
	// traffic. An upstream elsewhere would send prompts and completions there,
	// and would mark the Pod Ready while doing it.
	s := New(DefaultConfig)
	for _, addr := range []string{
		"attacker.evil.svc:80",
		"10.0.0.5:9001",
		"[2001:db8::1]:9001",
		"0.0.0.0:9001",
	} {
		if err := s.SetUpstream(addr); !errors.Is(err, ErrUpstreamNotLocal) {
			t.Errorf("SetUpstream(%q) = %v, want ErrUpstreamNotLocal", addr, err)
		}
		if s.Upstream() != "" {
			t.Fatalf("a refused address must not be stored: %q", s.Upstream())
		}
	}
	for _, addr := range []string{"127.0.0.1:9001", "localhost:9002", "[::1]:9003"} {
		if err := s.SetUpstream(addr); err != nil {
			t.Errorf("SetUpstream(%q) = %v, want it accepted", addr, err)
		}
	}
}

func TestTheControlEndpointRefusesARemoteUpstream(t *testing.T) {
	s := New(DefaultConfig)
	rec := httptest.NewRecorder()
	s.UpstreamHandler(rec, put(t, "attacker.evil.svc:80"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a remote upstream, got %d", rec.Code)
	}
	if s.Upstream() != "" {
		t.Fatalf("nothing must be stored: %q", s.Upstream())
	}
}

func TestControlEndpointReportsSetsAndClears(t *testing.T) {
	// Real engines, because the control endpoint now vets one before it will
	// point at it: an upstream that is set must mean an engine answered.
	s := New(DefaultConfig)
	backend := engine(t, "qwen")

	rec := httptest.NewRecorder()
	s.UpstreamHandler(rec, httptest.NewRequest(http.MethodGet, UpstreamPath, nil))
	if body := decode(t, rec); body.Address != "" {
		t.Fatalf("a new proxy has no upstream, got %q", body.Address)
	}

	rec = httptest.NewRecorder()
	s.UpstreamHandler(rec, put(t, backend))
	if body := decode(t, rec); body.Address != backend {
		t.Fatalf("PUT did not set the upstream: %q", body.Address)
	}

	// PUT again: a second wake in the same Pod is normal, not a conflict.
	other := engine(t, "llama")
	rec = httptest.NewRecorder()
	s.UpstreamHandler(rec, put(t, other))
	if rec.Code != http.StatusOK || s.Upstream() != other {
		t.Fatalf("re-pointing must be allowed: code %d, upstream %q", rec.Code, s.Upstream())
	}

	rec = httptest.NewRecorder()
	s.UpstreamHandler(rec, httptest.NewRequest(http.MethodDelete, UpstreamPath, nil))
	if rec.Code != http.StatusNoContent || s.Upstream() != "" {
		t.Fatalf("DELETE did not clear: code %d, upstream %q", rec.Code, s.Upstream())
	}
}

func TestControlEndpointRejectsMalformedInput(t *testing.T) {
	s := New(DefaultConfig)

	rec := httptest.NewRecorder()
	s.UpstreamHandler(rec, httptest.NewRequest(http.MethodPut, UpstreamPath, strings.NewReader("not json")))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body: want 400, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.UpstreamHandler(rec, put(t, "not-an-address"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed address: want 400, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.UpstreamHandler(rec, httptest.NewRequest(http.MethodPatch, UpstreamPath, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("unsupported method: want 405, got %d", rec.Code)
	}
}

func TestReadinessFollowsWhetherAModelIsAwake(t *testing.T) {
	// The Pod must leave its InferencePool when it sleeps. Doing that through an
	// ordinary readinessProbe rather than a patched readiness gate is what keeps
	// the controller out of `pods/status` -- and means a Pod stops taking
	// traffic even if the controller is gone.
	s := New(DefaultConfig)

	rec := httptest.NewRecorder()
	s.ReadyHandler(rec, httptest.NewRequest(http.MethodGet, ReadyPath, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("asleep Pod must not be ready, got %d", rec.Code)
	}

	mustSetUpstream(t, s, net.JoinHostPort(loopbackHost, "9001"))
	rec = httptest.NewRecorder()
	s.ReadyHandler(rec, httptest.NewRequest(http.MethodGet, ReadyPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("awake Pod must be ready, got %d", rec.Code)
	}

	_ = s.SetUpstream("")
	rec = httptest.NewRecorder()
	s.ReadyHandler(rec, httptest.NewRequest(http.MethodGet, ReadyPath, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Pod must leave the pool when the model sleeps, got %d", rec.Code)
	}
}

func put(t *testing.T, addr string) *http.Request {
	t.Helper()
	body, err := json.Marshal(upstreamBody{Address: addr})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return httptest.NewRequest(http.MethodPut, UpstreamPath, strings.NewReader(string(body)))
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) upstreamBody {
	t.Helper()
	var body upstreamBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (%q)", err, rec.Body.String())
	}
	return body
}

// probe sends a GET and returns status and body, which is what a kubelet does.
func probe(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(body)
}

func TestTheProbesAreNotReachableOnTheServingPort(t *testing.T) {
	// The serving port is reachable by every Pod in the namespace. Answering
	// the probes there would wire container restarts to traffic a tenant
	// controls: ~30 s of slow responses under a per-token flush is enough to
	// trip a liveness probe, and this proxy cannot tell a slow client from a
	// hostile one.
	//
	// An earlier version did intercept them here, reasoning that a kubelet
	// probe comes from the node and this port's NetworkPolicy rule admits it.
	// That was wrong: a rule carrying ANY `from:` restricts its sources, and
	// this one carries `podSelector: {}`. The node matches it no better than it
	// matches the control port's rule.
	s, base := startProxy(t)
	mustSetUpstream(t, s, engine(t, "qwen"))

	for _, path := range []string{ReadyPath, HealthPath} {
		code, body := probe(t, base+path)
		if code != http.StatusOK || !strings.Contains(body, "qwen") {
			t.Errorf("%s on the serving port = %d %q, want it forwarded to the engine",
				path, code, body)
		}
	}
}

func TestCheckIsHowTheProbesRun(t *testing.T) {
	// Exec rather than httpGet, because the request then never leaves the Pod's
	// network namespace and no NetworkPolicy applies to it. The image is
	// distroless, so this binary making the request is the only option.
	s := New(DefaultConfig)
	mux := http.NewServeMux()
	mux.HandleFunc(ReadyPath, s.ReadyHandler)
	mux.HandleFunc(HealthPath, s.HealthHandler)
	control := httptest.NewServer(mux)
	defer control.Close()

	port := portOfURL(t, control.URL)
	ctx := context.Background()

	// Liveness says the proxy is up; readiness says a model is awake here. The
	// difference is the whole point: a liveness probe that failed while the Pod
	// slept would restart it for doing its job.
	if err := Check(ctx, port, HealthPath, time.Second); err != nil {
		t.Errorf("live check must pass while asleep: %v", err)
	}
	if err := Check(ctx, port, ReadyPath, time.Second); err == nil {
		t.Error("ready check must fail while asleep")
	}

	mustSetUpstream(t, s, engine(t, "qwen"))
	if err := Check(ctx, port, ReadyPath, time.Second); err != nil {
		t.Errorf("ready check must pass once a model is awake: %v", err)
	}
}

func TestReadinessFollowsTheEngineAndNotJustThePointer(t *testing.T) {
	// An upstream that is set is not an engine that answers. A vLLM process
	// that crashed or was OOM-killed leaves the pointer exactly as it was, so
	// readiness on the pointer alone keeps the Pod in its InferencePool and
	// every dispatched request becomes a 502 -- indefinitely, because nothing
	// else notices. The controller's eventual Deactivate cannot sleep a dead
	// engine either, so the Pod stays lent and its GPU is lost.
	s, base := startProxy(t)
	mustSetUpstream(t, s, engine(t, "qwen"))

	rec := httptest.NewRecorder()
	s.ReadyHandler(rec, httptest.NewRequest(http.MethodGet, ReadyPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("a live engine must read as ready, got %d", rec.Code)
	}

	// The engine dies. Nothing tells the proxy; it finds out by failing.
	mustSetUpstream(t, s, deadEngine(t))
	if code, _ := probe(t, base+"/v1/models"); code != http.StatusBadGateway {
		t.Fatalf("a request to a dead engine = %d, want 502", code)
	}

	rec = httptest.NewRecorder()
	s.ReadyHandler(rec, httptest.NewRequest(http.MethodGet, ReadyPath, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness = %d after the engine died, want 503", rec.Code)
	}
}

func TestReadinessRecoversWhenTheEngineComesBack(t *testing.T) {
	// Latching would be worse than not checking at all: a Pod marked NotReady
	// takes no traffic, so nothing would ever produce the success that clears
	// the flag. The check has to be able to un-fail.
	s := New(DefaultConfig)
	mustSetUpstream(t, s, engine(t, "qwen"))

	s.degraded.Store(true)
	rec := httptest.NewRecorder()
	s.ReadyHandler(rec, httptest.NewRequest(http.MethodGet, ReadyPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readiness = %d, want the live engine to clear the flag", rec.Code)
	}
	if s.degraded.Load() {
		t.Error("the flag must be cleared once the engine answers again")
	}
}

func TestAnUpstreamMustBeAnEngineAndNotJustLoopback(t *testing.T) {
	// The dangerous ports in a pool Pod are loopback too. The supervisor spawns
	// processes with caller-supplied argv in a container mounting the shared
	// model cache read-write, and this proxy's own control endpoint re-points
	// it. An upstream aimed at either would turn the serving port -- the one
	// every Pod in the namespace may dial -- into a route to them, straight
	// through the NetworkPolicy that exists to keep them closed.
	s := New(DefaultConfig)
	for _, addr := range []string{
		"127.0.0.1:8001", // the FMA supervisor: arbitrary execution
		"127.0.0.1:8002", // this proxy's own control endpoint
		"127.0.0.1:8000", // its serving port, which would be a loop
		"127.0.0.1:22",
	} {
		if err := s.SetUpstream(addr); !errors.Is(err, ErrUpstreamNotAnEngine) {
			t.Errorf("SetUpstream(%q) = %v, want it refused as not an engine", addr, err)
		}
	}
	if s.Upstream() != "" {
		t.Errorf("a refused upstream must not be stored, got %q", s.Upstream())
	}
	if err := s.SetUpstream(net.JoinHostPort(loopbackHost, "9001")); err != nil {
		t.Errorf("SetUpstream on an engine port: %v", err)
	}
}

// deadEngine returns an address in the engine range that is proven to have
// nothing behind it: a server is bound and then closed, so the kernel has told
// us the port was free rather than us guessing one. Guessing found a port this
// developer's machine was already using, and the test failed with a 404 from
// something else entirely.
func deadEngine(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := strings.TrimPrefix(server.URL, "http://")
	server.Close()
	return addr
}

// portOfURL extracts the port an httptest server bound.
func portOfURL(t *testing.T, raw string) uint16 {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(raw, "http://"))
	if err != nil {
		t.Fatalf("split %q: %v", raw, err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("port %q: %v", port, err)
	}
	return uint16(number)
}
