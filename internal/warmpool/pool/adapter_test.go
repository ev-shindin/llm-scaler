package pool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const testNamespace = "pool-ns"

// journal records what happened, in order, across all three protocols and the
// Kubernetes client. The orderings are the thing worth asserting: sleeping
// before the traffic is gone is measurably a 503, and labelling after the wake
// would put the EPP's admit latency on the critical path.
type journal struct {
	mu     sync.Mutex
	events []string
}

func (j *journal) add(event string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.events = append(j.events, event)
}

func (j *journal) list() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.events...)
}

func (j *journal) at(t *testing.T, prefix string) int {
	t.Helper()
	for i, e := range j.list() {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	t.Fatalf("%q never happened: %v", prefix, j.list())
	return -1
}

// inOrder asserts that each step happened, and in this sequence. The orderings
// are the point of the adapter: reversing one is a 503 or a lost admit window.
func (j *journal) inOrder(t *testing.T, steps ...string) {
	t.Helper()
	previous := -1
	for _, step := range steps {
		at := j.at(t, step)
		if at <= previous {
			t.Fatalf("%v out of order, wanted %v", j.list(), steps)
		}
		previous = at
	}
}

func (j *journal) never(t *testing.T, prefix string) {
	t.Helper()
	for _, e := range j.list() {
		if strings.HasPrefix(e, prefix) {
			t.Fatalf("%q happened and must not have: %v", prefix, j.list())
		}
	}
}

// harness stands up an Adapter whose three protocol clients point at httptest
// servers, so the production request code is exercised rather than restated.
type harness struct {
	adapter *Adapter
	journal *journal
	k8s     client.Client

	mu        sync.Mutex
	instances []Instance
	upstream  string
	asleep    bool
	wakeFails bool
}

func newHarness(t *testing.T, pods ...client.Object) *harness {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	h := &harness{
		journal:   &journal{},
		asleep:    true,
		instances: []Instance{{ID: "qwen", Status: "running", Options: "--model Qwen/Qwen3-0.6B --port 9001"}},
	}
	h.k8s = fake.NewClientBuilder().WithScheme(scheme).WithObjects(pods...).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
				// One label patch is either joining or leaving a pool; which it
				// is shows in the resulting labels.
				if pod, ok := obj.(*corev1.Pod); ok {
					if _, member := pod.Labels["llm-d.ai/model"]; member {
						h.journal.add("label")
					} else {
						h.journal.add("unlabel")
					}
				}
				return c.Patch(ctx, obj, p, opts...)
			},
		}).Build()

	supervisor := httptest.NewServer(http.HandlerFunc(h.serveSupervisor))
	engine := httptest.NewServer(http.HandlerFunc(h.serveEngine))
	proxy := httptest.NewServer(http.HandlerFunc(h.serveProxy))
	t.Cleanup(supervisor.Close)
	t.Cleanup(engine.Close)
	t.Cleanup(proxy.Close)

	a := NewAdapter(h.k8s, testNamespace, Ram)
	a.DrainWait = 5 * time.Millisecond
	a.newSupervisor = func(string) *Supervisor {
		return &Supervisor{client: supervisor.Client(), baseURL: supervisor.URL}
	}
	a.newEngine = func(Endpoint) *Engine {
		return &Engine{client: engine.Client(), baseURL: engine.URL, pollWait: time.Millisecond}
	}
	a.newProxy = func(string) *Proxy {
		return &Proxy{client: proxy.Client(), baseURL: proxy.URL}
	}
	h.adapter = a
	return h
}

func (h *harness) serveSupervisor(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case r.URL.Path == "/health":
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && r.URL.Path == instancesPath:
		byID := map[string]Instance{}
		for _, inst := range h.instances {
			byID[inst.ID] = inst
		}
		_ = json.NewEncoder(w).Encode(byID)
	case r.Method == http.MethodPut:
		id := strings.TrimPrefix(r.URL.Path, instancesPath+"/")
		var spec InstanceSpec
		_ = json.NewDecoder(r.Body).Decode(&spec)
		h.journal.add("create " + id + " " + spec.Options)
		inst := Instance{ID: id, Status: "running", Options: spec.Options}
		h.instances = append(h.instances, inst)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(inst)
	case r.Method == http.MethodDelete:
		h.journal.add("delete " + strings.TrimPrefix(r.URL.Path, instancesPath+"/"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	default:
		http.NotFound(w, r)
	}
}

func (h *harness) serveEngine(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case strings.HasPrefix(r.URL.Path, "/sleep"):
		h.journal.add("sleep")
		h.asleep = true
		w.WriteHeader(http.StatusOK)
	case r.URL.Path == "/wake_up":
		h.journal.add("wake")
		if h.wakeFails {
			http.Error(w, "CUDA error: out of memory", http.StatusInternalServerError)
			return
		}
		h.asleep = false
		w.WriteHeader(http.StatusOK)
	case r.URL.Path == "/is_sleeping":
		_, _ = w.Write([]byte(`{"is_sleeping":` + strconv.FormatBool(h.asleep) + `}`))
	case r.URL.Path == "/health":
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

func (h *harness) serveProxy(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Address string `json:"address"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.journal.add("point " + body.Address)
		h.upstream = body.Address
		_, _ = w.Write([]byte(`{"address":"` + body.Address + `"}`))
	case http.MethodDelete:
		h.journal.add("clear")
		h.upstream = ""
		w.WriteHeader(http.StatusNoContent)
	default:
		_, _ = w.Write([]byte(`{"address":"` + h.upstream + `"}`))
	}
}

func poolPod(name, ip string, extra map[string]string) *corev1.Pod {
	labels := map[string]string{ComponentLabel: ComponentValue}
	for k, v := range extra {
		labels[k] = v
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: labels},
		Status:     corev1.PodStatus{PodIP: ip},
	}
}

// podA names the Pod every test acts on; the fakes answer for whichever Pod is
// addressed, so a second name would add nothing.
func podA() types.NamespacedName {
	return types.NamespacedName{Namespace: testNamespace, Name: "pod-a"}
}

func qwen() ModelRef {
	return ModelRef{
		Namespace:     testNamespace,
		Variant:       "qwen",
		EngineOptions: "--model Qwen/Qwen3-0.6B --enable-sleep-mode",
		PoolLabels: map[string]string{
			"llm-d.ai/model":            "qwen",
			"llm-d.ai/inferenceServing": "true",
		},
	}
}

func TestActivateLabelsBeforeWaking(t *testing.T) {
	// Deliberate: readiness is what admits traffic, so labelling early costs
	// nothing and takes the EPP's ~460 ms admit off the critical path.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))

	ep, err := h.adapter.Activate(context.Background(), podA(), qwen())
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if ep.Port != 9001 {
		t.Fatalf("endpoint = %+v, want the instance's own port", ep)
	}
	h.journal.inOrder(t, "label", "wake", "point")
	if got := h.upstream; got != "127.0.0.1:9001" {
		t.Errorf("proxy must be pointed at the Pod-local address, got %q", got)
	}
}

func TestActivateAppliesThePoolsOwnSelectorLabels(t *testing.T) {
	// Selectors belong to the tenant; assuming llm-d.ai/model breaks on the
	// first InferencePool that selects on something else.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))
	model := qwen()
	model.PoolLabels = map[string]string{"custom.example/pool": "team-a", "llm-d.ai/role": "decode"}

	if _, err := h.adapter.Activate(context.Background(), podA(), model); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	var got corev1.Pod
	if err := h.k8s.Get(context.Background(), podA(), &got); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	for k, v := range model.PoolLabels {
		if got.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, got.Labels[k], v)
		}
	}
	if got.Labels[ComponentLabel] != ComponentValue {
		t.Error("the pool-capacity label must survive: it is what excludes a lent Pod from replica accounting")
	}
}

func TestActivateRefusesWithoutPoolLabels(t *testing.T) {
	// A wake with no membership leaves an engine awake, holding its GPU, in no
	// InferencePool: cost without benefit.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))
	model := qwen()
	model.PoolLabels = nil

	if _, err := h.adapter.Activate(context.Background(), podA(), model); err == nil {
		t.Fatal("want an error when no selector labels are supplied")
	}
	h.journal.never(t, "wake")
}

func TestActivateNeverPointsAtAnEngineThatDidNotWake(t *testing.T) {
	// Two of three unbound sleepers could not wake on the cluster; the caller
	// must be able to fall through to the cold path.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))
	h.wakeFails = true

	if _, err := h.adapter.Activate(context.Background(), podA(), qwen()); err == nil {
		t.Fatal("a failed wake must be reported")
	}
	h.journal.never(t, "point")
}

func TestDeactivateClearsTheProxyBeforeSleeping(t *testing.T) {
	// The ordering that is a 503 if reversed: clearing the proxy is what makes
	// the Pod NotReady and drains the EPP (~630 ms measured).
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", map[string]string{"llm-d.ai/model": "qwen"}))

	if err := h.adapter.Deactivate(context.Background(), podA(), qwen()); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	h.journal.inOrder(t, "clear", "unlabel", "sleep")

	var got corev1.Pod
	if err := h.k8s.Get(context.Background(), podA(), &got); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if _, still := got.Labels["llm-d.ai/model"]; still {
		t.Error("membership must be given up when the model sleeps")
	}
	if got.Labels[ComponentLabel] != ComponentValue {
		t.Error("the pool-capacity label must never be removed")
	}
}

func TestWarmAdmitsAndLeavesTheModelAsleep(t *testing.T) {
	// A model admitted and left awake would hold the GPU it was meant to share.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))
	h.instances = nil
	h.asleep = false

	model := qwen()
	model.Variant = "llama"
	if err := h.adapter.Warm(context.Background(), podA(), model, Ram); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	h.journal.inOrder(t, "create", "sleep")
	if !h.asleep {
		t.Error("admission must end with the model asleep")
	}
	created := h.journal.list()[h.journal.at(t, "create")]
	if !strings.Contains(created, "--port 9001") {
		t.Errorf("the caller assigns the port: %q", created)
	}
}

func TestWarmIsIdempotent(t *testing.T) {
	// A controller that must remember whether it already admitted something
	// will be wrong after a restart.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))
	if err := h.adapter.Warm(context.Background(), podA(), qwen(), Ram); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	h.journal.never(t, "create")
}

func TestListWarmDiscoversStateRatherThanRemembering(t *testing.T) {
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))

	got, err := h.adapter.ListWarm(context.Background())
	if err != nil {
		t.Fatalf("ListWarm: %v", err)
	}
	if len(got) != 1 || got[0].Model.Variant != "qwen" {
		t.Fatalf("ListWarm = %+v", got)
	}
	if got[0].State != Asleep {
		t.Errorf("a sleeping instance reads as asleep, got %q", got[0].State)
	}
	if got[0].Endpoint.Port != 9001 || got[0].Endpoint.PodIP != "10.0.0.1" {
		t.Errorf("endpoint = %+v", got[0].Endpoint)
	}

	// Awake and pointed at means serving; awake and not pointed at means the
	// wake is still in flight.
	h.asleep = false
	h.upstream = "127.0.0.1:9001"
	if got, _ = h.adapter.ListWarm(context.Background()); got[0].State != Serving {
		t.Errorf("state = %q, want serving", got[0].State)
	}
	h.upstream = ""
	if got, _ = h.adapter.ListWarm(context.Background()); got[0].State != Waking {
		t.Errorf("state = %q, want waking", got[0].State)
	}
}

func TestListWarmIgnoresPodsWithoutAnAddress(t *testing.T) {
	// A Pod still being scheduled is not reserve, and must not be counted as
	// one -- nor may it make the whole listing fail.
	pending := poolPod("pod-b", "", nil)
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil), pending)

	got, err := h.adapter.ListWarm(context.Background())
	if err != nil {
		t.Fatalf("ListWarm: %v", err)
	}
	for _, m := range got {
		if m.Pod.Name == "pod-b" {
			t.Fatal("an unaddressable Pod must not appear in the warm set")
		}
	}
}

func TestFreePortAvoidsCollisionsWithinAPod(t *testing.T) {
	// FMA derives the port from the InferenceServerConfig, so two instances of
	// one model collide. Ports are ours to choose, so they do not.
	if got, err := freePort(nil); err != nil || got != BasePort {
		t.Fatalf("first instance takes the base port: %d, %v", got, err)
	}
	if got, err := freePort([]Instance{{Options: "--model m --port 9001"}}); err != nil || got != 9002 {
		t.Fatalf("second instance must not reuse 9001: %d, %v", got, err)
	}
	full := make([]Instance, 0, MaxInstancesPerPod)
	for i := 0; i < MaxInstancesPerPod; i++ {
		full = append(full, Instance{Options: "--port " + strconv.Itoa(BasePort+i)})
	}
	if _, err := freePort(full); err == nil {
		t.Fatal("a full Pod must refuse rather than collide")
	}
}

func TestPortOfReadsWhatTheProcessWasStartedWith(t *testing.T) {
	for _, tc := range []struct {
		options string
		want    int
	}{
		{"--model m --port 9003", 9003},
		{"--model m --port=9004", 9004},
		{"--model m", 0},
	} {
		if got := portOf(tc.options); got != tc.want {
			t.Errorf("portOf(%q) = %d, want %d", tc.options, got, tc.want)
		}
	}
}
