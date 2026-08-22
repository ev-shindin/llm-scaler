package warmpool

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func specWith(command, args []string) *corev1.PodSpec {
	return &corev1.PodSpec{Containers: []corev1.Container{
		{Name: "sidecar", Args: []string{"--not-a-model-server"}},
		{Name: "vllm", Command: command, Args: args},
	}}
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
