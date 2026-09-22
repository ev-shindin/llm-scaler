package capacity

import (
	"strconv"
	"strings"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// ParseEngineArgs parses a scale target's container args using the parser
// appropriate for the given inference engine, returning the shared
// EngineParams. It dispatches to ParseSGLangArgs for SGLang and ParseVLLMArgs
// otherwise (the default), so existing vLLM behavior is unchanged.
func ParseEngineArgs(engine inferenceengine.Engine, scaleTarget scaletarget.ScaleTargetAccessor) EngineParams {
	if engine == inferenceengine.EngineSGLang {
		return ParseSGLangArgs(scaleTarget)
	}
	return ParseVLLMArgs(scaleTarget)
}

// EngineParams holds inference-engine configuration parameters parsed from a
// Deployment/LWS's container args and environment variables. These are used
// to derive compute-bound capacity (k2) when no live metrics are available.
//
// It is the shared engine-params type for all supported engines: ParseVLLMArgs
// populates it from vLLM flags and ParseSGLangArgs populates it from SGLang flags
// (mapped onto the same fields). Field comments note the per-engine flag mapping.
type EngineParams struct {
	// Engine records which inference engine produced these params (set by the
	// parser: EngineVLLM for ParseVLLMArgs, EngineSGLang for ParseSGLangArgs).
	// IsCapacityCompatible compares it so a capacity record learned for one engine
	// is never reused as the zero-replica estimate for a different engine serving
	// the same model on the same hardware.
	Engine inferenceengine.Engine

	GpuMemoryUtilization  float64 // default: 0.9
	BlockSize             int64   // default: 16 (vLLM block size / SGLang page size)
	KvCacheDtype          string  // default: "auto"
	TensorParallelSize    int     // default: 1
	NumGpuBlocksOverride  int64   // default: 0 (not set) — vLLM only
	MaxNumBatchedTokens   int64   // default: 0 (auto)
	MaxNumSeqs            int64   // default: 256 (vLLM --max-num-seqs / SGLang --max-running-requests)
	MaxModelLen           int64   // default: 0 (auto) (vLLM --max-model-len / SGLang --context-length)
	EnforceEager          bool    // default: false (vLLM --enforce-eager / SGLang --disable-cuda-graph)
	IsV1Engine            bool    // VLLM_USE_V1 env detection (default: true since v0.8); always true for SGLang
	ChunkedPrefillEnabled bool    // true for V1, or --enable-chunked-prefill

	// TotalKvTokensOverride is the explicit total KV-cache token capacity, when the
	// engine exposes it as a deployment flag (SGLang --max-total-tokens). 0 = unset.
	// vLLM has no equivalent flag and uses NumGpuBlocksOverride instead.
	TotalKvTokensOverride int64

	// EffectiveMaxBatchedTokens is the resolved per-step token budget used
	// for k2 derivation. It is computed after parsing all other fields.
	EffectiveMaxBatchedTokens int64
}

// defaultEngineParams returns EngineParams with vLLM defaults
// as of vLLM v0.8+. If vLLM changes its defaults in a future version,
// these values should be updated accordingly.
func defaultEngineParams() EngineParams {
	return EngineParams{
		Engine:                inferenceengine.EngineVLLM,
		GpuMemoryUtilization:  0.9,
		BlockSize:             16,
		KvCacheDtype:          "auto",
		TensorParallelSize:    1,
		MaxNumSeqs:            256,
		IsV1Engine:            true, // default since vLLM v0.8
		ChunkedPrefillEnabled: true, // V1 engine uses chunked prefill by default
	}
}

// ParseVLLMArgs scans a Deployment/LWS's containers for vLLM CLI arguments
// and environment variables, returning the parsed parameters.
//
// It handles:
//   - --key=value and --key value argument formats
//   - Hyphen/underscore normalization (--gpu-memory-utilization = --gpu_memory_utilization)
//   - Shell commands: ["/bin/sh", "-c", "vllm serve model --arg=val"]
//   - Boolean flags: --enforce-eager (no value)
//   - VLLM_USE_V1 environment variable for V1 engine detection
func ParseVLLMArgs(scaleTarget scaletarget.ScaleTargetAccessor) EngineParams {
	params := defaultEngineParams()
	if scaleTarget == nil {
		resolveEffectiveMaxBatchedTokens(&params)
		return params
	}

	podTemplateSpec := scaleTarget.GetLeaderPodTemplateSpec()
	if podTemplateSpec == nil || len(podTemplateSpec.Spec.Containers) == 0 {
		resolveEffectiveMaxBatchedTokens(&params)
		return params
	}

	for _, container := range podTemplateSpec.Spec.Containers {
		// Check environment variables first
		for _, env := range container.Env {
			if env.Name == "VLLM_USE_V1" {
				if env.Value == "0" {
					params.IsV1Engine = false
					params.ChunkedPrefillEnabled = false // V0 default
				}
				// Any other value (including "1", empty) keeps V1 = true
			}
		}

		// Collect all args from Command + Args, handling shell commands
		allArgs := collectArgs(container.Command, container.Args)

		// Parse the collected arguments
		parseArgs(allArgs, &params)
	}

	// V1 engine always enables chunked prefill regardless of flag
	if params.IsV1Engine {
		params.ChunkedPrefillEnabled = true
	}

	resolveEffectiveMaxBatchedTokens(&params)
	return params
}

// collectArgs merges container Command and Args, expanding shell commands.
// If the command is a shell invocation (e.g. ["/bin/sh", "-c", "..."]),
// the shell string is split into tokens.
func collectArgs(command, args []string) []string {
	all := make([]string, 0, len(command)+len(args))
	all = append(all, command...)
	all = append(all, args...)

	// Detect shell invocation: ["/bin/sh", "-c", "cmd ..."] or similar
	for i := 0; i < len(all)-1; i++ {
		base := all[i]
		if (base == "/bin/sh" || base == "/bin/bash" || base == "sh" || base == "bash") && i+1 < len(all) && all[i+1] == "-c" && i+2 < len(all) {
			// Split the shell command string
			shellTokens := splitShellString(all[i+2])
			return shellTokens
		}
	}

	return all
}

// splitShellString performs basic shell-like splitting on a command string.
// It handles simple single/double quoting, and shell line-continuation
// (a backslash immediately followed by a newline, the idiomatic way to write
// a long `vllm serve ...` invocation across multiple lines -- used by every
// scenario in this repo and, in practice, by real deployments generally). It
// is not a full shell parser: escape sequences (\"), variable expansion
// ($VAR), and command substitution are not supported.
//
// Line continuation matters more than it looks: left unhandled, the
// backslash and newline at the end of one line get glued onto the front of
// the next line's first token (e.g. "\\\n--max-num-batched-tokens" instead
// of "--max-num-batched-tokens"). That fails the "--" prefix check in
// parseArgsWith, so every flag after the first physical line of a multi-line
// command was silently skipped -- not a rare edge case, but the normal shape
// of a customCommand block. A bare newline (no preceding backslash, as
// between this function's non-flag preamble lines) is treated the same as a
// space: it ends a token, but isn't itself content.
func splitShellString(s string) []string {
	var tokens []string
	var current strings.Builder
	inSingleQuote := false
	inDoubleQuote := false

	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\\' && !inSingleQuote && !inDoubleQuote && isLineBreakAt(s, i+1):
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
			if s[i+1] == '\r' {
				i++ // CRLF: consume the carriage return too
			}
			i++ // consume the newline
		case ch == '\'' && !inDoubleQuote:
			inSingleQuote = !inSingleQuote
		case ch == '"' && !inSingleQuote:
			inDoubleQuote = !inDoubleQuote
		case isSpace(ch) && !inSingleQuote && !inDoubleQuote:
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		default:
			current.WriteByte(ch)
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	return tokens
}

// isSpace reports whether ch separates tokens. Tabs and carriage returns
// count: a YAML block scalar indented with tabs, or a manifest authored on
// Windows, otherwise glues the whitespace onto the next token, which then
// fails the "--" prefix check in parseArgsWith and is silently skipped --
// the same failure mode as an unhandled line continuation.
func isSpace(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r'
}

// isLineBreakAt reports whether position i begins a newline, LF or CRLF, so a
// trailing backslash is recognised as a line continuation in both.
func isLineBreakAt(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	return s[i] == '\n' || (s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n')
}

// normalizeKey replaces hyphens with underscores and strips the leading
// dashes so that --gpu-memory-utilization and --gpu_memory_utilization
// both normalize to "gpu_memory_utilization".
func normalizeKey(key string) string {
	key = strings.TrimLeft(key, "-")
	return strings.ReplaceAll(key, "-", "_")
}

// parseArgs walks the argument list and populates params using the vLLM flag mapping.
func parseArgs(args []string, params *EngineParams) {
	parseArgsWith(args, params, applyParam)
}

// parseArgsWith walks the argument list and applies each normalized --key/value
// pair via the supplied apply function. It is shared by the vLLM and SGLang
// parsers, which differ only in their per-flag mapping (applyParam vs
// applySGLangParam). Boolean flags (no following value) are passed with an empty
// value string.
func parseArgsWith(args []string, params *EngineParams, apply func(key, value string, params *EngineParams)) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			continue
		}

		var key, value string
		if idx := strings.Index(arg, "="); idx >= 0 {
			// --key=value format
			key = normalizeKey(arg[:idx])
			value = arg[idx+1:]
		} else {
			key = normalizeKey(arg)
			// Check if next token is the value (not another flag)
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				value = args[i+1]
				i++ // consume the value
			}
			// Otherwise it's a boolean flag (no value)
		}

		apply(key, value, params)
	}
}

// applyParam sets the corresponding EngineParams field from a
// normalized key and its string value. Parse errors are silently ignored
// and the default value is preserved — this is intentional graceful
// degradation since deployment args are operator-controlled.
func applyParam(key, value string, params *EngineParams) {
	switch key {
	case "gpu_memory_utilization":
		if v, err := strconv.ParseFloat(value, 64); err == nil {
			params.GpuMemoryUtilization = v
		}
	case "block_size":
		if v, err := strconv.ParseInt(value, 10, 64); err == nil {
			params.BlockSize = v
		}
	case "kv_cache_dtype":
		params.KvCacheDtype = value
	case "tensor_parallel_size":
		if v, err := strconv.Atoi(value); err == nil {
			params.TensorParallelSize = v
		}
	case "num_gpu_blocks_override":
		if v, err := strconv.ParseInt(value, 10, 64); err == nil {
			params.NumGpuBlocksOverride = v
		}
	case "max_num_batched_tokens":
		if v, err := strconv.ParseInt(value, 10, 64); err == nil {
			params.MaxNumBatchedTokens = v
		}
	case "max_num_seq", "max_num_seqs":
		// vLLM accepts both --max-num-seq and --max-num-seqs (confirmed live:
		// this repo's own scenarios invoke the singular form, and vLLM starts
		// normally with it), but only the plural form was recognized here --
		// the singular one fell through to the same "unrecognized flag" path
		// as an actually-unknown flag, silently leaving MaxNumSeqs at its
		// struct default.
		if v, err := strconv.ParseInt(value, 10, 64); err == nil {
			params.MaxNumSeqs = v
		}
	case "max_model_len":
		if v, err := strconv.ParseInt(value, 10, 64); err == nil {
			params.MaxModelLen = v
		}
	case "enforce_eager":
		params.EnforceEager = true
	case "enable_chunked_prefill":
		params.ChunkedPrefillEnabled = true
	}
}

// IsCapacityCompatible checks whether two EngineParams configurations
// would produce equivalent per-replica capacity (both k1 and k2).
// Used by Store.FindCompatible to identify variants
// whose stored capacity can be reused for zero-replica estimation.
func (p *EngineParams) IsCapacityCompatible(other *EngineParams) bool {
	if p == nil || other == nil {
		return false
	}
	return p.Engine == other.Engine &&
		p.GpuMemoryUtilization == other.GpuMemoryUtilization &&
		p.BlockSize == other.BlockSize &&
		p.KvCacheDtype == other.KvCacheDtype &&
		p.TensorParallelSize == other.TensorParallelSize &&
		p.NumGpuBlocksOverride == other.NumGpuBlocksOverride &&
		p.TotalKvTokensOverride == other.TotalKvTokensOverride &&
		p.EffectiveMaxBatchedTokens == other.EffectiveMaxBatchedTokens
}

// resolveEffectiveMaxBatchedTokens computes the per-step token budget
// based on parsed parameters. This is the value used for k2 derivation.
//
// Priority:
//  1. Explicitly set --max-num-batched-tokens → use that
//  2. V1 engine with chunked prefill → 8192 (vLLM V1 default since v0.8)
//  3. V0 engine with chunked prefill → 2048 (vLLM V0 default since v0.6.5)
//  4. Unchunked prefill → max(MaxModelLen, 2048)
//  5. Fallback → 2048
func resolveEffectiveMaxBatchedTokens(params *EngineParams) {
	if params.MaxNumBatchedTokens > 0 {
		params.EffectiveMaxBatchedTokens = params.MaxNumBatchedTokens
		return
	}

	if params.ChunkedPrefillEnabled {
		if params.IsV1Engine {
			params.EffectiveMaxBatchedTokens = 8192
		} else {
			params.EffectiveMaxBatchedTokens = 2048
		}
		return
	}

	// Unchunked prefill
	if params.MaxModelLen > 2048 {
		params.EffectiveMaxBatchedTokens = params.MaxModelLen
		return
	}

	params.EffectiveMaxBatchedTokens = 2048
}
