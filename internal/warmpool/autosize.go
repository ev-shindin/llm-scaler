package warmpool

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Annotations bounding how far the pool may resize itself.
//
// Both absent means the pool NEVER resizes, which is the behaviour every
// install has today and the right default: a pool holds accelerators
// continuously, so growing one spends real money and shrinking one throws away
// loaded models. An operator opts in by stating the range they are willing to
// pay for.
const (
	// AnnotationMinReplicas is the floor the pool may shrink to.
	AnnotationMinReplicas = "llm-d.ai/warm-pool-min-replicas"
	// AnnotationMaxReplicas is the ceiling it may grow to. Without this, no
	// growth happens at all -- an unbounded pool answering a persistent
	// shortfall would consume the cluster.
	AnnotationMaxReplicas = "llm-d.ai/warm-pool-max-replicas"
)

// Resize is a decision to change a pool's size.
type Resize struct {
	// Pool is the Deployment to scale.
	Pool types.NamespacedName
	// To is the replica count to set.
	To int
	// Why is the reason, for the log line that accompanies it.
	Why string
}

// SizeBounds is the range a pool may resize within, and whether it may at all.
type SizeBounds struct {
	Min, Max int
	Enabled  bool
}

// BoundsFrom reads the resize range from a pool Deployment's annotations.
//
// Requires BOTH to be present and sane. A max alone with no floor could shrink
// a pool to nothing the first time it was quiet; a min alone could never grow.
// Half a range is a misconfiguration rather than a partial opt-in, so it is
// refused whole and reported.
func BoundsFrom(annotations map[string]string) (SizeBounds, error) {
	rawMin, hasMin := annotations[AnnotationMinReplicas]
	rawMax, hasMax := annotations[AnnotationMaxReplicas]
	if !hasMin && !hasMax {
		return SizeBounds{}, nil
	}
	if !hasMin || !hasMax {
		return SizeBounds{}, fmt.Errorf("%s and %s must be set together; resizing stays off",
			AnnotationMinReplicas, AnnotationMaxReplicas)
	}
	min, okMin := parseInt(rawMin)
	max, okMax := parseInt(rawMax)
	if !okMin || !okMax {
		return SizeBounds{}, fmt.Errorf("%s=%q and %s=%q must both be non-negative whole numbers",
			AnnotationMinReplicas, rawMin, AnnotationMaxReplicas, rawMax)
	}
	if min > max {
		return SizeBounds{}, fmt.Errorf("%s=%d is above %s=%d", AnnotationMinReplicas, min, AnnotationMaxReplicas, max)
	}
	return SizeBounds{Min: min, Max: max, Enabled: true}, nil
}

// SizeFor decides whether a pool should change size.
//
// Growth answers a shortfall against the reserve, and only a SUSTAINED one:
// `shortFor` is how many consecutive passes the pool has been short, and a
// single pass is a borrow doing its job rather than a pool that is too small.
//
// Shrinking is deliberately harder than growing, and asymmetric on purpose. A
// pool that is too small costs latency on every spike it cannot cover; a pool
// that is too large costs money. Shrinking early to save money and then paying
// a full model load to grow back is the worst of both, so it takes a longer
// quiet period AND requires the pool to be entirely idle -- no bridge open
// anywhere, because scale-down cannot choose its victim precisely enough to
// promise otherwise.
func SizeFor(spec PoolSpec, bounds SizeBounds, free, lent, shortFor, idleFor int, growAfter, shrinkAfter int) (Resize, bool) {
	if !bounds.Enabled || spec.Replicas <= 0 {
		return Resize{}, false
	}
	switch {
	case shortFor >= growAfter && spec.Replicas < bounds.Max:
		return Resize{
			To: spec.Replicas + 1,
			Why: fmt.Sprintf("short of its reserve for %d consecutive passes (free=%d, reserve=%d)",
				shortFor, free, spec.Config.SleepMinSize),
		}, true
	case lent == 0 && idleFor >= shrinkAfter && spec.Replicas > bounds.Min &&
		free > spec.Config.SleepMinSize:
		return Resize{
			To: spec.Replicas - 1,
			Why: fmt.Sprintf("idle above its reserve for %d consecutive passes (free=%d, reserve=%d)",
				idleFor, free, spec.Config.SleepMinSize),
		}, true
	}
	return Resize{}, false
}

// Resizer applies a resize to a pool Deployment.
type Resizer struct {
	Client    client.Client
	Namespace string
}

// Apply sets the pool Deployment's replica count, through the SCALE
// subresource.
//
// Not a patch of the Deployment itself, and the difference is a privilege one.
// `deployments/scale` can change a replica count and nothing else; `patch` on
// deployments would let this controller rewrite any workload's pod template in
// every namespace it watches -- image, command, volumes, service account. The
// pool needs one integer, so it asks for the permission that grants one
// integer. It is also what an HPA uses, so a GitOps tool that already tolerates
// replica drift from autoscaling tolerates this by the same rule.
func (r *Resizer) Apply(ctx context.Context, name string, to int) error {
	var pool appsv1.Deployment
	ref := types.NamespacedName{Namespace: r.Namespace, Name: name}
	if err := r.Client.Get(ctx, ref, &pool); err != nil {
		return fmt.Errorf("read pool deployment %s: %w", ref, err)
	}
	scale := &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name},
		Spec:       autoscalingv1.ScaleSpec{Replicas: int32(to)}, //nolint:gosec // bounded by the max annotation
	}
	if err := r.Client.SubResource("scale").Update(ctx, &pool, client.WithSubResourceBody(scale)); err != nil {
		return fmt.Errorf("resize pool deployment %s to %d: %w", ref, to, err)
	}
	return nil
}
