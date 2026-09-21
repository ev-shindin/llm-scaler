package capacity

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

var _ = Describe("ParseVLLMArgs", func() {

	Describe("Argument formats", func() {
		It("should parse hyphen format (--gpu-memory-utilization=0.85)", func() {
			deploy := makeTestDeployment("--gpu-memory-utilization=0.85")
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.GpuMemoryUtilization).To(Equal(0.85))
		})

		It("should parse underscore format (--gpu_memory_utilization=0.85)", func() {
			deploy := makeTestDeployment("--gpu_memory_utilization=0.85")
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.GpuMemoryUtilization).To(Equal(0.85))
		})

		It("should parse space-separated format (--gpu-memory-utilization 0.85)", func() {
			deploy := makeTestDeployment("--gpu-memory-utilization", "0.85")
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.GpuMemoryUtilization).To(Equal(0.85))
		})
	})

	Describe("Shell command parsing", func() {
		It("should parse args from shell command string", func() {
			deploy := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:    "vllm",
									Command: []string{"/bin/sh", "-c", "vllm serve model-name --gpu-memory-utilization=0.85 --max-num-seqs=128"},
								},
							},
						},
					},
				},
			}
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.GpuMemoryUtilization).To(Equal(0.85))
			Expect(params.MaxNumSeqs).To(Equal(int64(128)))
		})

		It("should parse a multi-line, backslash-continued shell command", func() {
			// Matches the shape this repo's own scenarios (and, in practice,
			// most real vLLM deployments) actually write: a long `vllm
			// serve ...` invocation split across many lines, each ending in
			// `\`. Every flag here is on its own physical line -- exactly
			// the pattern that glued a stray "\\\n" onto every flag after
			// the first one, defeating the "--" prefix check silently.
			script := "vllm serve /model-cache/model \\\n" +
				"--host 0.0.0.0 \\\n" +
				"--block-size 128 \\\n" +
				"--max-num-seq 256 \\\n" +
				"--max-num-batched-tokens 65536 \\\n" +
				"--tensor-parallel-size 1 \\\n" +
				"--gpu-memory-utilization 0.9 \\\n" +
				"--no-enable-prefix-caching"
			deploy := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:    "vllm",
									Command: []string{"/bin/bash", "-c"},
									Args:    []string{script},
								},
							},
						},
					},
				},
			}
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.BlockSize).To(Equal(int64(128)))
			Expect(params.MaxNumSeqs).To(Equal(int64(256)))
			Expect(params.MaxNumBatchedTokens).To(Equal(int64(65536)))
			Expect(params.EffectiveMaxBatchedTokens).To(Equal(int64(65536)))
			Expect(params.TensorParallelSize).To(Equal(1))
			Expect(params.GpuMemoryUtilization).To(Equal(0.9))
		})

		It("should parse preamble lines with no trailing backslash before the flags", func() {
			// The bare-newline case: this repo's customCommand blocks start
			// with a couple of plain statements (env sourcing, an
			// accelerator preamble) that end in a plain newline, not `\`.
			script := "export FOO=bar\n" +
				". /shared-config/env.sh\n" +
				"vllm serve /model-cache/model \\\n" +
				"--max-num-batched-tokens 8192"
			deploy := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:    "vllm",
									Command: []string{"/bin/sh", "-c"},
									Args:    []string{script},
								},
							},
						},
					},
				},
			}
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.MaxNumBatchedTokens).To(Equal(int64(8192)))
		})

		It("should parse an indented continuation, where the next line starts with a tab", func() {
			// The same failure as an unhandled backslash-newline, reached a
			// different way: whitespace that is not a space, left glued to the
			// front of the next token, makes it fail the "--" prefix check and
			// the flag is skipped in silence. A YAML block scalar is indented
			// by definition, so this is the normal shape, not an edge case.
			script := "vllm serve /model-cache/model \\\n" +
				"\t--max-num-batched-tokens 4096 \\\n" +
				"\t--max-num-seqs 32"
			deploy := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:    "vllm",
									Command: []string{"/bin/sh", "-c"},
									Args:    []string{script},
								},
							},
						},
					},
				},
			}
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.MaxNumBatchedTokens).To(Equal(int64(4096)))
			Expect(params.MaxNumSeqs).To(Equal(int64(32)))
		})

		It("should parse a command whose line endings are CRLF", func() {
			// A manifest authored on Windows carries \r\n, so the continuation
			// is backslash-CR-LF. Without handling it, both the backslash and
			// the carriage return survive into the token stream.
			script := "vllm serve /model-cache/model \\\r\n" +
				"--max-num-batched-tokens 2048 \\\r\n" +
				"--max-num-seqs 16"
			deploy := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:    "vllm",
									Command: []string{"/bin/sh", "-c"},
									Args:    []string{script},
								},
							},
						},
					},
				},
			}
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.MaxNumBatchedTokens).To(Equal(int64(2048)))
			Expect(params.MaxNumSeqs).To(Equal(int64(16)))
		})
	})

	Describe("Default values", func() {
		It("should return vLLM defaults when no args are provided", func() {
			deploy := makeTestDeployment() // no args
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))

			Expect(params.GpuMemoryUtilization).To(Equal(0.9))
			Expect(params.BlockSize).To(Equal(int64(16)))
			Expect(params.KvCacheDtype).To(Equal("auto"))
			Expect(params.TensorParallelSize).To(Equal(1))
			Expect(params.MaxNumSeqs).To(Equal(int64(256)))
			Expect(params.NumGpuBlocksOverride).To(Equal(int64(0)))
			Expect(params.MaxNumBatchedTokens).To(Equal(int64(0)))
			Expect(params.MaxModelLen).To(Equal(int64(0)))
			Expect(params.EnforceEager).To(BeFalse())
			Expect(params.IsV1Engine).To(BeTrue())
			Expect(params.ChunkedPrefillEnabled).To(BeTrue())
			// V1 engine default: 8192 (since vLLM v0.8)
			Expect(params.EffectiveMaxBatchedTokens).To(Equal(int64(8192)))
		})
	})

	Describe("All capacity-related params", func() {
		It("should parse all known capacity parameters", func() {
			deploy := makeTestDeployment(
				"--gpu-memory-utilization=0.85",
				"--block-size=32",
				"--kv-cache-dtype=fp8",
				"--tensor-parallel-size=4",
				"--max-num-batched-tokens=4096",
				"--max-num-seqs=128",
				"--max-model-len=8192",
			)
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))

			Expect(params.GpuMemoryUtilization).To(Equal(0.85))
			Expect(params.BlockSize).To(Equal(int64(32)))
			Expect(params.KvCacheDtype).To(Equal("fp8"))
			Expect(params.TensorParallelSize).To(Equal(4))
			Expect(params.MaxNumBatchedTokens).To(Equal(int64(4096)))
			Expect(params.MaxNumSeqs).To(Equal(int64(128)))
			Expect(params.MaxModelLen).To(Equal(int64(8192)))
			Expect(params.EffectiveMaxBatchedTokens).To(Equal(int64(4096)))
		})
	})

	Describe("Boolean flag detection", func() {
		It("should detect --enforce-eager as a boolean flag", func() {
			deploy := makeTestDeployment("--enforce-eager", "--gpu-memory-utilization=0.85")
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))

			Expect(params.EnforceEager).To(BeTrue())
			Expect(params.GpuMemoryUtilization).To(Equal(0.85))
		})
	})

	Describe("NumGpuBlocksOverride", func() {
		It("should parse --num-gpu-blocks-override", func() {
			deploy := makeTestDeployment("--num-gpu-blocks-override=5000")
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.NumGpuBlocksOverride).To(Equal(int64(5000)))
		})
	})

	Describe("V1 engine detection", func() {
		It("should default to V1 engine when no VLLM_USE_V1 env var is set", func() {
			deploy := makeTestDeployment()
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.IsV1Engine).To(BeTrue())
			Expect(params.ChunkedPrefillEnabled).To(BeTrue())
		})

		It("should detect V1 engine when VLLM_USE_V1=1", func() {
			deploy := makeDeploymentWithEnv(
				[]corev1.EnvVar{{Name: "VLLM_USE_V1", Value: "1"}},
			)
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.IsV1Engine).To(BeTrue())
			Expect(params.ChunkedPrefillEnabled).To(BeTrue())
		})

		It("should detect V0 engine when VLLM_USE_V1=0", func() {
			deploy := makeDeploymentWithEnv(
				[]corev1.EnvVar{{Name: "VLLM_USE_V1", Value: "0"}},
			)
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.IsV1Engine).To(BeFalse())
			Expect(params.ChunkedPrefillEnabled).To(BeFalse())
		})

		It("should enable chunked prefill on V0 engine with --enable-chunked-prefill", func() {
			deploy := makeDeploymentWithEnvAndArgs(
				[]corev1.EnvVar{{Name: "VLLM_USE_V1", Value: "0"}},
				"--enable-chunked-prefill",
			)
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.IsV1Engine).To(BeFalse())
			Expect(params.ChunkedPrefillEnabled).To(BeTrue())
			Expect(params.EffectiveMaxBatchedTokens).To(Equal(int64(2048)))
		})
	})

	Describe("EffectiveMaxBatchedTokens resolution", func() {
		It("should use explicit --max-num-batched-tokens when set", func() {
			deploy := makeTestDeployment("--max-num-batched-tokens=4096")
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.EffectiveMaxBatchedTokens).To(Equal(int64(4096)))
		})

		It("should default to 8192 for V1 engine chunked prefill", func() {
			deploy := makeTestDeployment() // V1 default → chunked
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.EffectiveMaxBatchedTokens).To(Equal(int64(8192)))
		})

		It("should default to 2048 for V0 engine chunked prefill", func() {
			deploy := makeDeploymentWithEnvAndArgs(
				[]corev1.EnvVar{{Name: "VLLM_USE_V1", Value: "0"}},
				"--enable-chunked-prefill",
			)
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.EffectiveMaxBatchedTokens).To(Equal(int64(2048)))
		})

		It("should use max_model_len for unchunked prefill when larger than 2048", func() {
			deploy := makeDeploymentWithEnvAndArgs(
				[]corev1.EnvVar{{Name: "VLLM_USE_V1", Value: "0"}},
				"--max-model-len=8192",
			)
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.ChunkedPrefillEnabled).To(BeFalse())
			Expect(params.EffectiveMaxBatchedTokens).To(Equal(int64(8192)))
		})

		It("should fallback to 2048 for unchunked prefill with small model len", func() {
			deploy := makeDeploymentWithEnvAndArgs(
				[]corev1.EnvVar{{Name: "VLLM_USE_V1", Value: "0"}},
				"--max-model-len=1024",
			)
			params := ParseVLLMArgs(scaletarget.NewDeploymentAccessor(deploy))
			Expect(params.EffectiveMaxBatchedTokens).To(Equal(int64(2048)))
		})
	})

	Describe("Nil deployment", func() {
		It("should return defaults for nil deployment", func() {
			params := ParseVLLMArgs(nil)
			Expect(params.GpuMemoryUtilization).To(Equal(0.9))
			Expect(params.IsV1Engine).To(BeTrue())
			// V1 engine default: 8192
			Expect(params.EffectiveMaxBatchedTokens).To(Equal(int64(8192)))
		})
	})
})

var _ = Describe("IsCapacityCompatible", func() {
	It("should return true for identical default params", func() {
		p1 := defaultEngineParams()
		resolveEffectiveMaxBatchedTokens(&p1)
		p2 := defaultEngineParams()
		resolveEffectiveMaxBatchedTokens(&p2)
		Expect(p1.IsCapacityCompatible(&p2)).To(BeTrue())
	})

	It("should return false when GpuMemoryUtilization differs", func() {
		p1 := defaultEngineParams()
		resolveEffectiveMaxBatchedTokens(&p1)
		p2 := defaultEngineParams()
		resolveEffectiveMaxBatchedTokens(&p2)
		p2.GpuMemoryUtilization = 0.5
		Expect(p1.IsCapacityCompatible(&p2)).To(BeFalse())
	})

	It("should return false when BlockSize differs", func() {
		p1 := defaultEngineParams()
		resolveEffectiveMaxBatchedTokens(&p1)
		p2 := defaultEngineParams()
		resolveEffectiveMaxBatchedTokens(&p2)
		p2.BlockSize = 32
		Expect(p1.IsCapacityCompatible(&p2)).To(BeFalse())
	})

	It("should return false when KvCacheDtype differs", func() {
		p1 := defaultEngineParams()
		resolveEffectiveMaxBatchedTokens(&p1)
		p2 := defaultEngineParams()
		resolveEffectiveMaxBatchedTokens(&p2)
		p2.KvCacheDtype = "fp8"
		Expect(p1.IsCapacityCompatible(&p2)).To(BeFalse())
	})

	It("should return false when TensorParallelSize differs", func() {
		p1 := defaultEngineParams()
		resolveEffectiveMaxBatchedTokens(&p1)
		p2 := defaultEngineParams()
		resolveEffectiveMaxBatchedTokens(&p2)
		p2.TensorParallelSize = 4
		Expect(p1.IsCapacityCompatible(&p2)).To(BeFalse())
	})

	It("should return false when EffectiveMaxBatchedTokens differs", func() {
		p1 := defaultEngineParams()
		resolveEffectiveMaxBatchedTokens(&p1)
		p2 := defaultEngineParams()
		resolveEffectiveMaxBatchedTokens(&p2)
		p2.EffectiveMaxBatchedTokens = 4096
		Expect(p1.IsCapacityCompatible(&p2)).To(BeFalse())
	})

	It("should return false when the Engine differs (no cross-engine capacity reuse)", func() {
		// Same model on the same hardware served by two engines must not share a
		// capacity record: vLLM and SGLang derive KV capacity from different sources.
		p1 := defaultEngineParams() // Engine == EngineVLLM
		resolveEffectiveMaxBatchedTokens(&p1)
		p2 := defaultSGLangEngineParams() // Engine == EngineSGLang
		// Force every other capacity field equal so only Engine distinguishes them.
		p2.GpuMemoryUtilization = p1.GpuMemoryUtilization
		p2.BlockSize = p1.BlockSize
		p2.KvCacheDtype = p1.KvCacheDtype
		p2.TensorParallelSize = p1.TensorParallelSize
		p2.NumGpuBlocksOverride = p1.NumGpuBlocksOverride
		p2.TotalKvTokensOverride = p1.TotalKvTokensOverride
		resolveEffectiveMaxBatchedTokens(&p2)
		p2.EffectiveMaxBatchedTokens = p1.EffectiveMaxBatchedTokens

		Expect(p1.Engine).NotTo(Equal(p2.Engine))
		Expect(p1.IsCapacityCompatible(&p2)).To(BeFalse())
	})

	It("should treat same-engine parsed params as Engine-compatible", func() {
		p1 := ParseEngineArgs(inferenceengine.EngineSGLang, scaletarget.NewDeploymentAccessor(makeTestDeployment()))
		p2 := ParseEngineArgs(inferenceengine.EngineSGLang, scaletarget.NewDeploymentAccessor(makeTestDeployment()))
		Expect(p1.Engine).To(Equal(inferenceengine.EngineSGLang))
		Expect(p1.IsCapacityCompatible(&p2)).To(BeTrue())
	})

	It("should return false when TotalKvTokensOverride differs", func() {
		// Two SGLang variants differing only in --max-total-tokens must NOT be
		// treated as capacity-compatible: the override directly sets KV capacity.
		p1 := defaultEngineParams()
		resolveEffectiveMaxBatchedTokens(&p1)
		p2 := defaultEngineParams()
		resolveEffectiveMaxBatchedTokens(&p2)
		p1.TotalKvTokensOverride = 100000
		p2.TotalKvTokensOverride = 200000
		Expect(p1.IsCapacityCompatible(&p2)).To(BeFalse())
	})

	It("should return false when either param is nil", func() {
		p1 := defaultEngineParams()
		resolveEffectiveMaxBatchedTokens(&p1)
		Expect(p1.IsCapacityCompatible(nil)).To(BeFalse())

		var p2 *EngineParams
		Expect(p2.IsCapacityCompatible(&p1)).To(BeFalse())
	})

	It("should ignore non-capacity fields like MaxNumSeqs and MaxModelLen", func() {
		p1 := defaultEngineParams()
		resolveEffectiveMaxBatchedTokens(&p1)
		p2 := defaultEngineParams()
		resolveEffectiveMaxBatchedTokens(&p2)
		p2.MaxNumSeqs = 512
		p2.MaxModelLen = 16384
		p2.EnforceEager = true
		Expect(p1.IsCapacityCompatible(&p2)).To(BeTrue())
	})
})

// makeDeploymentWithEnv creates a deployment with the given env vars and no extra args.
func makeDeploymentWithEnv(envVars []corev1.EnvVar) *appsv1.Deployment {
	return &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "vllm",
							Command: []string{"vllm", "serve", "model-name"},
							Env:     envVars,
						},
					},
				},
			},
		},
	}
}

// makeDeploymentWithEnvAndArgs creates a deployment with env vars and CLI args.
func makeDeploymentWithEnvAndArgs(envVars []corev1.EnvVar, args ...string) *appsv1.Deployment {
	return &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "vllm",
							Command: []string{"vllm", "serve", "model-name"},
							Args:    args,
							Env:     envVars,
						},
					},
				},
			},
		},
	}
}
