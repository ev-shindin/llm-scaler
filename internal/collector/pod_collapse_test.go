package collector

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

func TestCollapseToPods_MergesDPRanksIntoOneReplica(t *testing.T) {
	ts := time.Now()
	// Two DP ranks on one pod, deliberately asymmetric: rank B has twice the
	// cache and three times the traffic, so every merge rule is observable.
	instances := []domain.ReplicaMetrics{
		{
			PodName: "pod-dp", VariantName: "va-1", ModelID: "m", Namespace: "ns",
			NumGpuBlocks: 1000, BlockSize: 16, TotalKvCapacityTokens: 16000, TokensInUse: 4000,
			KvCacheUsage: 0.25, KvUsageInstant: 0.25, QueueLength: 2,
			RequestRate: 1, GenerationTokenRate: 100,
			AvgInputTokens: 100, AvgOutputTokens: 20, AvgITL: 0.02, AvgServiceTime: 10, PrefixCacheHitRate: 0.4,
			Metadata: &domain.ReplicaMetricsMetadata{CollectedAt: ts, Age: 5 * time.Second, FreshnessStatus: "fresh"},
		},
		{
			PodName: "pod-dp", VariantName: "va-1", ModelID: "m", Namespace: "ns",
			NumGpuBlocks: 2000, BlockSize: 16, TotalKvCapacityTokens: 32000, TokensInUse: 32000,
			KvCacheUsage: 1.0, KvUsageInstant: 1.0, QueueLength: 5,
			RequestRate: 3, GenerationTokenRate: 300,
			AvgInputTokens: 200, AvgOutputTokens: 40, AvgITL: 0.06, AvgServiceTime: 30, PrefixCacheHitRate: 0.8,
			Metadata: &domain.ReplicaMetricsMetadata{CollectedAt: ts, Age: 30 * time.Second, FreshnessStatus: "stale"},
		},
	}

	pods := collapseToPods(instances)

	require.Len(t, pods, 1, "both ranks belong to one pod, so they must produce one replica")
	pod := pods[0]

	assert.Equal(t, "pod-dp", pod.PodName)
	assert.Equal(t, "va-1", pod.VariantName)

	// Extensive quantities add up.
	assert.Equal(t, int64(3000), pod.NumGpuBlocks)
	assert.Equal(t, int64(48000), pod.TotalKvCapacityTokens)
	assert.Equal(t, int64(36000), pod.TokensInUse)
	assert.Equal(t, 7, pod.QueueLength)
	assert.Equal(t, 4.0, pod.RequestRate)
	assert.Equal(t, 400.0, pod.GenerationTokenRate)
	assert.Equal(t, pod.NumGpuBlocks*pod.BlockSize, pod.TotalKvCapacityTokens,
		"blocks x block size must still describe the merged capacity")

	// Utilization is tokens over capacity, not the mean of the two fractions
	// (which would be 0.625 and would understate a pod that is three quarters full).
	assert.InDelta(t, 0.75, pod.KvCacheUsage, 1e-9)
	assert.InDelta(t, 0.75, pod.KvUsageInstant, 1e-9)

	// Request shape is weighted by traffic: rank B serves 3 of every 4 requests.
	assert.InDelta(t, 175.0, pod.AvgInputTokens, 1e-9)
	assert.InDelta(t, 35.0, pod.AvgOutputTokens, 1e-9)
	assert.InDelta(t, 0.05, pod.AvgITL, 1e-9)
	// Weighted by request rate like AvgITL, and asymmetric on purpose: a plain
	// mean would give 20, so 25 is what distinguishes the weighting from an
	// average. (1x10 + 3x30) / 4.
	assert.InDelta(t, 25.0, pod.AvgServiceTime, 1e-9)
	assert.InDelta(t, 0.7, pod.PrefixCacheHitRate, 1e-9)

	// A pod is only as trustworthy as its stalest rank.
	require.NotNil(t, pod.Metadata)
	assert.Equal(t, "stale", pod.Metadata.FreshnessStatus)
	assert.Equal(t, 30*time.Second, pod.Metadata.Age)
}

// TestCollectReplicaMetrics_DPRanksCollapseToOneReplica drives the collapse
// through the whole collection path: a data-parallel pod is scraped as two
// series on different ports, and must come out as one replica carrying the
// pod's combined capacity — otherwise counting the result would give a DP-rank
// count where every consumer expects a scale-target replica count.
func TestCollectReplicaMetrics_DPRanksCollapseToOneReplica(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	scheme := runtime.NewScheme()
	require.NoError(t, llmdVariantAutoscalingV1alpha1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	ts := time.Now()
	rank := func(port string) map[string]string {
		return namespaceSeriesLabels("test-model", "pod-dp", "10.0.0.1:"+port)
	}
	cacheConfig := func(port string) map[string]string {
		labels := rank(port)
		labels["num_gpu_blocks"] = "1000"
		labels["block_size"] = "16"
		return labels
	}

	mockSource := &mockMetricsSource{
		refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return map[string]*source.MetricResult{
				"kv_cache_usage": {Values: []source.MetricValue{
					{Labels: rank("8000"), Value: 0.5, Timestamp: ts},
					{Labels: rank("8001"), Value: 0.5, Timestamp: ts},
				}},
				"cache_config_info": {Values: []source.MetricValue{
					{Labels: cacheConfig("8000"), Value: 1, Timestamp: ts},
					{Labels: cacheConfig("8001"), Value: 1, Timestamp: ts},
				}},
			}, nil
		},
	}

	collector := NewReplicaMetricsCollector(mockSource, k8sClient, nil, nil, scalerLocator(map[string]string{"pod-dp": "va-1"}))
	results, err := collector.CollectReplicaMetrics(
		context.Background(), "test-model", "test-ns",
		make(map[string]scaletarget.ScaleTargetAccessor),
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
		nil,
	)
	require.NoError(t, err)

	require.Len(t, results, 1, "two DP ranks on one pod are one replica")
	assert.Equal(t, "pod-dp", results[0].PodName)
	assert.Equal(t, int64(32000), results[0].TotalKvCapacityTokens, "the pod's capacity is both ranks'")
	assert.InDelta(t, 0.5, results[0].KvCacheUsage, 1e-9)
}

func TestCollapseToPods_IdlePodFallsBackToPlainMean(t *testing.T) {
	// No traffic anywhere on the pod: there is nothing to weigh the shape ratios
	// by, so they must not collapse to zero.
	instances := []domain.ReplicaMetrics{
		{PodName: "pod-idle", AvgInputTokens: 100, AvgOutputTokens: 10, PrefixCacheHitRate: 0.2},
		{PodName: "pod-idle", AvgInputTokens: 300, AvgOutputTokens: 30, PrefixCacheHitRate: 0.6},
	}

	pods := collapseToPods(instances)

	require.Len(t, pods, 1)
	assert.InDelta(t, 200.0, pods[0].AvgInputTokens, 1e-9)
	assert.InDelta(t, 20.0, pods[0].AvgOutputTokens, 1e-9)
	assert.InDelta(t, 0.4, pods[0].PrefixCacheHitRate, 1e-9)
}

func TestCollapseToPods_NoCapacityFallsBackToPlainMean(t *testing.T) {
	// cache_config_info unavailable: with no capacity to weigh by, the usage
	// fractions average rather than divide by zero.
	instances := []domain.ReplicaMetrics{
		{PodName: "pod-nocap", KvCacheUsage: 0.2, KvUsageInstant: 0.4},
		{PodName: "pod-nocap", KvCacheUsage: 0.6, KvUsageInstant: 0.8},
	}

	pods := collapseToPods(instances)

	require.Len(t, pods, 1)
	assert.InDelta(t, 0.4, pods[0].KvCacheUsage, 1e-9)
	assert.InDelta(t, 0.6, pods[0].KvUsageInstant, 1e-9)
	assert.Zero(t, pods[0].TotalKvCapacityTokens)
}

func TestCollapseToPods_DistinctPodsPassThroughInOrder(t *testing.T) {
	// The common case — one instance per pod — must be left exactly as it was,
	// in input order.
	instances := []domain.ReplicaMetrics{
		{PodName: "pod-b", KvCacheUsage: 0.5, TotalKvCapacityTokens: 100},
		{PodName: "pod-a", KvCacheUsage: 0.9, TotalKvCapacityTokens: 200},
		{PodName: "pod-c", KvCacheUsage: 0.1, TotalKvCapacityTokens: 300},
	}

	pods := collapseToPods(instances)

	assert.Equal(t, instances, pods)
}

func TestCollapseToPods_MixedPodsMergeOnlyWhereShared(t *testing.T) {
	// One DP pod alongside a single-instance pod: the counts must come out as
	// two replicas, not three.
	instances := []domain.ReplicaMetrics{
		{PodName: "pod-dp", TotalKvCapacityTokens: 1000, RequestRate: 1},
		{PodName: "pod-single", TotalKvCapacityTokens: 4000, RequestRate: 7},
		{PodName: "pod-dp", TotalKvCapacityTokens: 1000, RequestRate: 1},
	}

	pods := collapseToPods(instances)

	require.Len(t, pods, 2)
	assert.Equal(t, "pod-dp", pods[0].PodName)
	assert.Equal(t, int64(2000), pods[0].TotalKvCapacityTokens)
	assert.Equal(t, 2.0, pods[0].RequestRate)
	assert.Equal(t, "pod-single", pods[1].PodName)
	assert.Equal(t, int64(4000), pods[1].TotalKvCapacityTokens)
}

// TestMergeMetadata_AgeVerdictBeatsMissing pins that a rank which reported
// NOTHING cannot hide a rank that reported something old.
//
// freshnessSeverity ranks "missing" above both age verdicts, which is right
// within a single row -- there it can only mean no metric was present at all --
// and wrong across a pod's ranks. Merging to "missing" would hand the result to
// StaleOrOlder, which reads "missing" as "never scraped, not old", so a DP pod
// could carry five-minute-old numbers while reporting a status that exempts it
// from every staleness check in the codebase, including the abstain.
func TestMergeMetadata_AgeVerdictBeatsMissing(t *testing.T) {
	ts := time.Now()

	cases := []struct {
		name     string
		statuses []string
		want     string
	}{
		{"stale rank is not hidden by a missing one", []string{"stale", "missing"}, "stale"},
		{"unavailable rank is not hidden by a missing one", []string{"missing", "unavailable"}, "unavailable"},
		{"the worse age verdict wins", []string{"stale", "unavailable", "missing"}, "unavailable"},
		// Unchanged behaviour: with no age verdict at all, "missing" still wins
		// over "fresh" -- this must not make a pod look fresher than it is.
		{"missing still outranks fresh", []string{"fresh", "missing"}, "missing"},
		{"all fresh stays fresh", []string{"fresh", "fresh"}, "fresh"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			group := make([]domain.ReplicaMetrics, 0, len(tc.statuses))
			for _, s := range tc.statuses {
				group = append(group, domain.ReplicaMetrics{
					Metadata: &domain.ReplicaMetricsMetadata{CollectedAt: ts, FreshnessStatus: s},
				})
			}
			merged := mergeMetadata(group)
			require.NotNil(t, merged)
			assert.Equal(t, tc.want, merged.FreshnessStatus)
		})
	}
}
