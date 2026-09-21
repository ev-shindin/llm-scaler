// Package floor is the throughput floor: the demand the offered load implies
// per role once each role's saturated completion rate is known. What it
// prices, why, and what the alternatives cost, measured:
//
// Occupancy -- resident KV plus the waiting queues -- is a state of the
// fleet, not a property of the load, and it falls as replicas are added. Any
// floor built from a per-request cost measured on the current fleet has the
// same defect: the service time the engines report is ITL x output length,
// and ITL grows with the batch a replica is running. Measured on one decode
// pod across a run at a constant 6 req/s and a constant 6000/1000 shape
// (biran-20260915-102548-571):
//
//	replicas sharing the load   batch   ITL      W (service time)
//	6                           ~1      2.0 ms   2.0 s
//	1                           136-178 18-50 ms 16-52 s
//
// A 25x range in W at the same load, so lambda x W x tokens with the W of the
// moment is current occupancy restated. A floor built that way (the
// arrival-rate floor this package replaced) read 88k tokens at six replicas and
// authorised the scale-down to one; and on the way UP it did the opposite
// harm, pricing the load at the inflated W of a fleet that was behind,
// ordering replicas before any had reached saturation, and releasing them
// once they had brought W back down. Measured as a 2-4 replica oscillation
// with a ten-minute period on the second phase of the shape-swap benchmark
// (docs/proposals/backlog-sizing.md).
//
// What a replica can do does not move with the fleet: its completion rate
// when saturated. With the queue over the threshold the engine is completing
// requests as fast as it can for this shape, and that rate -- recorded beside
// k2 at the same P1 moment, under the same history key -- is a per-replica
// THROUGHPUT, mu. The fleet then needs lambda / mu replicas to keep up, which
// in the saturation analyzer's (D, P) contract is a demand of (lambda / mu) x P tokens.
// On the run above: mu read 5.4 req/s at the 6000/1000 shape and 2.5 req/s at
// 1000/4000, against 6 req/s arriving -- 2 and 3 decode replicas, which is
// where the fleet in fact held when it was left alone at those sizes.
//
// Strictly a floor: it never lowers demand. It is not capped at the fleet's
// own size. The first version was, so that it could hold a fleet but never
// order one, and scale-up stayed with occupancy and the queues. Measured, that
// left the order too late: a lone replica at 6 req/s against a mu of 5.4-6.4
// is at 94% of what it can do from the first cycle, and it tips into
// preemption at 75-145 s; occupancy crossed k1 at +69-78 s on every run,
// which with a 60-100 s start lands the second replica after the tip on most
// of them. The floor's lambda / mu = 0.94 replicas, through the engine's
// scale-up headroom, orders it in the first cycle lambda is measured -- some
// 50 s earlier -- and the bound on what it can order is
// (lambda + backlog / drain) / mu by construction, a figure that does not
// move as replicas are added. What a mu that under-read costs is bounded by
// the under-read (the window keeps a max, so it corrects upward at the next
// saturation and never drifts down); what a late order cost was five to
// seven replicas at the first ramp of every run.
//
// Two readings are not trusted with an order, only with a hold. A mu BORROWED from a
// neighbouring bucket (the saturation analyzer's
// nearestSaturatedThroughput) is wrong in a known
// direction and, from a longer shape, over-orders: the old cap at scaleUp x
// anticipated supply stays, a hold and no more. A window with a SINGLE
// reading is the first window's under-read -- an order on it
// over-provisions, and the over-provisioned fleet never saturates again to
// record the second reading that would have corrected it
// (MinThroughputSamplesToOrder) -- the cap stays, a hold and no more. The
// second reading is a rate window away (ThroughputSampleSpacing), and a
// fleet held at its size for that minute, its queues priced nowhere, lands
// the cold ramp's next replica 15-45 s after the old dead guard would have
// (occupancy orders in the meantime, or the second reading does); letting
// the one reading order one replica was tried and dropped, see
// Estimate.
//
// The same model prices a BACKLOG. Occupancy charged every queued request at
// its full KV footprint, as if all of them had to be resident at once, and
// the engine sized the fleet to hold them: 350 queued requests became five to
// seven extra replicas, each arriving after the queue was gone. A backlog
// needs throughput, not simultaneous residency: B requests to be cleared
// within T seconds on top of lambda arriving is (lambda + B / T) / mu
// replicas. For a role whose mu is known, the residency charge on its queues
// (the engines' own and the scheduler's) is taken back out and the queued
// requests enter the floor as B / T instead; the resident KV term stays as
// measured. A role with no mu keeps the residency charge, which is at least
// an opinion -- except prefill's share of the scheduler queue, which is
// dropped: a prefill replica holds a prompt's KV for its prefill time plus
// the hand-off to decode, and the only thing that makes it hold more is
// decode being saturated, which more prefill replicas do not fix. Measured:
// every prefill replica beyond the first at the first ramp was ordered by
// that term, and none of them prefilled anything a single one could not.
package floor

import (
	"slices"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
)

const (
	// BacklogDrainSeconds is how long a queued request may wait for capacity
	// that is not yet running: the throughput model prices a backlog of B
	// requests as B / BacklogDrainSeconds extra arrivals per second, so the
	// fleet it asks for clears the backlog in about this long while keeping up
	// with the load. It should not be shorter than a replica's start time --
	// capacity ordered to drain a backlog faster than it can start drains
	// nothing -- and a replica on the benchmark clusters takes 60-100 s. Sixty
	// keeps the order within one start.
	BacklogDrainSeconds = 60.0

	// MinThroughputSamplesToOrder is how many saturated readings a role's own
	// output-length bucket must hold before the throughput floor may ORDER a
	// replica from it; with fewer it holds the fleet and no more. The first
	// reading at a saturation under-reads (a 1m rate on a replica that has
	// been full for 20 s counts a third of a minute's completions), and an
	// order on an under-read over-provisions in a way that removes the
	// saturation which would have corrected it. Measured on the shape-swap
	// trace: 3.67, then 5.23, then 7.13 req/s on three consecutive saturated
	// scrape pairs, 60-90 s apart. Two readings from two rate windows -- not
	// the same window read twice (the saturation analyzer's
	// recordSaturatedThroughput and ThroughputSampleSpacing).
	MinThroughputSamplesToOrder = 2
)

// Floor is the demand the offered load implies per role once each
// role's saturated throughput is known, and the terms it was built from.
type Floor struct {
	// ByRole is the floor in tokens per role; a role with no saturated
	// throughput on record is absent.
	ByRole map[string]float64
	// Terms carries, per role in ByRole, the numbers a bound floor changes a
	// decision with.
	Terms map[string]Term
	// Lambda is the arrival rate the floors were built from.
	Lambda float64
	// DrainSeconds is the backlog drain target the floors were built with.
	DrainSeconds float64
}

// Term is one role's floor arithmetic, kept for the log line.
type Term struct {
	// Mu is the saturated completion rate of one replica, requests/s.
	Mu float64
	// PerReplica is the tokens one replica of the role is worth (P).
	PerReplica float64
	// Backlog is the queued requests priced into the floor: the role's own
	// engine queues plus the scheduler's.
	Backlog float64
	// Replicas is (lambda + Backlog / DrainSeconds) / Mu.
	Replicas float64
	// Held reports that the floor was capped at the fleet's anticipated size
	// because its mu is not one the floor may order on -- HeldWhy says
	// which: "borrowed" (a neighbouring bucket's reading) or "single-sample".
	Held    bool
	HeldWhy string
}

// Estimate computes the per-role floor from lambda, the
// backlog per role and the saturated throughput each role's own replicas
// carry.
//
// Per role, the floor is (lambda + backlog / drainSeconds) x P / mu, with
// P / mu taken as the median over the role's own (non-bridge) replicas that
// have a throughput on record, each priced at its variant's P. A role none of
// whose replicas has one gets no floor: on a P/D fleet that is prefill in
// practice, whose queue is rarely the one that saturates, and a role that has
// never been seen saturated has no business being sized by this file.
//
// The floor is not capped at the fleet's size (see the file header for what
// the cap cost) -- with one exception. A role whose readings are all borrowed
// from a neighbouring bucket, or whose own window holds fewer than
// MinThroughputSamplesToOrder readings, may hold the fleet but not grow it:
// its floor is capped at scaleUp x the role's anticipated supply, the largest
// demand the engine's RC = D / scaleUp - anticipated turns into nothing. The
// term says so (Held, HeldWhy). scaleUp <= 0 disables the cap.
//
// A borrowed reading never outvotes a replica's own. The median is taken over
// the role's replicas that read their OWN bucket when any does, and over the
// borrowed ones only when none does -- the rule the saturation analyzer's
// nearestSaturatedThroughput states per key ("used only until the bucket has a reading of its own"),
// applied to the role. Without it a shape switch flapped the fleet: a fresh
// replica's first completions are the short requests (they finish first, and
// a replica with none yet reads an output length of 0), so its key lands in
// a short bucket that has no reading and borrows the previous shape's mu --
// 4.38 req/s from 1000-token outputs -- while the replica that had been
// saturated under the new 6000-token shape read 1.4-1.7 of its own. Two
// fresh replicas out of three put the borrowed 4.38 at the median: the
// backlog of 441 requests read as 2 replicas' worth instead of 5, the floor
// fell from 10 M tokens to 3 M in one cycle, the target from 10 to 4, and
// the backlog kept growing (256 -> 642) under the figure that said it would
// not. Measured on the 1000/6000 shape-swap trace, 2026-09-20, cycles
// 11:45:22-11:47:22; the target then swung 10 <-> 4 for ten minutes.
func Estimate(
	lambda float64,
	replicas []ReplicaCapacity,
	variants []domain.VariantCapacity,
	backlog map[string]float64,
	drainSeconds float64,
	scaleUp float64,
) Floor {
	out := Floor{Lambda: lambda, DrainSeconds: drainSeconds}
	if lambda <= 0 || len(replicas) == 0 || len(variants) == 0 {
		return out
	}

	perReplica := make(map[string]float64, len(variants))
	roleOf := make(map[string]string, len(variants))
	for _, vc := range variants {
		perReplica[vc.VariantName] = vc.PerReplicaCapacity
		roleOf[vc.VariantName] = canonicalRole(vc.Role)
	}
	// The per-role anticipated supply the hold cap is measured against, from
	// the one place that defines it: the engine reads the same figure through
	// the same helper, so the cap and the RC it exists to zero cannot drift.
	anticipated := aggregation.AggregateByRole(variants)

	// tokens per unit of arrival rate, per role: P / mu for each replica that
	// can price it -- and whether any of them may order (own window, enough
	// readings).
	costs := make(map[string][]float64)
	mus := make(map[string][]float64)
	// The same, from the replicas whose reading is a neighbouring bucket's;
	// taken only for a role none of whose replicas reads its own.
	borrowedCosts := make(map[string][]float64)
	borrowedMus := make(map[string][]float64)
	mayOrder := make(map[string]bool)
	borrowedOnly := make(map[string]bool)
	for _, rc := range replicas {
		if rc.FromWarmPool || rc.SaturatedThroughput <= 0 {
			continue
		}
		p := perReplica[rc.VariantName]
		if p <= 0 {
			continue
		}
		role := roleOf[rc.VariantName]
		if _, seen := borrowedOnly[role]; !seen {
			borrowedOnly[role] = true
		}
		if rc.SaturatedThroughputBorrowed {
			borrowedCosts[role] = append(borrowedCosts[role], p/rc.SaturatedThroughput)
			borrowedMus[role] = append(borrowedMus[role], rc.SaturatedThroughput)
			continue
		}
		costs[role] = append(costs[role], p/rc.SaturatedThroughput)
		mus[role] = append(mus[role], rc.SaturatedThroughput)
		borrowedOnly[role] = false
		if rc.SaturatedThroughputSamples >= MinThroughputSamplesToOrder {
			mayOrder[role] = true
		}
	}
	for role, c := range borrowedCosts {
		if len(costs[role]) == 0 {
			costs[role] = c
			mus[role] = borrowedMus[role]
		}
	}
	if len(costs) == 0 {
		return out
	}

	out.ByRole = make(map[string]float64, len(costs))
	out.Terms = make(map[string]Term, len(costs))
	for role, c := range costs {
		cost := Median(c)
		mu := Median(mus[role])
		rate := lambda
		var b float64
		if drainSeconds > 0 {
			b = max(backlog[role], 0)
			rate += b / drainSeconds
		}
		floor := rate * cost
		term := Term{Mu: mu, PerReplica: cost * mu, Backlog: b, Replicas: rate / mu}
		if !mayOrder[role] && scaleUp > 0 {
			// A hold, not an order, on either. Letting a single reading order
			// one replica was tried, twice: measured against the anticipated
			// supply it ordered a fourth replica at a phase switch whose
			// third was still starting; measured against the running supply
			// it was a ratchet -- nothing remembered that the reading had
			// already ordered, so once the ordered replica reported, the same
			// reading ordered the next, up to the full figure one start at a
			// time. Across four passes the one replica bought a single cycle
			// over the plain hold (a third replica 30 s ahead of occupancy,
			// on the under-read that then ran unneeded for 35 minutes), and
			// the hold's own cost, replayed, is 15-45 s on the cold ramp's
			// next replica: the second counted reading lands a window after
			// the first, and occupancy orders in the meantime.
			if hold := scaleUp * anticipated[role].TotalAnticipatedSupply; floor > hold {
				floor = hold
				term.Held = true
				term.HeldWhy = "single-sample"
				if borrowedOnly[role] {
					term.HeldWhy = "borrowed"
				}
			}
		}
		out.ByRole[role] = floor
		out.Terms[role] = term
	}
	return out
}

// Median is the median of values, averaging the central pair on an even
// count: every value here is a learned per-replica figure, none is suspect,
// and the midpoint is the better estimate -- the same convention as the saturation
// analyzer's median() for capacities.
func Median(values []float64) float64 {
	n := len(values)
	if n == 0 {
		return 0
	}
	sorted := make([]float64, n)
	copy(sorted, values)
	slices.Sort(sorted)
	if n%2 == 0 {
		return (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return sorted[n/2]
}

// canonicalRole reads an empty role as domain.RoleBoth, as aggregation does.
func canonicalRole(role string) string {
	if role == "" {
		return domain.RoleBoth
	}
	return role
}
