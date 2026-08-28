package warmpool

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

// alwaysReady is a Trigger that never stops saying "go round again".
//
// Not a strawman: the real trigger fires on every decision the store publishes
// for any watched target, and a busy fleet publishes constantly. What the
// reconciler must not do is take that as permission to run without limit.
type alwaysReady struct {
	ch   chan struct{}
	stop chan struct{}
}

func newAlwaysReady() *alwaysReady {
	t := &alwaysReady{ch: make(chan struct{}, 1), stop: make(chan struct{})}
	go func() {
		for {
			select {
			case <-t.stop:
				return
			case t.ch <- struct{}{}:
			}
		}
	}()
	return t
}

func (t *alwaysReady) Notify() <-chan struct{} { return t.ch }

// countingPool counts observations, which is what a pass costs on a cluster:
// every one is several HTTP calls against each pool Pod's launcher.
type countingPool struct {
	*fakePool
	observations atomic.Int64
}

func (p *countingPool) ListWarm(ctx context.Context) ([]pool.Membership, error) {
	p.observations.Add(1)
	return p.fakePool.ListWarm(ctx)
}

// A trigger that is always ready must not spin the loop.
//
// Measured on a real cluster before this floor existed: 19,992 supervisor calls
// in 64 seconds -- about 310 a second -- against a launcher that was at the same
// time trying to start an engine. It buried the engine's own output, which is
// what made a genuine fan-out failure far harder to read than it should have
// been.
func TestATriggerThatIsAlwaysReadyDoesNotSpinTheLoop(t *testing.T) {
	p := &countingPool{fakePool: &fakePool{}}
	r := New(p, &staticDemand{}, testConfig())
	r.Interval = time.Hour // ticks must not contribute; this is about the trigger
	r.MinGap = 50 * time.Millisecond
	trigger := newAlwaysReady()
	defer close(trigger.stop)
	r.Trigger = trigger

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = r.Start(ctx)

	passes := p.observations.Load()

	// 500ms at a 50ms floor is about ten passes. Bounded generously because what
	// is asserted is the ORDER of magnitude: without the floor this same loop
	// runs thousands of times in half a second.
	if passes > 20 {
		t.Errorf("ran %d passes in 500ms with a 50ms floor; the loop is not being held back", passes)
	}
	// And it must still run. A floor that stopped the loop entirely would satisfy
	// the bound above while destroying the reason the trigger exists.
	if passes < 2 {
		t.Errorf("ran %d passes; the trigger must still drive the loop", passes)
	}
}
