#!/usr/bin/env bash
# Apply our fixes to the llm-d-benchmark clone.
#
# The clone is gitignored (.gitignore:33) and benchmark-install re-checks it out
# to a pinned tag, so anything edited in place is invisible and disappears on the
# next install. Fixes therefore live here, in a versioned script, and are
# reapplied after every install.
#
# The fixes below are upstream's bugs unless a block says otherwise (fix 3 is
# ours), present in v0.7.8 and on origin/main, so there is no release to
# upgrade to. Each block says whether it is reported upstream; delete a block
# when a release carries the fix.
#
# Why patching the clone reaches the cluster at all: step_06 builds the
# `llmdbench-harness-scripts` ConfigMap from the CHECKED-OUT tree --
# workload/harnesses/* plus llmdbenchmark/analysis/scripts/*-analyze_results.*
# -- and mounts it into the harness pod, "so a run can use a new/updated harness
# with an older benchmark image". Files outside those two sets (notably
# benchmark_report/native_to_br0_2.py) ship only in the image and cannot be
# fixed from here.
#
# Every edit is anchored on an exact upstream string and is idempotent. A
# missing anchor is a hard error, not a skip: a silent no-op after a version
# bump would leave us believing a fix is applied when it is not, which is the
# same failure mode that let an unversioned maxReplicas sit in the clone for
# weeks.
set -eu
# --help prints this file's header comment -- the documentation the script
# already carries, so it cannot drift from what the script does. Placed before
# any argument handling because several of these take a namespace as $1, and
# without it `--help` was consumed as one.
case "${1:-}" in
    -h|--help)
        sed -n '2,/^[^#]/p' "$0" | sed 's/^# \{0,1\}//; $d'
        exit 0
        ;;
esac


REPO_DIR="${1:?usage: patch_harness.sh <llm-d-benchmark clone dir>}"
[ -d "$REPO_DIR" ] || { echo "patch_harness: no clone at $REPO_DIR" >&2; exit 1; }

PY=${PYTHON:-python3}
applied=0
skipped=0

note()  { printf '  %s\n' "$1"; }
fail()  { printf 'patch_harness: %s\n' "$1" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Fix 1 -- process_epp_logs.py: EPP emits "ts" as a float, not an ISO string.
#
#   ts_str = re.sub(r"(\.\d{6})\d+", r"\1", ts_str)
#   TypeError: expected string or bytes-like object, got 'float'
#
# EPP writes {"ts":1786849846.9172602,...} on EVERY line, so the parser dies on
# the first entry and every EPP metric is discarded. That is why "Avg queue
# depth (EPP)" has been "?" in every run we have ever taken -- 49.8 MB of logs
# per run, parsed to nothing. The harness truncates stderr to 200 chars and
# calls it non-fatal, which is why the cause stayed hidden.
# ---------------------------------------------------------------------------
EPP="$REPO_DIR/workload/harnesses/process_epp_logs.py"
[ -f "$EPP" ] || fail "expected file missing: $EPP"

"$PY" - "$EPP" <<'PYEOF' || fail "fix 1 (process_epp_logs.py) failed"
import io, sys

path = sys.argv[1]
src = io.open(path, encoding="utf-8").read()

MARK = "# wva-patch: numeric ts"
if MARK in src:
    print("  fix 1 (EPP float ts): already applied")
    sys.exit(0)

IMPORT_OLD = "from datetime import datetime\n"
IMPORT_NEW = "from datetime import datetime, timezone\n"

# Anchor on the exact line that raises, and insert the numeric branch ahead of
# it -- after the falsy guard, so ts=0/"" keeps returning None as before.
ANCHOR = '    # Handle nanosecond timestamps by truncating to 6 decimal places\n'
BRANCH = (
    MARK + ": EPP logs carry epoch seconds as a JSON number, not an ISO\n"
    "    # string. re.sub() then raises TypeError on the first entry and the whole\n"
    "    # log is dropped. Naive UTC here matches the ISO path below, which strips\n"
    "    # the trailing Z and returns a naive datetime -- mixing the two would make\n"
    "    # every downstream subtraction raise.\n"
    "    if isinstance(ts_str, (int, float)) and not isinstance(ts_str, bool):\n"
    "        return datetime.fromtimestamp(ts_str, timezone.utc).replace(tzinfo=None)\n"
)

if IMPORT_OLD not in src:
    sys.exit("anchor missing: %r" % IMPORT_OLD)
if ANCHOR not in src:
    sys.exit("anchor missing: %r" % ANCHOR)

src = src.replace(IMPORT_OLD, IMPORT_NEW, 1)
src = src.replace(ANCHOR, "    " + BRANCH + ANCHOR, 1)

io.open(path, "w", encoding="utf-8", newline="\n").write(src)
print("  fix 1 (EPP float ts): applied")
PYEOF

# ---------------------------------------------------------------------------
# Fix 2 -- guidellm-analyze_results.sh: stop a broken report conversion from
# failing an otherwise good run.
#
#   native_to_br0_2.py:2751  native["config"] = data["args"]
#   KeyError: 'args'
#
# benchmark-report expects guidellm's old top-level "args" (its invocation
# arguments: model, data sources, rate). Current guidellm emits "metadata",
# "config", "benchmarks" and no "args" at all, so the lookup always raises --
# the guard above it, `if not native.get("config")`, never helps because
# nothing populates it for guidellm (benchmark-report has no config-file flag;
# "-w guidellm" names the generator, not a file).
#
# We do NOT alias "config" onto "args". They are not the same thing: "config"
# holds the llm-d-benchmark workload profile (metadata/spec/benchmarks --
# prefill_heavy and friends), not guidellm's arguments. Aliasing gets past the
# KeyError and then dies on `get_nested(data, ["args","data"])` returning None
# two lines later; worse, had it survived, it would have written a report whose
# recorded config was the wrong document entirely. A silently wrong report is
# worse than no report. Mapping the new schema onto the old is a real,
# version-coupled job and belongs upstream.
#
# So: keep reporting the failure loudly, but do not propagate it. The pod's
# entrypoint does `exit $((LOADGEN_EC + REPORT_EC))`, so a failed conversion
# marks the whole run FAILED and burns MAX_TRIES x 30s retrying a deterministic
# error. That cost is real and the artifact is not: nothing we run consumes
# benchmark_report v0.1/v0.2 -- postprocess.py reads results.json directly and
# the replica timeline comes from our own sampler. The FAILED banner is not
# free either; it is what led us to misread a valid run as a lost one.
#
# The conversion itself runs inside the pod against code baked into the image
# (/opt/benchmark_report), so it cannot be fixed from here at all. Only the
# analyzer script can, because step_06 ships it from this clone.
# ---------------------------------------------------------------------------
ANALYZER="$REPO_DIR/llmdbenchmark/analysis/scripts/guidellm-analyze_results.sh"
[ -f "$ANALYZER" ] || fail "expected file missing: $ANALYZER"

"$PY" - "$ANALYZER" <<'PYEOF' || fail "fix 2 (guidellm-analyze_results.sh) failed"
import io, sys

path = sys.argv[1]
src = io.open(path, encoding="utf-8").read()

MARK = "# wva-patch: conversion is not fatal"
if MARK in src:
    print("  fix 2 (report conversion non-fatal): already applied")
    sys.exit(0)

ANCHOR = (
    'if [[ $LLMDBENCH_RUN_EXPERIMENT_CONVERT_RC -ne 0 ]]; then\n'
    '  echo "Results data conversion completed with errors."\n'
    '  exit $LLMDBENCH_RUN_EXPERIMENT_CONVERT_RC\n'
    'fi\n'
)
if ANCHOR not in src:
    sys.exit("anchor missing (upstream shape changed): %r" % ANCHOR)

REPLACEMENT = (
    'if [[ $LLMDBENCH_RUN_EXPERIMENT_CONVERT_RC -ne 0 ]]; then\n'
    '  echo "Results data conversion completed with errors."\n'
    '  ' + MARK + '\n'
    '  # benchmark-report cannot read current guidellm output (it wants a\n'
    '  # top-level "args" that guidellm no longer emits). The measurement data\n'
    '  # in results.json is unaffected and is what we actually consume, so do\n'
    '  # not fail the run over the derived report: the entrypoint exits\n'
    '  # $((LOADGEN_EC + REPORT_EC)), which would mark a good run FAILED and\n'
    '  # retry a deterministic error MAX_TRIES x 30s.\n'
    '  echo "NOTE: benchmark_report v0.1/v0.2 were NOT produced (upstream bug,'
    ' present in v0.7.8 and main)."\n'
    '  echo "NOTE: results.json is complete; treat this run as valid."\n'
    '  LLMDBENCH_RUN_EXPERIMENT_CONVERT_RC=0\n'
    'fi\n'
)

src = src.replace(ANCHOR, REPLACEMENT, 1)
io.open(path, "w", encoding="utf-8", newline="\n").write(src)
print("  fix 2 (report conversion non-fatal): applied")
PYEOF

# ---------------------------------------------------------------------------
# Fix 3 -- give the FMA warm pool a placement mode that can span nodes.
#
# THE PROBLEM WITH WHAT UPSTREAM SHIPS
#
# A wake that finds a sleeping vLLM is ready in ~3s; one that rebuilds takes
# ~50-90s. dual-pods binds a requester to a launcher on the SAME NODE, so which
# one you get is a scheduling outcome. Upstream's answer is
# `fma.launcherNodeSelection`: step_06_fma_deploy.py scores GPU nodes, takes the
# single best one, strips the label from every OTHER node, labels its pick, and
# then sizes requester replicas, LauncherPopulationPolicy launcherCount and the
# KEDA ScaledObject ceiling to that node's free GPU count at that instant.
#
# That maximises the hit rate by collapsing placement to a point, and it makes
# the pool unscalable in three separate ways:
#
#   * The pool cannot grow past one node. Labeling a second one does not help --
#     the next standup's `kubectl label nodes -l <key>=true <key>-` removes it.
#   * The scaling ceiling is a point-in-time reading of one node's free GPUs.
#   * With no single node free, standup FAILS ("All GPU nodes are currently
#     occupied") -- the common case on a shared cluster, and exactly when warm
#     capacity is worth the most.
#
# WHAT THIS ADDS
#
# `fma.warmAffinity`: instead of pinning both halves to a fixed node, let the
# requester FOLLOW the warm pool. The launchers spread over every eligible GPU
# node (the LauncherPopulationPolicy already does this whenever node selection
# is off), and the requester expresses a PREFERENCE for nodes that already hold
# a sleeper.
#
# Two weighted terms rather than one, because they answer different questions:
#
#   weight 100  dual-pods.llm-d.ai/sleeping=true -- a wakeable sleeper is here.
#               This is the 3s path, so it outranks everything.
#   weight  50  app.kubernetes.io/component=launcher -- a launcher is here at
#               all. Covers the window before any instance has gone to sleep
#               (initial standup, or right after a scale-up), where the first
#               term scores every node zero.
#
# preferredDuringScheduling, never required: when the warm set is full or absent
# the pod still schedules and rebuilds cold, which is what it would have done
# anyway. A hard predicate would leave it Pending instead -- trading a slow
# replica for no replica.
#
# Sizing then belongs to hack/benchmark/warm_pool.sh, which takes REPLICAS and
# divides them across the policy's node set. Placement stays infra config,
# warming stays a separate step, and only the size changes hands.
#
# WHAT THIS DOES NOT BUY -- measured on pokprod, twice, 2026-08-18
#
# Placement, not wakes. Both runs put 3/3 replicas on a node holding a sleeper
# and consumed sleepers doing it, and all six replicas still REBUILT (43-79s).
# The reuse key is the GPU UUID, and a pool node has 7-8 GPUs while a requester
# reserves one (the launcher reserves none and uses its requester's). So the
# right node still leaves roughly a 1-in-7 chance of the right GPU.
#
# That is also why launcherNodeSelection works where this does not, and the
# reason is not the pinning: step_06 sizes requesters to the node's FREE GPU
# count, saturating it, so every sleeper's GPU is matched by exhaustion. The
# trade-off is therefore span-nodes OR wake-reliably, not both, until the reuse
# key stops hashing GPU UUIDs. This mode is still the right default on a shared
# cluster -- it is free, and it is a precondition for any match at all.
#
# Scope note: the selector matches launchers by component, not by model. A
# benchmark namespace serves one model, so this is precise there. Serving
# several from one namespace would need `llm-d.ai/model` added to both terms --
# but that label is only present on BOUND launchers, so a sleeping-launcher
# selector cannot use it as-is.
#
# This is ours, not an upstream bug, so unlike fixes 1 and 2 there is no release
# to wait for. Drop it if upstream grows a spreading placement mode.
# ---------------------------------------------------------------------------
FMATMPL="$REPO_DIR/config/templates/jinja/24_fma-deployment.yaml.j2"
[ -f "$FMATMPL" ] || fail "expected file missing: $FMATMPL"

"$PY" - "$FMATMPL" <<'PYEOF' || fail "fix 3 (fma warmAffinity) failed"
import io, sys

path = sys.argv[1]
src = io.open(path, encoding="utf-8").read()

MARK = "# wva-patch: warm-pool affinity"
if MARK in src:
    print("  fix 3 (fma warmAffinity): already applied")
    sys.exit(0)

# The requester pod spec's affinity block. Unique in the template: the
# LauncherPopulationPolicy above it uses enhancedNodeSelector, not affinity.
ANCHOR = (
    "      affinity:\n"
    "        nodeAffinity:\n"
    "          requiredDuringSchedulingIgnoredDuringExecution:\n"
)
if ANCHOR not in src:
    sys.exit("anchor missing (upstream shape changed): %r" % ANCHOR)

# Inserted INSIDE the existing affinity block, before nodeAffinity. That
# nodeAffinity stays load-bearing: WVA's resolver reads the accelerator name out
# of it, and an accelerator it cannot resolve silences the saturation engine.
INSERT = (
    "      affinity:\n"
    "{% if fma.warmAffinity is defined and fma.warmAffinity.enabled | default(false) %}\n"
    "        " + MARK + ": prefer nodes that already hold warm capacity.\n"
    "        # Binding is node-local, so a requester that lands where no sleeper\n"
    "        # lives rebuilds from scratch (~50-90s) instead of waking one (~3s).\n"
    "        # Preferred, not required: with the warm set full or absent the pod\n"
    "        # still schedules and rebuilds, rather than sitting Pending.\n"
    "        podAffinity:\n"
    "          preferredDuringSchedulingIgnoredDuringExecution:\n"
    "            # A sleeper here is the 3s path; outranks a mere launcher.\n"
    "            - weight: {{ fma.warmAffinity.sleeperWeight | default(100) }}\n"
    "              podAffinityTerm:\n"
    "                topologyKey: kubernetes.io/hostname\n"
    "                labelSelector:\n"
    "                  matchLabels:\n"
    "                    dual-pods.llm-d.ai/sleeping: \"true\"\n"
    "            # Before anything has slept, the term above scores every node\n"
    "            # zero. Fall back to where the launchers are at all.\n"
    "            - weight: {{ fma.warmAffinity.launcherWeight | default(50) }}\n"
    "              podAffinityTerm:\n"
    "                topologyKey: kubernetes.io/hostname\n"
    "                labelSelector:\n"
    "                  matchLabels:\n"
    "                    app.kubernetes.io/component: launcher\n"
    "{% endif %}\n"
    "        nodeAffinity:\n"
    "          requiredDuringSchedulingIgnoredDuringExecution:\n"
)

src = src.replace(ANCHOR, INSERT, 1)
io.open(path, "w", encoding="utf-8", newline="\n").write(src)
print("  fix 3 (fma warmAffinity): applied")
PYEOF

# ---------------------------------------------------------------------------
# Fix 6 -- defaults.yaml: harness.resources.memory is hardcoded 32Gi in the clone.
#
# This is the memory limit actually applied to the inference-perf harness pod
# for every `make benchmark-run` invocation. A separate override existed
# (BENCHMARK_HARNESS_MEMORY, sed'd into this same file) but only inside the
# benchmark-run-bursty target, which patches the value, runs, then restores
# the .bak -- a one-off for that target alone. benchmark-run itself never
# touched this file, so every run through it -- including staged/bursty
# profiles run via benchmark-run rather than benchmark-run-bursty -- got the
# clone's stock 32Gi regardless.
#
# Measured on pokprod001: a 60-minute, 5-stage, 33120-request inference-perf
# run (per_request logging disabled) was OOMKilled (exit 137) at 3358s of an
# expected 3600s. The harness accumulates per-request lifecycle records for
# summary/per_stage percentile computation for the whole run even with
# per_request off, and 32Gi was not enough at this request count.
#
# Patched HERE, unconditionally, so it applies regardless of which run target
# is used -- the same reason fix 4 lives here rather than in a scenario file.
HARNESS_MEMORY="${HARNESS_MEMORY:-64Gi}"
DEFAULTS="$REPO_DIR/config/templates/values/defaults.yaml"
if [ ! -f "$DEFAULTS" ]; then
    note "fix 6 (harness memory): no defaults.yaml, skipped"
else
    "$PY" - "$DEFAULTS" "$HARNESS_MEMORY" <<'PYEOF' || fail "fix 6 (harness memory) failed"
import io, re, sys

path, want = sys.argv[1], sys.argv[2]
src = io.open(path, encoding="utf-8").read()

# Anchored on the harness resources block itself (cpu line included), not
# just on the number: memory: 32Gi appears in the file body only once today,
# but anchoring on context rather than the bare value is what makes this
# survive both a value change (this fix already made) and an upstream
# reformat.
pat = re.compile(
    r"(harness:\n(?:.*\n)*?  resources:\n    cpu:\s*\d+\n    memory:\s*)([0-9A-Za-z.]+)(\s*\n)"
)
m = pat.search(src)
if not m:
    sys.exit("anchor missing (upstream shape changed): harness.resources.memory")
if m.group(2) == want:
    print("  fix 6 (harness memory): already %s" % want)
    sys.exit(0)
src = pat.sub(lambda mm: mm.group(1) + want + mm.group(3), src, count=1)
io.open(path, "w", encoding="utf-8", newline="\n").write(src)
print("  fix 6 (harness memory): %s -> %s" % (m.group(2), want))
PYEOF
fi

echo "patch_harness: done ($REPO_DIR)"

# ---------------------------------------------------------------------------
# Fix 4 -- defaults.yaml: the GPU memory fraction is hardcoded 0.95 in the clone.
#
# This is the value that actually reaches the model server. The scenario's
# model.gpuMemoryUtilization is read by the capacity validator, but the DEPLOYED
# fraction comes from a YAML anchor in the clone's own template defaults:
#
#   config/templates/values/defaults.yaml:45   gpu_memory_util: &gpu_memory_util 0.95
#   config/templates/values/defaults.yaml:274  gpuMemoryUtilization: *gpu_memory_util
#
# Editing the scenario therefore changed nothing, three times over: the log said
# 0.90 had been installed and the Deployment came up with
# VLLM_ACCELERATOR_MEM_UTIL=0.95 anyway. Confirmed by vLLM's own report --
# "GPU KV cache size: 688,448 tokens", the 0.95 figure, where 0.90 predicts 651k.
#
# 0.95 of an 80GB card leaves 4 GiB, which is less than a co-tenant holding the
# device without requesting it -- an FMA launcher requests ZERO GPUs while
# running the engine, so the scheduler places a model on a card it believes is
# empty. Measured on pokprod001: 77.3 of 79.18 GiB free at startup, then
# `torch.OutOfMemoryError: tried to allocate 594MiB, 500MiB free`. It succeeds
# whenever the card happens to be emptier, which is why it reads as flakiness.
#
# Patched HERE rather than in the scenario because the clone is gitignored and
# benchmark-install re-checks it out to the pinned tag: anything written before
# that is discarded, and this target is what re-applies our changes afterwards.
GPU_MEM_UTIL="${GPU_MEM_UTIL:-0.90}"
DEFAULTS="$REPO_DIR/config/templates/values/defaults.yaml"
if [ ! -f "$DEFAULTS" ]; then
    note "fix 4 (gpu memory fraction): no defaults.yaml, skipped"
else
    "$PY" - "$DEFAULTS" "$GPU_MEM_UTIL" <<'PYEOF' || fail "fix 4 (gpu memory fraction) failed"
import io, re, sys

path, want = sys.argv[1], sys.argv[2]
src = io.open(path, encoding="utf-8").read()

# Anchored on the anchor itself, not on the number: matching "0.95" would also
# rewrite unrelated fractions, and the point is to follow THIS definition
# wherever upstream moves it.
pat = re.compile(r"^(\s*gpu_memory_util:\s*&gpu_memory_util\s*)([0-9.]+)\s*$", re.M)
m = pat.search(src)
if not m:
    sys.exit("anchor missing (upstream shape changed): gpu_memory_util: &gpu_memory_util")
if m.group(2) == want:
    print("  fix 4 (gpu memory fraction): already %s" % want)
    sys.exit(0)
src = pat.sub(lambda mm: mm.group(1) + want, src, count=1)
io.open(path, "w", encoding="utf-8", newline="\n").write(src)
print("  fix 4 (gpu memory fraction): %s -> %s" % (m.group(2), want))
PYEOF
fi

# Fix 5 -- 18_podmonitor.yaml.j2: stop duplicating the chart's PodMonitor.
#
# Two independent toggles each render a PodMonitor over the SAME decode pods:
#
#   monitoring.podmonitor.enabled       -> 18_podmonitor.yaml.j2, an UNMANAGED
#                                          PodMonitor named vllm-<model>
#   <deployment>.monitoring.podmonitor  -> the modelservice Helm chart's
#                                          <model>-decode-podmonitor
#
# One pod, two scrape jobs, so every vllm:* series exists twice distinguished
# only by `job`. Measured on pokprod: vllm:num_requests_running returned two
# series both peaking at 121 for a single replica, and the benchmark dashboard --
# which plots the raw series -- drew that as two replicas. Additive queries over
# vllm:* double-count for the same reason.
#
# Patched in the TEMPLATE, after two attempts that could not work:
#
#   1. setting monitoring.podmonitor.enabled=false in the scenario
#   2. setting the same key in config/templates/values/defaults.yaml
#
# Both are overridden at render time. `make benchmark-standup` passes
# --monitoring (BENCHMARK_MONITORING defaults to true), and the flag FORCES the
# value regardless of scenario or defaults -- render_plans.py:
#
#   if self.cli_monitoring:
#       podmonitor_config["enabled"] = True
#
# Dropping --monitoring is not the fix either: --no-monitoring also switches off
# router.monitoring.prometheus.enabled, and that ServiceMonitor is how the EPP is
# scraped -- which the throughput analyzer's arrival rate depends on.
#
# So the condition itself has to know about the other monitor: render only when
# the modelservice chart is NOT already covering these pods, or when FMA is on.
# FMA is the one path that genuinely needs this template -- launcher pods belong
# to no modelservice release, so nothing else would scrape them.
TPL="$REPO_DIR/config/templates/jinja/18_podmonitor.yaml.j2"
if [ ! -f "$TPL" ]; then
    note "fix 5 (duplicate podmonitor): no 18_podmonitor.yaml.j2, skipped"
else
    "$PY" - "$TPL" <<'PYEOF' || fail "fix 5 (duplicate podmonitor) failed"
import io, sys

path = sys.argv[1]
src = io.open(path, encoding="utf-8").read()

GUARD = "_ms_podmonitor"
if GUARD in src:
    print("  fix 5 (duplicate podmonitor): already applied")
    sys.exit(0)

old = ("{% if monitoring is defined and monitoring.podmonitor is defined "
       "and monitoring.podmonitor.enabled | default(false) %}")
if old not in src:
    sys.exit("anchor missing (upstream shape changed): 18_podmonitor.yaml.j2 outer if")

new = (
    "{% set _ms_podmonitor = (decode is defined and decode.monitoring is defined"
    " and decode.monitoring.podmonitor is defined"
    " and decode.monitoring.podmonitor.enabled | default(false)) %}\n"
    "{% set _fma_on = (fma is defined and fma.enabled | default(false)) %}\n"
    "{% if monitoring is defined and monitoring.podmonitor is defined"
    " and monitoring.podmonitor.enabled | default(false)"
    " and (not _ms_podmonitor or _fma_on) %}"
)
io.open(path, "w", encoding="utf-8", newline="\n").write(src.replace(old, new, 1))
print("  fix 5 (duplicate podmonitor): template guarded on the chart's monitor")
PYEOF
fi

# ---------------------------------------------------------------------------
# Fix 6 -- defaults.yaml: pin the modelservice chart, which floats.
#
# The chart version is resolved at PLAN time while the sidecar image beside it
# is pinned, so the two halves of a matched pair drift apart on their own:
#
#   config/templates/values/defaults.yaml:117  llm-d-routing-sidecar_version: v0.9.0
#   config/templates/values/defaults.yaml:485  llmDModelservice: auto
#
# llm-d-modelservice v0.4.16 (published 2026-08-21) added, in _helpers.tpl:
#
#   - --model-server-port={{ default 8200 .proxy.targetPort }}
#
# which the pinned v0.9.0 sidecar rejects outright: "unknown flag:
# --model-server-port". The routing-proxy init container CrashLoopBackOffs, the
# decode pod never leaves PodInitializing, and benchmark-standup fails after its
# full 25-minute wait with "Decode pods not ready". Nothing in the clone changed
# and nothing we did changed -- the chart moved underneath a pinned image, which
# is what `auto` promises to do eventually.
#
# Pinned to v0.4.15, the last release compatible with the sidecar tag this
# harness pins. Both halves of a matched pair get pinned or neither does: a
# floating dependency is not a property a BENCHMARK can afford, because it makes
# two runs silently incomparable while both report success.
#
# Delete this block when the sidecar tag is bumped to a release accepting the
# flag, and pin the chart to whatever that release pairs with.
# ---------------------------------------------------------------------------
DEFAULTS="$REPO_DIR/config/templates/values/defaults.yaml"
[ -f "$DEFAULTS" ] || fail "expected file missing: $DEFAULTS"

"$PY" - "$DEFAULTS" <<'PYEOF' || fail "fix 6 (modelservice chart pin) failed"
import io, sys

path = sys.argv[1]
src = io.open(path, encoding="utf-8").read()

PINNED = "  llmDModelservice: v0.4.15"
if PINNED in src:
    print("  fix 6 (modelservice chart pin): already applied")
    sys.exit(0)

ANCHOR = "  llmDModelservice: auto"
if ANCHOR not in src:
    sys.exit("anchor missing (upstream shape changed): %r" % ANCHOR)

# Comment carried into the clone so anyone reading the rendered values sees why
# it is not "auto" like its neighbours.
REPLACEMENT = (
    "  # wva-patch: pinned. v0.4.16 emits --model-server-port, which the\n"
    "  # llm-d-routing-sidecar version pinned above does not accept.\n"
    + PINNED
)

src = src.replace(ANCHOR, REPLACEMENT, 1)
io.open(path, "w", encoding="utf-8", newline="\n").write(src)
print("  fix 6 (modelservice chart pin): applied")
PYEOF

# ---------------------------------------------------------------------------
# Fix 7 -- collect_metrics.sh: use the pod's ServiceAccount, not the operator's
# kubeconfig.
#
# step_07_deploy_harness.py base64-encodes the CLIENT's kubeconfig into
# LLMDBENCH_BASE64_CONTEXT_CONTENTS, and build/llm-d-benchmark.sh decodes it to
# ~/.kube/config inside the harness pod, "so kubectl works inside the pod
# (needed by collect_metrics.sh)". Both outcomes of that are bad:
#
#   inline token   the operator's credential is shipped onto the cluster, in an
#                  env var, on a pod anyone with read access can inspect.
#   tokenFile:     the safe kubeconfig form. The FILE is not copied, so inside
#                  the pod every kubectl call dies with
#                    open /home/<user>/.kube/<cluster>.token: no such file
#
# The second is silent. Every call in collect_metrics.sh ends in
# `2>/dev/null || true`, so an auth failure and an empty cluster are the same
# answer: it recorded 41 snapshots with empty controller lists, and a fleet that
# served the whole run was reported as "Avg replicas 0.00, GPU time 0.0" beside
# latency figures that were real -- guidellm talks to the service directly and
# never needs the cluster at all.
#
# The pod already runs as a ServiceAccount with a projected token. This points
# the collector at it, via a kubeconfig using tokenFile: so the token is never
# an argument (no `ps` exposure) and rotation is picked up for free.
# ---------------------------------------------------------------------------
CM="$REPO_DIR/workload/harnesses/collect_metrics.sh"
[ -f "$CM" ] || fail "expected file missing: $CM"

"$PY" - "$CM" <<'PYEOF' || fail "fix 7 (collect_metrics in-cluster auth) failed"
import io, sys

path = sys.argv[1]
src = io.open(path, encoding="utf-8", newline="").read().replace("\r\n", "\n")

MARK = "# wva-patch: in-cluster credentials"
if MARK in src:
    print("  fix 7 (collect_metrics in-cluster auth): already applied")
    sys.exit(0)

ANCHOR = '_EPP_AUTH_HEADER=""'
if ANCHOR not in src:
    sys.exit("anchor missing (upstream shape changed): %r" % ANCHOR)

BLOCK = '''%s -- prefer the pod's ServiceAccount over the kubeconfig
# the harness copies in. That kubeconfig is the operator's: with a tokenFile:
# reference the file is absent here and every kubectl call fails silently, and
# with an inline token it would put their credential on the cluster.
#
# tokenFile: rather than --token= so the token is never a process argument and
# a rotated projection is picked up without restarting anything.
_WVA_SA=/var/run/secrets/kubernetes.io/serviceaccount
if [ -z "${KUBECTL_CMD:-}" ] && [ -r "$_WVA_SA/token" ] && [ -n "${KUBERNETES_SERVICE_HOST:-}" ]; then
    _WVA_KUBECONFIG=/tmp/wva-incluster.kubeconfig
    cat > "$_WVA_KUBECONFIG" <<WVAEOF
apiVersion: v1
kind: Config
clusters:
- name: in-cluster
  cluster:
    server: https://${KUBERNETES_SERVICE_HOST}:${KUBERNETES_SERVICE_PORT:-443}
    certificate-authority: $_WVA_SA/ca.crt
users:
- name: sa
  user:
    tokenFile: $_WVA_SA/token
contexts:
- name: in-cluster
  context:
    cluster: in-cluster
    user: sa
current-context: in-cluster
WVAEOF
    KUBECTL_CMD="kubectl --kubeconfig=$_WVA_KUBECONFIG"
    export KUBECTL_CMD
fi

''' % MARK

src = src.replace(ANCHOR, BLOCK + ANCHOR, 1)
io.open(path, "w", encoding="utf-8", newline="\n").write(src)
print("  fix 7 (collect_metrics in-cluster auth): applied")
PYEOF

# ---------------------------------------------------------------------------
# Fix 8 -- the harness Role: let the collector read what it already asks for.
#
# collect_metrics.sh reads replica counts with
#
#   $kubectl_cmd --namespace "$namespace" get deployments,statefulsets -o json
#
# and the Role bound to the harness ServiceAccount grants pods, pods/log,
# services, jobs, serviceaccounts and one Secret -- not deployments, not
# statefulsets. That call therefore returns Forbidden, and the line ends in
# `|| all_json='{"items":[]}'`, so a denial and a namespace with no workloads
# produce the same empty answer.
#
# Invisible without fix 7: while the injected kubeconfig was failing, every call
# failed for that reason first. Once the ServiceAccount is used, this is the
# next thing in the way, and it fails the same silent way.
#
# Read-only, namespaced, and only the two kinds the collector names.
# ---------------------------------------------------------------------------
RBAC="$REPO_DIR/config/templates/jinja/05_namespace_sa_rbac_secret.yaml.j2"
[ -f "$RBAC" ] || fail "expected file missing: $RBAC"

"$PY" - "$RBAC" <<'PYEOF' || fail "fix 8 (harness RBAC for workloads) failed"
import io, sys

path = sys.argv[1]
src = io.open(path, encoding="utf-8", newline="").read().replace("\r\n", "\n")

MARK = "# wva-patch: replica counts"
if MARK in src:
    print("  fix 8 (harness RBAC for workloads): already applied")
    sys.exit(0)

ANCHOR = '''- apiGroups: [""]
  resources: ["pods/log"]
  verbs: ["get"]
'''
if ANCHOR not in src:
    sys.exit("anchor missing (upstream shape changed): %r" % ANCHOR)

BLOCK = ANCHOR + '''%s: collect_metrics.sh reads replica counts with
# `get deployments,statefulsets -o json`. Without this the call is Forbidden and
# the script falls back to {"items":[]}, so the run reports a fleet of zero.
- apiGroups: ["apps"]
  resources: ["deployments", "statefulsets"]
  verbs: ["get", "list", "watch"]
''' % MARK

src = src.replace(ANCHOR, BLOCK, 1)
io.open(path, "w", encoding="utf-8", newline="\n").write(src)
print("  fix 8 (harness RBAC for workloads): applied")
PYEOF

# ---------------------------------------------------------------------------
# Fix 10 -- defaults.yaml: find a started engine within seconds, not 30.
#
# The default startupProbe is initialDelaySeconds 30 / periodSeconds 30 /
# failureThreshold 60. A probe that fires every 30 s reports an engine Ready
# up to 30 s after it is: the decode pod above logged "Application startup
# complete" at 21:29:10 and was marked Ready at 21:29:39. On the autoscaler's
# first ramp those 29 s are 29 s more queue charged to the fleet.
#
# 1 / 5 / 360 keeps the same 30-minute budget (was 30 + 60 x 30 = 1830 s, now
# 1 + 360 x 5 = 1801 s) and finds the engine within 5 s of its first 200 -- 1
# rather than 0 because the API server drops a zero from the rendered probe
# and the harness's own config validator then reads None where it expects
# the scenario's value, failing the standup's smoketest. The
# first probes fail on connection refused while the API server binds; that is
# what failureThreshold is for. Scenarios that spell out their own probes
# (two-variant-wva, workload-autoscaling) carry the same numbers.
# ---------------------------------------------------------------------------
DEFAULTS="$REPO_DIR/config/templates/values/defaults.yaml"
if [ ! -f "$DEFAULTS" ]; then
    note "fix 10 (startup probe period): no defaults.yaml, skipped"
else
    "$PY" - "$DEFAULTS" <<'PYEOF' || fail "fix 10 (startup probe period) failed"
import io, sys

path = sys.argv[1]
src = io.open(path, encoding="utf-8", newline="").read().replace("\r\n", "\n")

OLD = '''  probe_startup: &probe_startup
    path: /health
    failureThreshold: 60
    initialDelaySeconds: 30
    periodSeconds: 30
    timeoutSeconds: 5
'''
NEW = '''  probe_startup: &probe_startup
    path: /health
    # wva-patch: 1/5/360 finds a started engine within 5 s; same 30-min budget.
    failureThreshold: 360
    initialDelaySeconds: 1
    periodSeconds: 5
    timeoutSeconds: 5
'''
if NEW in src:
    print("  fix 10 (startup probe period): already applied")
    sys.exit(0)
# An earlier form of this fix used initialDelaySeconds 0; move it on.
EARLIER = NEW.replace("1/5/360", "0/5/360").replace("initialDelaySeconds: 1", "initialDelaySeconds: 0")
if src.count(EARLIER) == 1:
    src = src.replace(EARLIER, NEW, 1)
    io.open(path, "w", encoding="utf-8", newline="\n").write(src)
    print("  fix 10 (startup probe period): initialDelaySeconds 0 -> 1")
    sys.exit(0)
if src.count(OLD) != 1:
    sys.exit("anchor missing or ambiguous (upstream shape changed): probe_startup: &probe_startup")
src = src.replace(OLD, NEW, 1)
io.open(path, "w", encoding="utf-8", newline="\n").write(src)
print("  fix 10 (startup probe period): applied")
PYEOF
fi

# ---------------------------------------------------------------------------
# Fix 11 -- 03_download_daemonset.yaml.j2: "ready" means the download is done,
# and the downloader can be placed by affinity.
#
# storage.hostPath (node-local weights, docs/reference/workload-preparation.md)
# has the standup wait for the download DaemonSet with wait_for_daemonset,
# which succeeds on numberReady == desired. The downloader has no readiness
# probe, so a container is Ready the moment it starts -- the wait returned
# while every node was still downloading, and the engines then started on a
# half-written model directory. A readinessProbe on the completion marker
# makes Ready mean what the wait assumes. And the template takes only a
# nodeSelector, which cannot express "any of these product labels"; an
# optional storage.hostPath.affinity renders as the pod's affinity, which is
# how hack/benchmark/model_hostpath.sh places it on every accelerator node.
# Not yet reported upstream.
#
# And the downloader's `chcon -R -t container_file_t` -- the SELinux relabel
# for OpenShift readers -- fails on a node without SELinux ("can't apply
# partial context to unlabeled file"), and under the script's `set -e` that
# ends it before the marker is written: measured on a Kubernetes cluster (Ubuntu nodes), all 16
# downloaders crash-looped, each restart downloading the model again and
# never marking it. The relabel is skipped on exactly that text now, and
# fails as before on any other error (a wrong SCC on OpenShift must not
# leave a marked directory the engines cannot read); it also sets the level
# to s0, since the spc_t downloader writes at whatever level the runtime
# gave it and the engines read at the namespace's.
# ---------------------------------------------------------------------------
DS_TPL="$REPO_DIR/config/templates/jinja/03_download_daemonset.yaml.j2"
if [ ! -f "$DS_TPL" ]; then
    note "fix 11 (download DaemonSet readiness + affinity): no template, skipped"
else
    "$PY" - "$DS_TPL" <<'PYEOF' || fail "fix 11 (download DaemonSet readiness + affinity) failed"
import io, sys

path = sys.argv[1]
src = io.open(path, encoding="utf-8", newline="").read().replace("\r\n", "\n")

PROBE_OLD = '''          image: {{ images.benchmark.repository }}:{{ images.benchmark.tag }}
          command: ["/bin/sh", "-c"]
'''
PROBE_NEW = '''          image: {{ images.benchmark.repository }}:{{ images.benchmark.tag }}
          # wva-patch: Ready is "the download finished on this node", which is
          # what the standup's wait for this DaemonSet assumes.
          readinessProbe:
            exec:
              command: ["test", "-f", "{{ storage.hostPath.path }}/{{ model.path }}/.download-complete"]
            periodSeconds: 10
          command: ["/bin/sh", "-c"]
'''
AFF_OLD = '''{% if storage.hostPath.tolerations is defined and storage.hostPath.tolerations %}
      tolerations:
{{ storage.hostPath.tolerations | toyaml | indent(8, true) }}
{% endif %}
'''
AFF_NEW = '''{% if storage.hostPath.tolerations is defined and storage.hostPath.tolerations %}
      tolerations:
{{ storage.hostPath.tolerations | toyaml | indent(8, true) }}
{% endif %}
{# wva-patch: an affinity, for a placement a nodeSelector cannot express #}
{% if storage.hostPath.affinity is defined and storage.hostPath.affinity %}
      affinity:
{{ storage.hostPath.affinity | toyaml | indent(8, true) }}
{% endif %}
'''
CHCON_OLD = '''              chcon -R -t container_file_t "{{ storage.hostPath.path }}"
'''
CHCON_NEW = '''              # wva-patch: skipped only where there is no SELinux (the text
              # chcon prints for an unlabelled filesystem); any other failure
              # is real and the marker must not be written. Level s0: readable
              # at every namespace level, since spc_t wrote at a random one.
              if ! chcon -R -t container_file_t -l s0 "{{ storage.hostPath.path }}" 2>/tmp/chcon.err; then
                if grep -q "unlabeled file\\|Operation not supported" /tmp/chcon.err; then
                  echo "Relabelling skipped: no SELinux on this node (wva-patch)."
                else
                  cat /tmp/chcon.err; exit 1
                fi
              fi
'''
if PROBE_NEW in src and AFF_NEW in src and CHCON_NEW in src:
    print("  fix 11 (download DaemonSet readiness + affinity): already applied")
    sys.exit(0)
# earlier forms of this fix: the chcon still fatal, or swallowed on any error
CHCON_PREV = '''              chcon -R -t container_file_t "{{ storage.hostPath.path }}" 2>/dev/null || echo "Relabelling skipped: no SELinux on this node (wva-patch)."
'''
for earlier, what in ((CHCON_OLD, "chcon made best-effort"), (CHCON_PREV, "chcon skip gated on the no-SELinux text, level s0")):
    if PROBE_NEW in src and AFF_NEW in src and src.count(earlier) == 1:
        src = src.replace(earlier, CHCON_NEW, 1)
        io.open(path, "w", encoding="utf-8", newline="\n").write(src)
        print("  fix 11 (download DaemonSet readiness + affinity): " + what)
        sys.exit(0)
for old in (PROBE_OLD, AFF_OLD, CHCON_OLD):
    if src.count(old) != 1:
        sys.exit("anchor missing or ambiguous (upstream shape changed): " + old.splitlines()[0].strip())
src = src.replace(PROBE_OLD, PROBE_NEW, 1).replace(AFF_OLD, AFF_NEW, 1).replace(CHCON_OLD, CHCON_NEW, 1)
io.open(path, "w", encoding="utf-8", newline="\n").write(src)
print("  fix 11 (download DaemonSet readiness + affinity): applied")
PYEOF
fi

# ---------------------------------------------------------------------------
# Fix 12 -- the harness's hostPath PersistentVolume is nobody's, and the
# teardown deletes every one of them in the cluster.
#
# 02a_pv_model-hostpath.yaml.j2 renders the volume with a fixed cluster-wide
# name (model-pvc-hostpath-pv) and no claimRef. Its storage class is a name
# no StorageClass object carries, so binding is by class match alone: a
# claim in ANY namespace asking for that class takes it -- a namespace's
# pods then mount the node directory read-write from a plain pod, on every
# node -- and a second benchmark namespace standing up with the mode gets
# AlreadyExists on the volume and a claim that stays Pending forever.
# The name carries the namespace now and the volume has a claimRef into
# it; 02_pvc_model-pvc.yaml.j2's volumeName follows. And the teardown's
# `kubectl delete pv -l usage=model-cache` -- cluster-wide, run by every
# `make benchmark-teardown` of any namespace -- is scoped to the volume the
# namespace being torn down owns, through a label the volume carries.
# Not yet reported upstream.
# ---------------------------------------------------------------------------
PV_TPL="$REPO_DIR/config/templates/jinja/02a_pv_model-hostpath.yaml.j2"
PVC_TPL="$REPO_DIR/config/templates/jinja/02_pvc_model-pvc.yaml.j2"
TEARDOWN="$REPO_DIR/llmdbenchmark/teardown/steps/step_03_delete_resources.py"
if [ ! -f "$PV_TPL" ] || [ ! -f "$PVC_TPL" ] || [ ! -f "$TEARDOWN" ]; then
    note "fix 12 (hostPath PV owned by its namespace): a file is missing, skipped"
else
    "$PY" - "$PV_TPL" "$PVC_TPL" "$TEARDOWN" <<'PYEOF' || fail "fix 12 (hostPath PV owned by its namespace) failed"
import io, sys

def load(p):
    return io.open(p, encoding="utf-8", newline="").read().replace("\r\n", "\n")

def save(p, s):
    io.open(p, "w", encoding="utf-8", newline="\n").write(s)

pv, pvc, td = sys.argv[1:4]
edits = [
    (pv, '''  name: {{ storage.modelPvc.name }}-hostpath-pv
  labels:
    app: {{ labels.app }}
    usage: model-cache
spec:
''', '''  name: {{ storage.modelPvc.name }}-{{ namespace.name }}-hostpath-pv
  labels:
    app: {{ labels.app }}
    usage: model-cache
    # wva-patch: the teardown deletes this namespace's volume, not every one
    wva.llmd.ai/model-namespace: {{ namespace.name }}
spec:
  # wva-patch: bound to this claim and no other; without a claimRef any
  # claim in any namespace asking for the class takes it
  claimRef:
    namespace: {{ namespace.name }}
    name: {{ storage.modelPvc.name }}
'''),
    (pvc, '''  volumeName: {{ storage.modelPvc.name }}-hostpath-pv
''', '''  volumeName: {{ storage.modelPvc.name }}-{{ namespace.name }}-hostpath-pv
'''),
]
done = 0
for path, old, new in edits:
    src = load(path)
    if new in src:
        done += 1
        continue
    if src.count(old) != 1:
        sys.exit("anchor missing or ambiguous (upstream shape changed): " + old.splitlines()[0].strip())
    save(path, src.replace(old, new, 1))
src = load(td)
OLD = '''            "usage=model-cache",
'''
NEW = '''            f"usage=model-cache,wva.llmd.ai/model-namespace={context.require_namespace()}",  # wva-patch
'''
if NEW in src:
    done += 1
else:
    if src.count(OLD) != 2:
        sys.exit("anchor missing or ambiguous (upstream shape changed): teardown pv delete, expected 2 sites, found %d" % src.count(OLD))
    save(td, src.replace(OLD, NEW))
if done == 3:
    print("  fix 12 (hostPath PV owned by its namespace): already applied")
else:
    print("  fix 12 (hostPath PV owned by its namespace): applied")
PYEOF
fi

# ---------------------------------------------------------------------------
# Fix 13 -- the harness scrapes the engines every 30 s by default; make it 10.
#
# PR #64 moved every PodMonitor this repo ships to 10 s (deploy/warmpool.sh,
# config/modelserver-metrics/, the two-variant-wva and workload-autoscaling
# scenarios via the chart's podmonitor, two-model-warm-pool via
# monitoring.scrapeInterval) and said why: the scale-up signal is a queue at
# the engine, it moves only when Prometheus holds a sample that shows it, and
# the interval is 0..30 s of jitter on every decision. The P/D scenario was
# left out because it sets neither key and so takes the harness's OWN default
# -- 30 s, in three places:
#
#   config/templates/values/defaults.yaml:522   monitoring.scrapeInterval: "30s"
#   config/templates/values/defaults.yaml       decode/prefill.monitoring.podmonitor.interval: "30s"
#   config/templates/jinja/17_standalone-podmonitor.yaml.j2
#   config/templates/jinja/18_podmonitor.yaml.j2   {{ monitoring.scrapeInterval | default('30s') }}
#
# Measured on the shape-swap P/D trace, two cold passes on the same code: the
# KV crossing that orders the second decode replica was seen at +53 s on one
# and +70 s on the other, and the replica served from +143 s against +175 s;
# the first-window p95 TTFT was 0.79 s against 3.25 s. The 15 s optimisation
# cycle read the same 30 s row twice, and every saturated completion rate the
# analyzer logged was an integer over 30. A scenario may still set either key
# and win; the defaults just stop being the slow ones.
# ---------------------------------------------------------------------------
DEFAULTS="$REPO_DIR/config/templates/values/defaults.yaml"
TPL17="$REPO_DIR/config/templates/jinja/17_standalone-podmonitor.yaml.j2"
TPL18="$REPO_DIR/config/templates/jinja/18_podmonitor.yaml.j2"
if [ ! -f "$DEFAULTS" ] || [ ! -f "$TPL18" ]; then
    note "fix 13 (scrape interval 10s): no defaults.yaml or 18_podmonitor.yaml.j2, skipped"
else
    "$PY" - "$DEFAULTS" "$TPL17" "$TPL18" <<'PYEOF' || fail "fix 13 (scrape interval 10s) failed"
import io, os, sys

defaults, tpl17, tpl18 = sys.argv[1:4]


def load(p):
    return io.open(p, encoding="utf-8", newline="").read().replace("\r\n", "\n")


def save(p, s):
    io.open(p, "w", encoding="utf-8", newline="\n").write(s)


# (path, old, new, how many sites the anchor must have)
edits = [
    (defaults, '''  metricsPath: /metrics
  scrapeInterval: "30s"
''', '''  metricsPath: /metrics
  scrapeInterval: "10s"  # wva-patch: 30s is 0..30s of jitter on every scaling decision
''', 1),
    (defaults, '''    podmonitor:
      enabled: false
      portName: "metrics"
      path: "/metrics"
      interval: "30s"
''', '''    podmonitor:
      enabled: false
      portName: "metrics"
      path: "/metrics"
      interval: "10s"  # wva-patch
''', 2),
    (tpl18, "{{ monitoring.scrapeInterval | default('30s') }}", "{{ monitoring.scrapeInterval | default('10s') }}", 2),
]
if os.path.isfile(tpl17):
    edits.append((tpl17, "{{ monitoring.scrapeInterval | default('30s') }}", "{{ monitoring.scrapeInterval | default('10s') }}", 1))

done = 0
for path, old, new, sites in edits:
    src = load(path)
    if src.count(new) == sites and old not in src:
        done += 1
        continue
    if src.count(old) != sites:
        sys.exit("anchor missing or ambiguous (upstream shape changed): %s, expected %d sites, found %d in %s"
                 % (old.strip().splitlines()[-1].strip(), sites, src.count(old), os.path.basename(path)))
    save(path, src.replace(old, new))
if done == len(edits):
    print("  fix 13 (scrape interval 10s): already applied")
else:
    print("  fix 13 (scrape interval 10s): applied")
PYEOF
fi
