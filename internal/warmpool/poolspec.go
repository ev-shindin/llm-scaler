package warmpool

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/policy"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PoolSpec is one pool: a name, the policy to run it under, and the replica
// count its reserve has to fit inside.
type PoolSpec struct {
	// Name is the pool's llm-d.ai/warm-pool label. Empty is the single unnamed
	// pool, which is what every install has until it declares otherwise.
	Name string
	// Config is the flag defaults with this pool's annotations layered on top.
	Config policy.Config
	// Replicas is the pool's CURRENT size, used for the arbitration hold. Zero
	// when unknown.
	Replicas int
	// MaxReplicas is the ceiling KEDA will hold this pool within, from its
	// ScaledObject. It, not the current size, is what decides whether a pool
	// can ever admit anything: a reserve that reaches the ceiling leaves a
	// budget of zero forever. Zero means unbounded/unknown.
	MaxReplicas int
	// GPUMemoryUtilization is a pool-scoped override of the adapter's value.
	// Zero inherits it.
	GPUMemoryUtilization float64
	// Deployment is the object to resize. Empty when the pool was not
	// discovered from one, in which case it cannot be resized at all.
	Deployment string
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
	if p.Config.SleepMinSize <= 0 {
		return "", false
	}
	// The CEILING, not the current size. A pool sitting at its reserve right now
	// is ordinary -- it grows on the next pass. A pool whose ceiling is its
	// reserve can never grow past it, and that is the state worth reporting.
	ceiling := p.MaxReplicas
	if ceiling <= 0 {
		ceiling = p.Replicas // no ScaledObject to bound it; judge what it has
	}
	if ceiling <= 0 || ceiling > p.Config.SleepMinSize {
		return "", false
	}
	return fmt.Sprintf("maxReplicaCount=%d does not exceed the reserve of %d, so the admission "+
		"budget is zero forever: raise maxReplicaCount or lower %s",
		ceiling, p.Config.SleepMinSize, registry.WarmPoolSleepMinSizeKey), true
}

// PoolSource reports the pools that exist. Substituted in tests.
type PoolSource interface {
	Pools(ctx context.Context) ([]PoolSpec, error)
}

// RegistryPools discovers pools from the ScaledObjects that declare them.
//
// A warm pool is a WVA concept that happens to have Pods, not a workload WVA
// happens to manage: nothing outside WVA reads one, nothing outside WVA creates
// one, and there is no way to operate one except through WVA. So it is declared
// the same way everything else WVA knows about is -- by a KEDA trigger -- and
// the invariant holds without exception: WVA manages what it is CALLED about.
//
// The Deployment still supplies the Pods and still states their physical facts,
// which are READ from the Pod rather than declared here. What moved is the
// tuning, which now sits beside the minReplicaCount and maxReplicaCount it has
// to agree with.
type RegistryPools struct {
	// Snapshot returns the registered ScaledObjects. Injected rather than taken
	// from the package default so tests need no global.
	Snapshot func() []registry.Entry
	// Namespace bounds discovery to the pool namespace.
	Namespace string
	// Fallback is the flag-derived config each pool's trigger is layered onto,
	// and the whole config for an install that declares no pool at all.
	Fallback policy.Config
}

// Pools lists the warm pools declared in this namespace.
//
// A namespace whose triggers declare no pool still yields ONE pool, unnamed,
// carrying the flag config: that is every install that predates named pools, and
// returning nothing would silently switch a working pool off on upgrade.
func (r *RegistryPools) Pools(ctx context.Context) ([]PoolSpec, error) {
	logger := log.FromContext(ctx).WithName("warmpool")

	var out []PoolSpec
	for _, entry := range r.Snapshot() {
		if r.Namespace != "" && entry.Namespace != r.Namespace {
			continue
		}
		if !registry.ScalesAWarmPool(entry.Metadata) {
			continue
		}
		meta, err := registry.ParsePoolMeta(entry.Metadata)
		if err != nil {
			// Refused whole rather than partly applied: a pool's knobs decide
			// how many GPUs it holds, so carrying on with some of them would
			// leave an operator reading a number that is in force nowhere.
			logger.Info("ignoring a warm pool whose trigger metadata cannot be read",
				"scaledObject", entry.Name, "namespace", entry.Namespace, "err", err.Error())
			continue
		}
		out = append(out, poolSpecFrom(meta, entry, r.Fallback))
	}
	if len(out) == 0 {
		return []PoolSpec{{Name: "", Config: r.Fallback}}, nil
	}
	// Ordered so two pools resolve the same way on every pass and every process.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// poolSpecFrom layers a pool's trigger over the flag defaults.
func poolSpecFrom(meta registry.PoolMeta, entry registry.Entry, fallback policy.Config) PoolSpec {
	spec := PoolSpec{
		Name:                 meta.Name,
		Config:               fallback,
		Deployment:           entry.Target.Name,
		GPUMemoryUtilization: meta.GPUMemoryUtilization,
	}
	if meta.SleepMinSize != nil {
		spec.Config.SleepMinSize = *meta.SleepMinSize
	}
	if meta.MaxHold != nil {
		spec.Config.MaxHold = *meta.MaxHold
	}
	if meta.PreloadTop != nil {
		spec.Config.PreloadTop = *meta.PreloadTop
	}
	if entry.Target.MaxReplicas != nil {
		spec.MaxReplicas = int(*entry.Target.MaxReplicas)
	}
	if entry.Target.MinReplicas != nil {
		// The floor is the best available reading of current size when nothing
		// else says: KEDA holds the Deployment at or above it.
		spec.Replicas = int(*entry.Target.MinReplicas)
	}
	return spec
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

// VariantsFor returns the variants that may borrow from one pool.
//
// The rule is that selection is only needed where there is real ambiguity:
//
//	one pool, variant names none    -> it takes that pool
//	variant names this pool         -> it takes this pool
//	variant names another pool      -> not this pool's concern
//	several pools, names none       -> NO pool, reported by Unassignable
//	names a pool that does not exist -> NO pool, reported by Unassignable
//
// The first line is the whole point. A namespace with one pool is the common
// case by a wide margin, and requiring every ScaledObject to name it would be
// ceremony that buys nothing. The fourth is the reason the rule cannot simply
// default to "the first pool": with two pools of different accelerators, a guess
// is wrong half the time, and being wrong costs a ~35 s load that can never
// serve the variant that triggered it.
func VariantsFor(variants []policy.VariantDemand, spec PoolSpec, pools []PoolSpec) []policy.VariantDemand {
	out := make([]policy.VariantDemand, 0, len(variants))
	for _, v := range variants {
		if v.WarmPool == spec.Name || (v.WarmPool == "" && len(pools) == 1) {
			out = append(out, v)
		}
	}
	return out
}

// Unassignable reports the variants that will get no warm copy because their
// pool selection cannot be resolved, with the reason for each.
//
// Keyed by variant so a caller can report each one once. Silence here would be
// the worst kind: the variant scales normally and simply never runs warm, which
// looks exactly like a pool that is too small.
func Unassignable(variants []policy.VariantDemand, pools []PoolSpec) map[string]string {
	declared := make(map[string]bool, len(pools))
	names := make([]string, 0, len(pools))
	for _, p := range pools {
		declared[p.Name] = true
		names = append(names, p.Name)
	}
	sort.Strings(names)

	out := map[string]string{}
	for _, v := range variants {
		switch {
		case v.WarmPool == "" && len(pools) > 1:
			out[v.Model.Variant] = fmt.Sprintf(
				"names no warm pool and this namespace has %d (%s); set the %q trigger metadata key",
				len(pools), strings.Join(names, ", "), registry.WarmPoolKey)
		case v.WarmPool != "" && !declared[v.WarmPool]:
			out[v.Model.Variant] = fmt.Sprintf(
				"names warm pool %q, which no pool Deployment declares (have: %s)",
				v.WarmPool, strings.Join(names, ", "))
		}
	}
	return out
}

// UndeclaredPools finds warm pool Deployments that no ScaledObject declares.
//
// This is the price of moving the declaration to the ScaledObject, paid
// deliberately. A pool Deployment is still what holds the accelerators, so one
// left behind after its ScaledObject is deleted goes on holding them while WVA
// no longer knows it exists. That is a silent GPU leak, and the whole point of
// this configuration surface is that nothing about a pool should be silent.
//
// Diagnostics only. It reads Deployments to REPORT them, never to configure
// anything, so there remains exactly one place a pool is defined.
type UndeclaredPools struct {
	Client    client.Client
	Namespace string
}

// Find returns the names of pool Deployments not declared by any of the pools.
func (u *UndeclaredPools) Find(ctx context.Context, declared []PoolSpec) ([]string, error) {
	var deployments appsv1.DeploymentList
	if err := u.Client.List(ctx, &deployments,
		client.InNamespace(u.Namespace),
		client.MatchingLabels{pool.ComponentLabel: pool.ComponentValue},
	); err != nil {
		return nil, fmt.Errorf("list pool deployments: %w", err)
	}

	claimed := make(map[string]bool, len(declared))
	for _, spec := range declared {
		if spec.Deployment != "" {
			claimed[spec.Deployment] = true
		}
	}

	var orphans []string
	for i := range deployments.Items {
		if name := deployments.Items[i].Name; !claimed[name] {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	return orphans, nil
}
