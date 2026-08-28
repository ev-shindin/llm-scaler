package utils

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// DumpControllerLogs fetches and prints the controller manager logs for debugging.
// Call this in AfterEach or DeferCleanup to capture logs on test failure.
func DumpControllerLogs(ctx context.Context, k8sClient *kubernetes.Clientset, controllerNamespace string, w io.Writer) {
	_, _ = fmt.Fprintf(w, "\n=== Controller Manager Logs ===\n")

	pods, err := k8sClient.CoreV1().Pods(controllerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=workload-variant-autoscaler",
	})
	if err != nil {
		_, _ = fmt.Fprintf(w, "Failed to list controller pods: %v\n", err)
		return
	}

	if len(pods.Items) == 0 {
		_, _ = fmt.Fprintf(w, "No controller pods found in namespace %s\n", controllerNamespace)
		return
	}

	for _, pod := range pods.Items {
		_, _ = fmt.Fprintf(w, "\n--- Logs from pod %s ---\n", pod.Name)
		logs, err := k8sClient.CoreV1().Pods(controllerNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			TailLines: ptr.To(int64(200)),
		}).DoRaw(ctx)
		if err != nil {
			_, _ = fmt.Fprintf(w, "Failed to get logs: %v\n", err)
			continue
		}
		_, _ = fmt.Fprintf(w, "%s\n", string(logs))
	}
}

// DumpManagedScalers fetches and prints the HorizontalPodAutoscalers in every
// namespace for debugging. WVA discovers variants from annotated HPAs (and KEDA
// ScaledObjects, which KEDA in turn manages via HPAs), so the HPA list plus its
// currentMetrics is the observable annotation-discovery surface.
func DumpManagedScalers(ctx context.Context, k8sClient *kubernetes.Clientset,
	dynClient dynamic.Interface, w io.Writer) {
	_, _ = fmt.Fprintf(w, "\n=== Managed HorizontalPodAutoscalers ===\n")

	hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		_, _ = fmt.Fprintf(w, "Failed to list HPAs: %v\n", err)
		return
	}

	// Said out loud, because an empty section under a heading reads as "the dump
	// did not look" when what it means is "KEDA is managing nothing at all" --
	// which is a finding, and the one that explains a ScaledObject whose metric
	// WVA republishes every cycle while the Deployment never moves.
	if len(hpaList.Items) == 0 {
		_, _ = fmt.Fprintf(w, "(none in any namespace -- nothing is actuating; "+
			"see the ScaledObject conditions below)\n")
	}

	for i := range hpaList.Items {
		hpa := &hpaList.Items[i]
		_, _ = fmt.Fprintf(w, "\nHPA: %s/%s\n", hpa.Namespace, hpa.Name)
		_, _ = fmt.Fprintf(w, "  ScaleTargetRef: %s/%s\n", hpa.Spec.ScaleTargetRef.Kind, hpa.Spec.ScaleTargetRef.Name)
		_, _ = fmt.Fprintf(w, "  Annotations: %v\n", hpa.Annotations)
		_, _ = fmt.Fprintf(w, "  DesiredReplicas: %d\n", hpa.Status.DesiredReplicas)
		_, _ = fmt.Fprintf(w, "  CurrentMetrics: %v\n", hpa.Status.CurrentMetrics)
	}
}

// DumpScaledObjects prints every KEDA ScaledObject with the status conditions
// that decide whether it is actuating anything.
//
// The HPA dump above shows the DERIVED object and says nothing about why it is
// absent. KEDA maintains an HPA only for a ScaledObject it considers Ready, so
// when nothing actuates the reason is a condition here -- and without it a run
// reports the symptom (replicas never moved) with no trace of the cause.
func DumpScaledObjects(ctx context.Context, dynClient dynamic.Interface, w io.Writer) {
	_, _ = fmt.Fprintf(w, "\n=== KEDA ScaledObjects ===\n")
	if dynClient == nil {
		_, _ = fmt.Fprintf(w, "(no dynamic client)\n")
		return
	}

	gvr := schema.GroupVersionResource{Group: "keda.sh", Version: "v1alpha1", Resource: "scaledobjects"}
	list, err := dynClient.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		_, _ = fmt.Fprintf(w, "Failed to list ScaledObjects: %v\n", err)
		return
	}
	if len(list.Items) == 0 {
		_, _ = fmt.Fprintf(w, "(none in any namespace)\n")
		return
	}

	for i := range list.Items {
		so := &list.Items[i]
		_, _ = fmt.Fprintf(w, "\nScaledObject: %s/%s\n", so.GetNamespace(), so.GetName())
		spec, _, _ := unstructured.NestedMap(so.Object, "spec")
		if spec != nil {
			_, _ = fmt.Fprintf(w, "  min/max: %v/%v\n", spec["minReplicaCount"], spec["maxReplicaCount"])
			if target, ok := spec["scaleTargetRef"].(map[string]interface{}); ok {
				_, _ = fmt.Fprintf(w, "  ScaleTargetRef: %v\n", target["name"])
			}
		}
		conds, _, _ := unstructured.NestedSlice(so.Object, "status", "conditions")
		if len(conds) == 0 {
			_, _ = fmt.Fprintf(w, "  Conditions: (none yet -- KEDA has not reconciled "+
				"this object, so no HPA exists for it)\n")
			continue
		}
		for _, c := range conds {
			cm, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			_, _ = fmt.Fprintf(w, "  Condition %v=%v reason=%v message=%v\n",
				cm["type"], cm["status"], cm["reason"], cm["message"])
		}
	}
}
