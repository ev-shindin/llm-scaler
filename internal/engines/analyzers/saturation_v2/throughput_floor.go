package saturation_v2

import (
	"maps"
	"slices"
	"strings"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/floor"
)

// The throughput floor: the demand the offered load implies once a replica's
// saturated completion rate (mu) is known, (lambda + backlog / drain) / mu
// replicas per role. The model and its arithmetic are signals/floor (see
// that package's header for the measurements behind it); this file is what
// the saturation analyzer keeps around it -- the window of saturated
// readings per history key, the borrow from a neighbouring bucket, and the
// application of the floor to the demand occupancy measured.

// recordSaturatedThroughput folds one saturated completion-rate reading into
// the window for key. A non-positive rate is not a reading.
//
// The window keeps a MAX, not a mean, and the asymmetry is the point. A
// replica's completion rate while saturated under-reads its capacity while
// the replica is full: in the first minute after it fills, the requests
// completing are the few that were admitted first (a 1m rate on a replica
// that has been full for 20s counts a third of a minute's completions);
// under KV pressure preemption and recompute drop it further. A mean of
// under-reads is an under-read, and the floor divides by it, so an
// under-read mu orders replicas that are not needed. The max of the window
// is the best estimate of what the replica sustains, and a high reading
// errs the way this file already accepts: a floor that is too LOW holds
// back, where occupancy still carries a real shortfall. The exception, and
// it is open, is the drain at the end of an episode -- see below.
//
// A reading counts as a SAMPLE of its own only when it lands
// ThroughputSampleSpacing after the last one that counted and differs from
// it. The rate is rate(...[RequestRateWindow]) evaluated afresh every 15 s
// cycle, so the next cycle reads mostly the same window -- at 30 s scrapes,
// exactly the same two samples, and the same value to the digit -- and one
// saturated moment shows up on several consecutive cycles. Counted each
// time, two cycles cleared MinThroughputSamplesToOrder on the first
// window's under-read: the guard was dead at that scrape interval. Value
// equality alone is not the test either: the rate is an integer count of
// completions over the scrape interval, so two different windows agree to
// the digit a few percent of the time, and two replicas saturating in one
// cycle read two values from the same moment. A minute apart, two readings
// share no samples.
//
// A reading inside the spacing belongs to the last sample's window, and is
// folded into it: the sample becomes the max of what that window read
// (RaiseLast) -- the max the window kept when every cycle was added, no
// more. Dropping in-spacing readings instead lost the highest reading of
// an episode exactly when no next window would come, and on two replayed
// passes left mu 10-12 % low for the rest of the pass, on one of them
// ordering a fourth replica the run never needed. Folding also makes two
// replicas saturating in one cycle read as one moment at the higher of
// the two, whichever row the collector's map yields first.
//
// What the max is worth is another matter. A saturated rate under-reads
// while the replica is full (file header),
// and fails in the last minute of an episode that ends by a replica
// landing: the queue gate is a one-minute max and stays up while the rate
// is fresh, and the fresh rate is the batch draining -- sequences admitted
// together finish together, and no new ones come. Measured on the cold
// pass of 2026-09-19 (second): 6.67 req/s on the last saturated cycle,
// 200 completions in 30 s, with token throughput below the plateau the
// replica held while full (~5.0 req/s sustained), and the window carried
// 6.67 for the rest of the phase. The max window has always taken these;
// nothing in it lowers a reading once taken. The fix is upstream of this
// function -- a reading gated on an instantaneous queue, or a mu priced
// from the generation-token rate over the bucket's output length, which
// does not burst on a drain -- and is open.
//
// A reading equal to the last one given, at or past the spacing, is the
// same scrape pair read again across the boundary (a re-read of the
// previous pair when the next scrape has not landed), or a genuine repeat,
// and only touches; that costs a cycle or two. Compared to the last
// READING, not to the sample, which the fold may have raised above it.
//
// Measured on the shape-swap trace: every saturated rate logged over four
// passes is N/30 (103/30 = 3.43, 110/30 = 3.67, 165/30 = 5.5 ...) but a
// fresh replica's first, extrapolated window, and on the first cold pass
// of 2026-09-19 one decode replica's 3.43 stood on four consecutive cycles
// -- two scrape pairs -- and let the floor order on it. The 35 minutes of
// a third replica that followed were the under-read itself, which
// occupancy would have ordered 30 s later and the floor then held either
// way; the sample count is what this fixes, not that.
//
// Same window size and staleness rule as k2 history, and pruned beside it in
// EvictStaleHistory.
func (a *SaturationAnalyzer) recordSaturatedThroughput(key string, rate float64) {
	if !(rate > 0) {
		return
	}
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	ra, ok := a.saturatedThroughput[key]
	if !ok || ra.Stale(capacity.HistoryEvictionTimeout) {
		ra = capacity.NewRollingAverage(capacity.RollingAverageWindowSize)
		a.saturatedThroughput[key] = ra
		delete(a.throughputSampledAt, key)
		delete(a.throughputLastRead, key)
	}
	lastRead, read := a.throughputLastRead[key]
	a.throughputLastRead[key] = rate
	if last, sampled := a.throughputSampledAt[key]; sampled {
		if now.Sub(last) < ThroughputSampleSpacing {
			ra.RaiseLast(rate) // the last sample's window, still being read
			return
		}
		if read && rate == lastRead {
			ra.Touch() // the same pair across the boundary, or a repeat
			return
		}
	}
	ra.Add(rate)
	a.throughputSampledAt[key] = now
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
	replicas []capacity.ReplicaCapacity,
	variants []domain.VariantCapacity,
	totalDemand float64,
	roleDemand map[string]float64,
	eppByRole map[string]float64,
	eppQueued float64,
	// staleShape is set while a fleet-shape change is outstanding: every
	// mu on record was learned under a shape the fleet has left, so the
	// floor may hold on one but not order on it (shape_change.go).
	staleShape bool,
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
	tf := floor.Estimate(offeredArrivalRate(input), replicas, variants, backlog, floor.BacklogDrainSeconds, scaleUp, staleShape)

	// Prefill with no mu: the scheduler queue's prompts are not resident work
	// for prefill (file header). Only the disaggregated case has a prefill
	// entry to correct, and only the role figure moves: the model-level total
	// carries the scheduler queue once, as input + output, which is decode's
	// share; prefill's input-only share was never in it
	// (estimateSchedulerQueueDemand).
	if roleDemand != nil {
		if _, priced := tf.ByRole[domain.RolePrefill]; !priced {
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
	if len(tf.ByRole) == 0 {
		return totalDemand
	}
	// Roles in a stable order, so a two-role fleet logs the same way each cycle.
	roles := slices.Sorted(maps.Keys(tf.ByRole))

	for _, role := range roles {
		tokens := tf.ByRole[role]
		term := tf.Terms[role]
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
			"arrivalRate", tf.Lambda, "backlogRequests", term.Backlog, "drainSeconds", tf.DrainSeconds,
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
