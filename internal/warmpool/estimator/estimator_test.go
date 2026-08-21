package estimator

import (
	"fmt"
	"testing"
	"time"
)

var base = time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)

func at(seconds int) time.Time { return base.Add(time.Duration(seconds) * time.Second) }

func spike(seconds int, model string) Event {
	return Event{At: at(seconds), Model: model, Kind: Spike}
}

func request(seconds int, model string) Event {
	return Event{At: at(seconds), Model: model, Kind: Request}
}

func cfg() Config {
	return Config{
		Memberships: 2,
		Slots:       1,
		Hold:        60 * time.Second,
		ColdStart:   41 * time.Second,
		Wake:        3 * time.Second,
		FillOnMiss:  true,
	}
}

func run(t *testing.T, events []Event, c Config) Result {
	t.Helper()
	got, err := Run(events, c)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return got
}

func TestFirstSpikeAlwaysMisses(t *testing.T) {
	// Nothing is resident before demand exists, so a cold cache cannot hit --
	// and the pool must cost nothing extra when it misses.
	got := run(t, []Event{spike(0, "a")}, cfg())
	if got.Misses != 1 || got.Hits != 0 {
		t.Fatalf("want 1 miss 0 hits, got %+v", got)
	}
	if got.MeanWait != got.MeanWaitBaseline {
		t.Errorf("a miss must cost exactly a cold start: got %s, baseline %s",
			got.MeanWait, got.MeanWaitBaseline)
	}
}

func TestSecondSpikeHitsOnceResident(t *testing.T) {
	// Far enough apart that the first spike's slot has been released.
	got := run(t, []Event{spike(0, "a"), spike(120, "a")}, cfg())
	if got.Hits != 1 || got.Misses != 1 {
		t.Fatalf("want 1 hit 1 miss, got %+v", got)
	}
	if want := 38 * time.Second; got.Saved != want {
		t.Errorf("saved = %s, want %s (cold 41s - wake 3s)", got.Saved, want)
	}
	if want := (41*time.Second + 3*time.Second) / 2; got.MeanWait != want {
		t.Errorf("mean wait = %s, want %s", got.MeanWait, want)
	}
}

func TestBlockedWhenEverySlotIsBusy(t *testing.T) {
	// Two models resident, one slot. The second spike arrives inside the first
	// one's hold, so it is resident but has nowhere to run.
	events := make([]Event, 0, 6)
	events = append(events,
		spike(0, "a"), spike(200, "a"), // make "a" resident, then release
		spike(400, "a"), // holds the only slot from 400 to 460
		spike(410, "b"), // miss: "b" not resident yet
		spike(600, "b"), // resident now; slot free again
	)
	got := run(t, events, cfg())
	if got.Blocked != 0 {
		t.Fatalf("no block expected yet, got %+v", got)
	}

	// Now force contention: "b" spikes while "a" holds the slot.
	events = append(events, spike(605, "a"))
	got = run(t, events, cfg())
	if got.Blocked != 1 {
		t.Fatalf("want 1 blocked spike, got %+v", got)
	}
	// A block costs a cold start, not a wake -- it must never look like a hit.
	if got.BlockRatio() == 0 {
		t.Error("BlockRatio must expose the block; a hit rate alone hides it")
	}
}

func TestEvictionMakesAResidentModelMiss(t *testing.T) {
	// Capacity 2, three models: the least recently wanted is evicted and its
	// next spike misses. This is what sizing C is about.
	c := cfg()
	events := []Event{
		spike(0, "a"), spike(10, "b"), spike(20, "c"), // a evicted by c
		spike(600, "a"),
	}
	got := run(t, events, c)
	if got.Hits != 0 {
		t.Fatalf("every spike here should miss, got %+v", got)
	}
	if got.Misses != 4 {
		t.Fatalf("want 4 misses, got %+v", got)
	}

	// With room for all three, the last spike hits.
	c.Memberships = 3
	got = run(t, events, c)
	if got.Hits != 1 {
		t.Fatalf("want 1 hit with C=3, got %+v", got)
	}
}

func TestRequestsShapeTheWarmSetWithoutSpiking(t *testing.T) {
	// A request must not count as a spike, but it must keep its model warm --
	// otherwise a busy model is evicted by an idle one that merely spiked.
	c := cfg()
	c.Memberships = 2
	events := []Event{
		spike(0, "a"), spike(10, "b"),
		request(20, "a"), // refresh "a" so "c" evicts "b" instead
		spike(30, "c"),
		spike(600, "a"),
	}
	got := run(t, events, c)
	if got.Spikes != 4 {
		t.Fatalf("requests must not count as spikes: got %d", got.Spikes)
	}
	if got.Hits != 1 {
		t.Fatalf("want the refreshed model to hit, got %+v", got)
	}
	if got.PerModel["a"].Hits != 1 {
		t.Errorf("the hit belongs to a: %+v", got.PerModel)
	}
}

func TestFillOnMissOffIsThePessimisticBound(t *testing.T) {
	c := cfg()
	c.FillOnMiss = false
	got := run(t, []Event{spike(0, "a"), spike(600, "a")}, c)
	if got.Hits != 0 || got.Misses != 2 {
		t.Fatalf("without fill-on-miss nothing ever becomes resident: %+v", got)
	}
}

func TestSlotIsReleasedExactlyAfterHold(t *testing.T) {
	c := cfg()
	c.Hold = 60 * time.Second
	resident := []Event{spike(0, "a"), spike(0, "b")}

	// 59s after the hit, the slot is still held.
	busy := run(t, append(resident, spike(300, "a"), spike(359, "b")), c)
	if busy.Blocked != 1 {
		t.Fatalf("slot must still be held at +59s, got %+v", busy)
	}
	// 61s after, it is free.
	freed := run(t, append(resident, spike(300, "a"), spike(361, "b")), c)
	if freed.Blocked != 0 || freed.Hits != 2 {
		t.Fatalf("slot must be free at +61s, got %+v", freed)
	}
}

func TestUnsortedTraceGivesTheSameAnswer(t *testing.T) {
	// Logs arrive out of order; the answer must not depend on file order.
	ordered := []Event{spike(0, "a"), spike(120, "a"), spike(240, "a")}
	shuffled := []Event{ordered[2], ordered[0], ordered[1]}
	a := run(t, ordered, cfg())
	b := run(t, shuffled, cfg())
	if a.Hits != b.Hits || a.Misses != b.Misses || a.Saved != b.Saved {
		t.Fatalf("order changed the result: %+v vs %+v", a, b)
	}
}

func TestConfigIsRefusedWhenAPoolCannotHelp(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config)
	}{
		{"no memberships", func(c *Config) { c.Memberships = 0 }},
		{"no slots", func(c *Config) { c.Slots = 0 }},
		{"no hold", func(c *Config) { c.Hold = 0 }},
		{"wake slower than cold start", func(c *Config) { c.Wake = c.ColdStart }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := cfg()
			tc.edit(&c)
			if _, err := Run(nil, c); err == nil {
				t.Fatal("want an error, got none")
			}
		})
	}
}

func TestRatiosSumToOne(t *testing.T) {
	events := make([]Event, 0, 50)
	for i := 0; i < 50; i++ {
		events = append(events, spike(i*30, fmt.Sprintf("m%d", i%7)))
	}
	got := run(t, events, cfg())
	sum := got.HitRatio() + got.BlockRatio() + got.MissRatio()
	if sum < 0.999 || sum > 1.001 {
		t.Fatalf("hit+block+miss = %f, want 1; every spike has exactly one outcome", sum)
	}
	if got.Hits+got.Blocked+got.Misses != got.Spikes {
		t.Fatalf("counts do not partition the spikes: %+v", got)
	}
}

func TestMeanWaitNeverExceedsTheBaseline(t *testing.T) {
	// The claim a reader most needs to trust: a pool cannot make anything
	// slower than it is today, whatever the trace.
	events := make([]Event, 0, 200)
	for i := 0; i < 200; i++ {
		events = append(events, spike(i*7, fmt.Sprintf("m%d", i%23)))
	}
	for _, c := range []int{1, 4, 16} {
		for _, k := range []int{1, 2, 8} {
			cf := cfg()
			cf.Memberships, cf.Slots = c, k
			got := run(t, events, cf)
			if got.MeanWait > got.MeanWaitBaseline {
				t.Fatalf("C=%d K=%d: mean wait %s exceeds baseline %s",
					c, k, got.MeanWait, got.MeanWaitBaseline)
			}
		}
	}
}
