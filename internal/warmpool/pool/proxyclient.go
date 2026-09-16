package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// ErrDrainUnsupported says the proxy predates the drain step. The caller falls
// back to clearing first, which is the old sequence and its old 503 window.
var ErrDrainUnsupported = errors.New("this proxy image has no drain endpoint")

// Drain starts the hand-back: /readyz fails from now on, so the kubelet marks
// the Pod NotReady and the EPP stops dispatching to it, while the proxy keeps
// forwarding whatever the EPP still sends in the meantime. Reports false when
// there was nothing to drain (no upstream).
//
// This is the FIRST step of putting a model to sleep. Clear comes after the
// EPP has had time to notice -- see Adapter.Deactivate -- because clearing
// first answered every request in that window with a 503.
func (p *Proxy) Drain(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+proxy.DrainPath, nil)
	if err != nil {
		return false, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNoContent:
		return false, nil
	case http.StatusNotFound:
		return false, ErrDrainUnsupported
	default:
		return false, fmt.Errorf("drain: %s answered %d", proxy.DrainPath, resp.StatusCode)
	}
}

// Clear takes the Pod out of service: no upstream, so /readyz fails and the
// kubelet marks the Pod NotReady, and the EPP stops dispatching within about
// 630 ms (measured drain). Requests that reach the Pod after this are refused.
func (p *Proxy) Clear(ctx context.Context) error {
	_, err := doJSON(ctx, p.client, http.MethodDelete, p.baseURL+proxy.UpstreamPath, nil)
	return err
}

// Upstream reports where the Pod is currently sending traffic, or "" if nothing
// is awake. Used to reconcile after a restart rather than to remember.
func (p *Proxy) Upstream(ctx context.Context) (string, error) {
	addr, _, err := p.State(ctx)
	return addr, err
}

// State reports the upstream and whether a hand-back is in progress.
//
// Draining has to be visible here. A hand-back that drained and then failed
// -- the controller restarted in the drain wait, the clear or the unlabel
// errored -- leaves the proxy pointed at an awake engine that no longer
// takes traffic. Read as "serving", that Pod would count as covering its
// variant and never be returned; read as draining it is an orphan, and the
// next pass finishes the hand-back. Before the drain step existed the first
// action was the clear itself, so the same failures read as Waking and were
// reclaimed that way; this keeps that property.
func (p *Proxy) State(ctx context.Context) (upstream string, draining bool, err error) {
	body, err := doJSON(ctx, p.client, http.MethodGet, p.baseURL+proxy.UpstreamPath, nil)
	if err != nil {
		return "", false, err
	}
	var answer struct {
		Address  string `json:"address"`
		Draining bool   `json:"draining"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", false, fmt.Errorf("decode upstream: %w", err)
	}
	return answer.Address, answer.Draining, nil
}
