package pool

import (
	"fmt"
	"testing"
)

// The two points this estimate is fitted to, measured on an H100 with vLLM
// level-1 sleep. They are what the arithmetic must reproduce; the formula is
// only how it gets there.
const (
	gib = 1 << 30
	// A 0.6B asleep charged its container 4.1 GiB over baseline.
	measuredSleeper06B = 4.1 * gib
	// An 8B charged 23.4 GiB -- against 14.9 GiB of weights, which is why
	// estimating weights alone was wrong by 57% in the direction that OOMs.
	measuredSleeper8B = 23.4 * gib
)

func TestTheEstimateReproducesWhatWasMeasured(t *testing.T) {
	// Level-1 sleep moves the weights into SHARED memory and the container's
	// cgroup is charged for all of it. Anonymous memory barely moves, which is
	// what made this easy to get wrong: measuring `anon` alone shows a sleeper
	// costing nothing at all.
	for _, tc := range []struct {
		options string
		want    float64
	}{
		{options: "--model Qwen/Qwen3-0.6B", want: measuredSleeper06B},
		{options: "--model Qwen/Qwen3-8B", want: measuredSleeper8B},
	} {
		got := ShapeOf(tc.options)
		if !got.Known() {
			t.Errorf("ShapeOf(%q) could not size it", tc.options)
			continue
		}
		// Within 15%: this is a two-point fit over a name-derived parameter
		// count, and an assertion tighter than the estimate would be theatre.
		if diff := float64(got.HostBytes) - tc.want; diff > tc.want*0.15 || diff < -tc.want*0.15 {
			t.Errorf("ShapeOf(%q).HostBytes = %.1f GiB, measured %.1f GiB",
				tc.options, float64(got.HostBytes)/gib, tc.want/gib)
		}
	}
}

func TestTheEstimateIsNeverBelowTheWeights(t *testing.T) {
	// The failure that matters is an estimate UNDER the truth: it admits a
	// model that does not fit and OOM-kills the Pod, taking every model already
	// resident with it. A sleeper always costs more than its weights.
	for _, tc := range []struct {
		options string
		weights float64
	}{
		{options: "--model Qwen/Qwen3-0.6B", weights: 1.2e9},
		{options: "--model meta-llama/Llama-3.1-8B", weights: 16e9},
		{options: "--model mistralai/Mixtral-8x7B", weights: 112e9},
	} {
		if got := ShapeOf(tc.options); float64(got.HostBytes) <= tc.weights {
			t.Errorf("ShapeOf(%q).HostBytes = %d, not above its %.0f bytes of weights",
				tc.options, got.HostBytes, tc.weights)
		}
	}
}

func TestParameterCountsComeFromTheModelName(t *testing.T) {
	// The only source available: the controller does not mount the weights, so
	// it cannot read a config.json, and asking the tenant would put the number
	// under the control of whoever benefits from understating it.
	//
	// Asserted through the ORDERING these produce rather than through exact
	// byte counts, so the cases stay about the parsing they are named for.
	bigger := func(a, b string) {
		t.Helper()
		if x, y := ShapeOf(a), ShapeOf(b); x.HostBytes <= y.HostBytes {
			t.Errorf("%q (%d) should size larger than %q (%d)", a, x.HostBytes, b, y.HostBytes)
		}
	}
	bigger("--model meta-llama/Llama-3.1-8B-Instruct", "--model Qwen/Qwen3-0.6B")
	bigger("--model openai/gpt-oss-120b", "--model meta-llama/Llama-3.1-8B")
	// A mixture of experts holds ALL its experts resident, so 8x7B is 56B and
	// not 7B. Reading it as 7B understates the weights eightfold.
	bigger("--model mistralai/Mixtral-8x7B", "--model meta-llama/Llama-3.1-8B")
	// float32 doubles it; fp8 halves it.
	bigger("--model meta-llama/Llama-3.1-8B --dtype float32", "--model meta-llama/Llama-3.1-8B")
	bigger("--model meta-llama/Llama-3.1-8B", "--model meta-llama/Llama-3.1-8B --quantization fp8")
	// A 4-bit scheme really weighs about half a byte per parameter and is
	// deliberately counted at two: a wrong guess should refuse an admission,
	// not kill a Pod.
	if awq, plain := ShapeOf("--model meta-llama/Llama-3.1-8B --quantization awq"),
		ShapeOf("--model meta-llama/Llama-3.1-8B"); awq.HostBytes != plain.HostBytes {
		t.Errorf("4-bit is not credited: %d vs %d", awq.HostBytes, plain.HostBytes)
	}

	for _, unsizeable := range []string{"--model BAAI/bge-m3", "--dtype bfloat16"} {
		if got := ShapeOf(unsizeable); got.Known() {
			t.Errorf("ShapeOf(%q) claimed to size it: %d", unsizeable, got.HostBytes)
		}
	}
}

func TestDataParallelismMultipliesTheHostCost(t *testing.T) {
	// Measured: a DP=2 sleeper held twice the shmem of a TP=1 one, while a TP=2
	// sleeper held about the same. Each DP rank is its own engine with its own
	// copy of the weights; tensor parallelism shards ONE copy across devices.
	one := ShapeOf("--model Qwen/Qwen3-8B")
	dp := ShapeOf("--model Qwen/Qwen3-8B --data-parallel-size 2")
	tp := ShapeOf("--model Qwen/Qwen3-8B --tensor-parallel-size 2")

	if dp.HostBytes != 2*one.HostBytes {
		t.Errorf("DP=2 costs %d, want twice the %d of one rank", dp.HostBytes, one.HostBytes)
	}
	if tp.HostBytes != one.HostBytes {
		t.Errorf("TP=2 costs %d, want the same %d: it shards one copy", tp.HostBytes, one.HostBytes)
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
	// Reading "3.1" as the count would size this at well under a tenth of an
	// 8B, so anything near the real figure proves the delimiters did their job.
	if got.HostBytes < 15e9 {
		t.Errorf("HostBytes = %d; the version number was read as the parameter count",
			got.HostBytes)
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
	if got.HostBytes < 15e9 {
		t.Errorf("HostBytes = %d; a dtype inside the model path was read as the dtype",
			got.HostBytes)
	}
}

func TestExpertParallelismDoesNotAskForGPUsOfItsOwn(t *testing.T) {
	// --enable-expert-parallel shards a mixture-of-experts model's experts
	// across the ranks tensor and data parallelism already provide. Counting it
	// as another multiplier would refuse admissions that fit perfectly well.
	plain := ShapeOf("--model mistralai/Mixtral-8x7B --tensor-parallel-size 2")
	withEP := ShapeOf("--model mistralai/Mixtral-8x7B --tensor-parallel-size 2 --enable-expert-parallel")

	if withEP.GPUs != plain.GPUs {
		t.Errorf("expert parallelism changed the GPU count: %d vs %d", withEP.GPUs, plain.GPUs)
	}
	if withEP.GPUs != 2 {
		t.Errorf("GPUs = %d, want the tensor-parallel size", withEP.GPUs)
	}
}

func TestDataParallelismMultipliesBecauseEachRankIsAnEngine(t *testing.T) {
	// Unlike expert parallelism: a DP rank is its own engine replica with its
	// own devices, so it really does need more of them.
	got := ShapeOf("--model m --tensor-parallel-size 2 --data-parallel-size 2")
	if got.GPUs != 4 {
		t.Errorf("GPUs = %d, want 4", got.GPUs)
	}
}

func TestShapeIsMemoisedWithoutChangingTheAnswer(t *testing.T) {
	// The memo is a pure-function cache: the same options must give the same
	// Shape whether computed or recalled, and different options must not
	// collide. A memo that returned a neighbour's answer would mis-size a warm
	// set, which is the failure that OOM-kills a Pod and takes every model in it.
	small := "--model Qwen/Qwen3-0.6B --tensor-parallel-size 1"
	big := "--model meta-llama/Llama-3.1-8B --tensor-parallel-size 2"

	first := ShapeOf(small)
	if first != ShapeOf(small) {
		t.Fatal("a memo hit must equal the computed value")
	}
	if first != computeShape(small) {
		t.Fatal("the memo must not diverge from the function it caches")
	}

	other := ShapeOf(big)
	if other == first {
		t.Fatal("different options must not share an entry")
	}
	if other != computeShape(big) {
		t.Fatal("the second entry is wrong")
	}
}

func TestTheShapeMemoStaysBounded(t *testing.T) {
	// Variants come and go over a controller's life, so an unbounded memo would
	// only ever grow. Cleared wholesale rather than evicted one at a time --
	// recomputing costs microseconds, so the simplest correct policy wins.
	for i := 0; i < shapeMemoMax+50; i++ {
		ShapeOf(fmt.Sprintf("--model org/model-%dB", i%97+1))
	}
	shapeMu.Lock()
	size := len(shapeMemo)
	shapeMu.Unlock()
	if size > shapeMemoMax {
		t.Fatalf("memo grew past its bound: %d", size)
	}
}
