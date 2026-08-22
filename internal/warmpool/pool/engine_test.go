package pool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEngine stands in for a vLLM instance: it records the calls made to it and
// can be told to answer /health only after a delay, which is how the wake race
// is exercised.
type fakeEngine struct {
	mu         sync.Mutex
	calls      []string
	sleeping   bool
	servingAt  time.Time // /health fails before this
	server     *httptest.Server
	wakeStatus int
}

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	f := &fakeEngine{wakeStatus: http.StatusOK}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path+queryOf(r))
		switch {
		case strings.HasPrefix(r.URL.Path, "/sleep"):
			f.sleeping = true
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/wake_up":
			if f.wakeStatus != http.StatusOK {
				http.Error(w, "CUDA error: out of memory", f.wakeStatus)
				return
			}
			f.sleeping = false
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == sleepStatePath:
			_, _ = w.Write([]byte(`{"is_sleeping":` + boolText(f.sleeping) + `}`))
		case r.URL.Path == healthPath:
			if time.Now().Before(f.servingAt) {
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

const healthPath = "/health"

// sleepStatePath is what the pool asks instead of inferring from /health: a
// sleeping vLLM answers /health perfectly well, because the process is alive.
const sleepStatePath = "/is_sleeping"

func queryOf(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func (f *fakeEngine) engine() *Engine {
	return &Engine{
		client:   f.server.Client(),
		baseURL:  f.server.URL,
		pollWait: 5 * time.Millisecond,
	}
}

func (f *fakeEngine) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func TestSleepLevelComesFromTheTier(t *testing.T) {
	// The level belongs to the pool, not the call site: a level-1 Pod reserved
	// host memory when it was created.
	if SleepLevel(Ram) != 1 || SleepLevel(Disk) != 2 {
		t.Fatalf("ram=%d disk=%d", SleepLevel(Ram), SleepLevel(Disk))
	}
	f := newFakeEngine(t)
	if err := f.engine().Sleep(context.Background(), Disk); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if got := f.seen(); len(got) != 1 || !strings.Contains(got[0], "level=2") {
		t.Fatalf("disk tier must sleep at level 2: %v", got)
	}
}

func TestWakeWaitsForTheEngineToAnswer(t *testing.T) {
	// The property this exists for: /wake_up returning is NOT the engine
	// serving. A caller that puts a Pod into service on the strength of the
	// call alone has built the race it was trying to avoid.
	f := newFakeEngine(t)
	f.servingAt = time.Now().Add(150 * time.Millisecond)

	start := time.Now()
	if err := f.engine().Wake(context.Background()); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("Wake returned after %s, before the engine could answer", elapsed)
	}
	calls := f.seen()
	if calls[0] != "POST /wake_up" {
		t.Fatalf("wake must come first: %v", calls)
	}
	if calls[len(calls)-1] != "GET "+healthPath {
		t.Fatalf("wake must end by confirming the engine answers: %v", calls)
	}
}

func TestWakeReportsAnEngineThatCannotWake(t *testing.T) {
	// Two of three unbound sleepers failed exactly this way on the cluster. A
	// pool must fall through to the cold path, so the error has to surface.
	f := newFakeEngine(t)
	f.wakeStatus = http.StatusInternalServerError

	err := f.engine().Wake(context.Background())
	if err == nil {
		t.Fatal("a failed wake must be an error, not a silent miss")
	}
	if !strings.Contains(err.Error(), "out of memory") {
		t.Errorf("the engine's reason must survive: %v", err)
	}
}

func TestWakeGivesUpWithTheContext(t *testing.T) {
	f := newFakeEngine(t)
	f.servingAt = time.Now().Add(time.Hour) // never answers

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := f.engine().Wake(ctx); err == nil {
		t.Fatal("an engine that never answers must not block the loop forever")
	}
}

func TestIsSleepingAsksTheEngineRatherThanInferring(t *testing.T) {
	f := newFakeEngine(t)
	e := f.engine()
	ctx := context.Background()

	if err := e.Sleep(ctx, Ram); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	asleep, err := e.IsSleeping(ctx)
	if err != nil || !asleep {
		t.Fatalf("IsSleeping = %v, %v", asleep, err)
	}

	if err := e.Wake(ctx); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if asleep, err = e.IsSleeping(ctx); err != nil || asleep {
		t.Fatalf("after waking IsSleeping = %v, %v", asleep, err)
	}
}
