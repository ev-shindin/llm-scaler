// Package estimator answers "what would a warm pool have delivered here?" from a
// trace of events, without a pool existing.
//
// The warm-pool design rests on two ratios nobody has measured on a real fleet:
// how often a spike finds its model already resident (a cache question), and how
// often it finds a free slot (a loss-system question). Both are decidable from
// data an installation already has, so this simulates the pool over a trace
// instead of asking anyone to hold GPUs on a hypothesis.
//
// The simulation is deliberately pure: a trace in, a result out, no clock, no
// cluster, no I/O. The same code serves live metrics, an offline traffic log and
// a synthetic generator, so a number produced in one context means the same
// thing in another.
package estimator

import (
	"fmt"
	"sort"
	"time"
)

// Kind distinguishes the two things a trace can record.
type Kind string

const (
	// Spike is a scale-up: a moment when capacity for a model was wanted and
	// was not already running. This is what a pool would have served.
	Spike Kind = "spike"
	// Request is a served request. Requests do not drive the simulation; they
	// establish which models are popular, which is what a cache is sized on.
	Request Kind = "request"
)

// Event is one line of a trace.
type Event struct {
	At    time.Time
	Model string
	Kind  Kind
}

// Config is the pool being simulated. Zero values are not useful; use Validate.
type Config struct {
	// Memberships is C: how many distinct models the pool can hold resident,
	// across every Pod. Coverage.
	Memberships int
	// Slots is K x M: how many models the pool can have awake at once.
	// Capacity.
	Slots int
	// Hold is W: how long a woken model occupies its slot before ordinary
	// replicas take over.
	Hold time.Duration
	// ColdStart is what a miss costs -- the time to serve without a pool.
	ColdStart time.Duration
	// Wake is what a hit costs.
	Wake time.Duration
	// FillOnMiss admits a model to the warm set when it misses, which models a
	// pool that learns from demand. With it false the warm set is preloaded
	// from popularity alone and never changes, which is the pessimistic bound.
	FillOnMiss bool
}

// Validate reports why a configuration cannot be simulated.
func (c Config) Validate() error {
	switch {
	case c.Memberships <= 0:
		return fmt.Errorf("memberships must be positive, got %d", c.Memberships)
	case c.Slots <= 0:
		return fmt.Errorf("slots must be positive, got %d", c.Slots)
	case c.Hold <= 0:
		return fmt.Errorf("hold must be positive, got %s", c.Hold)
	case c.ColdStart <= 0:
		return fmt.Errorf("cold start must be positive, got %s", c.ColdStart)
	case c.Wake < 0:
		return fmt.Errorf("wake must not be negative, got %s", c.Wake)
	case c.Wake >= c.ColdStart:
		return fmt.Errorf("wake %s is not faster than a cold start %s, so a pool saves nothing",
			c.Wake, c.ColdStart)
	}
	return nil
}

// Result is what the pool would have done.
type Result struct {
	Spikes  int
	Hits    int // resident, and a slot was free
	Blocked int // resident, but every slot was busy
	Misses  int // not resident

	// Saved is the total time a pool would have removed from the critical
	// path: one (ColdStart - Wake) per hit.
	Saved time.Duration
	// MeanWait is the mean time to capacity WITH the pool; MeanWaitBaseline is
	// the same without it, which is ColdStart by definition.
	MeanWait         time.Duration
	MeanWaitBaseline time.Duration

	// PerModel is keyed by model, for finding which models the pool serves and
	// which it never helps.
	PerModel map[string]ModelResult
}

// ModelResult is one model's share of the outcome.
type ModelResult struct {
	Spikes, Hits, Blocked, Misses int
	Saved                         time.Duration
}

// HitRatio is hits over spikes; zero when nothing spiked.
func (r Result) HitRatio() float64 { return ratio(r.Hits, r.Spikes) }

// BlockRatio is the share of spikes that found their model resident but every
// slot busy. It is the number that decides K, and it is invisible in a hit rate.
func (r Result) BlockRatio() float64 { return ratio(r.Blocked, r.Spikes) }

// MissRatio is the share of spikes whose model was not resident. It decides C.
func (r Result) MissRatio() float64 { return ratio(r.Misses, r.Spikes) }

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// Run simulates cfg over events, which need not be sorted.
//
// A spike resolves in one of three ways, and the distinction matters more than
// the hit rate alone: a MISS means the warm set was too small (raise C), a BLOCK
// means it was busy (raise K). Both degrade to a cold start, so a pool never
// makes anything slower than it is today.
func Run(events []Event, cfg Config) (Result, error) {
	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}

	ordered := make([]Event, len(events))
	copy(ordered, events)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].At.Before(ordered[j].At) })

	warm := newWarmSet(cfg.Memberships)
	// releases holds the times at which busy slots come free. It is small (K),
	// so a linear scan beats a heap and keeps the code obvious.
	releases := make([]time.Time, 0, cfg.Slots)

	result := Result{MeanWaitBaseline: cfg.ColdStart, PerModel: map[string]ModelResult{}}
	var totalWait time.Duration

	for _, ev := range ordered {
		if ev.Model == "" {
			continue
		}
		if ev.Kind == Request {
			// Requests only teach the cache what is popular. A request for a
			// resident model refreshes it; one for an absent model does not
			// pull it in, because the model is already serving and needs no
			// warm copy.
			warm.touch(ev.Model)
			continue
		}

		releases = free(releases, ev.At)
		per := result.PerModel[ev.Model]
		result.Spikes++
		per.Spikes++

		switch {
		case !warm.has(ev.Model):
			result.Misses++
			per.Misses++
			totalWait += cfg.ColdStart
			if cfg.FillOnMiss {
				warm.touch(ev.Model)
			}
		case len(releases) >= cfg.Slots:
			result.Blocked++
			per.Blocked++
			totalWait += cfg.ColdStart
			warm.touch(ev.Model)
		default:
			result.Hits++
			per.Hits++
			saved := cfg.ColdStart - cfg.Wake
			result.Saved += saved
			per.Saved += saved
			totalWait += cfg.Wake
			releases = append(releases, ev.At.Add(cfg.Hold))
			warm.touch(ev.Model)
		}
		result.PerModel[ev.Model] = per
	}

	if result.Spikes > 0 {
		result.MeanWait = totalWait / time.Duration(result.Spikes)
	}
	return result, nil
}

// free drops slots whose hold has expired by now.
func free(releases []time.Time, now time.Time) []time.Time {
	kept := releases[:0]
	for _, r := range releases {
		if r.After(now) {
			kept = append(kept, r)
		}
	}
	return kept
}
