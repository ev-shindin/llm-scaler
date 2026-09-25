package floor

import (
	"slices"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

const (
	// BacklogDrainSeconds is how long a queued request may wait for capacity
	// that is not yet running: the throughput model prices a backlog of B
	// requests as B / BacklogDrainSeconds extra arrivals per second, so the
	// fleet it asks for clears the backlog in about this long while keeping up
	// with the load.
	//
	// It must not be shorter than a replica's start, or it orders capacity
	// that drains nothing. Sixty keeps the order within one start on the
	// benchmark stands; see "A backlog is throughput, not residency" in
	// docs/developer-guide/analyzer-evidence.md.
	BacklogDrainSeconds = 60.0

	// MinThroughputSamplesToOrder is how many saturated readings a role's own
	// output-length bucket must hold before the throughput floor may ORDER a
	// replica from it; with fewer it holds the fleet and no more.
	//
	// The first reading at a saturation under-reads -- a 1m rate on a replica
	// that has been full for 20 s counts a third of a minute's completions --
	// and an order on it over-provisions in a way that removes the saturation
	// which would have corrected it. The two readings must come from two rate
	// windows, not the same window read twice (the saturation analyzer's
	// recordSaturatedThroughput and ThroughputSampleSpacing). See "Two
	// readings, not one, before a reading may order" in
	// docs/developer-guide/analyzer-evidence.md.
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
	// OrderedBehindQueue reports that a window too thin to order on was
	// allowed to anyway, because a backlog stood and no replica was pending.
	// Read it beside Held: the two are exclusive.
	OrderedBehindQueue bool
}

// Estimate computes the per-role floor from lambda, the
// backlog per role and the saturated throughput each role's own replicas
// carry. Units: lambda is the model's arrival rate in requests/s; backlog
// is queued requests, keyed by role as canonicalRole spells it (an empty
// variant role is domain.RoleBoth, so a backlog keyed "" prices nothing);
// drainSeconds is the seconds the backlog may take to clear
// (BacklogDrainSeconds), and a drainSeconds <= 0 leaves the backlog
// unpriced; scaleUpThreshold is the (0, 1] scale-up threshold the engine
// sizes with (RC = D / scaleUpThreshold - anticipated), which bounds a hold
// (below). The result is in tokens per role, each role priced at its
// variants' P.
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
// its floor is capped at scaleUpThreshold x the role's anticipated supply,
// the largest demand the engine's RC = D / scaleUpThreshold - anticipated
// turns into nothing. The term says so (Held, HeldWhy). scaleUpThreshold <= 0
// disables the cap.
//
// A borrowed reading never outvotes a replica's own. The median is taken over
// the role's replicas that read their OWN bucket when any does, and over the
// borrowed ones only when none does -- the rule the saturation analyzer's
// nearestSaturatedThroughput states per key ("used only until the bucket has
// a reading of its own"), applied to the role. Without it a shape switch
// flapped the fleet between two targets for ten minutes, because a fresh
// replica reads a short bucket and borrows the previous shape's mu; see "A
// borrowed reading never outvotes a replica's own" in
// docs/developer-guide/analyzer-evidence.md.
func Estimate(
	lambda float64,
	replicas []capacity.ReplicaCapacity,
	variants []domain.VariantCapacity,
	backlog map[string]float64,
	drainSeconds float64,
	scaleUpThreshold float64,
	// staleShape forces every role onto the hold path: the caller has seen
	// the fleet's shape change, so no window on record was taken under the
	// shape now arriving, however many readings it holds.
	staleShape bool,
	// schedulerQueued is the requests the SCHEDULER is holding, undispatched.
	// Separate from backlog, which merges it with the engines' own queues: a
	// request in an engine's queue may be waiting on a remote KV transfer,
	// which no further replica drains, while one in the scheduler's has not
	// been given to a pod at all. Only the latter releases the hold below.
	schedulerQueued float64,
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
		if rc.SaturatedThroughputSamples >= MinThroughputSamplesToOrder && !staleShape {
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
		cost := median(c)
		mu := median(mus[role])
		rate := lambda
		var b float64
		if drainSeconds > 0 {
			b = max(backlog[role], 0)
			rate += b / drainSeconds
		}
		floor := rate * cost
		term := Term{Mu: mu, PerReplica: cost * mu, Backlog: b, Replicas: rate / mu}
		// A single reading may order while the SCHEDULER holds a real queue.
		//
		// Nothing here withholds the figure while a replica is starting, and
		// an earlier version that did was measured inert: through every cycle
		// of a ramp the deployment has more replicas than are Ready, which is
		// what a ramp is, so the fleet stayed at two while the queue tripled.
		// The engine already does that job properly one layer up, where
		// RC = max(0, TotalDemand/scaleUp - TotalAnticipatedSupply) subtracts
		// the supply on its way and so cannot re-order it.
		//
		// What makes the climb safe is that the figure does not move:
		// (lambda + backlog/drain) / mu is fixed by the load and the queue,
		// not by the fleet, so repeated firings converge on it rather than
		// ratchet past it, and the rule stops firing when the queue drains.
		// See "A single reading may order behind a standing queue" in
		// ../../../docs/developer-guide/analyzer-evidence.md.
		//
		// A THIN window only. mayOrder is false for three different reasons
		// and this releases one of them: staleShape means every reading on
		// record was taken under a shape the fleet has left, and borrowedOnly
		// means the reading belongs to a neighbouring output-length bucket and
		// is wrong in a known direction. Ordering on either is the shape-swap
		// flap the two sections above this one in the guide describe; a queue
		// does not make a reading for the wrong shape right.
		// The queue must be worth more than a second of arrivals AND more
		// than one replica-second of service. Against lambda alone the test
		// degenerates as lambda falls: at 0.1 req/s a single stray request is
		// ten seconds of arrivals and would release the hold, which is jitter,
		// not a standing queue. mu is the other natural scale and needs no
		// constant.
		thinOwnWindow := !staleShape && !borrowedOnly[role]
		if !mayOrder[role] && thinOwnWindow && schedulerQueued > max(lambda, mu) {
			mayOrder[role] = true
			term.OrderedBehindQueue = true
		}
		if !mayOrder[role] && scaleUpThreshold > 0 {
			// A hold, not an order, on either. Letting a single reading
			// order one replica was tried twice and dropped: against the
			// anticipated supply it ordered a replica at a phase switch
			// whose predecessor was still starting, and against the running
			// supply it was a ratchet, because nothing remembered that the
			// reading had already ordered -- once the ordered replica
			// reported, the same reading ordered the next, up to the full
			// figure one start at a time. The hold's own cost is bounded:
			// the second counted reading lands a window after the first, and
			// occupancy orders in the meantime. See "Two readings, not one,
			// before a reading may order" in
			// docs/developer-guide/analyzer-evidence.md.
			//
			// The anticipated supply is floored at zero before it becomes a
			// cap. It is (ReplicaCount + PendingReplicas) x P summed over the
			// role, and a negative product would make the cap negative -- at
			// which point every real floor is above it and this branch
			// publishes the negative, inverting a package whose whole
			// contract is that it only ever raises demand. Today every
			// producer clamps PendingReplicas (variantmeta/discovery.go does
			// it explicitly, and logs), so the guard costs nothing; it is
			// here because this package cannot see that clamp, and the cost
			// path above already skips a variant whose P is <= 0 while this
			// one reads P again over every variant of the role.
			hold := scaleUpThreshold * max(anticipated[role].TotalAnticipatedSupply, 0)
			if floor > hold {
				floor = hold
				term.Held = true
				term.HeldWhy = "single-sample"
				if borrowedOnly[role] {
					term.HeldWhy = "borrowed"
				}
				if staleShape {
					term.HeldWhy = "shape-change"
				}
			}
		}
		out.ByRole[role] = floor
		out.Terms[role] = term
	}
	return out
}

// median is the median of values, averaging the central pair on an even
// count: every value here is a learned per-replica figure, none is suspect,
// and the midpoint is the better estimate -- the same convention as the saturation
// analyzer's median() for capacities.
func median(values []float64) float64 {
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
