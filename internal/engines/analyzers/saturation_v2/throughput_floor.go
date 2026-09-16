package saturation_v2

import (
	"sort"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// The arrival-rate floor in arrival_demand.go sizes the fleet by Little's law
// from the service time the engines currently report. That service time is not
// a property of the load: it is ITL x output length, and ITL grows with the
// batch a replica is running. Measured on one decode pod across a run at a
// constant 6 req/s and a constant 6000/1000 shape (biran-20260915-102548-571):
//
//	replicas sharing the load   batch   ITL      W (service time)
//	6                           ~1      2.0 ms   2.0 s
//	1                           136-178 18-50 ms 16-52 s
//
// A 25x range in W at the same load, so lambda x W x tokens with the W of the
// moment is current occupancy restated: at six replicas it read 88k tokens
// (one tenth of a replica) and authorised the scale-down to one, where the
// same load then saturated that one replica within a minute and the cycle
// repeated. That floor is bounded below by the uncontended W, which is real,
// but the uncontended W is what a fleet that is far too large measures, so
// the bound holds nothing up.
//
// What a replica can do does not move with the fleet: its completion rate
// when saturated. With the queue over the threshold the engine is completing
// requests as fast as it can for this shape, and that rate -- recorded beside
// k2 at the same P1 moment, under the same history key -- is a per-replica
// THROUGHPUT, mu. The fleet then needs lambda / mu replicas to keep up, which
// in this analyzer's (D, P) contract is a demand of (lambda / mu) x P tokens.
// On the run above: mu read 5.4 req/s at the 6000/1000 shape and 2.5 req/s at
// 1000/4000, against 6 req/s arriving -- 2 and 3 decode replicas, which is
// where the fleet in fact held when it was left alone at those sizes.
//
// Strictly a floor, and a HOLD rather than an order: it never lowers demand,
// and it is capped so that through the engine's scale-up headroom it can never
// ask for a replica the fleet does not have. Scale-up stays with occupancy and
// the queue, which are the signals that read well while a fleet is behind.

// throughputFloor is the demand the offered load implies per role once each
// role's saturated throughput is known, and the terms it was built from.
type throughputFloor struct {
	// ByRole is the floor in tokens per role; a role with no saturated
	// throughput on record is absent.
	ByRole map[string]float64
	// Terms carries, per role in ByRole, the numbers a bound floor changes a
	// decision with.
	Terms map[string]throughputTerm
	// Lambda is the arrival rate the floors were built from.
	Lambda float64
}

// throughputTerm is one role's floor arithmetic, kept for the log line.
type throughputTerm struct {
	// Mu is the saturated completion rate of one replica, requests/s.
	Mu float64
	// PerReplica is the tokens one replica of the role is worth (P).
	PerReplica float64
	// Replicas is lambda / Mu before the cap, and Capped reports whether the
	// hold cap bound instead.
	Replicas float64
	Capped   bool
}

// recordSaturatedThroughput folds one saturated completion-rate reading into
// the window for key. A non-positive rate is not a reading.
//
// The window keeps a MAX, not a mean, and the asymmetry is the point. A
// replica's completion rate while saturated can only under-read its capacity:
// in the first minute after it fills, the requests completing are the few that
// were admitted first (a 1m rate on a replica that has been full for 20s
// counts a third of a minute's completions); under KV pressure preemption and
// recompute drop it further. It cannot over-read -- nothing completes faster
// than the engine runs. A mean of under-reads is an under-read, and the floor
// divides by it, so an under-read mu orders replicas that are not needed. The
// max of the window is the best estimate of what the replica actually
// sustains, and an outlier high reading errs the way this file already
// accepts: a floor that is too LOW holds back, where occupancy still carries
// a real shortfall.
//
// Same window size and staleness rule as k2 history, and pruned beside it in
// EvictStaleHistory.
func (a *SaturationAnalyzer) recordSaturatedThroughput(key string, rate float64) {
	if !(rate > 0) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	ra, ok := a.saturatedThroughput[key]
	if !ok || ra.Stale(HistoryEvictionTimeout) {
		ra = newRollingAverage(RollingAverageWindowSize)
		a.saturatedThroughput[key] = ra
	}
	ra.Add(rate)
}

// saturatedThroughputFor returns the saturated completion rate on record for
// key, or 0 when saturation has never been observed for the bucket.
func (a *SaturationAnalyzer) saturatedThroughputFor(key string) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	ra, ok := a.saturatedThroughput[key]
	if !ok {
		return 0
	}
	return ra.Max()
}

// estimateThroughputDemand computes the per-role floor from lambda and the
// saturated throughput each role's own replicas carry.
//
// Per role, the floor is lambda x P / mu with P / mu taken as the median over
// the role's own (non-bridge) replicas that have a throughput on record, each
// priced at its variant's P. A role none of whose replicas has one gets no
// floor: on a P/D fleet that is prefill in practice, whose queue is rarely the
// one that saturates, and a role that has never been seen saturated has no
// business being held up by this file.
//
// The cap: the floor may not exceed scaleUp x the role's anticipated supply.
// The engine orders capacity as demand / scaleUp - anticipated supply, so that
// is the largest demand which orders nothing; above it the floor would stop
// holding the fleet and start growing it, and with a mu that under-read (see
// recordSaturatedThroughput) it would keep growing it every cycle. Scale-up is
// occupancy's job. The cap makes the worst a bad mu can do "no scale-down this
// cycle".
func estimateThroughputDemand(
	lambda float64,
	replicas []ReplicaCapacity,
	variants []domain.VariantCapacity,
	scaleUp float64,
) throughputFloor {
	out := throughputFloor{Lambda: lambda}
	if lambda <= 0 || len(replicas) == 0 || len(variants) == 0 {
		return out
	}

	perReplica := make(map[string]float64, len(variants))
	roleOf := make(map[string]string, len(variants))
	anticipated := make(map[string]float64)
	for _, vc := range variants {
		perReplica[vc.VariantName] = vc.PerReplicaCapacity
		role := canonicalRole(vc.Role)
		roleOf[vc.VariantName] = role
		anticipated[role] += float64(vc.ReplicaCount+vc.PendingReplicas) * vc.PerReplicaCapacity
	}

	// tokens per unit of arrival rate, per role: P / mu for each replica that
	// can price it.
	costs := make(map[string][]float64)
	mus := make(map[string][]float64)
	for _, rc := range replicas {
		if rc.FromWarmPool || rc.SaturatedThroughput <= 0 {
			continue
		}
		p := perReplica[rc.VariantName]
		if p <= 0 {
			continue
		}
		role := roleOf[rc.VariantName]
		costs[role] = append(costs[role], p/rc.SaturatedThroughput)
		mus[role] = append(mus[role], rc.SaturatedThroughput)
	}
	if len(costs) == 0 {
		return out
	}

	out.ByRole = make(map[string]float64, len(costs))
	out.Terms = make(map[string]throughputTerm, len(costs))
	for role, c := range costs {
		cost := medianFloat(c)
		mu := medianFloat(mus[role])
		floor := lambda * cost
		term := throughputTerm{Mu: mu, PerReplica: cost * mu, Replicas: lambda / mu}
		if scaleUp > 0 {
			if hold := scaleUp * anticipated[role]; floor > hold {
				floor = hold
				term.Capped = true
			}
		}
		out.ByRole[role] = floor
		out.Terms[role] = term
	}
	return out
}

// applyThroughputFloor raises roleDemand (in place) and returns the raised
// totalDemand wherever the throughput floor exceeds what occupancy measured.
//
// On a disaggregated fleet each role is floored on its own: the scheduler's
// arrival rate is every request, and every request passes through both
// roles, so each must keep up with all of it. The model-level total is raised
// by the same amount so RoleDemand and TotalDemand keep moving together --
// which is what raiseRoleDemandTo preserves from the other direction. On a
// non-disaggregated fleet there is no RoleDemand and the single "both" floor
// lands on the total directly.
//
// Logged at INFO when it binds, with the terms: a floor that changes a
// decision is worth a line, and which of lambda, mu and P moved is the first
// question anyone will ask of it. A role with no throughput on record is
// silent -- that is the normal state of a fleet that has never been
// saturated, not a gap.
func (a *SaturationAnalyzer) applyThroughputFloor(
	input domain.AnalyzerInput,
	cfg *config.ScalingPolicy,
	replicas []ReplicaCapacity,
	variants []domain.VariantCapacity,
	totalDemand float64,
	roleDemand map[string]float64,
	logger logr.Logger,
) float64 {
	floor := estimateThroughputDemand(offeredArrivalRate(input), replicas, variants, cfg.ScaleUpThreshold)
	if len(floor.ByRole) == 0 {
		return totalDemand
	}
	// Roles in a stable order, so a two-role fleet logs the same way each cycle.
	roles := make([]string, 0, len(floor.ByRole))
	for role := range floor.ByRole {
		roles = append(roles, role)
	}
	sort.Strings(roles)

	for _, role := range roles {
		tokens := floor.ByRole[role]
		term := floor.Terms[role]
		var measured float64
		if roleDemand != nil {
			if _, ok := roleDemand[role]; !ok {
				// A role the analyzer attributed no demand to is one no variant
				// serves this cycle (aggregateRoleDemand); nothing to hold.
				continue
			}
			measured = roleDemand[role]
		} else {
			measured = totalDemand
		}
		if tokens <= measured {
			continue
		}
		logger.Info("throughput-demand-floor",
			"modelID", input.ModelID, "namespace", input.Namespace, "role", role,
			"occupancyDemand", measured, "flooredTo", tokens,
			"arrivalRate", floor.Lambda, "saturatedThroughput", term.Mu,
			"perReplicaCapacity", term.PerReplica, "replicasImplied", term.Replicas,
			"heldAtFleet", term.Capped)
		if roleDemand != nil {
			roleDemand[role] = tokens
		}
		totalDemand += tokens - measured
	}
	return totalDemand
}

// offeredArrivalRate is the model-level arrival rate the two floors share:
// the scheduler's, or the completion rate of the replicas that generate
// output when the scheduler reports none. See estimateArrivalDemand for what
// the fallback is and is not good for.
func offeredArrivalRate(input domain.AnalyzerInput) float64 {
	if input.ArrivalRate > 0 {
		return input.ArrivalRate
	}
	var lambda float64
	for _, rm := range generatingReplicas(input) {
		lambda += rm.RequestRate
	}
	return lambda
}

// medianFloat is the median of values, averaging the central pair on an even
// count: every value here is a learned per-replica figure, none is suspect,
// and the midpoint is the better estimate -- the same convention as median()
// for capacities, not medianOf's outlier-defending lower median.
func medianFloat(values []float64) float64 {
	n := len(values)
	if n == 0 {
		return 0
	}
	sorted := make([]float64, n)
	copy(sorted, values)
	sort.Float64s(sorted)
	if n%2 == 0 {
		return (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return sorted[n/2]
}
