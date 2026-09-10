package domain

import "time"

// This file holds the ANALYZER-AGNOSTIC replica signal.
//
// It lived in saturation_analyzer.go, which read as though it belonged to one
// analyzer. It does not: every analyzer that works from measured rows consumes
// ReplicaMetrics (saturation_v2, throughput), the collector produces it, and the
// capacity-build step depends on what it carries. A shared type named after one
// consumer invites exactly the change this split exists to prevent -- a rule
// written into one analyzer and silently absent from the others.

// ReplicaMetrics holds one scale-target replica's measured signal.
//
// It is per *replica*, in the same units as domain.VariantMetadata and as the
// replica targets the optimizer produces: one record per pod, or for LWS per
// group (only leader pods emit engine metrics, so a leader's record is its
// group's). A data-parallel pod exposes one metrics endpoint per DP rank and is
// scraped as several series keyed pod_name:port; the collector merges those
// into this one record (collector.collapseToPods) so that counting these
// records yields a replica count and the capacity derived from one is a
// per-replica capacity.
//
// It carries *signal only*. Variant identity (cost, accelerator, role) belongs
// to the discovery step and reaches consumers via domain.VariantMetadata; it is
// not repeated on every replica record.
type ReplicaMetrics struct {
	PodName      string
	KvCacheUsage float64 // KV cache utilization (0.0-1.0)
	QueueLength  int     // Number of requests waiting
	VariantName  string  // Name of the variant this replica belongs to
	Namespace    string
	ModelID      string // Model ID for grouping variants

	// FromWarmPool marks a record that came from a BRIDGE: a warm pool Pod lent
	// to this variant while it is short, rather than one of its own replicas.
	//
	// Its load is this variant's load and counts toward DEMAND like any other.
	// Its capacity is not this variant's SUPPLY: the Pod is borrowed and goes
	// back when the ordinary replicas arrive, so counting it would tell the
	// optimizer the fleet is already big enough and suppress the scale-up the
	// bridge exists to cover -- after which the replicas that would release the
	// Pod are the ones it just prevented.
	//
	// So the two aggregations treat it differently, and this flag is what lets
	// them. What it is worth is measured separately and published for the
	// retained-pool switching decision; see decision.WarmPoolSupply.
	//
	// A BOOL, NOT A POOL NAME, AND THAT RESTS ON AN INVARIANT: a variant is
	// warmed by AT MOST ONE POOL. Its bridges therefore all come from the same
	// pool, run at that pool's --gpu-memory-utilization, and are directly
	// comparable -- which is what makes one median over them meaningful and what
	// lets decision.WarmPoolSupply key by variant alone.
	//
	// Different variants of one model MAY sit in different pools, including the
	// prefill and decode of the same model: a variant is named by its
	// ScaledObject, role included, and every figure here is per variant, so those
	// aggregate independently and never meet.
	//
	// Warming ONE variant from two pools would break this -- their bridges would
	// carry genuinely different capacities and the median would describe neither
	// -- and the pool layer makes that unreachable rather than merely unlikely.
	// A variant names at most one pool, in the `warmPool` trigger metadata key,
	// and warmpool.VariantsFor hands a variant only to the pool it names (or to
	// the single pool, when a namespace has exactly one and there is nothing to
	// disambiguate). With several pools declared, a variant naming none is
	// Unassignable: it gets no warm copy at all, and is reported.
	FromWarmPool bool

	// Ready is the Pod's own readiness condition, read from the Pod and not
	// inferred from the fact that it answered a scrape.
	//
	// The two are not the same and the gap is not small: an engine serves
	// /metrics as soon as its HTTP server is up, which is BEFORE it passes
	// readiness, so a starting replica reports for some seconds while no Service
	// and no EPP will route to it.
	//
	// That gap matters to exactly one consumer today. The warm pool asks how
	// many of a variant's own replicas are serving, to decide whether the Pod it
	// lent can go home; answering with a replica that is reporting but not yet
	// in the rotation hands the traffic back to nothing. It does NOT bear on
	// capacity: a Pod that is starting still holds its GPU and its KV cache is
	// real, so the analyzer counts it.
	Ready bool

	// Metadata contains freshness information (optional)
	Metadata *ReplicaMetricsMetadata `json:"metadata,omitempty"`

	// --- Fields for Saturation Analyzer V2 ---

	// NumGpuBlocks is the total number of KV cache blocks allocated on GPU.
	// Sourced from vllm:cache_config_info label "num_gpu_blocks".
	// Zero value means cache_config_info metric is not available.
	NumGpuBlocks int64

	// BlockSize is the number of tokens per KV cache block.
	// Sourced from vllm:cache_config_info label "block_size".
	// Zero value means cache_config_info metric is not available.
	BlockSize int64

	// TotalKvCapacityTokens is NumGpuBlocks × BlockSize (total token slots).
	// Computed by the collector after parsing cache_config_info labels.
	// Zero value means capacity data is unavailable.
	TotalKvCapacityTokens int64

	// TokensInUse is the derived current token demand on this replica.
	// Computed as KvCacheUsage × TotalKvCapacityTokens.
	// Zero when TotalKvCapacityTokens is unavailable.
	TokensInUse int64

	// AvgOutputTokens is the average generation tokens per request on this replica.
	// Derived from rate(generation_tokens_sum) / rate(generation_tokens_count).
	// Used by saturation V2 for token-demand estimation (k2 derivation) and by
	// the queueing model analyzer for RequestSize and service rate computation.
	// Zero when metrics are unavailable.
	AvgOutputTokens float64

	// AvgInputTokens is the average prompt tokens per request on this replica.
	// Derived from rate(prompt_tokens_sum) / rate(prompt_tokens_count).
	// Used by saturation V2 for token-demand estimation (k2 derivation) and by
	// the queueing model analyzer for RequestSize and service rate computation.
	// Zero when metrics are unavailable.
	AvgInputTokens float64

	// PrefixCacheHitRate is the fraction of prefix cache queries that were hits (0.0-1.0).
	// Derived from rate(vllm:prefix_cache_hits[5m]) / rate(vllm:prefix_cache_queries[5m]).
	// Used to reduce estimated input token demand for scheduler-queued requests.
	// Zero when prefix caching is disabled or metrics are unavailable.
	PrefixCacheHitRate float64

	// AvgServiceTime is how long a request occupies this replica, in seconds,
	// EXCLUDING time spent waiting in the queue.
	//
	// The distinction is what makes it usable for sizing. End-to-end latency
	// climbs when a replica is behind and falls when it catches up, so it varies
	// with capacity; service time is what one request costs to serve and does
	// not. Zero when the engine does not publish it or nothing has completed.
	AvgServiceTime float64

	// AvgITL is the average inter-token latency on this replica in seconds.
	// Derived from rate(vllm:time_per_output_token_seconds_sum[5m]) / rate(..._count[5m]).
	// Used by queueing model tuner as observed ITL for Kalman filter parameter learning.
	// TA notation: ITL_obs — the (k*, ITL_obs) pair drives OLS calibration of ITL(k) = A·k + B.
	// Zero when metrics are unavailable.
	AvgITL float64

	// --- Fields for Throughput Analyzer ---

	// GenerationTokenRate is the observed decode token generation rate on this replica (tokens/sec).
	// Derived from rate(vllm:request_generation_tokens_sum[1m]) per pod.
	// TA notation: μ_dec^obs — directly observable supply proxy; also used as a sanity check
	// against the demand estimate (μ_dec^obs ≈ λ_dec at steady state with no queueing).
	// Zero when metrics are unavailable.
	GenerationTokenRate float64

	// KvUsageInstant is the instantaneous KV cache utilization fraction on this replica (0.0–1.0).
	// Derived from vllm:kv_cache_usage_perc (no max_over_time window).
	// TA notation: k* — the current operating point in the ITL model ITL(k) = A·k + B.
	// Differs from KvCacheUsage which uses max_over_time[1m] for the saturation analyzer.
	// Zero when metrics are unavailable.
	KvUsageInstant float64

	// RequestRate is the engine-side request completion rate on this replica (req/s).
	// Engine-agnostic: derived per pod from rate(vllm:request_generation_tokens_count[1m])
	// for vLLM and rate(sglang:generation_tokens_histogram_count[1m]) for SGLang.
	// TA notation: fallback λ_req — used when ArrivalRate == 0 (EPP not deployed).
	// λ_dec_fallback = sum(RequestRate) × avg(AvgOutputTokens).
	// Measures completed requests only; undercounts when requests queue in the scheduler.
	// Zero when metrics are unavailable.
	RequestRate float64
}

// FreshnessStatus values, as written by the collector's classifier.
//
// Three of the four are AGE bands over the same present timestamp, and only the
// fourth means the metric was never scraped. That distinction is the trap this
// block exists to make visible: "unavailable" is not "absent", it is data so old
// the collector has given up on it, and its VALUE is still sitting in the row.
// Every consumer that tested FreshnessStatus == FreshnessStale by equality --
// there were three -- silently exempted the oldest data in the system from the
// staleness check it had just written.
const (
	// FreshnessFresh means the metric is younger than the fresh threshold
	// (1 minute by default).
	FreshnessFresh = "fresh"
	// FreshnessStale means the metric is between the fresh and unavailable
	// thresholds (1 to 5 minutes by default). The value is real but behind.
	FreshnessStale = "stale"
	// FreshnessUnavailable means the metric is past the unavailable threshold
	// (5 minutes by default). Still a value, only older than FreshnessStale.
	FreshnessUnavailable = "unavailable"
	// FreshnessMissing means the metric carries no timestamp at all, so it was
	// never scraped for this replica. Unlike the three above it says nothing
	// about age, and the row's value for it is zero.
	FreshnessMissing = "missing"
)

// ReplicaMetricsMetadata contains freshness information for replica metrics
type ReplicaMetricsMetadata struct {
	// CollectedAt is when the metrics were collected
	CollectedAt time.Time
	// Age is the age of the metrics
	Age time.Duration
	// FreshnessStatus indicates freshness: one of the Freshness* constants.
	FreshnessStatus string
}

// StaleOrOlder reports data the collector has judged to be past the fresh
// threshold -- FreshnessStale or FreshnessUnavailable.
//
// Use this rather than comparing FreshnessStatus to FreshnessStale. The two
// differ only in how far past the threshold the data is, so a consumer that
// excludes one and not the other applies its own rule everywhere except to the
// data most in need of it.
//
// FreshnessMissing is NOT stale. It means the metric was never scraped, which is
// also what a first collection looks like, and the row's value for it is zero --
// so a consumer skipping zeros has already skipped it, and a consumer counting
// it would be excluding a replica for being new.
//
// A nil metadata pointer is not stale either: rows reach some consumers without
// metadata at all, and reading "no information" as "too old" would empty a
// sample rather than leave it alone.
func (m *ReplicaMetricsMetadata) StaleOrOlder() bool {
	if m == nil {
		return false
	}
	return m.FreshnessStatus == FreshnessStale || m.FreshnessStatus == FreshnessUnavailable
}
