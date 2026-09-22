package capacity

// ReplicaCapacity holds the per-replica capacity breakdown computed by
// the saturation analyzer: what it measured on the replica, what it learned
// about it, and the throughput reading the floor (signals/floor) prices with.
type ReplicaCapacity struct {
	PodName               string
	VariantName           string
	AcceleratorName       string
	TokensInUse           int64
	TotalKvCapacityTokens int64
	MemoryBoundCapacity   int64    // k1: KV-cache-limited capacity
	ComputeBoundCapacity  int64    // k2: compute/scheduling-limited capacity
	K2Priority            K2Source // how k2 was computed
	EffectiveCapacity     int64    // min(k1, k2)
	// ReplicaDemand is the replica's resident KV tokens — TokensInUse on the main
	// path, kvCacheUsage * effectiveCapacity on the fallback path — plus the
	// role-aware waiting-queue footprint: queueLength * avgInputTokens for
	// prefill replicas, and queueLength * (avgInputTokens + avgOutputTokens) for
	// decode/"both". See the saturation analyzer's waitingQueueDemand.
	ReplicaDemand int64
	// QueueLength is the number of requests waiting in this replica's engine
	// queue, and LocalQueueDemand the residency charge waitingQueueDemand put
	// on them, which ReplicaDemand includes. Carried separately so the
	// throughput model (floor.Estimate) can take the residency charge
	// back out and price the same requests as work to be done instead.
	QueueLength      int
	LocalQueueDemand int64

	// FromWarmPool marks a BRIDGE: a warm pool Pod lent to this variant rather
	// than one of its own replicas. Carried through from the collector so
	// aggregation can put its demand in and keep its capacity out. See
	// domain.ReplicaMetrics.FromWarmPool.
	FromWarmPool bool

	// SaturatedThroughput is the completion rate (requests/s) one replica of
	// this bucket sustains when its queue is saturated, from the history
	// recorded beside k2; 0 when no saturation has been observed for the
	// bucket. Read by floor.Estimate.
	SaturatedThroughput float64
	// SaturatedThroughputSamples is how many readings the window that
	// produced SaturatedThroughput holds, and SaturatedThroughputBorrowed
	// whether that window is a neighbouring bucket's rather than the
	// replica's own (see the saturation analyzer's nearestSaturatedThroughput).
	// The floor orders a
	// replica only on an own window of MinThroughputSamplesToOrder readings;
	// anything less holds.
	SaturatedThroughputSamples  int
	SaturatedThroughputBorrowed bool
}
