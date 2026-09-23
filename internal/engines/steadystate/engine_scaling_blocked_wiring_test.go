package steadystate

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// policy.ScaleToZeroBlockReasons is unit-tested directly and the metric is unit-tested
// directly; what neither covers is that applyScaleToZeroEnforcement actually
// CALLS them, with the right owned set, on every path that returns early. Those
// early returns are the whole risk: the first implementation emitted below the
// empty-decisions return, so a quiet cycle — exactly when a stale "will never
// park" series is most misleading — published nothing at all.
var _ = Describe("applyScaleToZeroEnforcement blocked-reason wiring", func() {
	const (
		modelID   = "wired-model"
		namespace = "wired-ns"
	)

	var ctx = context.Background()

	target := func(image string) scaletarget.ScaleTargetAccessor {
		return scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "server", Image: image}},
					},
				},
			},
		})
	}

	vllm := map[string]scaletarget.ScaleTargetAccessor{"a": target("vllm/vllm-openai:latest")}
	sglang := map[string]scaletarget.ScaleTargetAccessor{"a": target("lmsysorg/sglang:latest")}

	// engineFor builds an engine with its own metric registry, so the reasons
	// gathered below are only the ones this spec published.
	engineFor := func(scaleToZero bool) (*Engine, *prometheus.Registry) {
		registry := prometheus.NewRegistry()
		Expect(metrics.InitMetrics(registry)).To(Succeed())

		cfg := config.NewTestConfig()
		cfg.UpdateScalingPolicyConfigForNamespace(namespace, map[string]config.ScalingPolicy{
			"default": {ScaleToZero: &config.ScaleToZeroEnvelope{
				Enabled: ptrTo(scaleToZero), RetentionPeriod: "10m",
			}},
		})
		return &Engine{
			Config:            cfg,
			lastBlockedModels: make(map[string]blockedModelRef),
			ScaleToZeroEnforcer: allocation.NewEnforcer(
				func(context.Context, inferenceengine.Engine, string, string, time.Duration) (float64, error) {
					return 0, nil // idle
				},
				nil,
			),
		}, registry
	}

	publishedReasons := func(registry *prometheus.Registry) []string {
		mfs, err := registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		var out []string
		for _, mf := range mfs {
			if mf.GetName() != constants.WVAModelScalingBlocked {
				continue
			}
			for _, m := range mf.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == constants.LabelReason {
						out = append(out, l.GetValue())
					}
				}
			}
		}
		return out
	}

	decisions := func() []domain.VariantDecision {
		return []domain.VariantDecision{
			{VariantName: "v1", ModelID: modelID, Namespace: namespace, Cost: 1.0, CurrentReplicas: 2, TargetReplicas: 2},
		}
	}

	floored := func() []domain.VariantReplicaState {
		min := 1
		return []domain.VariantReplicaState{{VariantName: "v1", MinReplicas: &min}}
	}

	permitsZero := func() []domain.VariantReplicaState {
		min := 0
		return []domain.VariantReplicaState{{VariantName: "v1", MinReplicas: &min}}
	}

	It("reports variant-floor when scale-to-zero is enabled and a variant is floored", func() {
		e, registry := engineFor(true)
		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			decisions(), vllm, floored())

		Expect(publishedReasons(registry)).To(ConsistOf(constants.ScalingBlockedVariantFloor))
	})

	It("reports policy-forbids-zero when every variant permits zero but the policy does not", func() {
		e, registry := engineFor(false)
		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			decisions(), vllm, permitsZero())

		Expect(publishedReasons(registry)).To(ConsistOf(constants.ScalingBlockedPolicyForbidsZero))
	})

	// engine-unsupported now means MORE THAN ONE engine, not "not vLLM": a single
	// SGLang model is measurable on sglang:num_requests_total and reports nothing.
	It("reports engine-unsupported for a model running two engines", func() {
		e, registry := engineFor(true)
		mixed := map[string]scaletarget.ScaleTargetAccessor{
			"a": target("vllm/vllm-openai:latest"),
			"b": target("lmsysorg/sglang:latest"),
		}
		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			decisions(), mixed, permitsZero())

		Expect(publishedReasons(registry)).To(ConsistOf(constants.ScalingBlockedEngineUnsupported))
	})

	It("reports nothing for a single-engine SGLang model", func() {
		e, registry := engineFor(true)
		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			decisions(), sglang, permitsZero())

		Expect(publishedReasons(registry)).To(BeEmpty(),
			"SGLang alone has its own request counter, so nothing blocks it")
	})

	It("publishes nothing for a coherent configuration", func() {
		e, registry := engineFor(true)
		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			decisions(), vllm, permitsZero())

		Expect(publishedReasons(registry)).To(BeEmpty())
	})

	// The regression that prompted this file. The emission must sit ABOVE the
	// empty-decisions return: the reasons are configuration reconciled against
	// discovered bounds and do not depend on there being a decision to enforce.
	It("still reports when the optimizer produced no decisions at all", func() {
		e, registry := engineFor(true)
		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			nil, vllm, floored())

		Expect(publishedReasons(registry)).To(ConsistOf(constants.ScalingBlockedVariantFloor),
			"a quiet cycle is exactly when a stale reason is most misleading")
	})

	// Clearing is the other half of the contract: the series must vanish when the
	// operator fixes the configuration, not linger until the process restarts.
	It("clears the reason once the contradiction is resolved", func() {
		e, registry := engineFor(true)
		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			decisions(), vllm, floored())
		Expect(publishedReasons(registry)).NotTo(BeEmpty())

		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			decisions(), vllm, permitsZero())

		Expect(publishedReasons(registry)).To(BeEmpty())
	})

	// The hold is transient, so it is the reason most likely to be dropped by a
	// refactor without anyone noticing — nothing else reports it, and it clears
	// itself before anyone looks.
	It("reports activation-retention while a just-woken model is held", func() {
		e, registry := engineFor(true)
		decision.DefaultActivations.Mark(namespace, modelID)
		DeferCleanup(func() { decision.DefaultActivations.Clear(namespace, modelID) })

		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			decisions(), vllm, permitsZero())

		Expect(publishedReasons(registry)).To(ConsistOf(constants.ScalingBlockedActivationRetention))
	})

	It("clears activation-retention once the hold lapses", func() {
		e, registry := engineFor(true)
		decision.DefaultActivations.Mark(namespace, modelID)
		decision.DefaultActivations.Clear(namespace, modelID)

		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			decisions(), vllm, permitsZero())

		Expect(publishedReasons(registry)).To(BeEmpty(),
			"a transient reason that never clears is worse than not reporting it")
	})

	// The enforcer owns only the policy reasons. If it cleared everything, it
	// would erase the scale-from-zero loop's verdict on every interval.
	It("leaves the wake reason alone", func() {
		e, registry := engineFor(true)
		metrics.SetModelScalingBlockedReasons(namespace, modelID,
			constants.ScalingBlockedReasonsWake,
			[]string{constants.ScalingBlockedNoWakeSignal})

		e.applyScaleToZeroEnforcement(ctx, modelID, namespace, "v2-saturation",
			decisions(), vllm, permitsZero())

		Expect(publishedReasons(registry)).To(ConsistOf(constants.ScalingBlockedNoWakeSignal))
	})
})
