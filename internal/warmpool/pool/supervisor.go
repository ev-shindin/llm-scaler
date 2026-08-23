package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// SupervisorPort is where the in-Pod supervisor listens. It is the launcher's
// own default and is set in the pool Deployment.
const SupervisorPort = 8001

// instancesPath is the supervisor's collection. The v2 prefix is the launcher's,
// kept as it ships so that the copied code and this client cannot drift.
const instancesPath = "/v2/vllm/instances"

// Supervisor talks to one pool Pod's supervisor: the process manager that spawns
// and removes vLLM instances inside the container holding the GPUs.
//
// It covers instance LIFECYCLE only. Sleeping and waking are not here, because
// they are not the supervisor's: vLLM exposes them itself and the control plane
// calls the engine directly, which is also how Fast Model Actuation does it.
type Supervisor struct {
	client  *http.Client
	baseURL string
}

// NewSupervisor addresses the supervisor in the Pod at podIP.
func NewSupervisor(podIP string, timeout time.Duration) *Supervisor {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Supervisor{
		client:  &http.Client{Timeout: timeout},
		baseURL: fmt.Sprintf("http://%s:%d", podIP, SupervisorPort),
	}
}

// Instance is one vLLM process as the supervisor reports it.
type Instance struct {
	ID      string            `json:"instance_id"`
	Status  string            `json:"status"`
	Options string            `json:"options"`
	EnvVars map[string]string `json:"env_vars,omitempty"`
}

// InstanceSpec is what creating an instance requires.
//
// The caller supplies the ID, the port (inside Options) and the GPUs. That is
// why the launcher needed no modification to serve a pool: identity keyed on the
// model, and ports from a range we choose, are decisions made HERE. FMA's
// controller made different ones -- an ID hashed over GPU UUIDs and a port from
// the InferenceServerConfig -- and the second is what made two instances of one
// model collide.
type InstanceSpec struct {
	Options string            `json:"options"`
	EnvVars map[string]string `json:"env_vars,omitempty"`
	// GPUUUIDs places an instance on particular devices. The pool NEVER sets
	// it, so every instance in a Pod sees every GPU the Pod holds -- which is
	// what lets a tensor-parallel engine run there with no placement logic at
	// all. The cost is that sleepers settle on the same devices rather than
	// spreading, so their residue accumulates; see config/warmpool-multi-gpu.
	GPUUUIDs []string `json:"gpu_uuids,omitempty"`
}

// List reports the instances in this Pod.
func (s *Supervisor) List(ctx context.Context) ([]Instance, error) {
	body, err := s.do(ctx, http.MethodGet, instancesPath, nil)
	if err != nil {
		return nil, err
	}
	// The launcher answers with a map keyed by instance id at the collection
	// root; tolerate a bare list too, so a future shape does not break reads.
	var asMap map[string]Instance
	if err := json.Unmarshal(body, &asMap); err == nil {
		out := make([]Instance, 0, len(asMap))
		for id, inst := range asMap {
			if inst.ID == "" {
				inst.ID = id
			}
			out = append(out, inst)
		}
		return out, nil
	}
	var asList []Instance
	if err := json.Unmarshal(body, &asList); err != nil {
		return nil, fmt.Errorf("supervisor returned neither an object nor a list: %w", err)
	}
	return asList, nil
}

// Create spawns an instance and returns once the supervisor has accepted it.
//
// Acceptance is NOT readiness. The launcher reports "running" when the PROCESS
// started, which on this cluster precedes the engine serving by more than thirty
// seconds. Callers wanting a usable engine must poll the engine itself.
func (s *Supervisor) Create(ctx context.Context, id string, spec InstanceSpec) (Instance, error) {
	payload, err := json.Marshal(spec)
	if err != nil {
		return Instance{}, fmt.Errorf("encode instance spec: %w", err)
	}
	body, err := s.do(ctx, http.MethodPut, instancesPath+"/"+url.PathEscape(id), payload)
	if err != nil {
		return Instance{}, err
	}
	var inst Instance
	if err := json.Unmarshal(body, &inst); err != nil {
		return Instance{}, fmt.Errorf("decode created instance: %w", err)
	}
	if inst.ID == "" {
		inst.ID = id
	}
	return inst, nil
}

// Delete removes an instance, freeing its host memory and GPU residue. A missing
// instance is not an error: eviction is idempotent by design, because a
// controller that must know whether it already evicted something is a controller
// that will get it wrong after a restart.
func (s *Supervisor) Delete(ctx context.Context, id string) error {
	_, err := s.do(ctx, http.MethodDelete, instancesPath+"/"+url.PathEscape(id), nil)
	var status statusError
	if errors.As(err, &status) && status.code == http.StatusNotFound {
		return nil
	}
	return err
}

func (s *Supervisor) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	return doJSON(ctx, s.client, method, s.baseURL+path, body)
}

// statusError carries the supervisor's own words. A controller that reports
// "failed to create instance" without them sends whoever reads the log to the
// wrong place.
type statusError struct {
	code   int
	method string
	path   string
	body   string
}

func (e statusError) Error() string {
	return fmt.Sprintf("%s %s: http %d: %s", e.method, e.path, e.code, truncate(e.body, 300))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
