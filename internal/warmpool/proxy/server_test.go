package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// echoBackend stands in for a vLLM instance: it answers with a fixed name so a
// test can tell which instance a connection reached.
func echoBackend(t *testing.T, name string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				line, err := bufio.NewReader(c).ReadString('\n')
				if err != nil {
					return
				}
				_, _ = fmt.Fprintf(c, "%s:%s", name, strings.TrimSpace(line))
			}(conn)
		}
	}()
	return listener.Addr().String()
}

// startProxy runs a proxy on a kernel-chosen port and returns its address.
func startProxy(t *testing.T) (*Server, string) {
	t.Helper()
	s := New(Config{Port: 0, DialTimeout: 2 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for s.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.Addr() == "" {
		t.Fatal("proxy did not bind")
	}
	return s, s.Addr()
}

// ask sends one line and returns the reply, or an error if the proxy refused.
func ask(addr, msg string) (string, error) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return "", err
	}
	if _, err := fmt.Fprintf(conn, "%s\n", msg); err != nil {
		return "", err
	}
	out, err := io.ReadAll(conn)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func TestForwardsToTheAwakeInstance(t *testing.T) {
	a := echoBackend(t, "modelA")
	s, addr := startProxy(t)
	s.SetUpstream(a)

	got, err := ask(addr, "hello")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if got != "modelA:hello" {
		t.Fatalf("reached the wrong instance: %q", got)
	}
}

func TestUpstreamSwitchesWhenADifferentModelWakes(t *testing.T) {
	// The case FMA's proxy refuses by design: a pool Pod holds several models
	// and the awake one changes, so the same port must follow it.
	a, b := echoBackend(t, "modelA"), echoBackend(t, "modelB")
	s, addr := startProxy(t)

	s.SetUpstream(a)
	if got, err := ask(addr, "one"); err != nil || got != "modelA:one" {
		t.Fatalf("before switch: got %q err %v", got, err)
	}

	s.SetUpstream(b)
	if got, err := ask(addr, "two"); err != nil || got != "modelB:two" {
		t.Fatalf("after switch: got %q err %v", got, err)
	}

	// And back again, because eviction and re-wake are ordinary events.
	s.SetUpstream(a)
	if got, err := ask(addr, "three"); err != nil || got != "modelA:three" {
		t.Fatalf("after switching back: got %q err %v", got, err)
	}
}

func TestConnectionIsRefusedWhenEveryModelIsAsleep(t *testing.T) {
	// An idle pool Pod is a normal state. The connection must be closed, not
	// held: the readiness gate is what keeps traffic away, and a queued
	// connection would hide that the Pod is serving nothing.
	_, addr := startProxy(t)
	got, err := ask(addr, "anyone there")
	if err != nil {
		return // refused outright is also acceptable
	}
	if got != "" {
		t.Fatalf("expected no reply with nothing awake, got %q", got)
	}
}

func TestUnreachableUpstreamDoesNotHangTheCaller(t *testing.T) {
	s, addr := startProxy(t)
	// A port nothing listens on: dialing must fail fast and close the client.
	s.SetUpstream("127.0.0.1:1")

	done := make(chan struct{})
	go func() {
		_, _ = ask(addr, "hello")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("caller hung on an unreachable upstream")
	}
}

func TestControlEndpointReportsSetsAndClears(t *testing.T) {
	s := New(DefaultConfig)
	backend := "10.0.0.1:9001"

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
	if s.Upstream() != backend {
		t.Fatalf("Upstream() = %q", s.Upstream())
	}

	// PUT again: a second wake in the same Pod is normal, not a conflict.
	other := "10.0.0.2:9002"
	rec = httptest.NewRecorder()
	s.UpstreamHandler(rec, put(t, other))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-pointing must be allowed, got %d", rec.Code)
	}
	if s.Upstream() != other {
		t.Fatalf("second PUT did not take: %q", s.Upstream())
	}

	rec = httptest.NewRecorder()
	s.UpstreamHandler(rec, httptest.NewRequest(http.MethodDelete, UpstreamPath, nil))
	if rec.Code != http.StatusNoContent || s.Upstream() != "" {
		t.Fatalf("DELETE did not clear: code %d, upstream %q", rec.Code, s.Upstream())
	}
}

func TestControlEndpointRejectsAnAddressThatIsNotHostPort(t *testing.T) {
	s := New(DefaultConfig)
	rec := httptest.NewRecorder()
	s.UpstreamHandler(rec, put(t, "not-an-address"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a malformed address, got %d", rec.Code)
	}
	if s.Upstream() != "" {
		t.Fatalf("a rejected address must not be stored, got %q", s.Upstream())
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

	s.SetUpstream("127.0.0.1:9001")
	rec = httptest.NewRecorder()
	s.ReadyHandler(rec, httptest.NewRequest(http.MethodGet, ReadyPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("awake Pod must be ready, got %d", rec.Code)
	}

	s.SetUpstream("")
	rec = httptest.NewRecorder()
	s.ReadyHandler(rec, httptest.NewRequest(http.MethodGet, ReadyPath, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Pod must leave the pool when the model sleeps, got %d", rec.Code)
	}
}
