// Package registration provides query registration for metrics sources.
// This file registers queries used by the throughput analyzer (ThroughputAnalyzer).
package registration

import (
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// Query name constants for throughput analyzer metrics.
//
// Only four queries are registered here — those that are genuinely new and not
// provided by other analyzer registrations. The remaining TA inputs are already
// collected and exposed via domain.ReplicaMetrics; the TA reads those fields
// directly instead of re-registering duplicate PromQL templates.
//
// TA notation → ReplicaMetrics field (query / registration):
//
//	KV_max  (total KV token capacity) → TotalKvCapacityTokens  (QueryCacheConfigInfo       / RegisterSaturationQueries)
//	ITL_obs (observed ITL, seconds)   → AvgITL                 (QueryAvgITL                / RegisterQueueingModelQueries)
//	OL      (avg output tokens)       → AvgOutputTokens        (QueryAvgOutputTokens       / RegisterSaturationQueries)
//	IL      (avg input tokens)        → AvgInputTokens         (QueryAvgInputTokens        / RegisterSaturationQueries)
//	H%      (prefix cache hit rate)   → PrefixCacheHitRate     (QueryPrefixCacheHitRate    / RegisterSaturationQueries)
//	λ_dec   (per-pod completion rate) → RequestRate            (QueryRequestRate           / RegisterArrivalRateQueries)
//	Λ_req   (model-level arrival)     → AnalyzerInput.ArrivalRate (QueryModelArrivalRate   / RegisterArrivalRateQueries, this file)
//	         λ_dec = Λ_req × avgOL, combined with the queue-drain term (model level, see Commit 2)
const (
	// RequestRateWindow is the range the per-pod completion rate is taken
	// over (rate(...[RequestRateWindow])). The saturation analyzer spaces
	// the readings it counts as samples of that rate by the same interval
	// (saturation_v2.ThroughputSampleSpacing): two readings this far apart
	// share no scrape, closer ones share most of them. Change the two
	// together; a test in saturation_v2 holds them equal.
	RequestRateWindow = "1m"

	// QueryGenerationTokenRate is the query name for the observed generation
	// (decode) token rate per pod (tokens/sec).
	// This is the direct observable proxy for μ_dec^obs — how many tokens each
	// replica is currently generating per second.
	// Source: vllm:request_generation_tokens_sum (histogram _sum counter)
	QueryGenerationTokenRate = "generation_token_rate"

	// QueryKvUsageInstant is the query name for the instantaneous KV cache utilization
	// fraction per pod (0.0–1.0). Used as k* (current operating point) in the ITL
	// model: ITL(k) = A·k + B.
	//
	// Same underlying metric as QueryKvCacheUsage (vllm:kv_cache_usage_perc), but
	// without max_over_time. QueryKvCacheUsage wraps the gauge in max_over_time[1m]
	// to give the saturation analyzer a conservative peak. This query reads the raw
	// gauge so the throughput analyzer sees the current operating point, not a
	// 1-minute high-water mark that could overestimate load and trigger premature
	// scale-up after a transient spike.
	//
	// max by (model_name, instance, pod): deduplication only. vllm:kv_cache_usage_perc
	// is a single scalar gauge per vLLM process; there is one series per pod in normal
	// deployment. The max by (...) collapses any duplicate series that arise when a pod
	// is scraped by multiple targets (e.g., PodMonitor + ServiceMonitor). Since duplicates
	// carry the same value, max = avg — the choice has no effect on correctness.
	// Source: vllm:kv_cache_usage_perc (gauge)
	QueryKvUsageInstant = "kv_usage_instant"

	// QueryRequestRate is the query name for the engine-side request completion
	// rate per pod (req/s), derived from the generation tokens histogram count.
	// It is engine-agnostic: the vLLM variant reads
	// vllm:request_generation_tokens_count and the SGLang variant reads
	// sglang:generation_tokens_histogram_count.
	//
	// Used as a fallback for λ_dec estimation when EPP/scheduler metrics are
	// unavailable. Per variant V, the analyzer computes:
	//   λ_dec = Σ_{r∈V}(RequestRate_r × AvgOutputTokens_r)
	//
	// Note: measures completed requests (served demand), not arriving requests.
	// It undercounts when requests are queued in the scheduler. The MODEL-level
	// arrival rate (QueryModelArrivalRate) is what sizes the fleet; this is only
	// the per-variant figure the throughput analyzer displays.
	QueryRequestRate = "request_rate"

	// QueryModelArrivalRate is the query name for the model-level request arrival
	// rate (requests/sec), summed across the whole model with no per-pod labels to
	// reconcile. A per-pod form of this metric used to exist and was removed: its
	// pod_name/port grouping could not be joined against vLLM's per-instance
	// series, because the EPP reports the port it ROUTES to while every engine
	// series is keyed on the port it is SCRAPED on.
	//
	// It reads the flow-control ENQUEUE counter, which increments when a
	// request is accepted, and not the scheduler's success counter, which
	// increments when one is placed on a pod. The difference is the whole
	// point: while the fleet is saturated, placements are capacity-bound, so
	// a rate built from them collapses exactly when the fleet is furthest
	// behind and most needs the capacity this figure orders. Measured at
	// 1.66 req/s against a true 6.16 with the scheduler queue at 234 and climbing to 356; the two agree
	// to the digit once the queue drains. See "The arrival rate is a
	// dispatch rate while the queue is building" in
	// docs/developer-guide/analyzer-evidence.md.
	//
	// No model_name fallback: inference_extension_scheduler_attempts_total has
	// never carried a model_name label on any EPP version examined (only
	// target_model_name) — unlike the flow-control queue metric, which carries
	// both. target_model_name is what the join below borrows.
	QueryModelArrivalRate = "model_arrival_rate"
)

// The model arrival rate, assembled so each clause can be read on its own:
// enqueues × (the model label, borrowed), falling back to placements.
//
// The label has to be borrowed because the enqueue counter does not carry one:
// it is per (job, fairness_id, priority, outcome). The flow-control queue gauge
// does carry target_model_name and comes from the same EPP, so it is joined on
// (namespace, job).
//
// Each `or` between a pair of metric names aggregates BEFORE the or, never
// after. `or` compares whole label sets, so a label present on one name and
// not the other makes the two series look unrelated and the union keeps both —
// a silent doubling. That is not hypothetical: on the benchmark cluster
// llm_d_epp_scheduler_attempts_total carries endpoint_name where
// inference_extension_scheduler_attempts_total carries pod_name, and the naive
// form reads 9.75 req/s against a true 4.88. Aggregating first drops the
// offending label before the comparison.
const (
	// The two names for each flow-control metric. llm_d_epp_* is current and
	// inference_extension_* the deprecated alias (constants/metrics.go), read
	// in that order as the rest of the collector does.
	arrivalEnqueueRate = `(sum by (namespace, job) (rate(llm_d_epp_flow_control_request_enqueue_duration_seconds_count{namespace="{{.namespace}}"}[1m]))` +
		` or sum by (namespace, job) (rate(inference_extension_flow_control_request_enqueue_duration_seconds_count{namespace="{{.namespace}}"}[1m])))`

	// target_model_name!="" is load-bearing, not hygiene. An EPP does export a
	// second queue_size series with the label absent — measured on a
	// benchmark-cluster namespace — and group_left needs the right-hand
	// (namespace, job) to be unique.
	//
	// What goes wrong depends on what else is present. Without this matcher the
	// guard below sees two series and drops the join, so the query silently
	// falls through to the placement arm and the under-read this whole file
	// exists to fix comes back with no signal that it did. Without the guard
	// as well, the expression fails outright with "found duplicate series for
	// the match group" — and a PromQL error is not something `or` can catch,
	// so the collector reads no series, which it cannot tell from idle
	// traffic. Both were reproduced.
	arrivalQueueByModel = `max by (namespace, job, target_model_name) (` +
		`llm_d_epp_flow_control_queue_size{namespace="{{.namespace}}",target_model_name!=""}` +
		` or inference_extension_flow_control_queue_size{namespace="{{.namespace}}",target_model_name!=""})`

	// ^ 0 is 1 for every finite value and for NaN, so it copies the label
	// across and leaves the rate alone. The count == 1 guard drops the join
	// entirely where one pool serves several models: the enqueue counter has
	// no model label, so it cannot be split between them, and an arrival rate
	// attributed to the wrong model is worse than the under-read below. Those
	// fleets fall through to the placement arm, which attributes correctly.
	arrivalModelLabel = `((` + arrivalQueueByModel + ` ^ 0) and on (namespace, job) ` +
		`(count by (namespace, job) (` + arrivalQueueByModel + `) == 1))`

	// Placements. Capacity-bound while the fleet is saturated — the whole
	// reason for the arm above — but correct once it is not, and the only
	// thing available on a fleet without flow control. This is the arm that
	// carries a fleet the flow-control metrics do not describe, so it reads
	// both names too, and it is the pair whose labels already disagree.
	arrivalPlacementRate = `(sum by (namespace, target_model_name) (rate(llm_d_epp_scheduler_attempts_total{status="success",namespace="{{.namespace}}"}[1m]))` +
		` or sum by (namespace, target_model_name) (rate(inference_extension_scheduler_attempts_total{status="success",namespace="{{.namespace}}"}[1m])))`

	// `> 0` so that a present-but-zero enqueue series cannot mask a live
	// placement rate: `or` keeps a right-hand series only where the left has
	// no series at all, and a zero is a series. Its shape is a fleet whose
	// flow controller is deployed but rarely queues.
	//
	// It is a presence test, not a plausibility one: an EPP that routed only
	// part of its traffic through flow control would pass it with a rate well
	// below the true one. No such fleet has been seen, and `max` of the two
	// arms would cover that case as well — at the cost of reporting a drain
	// rate as demand after traffic stops, which this form already does.
	modelArrivalRateQuery = `sum by (namespace, target_model_name) (` +
		arrivalEnqueueRate + ` * on (namespace, job) group_left(target_model_name) ` +
		arrivalModelLabel + `) > 0 or ` + arrivalPlacementRate
)

// RegisterThroughputAnalyzerQueries registers the four TA-exclusive queries.
// It must be called once at engine startup alongside other analyzer registrations.
//
// Registered queries:
//   - QueryGenerationTokenRate — μ_dec^obs: observed decode token rate per pod
//   - QueryKvUsageInstant      — k*: instantaneous KV cache utilization per pod
//   - QueryRequestRate     — fallback λ_req: completion rate per pod when EPP absent
//   - QueryModelArrivalRate    — Λ_req: model-level request arrival rate
//
// Additional TA inputs are read from domain.ReplicaMetrics fields populated by
// RegisterSaturationQueries (TotalKvCapacityTokens, AvgOutputTokens, AvgInputTokens,
// PrefixCacheHitRate) and RegisterQueueingModelQueries (AvgITL, ArrivalRate).
// See the package-level constant block for the full TA notation → field mapping.
//
// μ_dec is computed using a linear ITL model:
//
//	ITL(k)   = A·k + B            (calibrated from AvgITL × k* pairs over time)
//	IL_eff   = IL × (1 - H%)
//	KV_req   = IL_eff + OL/2
//	N_dec(k) = k × KV_max / KV_req
//	μ_dec    = N_dec(k_sat) / ITL(k_sat)
//
// Per variant V (summed over that variant's replicas only):
// λ_dec primary:  Σ_{r∈V}(ArrivalRate_r × AvgOutputTokens_r)     [EPP deployed]
// λ_dec fallback: Σ_{r∈V}(RequestRate_r × AvgOutputTokens_r) [EPP absent]
func RegisterThroughputAnalyzerQueries(sourceRegistry *source.SourceRegistry) {
	metricsSource := sourceRegistry.Get("prometheus")
	if metricsSource == nil {
		ctrl.Log.V(logging.DEBUG).Info("Prometheus source not registered, skipping throughput analyzer query registration")
		return
	}
	registry := metricsSource.QueryList()

	// The generation-token rate moved to RegisterArrivalRateQueries, which
	// runs whether or not this analyzer is enabled: the saturation analyzer's
	// demand floor prices mu from it. Registering it here as well would panic
	// on the duplicate the moment this analyzer is turned on.

	// Per-pod instantaneous KV cache utilization (0.0–1.0).
	// Does NOT use max_over_time: the throughput analyzer needs the current
	// operating point k*, not the worst-case peak used by the saturation analyzer.
	// Grouping key and namespace scoping: see the note above RegisterSaturationQueries
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryKvUsageInstant,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (vllm:kv_cache_usage_perc{namespace="{{.namespace}}"})`,
		Params:      []string{source.ParamNamespace},
		Description: "Instantaneous KV cache utilization per pod (0.0–1.0), used as k* in the ITL model",
	})

	registerSGLangThroughputAnalyzerQueries(registry)
}

// RegisterArrivalRateQueries registers how fast work is ARRIVING: the
// model-level rate from the scheduler, and the per-pod completion rate that
// stands in for it when the scheduler's is unavailable.
//
// Registered UNCONDITIONALLY, unlike the rest of this file. These two used to
// sit inside RegisterThroughputAnalyzerQueries, which cmd/main only calls when
// the throughput analyzer is enabled — and that analyzer is opt-in. The
// saturation analyzer's demand floor also needs λ, so with throughput disabled
// (the default) both of its sources were structurally zero and the floor could
// never compute anything. It failed silently and correctly: "no arrival rate",
// every cycle, on a fleet visibly serving 14 QPS.
//
// The cost of always collecting them is two Prometheus queries per namespace per
// cycle. The cost of not doing so was a feature that could not work at all.
func RegisterArrivalRateQueries(sourceRegistry *source.SourceRegistry) {
	registry := sourceRegistry.Get("prometheus").QueryList()

	// Per-pod vLLM request completion rate (req/s).
	// Derived from the generation tokens histogram _count (increments once per
	// completed request). Used as a fallback for λ when EPP/scheduler metrics
	// are unavailable; per variant V, the throughput analyzer falls back to:
	//   λ_dec_fallback = Σ_{r∈V}(RequestRate_r × AvgOutputTokens_r)
	// Grouping key and namespace scoping: see the note above RegisterSaturationQueries
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryRequestRate,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (model_name, instance, pod) (rate(vllm:request_generation_tokens_count{namespace="{{.namespace}}"}[` + RequestRateWindow + `]))`,
		Params:      []string{source.ParamNamespace},
		Description: "vLLM request completion rate per pod (req/s); fallback for λ when EPP metrics are unavailable",
	})

	// Model-level request arrival rate (requests/sec), summed across the whole
	// model. Grouped only by namespace and target_model_name — no pod_name/port
	// labels, so none of the per-instance attribution fragility. Engine-agnostic
	// (sourced from EPP, not vLLM/SGLang), so it needs no SGLang variant.
	//
	// Namespace-scoped like the engine queries: the collector selects its model by
	// target_model_name (see the note above RegisterSaturationQueries).
	//
	// The query and the reasoning for each clause are above, on
	// modelArrivalRateQuery.
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryModelArrivalRate,
		Type:        source.QueryTypePromQL,
		Template:    modelArrivalRateQuery,
		Params:      []string{source.ParamNamespace},
		Description: "Model-level request arrival rate (requests/sec): flow-control enqueues, with the scheduler's placements as a fallback where flow control is absent",
	})

	// Per-pod observed generation (decode) token rate (tokens/sec), the rate of
	// the generation-token COUNTER over 1m.
	//
	// Not the _sum of vllm:request_generation_tokens, which carries the same
	// running total but is a histogram observed when a request FINISHES: its rate
	// is zero while a long generation runs and jumps by the whole request at
	// completion, so it bursts on a drain exactly as the _count does. Measured on
	// the 2026-09-22 rerun, which took mu from the histogram sum: the window
	// ratcheted 0.92 -> 1.14 -> 1.42 -> 1.92 -> 2.26 -> 3.00 -> 3.60 req/s across
	// one phase of a workload whose shape never changed, and at 3.60 the floor
	// asked for 1.6 replicas where the fleet needed about 8. The counter does not
	// burst: tokens accrue as they are produced.
	//
	// Unconditional for the same reason as the two above, and discovered the
	// same way. The saturation analyzer's demand floor prices a replica's
	// saturated throughput as this rate over the shape's output length --
	// completions burst when a batch drains and a max window then carries the
	// burst, where generated tokens do not. Left registered only with the
	// throughput analyzer, which is opt-in and off by default, the field was
	// structurally zero and the floor recorded nothing at all: measured on the
	// 2026-09-22 rerun as 21 saturated cycles that produced no window and no
	// floor for the whole run.
	// Grouping key and namespace scoping: see the note above RegisterSaturationQueries
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryGenerationTokenRate,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (model_name, instance, pod) (rate(vllm:generation_tokens_total{namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Observed generation (decode) token rate per pod (tokens/sec), proxy for μ_dec^obs",
	})

	registerSGLangArrivalRateQueries(registry)
}

// registerSGLangArrivalRateQueries registers the SGLang completion-rate variant.
// The model-level arrival rate comes from the EPP and is engine-agnostic, so it
// has no counterpart here.
func registerSGLangArrivalRateQueries(registry *source.QueryList) {
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryRequestRate,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (model_name, instance, pod) (rate(sglang:generation_tokens_histogram_count{namespace="{{.namespace}}"}[` + RequestRateWindow + `]))`,
		Params:      []string{source.ParamNamespace},
		Description: "SGLang request completion rate per pod (req/s); fallback for λ when EPP metrics are unavailable",
	})

	// Per-pod observed generation token rate, unconditional for the reason given
	// on the vLLM template above.
	//
	// The counter where it exists, the histogram sum where it does not.
	//
	// SGLang exposes _total counters for its other token series
	// (sglang:prompt_tokens_total, sglang:cached_tokens_total), so
	// sglang:generation_tokens_total very probably exists and is the right
	// source for the reason the vLLM template above gives. It has not been read
	// off a live SGLang engine here, and naming a series that does not exist
	// fails the way this whole area fails -- silently, to zero, taking the
	// demand floor with it.
	//
	// PromQL's `or` resolves that without having to know: it yields the
	// left-hand vector's series, plus the right-hand series that have no match
	// on the left. So an engine exposing the counter is priced from the
	// counter, and one exposing only the histogram keeps exactly the behaviour
	// it has today. The vLLM side needs no such hedge; its counter was read off
	// a running pod.
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryGenerationTokenRate,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (model_name, instance, pod) (rate(sglang:generation_tokens_total{namespace="{{.namespace}}"}[1m]) or rate(sglang:generation_tokens_histogram_sum{namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Observed generation (decode) token rate per pod (tokens/sec), proxy for μ_dec^obs (SGLang)",
	})
}

// registerSGLangThroughputAnalyzerQueries registers the SGLang variants of the
// throughput-analyzer queries. SGLang exposes generation tokens via the
// generation_tokens_histogram series and KV utilization via token_usage.
func registerSGLangThroughputAnalyzerQueries(registry *source.QueryList) {
	// The SGLang generation-token rate moved to registerSGLangArrivalRateQueries,
	// for the reason given in RegisterThroughputAnalyzerQueries.

	// Per-pod instantaneous KV cache utilization (0.0-1.0), no max_over_time.
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryKvUsageInstant,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (sglang:token_usage{namespace="{{.namespace}}"})`,
		Params:      []string{source.ParamNamespace},
		Description: "Instantaneous KV cache utilization per pod (0.0-1.0), used as k* in the ITL model (SGLang)",
	})

	// The SGLang completion rate moved to registerSGLangArrivalRateQueries, which
	// runs whether or not this analyzer is enabled. Registering it here as well
	// would panic on the duplicate the moment the throughput analyzer is turned
	// on, so this is deliberately empty of it.
}
