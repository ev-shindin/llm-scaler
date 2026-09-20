// Package kueue reads GPU quotas from Kueue and presents them in the shape the
// quota limiter's static maps use, so a quota entry can be bounded by what Kueue
// would admit.
//
// It reads three kinds through the API server, unstructured, so the project
// carries no Kueue module dependency and follows whichever API version the
// cluster serves (the fields used are identical across v1beta1 and v1beta2):
//
//   - ResourceFlavor (cluster-scoped): spec.nodeLabels names the accelerator
//     product, e.g. nvidia.com/gpu.product: NVIDIA-H100-80GB-HBM3.
//   - ClusterQueue (cluster-scoped): spec.resourceGroups[].flavors[].resources[]
//     carries nominalQuota per flavor per extended resource.
//   - LocalQueue (namespaced): spec.clusterQueue links a namespace to a
//     ClusterQueue.
//
// Nothing here is watched. Reads happen on demand, at most once per refresh
// interval, and the last snapshot is kept so a failed read leaves the previous
// figures in force rather than none.
package kueue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

// Group is the Kueue API group. The version is resolved from the cluster's
// RESTMapper at read time, never hardcoded.
const Group = "kueue.x-k8s.io"

// Kinds read by the Reader.
const (
	KindClusterQueue   = "ClusterQueue"
	KindLocalQueue     = "LocalQueue"
	KindResourceFlavor = "ResourceFlavor"
)

// Options configures a Reader.
type Options struct {
	// Resources are the extended-resource names that count as GPUs. Empty means
	// every constants.VendorResources resource name.
	Resources []string

	// RefreshInterval is the minimum time between two reads of the API; a read
	// inside the interval returns the last snapshot. Non-positive means every
	// call reads.
	RefreshInterval time.Duration

	// Namespace, when set, restricts the LocalQueue listing to that namespace.
	// A namespace-scoped controller can list LocalQueues nowhere else, and a
	// cluster-wide list from it would be Forbidden — an error, not an empty
	// result (Forbidden is not absent). ClusterQueues and ResourceFlavors are
	// cluster-scoped and are always listed cluster-wide.
	Namespace string

	// now is the clock; nil means time.Now. Tests inject one.
	now func() time.Time
}

// Reader reads Kueue quotas and caches the last snapshot.
type Reader struct {
	client   client.Client
	opts     Options
	gpuNames map[string]bool

	mu      sync.Mutex
	last    config.ExternalQuotas // ObservedAt zero until the first successful read
	lastTry time.Time
	lastErr error
}

// NewReader constructs a Reader over the given client. Reads of unstructured
// objects through a controller-runtime client go to the API server directly
// unless the manager was told to cache unstructured kinds, which this project
// does not do — so no informer is started for Kueue objects.
func NewReader(c client.Client, opts Options) *Reader {
	names := make(map[string]bool, len(opts.Resources))
	for _, r := range opts.Resources {
		names[r] = true
	}
	if len(names) == 0 {
		for _, v := range constants.VendorResources {
			names[v.ResourceName] = true
		}
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	return &Reader{client: c, opts: opts, gpuNames: names}
}

// Quotas returns the current snapshot, reading Kueue if the last read is older
// than the refresh interval. On a failed read it returns the last good snapshot
// together with the error; ObservedAt on the result tells the caller how stale
// that snapshot is, and is zero when no read has ever succeeded — in which case
// the snapshot is empty and must not be taken as "Kueue grants nothing".
func (r *Reader) Quotas(ctx context.Context) (config.ExternalQuotas, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.opts.now()
	if !r.lastTry.IsZero() && r.opts.RefreshInterval > 0 && now.Sub(r.lastTry) < r.opts.RefreshInterval {
		return r.last, r.lastErr
	}
	r.lastTry = now
	quotas, err := r.read(ctx)
	if err != nil {
		r.lastErr = err
		return r.last, err
	}
	quotas.ObservedAt = now
	r.last, r.lastErr = quotas, nil
	return quotas, nil
}

// read performs one full read: flavors → type names, ClusterQueues → per-type
// caps, LocalQueues → namespace attribution. Any failure aborts the whole read;
// a partial snapshot (say, queues without their flavors) would mis-name every
// type and deny everything, which is worse than keeping the previous one.
func (r *Reader) read(ctx context.Context) (config.ExternalQuotas, error) {
	flavors, err := r.list(ctx, KindResourceFlavor)
	if err != nil {
		return config.ExternalQuotas{}, err
	}
	flavorType := make(map[string]string, len(flavors))
	for i := range flavors {
		flavorType[flavors[i].GetName()] = flavorAcceleratorType(&flavors[i])
	}

	queues, err := r.list(ctx, KindClusterQueue)
	if err != nil {
		return config.ExternalQuotas{}, err
	}
	perQueue := make(map[string]map[string]int, len(queues))
	cluster := make(map[string]int)
	for i := range queues {
		caps, err := r.clusterQueueCaps(&queues[i], flavorType)
		if err != nil {
			return config.ExternalQuotas{}, err
		}
		if len(caps) == 0 {
			continue // declares no GPU resource: this queue governs no accelerator
		}
		perQueue[queues[i].GetName()] = caps
		for accType, n := range caps {
			cluster[accType] = addCapped(cluster[accType], n)
		}
	}

	var listOpts []client.ListOption
	if r.opts.Namespace != "" {
		listOpts = append(listOpts, client.InNamespace(r.opts.Namespace))
	}
	localQueues, err := r.list(ctx, KindLocalQueue, listOpts...)
	if err != nil {
		return config.ExternalQuotas{}, err
	}
	// A namespace with two LocalQueues on one ClusterQueue has that queue's
	// budget once, not twice: the budget belongs to the ClusterQueue.
	seen := make(map[string]map[string]bool)
	byNamespace := make(map[string]map[string]int)
	for i := range localQueues {
		lq := &localQueues[i]
		ns := lq.GetNamespace()
		cqName, _, _ := unstructured.NestedString(lq.Object, "spec", "clusterQueue")
		if cqName == "" {
			continue
		}
		if seen[ns] == nil {
			seen[ns] = make(map[string]bool)
		}
		if seen[ns][cqName] {
			continue
		}
		seen[ns][cqName] = true
		caps, ok := perQueue[cqName]
		if !ok {
			// A dangling reference, or a queue that declares no GPU resource at
			// all (a CPU batch queue). Neither says anything about GPUs, so the
			// namespace is left ungoverned here and the static entry decides.
			// Listing it with nothing granted would instead zero every inference
			// namespace on a cluster that uses Kueue for CPU jobs. A queue that
			// DOES declare a GPU resource with nominalQuota 0 is a real grant of
			// nothing and is kept.
			continue
		}
		if byNamespace[ns] == nil {
			byNamespace[ns] = make(map[string]int, len(caps))
		}
		for accType, n := range caps {
			byNamespace[ns][accType] = addCapped(byNamespace[ns][accType], n)
		}
	}
	return config.ExternalQuotas{Namespace: byNamespace, Cluster: cluster}, nil
}

// list fetches every object of a Kueue kind at whatever version the cluster
// serves. A cluster without Kueue is reported as such rather than as an empty
// list: an operator who enabled the reader must learn it found nothing to read.
func (r *Reader) list(ctx context.Context, kind string, opts ...client.ListOption) ([]unstructured.Unstructured, error) {
	mapping, err := r.client.RESTMapper().RESTMapping(schema.GroupKind{Group: Group, Kind: kind})
	if err != nil {
		if meta.IsNoMatchError(err) {
			return nil, fmt.Errorf("kueue: %s.%s is not served by this cluster (is Kueue installed?): %w", kind, Group, err)
		}
		return nil, fmt.Errorf("kueue: resolving %s: %w", kind, err)
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(mapping.GroupVersionKind.GroupVersion().WithKind(kind + "List"))
	if err := r.client.List(ctx, list, opts...); err != nil {
		return nil, fmt.Errorf("kueue: listing %s: %w", kind, err)
	}
	return list.Items, nil
}

// clusterQueueCaps sums a ClusterQueue's nominalQuota over its GPU resources,
// per accelerator type. Two flavors of the same type (an SXM and a PCIe H100
// flavor, say) add up, as the static maps could not tell them apart either.
func (r *Reader) clusterQueueCaps(cq *unstructured.Unstructured, flavorType map[string]string) (map[string]int, error) {
	groups, _, err := unstructured.NestedSlice(cq.Object, "spec", "resourceGroups")
	if err != nil {
		return nil, fmt.Errorf("kueue: ClusterQueue %q: spec.resourceGroups: %w", cq.GetName(), err)
	}
	caps := make(map[string]int)
	for _, g := range groups {
		group, _ := g.(map[string]interface{})
		flavors, _, _ := unstructured.NestedSlice(group, "flavors")
		for _, f := range flavors {
			flavor, _ := f.(map[string]interface{})
			name, _, _ := unstructured.NestedString(flavor, "name")
			resources, _, _ := unstructured.NestedSlice(flavor, "resources")
			for _, res := range resources {
				rq, _ := res.(map[string]interface{})
				resName, _, _ := unstructured.NestedString(rq, "name")
				if !r.gpuNames[resName] {
					continue
				}
				n, err := quantityField(rq, "nominalQuota")
				if err != nil {
					return nil, fmt.Errorf("kueue: ClusterQueue %q flavor %q resource %q: %w", cq.GetName(), name, resName, err)
				}
				accType, ok := flavorType[name]
				if !ok || accType == "" {
					// No ResourceFlavor, or one without a product label: the
					// flavor name is the type. Operators commonly name flavors
					// after the model ("h100"), and sameAccelerator matches that
					// to a static "H100" ignoring case.
					accType = name
				}
				caps[accType] = addCapped(caps[accType], n)
			}
		}
	}
	return caps, nil
}

// flavorAcceleratorType derives the accelerator type from a ResourceFlavor's
// node labels — the product label of any known vendor (or its aliases), reduced
// to the short name the static maps and usage keys normalize to. Empty when the
// flavor carries no product label.
func flavorAcceleratorType(rf *unstructured.Unstructured) string {
	labels, _, _ := unstructured.NestedStringMap(rf.Object, "spec", "nodeLabels")
	for _, v := range constants.VendorResources {
		keys := append([]string{v.ProductLabel}, v.ProductLabelAliases...)
		for _, k := range keys {
			if product := labels[k]; product != "" {
				return accelerator.NormalizeAcceleratorName(product)
			}
		}
	}
	return ""
}

// quantityField parses a resource.Quantity field of an unstructured map into a
// whole GPU count, clamped to config.MaxQuotaValue. The API server stores a
// Quantity as a string, but a hand-built object (tests, or a future typed
// conversion) may carry a number, so both are accepted.
func quantityField(m map[string]interface{}, field string) (int, error) {
	raw, ok := m[field]
	if !ok || raw == nil {
		return 0, fmt.Errorf("%s is missing", field)
	}
	var q resource.Quantity
	switch v := raw.(type) {
	case string:
		parsed, err := resource.ParseQuantity(v)
		if err != nil {
			return 0, fmt.Errorf("%s %q: %w", field, v, err)
		}
		q = parsed
	case int64:
		q = *resource.NewQuantity(v, resource.DecimalSI)
	case float64:
		q = *resource.NewMilliQuantity(int64(v*1000), resource.DecimalSI)
	default:
		return 0, fmt.Errorf("%s has unexpected type %T", field, raw)
	}
	if q.Sign() < 0 {
		return 0, errors.New(field + " is negative")
	}
	// Value rounds up, so a fractional quota (which Kueue allows for other
	// resources and rejects for none) counts the GPU it partially names.
	return clampQuota(q.Value()), nil
}

// addCapped adds two caps without exceeding config.MaxQuotaValue.
func addCapped(a, b int) int {
	return clampQuota(int64(a) + int64(b))
}

// clampQuota bounds a figure to config.MaxQuotaValue, the largest finite cap
// the limiter accepts. Anything Kueue declares beyond it is a cap that will
// never bind, and clamping keeps the aggregate sums clear of overflow.
func clampQuota(n int64) int {
	if n > config.MaxQuotaValue {
		return config.MaxQuotaValue
	}
	return int(n)
}
