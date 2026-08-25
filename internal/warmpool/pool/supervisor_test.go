package pool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeSupervisor stands in for the launcher, recording what it was asked.
type fakeSupervisor struct {
	server    *httptest.Server
	lastPath  string
	lastBody  string
	respond   func(w http.ResponseWriter, r *http.Request)
	callCount int
}

func newFakeSupervisor(t *testing.T) *fakeSupervisor {
	t.Helper()
	f := &fakeSupervisor{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.callCount++
		f.lastPath = r.Method + " " + r.URL.Path
		if r.Body != nil {
			buf := make([]byte, 4096)
			n, _ := r.Body.Read(buf)
			f.lastBody = string(buf[:n])
		}
		f.respond(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

// client points a Supervisor at the fake, bypassing the podIP:port construction.
func (f *fakeSupervisor) client() *Supervisor {
	return &Supervisor{client: f.server.Client(), baseURL: f.server.URL}
}

func TestListAcceptsTheLauncherEnvelopeShape(t *testing.T) {
	// The shape the pinned launcher actually returns, copied from a live Pod.
	// Without this case the parser passed its tests and could not read a single
	// real Pod: the pool reported itself empty on every reconcile.
	f := newFakeSupervisor(t)
	f.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"revision":1,"total_instances":1,"running_instances":1,` +
			`"instances":[{"instance_id":"qwen-a","status":"running",` +
			`"options":"--model /model-cache/models/Qwen/Qwen3-0.6B --port 9001"}]}`))
	}
	got, err := f.client().List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != "qwen-a" {
		t.Fatalf("envelope not unwrapped: %+v", got)
	}
	if got[0].Status != "running" || !strings.Contains(got[0].Options, "Qwen3-0.6B") {
		t.Errorf("fields lost: %+v", got[0])
	}
}

func TestListReportsAnEmptyPoolRatherThanFailing(t *testing.T) {
	// An empty pool is the ordinary state of a Pod that holds nothing yet, and
	// it must not read as an error -- "could not be read" makes the reconciler
	// drop the Pod from the observation entirely.
	f := newFakeSupervisor(t)
	f.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"revision":0,"total_instances":0,"running_instances":0,"instances":[]}`))
	}
	got, err := f.client().List(context.Background())
	if err != nil {
		t.Fatalf("an empty pool must not be an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want no instances, got %+v", got)
	}
}

func TestListAcceptsTheLauncherMapShape(t *testing.T) {
	f := newFakeSupervisor(t)
	f.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"qwen":{"status":"running","options":"--model Qwen/Qwen3-0.6B --port 9001"}}`))
	}
	got, err := f.client().List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != "qwen" {
		t.Fatalf("id must be filled in from the map key: %+v", got)
	}
	if !strings.Contains(got[0].Options, "Qwen3-0.6B") {
		t.Errorf("options lost: %+v", got[0])
	}
}

func TestListAlsoAcceptsAList(t *testing.T) {
	// Tolerated on purpose: the launcher is copied code that may change shape,
	// and a read that breaks on it would take the whole pool down.
	f := newFakeSupervisor(t)
	f.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"instance_id":"a","status":"running"}]`))
	}
	got, err := f.client().List(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("List = %+v, err %v", got, err)
	}
}

func TestCreateSendsTheCallersIdAndOptions(t *testing.T) {
	// Identity keyed on the model, and the port inside the options, are the two
	// decisions that let the copied launcher serve a pool unmodified.
	f := newFakeSupervisor(t)
	f.respond = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"instance_id":"my-variant","status":"running"}`))
	}
	spec := InstanceSpec{
		Options: "--model Qwen/Qwen3-0.6B --port 9001",
		EnvVars: map[string]string{"VLLM_SERVER_DEV_MODE": "1"},
	}
	inst, err := f.client().Create(context.Background(), "my-variant", spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if inst.ID != "my-variant" {
		t.Errorf("instance id = %q", inst.ID)
	}
	if f.lastPath != "PUT /v2/vllm/instances/my-variant" {
		t.Errorf("wrong request: %s", f.lastPath)
	}
	var sent InstanceSpec
	if err := json.Unmarshal([]byte(f.lastBody), &sent); err != nil {
		t.Fatalf("body was not the spec: %q", f.lastBody)
	}
	if sent.EnvVars["VLLM_SERVER_DEV_MODE"] != "1" {
		t.Error("dev mode must be passed: /sleep and /wake_up do not exist without it")
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	// A controller that must remember whether it already evicted something will
	// be wrong about it after a restart.
	f := newFakeSupervisor(t)
	f.respond = func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such instance", http.StatusNotFound)
	}
	if err := f.client().Delete(context.Background(), "gone"); err != nil {
		t.Fatalf("deleting an absent instance must succeed: %v", err)
	}
}

func TestErrorsCarryTheSupervisorsOwnWords(t *testing.T) {
	f := newFakeSupervisor(t)
	f.respond = func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "GPU 0 already has 2 sleepers", http.StatusConflict)
	}
	_, err := f.client().Create(context.Background(), "x", InstanceSpec{Options: "--model m"})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "already has 2 sleepers") {
		t.Errorf("the reason must survive: %v", err)
	}
	if !strings.Contains(err.Error(), "409") {
		t.Errorf("the status must survive: %v", err)
	}
}

func TestRequestsHonourContextCancellation(t *testing.T) {
	f := newFakeSupervisor(t)
	f.respond = func(w http.ResponseWriter, _ *http.Request) { time.Sleep(2 * time.Second) }
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := f.client().List(ctx); err == nil {
		t.Fatal("a hung supervisor must not hang the loop")
	}
}
