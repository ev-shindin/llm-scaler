package steadystate

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// The capacity-build step is the sole owner of model-level supply, utilization
// and RoleCapacities: analyzers emit only (D, P) plus per-role demand, and the
// builder assembles the rest from the same VariantCapacities. These specs pin
// that assembly down directly, since no analyzer publishes those fields anymore.
var _ = Describe("buildCapacities", func() {
	// No thresholds in play for the assembly specs: scaleUp=1, scaleDown=1 keeps
	// applyUniversalThreshold's arithmetic trivial so the assertions isolate the
	// supply/utilization/role assembly.
	const (
		noScaleUp   = 1.0
		noScaleDown = 1.0
	)

	It("is a no-op on a nil result", func() {
		Expect(func() { buildCapacities(ctx, nil, nil, noScaleUp, noScaleDown) }).NotTo(Panic())
	})

	It("assembles model-level supply from each variant's (ReplicaCount, P)", func() {
		r := &allocation.NamedAnalyzerResult{
			Result: &domain.AnalyzerResult{
				TotalDemand: 12000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", ReplicaCount: 2, PendingReplicas: 1, PerReplicaCapacity: 5000},
					{VariantName: "v2", ReplicaCount: 1, PendingReplicas: 0, PerReplicaCapacity: 4000},
				},
			},
		}
		buildCapacities(ctx, r, nil, noScaleUp, noScaleDown)

		Expect(r.TotalSupply).To(Equal(14000.0))            // 2×5000 + 1×4000
		Expect(r.TotalAnticipatedSupply).To(Equal(19000.0)) // (2+1)×5000 + 1×4000
	})

	It("derives utilization as demand over supply", func() {
		r := &allocation.NamedAnalyzerResult{
			Result: &domain.AnalyzerResult{
				TotalDemand: 7000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", ReplicaCount: 2, PerReplicaCapacity: 5000},
				},
			},
		}
		buildCapacities(ctx, r, nil, noScaleUp, noScaleDown)
		Expect(r.Utilization).To(Equal(0.7)) // 7000 / 10000
	})

	It("leaves utilization at zero when there is no supply (no divide-by-zero)", func() {
		r := &allocation.NamedAnalyzerResult{
			Result: &domain.AnalyzerResult{
				TotalDemand: 5000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", ReplicaCount: 0, PerReplicaCapacity: 5000},
				},
			},
		}
		buildCapacities(ctx, r, nil, noScaleUp, noScaleDown)
		Expect(r.TotalSupply).To(BeZero())
		Expect(r.Utilization).To(BeZero())
	})

	It("overwrites any stale supply/utilization an analyzer may have set", func() {
		// Analyzers are contractually required to leave these zero; if one sets
		// them anyway the builder's derived values must win.
		r := &allocation.NamedAnalyzerResult{
			TotalSupply:            999999,
			TotalAnticipatedSupply: 888888,
			Utilization:            42,
			Result: &domain.AnalyzerResult{
				TotalDemand: 1000,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", ReplicaCount: 1, PerReplicaCapacity: 2000},
				},
			},
		}
		buildCapacities(ctx, r, nil, noScaleUp, noScaleDown)
		Expect(r.TotalSupply).To(Equal(2000.0))
		Expect(r.TotalAnticipatedSupply).To(Equal(2000.0))
		Expect(r.Utilization).To(Equal(0.5))
	})

	It("joins discovery identity onto the per-variant capacities", func() {
		r := &allocation.NamedAnalyzerResult{
			Result: &domain.AnalyzerResult{
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "v1", ReplicaCount: 1, PerReplicaCapacity: 1000},
				},
			},
		}
		meta := map[string]domain.VariantMetadata{
			"v1": {VariantName: "v1", Cost: 12.5, AcceleratorName: "H100", Role: "decode"},
		}
		buildCapacities(ctx, r, meta, noScaleUp, noScaleDown)

		// Only Role is joined now: cost and accelerator are not on VariantCapacity
		// at all, and the optimizer reads them from VariantMetadata instead.
		vc := r.Result.VariantCapacities[0]
		Expect(vc.Role).To(Equal("decode"))
	})

	Describe("RoleCapacities assembly", func() {
		It("returns nil RoleCapacities when the analyzer emitted no per-role demand", func() {
			// Non-disaggregated: the optimizer falls back to the model-level totals.
			r := &allocation.NamedAnalyzerResult{
				Result: &domain.AnalyzerResult{
					TotalDemand: 5000,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v1", Role: domain.RoleBoth, ReplicaCount: 1, PerReplicaCapacity: 10000},
					},
				},
			}
			buildCapacities(ctx, r, nil, noScaleUp, noScaleDown)
			Expect(r.RoleCapacities).To(BeNil())
		})

		It("pairs the analyzer's per-role demand with per-role supply", func() {
			r := &allocation.NamedAnalyzerResult{
				Result: &domain.AnalyzerResult{
					TotalDemand: 12000,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "p1", Role: "prefill", ReplicaCount: 1, PendingReplicas: 1, PerReplicaCapacity: 5000},
						{VariantName: "d1", Role: "decode", ReplicaCount: 2, PerReplicaCapacity: 4000},
					},
					RoleDemand: map[string]float64{"prefill": 4000, "decode": 8000},
				},
			}
			buildCapacities(ctx, r, nil, noScaleUp, noScaleDown)

			Expect(r.RoleCapacities).To(HaveLen(2))

			prefill := r.RoleCapacities["prefill"]
			Expect(prefill.Role).To(Equal("prefill"))
			Expect(prefill.TotalSupply).To(Equal(5000.0))             // 1×5000
			Expect(prefill.TotalAnticipatedSupply).To(Equal(10000.0)) // (1+1)×5000
			Expect(prefill.TotalDemand).To(Equal(4000.0))             // from RoleDemand

			decode := r.RoleCapacities["decode"]
			Expect(decode.TotalSupply).To(Equal(8000.0))
			Expect(decode.TotalAnticipatedSupply).To(Equal(8000.0))
			Expect(decode.TotalDemand).To(Equal(8000.0))
		})

		It("keeps a 'both' bucket in a mixed fleet so the optimizer can find it", func() {
			// cost_aware_optimizer looks up RoleCapacities[state.Role] with an empty
			// role canonicalized to "both". A "both" variant in an otherwise
			// disaggregated model must therefore get its own bucket rather than
			// silently falling back to the model-level signals.
			r := &allocation.NamedAnalyzerResult{
				Result: &domain.AnalyzerResult{
					TotalDemand: 9000,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "p1", Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 5000},
						{VariantName: "b1", Role: domain.RoleBoth, ReplicaCount: 2, PerReplicaCapacity: 3000},
					},
					RoleDemand: map[string]float64{"prefill": 4000, domain.RoleBoth: 5000},
				},
			}
			buildCapacities(ctx, r, nil, noScaleUp, noScaleDown)

			Expect(r.RoleCapacities).To(HaveKey(domain.RoleBoth))
			both := r.RoleCapacities[domain.RoleBoth]
			Expect(both.Role).To(Equal(domain.RoleBoth))
			Expect(both.TotalSupply).To(Equal(6000.0)) // 2×3000
			Expect(both.TotalDemand).To(Equal(5000.0))
		})

		It("groups an empty-role variant into the 'both' bucket's supply", func() {
			r := &allocation.NamedAnalyzerResult{
				Result: &domain.AnalyzerResult{
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "d1", Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 4000},
						{VariantName: "legacy", Role: "", ReplicaCount: 1, PerReplicaCapacity: 1000},
					},
					RoleDemand: map[string]float64{"decode": 2000, domain.RoleBoth: 500},
				},
			}
			buildCapacities(ctx, r, nil, noScaleUp, noScaleDown)
			Expect(r.RoleCapacities[domain.RoleBoth].TotalSupply).To(Equal(1000.0))
		})

		It("yields zero supply for a role the analyzer charged but no variant serves", func() {
			// Demand attributed to a role with no variants behind it produces a
			// zero-supply bucket rather than being dropped, so the shortfall stays
			// visible to the threshold post-step.
			r := &allocation.NamedAnalyzerResult{
				Result: &domain.AnalyzerResult{
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "d1", Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 4000},
					},
					RoleDemand: map[string]float64{"decode": 2000, "prefill": 1500},
				},
			}
			buildCapacities(ctx, r, nil, noScaleUp, noScaleDown)

			prefill := r.RoleCapacities["prefill"]
			Expect(prefill.TotalSupply).To(BeZero())
			Expect(prefill.TotalDemand).To(Equal(1500.0))
		})

		It("uses the discovery-joined role, not the analyzer's, when grouping supply", func() {
			// Step (1) overwrites Role from discovery before step (3) groups by it,
			// so per-role supply follows the authoritative identity.
			r := &allocation.NamedAnalyzerResult{
				Result: &domain.AnalyzerResult{
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v1", Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 4000},
					},
					RoleDemand: map[string]float64{"decode": 1000},
				},
			}
			meta := map[string]domain.VariantMetadata{
				"v1": {VariantName: "v1", Role: "decode"},
			}
			buildCapacities(ctx, r, meta, noScaleUp, noScaleDown)

			Expect(r.RoleCapacities["decode"].TotalSupply).To(Equal(4000.0))
		})
	})
})

// The supply the builder assembles must never describe a larger fleet than the
// scale target is committed to. When it does, the optimizer is credited with
// spare capacity that an in-flight scale-down has already claimed and removes
// the same replicas twice — which is how a variant under sustained load lands
// on one replica. The figures below are from a 14 QPS benchmark run that did
// exactly that (biran-20260822-021153-340, 23:31:08Z).
var _ = Describe("clampReplicaCountToScaleTarget", func() {
	const (
		noScaleUp   = 1.0
		noScaleDown = 1.0
		// Per-replica capacity measured throughout that run: min(k1, k2) with k2
		// from the rolling average (P2-hist). It held constant while the target
		// oscillated, which is what ruled the capacity estimate out as the cause.
		prc = 487065.0
	)

	It("caps a measured count that runs ahead of the scale target", func() {
		// Five replicas still reporting, target already reduced to three: the two
		// condemned replicas must not appear in supply.
		r := &allocation.NamedAnalyzerResult{
			Result: &domain.AnalyzerResult{
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "decode", ReplicaCount: 5, PerReplicaCapacity: prc},
				},
			},
		}
		meta := map[string]domain.VariantMetadata{
			"decode": {VariantName: "decode", CurrentReplicas: 3},
		}
		buildCapacities(ctx, r, meta, noScaleUp, noScaleDown)

		Expect(r.TotalSupply).To(Equal(3 * prc))
		Expect(r.TotalAnticipatedSupply).To(Equal(3 * prc))
	})

	It("leaves a measured count below the scale target alone", func() {
		// Replicas launching, or a pod that missed a scrape. The measured value
		// wins: counting capacity that has not arrived is how a fleet gets scaled
		// down onto replicas that cannot take the load.
		r := &allocation.NamedAnalyzerResult{
			Result: &domain.AnalyzerResult{
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "decode", ReplicaCount: 2, PendingReplicas: 3, PerReplicaCapacity: prc},
				},
			},
		}
		meta := map[string]domain.VariantMetadata{
			"decode": {VariantName: "decode", CurrentReplicas: 5},
		}
		buildCapacities(ctx, r, meta, noScaleUp, noScaleDown)

		Expect(r.TotalSupply).To(Equal(2 * prc))
		Expect(r.TotalAnticipatedSupply).To(Equal(5 * prc)) // (2+3) — pending still counts
	})

	It("does not credit spare capacity an in-flight scale-down has already claimed", func() {
		// The regression, with the run's own numbers. Unclamped this yields
		// SC = 5×prc − 710027/0.7 = 1,421,001, i.e. two more replicas of slack on
		// top of the two already being removed, and the decision became 3 − 2 = 1
		// in the middle of a 14 QPS stage.
		const scaleDown = 0.7
		r := &allocation.NamedAnalyzerResult{
			Result: &domain.AnalyzerResult{
				TotalDemand: 710027,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "decode", ReplicaCount: 5, PerReplicaCapacity: prc},
				},
			},
		}
		meta := map[string]domain.VariantMetadata{
			"decode": {VariantName: "decode", CurrentReplicas: 3},
		}
		buildCapacities(ctx, r, meta, 0.85, scaleDown)

		// Asserted as a replica count, because that is what the optimizer takes
		// from it: safeRemovalReplicasForRole removes floor(SC / prc).
		Expect(int(r.SpareCapacity/prc)).To(BeZero(),
			"a target already heading to 3 has no further replica to give up at this demand")
	})

	It("treats a zero replica count as unknown rather than clamping supply away", func() {
		// Discovery reports zero both for a parked variant and for a scale target
		// it could not read. Nothing is lost by skipping the clamp: a decision
		// based at zero replicas cannot remove any.
		r := &allocation.NamedAnalyzerResult{
			Result: &domain.AnalyzerResult{
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "decode", ReplicaCount: 2, PerReplicaCapacity: prc},
				},
			},
		}
		meta := map[string]domain.VariantMetadata{
			"decode": {VariantName: "decode", CurrentReplicas: 0},
		}
		buildCapacities(ctx, r, meta, noScaleUp, noScaleDown)

		Expect(r.TotalSupply).To(Equal(2 * prc))
	})

	It("clamps per-role supply too, since that is what a decode-only fleet reads", func() {
		// The optimizer takes RoleSpare from RoleCapacities for any variant with a
		// role, so a clamp that reached only the model scope would not change the
		// decision on the very fleet that exhibited this.
		r := &allocation.NamedAnalyzerResult{
			Result: &domain.AnalyzerResult{
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "decode", Role: "decode", ReplicaCount: 5, PerReplicaCapacity: prc},
				},
				RoleDemand: map[string]float64{"decode": 710027},
			},
		}
		meta := map[string]domain.VariantMetadata{
			"decode": {VariantName: "decode", Role: "decode", CurrentReplicas: 3},
		}
		buildCapacities(ctx, r, meta, 0.85, 0.7)

		Expect(r.RoleCapacities["decode"].TotalSupply).To(Equal(3 * prc))
		Expect(int(r.RoleCapacities["decode"].SpareCapacity / prc)).To(BeZero())
	})
})

// The clamp runs per variant inside the metadata-join loop, and per-role supply
// is summed across variants afterwards. These pin the composition: that it does
// not leak between variants, and what it does to the OTHER signal built from the
// same supply.
var _ = Describe("clampReplicaCountToScaleTarget composition", func() {
	const prc = 487065.0

	It("clamps only the variant that is over its own target", func() {
		// Role must be set on the metadata as well as the capacity: the join
		// overwrites Role from discovery, so metadata without one would blank it
		// and collapse both variants into the same role bucket.
		r := &allocation.NamedAnalyzerResult{
			Result: &domain.AnalyzerResult{
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "prefill", Role: "prefill", ReplicaCount: 4, PerReplicaCapacity: prc},
					{VariantName: "decode", Role: "decode", ReplicaCount: 6, PerReplicaCapacity: prc},
				},
				RoleDemand: map[string]float64{"prefill": 100, "decode": 100},
			},
		}
		meta := map[string]domain.VariantMetadata{
			"prefill": {VariantName: "prefill", Role: "prefill", CurrentReplicas: 4},
			"decode":  {VariantName: "decode", Role: "decode", CurrentReplicas: 3},
		}
		buildCapacities(ctx, r, meta, 1.0, 1.0)

		Expect(r.RoleCapacities["prefill"].TotalSupply).To(Equal(4*prc), "prefill was at its target and must be untouched")
		Expect(r.RoleCapacities["decode"].TotalSupply).To(Equal(3*prc), "decode was over its target and must be clamped")
		Expect(r.TotalSupply).To(Equal(7 * prc))
	})

	It("counts pending replicas against the clamped count", func() {
		// Benign in practice: pending describes replicas ARRIVING and the clamp
		// only fires while replicas are LEAVING, so the two do not normally
		// coincide. Pinned so that if they ever do, the behaviour is a decision
		// rather than an accident.
		r := &allocation.NamedAnalyzerResult{
			Result: &domain.AnalyzerResult{
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "decode", ReplicaCount: 5, PendingReplicas: 1, PerReplicaCapacity: prc},
				},
			},
		}
		meta := map[string]domain.VariantMetadata{
			"decode": {VariantName: "decode", CurrentReplicas: 3},
		}
		buildCapacities(ctx, r, meta, 1.0, 1.0)

		Expect(r.TotalSupply).To(Equal(3 * prc))
		Expect(r.TotalAnticipatedSupply).To(Equal(4 * prc)) // (3 clamped + 1 pending)
	})

	It("can turn the scale-up signal positive, and that is not suppressed", func() {
		// The asymmetry worth knowing about. SC shrinking can only hold replicas,
		// but RC is sized against anticipated supply, which shrinks too, while
		// demand is still summed over the larger pod set. So the same clamp that
		// makes scale-down safer makes scale-up EAGERER. Unclamped this demand
		// leaves RC at zero; clamped it asks for capacity.
		newResult := func() *allocation.NamedAnalyzerResult {
			return &allocation.NamedAnalyzerResult{
				Result: &domain.AnalyzerResult{
					TotalDemand: 900000,
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "decode", ReplicaCount: 3, PerReplicaCapacity: prc},
					},
				},
			}
		}

		unclamped := newResult()
		buildCapacities(ctx, unclamped, nil, 0.85, 0.70)
		Expect(unclamped.RequiredCapacity).To(BeZero(), "three replicas of supply already cover this demand")

		clamped := newResult()
		buildCapacities(ctx, clamped, map[string]domain.VariantMetadata{
			"decode": {VariantName: "decode", CurrentReplicas: 2},
		}, 0.85, 0.70)
		Expect(clamped.RequiredCapacity).To(BeNumerically(">", 0),
			"with only two replicas committed, the same demand is a shortfall")
	})
})
