package fixtures

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// SaturationConfigMap is where the limiters: list lives. The controller watches
// it, so a quota declared here takes effect without a restart.
const SaturationConfigMap = "wva-saturation-scaling-config"

// SetNamespaceQuota declares a namespace-scoped GPU quota and returns a restore
// func.
//
// The quota is what bounds WVA's own consumption, and a warm pool is part of
// that consumption -- so this is how a spec creates the condition where a pool
// wants to grow and the namespace cannot pay for it.
//
// Merges into whatever the ConfigMap already holds rather than replacing it: the
// suite shares one controller and one ConfigMap, and a spec that overwrote the
// whole document would silently disable everything else configured in it.
func SetNamespaceQuota(
	ctx context.Context,
	clientset *kubernetes.Clientset,
	configNamespace, targetNamespace, accelerator string,
	gpus int,
) (func(context.Context) error, error) {
	cms := clientset.CoreV1().ConfigMaps(configNamespace)

	existing, err := cms.Get(ctx, SaturationConfigMap, metav1.GetOptions{})
	existed := err == nil
	if err != nil && !errors.IsNotFound(err) {
		return nil, fmt.Errorf("read %s: %w", SaturationConfigMap, err)
	}

	var original *corev1.ConfigMap
	data := map[string]string{}
	if existed {
		original = existing.DeepCopy()
		for k, v := range existing.Data {
			data[k] = v
		}
	}

	// Parsed and re-marshalled rather than string-spliced: the document carries
	// other keys, and appending YAML text to a file whose indentation is not
	// known is how a config edit silently becomes a parse failure.
	doc := map[string]any{}
	if raw, ok := data["config.yaml"]; ok && raw != "" {
		if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
			return nil, fmt.Errorf("parse existing config.yaml: %w", err)
		}
	}
	doc["limiters"] = []any{
		map[string]any{
			"name":  "e2e-warm-pool-quota",
			"type":  "quota",
			"scope": "namespace",
			"namespaceQuotas": map[string]any{
				targetNamespace: map[string]any{accelerator: gpus},
			},
		},
	}
	merged, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal config.yaml: %w", err)
	}
	data["config.yaml"] = string(merged)

	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: SaturationConfigMap, Namespace: configNamespace},
		Data:       data,
	}
	if existed {
		desired.ObjectMeta.ResourceVersion = existing.ResourceVersion
		if _, err := cms.Update(ctx, desired, metav1.UpdateOptions{}); err != nil {
			return nil, fmt.Errorf("update %s: %w", SaturationConfigMap, err)
		}
	} else if _, err := cms.Create(ctx, desired, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("create %s: %w", SaturationConfigMap, err)
	}

	return func(ctx context.Context) error {
		if !existed {
			// It was not there before; leaving a quota behind would bound every
			// later spec in the suite by an allowance none of them declared.
			if err := cms.Delete(ctx, SaturationConfigMap, metav1.DeleteOptions{}); err != nil &&
				!errors.IsNotFound(err) {
				return err
			}
			return nil
		}
		current, err := cms.Get(ctx, SaturationConfigMap, metav1.GetOptions{})
		if err != nil {
			return err
		}
		restored := original.DeepCopy()
		restored.ObjectMeta.ResourceVersion = current.ResourceVersion
		_, err = cms.Update(ctx, restored, metav1.UpdateOptions{})
		return err
	}, nil
}
