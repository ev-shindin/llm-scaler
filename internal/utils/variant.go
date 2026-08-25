/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"context"
	"fmt"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	wvav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// VariantFilter is a function that determines if a VA should be included.
type VariantFilter func(scaletarget.ScaleTargetAccessor) bool

// ActiveVariantAutoscalingByModel retrieves all VariantAutoscaling resources that are ready for optimization
// and have at least one target replica.
// Returns the shallow-copied VAs (not safe for mutation) grouped by ModelID.
func ActiveVariantAutoscalingByModel(ctx context.Context, client client.Client, reg *registry.Registry) (map[string][]wvav1alpha1.VariantAutoscaling, error) {
	vas, _, err := ActiveVariantAutoscaling(ctx, client, reg)
	if err != nil {
		return nil, err
	}
	return GroupVariantAutoscalingByModel(vas), nil
}

// InactiveVariantAutoscalingByModel retrieves all VariantAutoscaling resources that are ready for optimization
// and have no target replicas.
// Returns the shallow-copied VAs (not safe for mutation) grouped by ModelID.
func InactiveVariantAutoscalingByModel(ctx context.Context, client client.Client, reg *registry.Registry) (map[string][]wvav1alpha1.VariantAutoscaling, error) {
	vas, _, err := InactiveVariantAutoscaling(ctx, client, reg)
	if err != nil {
		return nil, err
	}
	return GroupVariantAutoscalingByModel(vas), nil
}

// GroupVariantAutoscalingByModel groups VariantAutoscalings by model ID and namespace.
// Variants of the same model on different accelerators are grouped together to enable
// cost-based optimization (scale up cheaper variants, scale down expensive variants).
// The key format is "modelID|namespace".
func GroupVariantAutoscalingByModel(
	vas []wvav1alpha1.VariantAutoscaling,
) map[string][]wvav1alpha1.VariantAutoscaling {
	groups := make(map[string][]wvav1alpha1.VariantAutoscaling)
	for _, va := range vas {
		// Use modelID + namespace as key to group all variants of same model
		key := va.Spec.ModelID + "|" + va.Namespace
		groups[key] = append(groups[key], va)
	}
	return groups
}

// ActiveVariantAutoscaling retrieves all VariantAutoscaling resources that are ready for optimization
// and have at least one target replica.
// Returns a slice of deep-copied VariantAutoscaling objects.
// It also returns a map of scaleTargetAccessors keyed by "namespace/scaleTargetName".
func ActiveVariantAutoscaling(ctx context.Context, client client.Client, reg *registry.Registry) ([]wvav1alpha1.VariantAutoscaling, map[string]scaletarget.ScaleTargetAccessor, error) {
	return filterVariantsByScaleTargetAccessor(ctx, client, reg, isActive, "active")
}

// InactiveVariantAutoscaling retrieves all VariantAutoscaling resources that are ready for optimization
// and have no target replicas.
// Returns a slice of deep-copied VariantAutoscaling objects.
// It also returns a map of scaleTargetAccessors keyed by "namespace/scaleTargetName".
func InactiveVariantAutoscaling(ctx context.Context, client client.Client, reg *registry.Registry) ([]wvav1alpha1.VariantAutoscaling, map[string]scaletarget.ScaleTargetAccessor, error) {
	return filterVariantsByScaleTargetAccessor(ctx, client, reg, isInactive, "inactive")
}

// filterVariantsByScaleTargetAccessors is a generic function to filter VAs based on scaleTarget state.
// Returns filtered VAs and a map of scaleTargetAccessors keyed by "namespace/scaleTargetName".
func filterVariantsByScaleTargetAccessor(ctx context.Context, client client.Client, reg *registry.Registry, filter VariantFilter, filterName string) ([]wvav1alpha1.VariantAutoscaling, map[string]scaletarget.ScaleTargetAccessor, error) {
	readyVAs := readyVariantAutoscalings(ctx, reg)

	filteredVAs := make([]wvav1alpha1.VariantAutoscaling, 0, len(readyVAs))
	scaleTargetAccessors := make(map[string]scaletarget.ScaleTargetAccessor)

	for _, va := range readyVAs {
		// Check if the context is done
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}

		// Skip VAs without scaleTargetRef (required to know which deployment to look up)
		// TODO: Remove this check once scaleTargetRef.name is made a required field in the CRD.
		// This defensive check exists because the CRD currently allows empty scaleTargetRef,
		// but it should be enforced at the schema level instead.
		if va.Spec.ScaleTargetRef.Name == "" {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Skipping VA without scaleTargetRef", "namespace", va.Namespace, "name", va.Name)
			continue
		}

		scaleTargetName := va.Spec.ScaleTargetRef.Name
		scaleTargetAccessor, err := scaletarget.FetchScaleTarget(ctx, client, va.Name, va.Spec.ScaleTargetRef.Kind, scaleTargetName, va.Namespace)
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Deployment/LWS doesn't exist yet, this is expected for VAs without corresponding scale targets
				ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Scale target not found for VariantAutoscaling, skipping",
					"namespace", va.Namespace,
					"scaleTargetName", scaleTargetName,
					"vaName", va.Name)
			} else {
				// Unexpected error (permissions, network issues, etc.)
				ctrl.LoggerFrom(ctx).Error(err, "Failed to get scale target",
					"namespace", va.Namespace,
					"scaleTargetName", scaleTargetName,
					"vaName", va.Name)
			}
			continue
		}

		// Skip deleted scaleTargetAccessor
		if scaleTargetAccessor.GetDeletionTimestamp() != nil && !scaleTargetAccessor.GetDeletionTimestamp().IsZero() {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Skipping deleted scale target", "namespace", va.Namespace, "scaleTargetName", scaleTargetName)
			continue
		}

		// Apply the filter function
		if filter(scaleTargetAccessor) {
			filteredVAs = append(filteredVAs, va)
			// Store scaleTargetAccessor in map using namespace/scaleTargetName as key
			key := GetNamespacedKey(va.Namespace, scaleTargetName)
			scaleTargetAccessors[key] = scaleTargetAccessor
		}
	}
	ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Found filtered VariantAutoscaling resources",
		"filterType", filterName,
		"count", len(filteredVAs))

	return filteredVAs, scaleTargetAccessors, nil
}

// readyVariantAutoscalings retrieves all variants ready for optimization by
// synthesizing in-memory VariantAutoscaling objects from the registry — the set
// of workloads KEDA has called WVA about. When CONTROLLER_INSTANCE is configured,
// only variants whose trigger names a matching instance are returned, enabling
// multi-controller isolation.
//
// A nil registry yields nothing: there is no second discovery source any more.
// Nothing is listed here — the whole set comes from calls WVA has received.
//
// The k8sClient argument is retained for the caller's convenience (it threads
// through to the scale-target fetch in filterVariantsByScaleTargetAccessor) and
// is deliberately not used for discovery.
func readyVariantAutoscalings(ctx context.Context, reg *registry.Registry) []wvav1alpha1.VariantAutoscaling {
	logger := ctrl.LoggerFrom(ctx)

	// Keyed by what it scales, so two entries that somehow name the same workload
	// collapse rather than being optimized twice.
	byTarget := make(map[string]wvav1alpha1.VariantAutoscaling)
	for _, va := range registrySourcedVariants(ctx, reg) {
		byTarget[variantTargetKey(va)] = va
	}

	controllerInstance := metrics.GetControllerInstance()
	readyVAs := make([]wvav1alpha1.VariantAutoscaling, 0, len(byTarget))
	for _, va := range byTarget {
		// Filter by controller-instance label for multi-controller isolation.
		if controllerInstance != "" && va.Labels[constants.ControllerInstanceLabelKey] != controllerInstance {
			continue
		}
		readyVAs = append(readyVAs, va)
	}

	logger.V(logging.DEBUG).Info("Found variants ready for optimization",
		"count", len(readyVAs),
		"controllerInstance", controllerInstance)

	return readyVAs
}

// variantTargetKey identifies a variant by what it scales, so that the same
// workload found through two discovery paths collapses to one entry.
func variantTargetKey(va wvav1alpha1.VariantAutoscaling) string {
	return fmt.Sprintf("%s/%s/%s", va.Namespace, va.Spec.ScaleTargetRef.Kind, va.Spec.ScaleTargetRef.Name)
}

// registrySourcedVariants synthesizes in-memory VariantAutoscaling objects from
// the workloads KEDA has called WVA about.
//
// Nothing is listed here. Identity comes from the trigger metadata the call
// carried, and the scale target from the entry's enrichment — a read of the
// ScaledObject the Enricher has already done and cached, not one issued per
// cycle. An entry that has not been enriched yet is skipped rather than guessed
// at: without a resolved scale target there is nothing to collect metrics from
// or scale, and the next enrichment pass is at most one window away.
func registrySourcedVariants(ctx context.Context, reg *registry.Registry) []wvav1alpha1.VariantAutoscaling {
	if reg == nil {
		return nil
	}
	logger := ctrl.LoggerFrom(ctx)

	entries := reg.Snapshot()
	out := make([]wvav1alpha1.VariantAutoscaling, 0, len(entries))
	for _, entry := range entries {
		if registry.ScalesAWarmPool(entry.Metadata) {
			// A warm pool is a workload WVA sizes, not a model it optimizes. It
			// has no model, no engine options and no saturation to measure, so
			// letting it through would create a variant that every engine then
			// has to special-case.
			continue
		}
		meta, err := registry.ParseMeta(entry.Metadata)
		if err != nil {
			// The operator's only view of this: KEDA does not surface a scaler's
			// opinion of its trigger metadata anywhere on the ScaledObject. Logged
			// at DEBUG because the scale-from-zero loop reaches here at 10Hz and a
			// standing misconfiguration would otherwise flood the log.
			logger.V(logging.DEBUG).Info("Skipping a registered workload with unusable trigger metadata",
				"namespace", entry.Namespace, "scaledObject", entry.Name, "error", err.Error())
			continue
		}

		target := entry.Target
		if target.Name == "" {
			logger.V(logging.DEBUG).Info("Skipping a registered workload whose scale target is not resolved yet",
				"namespace", entry.Namespace, "scaledObject", entry.Name)
			continue
		}

		out = append(out, wvav1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{
				// The ScaledObject names the variant: there is one per scale target.
				Name:      entry.Name,
				Namespace: entry.Namespace,
				// The ScaledObject's labels come along: the accelerator lookup falls
				// back to them when the pod template names no GPU product, and
				// multi-controller isolation filters on the controller-instance
				// label below. Dropping them does not fail loudly — it silently
				// unresolves accelerators and empties the fleet.
				Labels: target.Labels,
				Annotations: map[string]string{
					annotations.Synthetic: "true",
				},
			},
			Spec: wvav1alpha1.VariantAutoscalingSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					APIVersion: defaulted(target.APIVersion, "apps/v1"),
					Kind:       defaulted(target.Kind, constants.DeploymentKind),
					Name:       target.Name,
				},
				ModelID:       meta.ModelID,
				ScalingPolicy: meta.ScalingPolicy,
				MinReplicas:   target.MinReplicas,
				// MaxReplicas is a plain int32 on the spec, and enrichment always
				// resolves one (KEDA's own default fills an omitted
				// maxReplicaCount), so a nil here means the entry has not been
				// enriched yet — which the scale-target check below rejects anyway.
				MaxReplicas: derefOr(target.MaxReplicas, 0),
				VariantAutoscalingConfigSpec: wvav1alpha1.VariantAutoscalingConfigSpec{
					VariantCost: meta.VariantCost,
				},
			},
		})
	}
	return out
}

// defaulted returns v, or fallback when v is empty. Applies KEDA's own
// scaleTargetRef defaults for an entry whose enrichment has not run.
func defaulted(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// derefOr returns *v, or fallback when v is nil.
func derefOr(v *int32, fallback int32) int32 {
	if v == nil {
		return fallback
	}
	return *v
}

// isActive explicitly requires that replicas > 0
func isActive(scaleTargetAccessor scaletarget.ScaleTargetAccessor) bool {
	return GetDesiredReplicas(scaleTargetAccessor) > 0
}

// isInactive explicitly requires that replicas == 0
func isInactive(scaleTargetAccessor scaletarget.ScaleTargetAccessor) bool {
	return GetDesiredReplicas(scaleTargetAccessor) == 0
}

// Helper function makes behavior explicit
func GetDesiredReplicas(scaleTargetAccessor scaletarget.ScaleTargetAccessor) int32 {
	if scaleTargetAccessor.GetReplicas() == nil {
		return 1 // Kubernetes default
	}
	return *scaleTargetAccessor.GetReplicas()
}

// GetNamespacedKey is a helper for building namespaced resource keys.
func GetNamespacedKey(namespace, name string) string {
	return namespace + "/" + name
}
