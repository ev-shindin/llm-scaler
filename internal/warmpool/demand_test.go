package warmpool

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
	poolutil "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/pool"
)

func specWith(command, args []string) *corev1.PodSpec {
	return &corev1.PodSpec{Containers: []corev1.Container{
		{Name: "sidecar", Args: []string{"--not-a-model-server"}},
		{Name: "vllm", Command: command, Args: args},
	}}
}

func TestEngineOptionsExpandTheContainersOwnEnv(t *testing.T) {
	// Copied from a live llm-d decode Deployment. The chart writes a SHELL
	// SCRIPT and relies on the shell expanding these from the container env at
	// runtime; the launcher expands nothing, so an unexpanded token reached vLLM
	// and the admitted instance went straight to `stopped`. Seen on a real
	// admission, after the controller had done everything else right.
	spec := &corev1.PodSpec{Containers: []corev1.Container{{
		Name:    "vllm",
		Command: []string{"/bin/bash", "-c"},
		Args: []string{"/bin/true ; vllm serve /model-cache/models/Qwen/Qwen3-0.6B \\\n" +
			"  --block-size $VLLM_BLOCK_SIZE \\\n" +
			"  --max-model-len ${VLLM_MAX_MODEL_LEN} \\\n" +
			"  --tensor-parallel-size $VLLM_TENSOR_PARALLELISM"},
		Env: []corev1.EnvVar{
			{Name: "VLLM_BLOCK_SIZE", Value: "64"},
			{Name: "VLLM_MAX_MODEL_LEN", Value: "16000"},
			{Name: "VLLM_TENSOR_PARALLELISM", Value: "1"},
		},
	}}}
	got, err := EngineOptionsFrom(spec)
	if err != nil {
		t.Fatalf("EngineOptionsFrom: %v", err)
	}
	for _, want := range []string{"--block-size 64", "--max-model-len 16000", "--tensor-parallel-size 1"} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in %q", want, got)
		}
	}
	if strings.Contains(got, "$") {
		t.Errorf("an unexpanded variable reached the warm copy: %q", got)
	}
	// The shell's line continuations must not survive as bare words, or the one
	// before a flag is read as that flag's value.
	for _, f := range strings.Fields(got) {
		if f == "\\" {
			t.Errorf("a line-continuation backslash survived: %q", got)
		}
	}
}

func TestEngineOptionsDropAFlagWhoseValueCannotBeResolved(t *testing.T) {
	// A valueFrom cannot be read from a PodSpec. Leaving the token in place lets
	// normaliseOptions drop the flag AND its value, which is safer than
	// expanding it to empty -- that would leave `--max-model-len` sitting next to
	// the following flag and read as a valid command line with the wrong shape.
	spec := &corev1.PodSpec{Containers: []corev1.Container{{
		Name:    "vllm",
		Command: []string{"/bin/bash", "-c"},
		Args:    []string{"vllm serve Qwen/Qwen3-0.6B --max-model-len $FROM_A_SECRET --dtype bfloat16"},
		Env: []corev1.EnvVar{{
			Name:      "FROM_A_SECRET",
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
		}},
	}}}
	got, err := EngineOptionsFrom(spec)
	if err != nil {
		t.Fatalf("EngineOptionsFrom: %v", err)
	}
	if strings.Contains(got, "max-model-len") {
		t.Errorf("an unresolvable flag must be dropped, not guessed: %q", got)
	}
	if !strings.Contains(got, "--dtype bfloat16") {
		t.Errorf("the resolvable flags must survive: %q", got)
	}
}

func TestEngineOptionsReadTheLlmdChartsPositionalForm(t *testing.T) {
	// The argv of a real llm-d decode Deployment, copied from a live cluster:
	// a shell wrapper, and the model given POSITIONALLY to `vllm serve` with no
	// --model anywhere. This was skipped as "no engine options", so the pool saw
	// zero demand and would never have warmed anything on the layout llm-d's own
	// chart produces.
	spec := specWith(
		[]string{"/bin/bash", "-c"},
		[]string{"/bin/true ; vllm serve /model-cache/models/Qwen/Qwen3-0.6B " +
			"--served-model-name Qwen/Qwen3-0.6B --gpu-memory-utilization 0.95 " +
			"--max-model-len 16000 --port 8000"},
	)
	got, err := EngineOptionsFrom(spec)
	if err != nil {
		t.Fatalf("EngineOptionsFrom: %v", err)
	}
	if !strings.Contains(got, "--model /model-cache/models/Qwen/Qwen3-0.6B") {
		t.Fatalf("the positional model must become an explicit --model: %q", got)
	}
	if strings.Count(got, "--model ") != 1 {
		t.Errorf("exactly one --model expected, got %q", got)
	}
	// The bare wrapper words must not survive into argv the pool will run.
	// Compared as TOKENS, not substrings: --served-model-name contains "serve".
	junk := map[string]bool{"/bin/bash": true, "/bin/true": true, "vllm": true, "serve": true, ";": true}
	for _, f := range strings.Fields(got) {
		if junk[f] {
			t.Errorf("%q leaked into the warm copy argv: %q", f, got)
		}
	}
	// And the shape flags still come across, because a warm copy that differs
	// misses the torch.compile cache.
	if !strings.Contains(got, "--gpu-memory-utilization 0.95") {
		t.Errorf("shape flags lost: %q", got)
	}
}

func TestEngineOptionsPreferTheSourceOverTheAdvertisedName(t *testing.T) {
	// --served-model-name is a repository id while the weights are already a
	// local path. Warming the advertised name would download a second copy of a
	// model that is on the shared cache already.
	spec := specWith(nil, []string{
		"vllm", "serve", "/model-cache/models/Qwen/Qwen3-0.6B",
		"--served-model-name", "Qwen/Qwen3-0.6B",
	})
	got, err := EngineOptionsFrom(spec)
	if err != nil {
		t.Fatalf("EngineOptionsFrom: %v", err)
	}
	if !strings.Contains(got, "--model /model-cache/models/Qwen/Qwen3-0.6B") {
		t.Fatalf("the SOURCE must win: %q", got)
	}
}

func TestEngineOptionsStillRefuseAContainerThatNamesNoModel(t *testing.T) {
	// `serve` alone is not a model, and a sidecar must not be mistaken for the
	// engine just because the word appears.
	if _, err := EngineOptionsFrom(specWith(nil, []string{"--not-a-model-server"})); err == nil {
		t.Fatal("a container naming no model must be refused")
	}
	if _, err := EngineOptionsFrom(specWith(nil, []string{"vllm", "serve"})); err == nil {
		t.Fatal("a bare `serve` with no model must be refused")
	}
}

func TestEngineOptionsComeFromTheOrdinaryReplicas(t *testing.T) {
	// Derived rather than configured, because the two must match: a different
	// --gpu-memory-utilization is a different torch.compile cache key.
	spec := specWith(nil, []string{
		"--model", "Qwen/Qwen3-0.6B",
		"--gpu-memory-utilization", "0.95",
		"--tensor-parallel-size", "1",
	})
	got, err := EngineOptionsFrom(spec)
	if err != nil {
		t.Fatalf("EngineOptionsFrom: %v", err)
	}
	if !strings.Contains(got, "--model Qwen/Qwen3-0.6B") {
		t.Errorf("the model must survive: %q", got)
	}
	if !strings.Contains(got, "--gpu-memory-utilization 0.95") {
		t.Errorf("utilisation must survive, or the compile cache misses: %q", got)
	}
}

func TestSleepModeIsAddedIfTheWorkloadDoesNotHaveIt(t *testing.T) {
	// Ordinary replicas have no reason to enable it; a warm copy cannot work
	// without it, since /sleep and /wake_up do not exist otherwise.
	got, err := EngineOptionsFrom(specWith(nil, []string{"--model", "m"}))
	if err != nil {
		t.Fatalf("EngineOptionsFrom: %v", err)
	}
	if !strings.Contains(got, "--enable-sleep-mode") {
		t.Fatalf("sleep mode must be added: %q", got)
	}

	// And not added twice when it is already there.
	got, err = EngineOptionsFrom(specWith(nil, []string{"--model", "m", "--enable-sleep-mode"}))
	if err != nil {
		t.Fatalf("EngineOptionsFrom: %v", err)
	}
	if strings.Count(got, "--enable-sleep-mode") != 1 {
		t.Fatalf("duplicated: %q", got)
	}
}

func TestThePortIsStrippedBecauseThePoolAssignsIt(t *testing.T) {
	// Instances share a Pod. A port copied from the workload would collide with
	// whichever copy is already listening on it.
	for _, args := range [][]string{
		{"--model", "m", "--port", "8000"},
		{"--model", "m", "--port=8000"},
	} {
		got, err := EngineOptionsFrom(specWith(nil, args))
		if err != nil {
			t.Fatalf("EngineOptionsFrom(%v): %v", args, err)
		}
		if strings.Contains(got, "8000") || strings.Contains(got, "--port") {
			t.Errorf("port must be stripped from %v: %q", args, got)
		}
		if !strings.Contains(got, "--model m") {
			t.Errorf("and nothing else lost: %q", got)
		}
	}
}

func TestTheLauncherIsStrippedFromTheCommand(t *testing.T) {
	// The supervisor starts the engine itself; only the arguments carry over.
	got, err := EngineOptionsFrom(specWith(
		[]string{"vllm", "serve"},
		[]string{"--model", "m", "--gpu-memory-utilization", "0.9"},
	))
	if err != nil {
		t.Fatalf("EngineOptionsFrom: %v", err)
	}
	if strings.Contains(got, "vllm") || strings.Contains(got, "serve") {
		t.Fatalf("the launcher must not be passed as an option: %q", got)
	}
	if !strings.HasPrefix(got, "--model m") {
		t.Fatalf("options should start at the first flag: %q", got)
	}
}

func TestAWorkloadWithNoModelIsNotWarmable(t *testing.T) {
	// Skipped rather than guessed at: a warm copy started with the wrong
	// options is slower than no warm copy at all, and silently so.
	if _, err := EngineOptionsFrom(&corev1.PodSpec{Containers: []corev1.Container{
		{Name: "proxy", Args: []string{"--listen", ":8080"}},
	}}); err == nil {
		t.Fatal("want an error when no container names a --model")
	}
}

// fakeDatastore answers only the one question Demand asks of a datastore. The
// interface is embedded rather than implemented so that any other call panics
// loudly: Demand reading more of the datastore than this would be a change
// worth noticing.
type fakeDatastore struct {
	datastore.Datastore
	pool *poolutil.EndpointPool
	err  error
}

func (f *fakeDatastore) PoolGetFromLabels(string, map[string]string) (*poolutil.EndpointPool, error) {
	return f.pool, f.err
}

// scaleTarget is the one workload these tests register; the variant is named
// separately because they are different identities and conflating them is how
// the decision store gets read under the wrong key.
const scaleTarget = "qwen-decode"

func deployment(ready int32, container corev1.Container) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: scaleTarget, Namespace: "tenant"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": scaleTarget}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{container}},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
}

func vllmContainer() corev1.Container {
	return corev1.Container{
		Name:  "vllm",
		Args:  []string{"--model", "Qwen/Qwen3-0.6B", "--gpu-memory-utilization", "0.95"},
		Image: "vllm",
	}
}

// demandFor wires a Demand over the given objects and datastore.
func demandFor(t *testing.T, objects []client.Object, ds datastore.Datastore) (*Demand, *registry.Registry, *decision.Store) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	reg := registry.New(0)
	decisions := decision.NewStore()
	return &Demand{
		Registry:  reg,
		Decisions: decisions,
		Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		Datastore: ds,
	}, reg, decisions
}

func registerVariant(reg *registry.Registry, target string) {
	reg.Observe("tenant", "qwen", nil)
	reg.SetTarget("tenant", "qwen", registry.Target{Kind: "Deployment", Name: target})
}

func inferencePool() *poolutil.EndpointPool {
	return &poolutil.EndpointPool{
		Name:      "qwen-pool",
		Namespace: "tenant",
		Selector: map[string]string{
			"llm-d.ai/model":            "qwen",
			"llm-d.ai/inferenceServing": "true",
		},
	}
}

func TestVariantsReadDesireFromTheDecisionStoreAndReadinessFromTheWorkload(t *testing.T) {
	// The accounting rule made structural: the pool must never be able to
	// influence how much a variant scales, only how quickly that capacity
	// arrives. A demand source that recomputed desire is how it would start to.
	d, reg, decisions := demandFor(t,
		[]client.Object{deployment(1, vllmContainer())},
		&fakeDatastore{pool: inferencePool()})
	registerVariant(reg, scaleTarget)
	decisions.Set("tenant", scaleTarget, 3)

	got, err := d.Variants(context.Background())
	if err != nil {
		t.Fatalf("Variants: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Variants = %+v, want one", got)
	}
	v := got[0]
	if v.Desired != 3 || v.Ready != 1 {
		t.Errorf("desired/ready = %d/%d, want 3/1", v.Desired, v.Ready)
	}
	if v.Parked {
		t.Error("a variant WVA wants three of is not parked")
	}
	// The only variant, so it is all of the fleet.
	if v.Share != 1 {
		t.Errorf("share = %v, want 1 for the only variant with any desire", v.Share)
	}
	if !strings.Contains(v.Model.EngineOptions, "--gpu-memory-utilization 0.95") {
		t.Errorf("options must be derived from the ordinary replicas: %q", v.Model.EngineOptions)
	}
	if !strings.Contains(v.Model.EngineOptions, "--enable-sleep-mode") {
		t.Errorf("a warm copy that cannot sleep is not a warm copy: %q", v.Model.EngineOptions)
	}
}

func TestPoolLabelsComeFromTheInferencePoolsOwnSelector(t *testing.T) {
	// Read rather than assumed. A selector belongs to the tenant, and one that
	// requires something other than llm-d.ai/model is not hypothetical -- a pool
	// Pod that guesses joins nothing, and its wake then serves no traffic.
	d, reg, _ := demandFor(t,
		[]client.Object{deployment(1, vllmContainer())},
		&fakeDatastore{pool: inferencePool()})
	registerVariant(reg, scaleTarget)

	got, err := d.Variants(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("Variants = %+v, %v", got, err)
	}
	labels := got[0].Model.PoolLabels
	if labels["llm-d.ai/inferenceServing"] != "true" || labels["llm-d.ai/model"] != "qwen" {
		t.Errorf("labels = %v, want the InferencePool's whole selector", labels)
	}
	if _, copied := labels["app"]; copied {
		t.Error("the workload's own labels are not the pool's selector")
	}
}

func TestParkedNeedsADecisionAndNotJustTwoZeroes(t *testing.T) {
	// Before WVA has decided anything -- every variant, on every restart --
	// desired reads 0, and a workload that has not started reads 0 ready.
	// Calling that parked would admit models nobody asked for, at ~35 s and a
	// reserve slot each, on every restart.
	d, reg, decisions := demandFor(t,
		[]client.Object{deployment(0, vllmContainer())},
		&fakeDatastore{pool: inferencePool()})
	registerVariant(reg, scaleTarget)

	got, err := d.Variants(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("Variants = %+v, %v", got, err)
	}
	if got[0].Parked {
		t.Fatal("no decision yet is not the same as a decision to park")
	}

	decisions.Set("tenant", scaleTarget, 0)
	if got, err = d.Variants(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("Variants = %+v, %v", got, err)
	}
	if !got[0].Parked {
		t.Error("a decided zero with nothing ready is parked, which is the case with no alternative")
	}
}

func TestVariantsSkipWhatItCannotWarmRatherThanGuessing(t *testing.T) {
	// A warm copy started with different options than the ordinary replicas is
	// a different torch.compile cache key: ~9 s of extra compile, and silently
	// so. Skipping is the only safe answer.
	sidecarOnly := corev1.Container{Name: "proxy", Args: []string{"--listen", ":8080"}}

	for _, tc := range []struct {
		name    string
		objects []client.Object
		ds      datastore.Datastore
		target  string
	}{
		{
			name:    "no scale target has been resolved yet",
			objects: []client.Object{deployment(1, vllmContainer())},
			ds:      &fakeDatastore{pool: inferencePool()},
			target:  "",
		},
		{
			name:    "the scale target cannot be read",
			objects: nil,
			ds:      &fakeDatastore{pool: inferencePool()},
			target:  scaleTarget,
		},
		{
			name:    "no container names a --model",
			objects: []client.Object{deployment(1, sidecarOnly)},
			ds:      &fakeDatastore{pool: inferencePool()},
			target:  scaleTarget,
		},
		{
			name:    "the workload belongs to no InferencePool",
			objects: []client.Object{deployment(1, vllmContainer())},
			ds:      &fakeDatastore{err: errors.New("no pool matches")},
			target:  scaleTarget,
		},
		{
			name:    "its InferencePool has an empty selector",
			objects: []client.Object{deployment(1, vllmContainer())},
			ds:      &fakeDatastore{pool: &poolutil.EndpointPool{Name: "empty", Namespace: "tenant"}},
			target:  scaleTarget,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, reg, _ := demandFor(t, tc.objects, tc.ds)
			registerVariant(reg, tc.target)

			got, err := d.Variants(context.Background())
			if err != nil {
				t.Fatalf("one unwarmable variant must not fail the read: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("Variants = %+v, want none", got)
			}
		})
	}
}

func TestOnlyThePoolsOwnNamespaceIsWarmed(t *testing.T) {
	// The registry spans every namespace WVA watches, while an Adapter is
	// pinned to one -- and a variant is keyed by the ScaledObject's NAME, which
	// is unique only within a namespace. Without this filter, two tenants can
	// collide on a single warm-set entry, and one tenant's traffic could be
	// pointed at the other's engine.
	// The other namespace needs a REAL scale target, or it would be skipped for
	// having none and the filter would look effective without being exercised.
	elsewhere := deployment(1, vllmContainer())
	elsewhere.Namespace = "other-tenant"

	d, reg, _ := demandFor(t,
		[]client.Object{deployment(1, vllmContainer()), elsewhere},
		&fakeDatastore{pool: inferencePool()})
	d.Namespace = "tenant"

	registerVariant(reg, scaleTarget)
	// The same variant name, in someone else's namespace.
	reg.Observe("other-tenant", "qwen", nil)
	reg.SetTarget("other-tenant", "qwen", registry.Target{Kind: "Deployment", Name: scaleTarget})

	got, err := d.Variants(context.Background())
	if err != nil {
		t.Fatalf("Variants: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Variants = %+v, want only this pool's namespace", got)
	}
	if got[0].Model.Namespace != "tenant" {
		t.Errorf("warmed %s, want tenant", got[0].Model.Namespace)
	}
}

func TestOnlyTheFlagsAWarmCopyMustMatchAreCopied(t *testing.T) {
	// These options become the argv of a process the pool runs on a GPU with
	// the shared model cache mounted, and they are copied from a Deployment the
	// tenant owns. Several vLLM flags load and run code the caller names; one
	// turns the namespace-open serving port into an arbitrary file read.
	spec := specWith(nil, []string{
		"--model", "Qwen/Qwen3-0.6B",
		"--gpu-memory-utilization", "0.95",
		"--trust-remote-code",
		"--tool-parser-plugin", "/model-cache/evil.py",
		"--allowed-local-media-path", "/",
		"--otlp-traces-endpoint", "http://elsewhere/",
		"--logits-processors", "attacker.Module",
	})
	got, err := EngineOptionsFrom(spec)
	if err != nil {
		t.Fatalf("EngineOptionsFrom: %v", err)
	}

	for _, kept := range []string{"--model Qwen/Qwen3-0.6B", "--gpu-memory-utilization 0.95"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q must survive -- it is part of what makes the copy the same engine: %q", kept, got)
		}
	}
	for _, dropped := range []string{
		"trust-remote-code", "tool-parser-plugin", "allowed-local-media-path",
		"otlp-traces-endpoint", "logits-processors", "evil.py", "attacker.Module",
	} {
		if strings.Contains(got, dropped) {
			t.Errorf("%q must not reach the pool's argv: %q", dropped, got)
		}
	}
}

func TestADroppedFlagTakesItsValueWithIt(t *testing.T) {
	// Otherwise the value is left as a bare word, which a later parse could
	// read as something else entirely.
	got, err := EngineOptionsFrom(specWith(nil, []string{
		"--model", "m", "--tool-parser-plugin", "/model-cache/evil.py", "--dtype", "bfloat16",
	}))
	if err != nil {
		t.Fatalf("EngineOptionsFrom: %v", err)
	}
	if strings.Contains(got, "evil.py") {
		t.Fatalf("the dropped flag's value survived: %q", got)
	}
	if !strings.Contains(got, "--dtype bfloat16") {
		t.Errorf("and the flag after it must be unaffected: %q", got)
	}
}

func TestShareIsAVariantsPortionOfTheFleet(t *testing.T) {
	// What the design asks for is share of REQUESTS; what exists in this
	// process is share of desired replicas. They are the same quantity scaled
	// by per-replica capacity, because holding utilisation roughly constant is
	// what an autoscaler does -- so a variant WVA wants three replicas of is
	// serving about three times the traffic of one it wants one of.
	busy := deployment(1, vllmContainer())
	quiet := deployment(1, vllmContainer())
	quiet.Name = "quiet-decode"

	d, reg, decisions := demandFor(t,
		[]client.Object{busy, quiet},
		&fakeDatastore{pool: inferencePool()})

	registerVariant(reg, scaleTarget)
	reg.Observe("tenant", "quiet", nil)
	reg.SetTarget("tenant", "quiet", registry.Target{Kind: "Deployment", Name: "quiet-decode"})

	decisions.Set("tenant", scaleTarget, 3)
	decisions.Set("tenant", "quiet-decode", 1)

	got, err := d.Variants(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("Variants = %+v, %v", got, err)
	}
	shares := map[string]float64{}
	for _, v := range got {
		shares[v.Model.Variant] = v.Share
	}
	if shares["qwen"] <= shares["quiet"] {
		t.Errorf("the busier variant must rank higher: %v", shares)
	}
	if total := shares["qwen"] + shares["quiet"]; total < 0.99 || total > 1.01 {
		t.Errorf("shares must sum to one, got %v", total)
	}
}

func TestAnEntirelyParkedFleetHasNoPopularitySignal(t *testing.T) {
	// Nothing is running, so nothing is busier than anything else. Preloading
	// falls back to what it was before: inert, with admission driven by parking
	// and the frequency filter, which is exactly right when every variant is
	// parked and therefore already admitted eagerly.
	d, reg, decisions := demandFor(t,
		[]client.Object{deployment(0, vllmContainer())},
		&fakeDatastore{pool: inferencePool()})
	registerVariant(reg, scaleTarget)
	decisions.Set("tenant", scaleTarget, 0)

	got, err := d.Variants(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("Variants = %+v, %v", got, err)
	}
	// Asserting Share == 0 alone would prove nothing: zero is its zero value,
	// so deleting withShares entirely would pass. Pin that shares ARE computed
	// for a running fleet and are absent only for a parked one, which is the
	// distinction this case is about.
	if got[0].Share != 0 {
		t.Errorf("share = %v, want 0 when nothing is running", got[0].Share)
	}
	decisions.Set("tenant", scaleTarget, 2)
	running, err := d.Variants(context.Background())
	if err != nil || len(running) != 1 {
		t.Fatalf("Variants = %+v, %v", running, err)
	}
	if running[0].Share == 0 {
		t.Error("the same variant must have a share once it is running; " +
			"a zero here means nothing computes shares at all")
	}
}

func TestThePoolCanSizeItsOwnGPUUtilisation(t *testing.T) {
	// A workload's value is chosen for a Pod running ONE engine and claims most
	// of the card; a pool Pod has to leave room for its sleepers' residue.
	d, reg, _ := demandFor(t,
		[]client.Object{deployment(1, vllmContainer())},
		&fakeDatastore{pool: inferencePool()})
	d.GPUMemoryUtilization = 0.6
	registerVariant(reg, scaleTarget)

	got, err := d.Variants(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("Variants = %+v, %v", got, err)
	}
	options := got[0].Model.EngineOptions
	if !strings.Contains(options, "--gpu-memory-utilization 0.6") {
		t.Errorf("the pool's value must be applied: %q", options)
	}
	// Replaced, not appended: two occurrences would leave the engine's
	// behaviour to argparse's last-wins rule and the intent to a reader's guess.
	if strings.Count(options, "--gpu-memory-utilization") != 1 {
		t.Errorf("the workload's value must be replaced, not duplicated: %q", options)
	}
	if strings.Contains(options, "0.95") {
		t.Errorf("the workload's value survived: %q", options)
	}
}

func TestWithoutAnOverrideTheWorkloadsUtilisationIsKept(t *testing.T) {
	// Zero inherits, which is the previous behaviour and the right default: the
	// value is part of the torch.compile cache key, so matching the ordinary
	// replicas is what makes the pool hit the cache they populated.
	d, reg, _ := demandFor(t,
		[]client.Object{deployment(1, vllmContainer())},
		&fakeDatastore{pool: inferencePool()})
	registerVariant(reg, scaleTarget)

	got, err := d.Variants(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("Variants = %+v, %v", got, err)
	}
	if !strings.Contains(got[0].Model.EngineOptions, "--gpu-memory-utilization 0.95") {
		t.Errorf("the workload's own value must survive: %q", got[0].Model.EngineOptions)
	}
}

func TestParallelismAndExpertLayoutSurviveIntoTheWarmCopy(t *testing.T) {
	// A warm copy has to be laid out like the replicas it stands in for. Drop
	// --enable-expert-parallel and the same model is arranged differently: a
	// different torch.compile cache key, a different memory profile, and a copy
	// that is neither as fast nor as large as the thing it replaces.
	got, err := EngineOptionsFrom(specWith(nil, []string{
		"--model", "mistralai/Mixtral-8x7B",
		"--tensor-parallel-size", "4",
		"--pipeline-parallel-size", "2",
		"--data-parallel-size", "2",
		"--enable-expert-parallel",
		"--enable-eplb",
	}))
	if err != nil {
		t.Fatalf("EngineOptionsFrom: %v", err)
	}
	for _, want := range []string{
		"--tensor-parallel-size 4",
		"--pipeline-parallel-size 2",
		"--data-parallel-size 2",
		"--enable-expert-parallel",
		"--enable-eplb",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%q must survive into the warm copy: %q", want, got)
		}
	}
}
