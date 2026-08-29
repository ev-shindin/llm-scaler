package pool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// rankRecorder is one supervisor per Pod, remembering what each was asked to do.
//
// One shared server cannot answer this test's question. What matters is WHICH
// ranks were cleaned up, and a single handler sees only paths -- every rank uses
// the same instance id, so the calls would be indistinguishable.
type rankRecorder struct {
	mu      sync.Mutex
	calls   map[string][]string // pod IP -> methods, in order
	failPUT map[string]bool     // pod IP -> its supervisor refuses to create
	servers map[string]*httptest.Server
}

func newRankRecorder(t *testing.T, ips []string, failPUT map[string]bool) *rankRecorder {
	t.Helper()
	r := &rankRecorder{
		calls:   map[string][]string{},
		failPUT: failPUT,
		servers: map[string]*httptest.Server{},
	}
	for _, ip := range ips {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			r.mu.Lock()
			r.calls[ip] = append(r.calls[ip], req.Method)
			fail := r.failPUT[ip]
			r.mu.Unlock()

			switch {
			case req.Method == http.MethodGet:
				// Nothing resident anywhere, so this is a real admission.
				_, _ = w.Write([]byte(`{"instances": []}`))
			case req.Method == http.MethodPut && fail:
				w.WriteHeader(http.StatusInternalServerError)
			case req.Method == http.MethodPut:
				_, _ = w.Write([]byte(`{"id": "ns/big", "options": ""}`))
			default:
				w.WriteHeader(http.StatusOK)
			}
		}))
		t.Cleanup(srv.Close)
		r.servers[ip] = srv
	}
	return r
}

func (r *rankRecorder) methods(ip string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls[ip]...)
}

func (r *rankRecorder) saw(ip, method string) bool {
	for _, m := range r.methods(ip) {
		if m == method {
			return true
		}
	}
	return false
}

// supervisorsFrom wires the adapter to one recorder server per Pod address.
func (r *rankRecorder) supervisorsFrom(a *Adapter) {
	a.newSupervisor = func(podIP string) *Supervisor {
		srv := r.servers[podIP]
		return &Supervisor{client: srv.Client(), baseURL: srv.URL}
	}
	// Never reached in these tests: the admission fails while creating ranks,
	// before anything is waited on. Pointed at a closed port so a change that
	// DID reach it fails loudly rather than hanging.
	a.newEngine = func(Endpoint) *Engine {
		return &Engine{client: http.DefaultClient, baseURL: "http://127.0.0.1:1", pollWait: time.Millisecond}
	}
}

// A group admission that fails partway takes back the ranks it already created.
//
// Ranks are created workers-first and the leader last, so the most likely
// failure -- the leader -- happens with every worker already holding its GPU.
// Those instances are useless alone: a group that never formed serves nothing,
// and vLLM does not release a rank because its siblings went away. Left behind
// they hold the accelerators of a whole group until the Pods are replaced, and
// nothing would report why -- the model reads as absent, admission is retried,
// and the retry finds the ranks occupied.
func TestAFailedGroupAdmissionRollsBackTheRanksItCreated(t *testing.T) {
	pods := []client.Object{
		groupPodPtr("warm-0", 0, 3),   // leader, 10.0.0.1 -- created LAST
		groupPodPtr("warm-0-1", 1, 3), // 10.0.0.2
		groupPodPtr("warm-0-2", 2, 3), // 10.0.0.3
	}
	const leaderIP, worker1IP, worker2IP = "10.0.0.1", "10.0.0.2", "10.0.0.3"

	// The leader refuses, with both workers already up.
	rec := newRankRecorder(t, []string{leaderIP, worker1IP, worker2IP},
		map[string]bool{leaderIP: true})

	a := &Adapter{client: fakeClientWith(pods...), namespace: "ns"}
	rec.supervisorsFrom(a)

	model := ModelRef{Namespace: "ns", Variant: "big", EngineOptions: "--model big --nnodes 3"}
	err := a.warmGroup(context.Background(), groupPodPtr("warm-0", 0, 3), model, Ram)

	if err == nil {
		t.Fatal("a rank that could not be created must fail the admission")
	}
	if !strings.Contains(err.Error(), "create rank 0") {
		t.Errorf("the error must name the rank that failed, got: %v", err)
	}
	for _, ip := range []string{worker1IP, worker2IP} {
		if !rec.saw(ip, http.MethodDelete) {
			t.Errorf("worker %s was created and never rolled back (%v): it holds its GPU "+
				"on an engine that can never form", ip, rec.methods(ip))
		}
	}
}

// The rollback reaches every rank even when one of them cannot be reached.
//
// A supervisor that is down is exactly when this matters. Rollback runs in
// creation order and meets the unreachable Pod first, so giving up there would
// leave the rank behind it holding its GPU -- and that rank is one the pool
// could still have freed.
func TestARollbackThatCannotReachOneRankStillClearsTheOthers(t *testing.T) {
	pods := []client.Object{
		groupPodPtr("warm-0", 0, 3),
		groupPodPtr("warm-0-1", 1, 3),
		groupPodPtr("warm-0-2", 2, 3),
	}
	const leaderIP, worker1IP, worker2IP = "10.0.0.1", "10.0.0.2", "10.0.0.3"

	rec := newRankRecorder(t, []string{leaderIP, worker1IP, worker2IP},
		map[string]bool{leaderIP: true})

	// Nothing is listening here. Rank 2 is created first and rolled back first,
	// so it is the one that goes away between the two.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	a := &Adapter{client: fakeClientWith(pods...), namespace: "ns"}
	rec.supervisorsFrom(a)
	perPod := a.newSupervisor
	a.newSupervisor = func(podIP string) *Supervisor {
		if podIP == worker2IP && rec.saw(podIP, http.MethodPut) {
			// Up long enough to be created, gone by the time it is rolled back.
			return &Supervisor{client: rec.servers[podIP].Client(), baseURL: deadURL}
		}
		return perPod(podIP)
	}

	model := ModelRef{Namespace: "ns", Variant: "big", EngineOptions: "--model big --nnodes 3"}
	if err := a.warmGroup(context.Background(), groupPodPtr("warm-0", 0, 3), model, Ram); err == nil {
		t.Fatal("the admission must still fail")
	}

	if !rec.saw(worker1IP, http.MethodDelete) {
		t.Errorf("rank 1 was not rolled back after an unreachable rank came first (%v)",
			rec.methods(worker1IP))
	}
}

// Nothing is rolled back when nothing was created. A group whose FIRST rank
// refuses holds no instances anywhere, and deleting on the strength of an
// attempt would remove an instance this admission never made.
func TestAGroupThatCreatedNothingRollsBackNothing(t *testing.T) {
	pods := []client.Object{
		groupPodPtr("warm-0", 0, 2),
		groupPodPtr("warm-0-1", 1, 2),
	}
	const leaderIP, workerIP = "10.0.0.1", "10.0.0.2"

	// The worker is rank 1 and therefore the first to be created.
	rec := newRankRecorder(t, []string{leaderIP, workerIP}, map[string]bool{workerIP: true})

	a := &Adapter{client: fakeClientWith(pods...), namespace: "ns"}
	rec.supervisorsFrom(a)

	model := ModelRef{Namespace: "ns", Variant: "big", EngineOptions: "--model big --nnodes 2"}
	if err := a.warmGroup(context.Background(), groupPodPtr("warm-0", 0, 2), model, Ram); err == nil {
		t.Fatal("the admission must fail")
	}

	for _, ip := range []string{leaderIP, workerIP} {
		if rec.saw(ip, http.MethodDelete) {
			t.Errorf("%s was deleted though this admission created nothing (%v)",
				ip, rec.methods(ip))
		}
	}
}
