package saturation_v2

import (
	"maps"
	"slices"
	"strings"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

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
// arrival-rate floor this file replaced) read 88k tokens at six replicas and
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
// in this analyzer's (D, P) contract is a demand of (lambda / mu) x P tokens.
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
// Two readings are not trusted with an order, only with a hold, and for
// those the old cap at scaleUp x anticipated supply stays: a mu BORROWED from
// a neighbouring bucket (nearestSaturatedThroughput), which is wrong in a
// known direction and, from a longer shape, over-orders; and a window with a
// SINGLE reading, which is the first cycle's under-read -- an order on it
// over-provisions, and the over-provisioned fleet never saturates again to
// record the second reading that would have corrected it
// (MinThroughputSamplesToOrder).
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
	// DrainSeconds is the backlog drain target the floors were built with.
	DrainSeconds float64
}

// throughputTerm is one role's floor arithmetic, kept for the log line.
type throughputTerm struct {
	// Mu is the saturated completion rate of one replica, requests/s.
	Mu float64
	// PerReplica is the tokens one replica of the role is worth (P).
	PerReplica float64
	// Backlog is the queued requests priced into the floor: the role's own
	// engine queues plus the scheduler's.
	Backlog float64
	// Replicas is (lambda + Backlog / DrainSeconds) / Mu.
	Replicas float64
	// Held reports that the floor was capped at the fleet's own size because
	// its mu is not one the floor may order on -- HeldWhy says which:
	// "borrowed" (a neighbouring bucket's reading) or "single-sample".
	Held    bool
	HeldWhy string
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
// key, and the bucket it came from. When the key's own bucket has no reading
// it borrows the nearest output-length bucket's under the same key prefix
// (see nearestSaturatedThroughput); the returned bucket names which, so the
// log can say the figure is borrowed. Returns 0 and "" when no bucket has one.
func (a *SaturationAnalyzer) saturatedThroughputFor(key string) (float64, string) {
	r := a.saturatedThroughputReading(key)
	return r.rate, r.bucket
}

// throughputReading is what the floor knows about a key's saturated
// throughput: the figure, the bucket it came from, how many readings that
// bucket's window holds, and whether the bucket is a neighbour's.
type throughputReading struct {
	rate     float64
	bucket   string
	samples  int
	borrowed bool
}

// saturatedThroughputReading is saturatedThroughputFor with the window's
// size and provenance, which decide whether the floor may order on it.
func (a *SaturationAnalyzer) saturatedThroughputReading(key string) throughputReading {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ra, ok := a.saturatedThroughput[key]; ok {
		return throughputReading{rate: ra.Max(), bucket: bucketOf(key), samples: ra.Len()}
	}
	rate, bucket, samples := a.nearestSaturatedThroughput(key)
	return throughputReading{rate: rate, bucket: bucket, samples: samples, borrowed: rate > 0}
}

// nearestSaturatedThroughput finds the reading in the output-length bucket
// closest to key's, on the same model, accelerator, GPU count, role and queue
// threshold. Caller holds a.mu.
//
// A shape the fleet has not yet been seen saturated under has no throughput
// of its own, and without this the floor would simply vanish on the first
// cycle of a new shape -- which is the moment a floor is for. Measured on the
// shape-swap benchmark: the switch from 1000 to 4000 output tokens landed in
// an empty bucket, occupancy read 400k at three replicas, and the target went
// from 3 towards 1 in one cycle, where the shared bucket it replaced would
// at least have held two.
//
// The neighbour's figure is wrong in a known direction. Throughput falls with
// output length, so a shorter shape's mu is too high and holds too few
// replicas (the fleet then saturates and learns its own, exactly once); a
// longer shape's mu is too low and holds too many, which the cap bounds at
// the fleet's own size. Either is better than no floor. The borrowed figure
// is used only until the bucket has a reading of its own: a fresh window
// takes over the first cycle it exists, so it is never masked by the
// neighbour's max.
//
// Ties between an equally distant shorter and longer bucket go to the shorter
// one -- the under-hold, which occupancy corrects, rather than the over-hold,
// which only the cap does.
func (a *SaturationAnalyzer) nearestSaturatedThroughput(key string) (float64, string, int) {
	prefix, bucket, suffix, ok := splitHistoryKey(key)
	if !ok {
		return 0, "", 0
	}
	own := slices.Index(outputBuckets, bucket)
	if own < 0 {
		return 0, "", 0
	}
	for dist := 1; dist < len(outputBuckets); dist++ {
		for _, i := range []int{own - dist, own + dist} {
			if i < 0 || i >= len(outputBuckets) {
				continue
			}
			if ra, found := a.saturatedThroughput[prefix+outputBuckets[i]+suffix]; found {
				return ra.Max(), outputBuckets[i], ra.Len()
			}
		}
	}
	return 0, "", 0
}

// splitHistoryKey takes a key of the form built by historyKey --
// model|accelerator|gpus|role|bucket|qN -- apart around its bucket. The
// model ID may itself contain "|"-free "/" and other characters but never
// "|", so counting from the right is safe: the bucket is the second-to-last
// field.
func splitHistoryKey(key string) (prefix, bucket, suffix string, ok bool) {
	last := strings.LastIndexByte(key, '|')
	if last < 0 {
		return "", "", "", false
	}
	prev := strings.LastIndexByte(key[:last], '|')
	if prev < 0 {
		return "", "", "", false
	}
	return key[:prev+1], key[prev+1 : last], key[last:], true
}

// bucketOf returns the output-length bucket a history key was built with.
func bucketOf(key string) string {
	_, bucket, _, ok := splitHistoryKey(key)
	if !ok {
		return ""
	}
	return bucket
}

// estimateThroughputDemand computes the per-role floor from lambda, the
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
func estimateThroughputDemand(
	lambda float64,
	replicas []ReplicaCapacity,
	variants []domain.VariantCapacity,
	backlog map[string]float64,
	drainSeconds float64,
	scaleUp float64,
) throughputFloor {
	out := throughputFloor{Lambda: lambda, DrainSeconds: drainSeconds}
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
		costs[role] = append(costs[role], p/rc.SaturatedThroughput)
		mus[role] = append(mus[role], rc.SaturatedThroughput)
		if _, seen := borrowedOnly[role]; !seen {
			borrowedOnly[role] = true
		}
		if !rc.SaturatedThroughputBorrowed {
			borrowedOnly[role] = false
			if rc.SaturatedThroughputSamples >= MinThroughputSamplesToOrder {
				mayOrder[role] = true
			}
		}
	}
	if len(costs) == 0 {
		return out
	}

	out.ByRole = make(map[string]float64, len(costs))
	out.Terms = make(map[string]throughputTerm, len(costs))
	for role, c := range costs {
		cost := medianFloat(c)
		mu := medianFloat(mus[role])
		rate := lambda
		var b float64
		if drainSeconds > 0 {
			b = max(backlog[role], 0)
			rate += b / drainSeconds
		}
		floor := rate * cost
		term := throughputTerm{Mu: mu, PerReplica: cost * mu, Backlog: b, Replicas: rate / mu}
		if !mayOrder[role] && scaleUp > 0 {
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

// applyThroughputFloor raises roleDemand (in place) and returns the raised
// totalDemand wherever the throughput model exceeds what occupancy measured,
// after taking the queues' residency charge out of the measurement for every
// role the model can price.
//
// On a disaggregated fleet each role is floored on its own: the scheduler's
// arrival rate is every request, and every request passes through both
// roles, so each must keep up with all of it. The model-level total is raised
// by the same amount so RoleDemand and TotalDemand keep moving together. On a
// non-disaggregated fleet there is no RoleDemand and the single "both" floor
// lands on the total directly.
//
// eppByRole is the residency charge estimateSchedulerQueueDemand put on the
// scheduler queue per role, and eppQueued the requests in it. For a role with
// a mu, both that charge and the engines' own queue charge (LocalQueueDemand)
// come back out and the queued requests go into the floor as a backlog. For
// prefill without a mu, the scheduler-queue charge is dropped (file header),
// and that is logged at the per-replica verbosity when it changes the figure:
// it is the normal state of a P/D fleet, so an INFO line every cycle would be
// noise, but a reader comparing scheduler-queue-demand's byRole with
// RoleDemand needs to find the gap somewhere.
//
// Logged at INFO when it binds, with the terms: a floor that changes a
// decision is worth a line, and which of lambda, mu, P and the backlog moved
// is the first question anyone will ask of it. A role with no throughput on
// record is silent -- that is the normal state of a fleet that has never been
// saturated, not a gap.
func (a *SaturationAnalyzer) applyThroughputFloor(
	input domain.AnalyzerInput,
	cfg *config.ScalingPolicy,
	replicas []ReplicaCapacity,
	variants []domain.VariantCapacity,
	totalDemand float64,
	roleDemand map[string]float64,
	eppByRole map[string]float64,
	eppQueued float64,
	logger logr.Logger,
) float64 {
	roleOf := make(map[string]string, len(variants))
	for _, vc := range variants {
		roleOf[vc.VariantName] = canonicalRole(vc.Role)
	}
	// Per role: the requests waiting in the engines' own queues, and the
	// residency charge on them. Own replicas only -- a bridge's queue is
	// counted toward the variant's demand by aggregation, but it is not this
	// variant's backlog to size for.
	backlog := make(map[string]float64)
	residency := make(map[string]float64)
	for _, rc := range replicas {
		if rc.FromWarmPool {
			continue
		}
		role := roleOf[rc.VariantName]
		backlog[role] += float64(rc.QueueLength)
		residency[role] += float64(rc.LocalQueueDemand)
	}
	for role, tokens := range eppByRole {
		backlog[role] += eppQueued
		residency[role] += tokens
	}

	// The hold cap is measured against the threshold the ENGINE sizes RC
	// with, which is the saturation analyzer's own (config.AnalyzerThresholds):
	// a cap drawn at the policy-level figure while the engine divides by a
	// per-analyzer override would leave a gap that orders a replica.
	scaleUp, _ := cfg.AnalyzerThresholds(domain.SaturationAnalyzerName)
	floor := estimateThroughputDemand(offeredArrivalRate(input), replicas, variants, backlog, BacklogDrainSeconds, scaleUp)

	// Prefill with no mu: the scheduler queue's prompts are not resident work
	// for prefill (file header). Only the disaggregated case has a prefill
	// entry to correct, and only the role figure moves: the model-level total
	// carries the scheduler queue once, as input + output, which is decode's
	// share; prefill's input-only share was never in it
	// (estimateSchedulerQueueDemand).
	if roleDemand != nil {
		if _, priced := floor.ByRole[domain.RolePrefill]; !priced {
			if share, ok := eppByRole[domain.RolePrefill]; ok && share > 0 {
				if before, ok := roleDemand[domain.RolePrefill]; ok {
					roleDemand[domain.RolePrefill] = before - share
					logger.V(logging.DEFAULT).Info("scheduler-queue-prefill-share-dropped",
						"modelID", input.ModelID, "namespace", input.Namespace,
						"eppQueueSize", eppQueued, "droppedTokens", share,
						"prefillDemandBefore", before, "prefillDemandAfter", before-share)
				}
			}
		}
	}
	if len(floor.ByRole) == 0 {
		return totalDemand
	}
	// Roles in a stable order, so a two-role fleet logs the same way each cycle.
	roles := slices.Sorted(maps.Keys(floor.ByRole))

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
		// What occupancy measured with the queues' residency charge removed:
		// the resident KV alone. The queued requests are in the floor as a
		// backlog now, and must not be counted twice.
		resident := max(measured-residency[role], 0)
		want := max(resident, tokens)
		if want == measured {
			continue
		}
		logger.Info("throughput-demand-floor",
			"modelID", input.ModelID, "namespace", input.Namespace, "role", role,
			"demandBeforeFloor", measured, "residentDemand", resident, "flooredTo", want,
			"arrivalRate", floor.Lambda, "backlogRequests", term.Backlog, "drainSeconds", floor.DrainSeconds,
			"saturatedThroughput", term.Mu, "perReplicaCapacity", term.PerReplica,
			"replicasImplied", term.Replicas, "heldAtFleet", term.Held, "heldWhy", term.HeldWhy)
		if roleDemand != nil {
			roleDemand[role] = want
		}
		totalDemand += want - measured
	}
	return totalDemand
}

// offeredArrivalRate is the model-level arrival rate: the scheduler's, or the
// completion rate of the replicas that generate output when the scheduler
// reports none.
//
// The EPP is the only source of a model-level arrival rate. Without it, fall
// back to what the engines completed: at steady state a queue that is neither
// growing nor shrinking makes completion rate equal arrival rate. That
// equality fails exactly when a queue is building, and completions are then
// capped by capacity -- so this understates lambda precisely when demand is
// highest. Tolerable because a building queue is the case OCCUPANCY reads
// well, so the floor is not what carries that decision.
//
// The fallback sums the generating replicas only. A P/D request completes on
// its prefill replica AND on its decode replica, so summing every replica's
// completion rate counts each request twice.
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
// for capacities.
func medianFloat(values []float64) float64 {
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

// generatingReplicas returns the replicas whose completions describe the
// workload -- every role but prefill (see generatesOutput). On a P/D fleet a
// prefill replica finishes each request after one token and hands it off, so
// its completion rate, output length and service time are properties of the
// role, not of the workload. When that leaves nothing, which happens only on a
// fleet whose decode side reports no metrics this cycle, it returns the full
// set rather than an empty one: a prefill reading is a poor estimate, but no
// reading at all would make the floor silently decline on a fleet that is
// demonstrably serving.
func generatingReplicas(input domain.AnalyzerInput) []domain.ReplicaMetrics {
	roles := rolesFromStates(input.VariantStates)
	out := make([]domain.ReplicaMetrics, 0, len(input.ReplicaMetrics))
	for _, rm := range input.ReplicaMetrics {
		if generatesOutput(rm, roles) {
			out = append(out, rm)
		}
	}
	if len(out) == 0 {
		return input.ReplicaMetrics
	}
	return out
}
