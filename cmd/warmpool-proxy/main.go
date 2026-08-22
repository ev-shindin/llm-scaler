// Command warmpool-proxy runs inside a warm-pool Pod and gives it a stable
// serving port.
//
// The Pod holds several models resident and at most one awake. Each model is a
// separate vLLM process that binds its own port for life -- a sleeping process
// keeps its socket -- so they cannot take turns binding the port the
// InferencePool dials. This process owns that port and forwards to whichever
// instance is currently awake; waking a different model is a re-point.
//
// It carries no policy. WVA decides which model is awake and tells this proxy
// where to send traffic, exactly as it tells the engine to sleep or wake.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/proxy"
)

func main() {
	// run() rather than main() so deferred cleanup happens before the exit code
	// is set: os.Exit skips defers, and the signal handler's stop() is one.
	os.Exit(run())
}

func run() int {
	var (
		servePort   uint
		controlPort uint
		dialTimeout time.Duration
		upstream    string
		check       string
	)
	flag.StringVar(&check, "check", "",
		"run as a probe instead of a server: \"ready\" or \"live\". Exits 0 if the "+
			"proxy in this container answers, 1 otherwise. This is how the Pod's "+
			"probes run -- see the comment on proxy.Check.")
	flag.UintVar(&servePort, "port", 8000,
		"port to serve on; must match the InferencePool's target port")
	flag.UintVar(&controlPort, "control-port", 8002,
		"port for the upstream control endpoint WVA calls")
	flag.DurationVar(&dialTimeout, "dial-timeout", 10*time.Second,
		"timeout for dialing the awake instance")
	flag.StringVar(&upstream, "upstream", "",
		"initial upstream host:port; empty means no model is awake yet")
	klog.InitFlags(nil)
	flag.Parse()

	logger := klog.NewKlogr().WithName("warmpool-proxy")
	ctx := klog.NewContext(context.Background(), logger)

	if check != "" {
		return runCheck(ctx, check, uint16(controlPort))
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	server := proxy.New(proxy.Config{Port: uint16(servePort), DialTimeout: dialTimeout})
	if upstream != "" {
		if err := server.SetUpstream(upstream); err != nil {
			logger.Error(err, "refusing the initial upstream")
			return 1
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc(proxy.UpstreamPath, server.UpstreamHandler)
	// Readiness: ready exactly when a model is awake here. The kubelet then
	// keeps the Pod out of its InferencePool while it sleeps, with no Pod-status
	// write and so no extra permission for the controller.
	mux.HandleFunc(proxy.ReadyPath, server.ReadyHandler)
	mux.HandleFunc(proxy.HealthPath, server.HealthHandler)
	// The Pod's probes reach these through `--check`, which runs in this
	// container and dials loopback. They are not exposed on the serving port:
	// that port is reachable by every Pod in the namespace, and wiring container
	// restarts to it would let a tenant's traffic restart this proxy.

	control := &http.Server{
		Addr:              fmt.Sprintf(":%d", controlPort),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = control.Shutdown(shutdownCtx)
	}()
	go func() {
		logger.Info("Control endpoint listening", "addr", control.Addr, "path", proxy.UpstreamPath)
		if err := control.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error(err, "Control endpoint failed")
		}
	}()

	if err := server.Run(ctx); err != nil {
		logger.Error(err, "Proxy stopped")
		return 1
	}
	return 0
}

// runCheck is the probe path: one request to this container's own control
// endpoint, and an exit code.
//
// An exec probe rather than an httpGet one because a kubelet probe originates
// from the NODE, and a NetworkPolicy `from:` selector cannot name a node. Any
// port this pool restricts is therefore a port the kubelet may or may not
// reach depending on the CNI -- a Pod that is permanently NotReady, or a
// container restart-looping, decided by something no manifest states. Running
// the check inside the container removes the question: the request never
// leaves the Pod's network namespace, so no policy applies to it.
//
// The image is distroless, so this binary is the only thing available to make
// the request with. That is why the check lives here rather than being a curl.
func runCheck(ctx context.Context, check string, controlPort uint16) int {
	logger := klog.FromContext(ctx)

	var path string
	switch check {
	case "ready":
		path = proxy.ReadyPath
	case "live":
		path = proxy.HealthPath
	default:
		logger.Error(nil, "unknown check", "check", check, "want", "ready or live")
		return 2
	}
	// Short: a probe that outlives its own period is a probe that has failed.
	if err := proxy.Check(ctx, controlPort, path, 3*time.Second); err != nil {
		logger.V(2).Info("check failed", "check", check, "err", err)
		return 1
	}
	return 0
}
