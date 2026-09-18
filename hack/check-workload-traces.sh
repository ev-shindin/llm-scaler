#!/usr/bin/env bash
# Executes hack/benchmark/workload_traces.sh against fixtures and asserts what
# it does, and checks that every replay profile this repo ships names a trace
# that is committed beside it.
#
# The defect this exists for parses fine and runs for five minutes: a replay
# profile copied to the harness without its trace produced a harness pod that
# died on "Trace file not found" and a report of a run that never sent a
# request.
set -euo pipefail
cd "$(dirname "$0")/.."

T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
FAILED=0
ok()   { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1"; FAILED=1; }

# 1. every replay profile shipped here names a trace that is committed beside it
missing=0
for p in test/benchmark/scenarios/*.yaml.in; do
    for name in $(sed -n 's#^[[:space:]]*path:[[:space:]]*/workspace/profiles/guidellm/\([A-Za-z0-9._-]*\)[[:space:]]*$#\1#p' "$p"); do
        if [ ! -f "test/benchmark/scenarios/$name" ]; then fail "$(basename "$p") replays $name, which is not committed in test/benchmark/scenarios"; missing=1; fi
    done
done
[ "$missing" -eq 0 ] && ok "every replay profile in test/benchmark/scenarios names a trace committed beside it"
n="$(ls test/benchmark/scenarios/*.jsonl 2>/dev/null | wc -l)"
[ "$n" -ge 3 ] && ok "$n replay traces are committed (a count of 0 would make the check above vacuous)" || fail "expected at least 3 committed traces, found $n"

# 2. the copy, offline
mkdir -p "$T/scen" "$T/dest"
printf 'spec:\n  data:\n    - kind: trace_synthetic\n      path: /workspace/profiles/guidellm/one.jsonl\n' > "$T/scen/replay.yaml.in"
printf 'spec:\n  data:\n    - kind: synthetic\n' > "$T/scen/plain.yaml.in"
head -c 300000 /dev/zero | tr '\0' 'x' > "$T/scen/one.jsonl"
head -c 300000 /dev/zero | tr '\0' 'y' > "$T/scen/two.jsonl"
cp "$T/scen/two.jsonl" "$T/dest/two.jsonl"          # an earlier run's
printf 'x' > "$T/dest/theirs.jsonl"                   # not ours: not in the scenarios dir
printf 'template\n' > "$T/dest/replay.yaml.in"
if out="$(bash hack/benchmark/workload_traces.sh "$T/scen/replay.yaml.in" "$T/scen" "$T/dest" guidellm 2>&1)"; then
    [ -f "$T/dest/one.jsonl" ] && cmp -s "$T/scen/one.jsonl" "$T/dest/one.jsonl" && ok "the referenced trace is copied to the harness" || fail "trace not copied: $out"
    [ ! -f "$T/dest/two.jsonl" ] && ok "a trace of ours the profile does not name is removed (the ConfigMap holds 1 MiB in all)" || fail "stale trace kept"
    [ -f "$T/dest/theirs.jsonl" ] && ok "a file that is not ours is left alone" || fail "a foreign file was removed"
    case "$out" in *"Copying replay trace one.jsonl (300000 bytes)"*) ok "the copy says what it copied and how big" ;; *) fail "copy message: $out" ;; esac
else
    fail "workload_traces failed on the good case: $out"
fi
if out="$(bash hack/benchmark/workload_traces.sh "$T/scen/plain.yaml.in" "$T/scen" "$T/dest" guidellm 2>&1)"; then
    [ ! -f "$T/dest/one.jsonl" ] && ok "a profile that replays nothing leaves no trace of ours behind" || fail "one.jsonl kept for a non-replay profile"
else
    fail "workload_traces failed on a profile with no trace: $out"
fi
# the trace the profile names is not committed: refused, with the generator command
printf 'spec:\n  data:\n    - kind: trace_synthetic\n      path: /workspace/profiles/guidellm/absent.jsonl\n' > "$T/scen/absent.yaml.in"
printf 'phases: []\n' > "$T/scen/absent.params.yaml"
if out="$(bash hack/benchmark/workload_traces.sh "$T/scen/absent.yaml.in" "$T/scen" "$T/dest" guidellm 2>&1)"; then fail "a missing trace must be refused"; else
    case "$out" in *"gen_shape_trace.py $T/scen/absent.params.yaml --out $T/scen/absent.jsonl"*) ok "a missing trace is refused with the command that generates it from its params file" ;; *) fail "missing-trace message: $out" ;; esac; fi
rm -f "$T/scen/absent.params.yaml"
if out="$(bash hack/benchmark/workload_traces.sh "$T/scen/absent.yaml.in" "$T/scen" "$T/dest" guidellm 2>&1)"; then fail "a missing trace must be refused"; else
    case "$out" in *"no absent.params.yaml beside it"*) ok "a missing trace with no params file says so" ;; *) fail "missing-trace-no-params message: $out" ;; esac; fi
# over the ConfigMap's cap: refused, files listed
head -c 900000 /dev/zero | tr '\0' 'z' > "$T/dest/big.dat"
if out="$(bash hack/benchmark/workload_traces.sh "$T/scen/replay.yaml.in" "$T/scen" "$T/dest" guidellm 2>&1)"; then fail "a bundle over 1 MiB must be refused"; else
    case "$out" in *"over its 1048576-byte cap"*"big.dat"*) ok "a bundle over the ConfigMap's cap is refused, naming the files" ;; *) fail "cap message: $out" ;; esac; fi
rm -f "$T/dest/big.dat"
# the harness name scopes the mount path
printf 'spec:\n  data:\n    - kind: trace_synthetic\n      path: /workspace/profiles/inference-perf/one.jsonl\n' > "$T/scen/other.yaml.in"
rm -f "$T/dest/one.jsonl"
bash hack/benchmark/workload_traces.sh "$T/scen/other.yaml.in" "$T/scen" "$T/dest" guidellm >/dev/null 2>&1 || true
[ ! -f "$T/dest/one.jsonl" ] && ok "a path under another harness's mount is not this harness's trace" || fail "copied a trace referenced under another harness's mount"

# 3. benchmark-run calls it right after the profile copy, with the same directories
line="$(tr -d '\r' < Makefile | grep -n 'bash hack/benchmark/workload_traces.sh' | head -1 || true)"
case "$line" in
    *'bash hack/benchmark/workload_traces.sh "$(BENCHMARK_REPO_DIR)/workload/profiles/$(BENCHMARK_HARNESS)/$(BENCHMARK_WORKLOAD).yaml.in" "$(BENCHMARK_SCENARIOS_DIR)" "$(BENCHMARK_REPO_DIR)/workload/profiles/$(BENCHMARK_HARNESS)" "$(BENCHMARK_HARNESS)"'*) ok "benchmark-run copies the traces from the scenarios directory it copied the profile from" ;;
    *) fail "benchmark-run's call: ${line:-missing}" ;;
esac
copy_line="$(tr -d '\r' < Makefile | grep -n 'Copying local workload $(BENCHMARK_WORKLOAD).yaml.in' | head -1 | cut -d: -f1)"
call_line="${line%%:*}"
[ -n "$copy_line" ] && [ -n "$call_line" ] && [ "$call_line" -gt "$copy_line" ] && [ $((call_line - copy_line)) -lt 8 ] && ok "the trace copy follows the profile copy in the same branch" || fail "trace copy at line ${call_line:-?}, profile copy at ${copy_line:-?}"

if [ "$FAILED" -ne 0 ]; then echo "workload trace checks: FAIL"; exit 1; fi
echo "workload trace checks OK"
