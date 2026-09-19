#!/usr/bin/env bash
# Every scrape this repo sets up runs at 10 s -- and the harness's own default
# is patched to say so.
#
# PR #64 moved the pool PodMonitor, the shipped model-server PodMonitor and two
# benchmark scenarios to 10 s and measured why (decision lag 25/56/56/87 s at
# 30 s, 15/60/60/76 s at 10 s). The P/D scenario was missed: it sets no
# interval and took llm-d-benchmark's own default, 30 s, and two cold passes on
# the same code then saw the same KV crossing 17 s apart. `bash -n` cannot see
# an interval; this does. It needs no cluster.
#
#   1. every `interval:` / `scrapeInterval:` in a monitor this repo ships or a
#      scenario it renders says 10s;
#   2. patch_harness.sh fix 13, run on a fixture carrying the upstream anchors,
#      turns the harness's three 30 s defaults into 10 s, and is idempotent.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PY=${PYTHON:-python3}
FAILED=0
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT

ok()   { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1"; FAILED=1; }

# ---------------------------------------------------------------------------
# 1. Shipped monitors and scenarios.
# ---------------------------------------------------------------------------
files="$(grep -rlE 'kind: (PodMonitor|ServiceMonitor)|scrapeInterval:|podmonitor:' \
    "$ROOT/config" "$ROOT/deploy" "$ROOT/hack/benchmark/scenarios" \
    --include='*.yaml' --include='*.yaml.in' --include='*.sh' 2>/dev/null | sort)"
[ -n "$files" ] || fail "found no monitor manifests or scenarios to check"
slow="$(printf '%s\n' "$files" | xargs grep -nE '^[[:space:]]*-?[[:space:]]*(interval|scrapeInterval):[[:space:]]*"?[0-9]+[smh]"?' \
    | grep -vE ':[[:space:]]*"?10s"?([[:space:]]|$|#)')"
if [ -z "$slow" ]; then
    n="$(printf '%s\n' "$files" | xargs grep -HcE '^[[:space:]]*-?[[:space:]]*(interval|scrapeInterval):[[:space:]]*"?10s' | awk -F: '{s+=$NF} END {print s+0}')"
    [ "$n" -gt 0 ] && ok "every shipped scrape interval is 10s ($n sites in $(printf '%s\n' "$files" | wc -l | tr -d ' ') files)" \
                   || fail "no 10s scrape interval found at all -- the grep is broken"
else
    fail "scrape intervals other than 10s:"
    printf '%s\n' "$slow" | sed "s|$ROOT/||; s/^/         /"
fi

# ---------------------------------------------------------------------------
# 2. Fix 13 on a fixture with the upstream anchors (llm-d-benchmark main,
#    defaults.yaml:522 and the decode/prefill podmonitor blocks; templates 17
#    and 18).
# ---------------------------------------------------------------------------
sed -n '/fix 13 (scrape interval 10s) failed"/,/^PYEOF$/p' "$ROOT/hack/benchmark/patch_harness.sh" | sed '1d;$d' > "$T/fix13.py"
[ -s "$T/fix13.py" ] || fail "could not extract fix 13 from patch_harness.sh"

block='    podmonitor:
      enabled: false
      portName: "metrics"
      path: "/metrics"
      interval: "30s"
      labels: {}
'
printf 'monitoring:\n  metricsPath: /metrics\n  scrapeInterval: "30s"\n  metricsScrapeEnabled: false\ndecode:\n  monitoring:\n%s      relabelings: []\nprefill:\n  monitoring:\n%s' "$block" "$block" > "$T/defaults.yaml"
printf 'spec:\n  podMetricsEndpoints:\n  - interval: {{ monitoring.scrapeInterval | default('"'"'30s'"'"') }}\n    path: /metrics\n  x:\n    interval: {{ monitoring.scrapeInterval | default('"'"'30s'"'"') }}\n' > "$T/18.j2"
printf 'spec:\n  endpoints:\n  - interval: {{ monitoring.scrapeInterval | default('"'"'30s'"'"') }}\n' > "$T/17.j2"

out="$("$PY" "$T/fix13.py" "$T/defaults.yaml" "$T/17.j2" "$T/18.j2" 2>&1)"
case "$out" in *"applied"*) ok "fix 13: applies to the upstream anchors" ;; *) fail "fix 13 on the fixture: $out" ;; esac
[ "$(grep -c '"10s"' "$T/defaults.yaml")" = 3 ] && ! grep -q '"30s"' "$T/defaults.yaml" \
    && ok "fix 13: defaults.yaml -- scrapeInterval and both podmonitor blocks at 10s, no 30s left" \
    || fail "fix 13: defaults.yaml still carries a 30s or is missing a 10s: $(grep -n '0s"' "$T/defaults.yaml" | tr '\n' ' ')"
[ "$(grep -c "default('10s')" "$T/18.j2")" = 2 ] && [ "$(grep -c "default('10s')" "$T/17.j2")" = 1 ] && ! grep -q "30s" "$T/17.j2" "$T/18.j2" \
    && ok "fix 13: templates 17 and 18 default to 10s" || fail "fix 13: template defaults: $(grep -n 'default' "$T/17.j2" "$T/18.j2" | tr '\n' ' ')"
out="$("$PY" "$T/fix13.py" "$T/defaults.yaml" "$T/17.j2" "$T/18.j2" 2>&1)"
case "$out" in *"already applied"*) ok "fix 13: idempotent" ;; *) fail "fix 13 second run: $out" ;; esac
# Without template 17 (older harness) the fix still applies the rest.
cp "$T/defaults.yaml" "$T/d2.yaml"; sed -i 's/"10s"  # wva-patch.*/"30s"/' "$T/d2.yaml"
printf 'spec:\n  podMetricsEndpoints:\n  - interval: {{ monitoring.scrapeInterval | default('"'"'30s'"'"') }}\n  x:\n    interval: {{ monitoring.scrapeInterval | default('"'"'30s'"'"') }}\n' > "$T/18b.j2"
out="$("$PY" "$T/fix13.py" "$T/d2.yaml" "$T/absent.j2" "$T/18b.j2" 2>&1)"
case "$out" in *": applied"*) ok "fix 13: applies without template 17" ;; *) fail "fix 13 without template 17: $out" ;; esac
# A moved anchor is refused, not half-applied.
printf 'monitoring:\n  scrapeInterval: "30s"\n' > "$T/d3.yaml"
out="$("$PY" "$T/fix13.py" "$T/d3.yaml" "$T/absent.j2" "$T/18b.j2" 2>&1)"; rc=$?
[ "$rc" -ne 0 ] && case "$out" in *"anchor missing"*) ok "fix 13: refuses a changed upstream shape" ;; *) fail "fix 13 on a changed shape: $out" ;; esac \
    || fail "fix 13 on a changed shape exited 0"

exit $FAILED
