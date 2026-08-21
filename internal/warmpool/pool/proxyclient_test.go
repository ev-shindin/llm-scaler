package pool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/proxy"
)

// realProxy runs the actual proxy control handler, so these tests exercise the
// two halves against each other rather than against a restatement of the
// protocol.
func realProxy(t *testing.T) (*Proxy, *proxy.Server) {
	t.Helper()
	server := proxy.New(proxy.DefaultConfig)
	mux := http.NewServeMux()
	mux.HandleFunc(proxy.UpstreamPath, server.UpstreamHandler)
	mux.HandleFunc(proxy.ReadyPath, server.ReadyHandler)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &Proxy{client: ts.Client(), baseURL: ts.URL}, server
}

func TestPointAndClearDriveTheRealProxy(t *testing.T) {
	client, server := realProxy(t)
	ctx := context.Background()

	if err := client.Point(ctx, Endpoint{PodIP: "10.0.0.5", Port: 9001}); err != nil {
		t.Fatalf("Point: %v", err)
	}
	// The address is Pod-LOCAL: the proxy shares a network namespace with the
	// engines, so it dials 127.0.0.1 whatever the Pod's cluster IP is.
	if got := server.Upstream(); got != "127.0.0.1:9001" {
		t.Fatalf("upstream = %q, want the loopback address", got)
	}

	if err := client.Clear(ctx); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if got := server.Upstream(); got != "" {
		t.Fatalf("Clear left %q behind", got)
	}
}

func TestPointingMakesThePodReadyAndClearingUnmakesIt(t *testing.T) {
	// This is the whole readiness mechanism: no Pod status is written anywhere,
	// the kubelet simply reads /readyz.
	client, server := realProxy(t)
	ctx := context.Background()

	rec := httptest.NewRecorder()
	server.ReadyHandler(rec, httptest.NewRequest(http.MethodGet, proxy.ReadyPath, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a Pod with nothing awake must not be ready: %d", rec.Code)
	}

	if err := client.Point(ctx, Endpoint{Port: 9002}); err != nil {
		t.Fatalf("Point: %v", err)
	}
	rec = httptest.NewRecorder()
	server.ReadyHandler(rec, httptest.NewRequest(http.MethodGet, proxy.ReadyPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pointing the proxy must make the Pod ready: %d", rec.Code)
	}

	if err := client.Clear(ctx); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	rec = httptest.NewRecorder()
	server.ReadyHandler(rec, httptest.NewRequest(http.MethodGet, proxy.ReadyPath, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("clearing must take the Pod out of service: %d", rec.Code)
	}
}

func TestUpstreamIsReadBackForReconciliation(t *testing.T) {
	// After a restart the controller asks the Pod what it is doing rather than
	// remembering what it asked for.
	client, _ := realProxy(t)
	ctx := context.Background()
	if err := client.Point(ctx, Endpoint{Port: 9003}); err != nil {
		t.Fatalf("Point: %v", err)
	}
	got, err := client.Upstream(ctx)
	if err != nil {
		t.Fatalf("Upstream: %v", err)
	}
	if !strings.HasSuffix(got, ":9003") {
		t.Fatalf("Upstream = %q", got)
	}
}
