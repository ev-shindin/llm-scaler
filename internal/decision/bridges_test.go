package decision

import (
	"testing"
	"time"
)

// variantA is the variant these tests lend a Pod to.
const variantA = "variant-a"

// A lent Pod resolves to the variant it is serving, not to the pool that owns
// it. That translation is the whole point of the store: the collector's
// ownerReference walk reaches the pool's scale target, which is not the answer.
func TestALentPodResolvesToTheVariantItServes(t *testing.T) {
	s := NewBridgeStore()
	now := time.Now()
	s.Publish("tenant", map[string]string{"pool-0": variantA}, now)

	if got, lent := s.VariantFor("tenant", "pool-0", time.Minute, now); !lent || got != variantA {
		t.Errorf("VariantFor(pool-0) = %q, %v; want variant-a, true", got, lent)
	}
	if got, lent := s.VariantFor("tenant", "pool-1", time.Minute, now); lent {
		t.Errorf("VariantFor(pool-1) = %q, %v; want no lending for a Pod that is not lent", got, lent)
	}
	if got, lent := s.VariantFor("other", "pool-0", time.Minute, now); lent {
		t.Errorf("VariantFor(other/pool-0) = %q, %v; want nothing across namespaces", got, lent)
	}
}

// A Pod handed back stops being a bridge.
//
// Publishing is wholesale for exactly this: a store that only ever learned of
// new lendings would go on attributing a returned Pod's metrics to a variant it
// no longer serves, adding demand for load nobody is carrying.
func TestAReturnedPodStopsBeingABridge(t *testing.T) {
	s := NewBridgeStore()
	now := time.Now()
	s.Publish("tenant", map[string]string{"pool-0": variantA, "pool-1": "variant-b"}, now)
	s.Publish("tenant", map[string]string{"pool-1": "variant-b"}, now.Add(time.Second))

	if got, lent := s.VariantFor("tenant", "pool-0", time.Minute, now.Add(time.Second)); lent {
		t.Errorf("VariantFor(pool-0) = %q, %v after it was returned; want nothing", got, lent)
	}
	if _, lent := s.VariantFor("tenant", "pool-1", time.Minute, now.Add(time.Second)); !lent {
		t.Error("pool-1 is still lent and must still resolve")
	}
}

// An EMPTY publish clears the namespace. The pool publishes on every pass it
// could observe itself, so "nothing is lent" is a real answer that has to
// overwrite the previous one.
func TestAnEmptyLendingClearsTheNamespace(t *testing.T) {
	s := NewBridgeStore()
	now := time.Now()
	s.Publish("tenant", map[string]string{"pool-0": variantA}, now)
	s.Publish("tenant", map[string]string{}, now.Add(time.Second))

	if got, lent := s.VariantFor("tenant", "pool-0", time.Minute, now.Add(time.Second)); lent {
		t.Errorf("VariantFor = %q, %v; want nothing once the pool reported no lending", got, lent)
	}
}

// A STALE map is not acted on. The pool republishes every reconcile pass, so an
// old one means its reconciler has stopped -- and a lending that may since have
// ended would keep adding demand for as long as the controller ran.
func TestAStaleLendingIsNotUsed(t *testing.T) {
	s := NewBridgeStore()
	now := time.Now()
	s.Publish("tenant", map[string]string{"pool-0": variantA}, now.Add(-10*time.Minute))

	if got, lent := s.VariantFor("tenant", "pool-0", 2*time.Minute, now); lent {
		t.Errorf("VariantFor(stale) = %q, %v; want nothing", got, lent)
	}
	if _, lent := s.VariantFor("tenant", "pool-0", 0, now); !lent {
		t.Error("with no staleness window the reading should still answer")
	}
}

// The store copies the map it is given, so a caller that goes on using its own
// map cannot change what the collector reads.
func TestTheLendingMapIsCopied(t *testing.T) {
	s := NewBridgeStore()
	now := time.Now()
	mine := map[string]string{"pool-0": variantA}
	s.Publish("tenant", mine, now)
	mine["pool-0"] = "variant-b"
	mine["pool-9"] = "variant-c"

	if got, _ := s.VariantFor("tenant", "pool-0", time.Minute, now); got != variantA {
		t.Errorf("VariantFor = %q; want variant-a -- the published map was mutated afterwards", got)
	}
	if _, lent := s.VariantFor("tenant", "pool-9", time.Minute, now); lent {
		t.Error("a Pod added to the caller's map after publishing must not appear")
	}
}

// The package-level helpers reach the store the controller wires up.
func TestTheBridgeHelpersUseTheDefaultStore(t *testing.T) {
	t.Cleanup(DefaultBridges.Reset)
	now := time.Now()
	PublishBridges("tenant", map[string]string{"pool-0": variantA}, now)

	if got, lent := BridgeVariant("tenant", "pool-0", time.Minute, now); !lent || got != variantA {
		t.Errorf("BridgeVariant = %q, %v; want variant-a, true", got, lent)
	}
}

// Bridge supply is published per variant, and ZERO is a reading rather than an
// absence -- "this variant has no bridge" is what a switching decision most
// needs to know, and a missing entry would leave the last positive figure
// standing after the Pod went back.
func TestBridgeSupplyRecordsZeroAsAnAnswer(t *testing.T) {
	s := NewWarmPoolSupplyStore()
	now := time.Now()
	s.Publish("tenant", variantA, 2, 2000, now)
	s.Publish("tenant", variantA, 0, 0, now.Add(time.Second))

	got, known := s.Get("tenant", variantA, time.Minute, now.Add(time.Second))
	if !known {
		t.Fatal("a zero reading must still be a reading")
	}
	if got.Replicas != 0 || got.Capacity != 0 {
		t.Errorf("supply = %+v, want zero once the bridge was returned", got)
	}
	if _, known := s.Get("tenant", "never-measured", time.Minute, now); known {
		t.Error("a variant nothing was published for must read as unknown, not as zero")
	}
}
