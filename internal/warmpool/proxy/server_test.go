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
	mustSetUpstream(t, s, "127.0.0.1:1") // nothing listens there

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
	s := New(DefaultConfig)
	backend := net.JoinHostPort(loopbackHost, "9001")

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
	other := net.JoinHostPort(loopbackHost, "9002")
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

func TestTheProbesAreAnsweredOnTheServingPort(t *testing.T) {
	// The Pod's probes point here, not at the control port. A kubelet probe
	// originates from the node, which no NetworkPolicy `from:` selector can
	// name, and this pool's policy restricts the control port to the
	// controller -- so probing it relies on the CNI permitting host traffic,
	// which is an implementation detail rather than a guarantee. Probing the
	// serving port also catches the failure readiness exists for: a wedged
	// serving listener behind a healthy control port.
	s, base := startProxy(t)

	// Asleep: live, but not ready. Both halves matter -- a liveness probe that
	// failed while the Pod slept would restart it for doing its job.
	if code, _ := probe(t, base+HealthPath); code != http.StatusOK {
		t.Errorf("%s on the serving port = %d, want 200 while asleep", HealthPath, code)
	}
	if code, _ := probe(t, base+ReadyPath); code != http.StatusServiceUnavailable {
		t.Errorf("%s on the serving port = %d, want 503 while asleep", ReadyPath, code)
	}

	mustSetUpstream(t, s, engine(t, "qwen"))
	if code, _ := probe(t, base+ReadyPath); code != http.StatusOK {
		t.Errorf("%s = %d, want 200 once a model is awake", ReadyPath, code)
	}
	if code, _ := probe(t, base+HealthPath); code != http.StatusOK {
		t.Errorf("%s = %d, want 200 once a model is awake", HealthPath, code)
	}
}

func TestTheProbePathsAreNotForwardedToTheEngine(t *testing.T) {
	// Intercepted rather than proxied. An awake Pod would otherwise report
	// whatever the engine says at those paths, and vLLM serves neither, so
	// readiness would answer 404 and the Pod would never join its InferencePool.
	s, base := startProxy(t)
	mustSetUpstream(t, s, engine(t, "qwen"))

	for _, path := range []string{ReadyPath, HealthPath} {
		code, body := probe(t, base+path)
		if code != http.StatusOK {
			t.Errorf("%s = %d, want 200 from the proxy itself", path, code)
		}
		if strings.Contains(body, "qwen") {
			t.Errorf("%s was answered by the engine (%q); it must not be forwarded", path, body)
		}
	}

	// And every other path still reaches the engine.
	if code, body := probe(t, base+"/v1/models"); code != http.StatusOK || !strings.Contains(body, "qwen") {
		t.Errorf("/v1/models = %d %q: ordinary paths must still be forwarded", code, body)
	}
}
