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

// defaultEntryKey is the ConfigMap key holding cluster-scope settings. It
// mirrors config.GlobalDefaultsKey, restated here so the fixtures package does
// not depend on the controller's internal packages.
const defaultEntryKey = "default"

// SetNamespaceQuota declares a namespace-scoped GPU quota and returns a restore
// func.
//
// configName is the ConfigMap the CONTROLLER is actually reading, and the caller
// resolves it rather than this fixture assuming it. There are two names in play
// -- the current one and a pre-rename one the controller ignores whenever the
// current one exists -- and writing a quota into the ignored document produces a
// spec that sets a limit nobody enforces, then fails claiming the limiter is
// broken. That is not hypothetical: it is how this fixture failed the first time
// it ran.
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
	configNamespace, configName, targetNamespace, accelerator string,
	gpus int,
) (func(context.Context) error, error) {
	cms := clientset.CoreV1().ConfigMaps(configNamespace)

	existing, err := cms.Get(ctx, configName, metav1.GetOptions{})
	existed := err == nil
	if err != nil && !errors.IsNotFound(err) {
		return nil, fmt.Errorf("read %s: %w", configName, err)
	}

	var original *corev1.ConfigMap
	data := map[string]string{}
	if existed {
		original = existing.DeepCopy()
		for k, v := range existing.Data {
			data[k] = v
		}
	}

	// The "default" ENTRY, not "config.yaml". A limiter is a cluster-scope
	// setting the controller reads only from the global default entry: declared
	// anywhere else it is ignored with a log line and nothing else -- the quota
	// applies to no one, the pool grows past it, and the spec fails claiming
	// enforcement is broken. That is exactly how this fixture failed twice.
	doc := map[string]any{}
	if raw, ok := data[defaultEntryKey]; ok && raw != "" {
		if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
			return nil, fmt.Errorf("parse existing %s entry: %w", defaultEntryKey, err)
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
		return nil, fmt.Errorf("marshal %s entry: %w", defaultEntryKey, err)
	}
	data[defaultEntryKey] = string(merged)

	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configName, Namespace: configNamespace},
		Data:       data,
	}
	if existed {
		desired.ResourceVersion = existing.ResourceVersion
		if _, err := cms.Update(ctx, desired, metav1.UpdateOptions{}); err != nil {
			return nil, fmt.Errorf("update %s: %w", configName, err)
		}
	} else if _, err := cms.Create(ctx, desired, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("create %s: %w", configName, err)
	}

	return func(ctx context.Context) error {
		if !existed {
			// It was not there before; leaving a quota behind would bound every
			// later spec in the suite by an allowance none of them declared.
			if err := cms.Delete(ctx, configName, metav1.DeleteOptions{}); err != nil &&
				!errors.IsNotFound(err) {
				return err
			}
			return nil
		}
		current, err := cms.Get(ctx, configName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		restored := original.DeepCopy()
		restored.ResourceVersion = current.ResourceVersion
		_, err = cms.Update(ctx, restored, metav1.UpdateOptions{})
		return err
	}, nil
}
