package domain

import (
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DecisionReason categorizes the reason for a scaling decision.
// It is the typed category passed to SetDecisionReason (read back via
// ReasonCategory) and is paired there with a human-readable detail string
// (read back via Reason).
type DecisionReason string

// Defined DecisionReason values for the built-in scaling categories.
const (
	// DecisionReasonV2 indicates a V2 pipeline decision.
	DecisionReasonV2 DecisionReason = "V2"
	// DecisionReasonSaturationOnly indicates decision from saturation-only mode.
	DecisionReasonSaturationOnly DecisionReason = "saturation-only mode"
	// DecisionReasonScaleFromZero indicates scale-up from zero replicas.
	DecisionReasonScaleFromZero DecisionReason = "scale-from-zero"
	// DecisionReasonRescale indicates a priority-weighted rescale reclaim — a
	// deliberate, redistributive scale-down (not flappy demand), which downstream
	// stabilization must not damp as if it were noise.
	DecisionReasonRescale DecisionReason = "rescale"
	// DecisionReasonTest is used for test scenarios.
	DecisionReasonTest DecisionReason = "test"
)

// SaturationAnalyzerName is the canonical name for the saturation analyzer.
const SaturationAnalyzerName = "saturation"

// RoleBoth represents the default role when a variant serves both prefill and decode.
const RoleBoth = "both"

// RolePrefill represents the prefill-only role in a P/D disaggregated deployment.
const RolePrefill = "prefill"

// RoleDecode represents the decode-only role in a P/D disaggregated deployment.
const RoleDecode = "decode"

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
	FromWarmPool bool

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

// ReplicaMetricsMetadata contains freshness information for replica metrics
type ReplicaMetricsMetadata struct {
	// CollectedAt is when the metrics were collected
	CollectedAt time.Time
	// Age is the age of the metrics
	Age time.Duration
	// FreshnessStatus indicates freshness: "fresh", "stale", "unavailable"
	FreshnessStatus string
}

// DecisionStep represents a single step in the decision pipeline.
// Each pipeline stage (saturation analysis, resource limiting, etc.) adds its own step.
type DecisionStep struct {
	// Name identifies the pipeline stage (e.g., "saturation", "limiter", "enforcer")
	Name string
	// Action is the action determined by this step
	Action SaturationAction
	// TargetReplicas is the target replicas after this step
	TargetReplicas int
	// Reason explains why this step made its decision
	Reason string
	// WasConstrained is true if this step modified the previous step's target
	WasConstrained bool
	// Timestamp when this step was executed
	Timestamp metav1.Time
}

// VariantDecision represents the scaling decision for a single variant.
//
// This type serves as shared state that flows through the decision pipeline.
// Each pipeline stage (saturation analysis, resource limiting, enforcement)
// reads and modifies the decision, adding its step to DecisionSteps.
//
// Pipeline stages modify the state they own:
//   - Saturation analyzer: sets initial Action, TargetReplicas, SaturationBased
//   - Resource limiter: may constrain TargetReplicas, adds limiting step
//   - Enforcer: applies final constraints (min/max), adds enforcement step
type VariantDecision struct {
	// --- Variant identification ---
	VariantName     string
	Namespace       string
	ModelID         string
	AcceleratorName string
	Cost            float64
	Role            string // "prefill", "decode", "both"

	// --- Scaling state ---
	Action                 SaturationAction
	CurrentReplicas        int
	TargetReplicas         int // Current target (modified by pipeline stages)
	OriginalTargetReplicas int // Original target before resource limiting (for logging)
	DesiredReplicas        int // Original desired replicas from optimizer (from CRD status)

	// --- Resource requirements (for resource limiting) ---
	GPUsPerReplica int // GPUs required per replica
	// SpareCapacity is the variant's absolute spare in KV-cache tokens,
	// max(0, TotalSupply - TotalDemand/scaleDownBoundary) from AnalyzerResult —
	// the scale-down companion to RequiredCapacity, in the same token units.
	SpareCapacity float64
	// Utilization is the variant-level utilization ratio reported for
	// observability: TotalDemand / TotalCapacity from AnalyzerResult. It is
	// capacity-weighted, and unbounded above — a value > 1.0 means demand
	// exceeds supply.
	Utilization float64
	// KvCacheTokensUsed is the sum of TokensInUse across this variant's replicas.
	KvCacheTokensUsed int64
	// KvCacheTokensCapacity is the sum of TotalKvCapacityTokens across this variant's replicas.
	KvCacheTokensCapacity int64
	// RequiredCapacity indicates whether scale-up is needed (>0 means yes). It is
	// the token-based deficit from AnalyzerResult — per-role for P/D
	// disaggregated models, model-level otherwise.
	RequiredCapacity float64
	// RequiredCapacityUnit names the unit of RequiredCapacity, always
	// constants.UnitContinuous (a token magnitude). Exposed as the `unit`
	// Prometheus label on wva_required_capacity and wva_spare_capacity.
	RequiredCapacityUnit string
	// ScaleTargetRef references the Deployment/StatefulSet for scheduling constraints
	ScaleTargetRef *autoscalingv2.CrossVersionObjectReference

	// --- Pipeline tracking ---
	// DecisionSteps records each pipeline stage's contribution to the final decision.
	// This replaces the single Reason field with structured multi-step tracking.
	DecisionSteps []DecisionStep

	// decisionReason is the categorized reason used for Prometheus metric labels.
	// Set via SetDecisionReason along with the detailed reason string.
	decisionReason DecisionReason

	// reason contains the detailed human-readable reason for this decision.
	// Used in logs, events, and status updates.
	// Set via SetDecisionReason along with the categorized decisionReason.
	reason string

	// --- Saturation-specific flags ---
	SaturationBased    bool        // True if decision is primarily saturation-driven
	ModelBasedDecision bool        // True if decision considers model-based optimizer
	SafetyOverride     bool        // True if saturation veto overrode model-based decision
	LastRunTime        metav1.Time // Time when decision was made (for status updates)
	SaturationOnly     bool        // True if operating in saturation-only mode (no model-based analysis)

	// --- Allocation state ---
	// CurrentAllocation carries the collected metrics/allocation state
	// This helps the Controller update status without re-collecting metrics
	CurrentAllocation *Allocation

	// --- Resource limiting results ---
	// GPUsAllocated is the total GPU footprint the target commits for this
	// variant (TargetReplicas × GPUsPerReplica), not the scale-up delta.
	GPUsAllocated int
	// WasLimited indicates that the optimizer wanted to scale this variant
	// further but a GPU budget ran out. It is set only for genuine scarcity —
	// a target held back by the variant's own MaxReplicas ceiling is user
	// intent and leaves this false.
	WasLimited bool
	// LimitedBy names the constraint provider whose pool bound the decision
	// (empty when no provider imposes a finite bound).
	LimitedBy string

	// --- Replica bounds ---
	// MinReplicas is the minimum number of replicas for this variant (from VA spec field).
	// nil means not set (default: 0).
	MinReplicas *int
	// MaxReplicas is the maximum number of replicas for this variant (from VA spec field).
	// nil means not set (no cap).
	MaxReplicas *int

	// --- Metrics availability ---
	// MetricsAvailable indicates whether saturation metrics were available for this decision
	MetricsAvailable bool
	// MetricsReason is the reason for the MetricsAvailable condition
	MetricsReason string
	// MetricsMessage is the human-readable message for the MetricsAvailable condition
	MetricsMessage string
}

// AddDecisionStep adds a step to the decision pipeline history.
// This should be called by each pipeline stage after modifying the decision.
func (d *VariantDecision) AddDecisionStep(name string, reason string, wasConstrained bool) {
	step := DecisionStep{
		Name:           name,
		Action:         d.Action,
		TargetReplicas: d.TargetReplicas,
		Reason:         reason,
		WasConstrained: wasConstrained,
		Timestamp:      metav1.Now(),
	}
	d.DecisionSteps = append(d.DecisionSteps, step)
}

// LastStep returns the most recent decision step, or nil if none.
func (d *VariantDecision) LastStep() *DecisionStep {
	if len(d.DecisionSteps) == 0 {
		return nil
	}
	return &d.DecisionSteps[len(d.DecisionSteps)-1]
}

// SetDecisionReason sets both the typed reason category and detailed reason string.
// The decisionReason should be one of the DecisionReason* constants.
// The detailedReason provides human-readable context for logs, events, and status.
func (d *VariantDecision) SetDecisionReason(action SaturationAction, decisionReason DecisionReason, detailedReason string) {
	d.Action = action
	d.decisionReason = decisionReason
	d.reason = detailedReason
}

// ReasonCategory returns the categorized reason used for Prometheus metric labels.
func (d *VariantDecision) ReasonCategory() DecisionReason {
	return d.decisionReason
}

// Reason returns the detailed human-readable reason for this decision.
func (d *VariantDecision) Reason() string {
	return d.reason
}

// SaturationAction represents the scaling action
type SaturationAction string

const (
	ActionScaleUp   SaturationAction = "scale-up"
	ActionScaleDown SaturationAction = "scale-down"
	ActionNoChange  SaturationAction = "no-change"
)

// VariantReplicaState holds the current and desired replica counts for a variant
type VariantReplicaState struct {
	VariantName     string
	CurrentReplicas int
	DesiredReplicas int // From optimizer/CRD status, 0 if not set
	// PendingReplicas are pods that exist but are not yet ready to serve traffic
	// (CurrentReplicas - ReadyReplicas). This typically occurs during scale-up when
	// new pods are starting (containers initializing, model loading, health checks).
	// Pod startup can take 2-7 minutes depending on model size and hardware.
	// WVA uses this to prevent cascade scaling - avoiding new scale-up requests
	// while pending pods are still becoming ready.
	PendingReplicas int
	// GPUsPerReplica is the number of GPUs required per replica, extracted from
	// the deployment's container resource requests (nvidia.com/gpu, amd.com/gpu, etc.).
	// Defaults to 1 if no GPU requests are found.
	GPUsPerReplica int
	// Role is the P/D disaggregation role: "prefill", "decode", or "both" (default).
	Role string
	// AcceleratorName is the GPU product the variant runs on, resolved by the
	// discovery step. It is an analyzer *input* (used e.g. for cross-variant
	// capacity lookup); the per-variant identity on the analyzer's *output* is
	// filled by the capacity builder from discovery, not laundered per-pod.
	AcceleratorName string
	// Engine is the inference engine the variant runs ("vllm", "sglang"),
	// resolved by discovery. Analyzers with per-engine query bodies use it to
	// select the body for this variant.
	Engine string
	// MinReplicas is the minimum number of replicas for this variant (from VA spec field).
	// nil means not set (default: 0, allows scale to zero).
	MinReplicas *int
	// MaxReplicas is the maximum number of replicas for this variant (from VA spec field).
	// nil means not set (default: 0, no cap).
	MaxReplicas *int
}
