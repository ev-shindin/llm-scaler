#!/usr/bin/env bash
# The engine's launch preamble asks the loader cache for libcuda, and only
# walks the root filesystem when the cache has no answer.
#
# llm-d-benchmark's accelerator.runtimePreamble opens every engine command with
# two `find / -name libcuda.so.1` walks across every mount in the pod. Fix 14 in
# patch_harness.sh replaces them with `ldconfig -p` and a fallback walk of /usr
# and /opt on the root filesystem. `bash -n` cannot tell a slow preamble from a
# fast one, and the patched text is shell inside YAML inside Python: this runs
# the fix on a fixture with the upstream anchor, then runs the preamble it
# produced under stubbed `ldconfig`/`find` and reads the two variables it must
# export. It needs no cluster and no GPU.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PY=${PYTHON:-python3}
FAILED=0
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT

ok()   { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1"; FAILED=1; }

# ---------------------------------------------------------------------------
# 1. Fix 14 on a fixture carrying the upstream anchor (defaults.yaml:576).
# ---------------------------------------------------------------------------
sed -n '/fix 14 (libcuda from the loader cache) failed"/,/^PYEOF$/p' "$ROOT/hack/benchmark/patch_harness.sh" | sed '1d;$d' > "$T/fix14.py"
[ -s "$T/fix14.py" ] || fail "could not extract fix 14 from patch_harness.sh"

cat > "$T/defaults.yaml" <<'EOF'
accelerator:
  type: nvidia
  runtimePreamble: |
    export LD_LIBRARY_PATH=$(find / -name libcuda.so.1 -printf '%h\n' 2>/dev/null | sed ':a; N; $!ba; s/\n/:/g'):${LD_LIBRARY_PATH}
    export LIBRARY_PATH=$(find / -name libcuda.so.1 -printf '%h\n' 2>/dev/null | head -1):${LIBRARY_PATH}
  dtypeArgs: "--dtype bfloat16"
EOF
out="$("$PY" "$T/fix14.py" "$T/defaults.yaml" 2>&1)"
case "$out" in *": applied"*) ok "fix 14: applies to the upstream anchor" ;; *) fail "fix 14 on the fixture: $out" ;; esac
grep -q 'find / -name libcuda' "$T/defaults.yaml" && fail "fix 14: a walk of / survived" || ok "fix 14: no walk of / left in the preamble"
out="$("$PY" "$T/fix14.py" "$T/defaults.yaml" 2>&1)"
case "$out" in *"already applied"*) ok "fix 14: idempotent" ;; *) fail "fix 14 second run: $out" ;; esac
printf 'accelerator:\n  runtimePreamble: |\n    export LD_LIBRARY_PATH=/somewhere\n' > "$T/d2.yaml"
out="$("$PY" "$T/fix14.py" "$T/d2.yaml" 2>&1)"; rc=$?
[ "$rc" -ne 0 ] && case "$out" in *"anchor missing"*) ok "fix 14: refuses a changed upstream shape" ;; *) fail "fix 14 on a changed shape: $out" ;; esac \
    || fail "fix 14 on a changed shape exited 0"

# ---------------------------------------------------------------------------
# 2. The preamble it produced, run. The block scalar's lines are the script;
#    `ldconfig` and `find` are stubs on PATH so the check is the same on every
#    machine, and the harness's own `${dotted.path}` substitution (a dot inside
#    the braces) must not see any of the shell expansions the preamble uses.
# ---------------------------------------------------------------------------
"$PY" - "$T/defaults.yaml" "$T/preamble.sh" <<'PYEOF'
import re, sys
src = open(sys.argv[1], encoding="utf-8").read()
m = re.search(r"  runtimePreamble: \|\n((?:    .*\n)+)", src)
body = "".join(line[4:] + "\n" for line in m.group(1).splitlines())
open(sys.argv[2], "w", encoding="utf-8", newline="\n").write(body)
if re.search(r"\$\{([\w]+(?:\.[\w]+)+)\}", body):
    sys.exit("the preamble contains a ${dotted.path} the harness would try to substitute")
PYEOF
[ $? -eq 0 ] && ok "preamble: no \${dotted.path} for the harness's substitution to catch" || fail "preamble: harness substitution would fire on it"
bash -n "$T/preamble.sh" && ok "preamble: parses" || fail "preamble: does not parse"

mkdir -p "$T/bin"
# The stubs record whether they were called.
printf '#!/bin/sh\necho called >> "%s/ldconfig.calls"\ncat "%s/ldconfig.out"\n' "$T" "$T" > "$T/bin/ldconfig"
printf '#!/bin/sh\necho "$*" >> "%s/find.calls"\ncat "%s/find.out" 2>/dev/null\n' "$T" "$T" > "$T/bin/find"
chmod +x "$T/bin/ldconfig" "$T/bin/find"
# The preamble also globs the image's own /usr/local/cuda*/compat; on a check
# host that has one, it lands at the end of the list, so the expectations
# carry it.
compat=""; for d in /usr/local/cuda*/compat; do [ -e "$d/libcuda.so.1" ] && compat="$compat:$d"; done
run() {  # run <ldconfig output file> <find output file>: prints LD_LIBRARY_PATH|LIBRARY_PATH
    cp "$1" "$T/ldconfig.out"; cp "$2" "$T/find.out"; rm -f "$T/ldconfig.calls" "$T/find.calls"
    PATH="$T/bin:$PATH" LD_LIBRARY_PATH=/pre/ld LIBRARY_PATH=/pre/lib bash -c '. "$1"; printf "%s|%s" "$LD_LIBRARY_PATH" "$LIBRARY_PATH"' _ "$T/preamble.sh"
}
# a. the loader cache knows two copies (the driver's and a compat one)
printf '\tlibcuda.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libcuda.so.1\n\tlibcuda.so.1 (libc6,x86-64) => /usr/local/cuda-13.0/compat/libcuda.so.1\n\tlibcudart.so.13 (libc6,x86-64) => /usr/local/cuda/lib64/libcudart.so.13\n' > "$T/lc2"
: > "$T/empty"
got="$(run "$T/lc2" "$T/empty")"
[ "$got" = "/usr/lib/x86_64-linux-gnu:/usr/local/cuda-13.0/compat$compat:/pre/ld|/usr/lib/x86_64-linux-gnu:/pre/lib" ] \
    && ok "preamble: both cache entries on LD_LIBRARY_PATH, the first on LIBRARY_PATH, the old values kept" \
    || fail "preamble with two cache entries: $got"
[ -f "$T/find.calls" ] && fail "preamble: walked the filesystem although the cache answered" || ok "preamble: no walk when the cache answers"
# b. the cache has no libcuda and the image has no compat copy: the fallback
#    walk of /usr and /opt, on the root filesystem
printf '\tlibcudart.so.13 (libc6,x86-64) => /usr/local/cuda/lib64/libcudart.so.13\n' > "$T/lc0"
printf '/opt/cuda/compat\n' > "$T/f1"
if [ -z "$compat" ]; then
    got="$(run "$T/lc0" "$T/f1")"
    [ "$got" = "/opt/cuda/compat:/pre/ld|/opt/cuda/compat:/pre/lib" ] \
        && ok "preamble: falls back to the walk when the cache has no libcuda" || fail "preamble fallback: $got"
    case "$(cat "$T/find.calls" 2>/dev/null)" in
        "/usr /opt -xdev -name libcuda.so.1 "*) ok "preamble: the fallback walks /usr and /opt on the root filesystem only" ;;
        *) fail "preamble fallback walk: $(cat "$T/find.calls" 2>/dev/null)" ;;
    esac
    # c. nothing anywhere: the variables are left as they were, no stray colon
    got="$(run "$T/lc0" "$T/empty")"
    [ "$got" = "/pre/ld|/pre/lib" ] && ok "preamble: with no libcuda anywhere the variables are untouched" || fail "preamble with nothing found: $got"
else
    # this host has a compat copy of its own, which answers before the walk
    got="$(run "$T/lc0" "$T/empty")"
    [ "$got" = "${compat#:}:/pre/ld|${compat#:}:/pre/lib" ] && [ ! -f "$T/find.calls" ] \
        && ok "preamble: the host's own compat copy answers, no walk (fallback cases need a host without one)" \
        || fail "preamble with the host's compat copy: $got"
fi

exit $FAILED
