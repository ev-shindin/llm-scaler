package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

// Engine talks to one vLLM instance inside a pool Pod.
//
// Sleeping and waking are the engine's own API, not the supervisor's, and the
// control plane calls them directly -- which is how Fast Model Actuation does it
// too (its controller hits /wake_up, /sleep and /is_sleeping on the instance).
// Both require VLLM_SERVER_DEV_MODE=1 in the instance's environment; without it
// the routes do not exist, and the pool silently has no mechanism at all.
type Engine struct {
	client   *http.Client
	baseURL  string
	pollWait time.Duration
}

// NewEngine addresses one instance at its endpoint.
func NewEngine(ep Endpoint, timeout time.Duration) *Engine {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Engine{
		client:   &http.Client{Timeout: timeout},
		baseURL:  fmt.Sprintf("http://%s:%d", ep.PodIP, ep.Port),
		pollWait: 20 * time.Millisecond,
	}
}

// SleepLevel is vLLM's own notion, derived from the tier the pool was built for.
//
// Level 1 offloads the weights to host memory; level 2 discards them and re-reads
// on wake. The choice belongs to the pool, not to a call site: a level-1 Pod must
// have reserved the host memory when it was created.
func SleepLevel(tier Tier) int {
	if tier == Disk {
		return 2
	}
	return 1
}

// Sleep releases the GPU memory this instance holds. Measured: ~70 ms, dropping
// a 0.6B instance from 77,782 MiB to 1,386 MiB.
//
// The caller must already have taken this instance out of service. Sleeping
// while traffic can still arrive is the Ready-but-asleep window, which is a 503.
func (e *Engine) Sleep(ctx context.Context, tier Tier) error {
	path := fmt.Sprintf("/sleep?level=%d", SleepLevel(tier))
	_, err := e.do(ctx, http.MethodPost, path)
	return err
}

// Wake restores the instance and waits until it answers.
//
// Both halves matter. /wake_up returning is not the same as the engine serving --
// measured, the call takes ~355 ms and the first successful /health follows about
// 8 ms later -- and a caller that puts a Pod into service on the strength of the
// call alone has built the race it was trying to avoid.
func (e *Engine) Wake(ctx context.Context) error {
	if _, err := e.do(ctx, http.MethodPost, "/wake_up"); err != nil {
		return err
	}
	return e.WaitServing(ctx)
}

// WaitServing blocks until the engine answers /health or ctx ends.
//
// Polled with the same helper the rest of this codebase uses for "wait for a
// condition or give up with the context" (internal/prometheus, internal/engines/
// executor, internal/resources), rather than a hand-rolled ticker.
func (e *Engine) WaitServing(ctx context.Context) error {
	err := wait.PollUntilContextCancel(ctx, e.pollWait, true, func(ctx context.Context) (bool, error) {
		_, err := e.do(ctx, http.MethodGet, "/health")
		return err == nil, nil
	})
	if err != nil {
		return fmt.Errorf("engine at %s did not answer: %w", e.baseURL, err)
	}
	return nil
}

// IsSleeping asks the engine what it is, rather than inferring it.
//
// Worth preferring over any label or cached belief: a Pod label is per-Pod and
// flips for reasons unrelated to a given model, which cost real measurement time
// to learn.
func (e *Engine) IsSleeping(ctx context.Context) (bool, error) {
	body, err := e.do(ctx, http.MethodGet, "/is_sleeping")
	if err != nil {
		return false, err
	}
	var answer struct {
		IsSleeping bool `json:"is_sleeping"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return false, fmt.Errorf("decode is_sleeping: %w", err)
	}
	return answer.IsSleeping, nil
}

func (e *Engine) do(ctx context.Context, method, path string) ([]byte, error) {
	return doJSON(ctx, e.client, method, e.baseURL+path, nil)
}
