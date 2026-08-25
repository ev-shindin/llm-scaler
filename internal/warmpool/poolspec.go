package warmpool

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/policy"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

// Annotations carrying a pool's tuning, read from the pool Deployment.
//
// They live on the DEPLOYMENT, not on its pod template, for two reasons. A pod
// template annotation is part of the template, so editing one rolls every Pod in
// the pool -- each of which is holding sleeping models that would have to be
// loaded again, which is the most expensive thing the pool does. And the
// Deployment is the object these values have to agree with: replicas against the
// reserve, the GPU request against what a tensor-parallel variant needs. A knob
// that must agree with an object belongs on it.
const (
	// AnnotationSleepMinSize is the floor on FREE Pods -- the reserve kept for
	// the next spike. It must be LOWER than the Deployment's replica count: at
	// equality the admission budget is replicas-minus-reserve, which is zero
	// forever, and the pool holds GPUs while warming nothing.
	AnnotationSleepMinSize = "llm-d.ai/warm-pool-sleep-min-size"
	// AnnotationMaxHold bounds how long a borrowed Pod may serve before it is
	// returned regardless, so a scale-up that never arrives cannot turn the
	// reserve into permanent capacity for one variant.
	AnnotationMaxHold = "llm-d.ai/warm-pool-max-hold"
	// AnnotationPreloadTop warms this many of the busiest variants without
	// waiting for each to miss first.
	AnnotationPreloadTop = "llm-d.ai/warm-pool-preload-top"
	// AnnotationGPUMemoryUtilization overrides --gpu-memory-utilization for warm
	// copies, trading KV cache for warm-set size.
	AnnotationGPUMemoryUtilization = "llm-d.ai/warm-pool-gpu-memory-utilization"
)

// PoolSpec is one pool: a name, the policy to run it under, and the replica
// count its reserve has to fit inside.
type PoolSpec struct {
	// Name is the pool's llm-d.ai/warm-pool label. Empty is the single unnamed
	// pool, which is what every install has until it declares otherwise.
	Name string
	// Config is the flag defaults with this pool's annotations layered on top.
	Config policy.Config
	// Replicas is what the Deployment asks for, used only to tell an operator
	// that a reserve cannot fit inside it. Zero when the pool was not discovered
	// from a Deployment.
	Replicas int
	// GPUMemoryUtilization is a pool-scoped override of the adapter's value.
	// Zero inherits it.
	GPUMemoryUtilization float64
}

// Inert reports whether this pool can never admit anything, and why.
//
// The reserve is a floor on FREE Pods and the admission budget is free minus
// reserve, so a pool whose replica count does not exceed its reserve sits at a
// budget of zero for its whole life. Nothing is wrong from the inside -- the
// reserve is doing exactly what it was told -- which is what makes it worth
// saying out loud: it is the state the obvious first deployment lands in, with
// one replica and the default reserve of one.
func (p PoolSpec) Inert() (string, bool) {
	if p.Replicas <= 0 || p.Config.SleepMinSize <= 0 {
		return "", false
	}
	if p.Replicas > p.Config.SleepMinSize {
		return "", false
	}
	return fmt.Sprintf("replicas=%d does not exceed the reserve of %d, so the admission budget "+
		"is zero forever: raise replicas or lower %s",
		p.Replicas, p.Config.SleepMinSize, AnnotationSleepMinSize), true
}

// PoolSource reports the pools that exist. Substituted in tests.
type PoolSource interface {
	Pools(ctx context.Context) ([]PoolSpec, error)
}

// DeploymentPools discovers pools by listing the Deployments that declare them.
//
// The Deployment IS the pool: it is what holds the GPUs, and it already carries
// every physical fact about one. Discovering pools from it means an operator
// creates a pool by applying a manifest rather than by applying a manifest AND
// restarting the controller with a matching flag -- which is the split that made
// three of this feature's four setup steps fail closed and silently.
type DeploymentPools struct {
	Client    client.Client
	Namespace string
	// Fallback is the flag-derived config, used as the base every pool's
	// annotations are layered onto, and used whole for the single unnamed pool
	// when no Deployment declares one.
	Fallback policy.Config
}

// Pools lists the pool Deployments in the namespace.
//
// A namespace with no pool Deployment still yields ONE pool, unnamed, carrying
// the flag config: that is every install that predates this, and the Pods it
// finds are labelled for discovery but not for a named pool. Returning nothing
// there would silently disable a working pool on upgrade.
func (d *DeploymentPools) Pools(ctx context.Context) ([]PoolSpec, error) {
	var deployments appsv1.DeploymentList
	if err := d.Client.List(ctx, &deployments,
		client.InNamespace(d.Namespace),
		client.MatchingLabels{pool.ComponentLabel: pool.ComponentValue},
	); err != nil {
		return nil, fmt.Errorf("list pool deployments: %w", err)
	}
	if len(deployments.Items) == 0 {
		return []PoolSpec{{Name: "", Config: d.Fallback}}, nil
	}

	out := make([]PoolSpec, 0, len(deployments.Items))
	for i := range deployments.Items {
		dep := &deployments.Items[i]
		spec := PoolSpec{
			Name:   dep.Labels[pool.PoolLabel],
			Config: d.Fallback,
		}
		if dep.Spec.Replicas != nil {
			spec.Replicas = int(*dep.Spec.Replicas)
		}
		applyAnnotations(&spec, dep.Annotations)
		out = append(out, spec)
	}
	// Ordered so two pools are processed the same way on every pass and every
	// process. Map and list order are not guaranteed, and a pool that wins a
	// free Pod only on some passes is worse than either outcome consistently.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// applyAnnotations layers a Deployment's annotations over the flag defaults.
//
// An unparseable value is IGNORED rather than treated as zero. Zero is a
// meaningful setting for every one of these -- no reserve, no hold limit, no
// preloading -- so parsing "2x" as "disable the reserve" would turn a typo into
// a policy change, and a silent one.
func applyAnnotations(spec *PoolSpec, annotations map[string]string) {
	if v, ok := parseInt(annotations[AnnotationSleepMinSize]); ok {
		spec.Config.SleepMinSize = v
	}
	if v, err := time.ParseDuration(annotations[AnnotationMaxHold]); err == nil && v > 0 {
		spec.Config.MaxHold = v
	}
	if v, ok := parseInt(annotations[AnnotationPreloadTop]); ok {
		spec.Config.PreloadTop = v
	}
	if v, err := strconv.ParseFloat(annotations[AnnotationGPUMemoryUtilization], 64); err == nil && v > 0 && v <= 1 {
		spec.GPUMemoryUtilization = v
	}
}

func parseInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// MembershipsIn returns the memberships belonging to one pool.
//
// A Pod whose label does not match any declared pool belongs to none, and is
// left out of every pool's view rather than folded into the first: it holds a
// GPU that no pool will ever lend, which the caller reports.
func MembershipsIn(all []pool.Membership, name string) []pool.Membership {
	out := make([]pool.Membership, 0, len(all))
	for _, m := range all {
		if m.Pool == name {
			out = append(out, m)
		}
	}
	return out
}

// Orphaned returns the Pods carrying a pool label no declared pool matches.
func Orphaned(all []pool.Membership, pools []PoolSpec) []string {
	declared := make(map[string]bool, len(pools))
	for _, p := range pools {
		declared[p.Name] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range all {
		if declared[m.Pool] || seen[m.Pod.Name] {
			continue
		}
		seen[m.Pod.Name] = true
		out = append(out, m.Pod.Name)
	}
	sort.Strings(out)
	return out
}
