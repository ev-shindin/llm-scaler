package fixtures

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// kueueGroup mirrors internal/kueue.Group; restated so the fixtures package
// does not depend on the controller's internal packages.
const kueueGroup = "kueue.x-k8s.io"

// KueueQuota is the smallest Kueue arrangement that grants a namespace GPUs: a
// ResourceFlavor naming an accelerator, a ClusterQueue with a nominal quota on
// that flavor, and a LocalQueue tying the namespace to the ClusterQueue. The
// three share Name.
type KueueQuota struct {
	// Name is used for all three objects.
	Name string
	// Namespace holds the LocalQueue -- the namespace being granted the quota.
	Namespace string
	// ProductLabel and Product are the node label the ResourceFlavor carries,
	// e.g. nvidia.com/gpu.product = NVIDIA-H100-80GB-HBM3. The reader turns the
	// value into the accelerator type the quota is keyed by.
	ProductLabel, Product string
	// Resource is the extended resource the quota counts, e.g. nvidia.com/gpu.
	Resource string
	// GPUs is the ClusterQueue's nominalQuota for Resource on the flavor.
	GPUs int
}

// KueueInstalled reports whether the cluster serves the Kueue kinds the reader
// lists. A cluster without them is a test-environment defect for a spec that
// needs them, not a reason to pass quietly: the caller should Fail, not Skip.
func KueueInstalled(c client.Client) bool {
	for _, kind := range []string{"ClusterQueue", "LocalQueue", "ResourceFlavor"} {
		if _, err := kueueVersion(c, kind); err != nil {
			return false
		}
	}
	return true
}

// kueueVersion resolves the API version the cluster serves for a Kueue kind, as
// the reader under test does, so the fixture writes whichever of v1beta1 and
// v1beta2 this cluster prefers.
func kueueVersion(c client.Client, kind string) (schema.GroupVersionKind, error) {
	mapping, err := c.RESTMapper().RESTMapping(schema.GroupKind{Group: kueueGroup, Kind: kind})
	if err != nil {
		if meta.IsNoMatchError(err) {
			return schema.GroupVersionKind{}, fmt.Errorf("kueue kind %s is not served (are the Kueue CRDs installed?): %w", kind, err)
		}
		return schema.GroupVersionKind{}, err
	}
	return mapping.GroupVersionKind, nil
}

// CreateKueueQuota creates the flavor, ClusterQueue and LocalQueue. Objects
// that already exist (a previous run that did not clean up) are replaced.
func CreateKueueQuota(ctx context.Context, c client.Client, q KueueQuota) error {
	for _, obj := range kueueObjects(q) {
		gvk, err := kueueVersion(c, obj.GetKind())
		if err != nil {
			return err
		}
		obj.SetGroupVersionKind(gvk)
		if err := c.Create(ctx, obj); err != nil {
			if !errors.IsAlreadyExists(err) {
				return fmt.Errorf("create %s %s: %w", obj.GetKind(), obj.GetName(), err)
			}
			if err := c.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("replace %s %s: %w", obj.GetKind(), obj.GetName(), err)
			}
			// Where Kueue's controller runs its objects carry finalizers, so the
			// delete is not immediate and an immediate Create fails with
			// AlreadyExists. Wait for it to be gone.
			if err := waitGone(ctx, c, obj); err != nil {
				return err
			}
			if err := c.Create(ctx, obj); err != nil {
				return fmt.Errorf("recreate %s %s: %w", obj.GetKind(), obj.GetName(), err)
			}
		}
	}
	return nil
}

// waitGone polls until obj no longer exists, for up to a minute.
func waitGone(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
	probe := &unstructured.Unstructured{}
	probe.SetGroupVersionKind(obj.GroupVersionKind())
	deadline := time.Now().Add(time.Minute)
	for {
		err := c.Get(ctx, client.ObjectKeyFromObject(obj), probe)
		if errors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("waiting for %s %s to be deleted: %w", obj.GetKind(), obj.GetName(), err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s %s still exists a minute after deletion (finalizers?)", obj.GetKind(), obj.GetName())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// DeleteKueueLocalQueue removes only the LocalQueue, which is how a namespace
// stops being governed by Kueue while the ClusterQueue and its quota stay.
func DeleteKueueLocalQueue(ctx context.Context, c client.Client, q KueueQuota) error {
	objs := kueueObjects(q)
	return deleteKueueObject(ctx, c, objs[2])
}

// DeleteKueueQuota removes all three objects; missing ones are not an error.
func DeleteKueueQuota(ctx context.Context, c client.Client, q KueueQuota) error {
	for _, obj := range kueueObjects(q) {
		if err := deleteKueueObject(ctx, c, obj); err != nil {
			return err
		}
	}
	return nil
}

func deleteKueueObject(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
	gvk, err := kueueVersion(c, obj.GetKind())
	if err != nil {
		return err
	}
	obj.SetGroupVersionKind(gvk)
	if err := c.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete %s %s: %w", obj.GetKind(), obj.GetName(), err)
	}
	return nil
}

// kueueObjects builds the three objects, in creation order: flavor, ClusterQueue,
// LocalQueue. The API version is left for the caller to resolve.
func kueueObjects(q KueueQuota) []*unstructured.Unstructured {
	flavor := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": q.Name},
		"spec": map[string]interface{}{
			"nodeLabels": map[string]interface{}{q.ProductLabel: q.Product},
		},
	}}
	flavor.SetKind("ResourceFlavor")

	cq := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": q.Name},
		"spec": map[string]interface{}{
			"namespaceSelector": map[string]interface{}{},
			"resourceGroups": []interface{}{
				map[string]interface{}{
					"coveredResources": []interface{}{q.Resource},
					"flavors": []interface{}{
						map[string]interface{}{
							"name": q.Name,
							"resources": []interface{}{
								map[string]interface{}{
									"name":         q.Resource,
									"nominalQuota": strconv.Itoa(q.GPUs),
								},
							},
						},
					},
				},
			},
		},
	}}
	cq.SetKind("ClusterQueue")

	lq := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": q.Name, "namespace": q.Namespace},
		"spec":     map[string]interface{}{"clusterQueue": q.Name},
	}}
	lq.SetKind("LocalQueue")

	return []*unstructured.Unstructured{flavor, cq, lq}
}
