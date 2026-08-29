package warmpool

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/policy"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

func lentMembership(podName, variant string, state pool.State) pool.Membership {
	return pool.Membership{
		Pod:   types.NamespacedName{Namespace: "pool-ns", Name: podName},
		Model: pool.ModelRef{Namespace: "tenant", Variant: variant},
		State: state,
	}
}

func bridgeDemandFor(variant, target string) policy.VariantDemand {
	return policy.VariantDemand{
		Model:  pool.ModelRef{Namespace: "tenant", Variant: variant},
		Target: target,
	}
}

// Only a SERVING Pod is a bridge, and it is published under the scale target's
// name.
//
// The name matters as much as the set. The pool keys its warm set by the
// ScaledObject's name; the collector and the analyzer key a variant by the scale
// target it drives. Publishing the pool's name would attribute the Pod's metrics
// to a variant nothing recognises, which is the same as not publishing at all --
// except that it would look as though it were working.
func TestOnlyALentPodIsPublishedAsABridge(t *testing.T) {
	memberships := []pool.Membership{
		lentMembership("pool-0", "variant-a", pool.Serving),
		lentMembership("pool-1", "variant-a", pool.Asleep),
		lentMembership("pool-2", "variant-b", pool.Waking),
		lentMembership("pool-3", "variant-b", pool.Serving),
	}
	variants := []policy.VariantDemand{
		bridgeDemandFor("variant-a", "deploy-a"),
		bridgeDemandFor("variant-b", "deploy-b"),
	}

	got := lentPodsByTarget(memberships, variants)

	want := map[string]string{"pool-0": "deploy-a", "pool-3": "deploy-b"}
	if len(got) != len(want) {
		t.Fatalf("lending = %v, want %v", got, want)
	}
	for pod, target := range want {
		if got[pod] != target {
			t.Errorf("lending[%s] = %q, want %q", pod, got[pod], target)
		}
	}
}

// A Pod lent to a variant that has since gone is left out rather than guessed
// at. Attributing it to a variant nothing is scaling would add demand no
// optimizer pass could act on, and the Pod is about to be reclaimed as an orphan
// anyway.
func TestAPodLentToAVanishedVariantIsNotPublished(t *testing.T) {
	memberships := []pool.Membership{lentMembership("pool-0", "variant-gone", pool.Serving)}

	if got := lentPodsByTarget(memberships, nil); len(got) != 0 {
		t.Errorf("lending = %v, want nothing for a variant no longer in the demand", got)
	}
}

// A variant whose demand carries no scale target is skipped for the same reason:
// there is no name to publish it under that anything downstream would match.
func TestAVariantWithNoScaleTargetIsSkipped(t *testing.T) {
	memberships := []pool.Membership{lentMembership("pool-0", "variant-a", pool.Serving)}
	variants := []policy.VariantDemand{bridgeDemandFor("variant-a", "")}

	if got := lentPodsByTarget(memberships, variants); len(got) != 0 {
		t.Errorf("lending = %v, want nothing when the variant names no scale target", got)
	}
}

// Nothing lent publishes an empty map, not a nil one that a caller might read as
// "no answer". The pool publishes this on every pass it could observe itself,
// and an empty answer is what clears a Pod that has been handed back.
func TestNothingLentPublishesAnEmptyMap(t *testing.T) {
	memberships := []pool.Membership{lentMembership("pool-0", "variant-a", pool.Asleep)}
	variants := []policy.VariantDemand{bridgeDemandFor("variant-a", "deploy-a")}

	got := lentPodsByTarget(memberships, variants)
	if got == nil {
		t.Fatal("want an empty map, not nil")
	}
	if len(got) != 0 {
		t.Errorf("lending = %v, want empty", got)
	}
}
