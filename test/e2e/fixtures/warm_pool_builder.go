package fixtures

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// A warm-pool Pod, without a GPU.
//
// The real pool Pod runs FMA's launcher next to several vLLM engines; a
// launcher needs a GPU and an engine needs a much larger one, so neither can run
// in the emulated environment. What CAN run there is everything the pool's
// correctness actually depends on: the kubelet's view of the probes, the
// NetworkPolicy the cluster enforces, and the proxy — which is the real image,
// not a stand-in, because it is the piece with the subtle behaviour.
//
// So the supervisor and the engines are emulated and the proxy is real. The
// emulator speaks the launcher's own wire format (a map keyed by instance id at
// the collection root, PUT to create, DELETE to remove) and each instance it
// creates gets a listener answering vLLM's /health, /sleep, /wake_up and
// /is_sleeping, plus enough of /v1/completions to prove which engine a request
// reached.
//
// What this cannot cover, and what only a GPU cluster can: that a real engine
// survives sleeping and waking, and how long that takes.
const (
	// WarmPoolComponentLabel and WarmPoolComponentValue mark pool capacity, and
	// are what keeps a lent Pod out of a variant's replica accounting.
	WarmPoolComponentLabel = "app.kubernetes.io/component"
	WarmPoolComponentValue = "warm-pool"

	// warmPoolEmulatorImage is CPU-only and not from docker.io, per the
	// project's e2e image policy. It is the same base the SGLang emulator uses.
	warmPoolEmulatorImage = "registry.access.redhat.com/ubi9/python-311:latest"

	// WarmPoolSupervisorPort, WarmPoolServingPort and WarmPoolControlPort match
	// the real manifest, because the point of an e2e is to exercise what ships.
	WarmPoolSupervisorPort = 8001
	WarmPoolServingPort    = 8000
	WarmPoolControlPort    = 8002

	// WarmPoolNameLabel names the pool a Pod belongs to, matching the label the
	// controller groups by.
	WarmPoolNameLabel = "llm-d.ai/warm-pool"
	// gpuResourceName is what a pool Pod asks for when a spec gives it GPUs.
	gpuResourceName = "nvidia.com/gpu"
	// WarmPoolBasePort is the first port an emulated engine listens on, and the
	// floor the proxy enforces on an upstream.
	WarmPoolBasePort = 9001

	warmPoolScriptSuffix = "-supervisor-script"
)

// warmPoolSupervisorScript emulates FMA's launcher closely enough to drive the
// pool, and no more.
//
// Instances live in memory. Creating one starts a thread serving that instance's
// port; the engine answers /health whether awake or asleep, exactly as vLLM does
// — which is why the pool asks /is_sleeping rather than inferring from /health,
// and why a test can tell the two states apart.
const warmPoolSupervisorScript = `import json, os, threading, urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer

INSTANCES = {}          # id -> {"instance_id","status","options","env_vars"}
SLEEPING = {}           # port -> bool
LAST_SLEEP = {}         # port -> the last /sleep request line, query included
LOCK = threading.Lock()


def port_of(options):
    fields = options.split()
    for i, f in enumerate(fields):
        if f == "--port" and i + 1 < len(fields):
            return int(fields[i + 1])
        if f.startswith("--port="):
            return int(f.split("=", 1)[1])
    return 0


def model_of(options):
    fields = options.split()
    for i, f in enumerate(fields):
        if f == "--model" and i + 1 < len(fields):
            return fields[i + 1]
    return "unknown"


class Engine(BaseHTTPRequestHandler):
    """One emulated vLLM instance."""

    def _send(self, code, body=b""):
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = urllib.parse.urlparse(self.path).path
        if path == "/health":
            # Answered while ASLEEP too, as vLLM does: the process is alive, so
            # /health cannot distinguish the two states. That is exactly why the
            # pool asks /is_sleeping instead of inferring.
            self._send(200, b"{}")
        elif path == "/last_sleep":
            with LOCK:
                asked = LAST_SLEEP.get(self.server.server_address[1], "")
            self._send(200, json.dumps({"last_sleep": asked}).encode())
        elif path == "/is_sleeping":
            with LOCK:
                asleep = SLEEPING.get(self.server.server_address[1], True)
            self._send(200, json.dumps({"is_sleeping": asleep}).encode())
        else:
            self._send(404, b"{}")

    def do_POST(self):
        path = urllib.parse.urlparse(self.path).path
        port = self.server.server_address[1]
        if path == "/sleep":
            with LOCK:
                SLEEPING[port] = True
                # The QUERY, kept verbatim. vLLM's /sleep takes a mode and
                # defaults to "abort", which cancels every in-flight request;
                # only mode=wait drains them. The emulator has no work to drain,
                # so the only thing it can prove is what the controller ASKED
                # for -- and that is worth proving, because the difference
                # between the two is invisible everywhere else in this suite.
                LAST_SLEEP[port] = self.path
            self._send(200, b"{}")
        elif path == "/wake_up":
            with LOCK:
                SLEEPING[port] = False
            self._send(200, b"{}")
        elif path.startswith("/v1/"):
            with LOCK:
                asleep = SLEEPING.get(port, True)
            if asleep:
                # A sleeping engine cannot serve. Answering anyway would hide
                # the Ready-but-asleep bug this suite exists to catch.
                self._send(503, b'{"error":"engine is asleep"}')
                return
            body = json.dumps({
                "model": self.server.model,
                "served_by_port": port,
                "choices": [{"text": " served by " + self.server.model}],
            }).encode()
            self._send(200, body)
        else:
            self._send(404, b"{}")

    def log_message(self, *args):
        pass


def start_engine(port, model):
    server = HTTPServer(("0.0.0.0", port), Engine)
    server.model = model
    with LOCK:
        SLEEPING[port] = False   # a freshly created engine is awake, then slept
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


class Supervisor(BaseHTTPRequestHandler):
    """FMA's launcher API, reduced to what the pool calls."""

    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _id(self):
        path = urllib.parse.urlparse(self.path).path
        return path.rsplit("/", 1)[-1]

    def do_GET(self):
        path = urllib.parse.urlparse(self.path).path
        if path == "/health":
            self._send(200, {})
        elif path == "/v2/vllm/instances":
            # Keyed by instance id at the collection root, which is the shape
            # the launcher actually returns.
            with LOCK:
                self._send(200, dict(INSTANCES))
        else:
            self._send(404, {})

    def do_PUT(self):
        length = int(self.headers.get("Content-Length", 0))
        spec = json.loads(self.rfile.read(length) or b"{}")
        options = spec.get("options", "")
        instance_id = self._id()
        port = port_of(options)
        if port < 1:
            self._send(400, {"error": "options carry no --port"})
            return
        start_engine(port, model_of(options))
        record = {
            "instance_id": instance_id,
            "status": "running",
            "options": options,
            "env_vars": spec.get("env_vars", {}),
        }
        with LOCK:
            INSTANCES[instance_id] = record
        self._send(201, record)

    def do_DELETE(self):
        instance_id = self._id()
        with LOCK:
            INSTANCES.pop(instance_id, None)
        self._send(200, {})

    def log_message(self, *args):
        pass


port = int(os.environ.get("SUPERVISOR_PORT", "8001"))
HTTPServer(("0.0.0.0", port), Supervisor).serve_forever()
`

// WarmPoolSpec describes an emulated pool.
type WarmPoolSpec struct {
	Name       string
	Namespace  string
	ProxyImage string

	// PoolName is the llm-d.ai/warm-pool label, on both the Deployment and its
	// Pods. Empty leaves the pool unnamed, which is what an install with one
	// pool has and what every spec before named pools existed assumed.
	PoolName string
	// NodeName pins the Pod to one node. Empty lets the scheduler choose.
	//
	// Needed because a pool Pod's ACCELERATOR is a property of the node it runs
	// on, so a test about accelerator matching has to decide which node that is.
	// Nothing else in the suite cares where a pool Pod lands.
	NodeName string
	// SupervisorImage replaces the emulated supervisor with a REAL launcher.
	//
	// Empty takes the emulator, which is what every spec on a cluster without
	// GPUs needs: a launcher needs a device and an engine needs a much larger
	// one. The emulator speaks the launcher's wire format and holds no model, so
	// it can answer /sleep but has nothing to drain -- which is exactly the
	// difference a spec about draining cannot ignore, and why one exists that
	// sets this.
	//
	// Setting it also drops the script volume and the emulator's command: a real
	// launcher is its own entrypoint.
	SupervisorImage string
	// CacheClaim is the RWX model cache a REAL launcher reads weights from.
	//
	// Only meaningful with SupervisorImage. Empty leaves the Pod without one,
	// which is right for the emulator -- it holds no model -- and wrong for a
	// real engine on a cluster with no route to Hugging Face: it would sit
	// downloading nothing until the spec timed out. That is how this fixture
	// first failed on a GPU cluster.
	CacheClaim string
	// Replicas defaults to 1.
	Replicas int32
	// GPUs each Pod requests. Zero requests none, which is what an emulated pool
	// wants on a cluster with no devices -- the count the policy reads comes
	// from this field, so a spec about capacity sets it and the rest do not.
	GPUs int
	// GroupSize is how many Pods make ONE warm unit. Zero or one builds the
	// ordinary Deployment pool; two or more builds a LeaderWorkerSet, where the
	// leader runs the supervisor and the workers only have to exist. See
	// CreateWarmPoolGroup.
	GroupSize int
	// Annotations go on the DEPLOYMENT, where the pool's tuning lives.
	Annotations map[string]string
}

// CreateWarmPool stands up an emulated pool Pod: a supervisor emulator and the
// real proxy, wired exactly as config/warmpool does.
//
// The probes are the real ones too — exec, running inside the container. That is
// the point of testing them here rather than in a unit test: a kubelet probe
// comes from the node, and no NetworkPolicy `from:` selector can name a node, so
// whether an httpGet probe reaches a restricted port is a property of the CNI
// rather than of the manifest. Running the check inside the container is what
// removes that dependency, and only a real kubelet can confirm it.
func CreateWarmPool(ctx context.Context, clientset *kubernetes.Clientset, spec WarmPoolSpec) error {
	if err := createWarmPoolScript(ctx, clientset, spec); err != nil {
		return err
	}

	labels := map[string]string{
		"app.kubernetes.io/name": "workload-variant-autoscaler",
		WarmPoolComponentLabel:   WarmPoolComponentValue,
		"warm-pool-fixture":      spec.Name,
	}
	if spec.PoolName != "" {
		labels[WarmPoolNameLabel] = spec.PoolName
	}
	replicas := spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	supervisor := warmPoolSupervisorContainerFor(spec.SupervisorImage, spec.CacheClaim)
	if spec.GPUs > 0 {
		// On the ENGINE container, which is where capacityOf looks: the proxy
		// sidecar's own limits have nothing to do with how many models fit.
		supervisor.Resources = corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				gpuResourceName: *resource.NewQuantity(int64(spec.GPUs), resource.DecimalSI),
			},
		}
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        spec.Name,
			Namespace:   spec.Namespace,
			Labels:      labels,
			Annotations: spec.Annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			// Recreate, as the real manifest does: two pool Pods holding the
			// same models is not a rollout, it is double the memory.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"warm-pool-fixture": spec.Name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					// The controller reaches this Pod over the network; it has
					// no reason to hold a token of its own.
					AutomountServiceAccountToken: ptr.To(false),
					NodeName:                     spec.NodeName,
					Volumes: append([]corev1.Volume{{
						Name: "script",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: spec.Name + warmPoolScriptSuffix},
							},
						},
					}}, poolVolumes(spec)...),
					Containers: []corev1.Container{
						supervisor,
						warmPoolProxyContainer(spec.ProxyImage),
					},
				},
			},
		},
	}

	_, err := clientset.AppsV1().Deployments(spec.Namespace).Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create warm pool deployment %s: %w", spec.Name, err)
	}
	return nil
}

// warmPoolSupervisorContainerFor builds the supervisor, emulated or real.
//
// A real launcher is its own entrypoint and mounts no script: everything the
// emulator needs from /script it has built in. Both listen on the same port and
// speak the same wire format, which is the point of the emulator.
func warmPoolSupervisorContainerFor(image, cacheClaim string) corev1.Container {
	c := warmPoolSupervisorContainer()
	if image == "" {
		return c
	}
	c.Image = image
	c.Command = []string{"python3", "/app/launcher.py", "--host", "0.0.0.0",
		"--port=" + strconv.Itoa(WarmPoolSupervisorPort)}

	// The script mount goes: a real launcher is its own entrypoint.
	var kept []corev1.VolumeMount
	for _, m := range c.VolumeMounts {
		if m.Name != "script" {
			kept = append(kept, m)
		}
	}
	kept = append(kept,
		corev1.VolumeMount{Name: "pod-home", MountPath: "/pod-home"},
		corev1.VolumeMount{Name: "dshm", MountPath: "/dev/shm"},
	)
	c.VolumeMounts = kept

	// The same environment config/warmpool gives it, and for the same reasons:
	// an arbitrary UID has no home directory, and every cache vLLM writes has to
	// land somewhere writable. Copied rather than referenced because the fixture
	// builds its Pod in Go and the manifest is YAML; a real launcher missing any
	// of it fails at model load, which reads from outside as "the engine never
	// served".
	c.Env = append(c.Env,
		corev1.EnvVar{Name: "HOME", Value: "/pod-home"},
		corev1.EnvVar{Name: "PYTHONNOUSERSITE", Value: "1"},
		corev1.EnvVar{Name: "USER", Value: "warmpool"},
	)
	if cacheClaim != "" {
		c.VolumeMounts = append(c.VolumeMounts,
			corev1.VolumeMount{Name: "model-cache", MountPath: "/model-cache"})
		c.Env = append(c.Env,
			corev1.EnvVar{Name: "HF_HOME", Value: "/model-cache"},
			corev1.EnvVar{Name: "VLLM_CACHE_ROOT", Value: "/model-cache/vllm"},
			corev1.EnvVar{Name: "FLASHINFER_WORKSPACE_DIR", Value: "/model-cache/flashinfer"},
			corev1.EnvVar{Name: "TRITON_CACHE_DIR", Value: "/model-cache/triton"},
			corev1.EnvVar{Name: "XDG_CACHE_HOME", Value: "/model-cache"},
			corev1.EnvVar{Name: "XDG_CONFIG_HOME", Value: "/model-cache/config"},
		)
	}
	return c
}

// realSupervisorVolumes are what a real launcher needs and the emulator does
// not: somewhere to call home, shared memory for the engine's workers, and the
// weights.
func realSupervisorVolumes(cacheClaim string) []corev1.Volume {
	shm := resource.MustParse("16Gi")
	out := []corev1.Volume{
		{Name: "pod-home", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "dshm", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium: corev1.StorageMediumMemory, SizeLimit: &shm,
		}}},
	}
	if cacheClaim != "" {
		out = append(out, corev1.Volume{
			Name: "model-cache",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: cacheClaim},
			},
		})
	}
	return out
}

func warmPoolSupervisorContainer() corev1.Container {
	return corev1.Container{
		Name:    "inference-server",
		Image:   warmPoolEmulatorImage,
		Command: []string{"python3", "/script/supervisor.py"},
		Env: []corev1.EnvVar{
			{Name: "SUPERVISOR_PORT", Value: strconv.Itoa(WarmPoolSupervisorPort)},
		},
		Ports: []corev1.ContainerPort{
			{Name: "supervisor", ContainerPort: WarmPoolSupervisorPort, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: "script", MountPath: "/script"}},
		// exec, matching the real manifest: :8001 is closed to everything but
		// the controller, and a kubelet probe cannot be admitted by any
		// `from:` selector.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
				Command: warmPoolPythonCheck(WarmPoolSupervisorPort, "/health"),
			}},
			InitialDelaySeconds: 2,
			PeriodSeconds:       5,
		},
		// Production ships a liveness probe here too, and the suite asserts that
		// an idle pool is not restart-looped. Without one on this container that
		// assertion would be true by construction for it -- nothing could
		// restart it -- which is a pass that establishes nothing.
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
				Command: warmPoolPythonCheck(WarmPoolSupervisorPort, "/health"),
			}},
			InitialDelaySeconds: 10,
			PeriodSeconds:       10,
		},
	}
}

func warmPoolProxyContainer(image string) corev1.Container {
	return corev1.Container{
		Name:  "proxy",
		Image: image,
		Args: []string{
			fmt.Sprintf("--port=%d", WarmPoolServingPort),
			fmt.Sprintf("--control-port=%d", WarmPoolControlPort),
			"--v=2",
		},
		Ports: []corev1.ContainerPort{
			{Name: "serving", ContainerPort: WarmPoolServingPort, Protocol: corev1.ProtocolTCP},
			{Name: "proxy-control", ContainerPort: WarmPoolControlPort, Protocol: corev1.ProtocolTCP},
		},
		// The binary probes itself: the image is distroless, so there is no
		// shell and no curl to do it with.
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
				Command: []string{"/warmpool-proxy", "--check=live",
					fmt.Sprintf("--control-port=%d", WarmPoolControlPort)},
			}},
			InitialDelaySeconds: 3,
			PeriodSeconds:       10,
		},
		// periodSeconds and failureThreshold are the real ones. With the
		// defaults it takes 5-20 s to mark a Pod NotReady, while the controller
		// sleeps the engine after its drain wait -- which is the
		// Ready-but-asleep window, and a 503 for every request in it.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
				Command: []string{"/warmpool-proxy", "--check=ready",
					fmt.Sprintf("--control-port=%d", WarmPoolControlPort)},
			}},
			InitialDelaySeconds: 3,
			PeriodSeconds:       1,
			FailureThreshold:    1,
			TimeoutSeconds:      2,
		},
	}
}

// warmPoolPythonCheck is an exec probe for a container that has python but no
// curl.
func warmPoolPythonCheck(port int, path string) []string {
	return []string{"python3", "-c", fmt.Sprintf(
		`import sys,urllib.request
try:
    urllib.request.urlopen("http://127.0.0.1:%d%s", timeout=3)
except Exception as err:
    print(err); sys.exit(1)`, port, path)}
}

func createWarmPoolScript(ctx context.Context, clientset *kubernetes.Clientset, spec WarmPoolSpec) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name + warmPoolScriptSuffix,
			Namespace: spec.Namespace,
		},
		Data: map[string]string{"supervisor.py": warmPoolSupervisorScript},
	}
	_, err := clientset.CoreV1().ConfigMaps(spec.Namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create supervisor script configmap: %w", err)
	}
	return nil
}

// ShippedNetworkPolicyPath is the policy the pool actually ships with, relative
// to the e2e package directory (which is the working directory `go test` uses).
const ShippedNetworkPolicyPath = "../../config/warmpool/warmpool-networkpolicy.yaml"

// ApplyWarmPoolNetworkPolicy installs the SHIPPED policy, read from the manifest
// rather than restated here.
//
// Read rather than rebuilt on purpose. A copy in Go would drift from the file
// that actually gets deployed, and the drift would be invisible: the suite would
// keep passing against a policy nobody applies. It also means the e2e is testing
// the artefact, which is the only thing worth testing about a manifest.
//
// The fixture's pool Pod deliberately carries the same labels as the real one,
// so the shipped podSelector matches it, and the controller-labelled driver Pod
// matches the same-namespace `from:` rule.
func ApplyWarmPoolNetworkPolicy(
	ctx context.Context,
	clientset *kubernetes.Clientset,
	namespace string,
) (string, error) {
	raw, err := os.ReadFile(ShippedNetworkPolicyPath)
	if err != nil {
		return "", fmt.Errorf("read the shipped policy at %s: %w", ShippedNetworkPolicyPath, err)
	}

	var policy networkingv1.NetworkPolicy
	if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096).Decode(&policy); err != nil {
		return "", fmt.Errorf("decode %s: %w", ShippedNetworkPolicyPath, err)
	}
	policy.Namespace = namespace
	policy.ResourceVersion = ""

	// Point the controller rule at the namespace the suite actually runs in.
	//
	// This is not a convenience: it is precisely the edit the manifest tells a
	// namespace-scoped operator to make, so performing it here keeps the suite
	// honest about what has to be done and would fail loudly if the rule were
	// ever removed or renamed. The shipped default names the cluster-scoped
	// install's namespace, which no test cluster uses.
	edited := 0
	for i := range policy.Spec.Ingress {
		for j := range policy.Spec.Ingress[i].From {
			peer := &policy.Spec.Ingress[i].From[j]
			if peer.NamespaceSelector == nil || peer.PodSelector == nil {
				continue
			}
			peer.NamespaceSelector = &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace},
			}
			edited++
		}
	}
	if edited == 0 {
		return "", fmt.Errorf(
			"%s has no namespaceSelector+podSelector rule to point at the controller; "+
				"the suite cannot admit the controller and every assertion about the "+
				"boundary would be meaningless", ShippedNetworkPolicyPath)
	}

	if _, err := clientset.NetworkingV1().NetworkPolicies(namespace).Create(
		ctx, &policy, metav1.CreateOptions{}); err != nil {
		if !errors.IsAlreadyExists(err) {
			return "", fmt.Errorf("create network policy %s: %w", policy.Name, err)
		}
		// Someone applied it out of band. Take it over rather than test against
		// a version this run did not choose.
		existing, getErr := clientset.NetworkingV1().NetworkPolicies(namespace).Get(
			ctx, policy.Name, metav1.GetOptions{})
		if getErr != nil {
			return "", fmt.Errorf("get existing network policy %s: %w", policy.Name, getErr)
		}
		policy.ResourceVersion = existing.ResourceVersion
		if _, err := clientset.NetworkingV1().NetworkPolicies(namespace).Update(
			ctx, &policy, metav1.UpdateOptions{}); err != nil {
			return "", fmt.Errorf("update network policy %s: %w", policy.Name, err)
		}
	}
	return policy.Name, nil
}

// DeleteWarmPoolNetworkPolicy removes it again.
func DeleteWarmPoolNetworkPolicy(
	ctx context.Context,
	clientset *kubernetes.Clientset,
	namespace, name string,
) error {
	if name == "" {
		return nil
	}
	if err := clientset.NetworkingV1().NetworkPolicies(namespace).Delete(
		ctx, name, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete network policy %s: %w", name, err)
	}
	return nil
}

// DeleteWarmPool removes the pool and its script.
func DeleteWarmPool(ctx context.Context, clientset *kubernetes.Clientset, spec WarmPoolSpec) error {
	policy := metav1.DeletePropagationForeground
	opts := metav1.DeleteOptions{PropagationPolicy: &policy}

	if err := clientset.AppsV1().Deployments(spec.Namespace).Delete(ctx, spec.Name, opts); err != nil &&
		!errors.IsNotFound(err) {
		return fmt.Errorf("delete warm pool %s: %w", spec.Name, err)
	}
	if err := clientset.CoreV1().ConfigMaps(spec.Namespace).Delete(ctx,
		spec.Name+warmPoolScriptSuffix, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete supervisor script: %w", err)
	}
	return nil
}

// WarmPoolInstanceOptions is the engine command line the pool would derive from
// a workload's own pod spec, reduced to what the emulator reads.
func WarmPoolInstanceOptions(model string, port int) string {
	return fmt.Sprintf("--model %s --gpu-memory-utilization 0.95 --enable-sleep-mode --port %d", model, port)
}

// poolVolumes adds what a REAL launcher needs. The emulator needs none of it, so
// an emulated pool is unchanged.
func poolVolumes(spec WarmPoolSpec) []corev1.Volume {
	if spec.SupervisorImage == "" {
		return nil
	}
	return realSupervisorVolumes(spec.CacheClaim)
}
