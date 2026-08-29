package decision

import (
	"testing"
	"time"
)

// Contention is a BRAKE on the warm pool: while a model replica is being denied
// GPUs of some accelerator, a pool on that accelerator stops asking for more.
// Every branch below decides whether the brake is on, and getting any of them
// backwards either pins a pool at its current size forever or lets it compete
// with the traffic it exists to serve.

// The ordinary case: a namespace whose replicas are being denied that
// accelerator reads as contended, and one nobody is denying does not.
func TestContendedOnlyWhereReplicasAreBeingDenied(t *testing.T) {
	s := NewContentionStore()
	now := time.Now()
	s.Publish(map[string]map[string]bool{"tenant": {"H100": true}}, now)

	if !s.Contended("tenant", "H100", time.Minute, now) {
		t.Error("a namespace being denied H100 must read as contended")
	}
	if s.Contended("tenant", "A100", time.Minute, now) {
		t.Error("a different accelerator in the same namespace is not contended")
	}
	if s.Contended("other", "H100", time.Minute, now) {
		t.Error("a different namespace is not contended")
	}
}

// A SNAPSHOT THAT SAYS NOBODY IS DENIED must clear a previous one that said
// somebody was. Publishing only the non-empty snapshots would leave the last
// contended reading standing forever, holding a pool down long after the burst
// that caused it -- which the source comment on the publisher warns about.
func TestAnEmptySnapshotReleasesTheBrake(t *testing.T) {
	s := NewContentionStore()
	now := time.Now()
	s.Publish(map[string]map[string]bool{"tenant": {"H100": true}}, now)
	if !s.Contended("tenant", "H100", time.Minute, now) {
		t.Fatal("precondition: should be contended")
	}

	s.Publish(map[string]map[string]bool{}, now.Add(time.Second))

	if s.Contended("tenant", "H100", time.Minute, now.Add(time.Second)) {
		t.Error("an empty snapshot means nobody is being denied; the brake must come off")
	}
}

// A STALE reading is not evidence. If the optimizer stops publishing, the last
// snapshot ages out and the pool is released -- otherwise a controller that
// stopped deciding would hold every pool down indefinitely, and nothing would
// say why.
func TestAStaleReadingDoesNotHoldTheBrakeOn(t *testing.T) {
	s := NewContentionStore()
	now := time.Now()
	s.Publish(map[string]map[string]bool{"tenant": {"H100": true}}, now.Add(-10*time.Minute))

	if s.Contended("tenant", "H100", time.Minute, now) {
		t.Error("a reading older than maxAge must not hold the brake on")
	}
	if !s.Contended("tenant", "H100", time.Hour, now) {
		t.Error("within maxAge the same reading still applies")
	}
}

// Nothing published at all is not contention. A fleet that has never run the
// optimizer must not have its pools pinned.
func TestNoSnapshotIsNotContention(t *testing.T) {
	s := NewContentionStore()
	if s.Contended("tenant", "H100", time.Minute, time.Now()) {
		t.Error("with no snapshot at all, nothing is contended")
	}
	if _, ok := s.Get(); ok {
		t.Error("Get must report that there is no snapshot")
	}
}

// An accelerator nobody can name is not contended. A workload whose accelerator
// could not be resolved reads as an empty string, and treating that as a match
// would brake every pool in the namespace on the strength of a workload nobody
// could place.
func TestAnUnnamedAcceleratorIsNotContended(t *testing.T) {
	s := NewContentionStore()
	now := time.Now()
	s.Publish(map[string]map[string]bool{"tenant": {"H100": true}}, now)

	if s.Contended("tenant", "", time.Minute, now) {
		t.Error("an empty accelerator must not match a contended one")
	}
}
