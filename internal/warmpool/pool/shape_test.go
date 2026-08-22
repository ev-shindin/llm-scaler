package pool

import "testing"

func TestParameterCountsComeFromTheModelName(t *testing.T) {
	// The only source available: the controller does not mount the weights, so
	// it cannot read a config.json, and asking the tenant would put the number
	// under the control of whoever benefits from understating it.
	const gb = 1 << 30
	for _, tc := range []struct {
		options string
		want    int64 // in bytes, bf16 unless stated
		known   bool
	}{
		{options: "--model Qwen/Qwen3-0.6B", want: int64(0.6 * 2e9), known: true},
		{options: "--model meta-llama/Llama-3.1-8B-Instruct", want: 16e9, known: true},
		{options: "--model Qwen/Qwen2.5-0.5B-Instruct", want: 1e9, known: true},
		{options: "--model openai/gpt-oss-120b", want: 240e9, known: true},

		// A mixture of experts holds ALL its experts resident. Reading 8x7B as
		// 7B would understate the weights eightfold, which is exactly the
		// direction that OOMs a Pod.
		{options: "--model mistralai/Mixtral-8x7B-Instruct-v0.1", want: 112e9, known: true},

		// fp8 halves it; float32 doubles it.
		{options: "--model meta-llama/Llama-3.1-8B --quantization fp8", want: 8e9, known: true},
		{options: "--model meta-llama/Llama-3.1-8B --dtype float32", want: 32e9, known: true},

		// A 4-bit scheme really weighs about half a byte per parameter, and is
		// deliberately counted at two: a wrong guess should refuse an admission,
		// not kill a Pod.
		{options: "--model meta-llama/Llama-3.1-8B --quantization awq", want: 16e9, known: true},

		// No parameter count to find.
		{options: "--model BAAI/bge-m3", known: false},
		{options: "--dtype bfloat16", known: false},
	} {
		got := ShapeOf(tc.options)
		if got.Known() != tc.known {
			t.Errorf("ShapeOf(%q).Known() = %v, want %v", tc.options, got.Known(), tc.known)
			continue
		}
		if !tc.known {
			continue
		}
		// Within a GiB: the estimate is an estimate, and the assertion should
		// not be more precise than the thing it measures.
		if diff := got.WeightsBytes - tc.want; diff > gb || diff < -gb {
			t.Errorf("ShapeOf(%q).WeightsBytes = %d, want about %d", tc.options, got.WeightsBytes, tc.want)
		}
	}
}

func TestAVersionNumberIsNotAParameterCount(t *testing.T) {
	// "Llama-3.1-8B" must read as 8B and never as 3.1B. Requiring a delimiter
	// on each side of the number is what makes that work, and getting it wrong
	// understates a model by more than half.
	got := ShapeOf("--model meta-llama/Llama-3.1-8B-Instruct")
	if !got.Known() {
		t.Fatal("want a known size")
	}
	if got.WeightsBytes < 15e9 {
		t.Errorf("WeightsBytes = %d; the version number was read as the parameter count",
			got.WeightsBytes)
	}
}

func TestParallelismIsTheProductOfTheSizes(t *testing.T) {
	// What decides whether an engine can start in a Pod at all. A warm copy
	// inherits these flags from the ordinary replicas, so a tensor-parallel
	// workload asks for more devices than a single-GPU pool Pod has.
	for _, tc := range []struct {
		options string
		want    int
	}{
		{options: "--model m", want: 1},
		{options: "--model m --tensor-parallel-size 2", want: 2},
		{options: "--model m --tensor-parallel-size=4", want: 4},
		{options: "--model m --tensor-parallel-size 2 --pipeline-parallel-size 2", want: 4},
		{options: "--model m --tensor-parallel-size 1", want: 1},
	} {
		if got := ShapeOf(tc.options).GPUs; got != tc.want {
			t.Errorf("ShapeOf(%q).GPUs = %d, want %d", tc.options, got, tc.want)
		}
	}
}

func TestAFlagValueCannotImpersonateAFlag(t *testing.T) {
	// Same trap as the port: a value containing something that looks like
	// another flag must not be read as one. Here it would understate a model's
	// size, which is the direction that OOMs a Pod.
	got := ShapeOf("--model /model-cache/w--dtype=float8/Llama-3.1-8B")
	if !got.Known() {
		t.Fatal("want a known size")
	}
	if got.WeightsBytes < 15e9 {
		t.Errorf("WeightsBytes = %d; a dtype inside the model path was read as the dtype",
			got.WeightsBytes)
	}
}
