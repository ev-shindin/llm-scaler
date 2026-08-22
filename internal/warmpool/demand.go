package warmpool

import (
	"context"
	"errors"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/policy"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

// Demand answers what each variant wants, entirely from decisions WVA has
// already made. It computes nothing about scaling and holds no opinion about
// it: the desired count is read from the decision store, the ready count from
// the scale target.
//
// That separation is the accounting rule from the design, made structural. The
// pool must never be able to influence how much a variant scales -- only how
// quickly that capacity arrives -- and a demand source that recomputed desire
// would be the obvious way for it to start doing so.
type Demand struct {
	Registry  *registry.Registry
	Decisions *decision.Store
	Client    client.Client
	Datastore datastore.Datastore
}

// Variants reports every registered variant the pool could act on.
//
// A variant with no derivable engine options is skipped rather than guessed at:
// a warm copy started with different options than the ordinary replicas is a
// different torch.compile cache key, which costs ~9 s of extra compile and
// quietly makes the pool slower than it should be.
func (d *Demand) Variants(ctx context.Context) ([]policy.VariantDemand, error) {
	logger := log.FromContext(ctx).WithName("warmpool-demand")

	var out []policy.VariantDemand
	for _, entry := range d.Registry.Snapshot() {
		if entry.Target.Name == "" {
			continue // never enriched; nothing to scale and nothing to bridge
		}
		target := types.NamespacedName{Namespace: entry.Namespace, Name: entry.Target.Name}

		var workload appsv1.Deployment
		if err := d.Client.Get(ctx, target, &workload); err != nil {
			logger.V(2).Info("skipping variant: cannot read its scale target",
				"variant", entry.Name, "target", target, "err", err)
			continue
		}

		options, err := EngineOptionsFrom(&workload.Spec.Template.Spec)
		if err != nil {
			logger.V(2).Info("skipping variant: no engine options",
				"variant", entry.Name, "err", err)
			continue
		}

		labels, err := d.poolLabels(entry.Namespace, workload.Spec.Template.Labels)
		if err != nil {
			logger.V(2).Info("skipping variant: cannot resolve its InferencePool",
				"variant", entry.Name, "err", err)
			continue
		}

		desired := 0
		if decided, ok := d.Decisions.Get(entry.Namespace, entry.Target.Name); ok {
			desired = int(decided.DesiredReplicas)
		}
		ready := int(workload.Status.ReadyReplicas)

		out = append(out, policy.VariantDemand{
			Model: pool.ModelRef{
				Namespace:     entry.Namespace,
				Variant:       entry.Name,
				EngineOptions: options,
				PoolLabels:    labels,
			},
			Desired: desired,
			Ready:   ready,
			// Parked is the case with no alternative: at zero replicas a cold
			// start is the whole of the first request's latency.
			Parked: desired == 0 && ready == 0,
			// Share stays zero until a popularity source is wired in. Preloading
			// is therefore inert rather than wrong -- admission still works from
			// parking and from the frequency filter.
			Share: 0,
		})
	}
	return out, nil
}

// poolLabels resolves the InferencePool a workload belongs to and returns that
// pool's OWN selector.
//
// Read rather than assumed: a selector belongs to the tenant, and one that
// requires something other than llm-d.ai/model is not hypothetical. A pool Pod
// joins by carrying exactly these labels.
func (d *Demand) poolLabels(namespace string, workloadLabels map[string]string) (map[string]string, error) {
	if len(workloadLabels) == 0 {
		return nil, errors.New("workload has no pod-template labels to match a pool with")
	}
	found, err := d.Datastore.PoolGetFromLabels(namespace, workloadLabels)
	if err != nil {
		return nil, err
	}
	if found == nil || len(found.Selector) == 0 {
		return nil, fmt.Errorf("InferencePool for %s has no selector", namespace)
	}
	labels := make(map[string]string, len(found.Selector))
	for k, v := range found.Selector {
		labels[k] = v
	}
	return labels, nil
}

// EngineOptionsFrom derives the vLLM command line for a warm copy from the
// ordinary replicas' own pod spec.
//
// Derived rather than configured, because the two must MATCH: a different
// --gpu-memory-utilization is a different torch.compile cache key, and a miss
// costs ~9 s of compile on top of the load. Configuring it separately would
// make that divergence silent and permanent.
func EngineOptionsFrom(spec *corev1.PodSpec) (string, error) {
	for i := range spec.Containers {
		container := &spec.Containers[i]
		joined := strings.TrimSpace(strings.Join(append(append([]string{}, container.Command...), container.Args...), " "))
		if !strings.Contains(joined, "--model") {
			continue
		}
		return normaliseOptions(joined), nil
	}
	return "", errors.New("no container names a --model")
}

// normaliseOptions strips what the pool assigns itself and adds what a warm copy
// cannot do without.
func normaliseOptions(options string) string {
	fields := strings.Fields(options)
	out := make([]string, 0, len(fields)+1)
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		switch {
		case f == "--port":
			// The pool assigns ports from its own range: instances share a Pod,
			// and a port taken from the workload would collide with the copy
			// already listening on it.
			i++
		case strings.HasPrefix(f, "--port="):
		case f == "vllm" || f == "serve" || strings.HasSuffix(f, "python") || strings.HasSuffix(f, "python3"):
			// The supervisor starts the engine itself; only the arguments carry.
		case strings.HasPrefix(f, "-m") && i+1 < len(fields) && fields[i+1] == "vllm.entrypoints.openai.api_server":
			i++
		default:
			out = append(out, f)
		}
	}
	joined := strings.Join(out, " ")
	if !strings.Contains(joined, "--enable-sleep-mode") {
		// Without this the copy cannot sleep, which is the entire mechanism.
		joined += " --enable-sleep-mode"
	}
	return strings.TrimSpace(joined)
}
