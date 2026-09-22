package collector

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// podMetricData holds per-pod metric values and timestamps
type podMetricData struct {
	podName        string // Actual pod name for K8s API lookups
	vaName         string // scaler name the pod's ownerReference walk resolved to
	kvUsage        float64
	kvTimestamp    time.Time
	hasKv          bool
	queueLen       int
	queueTimestamp time.Time
	hasQueue       bool
	// V2 fields for token-based capacity analysis
	numGpuBlocks                int64
	blockSize                   int64
	avgOutputTokens             float64
	avgOutputTokensTimestamp    time.Time
	avgInputTokens              float64
	avgInputTokensTimestamp     time.Time
	prefixCacheHitRate          float64
	prefixCacheHitRateTimestamp time.Time
	hasCacheConfig              bool
	cacheConfigTimestamp        time.Time
	// Queueing model fields
	avgITL                  float64
	avgITLTimestamp         time.Time
	avgServiceTime          float64
	avgServiceTimeTimestamp time.Time
	// Throughput analyzer fields
	generationTokenRate float64
	kvUsageInstant      float64
	requestRate         float64
}

// extractPodMetrics reads every series the replica queries returned into one
// podMetricData per engine instance, keyed by buildInstanceKey.
//
// Two kinds of query. The model-scoped ones that DEFINE the fleet (KV cache
// usage, queue length, the token averages, the timings) create an entry for
// an instance they see; a query error on the two saturation series is the
// collection's error. The namespace-wide or supplementary ones (cache config,
// which carries no model label; the throughput analyzer's rates) attach to
// instances already seen and skip the rest, so a foreign model's pod or a
// scrape-skewed row cannot enter this model's fleet. Values are taken as
// scraped, with the validity rule each series needs (finite, in range,
// positive) stated at its block.
func (c *ReplicaMetricsCollector) extractPodMetrics(
	ctx context.Context,
	modelID string,
	namespace string,
	engines []inferenceengine.Engine,
	results map[string]*source.MetricResult,
) (map[string]*podMetricData, error) {
	logger := ctrl.LoggerFrom(ctx)

	// Extract per-pod metrics from results
	podData := make(map[string]*podMetricData)

	// Process KV cache results
	if result := results[registration.QueryKvCacheUsage]; result != nil {
		if result.HasError() {
			return nil, fmt.Errorf("KV cache query failed: %w", result.Error)
		}
		for _, value := range result.Values {
			instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
			if instanceKey == "" {
				continue
			}

			if podData[instanceKey] == nil {
				podData[instanceKey] = &podMetricData{
					podName: podName,
					vaName:  vaName,
				}
			}
			podData[instanceKey].kvUsage = value.Value
			podData[instanceKey].kvTimestamp = value.Timestamp
			podData[instanceKey].hasKv = true

			logger.V(logging.DEBUG).Info("KV cache metric",
				"instanceKey", instanceKey,
				"pod", podName,
				"usage", value.Value,
				"usagePercent", value.Value*100)
		}
	}

	// Process queue length results
	if result := results[registration.QueryQueueLength]; result != nil {
		if result.HasError() {
			return nil, fmt.Errorf("queue length query failed: %w", result.Error)
		}
		for _, value := range result.Values {
			instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
			if instanceKey == "" {
				continue
			}

			if podData[instanceKey] == nil {
				podData[instanceKey] = &podMetricData{
					podName: podName,
					vaName:  vaName,
				}
			}
			podData[instanceKey].queueLen = int(value.Value)
			podData[instanceKey].queueTimestamp = value.Timestamp
			podData[instanceKey].hasQueue = true

			logger.V(logging.DEBUG).Info("Queue metric",
				"instanceKey", instanceKey,
				"pod", podName,
				"queueLength", int(value.Value))
		}
	}

	// Process cache config info results (V2)
	//
	// vllm:cache_config_info has no model_name label (see QueryCacheConfigInfo),
	// so it is queried namespace-wide and may include pods of other models in the
	// same namespace. Attach cache config only to instances already discovered by
	// the model-scoped KV/queue queries above; skip unknown instances so foreign
	// pods are not introduced into this model's metrics (and do not inflate the
	// discovered-pods / freshness counters).
	if result := results[registration.QueryCacheConfigInfo]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, podName, _ := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}

				data := podData[instanceKey]
				if data == nil {
					// Instance not seen by the model-scoped queries: it belongs to a
					// different model (or lacks KV/queue metrics) — not one of ours.
					continue
				}

				// Parse num_gpu_blocks and block_size from string labels
				if blocksStr, ok := value.Labels["num_gpu_blocks"]; ok && blocksStr != "" {
					if blocks, err := strconv.ParseInt(blocksStr, 10, 64); err == nil {
						data.numGpuBlocks = blocks
					}
				}
				if sizeStr, ok := value.Labels["block_size"]; ok && sizeStr != "" {
					if size, err := strconv.ParseInt(sizeStr, 10, 64); err == nil {
						data.blockSize = size
					}
				}
				if data.numGpuBlocks > 0 && data.blockSize > 0 {
					data.hasCacheConfig = true
					data.cacheConfigTimestamp = value.Timestamp
				}

				logger.V(logging.DEBUG).Info("Cache config info metric",
					"instanceKey", instanceKey,
					"pod", podName,
					"numGpuBlocks", data.numGpuBlocks,
					"blockSize", data.blockSize)
			}
		}
	}

	// Process SGLang cache config (structural difference from vLLM).
	//
	// SGLang exposes total KV-cache token capacity directly via
	// sglang:max_total_num_tokens (the value), rather than as
	// num_gpu_blocks/block_size labels. We map the capacity onto the existing
	// numGpuBlocks × blockSize computation by setting blockSize = 1 and
	// numGpuBlocks = capacity, so the downstream TotalKvCapacityTokens math is
	// unchanged. Only runs when an SGLang variant is present for this model.
	// Read through the physical key, which filterResultsToModel leaves alone
	// (cache_config_info is unpartitioned because the vLLM variant has no model
	// identity). The SGLang variant does carry model_name, so it is filtered
	// here.
	if containsEngine(engines, inferenceengine.EngineSGLang) {
		sglangCacheKey := registration.EngineQuery(inferenceengine.EngineSGLang, registration.QueryCacheConfigInfo)
		if result := results[sglangCacheKey]; result != nil && !result.HasError() {
			for _, value := range result.Values {
				if value.Labels[seriesModelLabel] != modelID {
					continue
				}
				instanceKey, podName, _ := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}
				data := podData[instanceKey]
				if data == nil {
					// Not seen by the model-scoped KV/queue queries — skip.
					continue
				}
				capacity := int64(value.Value)
				if capacity > 0 {
					data.numGpuBlocks = capacity
					data.blockSize = 1
					data.hasCacheConfig = true
					data.cacheConfigTimestamp = value.Timestamp
				}

				logger.V(logging.DEBUG).Info("SGLang cache config metric",
					"instanceKey", instanceKey,
					"pod", podName,
					"totalKvCapacityTokens", capacity)
			}
		}
	}

	// Process average output tokens results (V2)
	if result := results[registration.QueryAvgOutputTokens]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}

				if podData[instanceKey] == nil {
					podData[instanceKey] = &podMetricData{
						podName: podName,
						vaName:  vaName,
					}
				}
				// NaN check: rate division by zero produces NaN
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) {
					podData[instanceKey].avgOutputTokens = value.Value
					podData[instanceKey].avgOutputTokensTimestamp = value.Timestamp
				}
			}
		}
	}

	// Process average input tokens results (V2)
	if result := results[registration.QueryAvgInputTokens]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}

				if podData[instanceKey] == nil {
					podData[instanceKey] = &podMetricData{
						podName: podName,
						vaName:  vaName,
					}
				}
				// NaN check: rate division by zero produces NaN
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) {
					podData[instanceKey].avgInputTokens = value.Value
					podData[instanceKey].avgInputTokensTimestamp = value.Timestamp
				}
			}
		}
	}

	// Process prefix cache hit rate results (V2)
	if result := results[registration.QueryPrefixCacheHitRate]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}

				if podData[instanceKey] == nil {
					podData[instanceKey] = &podMetricData{
						podName: podName,
						vaName:  vaName,
					}
				}
				// NaN check: rate division by zero produces NaN when no prefix cache queries
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value >= 0 && value.Value <= 1 {
					podData[instanceKey].prefixCacheHitRate = value.Value
					podData[instanceKey].prefixCacheHitRateTimestamp = value.Timestamp
				}
			}
		}
	}

	// Process average ITL results (seconds)
	if result := results[registration.QueryAvgITL]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}

				if podData[instanceKey] == nil {
					podData[instanceKey] = &podMetricData{
						podName: podName,
						vaName:  vaName,
					}
				}
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value > 0 {
					podData[instanceKey].avgITL = value.Value
					podData[instanceKey].avgITLTimestamp = value.Timestamp

					logger.V(logging.DEBUG).Info("Avg ITL metric",
						"instanceKey", instanceKey,
						"pod", podName,
						"avgITLSeconds", value.Value)
				}
			}
		}
	}

	// Process average service time results (seconds, queue wait excluded)
	if result := results[registration.QueryAvgServiceTime]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, podName, vaName := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}

				if podData[instanceKey] == nil {
					podData[instanceKey] = &podMetricData{
						podName: podName,
						vaName:  vaName,
					}
				}
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value > 0 {
					podData[instanceKey].avgServiceTime = value.Value
					podData[instanceKey].avgServiceTimeTimestamp = value.Timestamp

					logger.V(logging.DEBUG).Info("Avg service time metric",
						"instanceKey", instanceKey,
						"pod", podName,
						"avgServiceTimeSeconds", value.Value)
				}
			}
		}
	}

	// Process generation token rate results (tokens/sec) — throughput analyzer μ_dec^obs
	if result := results[registration.QueryGenerationTokenRate]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, _, _ := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}
				if podData[instanceKey] == nil {
					continue // skip pods the KV/queue queries didn't see (scrape skew)
				}
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value >= 0 {
					podData[instanceKey].generationTokenRate = value.Value
				}
			}
		}
	}

	// Process instantaneous KV usage (k*) results (0.0–1.0) — throughput analyzer k*
	if result := results[registration.QueryKvUsageInstant]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, _, _ := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}
				if podData[instanceKey] == nil {
					continue // skip pods the KV/queue queries didn't see (scrape skew)
				}
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value >= 0 && value.Value <= 1 {
					podData[instanceKey].kvUsageInstant = value.Value
				}
			}
		}
	}

	// Process engine request completion rate (req/s) — throughput analyzer fallback λ_req
	if result := results[registration.QueryRequestRate]; result != nil {
		if !result.HasError() {
			for _, value := range result.Values {
				instanceKey, _, _ := c.buildInstanceKey(ctx, namespace, value.Labels)
				if instanceKey == "" {
					continue
				}
				if podData[instanceKey] == nil {
					continue // skip pods the KV/queue queries didn't see (scrape skew)
				}
				if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value >= 0 {
					podData[instanceKey].requestRate = value.Value
				}
			}
		}
	}

	return podData, nil
}
