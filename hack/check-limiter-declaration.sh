#!/usr/bin/env bash
#
# Executes limiter_entry_yaml() in deploy/lib/infra_wva.sh and asserts the
# controller would ACCEPT what it emits.
#
# It exists because WVA_LIMITER=quota had never bounded anything. The installer
# wrote `limiters: [{type: quota}]`, which parses as YAML, applies cleanly, and
# is rejected by the controller on read:
#
#     Invalid saturation scaling config entry ... "limiters: entry[0]: name must
#     not be empty"
#
# A rejected entry costs the WHOLE `default` policy -- thresholds included -- and
# the controller then builds `type: none`, logging "scaling is UNCONSTRAINED".
# The install printed "Scaling is now bounded by the quota limiter" either way.
# `bash -n` sees nothing wrong; neither does a cluster run, which reports a
# healthy controller and a fleet that scales.
#
# The rules asserted below are QuotaLimiterEntries.Validate() and
# validateLimiters() in internal/config. They are mirrored here rather than
# invoked, so keep_in_sync_with() reads the Go source and fails when a NEW
# required field appears -- a mirror nobody notices going stale is how this
# class of bug returns.
#
# kubectl is never called: limiter_entry_yaml is pure.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAIL=0

# Sourced through a CR-stripped copy, for the reason hack/check-workload-gaps.sh
# gives: a Windows checkout writes these LF files as CRLF and bash then stops at
# the first function definition, which looks exactly like every function missing.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh infra_wva.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib"
done
# common.sh logs in colour and dies on an unbound variable under `set -u`.
BLUE=''; GREEN=''; YELLOW=''; RED=''; NC=''
# shellcheck disable=SC1090
. "$WORK/common.sh"
# infra_wva.sh's header names what it needs; only these reach limiter_entry_yaml.
WVA_NS="wva-system"
# shellcheck disable=SC1090
. "$WORK/infra_wva.sh"

if ! declare -F limiter_entry_yaml >/dev/null; then
    echo "FAIL limiter_entry_yaml is not defined -- the library did not source"
    exit 1
fi

fail() { echo "FAIL $*"; FAIL=$((FAIL + 1)); }
# A case ends in one summarising ok, but its assertions are separate statements,
# so a failed one does not stop the ok from printing after it. Suppress it when
# anything failed since this case began -- an `ok` under four FAILs describing the
# same output is how a negative control gets read as half-passing.
ok()   { [ "$FAIL" -eq "${CASE_FAIL_AT:-0}" ] || return 0; echo "ok   $*"; }

# emit <type> -- runs limiter_entry_yaml with the environment already set by the
# caller, into $OUT/$RC/$ERR. Never lets a non-zero status abort the harness: a
# refusal is the expected result for half these cases. Every case starts here, so
# this is also where the per-case failure mark is taken.
emit() {
    CASE_FAIL_AT="$FAIL"
    OUT="$(limiter_entry_yaml "$1" 2>"$WORK/err")"
    RC=$?
    ERR="$(cat "$WORK/err")"
}

# ---------------------------------------------------------------------------
# The shape the controller accepts
# ---------------------------------------------------------------------------

# A namespace-scoped install names the namespace it manages, because the
# reserved `default` key means "this much PER unlisted namespace" -- ten tenants
# would get ten budgets, not one.
WVA_LIMITER=quota WVA_SCOPE=namespace WVA_WATCH_NS=tenant-a WVA_QUOTAS='H200=8 A100=4' \
    emit quota
if [ "$RC" -ne 0 ]; then
    fail "quota/namespace refused a valid budget: $ERR"
else
    got="$(printf '%s\n' "$OUT" | yq -o=json '.' 2>/dev/null)"
    if [ -z "$got" ]; then
        fail "quota/namespace emitted invalid YAML:"$'\n'"$OUT"
    else
        # Validate() rule by rule, on the parsed document.
        [ "$(printf '%s' "$got" | jq -r '.[0].name // ""')" != "" ] \
            || fail "quota/namespace: name is empty -- the controller rejects the entry and drops the whole policy"
        [ "$(printf '%s' "$got" | jq -r '.[0].type')" = "quota" ] \
            || fail "quota/namespace: type is not \"quota\""
        [ "$(printf '%s' "$got" | jq -r '.[0].scope')" = "namespace" ] \
            || fail "quota/namespace: scope is not \"namespace\""
        [ "$(printf '%s' "$got" | jq -r '.[0].quotas // "absent"')" = "absent" ] \
            || fail "quota/namespace: carries a cluster-scope quotas map, which Validate rejects for namespace scope"
        [ "$(printf '%s' "$got" | jq -r '.[0].namespaceQuotas["tenant-a"].H200 // "absent"')" = "8" ] \
            || fail "quota/namespace: H200 budget is not keyed under the managed namespace: $got"
        [ "$(printf '%s' "$got" | jq -r '.[0].namespaceQuotas["tenant-a"].A100 // "absent"')" = "4" ] \
            || fail "quota/namespace: the second WVA_QUOTAS entry was dropped: $got"
        ok "quota/namespace emits a budget the controller accepts, keyed on the managed namespace"
    fi
fi

# A cluster-scoped install has no single namespace to key on, so it falls through
# to the reserved key -- and uses `quotas`, never `namespaceQuotas`, for scope
# cluster.
WVA_LIMITER=quota WVA_SCOPE=cluster WVA_QUOTA_SCOPE=cluster WVA_QUOTAS='H100=16' emit quota
if [ "$RC" -ne 0 ]; then
    fail "quota/cluster refused a valid budget: $ERR"
else
    got="$(printf '%s\n' "$OUT" | yq -o=json '.' 2>/dev/null)"
    [ -n "$got" ] || fail "quota/cluster emitted invalid YAML:"$'\n'"$OUT"
    if [ -n "$got" ]; then
        [ "$(printf '%s' "$got" | jq -r '.[0].scope')" = "cluster" ] \
            || fail "quota/cluster: scope is not \"cluster\""
        [ "$(printf '%s' "$got" | jq -r '.[0].quotas.H100 // "absent"')" = "16" ] \
            || fail "quota/cluster: budget is not under .quotas: $got"
        [ "$(printf '%s' "$got" | jq -r '.[0].namespaceQuotas // "absent"')" = "absent" ] \
            || fail "quota/cluster: carries namespaceQuotas, which Validate rejects for cluster scope"
        ok "quota/cluster emits a cluster-wide budget under .quotas"
    fi
fi

WVA_SCOPE=cluster WVA_QUOTA_SCOPE=namespace WVA_QUOTAS='H200=2' emit quota
if [ "$RC" -ne 0 ]; then
    fail "quota/cluster-install-namespace-scope refused a valid budget: $ERR"
else
    got="$(printf '%s\n' "$OUT" | yq -o=json '.' 2>/dev/null)"
    [ "$(printf '%s' "$got" | jq -r '.[0].namespaceQuotas.default.H200 // "absent"')" = "2" ] \
        || fail "cluster install with namespace scope should fall through to the reserved \`default\` key: $got"
    ok "a cluster install with namespace scope keys on the reserved \`default\`"
fi

# The physical limiter must carry NO quota fields: validateLimiters rejects
# scope/quotas/namespaceQuotas/exclude on a gpu-inventory entry. This shape ships
# today and works; the case pins it so the quota fix cannot bleed into it.
WVA_QUOTAS='H200=8' emit gpu-inventory
if [ "$RC" -ne 0 ]; then
    fail "gpu-inventory refused: $ERR"
else
    got="$(printf '%s\n' "$OUT" | yq -o=json '.' 2>/dev/null)"
    [ "$(printf '%s' "$got" | jq -r '.[0].type')" = "gpu-inventory" ] \
        || fail "gpu-inventory: type is wrong: $got"
    [ "$(printf '%s' "$got" | jq -r '[.[0] | has("scope"), has("quotas"), has("namespaceQuotas"), has("exclude")] | any')" = "false" ] \
        || fail "gpu-inventory: carries quota fields, which validateLimiters rejects: $got"
    ok "gpu-inventory emits a bare entry, with none of the quota fields it forbids"
fi

# ---------------------------------------------------------------------------
# The refusals. Each one used to be a silently unbounded (or frozen) install.
# ---------------------------------------------------------------------------

# The original bug. An unset WVA_QUOTAS has no safe reading: an empty quota entry
# is not "unlimited", it is zero for every accelerator, so the fleet freezes.
unset WVA_QUOTAS 2>/dev/null || true
WVA_SCOPE=namespace WVA_WATCH_NS=tenant-a emit quota
if [ "$RC" -eq 0 ]; then
    fail "quota with no WVA_QUOTAS was accepted -- that is the entry the controller rejects, costing the whole policy: $OUT"
else
    case "$ERR" in
        *WVA_QUOTAS*) ok "quota with no budget is refused, naming WVA_QUOTAS" ;;
        *) fail "quota with no budget was refused without naming WVA_QUOTAS: $ERR" ;;
    esac
fi

for bad in 'H200' 'H200=' '=8' 'H200=eight' 'H200=-4' 'H200=1048577'; do
    WVA_QUOTAS="$bad" WVA_SCOPE=namespace WVA_WATCH_NS=tenant-a emit quota
    if [ "$RC" -eq 0 ]; then
        fail "WVA_QUOTAS='$bad' was accepted and emitted: $OUT"
    else
        ok "WVA_QUOTAS='$bad' refused"
    fi
done

WVA_QUOTAS='H200=8' WVA_QUOTA_SCOPE=global emit quota
if [ "$RC" -eq 0 ]; then
    fail "WVA_QUOTA_SCOPE=global was accepted; Validate only knows cluster and namespace"
else
    ok "an unknown WVA_QUOTA_SCOPE is refused"
fi

# -1 is the documented no-cap sentinel (QuotaUnlimited), so it must survive.
WVA_QUOTAS='H200=-1' WVA_SCOPE=namespace WVA_WATCH_NS=tenant-a emit quota
if [ "$RC" -ne 0 ]; then
    fail "the -1 no-cap sentinel was refused: $ERR"
else
    got="$(printf '%s\n' "$OUT" | yq -o=json '.' 2>/dev/null)"
    [ "$(printf '%s' "$got" | jq -r '.[0].namespaceQuotas["tenant-a"].H200 // "absent"')" = "-1" ] \
        || fail "the -1 no-cap sentinel did not survive: $got"
    ok "-1 (QuotaUnlimited) is accepted"
fi

# ---------------------------------------------------------------------------
# The merge into the policy. Emitting a good entry is half of it; the other half
# is landing it in data["default"] without losing the rest.
#
# This is not hypothetical: the first version used `env(LIMITERS_YAML)`, which
# yq parses as YAML and which fails on a multi-line document with `Error: EOF`
# and no mention of the variable. The install aborted AFTER re-applying the
# shipped ConfigMap, so the policy was left with no limiters at all.
# ---------------------------------------------------------------------------
CASE_FAIL_AT="$FAIL"
FIXTURE_POLICY='# a comment the operator wrote
scaleUpThreshold: 0.85
kvCacheThreshold: 0.80
enableRescale: false
limiters:
  - type: gpu-inventory'
WVA_SCOPE=namespace WVA_WATCH_NS=tenant-a WVA_QUOTAS='H200=8' emit quota
merged="$(policy_with_limiters "$FIXTURE_POLICY" "$OUT" 2>"$WORK/err")"
mrc=$?
if [ "$mrc" -ne 0 ] || [ -z "$merged" ]; then
    fail "policy_with_limiters failed on a multi-line entry: $(cat "$WORK/err")"
else
    [ "$(printf '%s\n' "$merged" | yq '.scaleUpThreshold')" = "0.85" ] \
        || fail "the merge lost scaleUpThreshold -- a rejected or truncated policy costs every default in it"
    [ "$(printf '%s\n' "$merged" | yq '.limiters | length')" = "1" ] \
        || fail "the merge APPENDED instead of replacing; two limiters means the other is silently unenforced"
    [ "$(printf '%s\n' "$merged" | yq '.limiters[0].name')" = "install-quota" ] \
        || fail "the merge did not replace the previous limiter: $merged"
    [ "$(printf '%s\n' "$merged" | yq '.limiters[0].namespaceQuotas.tenant-a.H200')" = "8" ] \
        || fail "the budget did not survive the merge: $merged"
    ok "the entry merges into the policy, replacing the old limiters and keeping the rest"
fi

# ---------------------------------------------------------------------------
# The mirror above is only worth anything while it matches the Go it mirrors.
# ---------------------------------------------------------------------------
VALIDATE="$ROOT/internal/config/quota_limiter.go"
CASE_FAIL_AT="$FAIL"
if [ ! -f "$VALIDATE" ]; then
    fail "cannot find $VALIDATE to cross-check the validation rules against"
else
    # Every `errs = append(errs, ...)` in Validate() is a way to have the entry
    # rejected. The count is the mirror's tripwire: a new one means a new rule
    # this file does not assert, and an installer that may be emitting an entry
    # the controller will throw the whole policy away over.
    rules=$(grep -c 'errs = append(errs' "$VALIDATE")
    expected=14
    if [ "$rules" -ne "$expected" ]; then
        fail "internal/config/quota_limiter.go now has $rules rejection rules, not $expected.
     A rule was added or removed since these cases were written. Read Validate(), decide whether
     limiter_entry_yaml can still emit an entry that trips the new one, add a case here, and
     update \$expected."
    else
        ok "the $rules rejection rules in Validate() are the ones these cases were written against"
    fi
fi

if [ "$FAIL" -ne 0 ]; then
    echo "limiter declaration check FAILED"
    exit 1
fi
echo "limiter declaration OK"
