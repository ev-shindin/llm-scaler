package registration

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
)

// Query name constants for type-safe query references.
const (
	// Saturation queries (per-pod peak metrics over time windows)
	QueryKvCacheUsage = "kv_cache_usage"
	QueryQueueLength  = "queue_length"

	// QueryMetricsAge is how old the engine's metrics actually are, in
	// seconds, per instance.
	//
	// It exists because the sample timestamps returned by every other query
	// here cannot answer that. WVA issues INSTANT queries
	// (promAPI.Query(ctx, q, time.Now())), and Prometheus stamps every sample
	// of an instant vector with the EVALUATION time, so subtracting it from
	// the collection time measures WVA's own round-trip -- milliseconds --
	// and never the age of the data. Freshness derived that way reports
	// "fresh" unconditionally, including for a scrape that died minutes ago.
	//
	// timestamp() is the one function that reports the sample's own
	// timestamp rather than the evaluation time, so time() - timestamp(x)
	// is the real age. Taken over the KV-cache metric because that is the
	// engine's liveness signal: it is emitted continuously by a serving
	// replica whether or not it is handling traffic, unlike the histogram
	// rates, which stop advancing on an idle replica that is perfectly
	// healthy.
	QueryMetricsAge = "metrics_age"

	// V2 queries (token-based capacity analysis)
	QueryCacheConfigInfo    = "cache_config_info"
	QueryAvgOutputTokens    = "avg_output_tokens"
	QueryAvgInputTokens     = "avg_input_tokens"
	QueryPrefixCacheHitRate = "prefix_cache_hit_rate"

	// Scheduler flow control queries (model-level, from inference scheduler)
	QuerySchedulerQueueSize  = "scheduler_queue_size"
	QuerySchedulerQueueBytes = "scheduler_queue_bytes"
)

// Per-replica engine queries are registered NAMESPACE-scoped, not model-scoped.
//
// They carry no model_name matcher; instead model_name is part of the grouping
// key, so one execution per namespace returns the series for every model in it
// and the collector partitions them by model_name in Go (see
// collector.filterResultsToModel). Previously each of these ran once per model
// per cycle over an identical namespace filter, so a namespace hosting M models
// paid M times the round trips to fetch the same set of series.
//
// Consequences to preserve when editing a template below:
//
//   - model_name MUST appear in the by()/grouping clause of any query the
//     collector partitions by model, or every model's slice comes back empty.
//   - A series with no model_name label cannot be attributed to a model and is
//     dropped by the collector — exactly what the removed matcher did.
//   - The rest of the grouping key is instance and pod: instance is IP:port,
//     which distinguishes DP ranks sharing a pod, and pod drives the
//     ownerReference walk that resolves the variant.
//   - llm_d_ai_variant is deliberately NOT in the grouping (issue #1263).
//     Neither vLLM nor SGLang emits it — it appears only where an operator
//     relabels the llm-d.ai/variant pod label onto the series — so it was an
//     empty column on every real deployment. Variant identity now comes from
//     the PodLocator owner-walk delivered by PR #1260.

// RegisterSaturationQueries registers queries used by the saturation analyzer.
func RegisterSaturationQueries(sourceRegistry *source.SourceRegistry) {
	registry := sourceRegistry.Get("prometheus").QueryList()

	// KV cache usage per instance (peak over last minute)
	// Uses max_over_time to catch saturation events between scrapes
	// Grouping key and namespace scoping: see the note above RegisterSaturationQueries
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryKvCacheUsage,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (max_over_time(vllm:kv_cache_usage_perc{namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Peak KV cache utilization per instance (0.0-1.0) over last minute",
	})

	// Age of the engine's own metrics per instance, in seconds. See
	// QueryMetricsAge for why this cannot be derived from sample timestamps.
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryMetricsAge,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (time() - timestamp(vllm:kv_cache_usage_perc{namespace="{{.namespace}}"}))`,
		Params:      []string{source.ParamNamespace},
		Description: "Age of the engine's metrics per instance (seconds)",
	})

	// Queue length per instance (peak over last minute)
	// Uses max_over_time to catch burst traffic
	// Grouping key and namespace scoping: see the note above RegisterSaturationQueries
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryQueueLength,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (max_over_time(vllm:num_requests_waiting{namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Peak queue length per instance over last minute",
	})

	// --- V2 queries for token-based capacity analysis ---

	// Cache config info per instance (static labels with block size and GPU blocks count)
	// Uses max to deduplicate when multiple series exist per instance with different label combinations
	// Used by Saturation Analyzer V2 for token capacity computation
	// Preserves instance (IP:port for multi-instance pods), pod (for pod lookup), and the config labels
	//
	// NOTE: vllm:cache_config_info is an info-style metric. Unlike vLLM's regular
	// gauges/counters, it is NOT labeled with model_name — its label set is derived
	// from CacheConfig fields (num_gpu_blocks, block_size, cache_dtype, ...) plus
	// "engine". It therefore cannot be partitioned by model at all: the collector
	// correlates the results to a model's pods by instance key instead, attaching
	// cache config only to instances the KV/queue queries already discovered for
	// that model. Do not add a model_name matcher or grouping label here.
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryCacheConfigInfo,
		Type:        source.QueryTypePromQL,
		Template:    `max by (instance, pod, num_gpu_blocks, block_size) (vllm:cache_config_info{namespace="{{.namespace}}"})`,
		Params:      []string{source.ParamNamespace},
		Description: "KV cache configuration info per instance (num_gpu_blocks and block_size as labels)",
	})

	// Average output (generation) tokens per completed request
	// Used for output-length-dependent k2 estimation
	// Grouping key and namespace scoping: see the note above RegisterSaturationQueries
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryAvgOutputTokens,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (rate(vllm:request_generation_tokens_sum{namespace="{{.namespace}}"}[5m]) / rate(vllm:request_generation_tokens_count{namespace="{{.namespace}}"}[5m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Average output tokens per completed request (5m rate)",
	})

	// Average input (prompt) tokens per completed request
	// Used in k2 derivation formula: k2 = N_max × (I + O/2)
	// Grouping key and namespace scoping: see the note above RegisterSaturationQueries
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryAvgInputTokens,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (rate(vllm:request_prompt_tokens_sum{namespace="{{.namespace}}"}[5m]) / rate(vllm:request_prompt_tokens_count{namespace="{{.namespace}}"}[5m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Average input tokens per completed request (5m rate)",
	})

	// Prefix cache hit rate per instance (5m rate)
	// Used to reduce estimated input token demand for scheduler-queued requests.
	// Returns 0..1 where 1 means all prefix lookups were cache hits.
	// Grouping key and namespace scoping: see the note above RegisterSaturationQueries
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryPrefixCacheHitRate,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (rate(vllm:prefix_cache_hits{namespace="{{.namespace}}"}[5m]) / rate(vllm:prefix_cache_queries{namespace="{{.namespace}}"}[5m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Prefix cache hit rate per instance (0.0-1.0, 5m rate)",
	})

	// --- Scheduler flow control queries (model-level) ---
	// These come from the llm-d inference scheduler, not engine pods.
	//
	// Like the engine queries above they carry no model matcher: both model
	// identity labels stay in the grouping key and the collector picks the
	// model per series — target_model_name when set, else model_name (see
	// eppSeriesModel). That replaces the "or" clause the model-scoped versions
	// used to need, and, since these metrics have no namespace label to scope
	// them by either, one cluster-wide execution per cycle now serves every
	// model the controller manages instead of two per model.
	//
	// TODO(#2309): These metrics currently lack a namespace label in the upstream
	// gateway-api-inference-extension EPP. If the same model name exists in
	// different namespaces, these queries aggregate across all of them. Once the
	// upstream adds a namespace label, they should group by (and be scoped to) it.

	// Number of requests queued in the scheduler's flow control layer.
	//
	// Both families are read, preferring llm-d's. The EPP renamed this metric to
	// the llm_d_epp_ prefix and upstream gateway-api-inference-extension kept the
	// inference_extension_ one, so which exists depends on the EPP build — and
	// PromQL `or` is exactly that preference: the left side where it has series,
	// the right side only where it does not. Reading only the deprecated name left
	// this query empty on a newer EPP, which mattered twice over once the
	// scale-from-zero fallback started reading it (queue_fallback.go): no series
	// means no demand means nothing is ever woken.
	registry.MustRegister(source.QueryTemplate{
		Name: QuerySchedulerQueueSize,
		Type: source.QueryTypePromQL,
		Template: `sum by (model_name, target_model_name) (llm_d_epp_flow_control_queue_size)` +
			` or sum by (model_name, target_model_name) (inference_extension_flow_control_queue_size)`,
		Description: "Requests queued in scheduler flow control, per model",
	})

	// Total bytes of request bodies queued in the scheduler's flow control layer
	registry.MustRegister(source.QueryTemplate{
		Name: QuerySchedulerQueueBytes,
		Type: source.QueryTypePromQL,
		Template: `sum by (model_name, target_model_name) (llm_d_epp_flow_control_queue_bytes)` +
			` or sum by (model_name, target_model_name) (inference_extension_flow_control_queue_bytes)`,
		Description: "Bytes queued in scheduler flow control, per model",
	})

	registerSGLangSaturationQueries(registry)
}

// registerSGLangSaturationQueries registers the SGLang variants of the
// engine-specific saturation queries. The scheduler flow-control queries above
// are engine-agnostic (sourced from EPP) and are not duplicated here.
func registerSGLangSaturationQueries(registry *source.QueryList) {
	// KV-cache token-pool utilization per instance (peak over last minute).
	// sglang:token_usage is the 0.0-1.0 fraction equivalent of vllm:kv_cache_usage_perc.
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryKvCacheUsage,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (max_over_time(sglang:token_usage{namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Peak KV cache utilization per instance (0.0-1.0) over last minute (SGLang)",
	})

	// Age of the engine's own metrics per instance, in seconds (SGLang).
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryMetricsAge,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (time() - timestamp(sglang:token_usage{namespace="{{.namespace}}"}))`,
		Params:      []string{source.ParamNamespace},
		Description: "Age of the engine's metrics per instance (seconds) (SGLang)",
	})

	// Queue length per instance (peak over last minute).
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryQueueLength,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (max_over_time(sglang:num_queue_reqs{namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Peak queue length per instance over last minute (SGLang)",
	})

	// Total KV-cache token capacity per instance.
	//
	// Structural difference from vLLM: SGLang exposes capacity directly via
	// sglang:max_total_num_tokens (a model-labeled gauge), so — unlike
	// vllm:cache_config_info — this one IS partitionable by model_name and
	// returns the capacity as the value, with no num_gpu_blocks/block_size
	// labels. The collector converts this value into TotalKvCapacityTokens
	// directly (see CollectReplicaMetrics).
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryCacheConfigInfo,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (sglang:max_total_num_tokens{namespace="{{.namespace}}"})`,
		Params:      []string{source.ParamNamespace},
		Description: "Total KV cache token capacity per instance (SGLang)",
	})

	// Average output (generation) tokens per completed request (5m rate).
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryAvgOutputTokens,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (rate(sglang:generation_tokens_histogram_sum{namespace="{{.namespace}}"}[5m]) / rate(sglang:generation_tokens_histogram_count{namespace="{{.namespace}}"}[5m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Average output tokens per completed request (5m rate) (SGLang)",
	})

	// Average input (prompt) tokens per completed request (5m rate).
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryAvgInputTokens,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (rate(sglang:prompt_tokens_histogram_sum{namespace="{{.namespace}}"}[5m]) / rate(sglang:prompt_tokens_histogram_count{namespace="{{.namespace}}"}[5m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Average input tokens per completed request (5m rate) (SGLang)",
	})

	// Prefix cache hit rate per instance (5m rate).
	//
	// Structural difference from vLLM: derived from SGLang's token counters
	// (cached prompt tokens / total prompt tokens) rather than hit/query counters.
	// This is unit-safe (0.0-1.0) and parallels the vLLM hits/queries formula.
	// SGLang also exposes sglang:cache_hit_rate directly, but its units are
	// version-dependent (0-1 vs 0-100), so the counter ratio is preferred.
	//
	// Each counter is aggregated with sum by(...) BEFORE the division. The two
	// counters do not share an identical label set — sglang:cached_tokens_total
	// carries an extra cache_source label — so dividing the raw rates would leave
	// the operator with no one-to-one matches and yield an empty vector. Summing
	// each side down to the (model_name, instance, pod) key first drops the
	// differing labels and makes the division well-defined. Both sides must
	// carry model_name for the division to match per model.
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryPrefixCacheHitRate,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (model_name, instance, pod) (rate(sglang:cached_tokens_total{namespace="{{.namespace}}"}[5m])) / sum by (model_name, instance, pod) (rate(sglang:prompt_tokens_total{namespace="{{.namespace}}"}[5m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Prefix cache hit rate per instance (0.0-1.0, 5m rate) (SGLang)",
	})
}
