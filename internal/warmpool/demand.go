package warmpool

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
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
	// GPUMemoryUtilization overrides what the ordinary replicas ask for, and
	// exists because the two are sizing different things.
	//
	// A workload's value is chosen for a Pod running ONE engine, so it is
	// usually near 0.95 -- vLLM then CLAIMS that share of the card and fills
	// the remainder with KV cache. A pool Pod runs one awake engine plus its
	// sleepers, and each sleeper keeps ~1.4 GiB of GPU residue. Measured: at
	// 0.95 an 80 GiB card had 4.4 GiB left, which is room for about three
	// sleepers, not the sixteen MaxInstancesPerPod allows. That limit is a port
	// range and was never a memory bound; THIS is the memory bound.
	//
	// It is not free. The value is part of the torch.compile cache key, so a
	// pool that uses a different one misses the cache the ordinary replicas
	// populated -- measured at 12.35 s against 3.08 s. That cost is paid once
	// per model per value, not per load, which is what makes the trade worth
	// having: nine seconds on a model's first admission buys a warm set several
	// times larger.
	//
	// Zero inherits the workload's value, which is the previous behaviour.
	GPUMemoryUtilization float64

	// Namespace is the pool's, and variants outside it are not its business.
	//
	// Load-bearing rather than tidy. The registry spans every namespace WVA
	// watches, while an Adapter is pinned to one -- and a variant is keyed by
	// the ScaledObject's NAME, which is only unique within a namespace. Without
	// this filter a variant called "decode" in one tenant's namespace and
	// another called "decode" somewhere else are the same key to the warm set,
	// so one tenant's traffic could be pointed at the other's engine.
	Namespace string
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
		if d.Namespace != "" && entry.Namespace != d.Namespace {
			continue // another namespace's variant; not this pool's to warm
		}
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
		options = withGPUMemoryUtilization(options, d.GPUMemoryUtilization)

		labels, err := d.poolLabels(entry.Namespace, workload.Spec.Template.Labels)
		if err != nil {
			logger.V(2).Info("skipping variant: cannot resolve its InferencePool",
				"variant", entry.Name, "err", err)
			continue
		}

		decided, haveDecision := d.Decisions.Get(entry.Namespace, entry.Target.Name)
		desired := 0
		if haveDecision {
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
			// Share is filled in below, once every variant's desire is known.
			// It cannot be computed one variant at a time.
			// Parked is the case with no alternative: at zero replicas a cold
			// start is the whole of the first request's latency.
			//
			// A DECISION is required, not merely two zeroes. Before WVA has
			// decided anything -- every variant, on every restart -- desired
			// reads 0, and a workload that has not started yet reads 0 ready.
			// Treating that as parked would eagerly admit models nobody asked
			// for, at ~35 s and a reserve slot each, on every restart.
			Parked: haveDecision && desired == 0 && ready == 0,
		})
	}
	withShares(out)
	return out, nil
}

// withShares fills in each variant's share of the fleet, in place.
//
// What the design asks for is share of REQUESTS, and what is available in this
// process is share of desired replicas. They are the same quantity scaled by
// per-replica capacity: an autoscaler's whole job is to hold utilisation
// roughly constant, so a variant WVA wants ten replicas of is serving about ten
// times the traffic of one it wants one of.
//
// The approximation breaks where per-replica capacity differs -- a variant on
// a smaller accelerator, or one whose model is heavy enough that a replica
// serves fewer requests, reads as more popular than it is. That is a
// distortion, not a hazard: the consequence of over-ranking a variant is that
// the pool preloads it, which costs a reserve slot it would otherwise have
// spent reactively. Nothing about correctness depends on this number.
//
// Zero desire contributes nothing, so a fleet that is entirely parked leaves
// every share at zero and preloading falls back to what it was before: inert,
// with admission driven by parking and the frequency filter.
func withShares(variants []policy.VariantDemand) {
	total := 0
	for _, v := range variants {
		if v.Desired > 0 {
			total += v.Desired
		}
	}
	if total == 0 {
		return
	}
	for i := range variants {
		if variants[i].Desired > 0 {
			variants[i].Share = float64(variants[i].Desired) / float64(total)
		}
	}
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
	return maps.Clone(found.Selector), nil
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

// warmableFlags are the vLLM options a warm copy may inherit from a workload.
//
// An ALLOWLIST, and it has to be. These options are copied out of a Deployment
// the tenant owns, and they become the argv of a process the pool runs on a GPU
// with the shared model cache mounted -- so the flag set is an execution
// surface, not a configuration detail. vLLM has several that load and run code
// the caller names: --trust-remote-code, --tool-parser-plugin,
// --logits-processors, --model-loader-extra-config, --speculative-config. It
// has others that read arbitrary local files through the serving port
// (--allowed-local-media-path) or send data off-cluster
// (--otlp-traces-endpoint), and the pool's NetworkPolicy governs ingress only.
//
// Denying those individually would mean tracking a moving target across vLLM
// releases that arrive in a third-party image. Naming what may pass does not.
//
// The list is what a warm copy must MATCH to be the same engine: anything that
// changes the model, its shape, or its memory arithmetic. A flag missing from
// here is not a bug in the tenant's workload, it is a warm copy the pool
// declines to make -- which costs a cold start and nothing else.
var warmableFlags = map[string]bool{
	"--model":                    true,
	"--served-model-name":        true,
	"--tokenizer":                true,
	"--tokenizer-mode":           true,
	"--revision":                 true,
	"--code-revision":            true,
	"--tokenizer-revision":       true,
	"--dtype":                    true,
	"--quantization":             true,
	"--kv-cache-dtype":           true,
	"--max-model-len":            true,
	"--max-num-seqs":             true,
	"--max-num-batched-tokens":   true,
	"--tensor-parallel-size":     true,
	"--pipeline-parallel-size":   true,
	"--data-parallel-size":       true,
	"--data-parallel-size-local": true,
	"--data-parallel-backend":    true,
	// Expert parallelism does not ask for GPUs of its own -- it shards a
	// mixture-of-experts model's experts across the ranks tensor and data
	// parallelism already provide. It still has to be copied: a warm copy
	// without it lays the same model out differently from the replicas it
	// exists to match, which is a different torch.compile cache key and a
	// different memory profile, so the copy is neither as fast nor as large as
	// the thing it stands in for.
	"--enable-expert-parallel":       true,
	"--enable-eplb":                  true,
	"--num-redundant-experts":        true,
	"--gpu-memory-utilization":       true,
	"--swap-space":                   true,
	"--block-size":                   true,
	"--seed":                         true,
	"--enable-prefix-caching":        true,
	"--disable-log-requests":         true,
	"--disable-log-stats":            true,
	"--enforce-eager":                true,
	"--enable-chunked-prefill":       true,
	"--load-format":                  true,
	"--config-format":                true,
	"--max-seq-len-to-capture":       true,
	"--num-scheduler-steps":          true,
	"--distributed-executor-backend": true,
	"--enable-sleep-mode":            true,
}

// withGPUMemoryUtilization applies the pool's own value, if it has one.
//
// Replaces rather than appends when the workload already names it: two
// occurrences would leave the engine's behaviour up to argparse's last-wins
// rule, and the pool's intent up to a reader's guess.
func withGPUMemoryUtilization(options string, value float64) string {
	if value <= 0 {
		return options
	}
	const flag = "--gpu-memory-utilization"

	fields := strings.Fields(options)
	out := make([]string, 0, len(fields)+2)
	for i := 0; i < len(fields); i++ {
		name, _, hasInline := strings.Cut(fields[i], "=")
		if name != flag {
			out = append(out, fields[i])
			continue
		}
		if !hasInline && i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") {
			i++
		}
	}
	out = append(out, flag, strconv.FormatFloat(value, 'g', -1, 64))
	return strings.Join(out, " ")
}

// normaliseOptions keeps the flags a warm copy must match, drops everything
// else, and adds what it cannot do without.
//
// Dropping rather than refusing outright is deliberate: a workload carrying an
// unlisted flag still gets a warm copy of the model, just without that flag.
// The copy is then not identical to the ordinary replicas, which matters for
// the torch.compile cache key -- so the flags that DO affect it are exactly the
// ones on the list.
func normaliseOptions(options string) string {
	fields := strings.Fields(options)
	out := make([]string, 0, len(fields)+1)
	for i := 0; i < len(fields); i++ {
		name, inline, hasInline := strings.Cut(fields[i], "=")
		if !strings.HasPrefix(name, "-") {
			// A bare word: the launcher, the module, or a value whose flag was
			// dropped. Either way it is not ours to pass on.
			continue
		}
		if !warmableFlags[name] {
			// Skip the flag AND its value, or the value becomes a bare word
			// that a later parse could read as something else.
			if !hasInline && i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") {
				i++
			}
			continue
		}
		if hasInline {
			out = append(out, name+"="+inline)
			continue
		}
		out = append(out, name)
		if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") {
			out = append(out, fields[i+1])
			i++
		}
	}
	joined := strings.Join(out, " ")
	if !strings.Contains(joined, "--enable-sleep-mode") {
		// Without this the copy cannot sleep, which is the entire mechanism.
		joined += " --enable-sleep-mode"
	}
	return strings.TrimSpace(joined)
}
