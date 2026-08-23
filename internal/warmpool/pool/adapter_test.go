package pool

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
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

// deadAddr is an address proven to have nothing behind it: bound and closed, so
// the kernel confirmed it was free rather than the test guessing a port.
var deadAddr = func() string {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := strings.TrimPrefix(server.URL, "http://")
	server.Close()
	return addr
}()

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

// has reports whether anything with this prefix has happened yet.
func (j *journal) has(prefix string) bool {
	for _, e := range j.list() {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
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
	// brokenPods answer as if their supervisor were unreachable. Per Pod
	// rather than per harness, so a test can put a healthy Pod and a broken one
	// behind the SAME ListWarm call -- which is the only way to exercise the
	// loop that must skip one and keep the other.
	brokenPods map[string]bool
	// serveSlowly delays /health so an admission can outlive its context.
	serveSlowly time.Duration

	// Failure knobs. Each names a step that can fail in production and whose
	// failure must stop the sequence rather than let it run on: the orderings
	// this adapter exists to enforce are only enforced if a failed step aborts.
	deleteFails  bool
	clearFails   bool
	sleepFails   bool
	garbageList  bool
	garbageState bool
}

func newHarness(t *testing.T, pods ...client.Object) *harness {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	h := &harness{
		journal:    &journal{},
		brokenPods: map[string]bool{},
		asleep:     true,
		// Exactly what the pool would have created: the variant's derived
		// options, --enable-sleep-mode (without which the copy cannot sleep at
		// all), and the port the pool assigned. A fixture missing any of those
		// is not a resident instance, and admission is right to refuse it.
		instances: []Instance{{
			ID:      "qwen",
			Status:  "running",
			Options: "--model Qwen/Qwen3-0.6B --enable-sleep-mode --port 9001",
		}},
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
	a.newSupervisor = func(podIP string) *Supervisor {
		if h.brokenPods[podIP] {
			// A supervisor that is not there at all: the address refuses.
			return &Supervisor{client: supervisor.Client(), baseURL: "http://" + deadAddr}
		}
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
		if h.garbageList {
			_, _ = w.Write([]byte(`{"qwen": "this is not an instance"}`))
			return
		}
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
		id := strings.TrimPrefix(r.URL.Path, instancesPath+"/")
		h.journal.add("delete " + id)
		if h.deleteFails {
			http.Error(w, "instance is busy", http.StatusConflict)
			return
		}
		// Actually remove it. A fake that journals the call but keeps the
		// instance lets a test assert the delete happened while the state it
		// was supposed to change did not.
		h.instances = slices.DeleteFunc(h.instances, func(inst Instance) bool { return inst.ID == id })
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
		if h.sleepFails {
			http.Error(w, "cumem: invalid argument", http.StatusInternalServerError)
			return
		}
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
	case r.URL.Path == sleepStatePath:
		if h.garbageState {
			_, _ = w.Write([]byte(`<html>502 Bad Gateway</html>`))
			return
		}
		_, _ = w.Write([]byte(`{"is_sleeping":` + strconv.FormatBool(h.asleep) + `}`))
	case r.URL.Path == "/health":
		// The delay is taken OUTSIDE the lock. Sleeping while holding it blocks
		// every other handler in this harness -- including the supervisor's
		// DELETE, which is exactly the call the abandonment path has to make,
		// so the test deadlocked against its own fake and waited out the whole
		// delay before failing.
		delay := h.serveSlowly
		if delay > 0 {
			// Journalled ONLY on the slow path, so no other test sees an extra
			// entry. It is the signal that Create has RETURNED to its caller:
			// the "create" entry is written by the supervisor handler, which
			// runs while the client is still reading the response, so
			// cancelling on that can abort Create itself -- and a Create that
			// fails leaves nothing to clean up, which is not what the test is
			// about.
			h.journal.add("serving-check")
			h.mu.Unlock()
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
			}
			h.mu.Lock()
		}
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
		if h.clearFails {
			http.Error(w, "proxy is not listening", http.StatusServiceUnavailable)
			return
		}
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

// admitted is the variant these tests admit, as opposed to the one already
// resident. Named because several tests now use it and the two must not be
// confused: admitting a model that is already there is a no-op.
const admitted = "llama"

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
	model.Variant = admitted
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

func TestAnInstanceThatWillNotAnswerReadsAsLoading(t *testing.T) {
	// An instance exists in the supervisor's list long before it can serve: a
	// load is ~33-37 s measured. What the caller needs to know is not "is it
	// asleep" but "can it serve a wake", and during a load the answer is no.
	// Reading an unanswerable engine as Asleep would offer it as reserve and
	// blow the borrow.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))
	h.garbageState = true

	got, err := h.adapter.ListWarm(context.Background())
	if err != nil {
		t.Fatalf("ListWarm: %v", err)
	}
	if len(got) != 1 || got[0].State != Loading {
		t.Fatalf("state = %+v, want loading", got)
	}
}

func TestAnUnreachablePodHidesOnlyItself(t *testing.T) {
	// One Pod that will not answer must not empty the warm set. Treating a
	// transient supervisor failure as "the pool is empty" re-admits models that
	// are already resident, at ~35 s and a reserve slot each.
	//
	// The two Pods must be in the SAME ListWarm call: testing them in separate
	// harnesses would show "one good Pod alone -> 1" and "one bad Pod alone ->
	// 0" without ever running the loop that has to skip one and keep the other.
	h := newHarness(t,
		poolPod("pod-a", "10.0.0.1", nil),
		poolPod("pod-b", "10.0.0.2", nil),
	)
	h.brokenPods["10.0.0.2"] = true

	got, err := h.adapter.ListWarm(context.Background())
	if err != nil {
		t.Fatalf("ListWarm must not fail for one unreachable Pod: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListWarm = %+v, want only the reachable Pod", got)
	}
	if got[0].Pod.Name != "pod-a" {
		t.Errorf("kept %s, want the Pod that answered", got[0].Pod.Name)
	}
}

func TestAFailedAdmissionDoesNotCostThePodItsPlaceInTheReserve(t *testing.T) {
	// An instance that was created but never served will not answer, so stateOf
	// reports it as Loading -- deliberately not part of the reserve, because a
	// Pod mid-load cannot serve a wake. Nothing revisits it: admission is
	// idempotent on the instance already existing. Left behind, one failed
	// admission costs the pool a Pod and its GPU, permanently.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))
	h.instances = nil
	h.sleepFails = true // created and serving, but it will not go to sleep

	model := qwen()
	model.Variant = admitted
	err := h.adapter.Warm(context.Background(), podA(), model, Ram)
	if err == nil {
		t.Fatal("want the admission failure reported")
	}
	h.journal.inOrder(t, "create", "sleep", "delete "+admitted)

	h.mu.Lock()
	remaining := len(h.instances)
	h.mu.Unlock()
	if remaining != 0 {
		t.Errorf("%d instances left behind; the Pod is out of the reserve forever", remaining)
	}
}

func TestAnAbandonedInstanceThatCannotBeRemovedSaysSo(t *testing.T) {
	// The Pod really is out of the pool in this case, and that is worth more to
	// whoever reads the log than the original failure on its own.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))
	h.instances = nil
	h.sleepFails = true
	h.deleteFails = true

	model := qwen()
	model.Variant = admitted
	err := h.adapter.Warm(context.Background(), podA(), model, Ram)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "out of the reserve") {
		t.Errorf("the error must say the Pod was lost: %v", err)
	}
	if !strings.Contains(err.Error(), admitted) {
		t.Errorf("and name the model: %v", err)
	}
}

func TestCleanupSurvivesTheContextThatFailed(t *testing.T) {
	// The context that failed is very often the reason it failed, so a cleanup
	// inheriting its deadline cleans nothing up -- which is exactly the case
	// where the Pod would be stranded.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))
	h.instances = nil
	// Long enough that WaitServing cannot finish on its own; the test decides
	// when the context ends rather than racing it. A deadline short enough to
	// beat a fast call is a deadline a loaded machine will trip in the wrong
	// place -- this test did exactly that before it was written this way.
	h.serveSlowly = time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Cancel once the instance exists, which is the only state from which
		// cleanup has anything to do.
		for !h.journal.has("serving-check") {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	model := qwen()
	model.Variant = admitted
	if err := h.adapter.Warm(ctx, podA(), model, Ram); err == nil {
		t.Fatal("want the admission to fail once its context ends")
	}
	h.journal.at(t, "delete "+admitted)
}

func TestEvictRemovesTheInstanceItWasAskedTo(t *testing.T) {
	// Eviction is what frees host memory. It is keyed on the variant, not on a
	// hash over GPU UUIDs, because a pool Pod owns its GPUs for as long as it
	// owns the model.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))

	if err := h.adapter.Evict(context.Background(), podA(), qwen()); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	deleted := h.journal.list()[h.journal.at(t, "delete")]
	if deleted != "delete qwen" {
		t.Errorf("evicted %q, want the variant's own instance", deleted)
	}
}

func TestAFailedEvictionIsReportedRatherThanAssumed(t *testing.T) {
	// A caller that believes it freed memory it did not free will admit the
	// next model into a Pod with no room for it.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))
	h.deleteFails = true

	err := h.adapter.Evict(context.Background(), podA(), qwen())
	if err == nil {
		t.Fatal("want an error when the supervisor refuses")
	}
	if !strings.Contains(err.Error(), "qwen") || !strings.Contains(err.Error(), "pod-a") {
		t.Errorf("the error must name what could not be evicted from where: %v", err)
	}
}

func TestDeactivateDoesNotSleepAModelStillTakingTraffic(t *testing.T) {
	// The whole reason the proxy is cleared first. If clearing fails the Pod is
	// still Ready and still in its InferencePool, so sleeping anyway is the
	// Ready-but-asleep window -- every request routed there 503s.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", map[string]string{"llm-d.ai/model": "qwen"}))
	h.clearFails = true

	if err := h.adapter.Deactivate(context.Background(), podA(), qwen()); err == nil {
		t.Fatal("a proxy that will not clear must be reported")
	}
	h.journal.never(t, "sleep")
	h.journal.never(t, "unlabel")

	var got corev1.Pod
	if err := h.k8s.Get(context.Background(), podA(), &got); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if _, member := got.Labels["llm-d.ai/model"]; !member {
		t.Error("membership must survive a failed deactivation, so a retry has something to undo")
	}
}

func TestDeactivateDoesNotSleepAModelStillInItsInferencePool(t *testing.T) {
	// Same window, one step later: unlabelling is what removes the Pod from the
	// EPP's endpoint set. Sleeping while it is still listed is a 503 for as long
	// as the EPP takes to notice.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", map[string]string{"llm-d.ai/model": "qwen"}))
	h.k8s = refusePatches(t, h.k8s)
	h.adapter.client = h.k8s

	if err := h.adapter.Deactivate(context.Background(), podA(), qwen()); err == nil {
		t.Fatal("a Pod that cannot leave its pool must be reported")
	}
	h.journal.inOrder(t, "clear")
	h.journal.never(t, "sleep")
}

func TestDeactivateReportsAnEngineThatWillNotSleep(t *testing.T) {
	// Measured on the cluster: sleep and wake both fail on real engines, with
	// cumem errors. A Pod whose engine refused to sleep is still holding its
	// GPU, and must not silently be counted back into the reserve.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", map[string]string{"llm-d.ai/model": "qwen"}))
	h.sleepFails = true

	err := h.adapter.Deactivate(context.Background(), podA(), qwen())
	if err == nil {
		t.Fatal("want an error when the engine will not sleep")
	}
	if !strings.Contains(err.Error(), "qwen") {
		t.Errorf("the error must name the model still holding the GPU: %v", err)
	}
}

func TestDeactivateGivesUpTheDrainWithTheContext(t *testing.T) {
	// The drain is a sleep in the middle of a sequence. On shutdown it must end
	// with the context rather than hold the loop open, and it must not go on to
	// sleep an engine whose Pod it can no longer observe.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", map[string]string{"llm-d.ai/model": "qwen"}))
	h.adapter.DrainWait = time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Let the clear land first, so the test covers the drain and not the
		// call before it.
		for len(h.journal.list()) == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	start := time.Now()
	if err := h.adapter.Deactivate(ctx, podA(), qwen()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Deactivate = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("waited %s: the drain must end with the context", elapsed)
	}
	h.journal.never(t, "sleep")
}

// refusePatches wraps a client so every Patch fails, which is how a Pod that
// cannot leave its InferencePool behaves.
func refusePatches(t *testing.T, inner client.Client) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(mustList(t, inner)...).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return errors.New("apiserver said no")
			},
		}).Build()
}

func mustList(t *testing.T, c client.Client) []runtime.Object {
	t.Helper()
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods); err != nil {
		t.Fatalf("list: %v", err)
	}
	out := make([]runtime.Object, 0, len(pods.Items))
	for i := range pods.Items {
		out = append(out, &pods.Items[i])
	}
	return out
}

func TestTheDrainYieldsToTheDeadlineRatherThanStrandingThePod(t *testing.T) {
	// Deactivate clears the proxy FIRST, so a timeout inside the drain leaves
	// the Pod NotReady, still carrying its InferencePool labels, and with its
	// engine awake holding the GPU. The next pass reads that as Waking, which
	// still counts as lent, and schedules the same Deactivate -- which fails at
	// the same point again. The Pod never returns to the reserve.
	//
	// That livelock is reachable purely by configuration, because DrainWait
	// lives on the Adapter and the deadline comes from the reconciler's
	// ActTimeout: two knobs in two packages with nothing tying them together.
	// The drain yields instead.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", map[string]string{"llm-d.ai/model": "qwen"}))
	h.adapter.DrainWait = time.Hour

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	if err := h.adapter.Deactivate(ctx, podA(), qwen()); err != nil {
		t.Fatalf("Deactivate must complete under a deadline shorter than the drain: %v", err)
	}
	// The whole sequence, in order -- the point is that it REACHES the sleep.
	h.journal.inOrder(t, "clear", "unlabel", "sleep")

	var got corev1.Pod
	if err := h.k8s.Get(context.Background(), podA(), &got); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if _, still := got.Labels["llm-d.ai/model"]; still {
		t.Error("the Pod must leave its InferencePool, or it is lent forever")
	}
	if !h.asleep {
		t.Error("the engine must sleep, or the Pod holds its GPU forever")
	}
}

func TestTheDrainIsUnshortenedWhenThereIsTimeForIt(t *testing.T) {
	// Yielding is for the cliff, not the common case: cutting the drain short
	// kills the requests still in flight, so a deadline with room must leave the
	// full DrainWait alone.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))
	h.adapter.DrainWait = 40 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	start := time.Now()
	if err := h.adapter.Deactivate(ctx, podA(), qwen()); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("drained for %s, want the full DrainWait when the budget allows it", elapsed)
	}
}

func TestTheDrainIsSkippedWhenTheDeadlineHasAllButPassed(t *testing.T) {
	// With nothing left, waiting at all guarantees the stranded Pod. Skipping
	// the drain costs in-flight requests, which is strictly the smaller loss.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))
	h.adapter.DrainWait = time.Hour

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	if wait := h.adapter.drainFor(ctx); wait != 0 {
		t.Fatalf("drainFor on an expired deadline = %s, want 0", wait)
	}
}

func TestASelectorCannotLabelAPoolPodIntoTheControlPlane(t *testing.T) {
	// A pool Pod already carries app.kubernetes.io/name:
	// workload-variant-autoscaler, which is half of what the NetworkPolicy
	// matches to recognise the controller. A tenant whose InferencePool
	// selector supplied the other half would have WVA label the Pod into the
	// trusted set for them -- and the tenant's own model code runs inside that
	// Pod, so it could then reach the supervisor of every other pool Pod in the
	// namespace and spawn processes there with argv of its choosing.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))

	model := qwen()
	model.PoolLabels["control-plane"] = "controller-manager"

	_, err := h.adapter.Activate(context.Background(), podA(), model)
	if err == nil {
		t.Fatal("want the join refused")
	}
	if !strings.Contains(err.Error(), "control-plane") {
		t.Errorf("the error must name the label it refused: %v", err)
	}
	h.journal.never(t, "wake")

	var got corev1.Pod
	if err := h.k8s.Get(context.Background(), podA(), &got); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if _, taken := got.Labels["control-plane"]; taken {
		t.Error("the label must never have been applied")
	}
}

func TestThePortIsReadAsATokenNotASubstring(t *testing.T) {
	// A regular expression over the whole options string matched the FIRST
	// --port=NNNN anywhere, including inside another flag's value -- while the
	// pool appends the real one last. Believing the wrong port makes freePort
	// reserve one already bound, and makes endpointOf resolve to a DIFFERENT
	// resident instance, which Activate would then wake and serve under the
	// requesting variant's labels.
	for _, tc := range []struct {
		options string
		want    int
	}{
		{options: "--model m --port 9001", want: 9001},
		{options: "--model m --port=9001", want: 9001},
		{options: "--model /model-cache/w--port=9002/ --port 9001", want: 9001},
		{options: "--served-model-name --port=9002 --port 9001", want: 9001},
		{options: "--model m", want: 0},
	} {
		if got := portOf(tc.options); got != tc.want {
			t.Errorf("portOf(%q) = %d, want %d", tc.options, got, tc.want)
		}
	}
}

func TestAResidentInstanceWithDifferentOptionsIsNotReused(t *testing.T) {
	// Admission is idempotent on the instance already being there -- but only
	// if it is the SAME engine. A different model, shape or compile cache key
	// under the same name would answer one variant's requests with another
	// variant's engine.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))

	model := qwen()
	model.EngineOptions = "--model SomeoneElse/Model-7B --enable-sleep-mode"

	err := h.adapter.Warm(context.Background(), podA(), model, Ram)
	if err == nil {
		t.Fatal("want the reuse refused")
	}
	if !strings.Contains(err.Error(), "different options") {
		t.Errorf("the error must say why: %v", err)
	}
	h.journal.never(t, "create")
}

func TestThePortIsNotPartOfWhatMakesTwoInstancesTheSame(t *testing.T) {
	// The port is the pool's own choice and differs between Pods; everything
	// else is the engine's identity. Comparing it would refuse every legitimate
	// reuse.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))
	if err := h.adapter.Warm(context.Background(), podA(), qwen(), Ram); err != nil {
		t.Fatalf("a resident instance differing only by port must be reused: %v", err)
	}
	h.journal.never(t, "create")
}
