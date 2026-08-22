// Package constants provides centralized constant definitions for the autoscaler.
// This file contains metric-related constants (VLLM input metrics, WVA output metrics, and metric label names).
package constants

// VLLM Input Metrics
// These metric names are used to query VLLM (vLLM inference engine) metrics from Prometheus.
// The metrics are emitted by VLLM servers and consumed by the collector to make scaling decisions.
const (
	// VLLMNumRequestRunning tracks the current number of running requests.
	// Used to validate metrics availability.
	VLLMNumRequestRunning = "vllm:num_requests_running"

	// VLLMRequestSuccessTotal tracks the total number of successful requests.
	// Used to calculate arrival rate.
	VLLMRequestSuccessTotal = "vllm:request_success_total"

	// VLLMRequestPromptTokensSum tracks the sum of prompt tokens across all requests.
	// Used with VLLMRequestPromptTokensCount to calculate average output tokens.
	VLLMRequestPromptTokensSum = "vllm:request_prompt_tokens_sum"

	// VLLMRequestPromptTokensCount tracks the count of requests for token generation.
	// Used with VLLMRequestPromptTokensSum to calculate average output tokens.
	VLLMRequestPromptTokensCount = "vllm:request_prompt_tokens_count"

	// VLLMRequestGenerationTokensSum tracks the sum of generated tokens across all requests.
	// Used with VLLMRequestGenerationTokensCount to calculate average output tokens.
	VLLMRequestGenerationTokensSum = "vllm:request_generation_tokens_sum"

	// VLLMRequestGenerationTokensCount tracks the count of requests for token generation.
	// Used with VLLMRequestGenerationTokensSum to calculate average output tokens.
	VLLMRequestGenerationTokensCount = "vllm:request_generation_tokens_count"

	// VLLMTimeToFirstTokenSecondsSum tracks the sum of TTFT (Time To First Token) across all requests.
	// Used with VLLMTimeToFirstTokenSecondsCount to calculate TTFT.
	VLLMTimeToFirstTokenSecondsSum = "vllm:time_to_first_token_seconds_sum"

	// VLLMTimeToFirstTokenSecondsCount tracks the count of requests for TTFT.
	// Used with VLLMTimeToFirstTokenSecondsSum to calculate TTFT.
	VLLMTimeToFirstTokenSecondsCount = "vllm:time_to_first_token_seconds_count"

	// VLLMTimePerOutputTokenSecondsSum tracks the sum of time per output token across all requests.
	// Used with VLLMTimePerOutputTokenSecondsCount to calculate ITL (Inter-Token Latency).
	VLLMTimePerOutputTokenSecondsSum = "vllm:time_per_output_token_seconds_sum"

	// VLLMTimePerOutputTokenSecondsCount tracks the count of requests for time per output token.
	// Used with VLLMTimePerOutputTokenSecondsSum to calculate ITL (Inter-Token Latency).
	VLLMTimePerOutputTokenSecondsCount = "vllm:time_per_output_token_seconds_count"

	// VLLMKvCacheUsagePerc tracks the KV cache utilization as a percentage (0.0-1.0).
	// Used by saturation analyzer to detect KV cache saturation and prevent OOM errors.
	VLLMKvCacheUsagePerc = "vllm:kv_cache_usage_perc"

	// VLLMNumRequestsWaiting tracks the number of requests waiting in the queue.
	// Used by saturation analyzer to detect request queue saturation.
	VLLMNumRequestsWaiting = "vllm:num_requests_waiting"

	// VLLMCacheConfigInfo is an info-style gauge that exposes KV cache configuration as labels.
	// Labels include num_gpu_blocks, block_size, cache_dtype, etc.
	// Value is always 1.0. Used by Saturation Analyzer V2 for token capacity computation.
	VLLMCacheConfigInfo = "vllm:cache_config_info"

	// VLLMPrefixCacheHits is a counter of prefix cache block hits.
	// Used with VLLMPrefixCacheQueries to compute prefix cache hit rate.
	VLLMPrefixCacheHits = "vllm:prefix_cache_hits"

	// VLLMPrefixCacheQueries is a counter of prefix cache block queries.
	// Used with VLLMPrefixCacheHits to compute prefix cache hit rate.
	VLLMPrefixCacheQueries = "vllm:prefix_cache_queries"
)

// SGLang Input Metrics
// These metric names are used to query SGLang inference engine metrics from Prometheus.
// Names and types were taken from SGLang's metrics collector
// (python/sglang/srt/observability/metrics_collector.py). SGLang labels its metrics
// with model_name, matching the label WVA filters on. See docs/proposals/sglang-backend.md.
const (
	// SGLangNumRunningReqs is the number of running requests (gauge).
	// SGLang equivalent of vllm:num_requests_running.
	SGLangNumRunningReqs = "sglang:num_running_reqs"

	// SGLangNumQueueReqs is the number of requests in the waiting queue (gauge).
	// SGLang equivalent of vllm:num_requests_waiting.
	SGLangNumQueueReqs = "sglang:num_queue_reqs"

	// SGLangTokenUsage is the KV-cache token-pool utilization as a fraction 0.0-1.0 (gauge).
	// SGLang equivalent of vllm:kv_cache_usage_perc.
	SGLangTokenUsage = "sglang:token_usage"

	// SGLangMaxTotalNumTokens is the total KV-cache token capacity (gauge).
	// SGLang exposes capacity directly, unlike vLLM which derives it from
	// vllm:cache_config_info (num_gpu_blocks x block_size).
	SGLangMaxTotalNumTokens = "sglang:max_total_num_tokens"

	// SGLangTimeToFirstTokenSecondsSum is the sum part of the TTFT histogram.
	// SGLang equivalent of vllm:time_to_first_token_seconds_sum.
	SGLangTimeToFirstTokenSecondsSum = "sglang:time_to_first_token_seconds_sum"

	// SGLangTimeToFirstTokenSecondsCount is the count part of the TTFT histogram.
	// SGLang equivalent of vllm:time_to_first_token_seconds_count.
	SGLangTimeToFirstTokenSecondsCount = "sglang:time_to_first_token_seconds_count"

	// SGLangInterTokenLatencySecondsSum is the sum part of the inter-token-latency histogram.
	// SGLang equivalent of vllm:inter_token_latency_seconds_sum.
	SGLangInterTokenLatencySecondsSum = "sglang:inter_token_latency_seconds_sum"

	// SGLangInterTokenLatencySecondsCount is the count part of the inter-token-latency histogram.
	// SGLang equivalent of vllm:inter_token_latency_seconds_count.
	SGLangInterTokenLatencySecondsCount = "sglang:inter_token_latency_seconds_count"

	// SGLangPromptTokensHistogramSum is the sum part of the prompt-token histogram.
	// SGLang equivalent of vllm:request_prompt_tokens_sum.
	SGLangPromptTokensHistogramSum = "sglang:prompt_tokens_histogram_sum"

	// SGLangPromptTokensHistogramCount is the count part of the prompt-token histogram.
	// SGLang equivalent of vllm:request_prompt_tokens_count.
	SGLangPromptTokensHistogramCount = "sglang:prompt_tokens_histogram_count"

	// SGLangGenerationTokensHistogramSum is the sum part of the generation-token histogram.
	// SGLang equivalent of vllm:request_generation_tokens_sum.
	SGLangGenerationTokensHistogramSum = "sglang:generation_tokens_histogram_sum"

	// SGLangGenerationTokensHistogramCount is the count part of the generation-token histogram.
	// SGLang equivalent of vllm:request_generation_tokens_count.
	SGLangGenerationTokensHistogramCount = "sglang:generation_tokens_histogram_count"

	// SGLangCachedTokensTotal is a counter of prompt tokens served from the prefix cache.
	// Used with SGLangPromptTokensTotal to compute the prefix cache hit rate, the
	// unit-safe analog of vllm:prefix_cache_hits / vllm:prefix_cache_queries.
	SGLangCachedTokensTotal = "sglang:cached_tokens_total"

	// SGLangPromptTokensTotal is a counter of total prompt tokens.
	SGLangPromptTokensTotal = "sglang:prompt_tokens_total"

	// SGLangNumRequestsTotal is a counter of received requests.
	// SGLang equivalent of vllm:request_success_total (used for scale-to-zero).
	SGLangNumRequestsTotal = "sglang:num_requests_total"
)

// llm-d Inference Scheduler Flow Control Metrics
// These metrics come from the Gateway API Inference Extension EPP (Endpoint Picker)
// flow control layer, not from vLLM pods. They are model-scoped (not per-pod).
//
// TODO(#2309): These metrics currently lack a namespace label upstream.
// If the same model and inference pool names exist in different namespaces,
// the metrics will collide. See gateway-api-inference-extension issue #2309.
const (
	// EPPFlowControlQueueSize is the number of requests queued in the inference
	// scheduler's flow control layer. This is the current name for the family
	// below: llm-d's EPP exports both and marks the inference_extension_ one
	// "[Deprecated: Use llm_d_epp_flow_control_queue_size]". Prefer this one and
	// keep the deprecated name only as a fallback.
	// Labels: fairness_id, priority, inference_pool, model_name, target_model_name
	// Note: no namespace label — see TODO(#2309) above.
	EPPFlowControlQueueSize = "llm_d_epp_flow_control_queue_size"

	// SchedulerFlowControlQueueSize is the deprecated alias of
	// EPPFlowControlQueueSize. Upstream gateway-api-inference-extension still
	// emits only this name (and without the inference_pool/model_name/
	// target_model_name labels llm-d's EPP adds), so it remains the fallback.
	// Labels: fairness_id, priority, inference_pool, model_name, target_model_name
	// Note: no namespace label — see TODO(#2309) above.
	SchedulerFlowControlQueueSize = "inference_extension_flow_control_queue_size"

	// SchedulerFlowControlQueueBytes is the total bytes of request bodies queued
	// in the inference scheduler's flow control layer.
	// Labels: fairness_id, priority, inference_pool, model_name, target_model_name
	// Note: no namespace label — see TODO(#2309) above.
	SchedulerFlowControlQueueBytes = "inference_extension_flow_control_queue_bytes"
)

// WVA Output Metrics
// These metric names are used to emit WVA (Workload Variant Autoscaler) metrics to Prometheus.
// The metrics expose scaling decisions and current state for monitoring and alerting.
const (
	// WVAReplicaScalingTotal is a counter that tracks the total number of scaling operations.
	// Labels: variant_name, namespace, direction (up/down), reason, accelerator_type
	WVAReplicaScalingTotal = "wva_replica_scaling_total"

	// WVADesiredReplicas is a gauge that tracks the desired number of replicas.
	// Labels: variant_name, namespace, accelerator_type
	WVADesiredReplicas = "wva_desired_replicas"

	// WVAUnattributedGPUs counts GPUs held by variants whose accelerator type
	// could not be resolved. Such usage cannot be charged to any accelerator
	// pool, so every pool over-states how much is free by this amount and the
	// budget check lets wakes through that should have been refused. It is
	// exported because the condition is otherwise entirely silent — nothing
	// errors, capacity simply looks larger than it is.
	WVAUnattributedGPUs = "wva_unattributed_gpus"

	// WVACurrentReplicas is a gauge that tracks the current number of replicas.
	// Labels: variant_name, namespace, accelerator_type
	WVACurrentReplicas = "wva_current_replicas"

	// WVADesiredRatio is a gauge that tracks the ratio of desired to current replicas.
	// Labels: variant_name, namespace, accelerator_type
	WVADesiredRatio = "wva_desired_ratio"

	// WVAOptimizationDurationSeconds is a histogram that tracks the duration of each optimization cycle.
	// Labels: status (success, error)
	WVAOptimizationDurationSeconds = "wva_optimization_duration_seconds"

	// WVAModelsProcessed is a gauge that tracks the number of models processed in the last optimization cycle.
	WVAModelsProcessed = "wva_models_processed"

	// WVADecisionsLimitedTotal is a counter that tracks the total number of decisions limited by the limiter.
	// Labels: variant_name, namespace, limiter_name
	WVADecisionsLimitedTotal = "wva_decisions_limited_total"

	// WVAGpuDiscoveryUp is a gauge that indicates whether GPU discovery is on or off.
	WVAGpuDiscoveryUp = "wva_gpu_discovery_up"

	// WVANodeAccessDenied is 1 while a configured physical limiter cannot read
	// nodes, and 0 when it can.
	//
	// This is the one combination that fails silently. A GPU-aware limiter
	// allocates out of per-accelerator pools, so a variant whose accelerator it
	// cannot resolve is charged to no pool, receives no budget, and never scales
	// up — with no error anywhere, because an unresolved accelerator is a normal
	// state when no limiter is configured. Node permission is optional right up
	// until someone turns the limiter on, and then it is load-bearing.
	WVANodeAccessDenied = "wva_node_access_denied"

	// WVAScaleFromZeroQueueFallbackActive is 1 while the scale-from-zero engine is
	// reading the EPP flow-control queue from Prometheus because the direct EPP
	// scrape is failing, and 0 while the direct scrape works. A sustained 1 means
	// wakes still happen but are slower and bounded by the Prometheus scrape
	// interval, and that the EPP metrics path (token, RBAC, NetworkPolicy) is
	// broken and should be fixed.
	WVAScaleFromZeroQueueFallbackActive = "wva_scale_from_zero_queue_fallback_active"

	// WVAAvailableGpus is a gauge that tracks the number of currently available GPUs. If wva_gpu_discovery_up is 1, it shows
	// the number of currently available GPUs. If wva_gpu_discovery_up is 0, it shows the number
	// of GPUs that were available at the last successful discovery.
	// Labels: accelerator_type
	WVAAvailableGpus = "wva_available_gpus"

	// WVAEnforcerModificationsTotal is a counter that tracks the total number of decision modifications made by the enforcer.
	// Labels: policy_type
	WVAEnforcerModificationsTotal = "wva_enforcer_modifications_total"

	// WVAOptimizerActive is a gauge that is 0 when an optimizer is inactive, and 1 when it's active.
	// Labels: optimizer_name
	WVAOptimizerActive = "wva_optimizer_active"
	// WVAErrorsTotal is a counter that tracks the total number of errors by component.
	// Labels: component, error_type
	WVAErrorsTotal = "wva_errors_total"
	// WVAAnalyzerDemand is a gauge exposing each analyzer's demand D (per model,
	// per role). Labels: analyzer_name, namespace, model_name, role.
	WVAAnalyzerDemand = "wva_analyzer_demand"
	// WVAAnalyzerTarget is a gauge exposing each analyzer's per-replica target P
	// (per variant). Labels: analyzer_name, namespace, model_name, variant_name.
	WVAAnalyzerTarget = "wva_analyzer_target"
	// WVAConfigInfo is an info-style gauge that exposes WVA configuration as labels.
	// Labels: analyzer_name, limiter_enabled, scale_to_zero_enabled
	WVAConfigInfo = "wva_config_info"

	// WVAConfigOptimizationIntervalSeconds is a gauge that tracks the optimization interval in seconds.
	WVAConfigOptimizationIntervalSeconds = "wva_config_optimization_interval_seconds"
	// WVAMetricsCollectionDurationSeconds is a histogram that tracks the duration of metrics collection operations.
	// Labels: query_type
	WVAMetricsCollectionDurationSeconds = "wva_metrics_collection_duration_seconds"

	// WVAMetricsCollectionErrorsTotal is a counter that tracks the total number of metrics collection errors.
	// Labels: query_type, reason
	WVAMetricsCollectionErrorsTotal = "wva_metrics_collection_errors_total"

	// WVAMetricsPodsDiscovered is a gauge that tracks the number of pods discovered for metrics collection.
	// Labels: namespace
	WVAMetricsPodsDiscovered = "wva_metrics_pods_discovered"

	// WVAMetricsFreshnessStatus is a gauge that tracks the freshness status of metrics for each variant.
	// Labels: variant_name, status
	WVAMetricsFreshnessStatus = "wva_metrics_freshness_status"

	// WVASaturationUtilization is a gauge that tracks per-variant utilization ratio (0.0-1.0).
	// Labels: variant_name, namespace, model_name, accelerator_type
	WVASaturationUtilization = "wva_saturation_utilization"

	// WVASpareCapacity is a gauge that tracks spare capacity; >0 means scale-down
	// headroom (per-role for P/D-disaggregated models, model-level otherwise).
	// The value is a token surplus, carrying unit="continuous".
	// Labels: variant_name, namespace, model_name, unit
	WVASpareCapacity = "wva_spare_capacity"

	// WVARequiredCapacity is a gauge that tracks required capacity; >0 means scale-up
	// needed (per-role for P/D-disaggregated models, model-level otherwise).
	// The value is a token demand, carrying unit="continuous".
	// Labels: variant_name, namespace, model_name, unit
	WVARequiredCapacity = "wva_required_capacity"

	// WVAKvCacheTokensUsed is a gauge that tracks total KV cache tokens currently in use per variant.
	// Labels: variant_name, namespace, model_name
	WVAKvCacheTokensUsed = "wva_kv_cache_tokens_used"

	// WVAKvCacheTokensCapacity is a gauge that tracks total KV cache token capacity per variant.
	// Labels: variant_name, namespace, model_name
	WVAKvCacheTokensCapacity = "wva_kv_cache_tokens_capacity"

	// WVASaturationMetricsUp is a per-VA freshness signal for the five
	// saturation/capacity gauges above. Set to 1.0 in cycles where the
	// optimizer produced a fresh decision for the variant (i.e. the other
	// gauges were just refreshed), and 0.0 in cycles where the analyzer was
	// aware of the variant but no fresh decision was emitted. Lets
	// dashboards distinguish "the system says utilization is X" from "the
	// system has not updated utilization in N minutes and X is the stalest
	// sample" without relying on Prometheus' 5-minute staleness marker.
	// Labels: variant_name, namespace
	WVASaturationMetricsUp = "wva_saturation_metrics_up"

	// WVAPodMappingMissTotal is a counter that tracks pods whose metrics could not be
	// attributed to a managed scaler: the walk up their ownerReferences ended
	// without reaching one. Makes the otherwise-silent skip visible.
	// Labels: namespace, reason
	WVAPodMappingMissTotal = "wva_pod_mapping_miss_total"

	// WVAUnmeasuredQueue is a gauge carrying the number of requests queued for a
	// model that WVA has NO attributed replica to act on.
	//
	// It separates two states the logs used to render identically. "No saturation
	// metrics available, skipping analysis" is the correct and quiet answer for a
	// model that is scaled to zero and idle. It is an emergency for a model that
	// is serving traffic through pods WVA cannot attribute — an FMA topology
	// whose launchers are unscraped, a PodMonitor that selects by a port the pods
	// do not declare, a workload whose ownerReferences reach no scale target.
	// Both produced the same line, so the second was invisible.
	//
	// Non-zero means: requests are queued, and the autoscaler is not going to do
	// anything about them. Sourced from the model-level scheduler flow-control
	// queue, which comes from EPP and is therefore independent of whether any
	// engine pod is scraped.
	//
	// Labels: namespace, model_name
	WVAUnmeasuredQueue = "wva_unmeasured_queue"

	// WVAVariantAtMaxReplicas is a gauge, 1 while a variant's target sits on its
	// own maxReplicas ceiling and 0 otherwise.
	//
	// Distinct from wva_decisions_limited_total, which means GPUs ran out. This
	// one means the operator's own ceiling is the binding constraint, and the
	// remedy is different: raise maxReplicas rather than add accelerators.
	// Conflating them would raise a spurious ResourceConstrained warning for a
	// variant that is simply honouring the bound it was given.
	//
	// Worth alerting on only in combination: at max AND utilization still above
	// the scale-up threshold means demand is going unserved. At max on its own
	// is the normal state of a variant sized exactly right.
	//
	// Kept as its own gauge rather than folded into WVAModelScalingBlocked, which
	// is the reason-labeled metric every other "why isn't this where it wants to
	// be" condition goes on. The two have different SCOPES: this one is per
	// variant, and the reason metric is per model precisely so it can be joined
	// against EPP series and so it can answer "can this model reach zero", a
	// question no single variant can. Adding variant_name there would break both.
	//
	// If a SECOND variant-scoped condition ever appears, convert this into
	// wva_variant_scaling_blocked{variant_name, reason} at that point — the
	// argument in docs/proposals/scale-from-zero-missing-signal.md §4 applies to
	// every scope, but a reason enum with one member is ceremony, not design.
	WVAVariantAtMaxReplicas = "wva_variant_at_max_replicas"

	// WVAModelScalingBlocked is a gauge, present and 1 for each reason a model
	// cannot reach the scaling state its configuration implies.
	//
	// Deliberately one metric with a `reason` label rather than a gauge per
	// condition. The enforcer alone refuses to park a model for three separate
	// reasons and logs each at V(DEBUG), so a metric per condition ends as a
	// metric per discovery — each with its own name, its own alert, and its own
	// entry in wvaMetricNames. This is the same shape wva_decisions_limited_total
	// already uses for `limited_by`, and it is what a status condition would be if
	// WVA owned an API object to write one on: it synthesizes VariantAutoscaling
	// from ScaledObjects, and a ScaledObject's status belongs to KEDA.
	//
	// Presence is the signal: a series exists only while its reason holds, so
	// every emission must clear the model's stale series first (see
	// SetModelScalingBlocked) or a resolved condition alerts forever. Several
	// reasons can hold at once, which emits several series — that is correct, and
	// more useful than picking a winner.
	//
	// Labels: namespace, model_name, reason
	WVAModelScalingBlocked = "wva_model_scaling_blocked"

	// WVAModelReplicas is a gauge carrying the total replicas currently serving a
	// model, summed across its variants.
	//
	// It exists to make one alert expressible. The failure an operator cares about
	// is "my model is parked and requests are being refused", whose two halves are
	// wva_current_replicas and EPP's llm_d_epp_request_error_total — and they
	// cannot be joined. wva_current_replicas is per VARIANT and carries
	// variant_name/namespace/accelerator_type with no model_name; EPP's counter
	// carries model_name and target_model_name with no namespace. PromQL can
	// rewrite a label, not derive a model from a variant name, and nothing in
	// Prometheus knows that mapping. WVA is the one component that does.
	//
	// So this is the join key, not a convenience: model_name matches EPP's, and
	// the sum answers "is anything serving this model at all", which is the
	// question a parked model poses. Do NOT add accelerator_type or variant_name —
	// that would put the series back on the per-variant side of the join it exists
	// to bridge.
	//
	// Labels: namespace, model_name
	WVAModelReplicas = "wva_model_replicas"

	// WVAScaleFromZeroWakeSeconds is how long a wake-from-zero took: from the
	// first activation WVA published to the model being observed serving.
	//
	// The distribution is the point, not the average. On FMA the same code path
	// takes roughly 2s when it binds a reusable sleeping instance and roughly 50s
	// when it has to build one, so a bimodal histogram is what a working warm pool
	// looks like and a shift toward the slow mode is how its regression shows up.
	// Nothing else would report that: the wake still succeeds, just slowly.
	WVAScaleFromZeroWakeSeconds = "wva_scale_from_zero_wake_seconds"

	// WVAWarmPoolFreePods is how many warm-pool Pods can serve a wake right
	// now: every instance asleep, none loading. Read against the pool's
	// sleepMinSize, which is a floor on THIS number -- a Pod holding eight
	// sleeping models is one unit of reserve, not eight, because it can serve
	// one wake.
	//
	// Labels: pool
	WVAWarmPoolFreePods = "wva_warmpool_free_pods"

	// WVAWarmPoolBorrowTotal counts attempts to cover a scale-up from the pool,
	// by outcome: hit, blocked or miss.
	//
	// The outcomes name different faults and a hit rate alone shows neither. A
	// MISS means the model was resident nowhere, so the warm set is wrong. A
	// BLOCK means it was resident but every holding Pod was already serving, so
	// the reserve is too small. One says raise the warm set, the other says
	// raise sleepMinSize.
	//
	// Labels: exported_namespace, model_name, outcome
	WVAWarmPoolBorrowTotal = "wva_warmpool_borrow_total"

	// WVAWarmPoolBridgeSeconds is how long a borrowed Pod served before the
	// ordinary replicas took over.
	//
	// This is the honest measure of what the pool is worth: it should track the
	// ordinary start time, measured at ~33-37s on this cluster. Sitting at
	// maxHoldSeconds instead means ordinary scale-ups are failing and the pool
	// is masking it -- which nothing else would report, because the requests
	// are being served.
	//
	// Labels: exported_namespace, model_name
	WVAWarmPoolBridgeSeconds = "wva_warmpool_bridge_seconds"
)

// Reasons a model cannot reach the scaling state its configuration implies
// (values for the `reason` label of WVAModelScalingBlocked).
//
// Each names a CONTRADICTION, not a setting. Scale-to-zero disabled alongside a
// variant floor is a consistent configuration and reports nothing; it is the half
// that permits zero while the other half forbids it that leaves an operator
// believing something the cluster will never do.
const (
	// ScalingBlockedVariantFloor indicates scale-to-zero is enabled for the model
	// but some variant declares minReplicas > 0. A model is at zero only when
	// nothing serves it, so one variant with a floor keeps the whole model up and
	// the scale-to-zero setting is inert — idle accelerators, billed indefinitely,
	// with no symptom to notice because the model is up and serving exactly as
	// every other metric says it should be.
	ScalingBlockedVariantFloor = "variant-floor"

	// ScalingBlockedPolicyForbidsZero indicates every variant permits zero but the
	// model's resolved policy disables scale-to-zero, so the bounds are inert.
	//
	// A valid configuration, reported because the operator's expectation is wrong
	// and nothing else will tell them. It matters most with a single variant,
	// where minReplicas: 0 reads exactly like a deliberate request to park.
	ScalingBlockedPolicyForbidsZero = "policy-forbids-zero"

	// ScalingBlockedEngineUnsupported indicates the model runs MORE THAN ONE
	// inference engine, so no single request counter measures its idleness.
	//
	// vLLM and SGLang are each supported alone: the enforcer asks for the counter
	// matching the detected engine (vllm:request_success_total or
	// sglang:num_requests_total). A model running both would need them summed, and
	// asking for one would see only part of its traffic — possibly parking a model
	// still serving through the other engine. That is refused rather than guessed.
	//
	// This reason previously meant "not vLLM", when the enforcer asked for the vLLM
	// counter whatever the model ran. It is now the narrower, genuinely ambiguous
	// case.
	ScalingBlockedEngineUnsupported = "engine-unsupported"

	// ScalingBlockedActivationRetention indicates the model was recently woken from
	// zero and is being held up for its retention period.
	//
	// TRANSIENT, unlike the other policy reasons: it clears itself once the hold
	// lapses, and it is not an error or a misconfiguration. It is reported because
	// it is otherwise indistinguishable from a model that simply will not park —
	// the enforcement path declines for several reasons and every one of them logs
	// only at V(DEBUG), so at the default verbosity an operator sees a model
	// sitting at one replica with no explanation anywhere.
	//
	// The hold exists because a just-woken model has served nothing yet: the
	// request that woke it is still queued in EPP while the pod pulls and loads,
	// so the idle counter reads zero for exactly the model that has demand waiting
	// on it. Without the hold the wake is undone before it can serve the request
	// that asked for it.
	//
	// Deliberately NOT alerted on — WVAModelWillNotScaleToZero matches only
	// variant-floor and policy-forbids-zero, which are standing configuration
	// contradictions rather than a few minutes of expected behaviour.
	ScalingBlockedActivationRetention = "activation-retention"

	// ScalingBlockedNoWakeSignal indicates EPP exports no flow-control queue at
	// all, so a model parked at zero has nothing that can wake it.
	//
	// Absence of the family, not a queue reading zero — an idle queue reports 0
	// and a queue that does not exist reports nothing, and conflating those is how
	// this failure hid for a week. The usual cause is an EPP pod older than the
	// ConfigMap that enabled flowControl: EPP reads --config-file once at startup.
	ScalingBlockedNoWakeSignal = "no-wake-signal"
)

// Reason ownership. Two engines write WVAModelScalingBlocked — the steady-state
// enforcer and the scale-from-zero loop — so each declares the reasons it owns
// and clears only those. Without this a 10Hz producer and a per-interval producer
// would take turns deleting each other's series.
var (
	// ScalingBlockedReasonsPolicy are decided by the steady-state enforcer, from
	// configuration reconciled against discovered replica bounds.
	ScalingBlockedReasonsPolicy = []string{
		ScalingBlockedVariantFloor,
		ScalingBlockedPolicyForbidsZero,
		ScalingBlockedEngineUnsupported,
		ScalingBlockedActivationRetention,
	}

	// ScalingBlockedReasonsWake are decided by the scale-from-zero loop, from
	// what EPP actually exports.
	ScalingBlockedReasonsWake = []string{
		ScalingBlockedNoWakeSignal,
	}
)

// Pod-mapping miss reasons (values for the `reason` label of WVAPodMappingMissTotal).
const (
	// PodMappingMissUnresolved indicates a scraped pod resolved to no managed scaler:
	// the locator found no owning ScaledObject above it. A pod owned by something
	// else entirely — an FMA launcher, owned by a LauncherConfig — counts here.
	PodMappingMissUnresolved = "unresolved"

	// PodMappingMissUnboundLauncher indicates an FMA server-providing pod with no
	// current binding: a launcher carrying no dual-pods pairing label.
	//
	// Expected and benign. An unbound launcher is a warm spare, serving nothing,
	// and declining to attribute it is correct. It is separated from
	// "unresolved" so a pool of spares — which can be large, and is permanent —
	// does not bury a genuine attribution bug in the same counter.
	//
	// Rarely seen where the shipped PodMonitor is in use, and that is by design:
	// classification happens only for pods that were scraped, and that monitor
	// drops launchers with no bound instance before a target is generated. This
	// reason therefore reports the case where something ELSE scrapes launchers —
	// a monitor with no such rule, or one inherited from another guide — which is
	// exactly when a warm spare could otherwise be mistaken for idle capacity.
	PodMappingMissUnboundLauncher = "unbound_launcher"

	// PodMappingMissPairingUnresolved indicates a launcher that DID declare a
	// pairing, whose partner still did not resolve to a managed scaler.
	//
	// Unlike the reason above this one is worth investigating: the partner pod
	// may be gone, its ownerReferences may reach no ScaledObject, or the two
	// halves may disagree on the model they serve.
	//
	// The proposal called this `foreign_model_instance`, after the third case.
	// Renamed because the collector cannot tell the three apart from here, and
	// naming one of them would repeat the mistake this investigation started
	// with — a log line that asserted "possible pod/pod_name label mismatch" for
	// a condition with several possible causes, and sent readers hunting the
	// wrong one.
	PodMappingMissPairingUnresolved = "pairing_unresolved"

	// PodMappingMissOtherModelVariant indicates a pod that resolved to a real
	// managed scaler which this model's pass does not own.
	//
	// The pass selects series by model_name, so the series says one model while
	// the pod's current pairing leads to another model's variant. An FMA launcher
	// rebound from one model to another produces exactly this: the pod IP and
	// port stay the same, FMA repoints the pairing and model labels at the new
	// requester, and samples from before the rebind are still inside the query
	// window.
	//
	// It is not an error. The row is ignored rather than charged to a variant it
	// does not belong to, which is the safe direction. It is counted because the
	// alternative is silence: the pairing DID resolve, so `pairing_unresolved`
	// never fires, and a rebind would otherwise leave no trace at all while the
	// old model reads slightly small for one query window.
	//
	// Sustained counts mean something else: a launcher whose pairing points at a
	// variant of a model it does not serve, which is the multi-instance case the
	// singular pairing label cannot express.
	PodMappingMissOtherModelVariant = "other_model_variant"
)

// Metric Label Names
// Common label names used across metrics for consistency.
const (
	LabelModelName          = "model_name"
	LabelNamespace          = "namespace"
	LabelComponent          = "component"
	LabelVariantName        = "variant_name"
	LabelDirection          = "direction"
	LabelReason             = "reason"
	LabelAcceleratorVendor  = "accelerator_vendor"
	LabelAcceleratorModel   = "accelerator_model"
	LabelAcceleratorType    = "accelerator_type"
	LabelControllerInstance = "controller_instance"
	LabelRole               = "role"
	LabelStatus             = "status"
	LabelLimiterName        = "limiter_name"
	LabelPolicyType         = "policy_type"
	LabelOptimizerName      = "optimizer_name"
	LabelPool               = "pool"
	// LabelOutcome distinguishes a borrow that hit, one blocked by an exhausted
	// reserve, and one that missed because the model was resident nowhere.
	LabelOutcome            = "outcome"
	LabelErrorType          = "error_type"
	LabelAnalyzerName       = "analyzer_name"
	LabelLimiterEnabled     = "limiter_enabled"
	LabelScaleToZeroEnabled = "scale_to_zero_enabled"
	LabelQueryType          = "query_type"
	// LabelUnit names the unit of a metric value. Applied to
	// wva_required_capacity and wva_spare_capacity, whose values are a
	// "continuous" token magnitude. It is retained as a stable part of those
	// series' identity so existing queries and alerts keep matching.
	LabelUnit = "unit"
)

// Metric Label Values for query_type
// These values are used as the query_type label in metrics collection metrics.
const (
	QueryTypeKVCache      = "kv_cache"
	QueryTypeQueueLength  = "queue_length"
	QueryTypeRequestCount = "request_count"
	QueryTypeCacheConfig  = "cache_config"
	QueryTypeArrivalRate  = "arrival_rate"
)

// Value for the LabelUnit Prometheus label: the metric carries an absolute
// quantity rather than a normalized ratio.
const (
	UnitContinuous = "continuous"
)
