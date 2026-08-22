package pool

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

// What a warm copy would COST, worked out from the options it would run with.
//
// The pool admits models into a Pod that has finite host memory and a fixed
// number of GPUs, and until this existed it checked neither. Both omissions have
// the same shape and the same consequence: the admission is accepted, the load
// runs, and it fails — after ~35 s, holding a reserve slot the whole time, and
// then again on the next cycle, because nothing records that this variant is the
// one that cannot fit.
//
// The memory case is worse than a wasted slot. A level-1 sleeper keeps its
// weights in HOST memory, and the container has a hard limit; admitting one
// model too many does not fail that admission, it OOM-kills the launcher and
// destroys every model already resident in that Pod.

// Shape is what one engine would occupy.
type Shape struct {
	// WeightsBytes is what the weights alone would take in host memory when the
	// instance sleeps. Zero means it could not be worked out.
	WeightsBytes int64
	// GPUs is how many devices the engine needs: the product of the
	// parallelism sizes, and at least one.
	GPUs int
}

// Known reports whether the memory estimate means anything.
func (s Shape) Known() bool { return s.WeightsBytes > 0 }

// paramCount finds a parameter count in a model reference.
//
// Model names carry it by near-universal convention -- Qwen3-0.6B,
// Llama-3.1-8B-Instruct, Mixtral-8x7B -- and that convention is the only source
// available here: the controller does not mount the weights, so it cannot read a
// config.json, and asking the tenant would put the number under the control of
// whoever benefits from understating it.
//
// The delimiters matter. Requiring one on each side is what stops the "3.1" in
// Llama-3.1-8B being read as 3.1 billion parameters, and the LAST match wins
// because a family name may carry digits of its own.
var paramCount = regexp.MustCompile(`(?i)(?:^|[-_/.])(?:(\d+)x)?(\d+(?:\.\d+)?)b(?:$|[-_/.])`)

// ShapeOf estimates what an engine started with these options would occupy.
func ShapeOf(options string) Shape {
	shape := Shape{GPUs: parallelism(options)}

	params, ok := paramsOf(flagValue(options, "--model"))
	if !ok {
		return shape
	}
	shape.WeightsBytes = int64(params * float64(bytesPerParam(options)))
	return shape
}

// paramsOf returns a parameter count, in parameters.
func paramsOf(model string) (float64, bool) {
	matches := paramCount.FindAllStringSubmatch(model, -1)
	if len(matches) == 0 {
		return 0, false
	}
	last := matches[len(matches)-1]

	billions, err := strconv.ParseFloat(last[2], 64)
	if err != nil || billions <= 0 {
		return 0, false
	}
	// "8x7B" is a mixture of experts: 8 experts of 7B each, and ALL of them are
	// resident. Reading it as 7B would understate the weights eightfold, which
	// is precisely the direction that OOMs a Pod.
	if last[1] != "" {
		if experts, err := strconv.ParseFloat(last[1], 64); err == nil && experts > 0 {
			billions *= experts
		}
	}
	return billions * 1e9, true
}

// bytesPerParam errs upward on purpose.
//
// Overestimating declines an admission that would have fitted, which costs a
// cold start. Underestimating OOM-kills the Pod and takes every other model
// resident in it. So an unrecognised dtype is assumed to be the common one
// rather than the smallest, and quantisation narrower than 8-bit is not
// credited at all.
func bytesPerParam(options string) int {
	dtype := strings.ToLower(flagValue(options, "--dtype"))
	quant := strings.ToLower(flagValue(options, "--quantization"))

	switch {
	case strings.Contains(dtype, "float32"), dtype == "float", dtype == "fp32":
		return 4
	case strings.Contains(dtype, "float8"), strings.Contains(dtype, "fp8"),
		strings.Contains(quant, "fp8"), strings.Contains(quant, "float8"):
		return 1
	default:
		// bfloat16, float16, "auto", anything unrecognised, and every 4-bit
		// scheme -- which really weighs about half a byte per parameter, but is
		// counted at two so that a wrong guess refuses an admission instead of
		// killing a Pod.
		return 2
	}
}

// parallelism is how many GPUs the engine would want.
func parallelism(options string) int {
	gpus := 1
	for _, flag := range []string{
		"--tensor-parallel-size",
		"--pipeline-parallel-size",
		"--data-parallel-size",
	} {
		if n, err := strconv.Atoi(flagValue(options, flag)); err == nil && n > 1 {
			gpus *= n
		}
	}
	if gpus < 1 || gpus > math.MaxInt32 {
		return 1
	}
	return gpus
}

// flagValue reads one option's value, tokenised, last occurrence winning --
// for the same reason portOf is tokenised: a value containing something that
// looks like another flag must not be read as one.
func flagValue(options, name string) string {
	value := ""
	fields := strings.Fields(options)
	for i := 0; i < len(fields); i++ {
		field, inline, hasInline := strings.Cut(fields[i], "=")
		if field != name {
			continue
		}
		switch {
		case hasInline:
			value = inline
		case i+1 < len(fields):
			value = fields[i+1]
			i++
		}
	}
	return value
}
