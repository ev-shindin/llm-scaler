package pool

import (
	"context"
	"encoding/json"
	"errors"
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
		h.journal.add("delete " + strings.TrimPrefix(r.URL.Path, instancesPath+"/"))
		if h.deleteFails {
			http.Error(w, "instance is busy", http.StatusConflict)
			return
		}
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
	case r.URL.Path == "/is_sleeping":
		if h.garbageState {
			_, _ = w.Write([]byte(`<html>502 Bad Gateway</html>`))
			return
		}
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

func TestASupervisorTalkingNonsenseHidesOnlyItsOwnPod(t *testing.T) {
	// One Pod answering with a body that does not parse must not empty the warm
	// set: treating a transient failure as "the pool is empty" admits models
	// that are already resident, at ~35 s and a reserve slot each.
	good := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))
	if got, err := good.adapter.ListWarm(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("baseline: %+v %v", got, err)
	}

	bad := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))
	bad.garbageList = true
	got, err := bad.adapter.ListWarm(context.Background())
	if err != nil {
		t.Fatalf("ListWarm must not fail for one bad Pod: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("an unreadable Pod contributes nothing, got %+v", got)
	}
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
