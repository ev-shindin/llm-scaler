package saturation_v2

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// The analyzer's capacity-decision logs are a machine-readable contract with
// hack/benchmark/dump_k2_decisions.py, which matches on the message name and
// reads fields by key. Renaming either side turns the report silently empty
// rather than producing an error, so the contract is pinned here: these tests
// fail instead.
//
// k2-decision, replica-capacity-decision and scheduler-queue-demand feed the
// report's table columns. The rest are dumped verbatim to k2_decisions.json,
// where the reason/source field is what a human reads to tell a measured number
// from an estimated one — so those are pinned too.
//
// When a message or field below changes, update MESSAGES and the corresponding
// e.get(...) keys in dump_k2_decisions.py in the same commit.
var logContract = map[string][]string{
	"k2-decision": {
		"modelID", "namespace", "variant", // join keys
		"priority", // the report's Priority column
	},
	"replica-capacity-decision": {
		"modelID", "namespace", "variant", // join keys
		"k1MemoryBound", "k2ComputeBound", "boundBy", // k1/k2/Bound columns
		"tokensInUse", "localQueueDemand", "replicaDemand", // demand columns
	},
	"scheduler-queue-demand": {
		"modelID",         // joins the queue line to the variant's model
		"estimatedTokens", // the report's EPPq column
	},
	// The floor changes a scaling decision when it binds, so the same contract
	// applies: a reader has to be able to tell the floored demand from the
	// demand it replaced, and which of lambda, mu and P produced it.
	"throughput-demand-floor": {
		"modelID", "namespace", "role",
		"demandBeforeFloor", "residentDemand", "flooredTo", // what changed
		"arrivalRate", "backlogRequests", "drainSeconds", "saturatedThroughput", "perReplicaCapacity", "replicasImplied", // and from which terms
		"heldAtFleet", "heldWhy", // and whether the floor was allowed to order on them
	},
	// Prefill's share of the scheduler queue is dropped while prefill has no
	// mu (throughput_floor.go); the line is what explains the gap between
	// scheduler-queue-demand's byRole and RoleDemand.
	"scheduler-queue-prefill-share-dropped": {"modelID", "namespace", "eppQueueSize", "droppedTokens", "prefillDemandBefore", "prefillDemandAfter"},
	// Prefill's demand is held in the no-order/no-release band while decode
	// is saturated (analyzer.go, holdPrefillDemand); the line is what explains
	// a prefill RoleDemand that matches neither its rows nor its floor.
	"prefill-demand-held":             {"modelID", "namespace", "demandBefore", "demandHeld", "holdFloor", "holdCap"},
	"replica-capacity-skipped":        {"modelID", "namespace", "variant", "reason"},
	"replica-capacity-store-fallback": {"modelID", "namespace", "variant", "reason"},
	"variant-capacity-source":         {"modelID", "namespace", "variant", "reason"},
	"zero-replica-capacity-estimate":  {"modelID", "namespace", "variant", "source"},
}

// k2PriorityLabels is the vocabulary a k2 SOURCE is labelled with; the
// report's Priority column renders these four, plus the two diagnostic values
// (k2ReasonObsImplausible, k2ReasonObsDownstream) that precede the tier a
// declined observation fell through to. dump_k2_decisions.py legends all six.
var k2PriorityLabels = map[string]bool{
	"P1-obs": true, "P2-hist": true, "P3-k2": true, "P4-k1": true,
}

// observedCtx returns a context carrying a logger that records everything the
// shipped deployment would emit. The per-replica lines sit at
// V(logging.DEFAULT), which zapr maps to zap level -logging.DEFAULT, so the
// observer core has to be enabled that far down or they are dropped before
// they are recorded.
func observedCtx(t *testing.T) (context.Context, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.Level(-logging.DEFAULT))
	return logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core))), logs
}

// infoOnlyCtx returns a context whose logger keeps Info and discards anything
// more verbose — what the controller emits when started with -v=0.
func infoOnlyCtx() (context.Context, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.InfoLevel)
	return logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core))), logs
}

// requireLogged asserts the message was emitted and carries every field key the
// report reads, returning the first such entry's fields.
func requireLogged(t *testing.T, logs *observer.ObservedLogs, msg string) map[string]any {
	t.Helper()
	entries := logs.FilterMessage(msg).All()
	require.NotEmpty(t, entries, "expected a %q log line; dump_k2_decisions.py parses it", msg)

	fields := entries[0].ContextMap()
	for _, key := range logContract[msg] {
		assert.Contains(t, fields, key,
			"%q must carry field %q; dump_k2_decisions.py reads it by name", msg, key)
	}
	return fields
}

// stringField reads a field the report treats as text, failing rather than
// panicking if it stops being a string.
func stringField(t *testing.T, fields map[string]any, key string) string {
	t.Helper()
	v, ok := fields[key].(string)
	require.True(t, ok, "field %q must be a string, got %T", key, fields[key])
	return v
}

// deploymentParams builds engine params good enough for k2 derivation. Returned
// fresh each call so compatibility is decided by value, the way FindCompatible
// decides it, not by two variants sharing one pointer.
func deploymentParams() *EngineParams {
	return &EngineParams{
		Engine:                    inferenceengine.EngineVLLM,
		BlockSize:                 16,
		MaxNumSeqs:                256,
		EffectiveMaxBatchedTokens: 8192,
	}
}

func TestLogContract_LiveReplicaEmitsCycleFields(t *testing.T) {
	ctx, logs := observedCtx(t)
	analyzer := NewSaturationAnalyzer(NewCapacityKnowledgeStore())

	input := makeAnalyzerInput(
		[]domain.ReplicaMetrics{
			makeReplicaMetrics("pod-1", "variant-a", 5000, 16000, 0, 100, 50),
		},
		[]domain.VariantReplicaState{
			{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		},
	)
	input.SchedulerQueue = &domain.SchedulerQueueMetrics{QueueSize: 3, QueueBytes: 4096}

	_, err := analyzer.Analyze(ctx, input)
	require.NoError(t, err)

	requireLogged(t, logs, "replica-capacity-decision")
	requireLogged(t, logs, "scheduler-queue-demand")

	k2 := requireLogged(t, logs, "k2-decision")
	assert.Contains(t, k2PriorityLabels, k2["priority"],
		"priority must be one of the four labels dump_k2_decisions.py legends")
}

// The observed tier is the one the report cares most about, and the only one
// whose label the analyzer picks from live queue state rather than a fallback.
func TestLogContract_SaturatedQueueReportsObservedTier(t *testing.T) {
	ctx, logs := observedCtx(t)
	analyzer := NewSaturationAnalyzer(NewCapacityKnowledgeStore())

	input := makeAnalyzerInput(
		// QueueLength 6 >= the fixture's QueueLengthThreshold of 5.
		[]domain.ReplicaMetrics{
			makeReplicaMetrics("pod-1", "variant-a", 8000, 16000, 6, 100, 50),
		},
		[]domain.VariantReplicaState{
			{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		},
	)

	_, err := analyzer.Analyze(ctx, input)
	require.NoError(t, err)

	assert.Equal(t, "P1-obs", requireLogged(t, logs, "k2-decision")["priority"])
	assert.Equal(t, "k2-compute", requireLogged(t, logs, "replica-capacity-decision")["boundBy"])
}

// Both fallback outcomes must be distinguishable by message name alone: one
// replica contributes no capacity, the other contributes a stored estimate.
// They shared a name until this was pinned, so the report could not tell a
// dropped replica from a covered one.
func TestLogContract_FallbackOutcomesHaveDistinctMessages(t *testing.T) {
	noRecord := makeReplicaMetrics("pod-1", "variant-a", 0, 0, 0, 100, 50)
	states := []domain.VariantReplicaState{
		{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}

	t.Run("no capacity-store record", func(t *testing.T) {
		ctx, logs := observedCtx(t)
		analyzer := NewSaturationAnalyzer(NewCapacityKnowledgeStore())

		_, err := analyzer.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{noRecord}, states))
		require.NoError(t, err)

		requireLogged(t, logs, "replica-capacity-skipped")
		assert.Empty(t, logs.FilterMessage("replica-capacity-store-fallback").All())
	})

	t.Run("covered by a capacity-store record", func(t *testing.T) {
		ctx, logs := observedCtx(t)
		store := NewCapacityKnowledgeStore()
		store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
			AcceleratorName:   "H100",
			GpuCount:          1,
			EffectiveCapacity: 10000,
			LearnedFrom:       learnedFromLive,
			LearnedAt:         time.Now(),
		})
		analyzer := NewSaturationAnalyzer(store)

		_, err := analyzer.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{noRecord}, states))
		require.NoError(t, err)

		requireLogged(t, logs, "replica-capacity-store-fallback")
		assert.Empty(t, logs.FilterMessage("replica-capacity-skipped").All())
	})
}

// A zero-replica variant has to say where its number came from, since every
// source below is an estimate of a different quality: a reused live
// observation, a deployment-arg derivation, or the raw stored value.
func TestLogContract_ZeroReplicaEstimateNamesItsSource(t *testing.T) {
	zeroState := []domain.VariantReplicaState{
		{VariantName: "variant-zero", AcceleratorName: "H100", CurrentReplicas: 0, GPUsPerReplica: 1},
	}

	t.Run("reused live observation", func(t *testing.T) {
		ctx, logs := observedCtx(t)
		store := NewCapacityKnowledgeStore()
		store.Update("test-ns", "test-model", "variant-zero", CapacityRecord{
			AcceleratorName:   "H100",
			GpuCount:          1,
			EffectiveCapacity: 10000,
			LearnedFrom:       learnedFromLive,
		})
		analyzer := NewSaturationAnalyzer(store)

		_, err := analyzer.Analyze(ctx, makeAnalyzerInput(nil, zeroState))
		require.NoError(t, err)

		fields := requireLogged(t, logs, "zero-replica-capacity-estimate")
		assert.Equal(t, "stored-live", stringField(t, fields, "source"))
	})

	t.Run("derived from deployment args", func(t *testing.T) {
		ctx, logs := observedCtx(t)
		store := NewCapacityKnowledgeStore()
		store.Update("test-ns", "test-model", "variant-zero", CapacityRecord{
			AcceleratorName:   "H100",
			GpuCount:          1,
			EffectiveCapacity: 4000,
			EngineParams:      deploymentParams(),
			LearnedFrom:       "deployment",
		})
		analyzer := NewSaturationAnalyzer(store)

		// A live variant alongside it supplies the model-level token averages
		// the derivation needs; without them the estimate falls through to the
		// raw stored value instead.
		_, err := analyzer.Analyze(ctx, makeAnalyzerInput(
			[]domain.ReplicaMetrics{
				makeReplicaMetrics("pod-live", "variant-live", 5000, 16000, 0, 100, 50),
			},
			append([]domain.VariantReplicaState{
				{VariantName: "variant-live", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
			}, zeroState...),
		))
		require.NoError(t, err)

		fields := requireLogged(t, logs, "zero-replica-capacity-estimate")
		assert.Equal(t, "deployment-derived", stringField(t, fields, "source"))
		assert.Contains(t, fields, "boundedBy", "the derived estimate must say what capped it")
		assert.Contains(t, fields, "perReplicaCapacity")
	})

	t.Run("no derivation possible", func(t *testing.T) {
		ctx, logs := observedCtx(t)
		store := NewCapacityKnowledgeStore()
		// Deployment-learned but with no engine params, so there is nothing to
		// derive from and the raw stored value is all that is left.
		store.Update("test-ns", "test-model", "variant-zero", CapacityRecord{
			AcceleratorName:   "H100",
			GpuCount:          1,
			EffectiveCapacity: 4000,
			LearnedFrom:       "deployment",
		})
		analyzer := NewSaturationAnalyzer(store)

		_, err := analyzer.Analyze(ctx, makeAnalyzerInput(nil, zeroState))
		require.NoError(t, err)

		fields := requireLogged(t, logs, "zero-replica-capacity-estimate")
		assert.Equal(t, "deployment-stored-fallback", stringField(t, fields, "source"))
	})
}

// The borrow branch: a variant with deployment params but no capacity of its
// own takes a compatible sibling's number. Nothing else in the report
// distinguishes that from a number the variant measured itself.
func TestLogContract_BorrowedCapacityNamesItsDonor(t *testing.T) {
	ctx, logs := observedCtx(t)

	store := NewCapacityKnowledgeStore()
	// No capacity of its own, so the stored-estimate branch cannot fire...
	store.Update("test-ns", "test-model", "variant-borrow", CapacityRecord{
		AcceleratorName:       "H100",
		GpuCount:              1,
		EffectiveCapacity:     0,
		TotalKvCapacityTokens: 0,
		EngineParams:          deploymentParams(),
		LearnedFrom:           "deployment",
	})
	// ...but a sibling on the same hardware, with equal-valued params, has one.
	store.Update("test-ns", "test-model", "variant-donor", CapacityRecord{
		AcceleratorName:   "H100",
		GpuCount:          1,
		EffectiveCapacity: 9000,
		EngineParams:      deploymentParams(),
		LearnedFrom:       learnedFromLive,
	})
	analyzer := NewSaturationAnalyzer(store)

	_, err := analyzer.Analyze(ctx, makeAnalyzerInput(nil, []domain.VariantReplicaState{
		{VariantName: "variant-borrow", AcceleratorName: "H100", CurrentReplicas: 0, GPUsPerReplica: 1},
	}))
	require.NoError(t, err)

	fields := requireLogged(t, logs, "variant-capacity-source")
	assert.Contains(t, stringField(t, fields, "reason"), "borrowed from a compatible variant")
	assert.Equal(t, float64(9000), fields["perReplicaCapacity"])
	assert.Equal(t, learnedFromLive, stringField(t, fields, "engineParamsSource"))
}

func TestLogContract_NoDataVariantSaysSo(t *testing.T) {
	ctx, logs := observedCtx(t)
	analyzer := NewSaturationAnalyzer(NewCapacityKnowledgeStore())

	_, err := analyzer.Analyze(ctx, makeAnalyzerInput(nil, []domain.VariantReplicaState{
		{VariantName: "variant-zero", AcceleratorName: "H100", CurrentReplicas: 0, GPUsPerReplica: 1},
	}))
	require.NoError(t, err)

	fields := requireLogged(t, logs, "variant-capacity-source")
	assert.Contains(t, stringField(t, fields, "reason"), "no compatible variant found")
}

// The two per-replica decision lines are the highest-volume logging in the
// controller — two per replica per optimize cycle. They are gated so an
// operator can drop to -v=1 and silence them without losing the V(1)
// diagnostics elsewhere; the once-per-cycle lines stay unconditional so that
// knob does not also blind the report to which variants reported at all.
//
// replica-capacity-skipped is deliberately not gated: it reports a replica
// contributing no capacity at all, which under-counts supply and makes the
// controller over-scale, so no -v setting may hide it.
func TestLogContract_PerReplicaLinesAreVerbosityGated(t *testing.T) {
	t.Run("routine decisions are hidden at -v=0", func(t *testing.T) {
		ctx, logs := infoOnlyCtx()
		analyzer := NewSaturationAnalyzer(NewCapacityKnowledgeStore())

		input := makeAnalyzerInput(
			[]domain.ReplicaMetrics{
				makeReplicaMetrics("pod-1", "variant-a", 5000, 16000, 0, 100, 50),
			},
			[]domain.VariantReplicaState{
				{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
			},
		)
		input.SchedulerQueue = &domain.SchedulerQueueMetrics{QueueSize: 3, QueueBytes: 4096}

		_, err := analyzer.Analyze(ctx, input)
		require.NoError(t, err)

		for _, msg := range []string{"k2-decision", "replica-capacity-decision"} {
			assert.Empty(t, logs.FilterMessage(msg).All(),
				"%q is per-replica, per-cycle and must not be logged unconditionally", msg)
		}
		assert.NotEmpty(t, logs.FilterMessage("scheduler-queue-demand").All(),
			"scheduler-queue-demand is once per cycle and stays at Info")
	})

	t.Run("a replica contributing nothing is not", func(t *testing.T) {
		ctx, logs := infoOnlyCtx()
		analyzer := NewSaturationAnalyzer(NewCapacityKnowledgeStore())

		_, err := analyzer.Analyze(ctx, makeAnalyzerInput(
			[]domain.ReplicaMetrics{
				makeReplicaMetrics("pod-1", "variant-a", 0, 0, 0, 100, 50),
			},
			[]domain.VariantReplicaState{
				{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
			},
		))
		require.NoError(t, err)

		assert.NotEmpty(t, logs.FilterMessage("replica-capacity-skipped").All(),
			"a replica contributing no capacity under-counts supply; -v must not hide it")
	})
}

// TestLogContract_ThroughputFloorBinds pins the line the floor emits when it
// changes a decision, and the terms it was built from: "the target held" is
// not diagnosable without knowing whether lambda moved or mu did.
func TestLogContract_ThroughputFloorBinds(t *testing.T) {
	ctx, logs := observedCtx(t)
	analyzer := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
	states := []domain.VariantReplicaState{
		{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}

	// Two saturated cycles first, so a throughput the floor may order on is
	// on record for the bucket (MinThroughputSamplesToOrder).
	sat := makeReplicaMetrics("pod-1", "variant-a", 500_000, 600_000, 10, 4000, 1000)
	sat.RequestRate = 5
	sat.Ready = true
	input := makeAnalyzerInput([]domain.ReplicaMetrics{sat}, states)
	input.ArrivalRate = 14
	for i := 0; i < MinThroughputSamplesToOrder; i++ {
		_, err := analyzer.Analyze(ctx, input)
		require.NoError(t, err)
	}

	// Then an idle-looking one: occupancy a fraction of a replica, the load
	// unchanged. The floor binds and says so.
	idle := makeReplicaMetrics("pod-1", "variant-a", 10_000, 600_000, 0, 4000, 1000)
	idle.RequestRate = 5
	idle.Ready = true
	states[0].CurrentReplicas = 4
	input = makeAnalyzerInput([]domain.ReplicaMetrics{idle, idle, idle, idle}, states)
	input.ArrivalRate = 14
	_, err := analyzer.Analyze(ctx, input)
	require.NoError(t, err)

	fields := requireLogged(t, logs, "throughput-demand-floor")
	assert.Equal(t, domain.RoleBoth, fields["role"])
	// The saturated cycles bind too (their queue of ten is a backlog), so the
	// idle cycle's line is the last one.
	entries := logs.FilterMessage("throughput-demand-floor").All()
	last := entries[len(entries)-1].ContextMap()
	assert.Equal(t, 0.0, last["backlogRequests"], "nothing queued: the floor is the load's alone")
	assert.Equal(t, BacklogDrainSeconds, last["drainSeconds"])
	assert.Equal(t, false, last["heldAtFleet"], "two readings of its own: the floor is the load's, not the cap's")
}
