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
	)
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
	// Both are also answered on the SERVING port, which is where the Pod's
	// probes point: the control port is restricted to the controller by this
	// pool's NetworkPolicy, and a kubelet probe comes from the node, which no
	// `from:` selector can name. They stay here because a human debugging a
	// pool Pod reaches the control port, not the tenant's serving port.

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
