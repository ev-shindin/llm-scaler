package collector

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// collapseToPods merges the per-instance rows of each pod into a single row, so
// a domain.ReplicaMetrics is one *scale-target replica* rather than one engine
// instance.
//
// A vLLM pod running data parallelism hosts DP independently-scraped engine
// instances, distinguished by the port in the instance label; the collector keys
// them "pod:port". Everything downstream, though, counts in scale-target units:
// the optimizer's targets, MaxReplicas, GPUsPerReplica, CurrentReplicas and
// PendingReplicas are all pods (or, for LWS, groups). Emitting per-instance rows
// made the analyzers' supply arithmetic disagree with the units its result is
// spent in — a DP=4 variant would have its pod target computed from a per-
// instance capacity. Collapsing here keeps that arithmetic in one unit without
// every consumer having to know a DP factor the cluster never reports.
//
// LWS needs no special handling: worker pods are dropped upstream, so a group's
// leader is already its single row.
//
// Merging is keyed by pod AND MODEL, not by pod alone -- see podModel. A warm
// pool Pod holds several models at once, and summing their capacity into one
// row would credit a variant with cache belonging to a model it is not running.
//
// How each field merges follows from what it measures:
//
//   - Capacities, queue depth and rates are extensive — summed. A pod's KV
//     capacity is the sum of its instances' capacities, and a request served by
//     any instance is a request served by the pod.
//   - Utilization fractions (KvCacheUsage, KvUsageInstant) are recomputed as
//     Σtokens/Σcapacity rather than averaged, so an instance with a larger cache
//     weighs proportionally more. Without capacity to weigh by they fall back to
//     the plain mean.
//   - Request-shape ratios (input/output tokens, ITL, prefix hit rate) are
//     weighted by RequestRate: they describe the average request, so the
//     instance serving more of them should dominate. With no traffic anywhere on
//     the pod they fall back to the plain mean.
//   - Metadata takes the worst freshness and the oldest age across the
//     instances, so a pod is only as trustworthy as its stalest rank.
//
// Input order is preserved by first appearance of each pod. Rows are copied;
// the input slice is not modified.
func collapseToPods(instances []domain.ReplicaMetrics) []domain.ReplicaMetrics {
	if len(instances) < 2 {
		return instances
	}

	order := make([]podModel, 0, len(instances))
	byPod := make(map[podModel][]domain.ReplicaMetrics, len(instances))
	for _, m := range instances {
		k := podModel{pod: m.PodName, model: m.ModelID}
		if _, seen := byPod[k]; !seen {
			order = append(order, k)
		}
		byPod[k] = append(byPod[k], m)
	}
	if len(order) == len(instances) {
		return instances // one instance per pod: nothing to merge
	}

	pods := make([]domain.ReplicaMetrics, 0, len(order))
	for _, k := range order {
		pods = append(pods, mergePodInstances(byPod[k]))
	}
	return pods
}

// podModel is the merge key: one pod's ranks OF ONE MODEL.
//
// The pod alone is not enough, and the case that breaks it is the warm pool. An
// ordinary Pod belongs to exactly one variant, so its rows can only be DP ranks
// of the same engine. A POOL Pod holds several models at once -- that is what a
// warm set is -- each on its own port, and the collector keys instances
// "pod:port", so they arrive as several rows for one Pod.
//
// Merging those would sum the KV capacity of unrelated models. On a lent Pod
// every row carries the variant it is lent to, so the sleepers' cache would be
// added to the awake model's supply: the variant is told it has more capacity
// than the engine serving it has, which is the direction that causes a
// scale-DOWN the fleet cannot then serve.
//
// Data parallelism is ranks of the SAME model, so the model separates the two.
type podModel struct {
	pod   string
	model string
}

// mergePodInstances merges one pod's instance rows per the rules documented on
// collapseToPods. group is non-empty.
func mergePodInstances(group []domain.ReplicaMetrics) domain.ReplicaMetrics {
	if len(group) == 1 {
		return group[0]
	}

	// Identity is shared by construction: the rows were grouped by pod, and a
	// pod belongs to one variant of one model.
	pod := group[0]

	pod.NumGpuBlocks = 0
	pod.TotalKvCapacityTokens = 0
	pod.TokensInUse = 0
	pod.QueueLength = 0
	pod.RequestRate = 0
	pod.GenerationTokenRate = 0

	var weightedKvUsage, weightedKvInstant float64
	for _, m := range group {
		pod.NumGpuBlocks += m.NumGpuBlocks
		pod.TotalKvCapacityTokens += m.TotalKvCapacityTokens
		pod.TokensInUse += m.TokensInUse
		pod.QueueLength += m.QueueLength
		pod.RequestRate += m.RequestRate
		pod.GenerationTokenRate += m.GenerationTokenRate

		weightedKvUsage += m.KvCacheUsage * float64(m.TotalKvCapacityTokens)
		weightedKvInstant += m.KvUsageInstant * float64(m.TotalKvCapacityTokens)
	}

	// BlockSize is a per-instance config value, uniform across a pod's ranks;
	// keeping the first preserves NumGpuBlocks × BlockSize == TotalKvCapacityTokens.
	capacity := float64(pod.TotalKvCapacityTokens)
	if capacity > 0 {
		pod.KvCacheUsage = weightedKvUsage / capacity
		pod.KvUsageInstant = weightedKvInstant / capacity
	} else {
		pod.KvCacheUsage = mean(group, func(m domain.ReplicaMetrics) float64 { return m.KvCacheUsage })
		pod.KvUsageInstant = mean(group, func(m domain.ReplicaMetrics) float64 { return m.KvUsageInstant })
	}

	pod.AvgInputTokens = weightedByRequestRate(group, func(m domain.ReplicaMetrics) float64 { return m.AvgInputTokens })
	pod.AvgOutputTokens = weightedByRequestRate(group, func(m domain.ReplicaMetrics) float64 { return m.AvgOutputTokens })
	pod.AvgITL = weightedByRequestRate(group, func(m domain.ReplicaMetrics) float64 { return m.AvgITL })
	// Weighted like AvgITL: both are per-request costs, so the engine instance
	// that served more requests should count for more when merging a pod's
	// instances into one replica.
	pod.AvgServiceTime = weightedByRequestRate(group, func(m domain.ReplicaMetrics) float64 { return m.AvgServiceTime })
	pod.PrefixCacheHitRate = weightedByRequestRate(group, func(m domain.ReplicaMetrics) float64 { return m.PrefixCacheHitRate })

	pod.Metadata = mergeMetadata(group)
	return pod
}

// weightedByRequestRate averages field across the group weighted by each
// instance's RequestRate, falling back to the plain mean when the pod is idle
// (every rate zero) and so carries no traffic to weigh by.
func weightedByRequestRate(group []domain.ReplicaMetrics, field func(domain.ReplicaMetrics) float64) float64 {
	var weighted, totalRate float64
	for _, m := range group {
		weighted += field(m) * m.RequestRate
		totalRate += m.RequestRate
	}
	if totalRate > 0 {
		return weighted / totalRate
	}
	return mean(group, field)
}

// mean returns the arithmetic mean of field across the group. group is non-empty.
func mean(group []domain.ReplicaMetrics, field func(domain.ReplicaMetrics) float64) float64 {
	var sum float64
	for _, m := range group {
		sum += field(m)
	}
	return sum / float64(len(group))
}

// mergeMetadata reports the pod as only as fresh as its least fresh instance:
// the worst FreshnessStatus and the oldest Age. Returns nil when no instance
// carried metadata.
func mergeMetadata(group []domain.ReplicaMetrics) *domain.ReplicaMetricsMetadata {
	var merged *domain.ReplicaMetricsMetadata
	// The worst AGE verdict seen, tracked separately from the severity rollup
	// below. "missing" outranks both age verdicts in freshnessSeverity, which is
	// right within a single row -- there it can only mean no metric was present
	// at all -- and wrong ACROSS a pod's ranks, where it lets a rank that
	// reported nothing outrank a rank that reported something stale. Merging
	// those to "missing" would hand the result to StaleOrOlder, which reads
	// "missing" as "never scraped, not old", and the stale rank would vanish:
	// a DP pod could carry five-minute-old numbers while reporting a status
	// that exempts it from every staleness check in the codebase.
	worstAgeVerdict := ""
	for _, m := range group {
		if m.Metadata == nil {
			continue
		}
		if m.Metadata.StaleOrOlder() &&
			freshnessSeverity[m.Metadata.FreshnessStatus] > freshnessSeverity[worstAgeVerdict] {
			worstAgeVerdict = m.Metadata.FreshnessStatus
		}
		if merged == nil {
			copied := *m.Metadata
			merged = &copied
			continue
		}
		if m.Metadata.Age > merged.Age {
			merged.Age = m.Metadata.Age
		}
		if freshnessSeverity[m.Metadata.FreshnessStatus] > freshnessSeverity[merged.FreshnessStatus] {
			merged.FreshnessStatus = m.Metadata.FreshnessStatus
		}
		if m.Metadata.CollectedAt.After(merged.CollectedAt) {
			merged.CollectedAt = m.Metadata.CollectedAt
		}
	}
	// An age verdict on any rank wins over a "missing" rollup. Only over
	// "missing": against "fresh" the severity order already prefers the age
	// verdict, and this must never make a pod look FRESHER than the rollup said.
	if merged != nil && worstAgeVerdict != "" && merged.FreshnessStatus == domain.FreshnessMissing {
		merged.FreshnessStatus = worstAgeVerdict
	}
	return merged
}

// freshnessSeverity orders freshness statuses from best to worst, so both the
// per-instance rollup in collectReplicaMetrics and the pod merge above can pick
// the single worst status across a set of metrics.
var freshnessSeverity = map[string]int{"fresh": 0, "stale": 1, "unavailable": 2, "missing": 3}
