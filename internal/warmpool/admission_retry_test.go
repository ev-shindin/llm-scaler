package warmpool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/policy"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

// Does a pool that FAILED to load a model try again?
//
// Asked because a real pool looked as though it did not. On a cluster where the
// engine could not start, one admission failed and then nothing further happened
// for ten minutes -- no second attempt, no state change -- until the controller
// was restarted, at which point it admitted immediately. Reasoning from the logs
// went nowhere twice: the registry entry could not have aged out (a push trigger
// HOLDS it), and the admission claim releases on return.
//
// So this asks the code directly. If a failed admission is retried here, the
// cluster behaviour had an environmental cause and the instrumentation added
// alongside this test is what will catch it next time. If it is NOT retried,
// this is the bug, and it is one a unit test can hold still.
func TestAFailedAdmissionIsTriedAgain(t *testing.T) {
	const ns = "tenant"
	podRef := types.NamespacedName{Namespace: ns, Name: "pool-0"}
	model := pool.ModelRef{Namespace: ns, Variant: "wanted"}

	p := &fakePool{
		memberships: []pool.Membership{{Pod: podRef, State: pool.Absent}},
		warmErr:     errors.New("engine never served"),
	}
	one := 1
	demand := &staticDemand{variants: []policy.VariantDemand{
		{Model: model, Desired: 1, Ready: 1, WarmCopies: &one},
	}}

	r := New(p, demand, testConfig())
	r.Namespace = ns

	// Several passes, with room between them for the admission goroutine to
	// fail and release its claim.
	for i := 0; i < 5; i++ {
		if _, err := r.Once(context.Background()); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	attempts := 0
	for _, c := range p.seen() {
		if strings.HasPrefix(c, "warm ") {
			attempts++
		}
	}
	if attempts < 2 {
		t.Errorf("the pool tried to load the model %d time(s) across five passes; "+
			"a failed admission is never retried, so a pool that fails once stays empty", attempts)
	}
}
