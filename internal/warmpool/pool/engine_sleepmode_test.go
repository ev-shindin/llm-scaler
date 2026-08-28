package pool

import (
	"context"
	"strings"
	"testing"
)

// Sleeping must DRAIN, not abort.
//
// /sleep takes a mode and defaults to "abort", which cancels every in-flight
// request the moment it lands. Returning a borrowed Pod is routine -- it happens
// whenever the ordinary replicas arrive -- so a sleep that aborts turns the pool
// into a source of failed requests rather than a way of avoiding cold starts.
//
// Pinned on the wire rather than on a constant: the mode reaches vLLM as a query
// parameter, and a change that dropped it would leave the code reading correctly
// while the engine went back to aborting, which nothing else here would notice.
func TestSleepAsksTheEngineToDrainFirst(t *testing.T) {
	f := newFakeEngine(t)

	if err := f.engine().Sleep(context.Background(), Ram); err != nil {
		t.Fatalf("Sleep: %v", err)
	}

	var sleepCall string
	for _, c := range f.seen() {
		if strings.Contains(c, "/sleep") {
			sleepCall = c
		}
	}
	if sleepCall == "" {
		t.Fatal("no /sleep call was made")
	}
	if !strings.Contains(sleepCall, "mode=wait") {
		t.Errorf("sleep called as %q; without mode=wait vLLM aborts every in-flight request", sleepCall)
	}
	if !strings.Contains(sleepCall, "level=1") {
		t.Errorf("sleep called as %q; the pool sleeps at level 1", sleepCall)
	}
}
