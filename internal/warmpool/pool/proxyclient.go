package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/proxy"
)

// ProxyControlPort is where the pool Pod's proxy takes instructions. Set in the
// pool Deployment; the serving port itself is the InferencePool's business.
const ProxyControlPort = 8002

// Proxy points a pool Pod's serving port at whichever instance is awake.
//
// It is also, indirectly, how the Pod's readiness is controlled: the proxy
// answers /readyz only while it has an upstream, so clearing the upstream takes
// the Pod out of its InferencePool without anyone writing Pod status.
type Proxy struct {
	client  *http.Client
	baseURL string
}

// NewProxy addresses the proxy in the Pod at podIP.
func NewProxy(podIP string, timeout time.Duration) *Proxy {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Proxy{
		client:  &http.Client{Timeout: timeout},
		baseURL: fmt.Sprintf("http://%s:%d", podIP, ProxyControlPort),
	}
}

// Point sends the Pod's serving port to this instance, which also makes the Pod
// ready. Repointing is ordinary: it happens on every wake.
func (p *Proxy) Point(ctx context.Context, ep Endpoint) error {
	body, err := json.Marshal(map[string]string{
		"address": fmt.Sprintf("127.0.0.1:%d", ep.Port),
	})
	if err != nil {
		return fmt.Errorf("encode upstream: %w", err)
	}
	_, err = doJSON(ctx, p.client, http.MethodPut, p.baseURL+proxy.UpstreamPath, body)
	return err
}

// Clear takes the Pod out of service: no upstream, so /readyz fails and the
// kubelet marks the Pod NotReady, and the EPP stops dispatching within about
// 630 ms (measured drain).
//
// This is the FIRST step of putting a model to sleep, not the last.
func (p *Proxy) Clear(ctx context.Context) error {
	_, err := doJSON(ctx, p.client, http.MethodDelete, p.baseURL+proxy.UpstreamPath, nil)
	return err
}

// Upstream reports where the Pod is currently sending traffic, or "" if nothing
// is awake. Used to reconcile after a restart rather than to remember.
func (p *Proxy) Upstream(ctx context.Context) (string, error) {
	body, err := doJSON(ctx, p.client, http.MethodGet, p.baseURL+proxy.UpstreamPath, nil)
	if err != nil {
		return "", err
	}
	var answer struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", fmt.Errorf("decode upstream: %w", err)
	}
	return answer.Address, nil
}
