package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The hand-back window. Clearing the upstream first answered every request the
// EPP still dispatched -- the probe period plus the endpoint update, ~1.6 s --
// with "no model is awake in this Pod". Measured on a two-model run: one to
// two such 503s at every hand-back, none anywhere else. Draining fails
// readiness while the serving port keeps forwarding, so the EPP drops the Pod
// before the upstream goes.

func TestDrainingFailsReadinessButKeepsServing(t *testing.T) {
	a := engine(t, "modelA")
	s, base := startProxy(t)
	mustSetUpstream(t, s, a)

	rec := httptest.NewRecorder()
	s.DrainHandler(rec, httptest.NewRequest(http.MethodPost, DrainPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("drain with an upstream: want 200, got %d", rec.Code)
	}
	if body := decode(t, rec); !body.Draining || body.Address != a {
		t.Fatalf("drain must report the state it entered: %+v", body)
	}

	rec = httptest.NewRecorder()
	s.ReadyHandler(rec, httptest.NewRequest(http.MethodGet, ReadyPath, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a draining Pod must be NotReady so the EPP drops it: got %d", rec.Code)
	}

	// THE point: what the EPP still sends is served, not refused.
	code, body := ask(t, http.DefaultClient, base, "still in the doorway")
	if code != http.StatusOK || body != "modelA:still in the doorway" {
		t.Fatalf("a request during the drain must be forwarded; got %d %q", code, body)
	}
	if n := s.refused.Load(); n != 0 {
		t.Fatalf("nothing was refused, yet refused=%d", n)
	}
}

func TestClearEndsTheDrainAndRefusesFromThenOn(t *testing.T) {
	a := engine(t, "modelA")
	s, base := startProxy(t)
	mustSetUpstream(t, s, a)
	if !s.Drain() {
		t.Fatal("Drain with an upstream must report true")
	}

	rec := httptest.NewRecorder()
	s.UpstreamHandler(rec, httptest.NewRequest(http.MethodDelete, UpstreamPath, nil))
	if rec.Code != http.StatusNoContent || s.Upstream() != "" || s.Draining() {
		t.Fatalf("clear must end the drain: code %d upstream %q draining %v", rec.Code, s.Upstream(), s.Draining())
	}
	code, _ := ask(t, http.DefaultClient, base, "too late")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("after the clear a request is refused: got %d", code)
	}
	if n := s.refused.Load(); n != 1 {
		t.Fatalf("the refusal must be counted (and logged): refused=%d", n)
	}
}

func TestANewPointEndsTheDrain(t *testing.T) {
	// A lend arriving while a hand-back is in progress is a normal sequence in
	// a pool shared by several models. The new point must leave the Pod Ready.
	a := engine(t, "modelA")
	b := engine(t, "modelB")
	s, _ := startProxy(t)
	mustSetUpstream(t, s, a)
	s.Drain()

	rec := httptest.NewRecorder()
	s.UpstreamHandler(rec, put(t, b))
	if rec.Code != http.StatusOK || s.Draining() {
		t.Fatalf("pointing at the next model must clear the drain: code %d draining %v", rec.Code, s.Draining())
	}
	rec = httptest.NewRecorder()
	s.ReadyHandler(rec, httptest.NewRequest(http.MethodGet, ReadyPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("Ready again after the new point: got %d", rec.Code)
	}
}

func TestDrainingTwiceReportsNothingNewToDrain(t *testing.T) {
	// A retry of a hand-back that drained and then failed comes back through
	// Drain. The Pod has been NotReady since the first drain, so the caller is
	// told "nothing to drain" and skips a wait the EPP satisfied long ago --
	// while the draining state itself is kept.
	a := engine(t, "modelA")
	s := New(wideConfig())
	mustSetUpstream(t, s, a)
	if !s.Drain() {
		t.Fatal("first drain must report true")
	}
	rec := httptest.NewRecorder()
	s.DrainHandler(rec, httptest.NewRequest(http.MethodPost, DrainPath, nil))
	if rec.Code != http.StatusNoContent || !s.Draining() {
		t.Fatalf("second drain: want 204 and still draining, got %d / %v", rec.Code, s.Draining())
	}
}

func TestDrainingNothingIsNotAnError(t *testing.T) {
	// A hand-back of a Pod that is already out of service: nothing to drain,
	// and the caller can tell (204) without asking twice.
	s := New(wideConfig())
	rec := httptest.NewRecorder()
	s.DrainHandler(rec, httptest.NewRequest(http.MethodPost, DrainPath, nil))
	if rec.Code != http.StatusNoContent || s.Draining() {
		t.Fatalf("drain with no upstream: want 204 and not draining, got %d / %v", rec.Code, s.Draining())
	}
	rec = httptest.NewRecorder()
	s.DrainHandler(rec, httptest.NewRequest(http.MethodGet, DrainPath, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("drain is a POST: got %d for GET", rec.Code)
	}
}
