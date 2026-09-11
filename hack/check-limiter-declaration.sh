#!/usr/bin/env bash
#
# Executes limiter_entry_yaml() and policy_with_limiters() in
# deploy/lib/limiter_policy.sh, and asserts the
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
# The rules asserted below mirror QuotaLimiterEntries.Validate() and
# validateLimiters() in internal/config. A mirror nobody notices going stale is
# how this class of bug returns, so two cases read the Go back: the constants
# limiter_entry_yaml hardcodes (MaxQuotaValue, QuotaUnlimited), and whether
# validateLimiters singles out the name we emit. Neither covers a NEW rejection
# rule -- read Validate() when you touch it.
#
# limiter_entry_yaml and policy_with_limiters are pure and are called directly.
# pl_set_limiter is not: it writes to a cluster, so it runs against a STUBBED
# kubectl, and what is asserted is what it would have written. That case exists
# because two earlier versions of it asserted on the source instead, and an
# audit walked past both.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAIL=0

# Sourced through a CR-stripped copy, for the reason hack/check-workload-gaps.sh
# gives: a Windows checkout writes these LF files as CRLF and bash then stops at
# the first function definition, which looks exactly like every function missing.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh limiter_policy.sh physical_limiter.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib"
done
# common.sh logs in colour and dies on an unbound variable under `set -u`.
BLUE=''; GREEN=''; YELLOW=''; RED=''; NC=''
# shellcheck disable=SC1090
. "$WORK/common.sh"
# EVERY variable the builders read, pinned -- not just the ones the cases set.
# WVA_QUOTA_SCOPE is a documented install variable an admin plausibly has
# exported, and with `WVA_QUOTA_SCOPE=cluster` in the environment this check
# produced six FAILs all blaming the code. A check that depends on the caller's
# shell reports on the shell.
unset WVA_QUOTAS WVA_QUOTA_SCOPE WVA_SCOPE WVA_WATCH_NS WVA_LIMITER WVA_LIMITER_TYPE
WVA_NS="wva-system"
# shellcheck disable=SC1090
. "$WORK/limiter_policy.sh"
# physical_limiter.sh is sourced only so the cluster-policy case below can call
# pl_set_limiter's validation sibling. It declares no top-level work.
WVA_POLICY_NS="wva-policy"
# shellcheck disable=SC1090
. "$WORK/physical_limiter.sh"

if ! declare -F limiter_entry_yaml >/dev/null; then
    echo "FAIL limiter_entry_yaml is not defined -- the library did not source"
    exit 1
fi

CASES=0
# One count per CASE, not per assertion. A case with several assertions can fail
# several times, and counting each inflated the total -- so a genuine failure
# also produced "FAIL 30 cases ran, not 24. A case was added or removed", which
# is a second verdict, untrue, and pointing at the wrong thing. Only the FIRST
# failure of a case counts; the rest are more evidence about the same case.
fail() {
    echo "FAIL $*"
    [ "$FAIL" -eq "${CASE_FAIL_AT:-0}" ] && CASES=$((CASES + 1))
    FAIL=$((FAIL + 1))
}
# A case ends in one summarising ok, but its assertions are separate statements,
# so a failed one does not stop the ok from printing after it. Suppress it when
# anything failed since this case began -- an `ok` under four FAILs describing the
# same output is how a negative control gets read as half-passing.
ok()   { CASES=$((CASES + 1)); [ "$FAIL" -eq "${CASE_FAIL_AT:-0}" ] || return 0; echo "ok   $*"; }

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

# Each pairs the bad value with the WORDS its refusal must carry. A bare
# "did it exit non-zero" is satisfied by a function replaced with `log_error
# "boom"` -- mutation-tested, all six printed ok against a one-line stub.
#
# '12abc' and '16Gi' are here because they were ACCEPTED: the guard was
# `[0-9][0-9]*`, a glob whose `*` matches anything, so every value starting with
# two digits passed and `H200: 12abc` was written into the policy.
refuse_quota() {
    local bad="$1" want="$2"
    WVA_QUOTAS="$bad" WVA_SCOPE=namespace WVA_WATCH_NS=tenant-a emit quota
    if [ "$RC" -eq 0 ]; then
        fail "WVA_QUOTAS='$bad' was accepted and emitted: $OUT"
        return
    fi
    case "$ERR" in
        *"$want"*) ok "WVA_QUOTAS='$bad' refused, saying why" ;;
        *) fail "WVA_QUOTAS='$bad' was refused without '$want'; it said: ${ERR:-<nothing>}" ;;
    esac
}
refuse_quota 'H200'         'is not TYPE=N'
refuse_quota 'H200='        'is not TYPE=N'
refuse_quota '=8'           'is not TYPE=N'
refuse_quota 'H200=eight'   'whole number of GPUs'
refuse_quota 'H200=12abc'   'whole number of GPUs'
refuse_quota 'H200=16Gi'    'whole number of GPUs'
refuse_quota 'H200=12.5'    'whole number of GPUs'
refuse_quota 'H200=-4'      'whole number of GPUs'
refuse_quota 'H200=1048577' 'maximum quota'
# All digits, so the numeric case lets it past; `[ -gt ]` on 20 digits then
# errors with "integer expression expected", which is not fatal inside an `if`,
# and the entry was emitted for go-yaml to refuse -- taking the whole policy.
refuse_quota 'H200=99999999999999999999' 'far above the maximum'
# Also all digits. YAML reads a leading zero as OCTAL: 010 becomes 8, so the
# operator asks for ten GPUs and silently gets eight.
refuse_quota 'H200=010'      'leading zero'
# Two budgets for one type emit a duplicate YAML key; the controller refuses the
# document and discards the entire policy.
refuse_quota 'H200=8 H200=4' 'twice'
# Non-empty, but naming NOTHING. The loop runs no iterations, so the entry came
# out with an empty accelerator map: structurally valid, accepted by
# validateLimiters, and read as a budget of ZERO for every type -- which stops
# every managed workload from scaling up. The check for an EMPTY WVA_QUOTAS did
# not see these, because they are not empty.
refuse_quota '   '           'names no accelerator'
refuse_quota ','             'names no accelerator'

# A glob in WVA_QUOTAS must not be expanded against the working directory. It
# was: with `for pair in $pairs` unguarded, WVA_QUOTAS='*' in a directory of
# `name=value` files produced a GPU budget synthesised from filenames, exit 0.
CASE_FAIL_AT="$FAIL"
globdir="$WORK/globtest"; mkdir -p "$globdir"
: > "$globdir/aaa=1"; : > "$globdir/bbb=2"
( cd "$globdir" && WVA_QUOTAS='*' WVA_SCOPE=namespace WVA_WATCH_NS=tenant-a \
    limiter_entry_yaml quota >"$WORK/globout" 2>"$WORK/globerr" )
if [ -s "$WORK/globout" ] && grep -q 'aaa' "$WORK/globout"; then
    fail "WVA_QUOTAS='*' was expanded against the working directory: $(cat "$WORK/globout")"
else
    ok "a glob in WVA_QUOTAS is not expanded against the working directory"
fi

# The namespace key cannot be empty: it renders as a bare `:`, yq refuses the
# document, and the cluster-policy caller (no set -e) then wrote an EMPTY policy
# and announced the limiter in force.
CASE_FAIL_AT="$FAIL"
( unset WVA_NS WVA_WATCH_NS; WVA_SCOPE=namespace WVA_QUOTAS='H200=8' \
    limiter_entry_yaml quota >"$WORK/nskey" 2>"$WORK/nserr" )
if [ -s "$WORK/nskey" ]; then
    fail "namespace scope with no namespace emitted an entry with an empty key: $(cat "$WORK/nskey")"
else
    ok "namespace scope with no namespace to key on is refused"
fi

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
# The CLUSTER policy path, EXECUTED.
#
# `make enable-physical-limiter WVA_LIMITER_TYPE=quota` publishes into the
# well-known namespace every WVA on the cluster reads and cannot opt out of. It
# built `[{"type": "quota"}]` of its own -- the entry the controller rejects --
# which would have stripped the policy from every controller at once while
# printing "The quota limiter is now in force for every WVA on this cluster."
#
# RUN, not pattern-matched. Two successive versions of this case asserted on the
# source instead, and an audit walked past both: the first grepped the FILE for
# two function names that also appear in its comments, and the second, reading
# `declare -f`, was satisfied by `.limiters |= [...]` (the glob wanted `=[`) and
# by the function NAME inside an unrelated log string. Pattern-matching source is
# not a test of behaviour. kubectl is stubbed, so what is asserted is what this
# function would WRITE.
# ---------------------------------------------------------------------------
CASE_FAIL_AT="$FAIL"
STUB="$WORK/stub"; mkdir -p "$STUB"
# Records every write and answers reads with whatever CURRENT_POLICY holds.
cat > "$STUB/kubectl" <<'STUBEOF'
#!/usr/bin/env bash
for a in "$@"; do
  case "$a" in
    --from-literal=default=*) printf '%s' "${a#--from-literal=default=}" > "$KWROTE" ;;
  esac
done
case "$1" in
  get) printf '%s' "${CURRENT_POLICY:-}" ;;
  *) : ;;
esac
exit 0
STUBEOF
chmod +x "$STUB/kubectl"

# run_pl <WVA_QUOTAS> -- pl_set_limiter in a SUBSHELL (its refusals exit), with
# the stub first on PATH. Leaves the written policy in $WROTE and status in $PLRC.
run_pl() {
    WROTE="$WORK/wrote"; : > "$WROTE"
    (
        PATH="$STUB:$PATH"; export PATH
        KWROTE="$WROTE"; export KWROTE
        CURRENT_POLICY="scaleUpThreshold: 0.85"; export CURRENT_POLICY
        WVA_QUOTAS="$1" WVA_SCOPE=namespace WVA_WATCH_NS=tenant-a \
            pl_set_limiter wva-policy quota
    ) >/dev/null 2>"$WORK/plerr"
    PLRC=$?
}

if ! declare -F pl_set_limiter >/dev/null; then
    fail "pl_set_limiter is not defined -- physical_limiter.sh did not source"
else
    run_pl 'H200=8'
    wrote="$(cat "$WROTE")"
    if [ "$PLRC" -ne 0 ]; then
        fail "pl_set_limiter refused a VALID budget: $(tail -1 "$WORK/plerr")"
    elif ! printf '%s' "$wrote" | grep -q 'install-quota'; then
        fail "pl_set_limiter wrote a policy with no named limiter entry, which the controller rejects: $wrote"
    elif ! printf '%s' "$wrote" | grep -q 'scaleUpThreshold'; then
        fail "pl_set_limiter dropped the rest of the policy while declaring the limiter: $wrote"
    else
        ok "pl_set_limiter writes a named entry and keeps the rest of the policy"
    fi

    # THE ONE THAT MATTERS. A budget the builder refuses must leave the policy
    # ALONE. Removing the status check here wrote an EMPTY data.default to every
    # target namespace and returned 0, and the command then announced the limiter
    # in force -- which is what the two source-pattern versions of this case
    # failed to catch.
    CASE_FAIL_AT="$FAIL"
    run_pl 'H200=12abc'
    wrote="$(cat "$WROTE")"
    if [ "$PLRC" -eq 0 ]; then
        fail "pl_set_limiter returned 0 on a budget the builder refuses; enable_physical_limiter would go on to announce a limiter it did not write"
    elif [ -s "$WROTE" ] && ! printf '%s' "$wrote" | grep -q 'scaleUpThreshold'; then
        fail "pl_set_limiter wrote a policy anyway, and it is not the one it started from: [$wrote]"
    elif [ -s "$WROTE" ]; then
        fail "pl_set_limiter wrote to the ConfigMap despite refusing the budget"
    else
        ok "a refused budget leaves the cluster policy untouched, and stops the command"
    fi
fi

# And it must refuse a budgetless quota BEFORE it creates the namespace and
# grants RBAC -- its own writes come last, so a late refusal leaves a
# half-configured cluster behind.
#
# ORDER is the assertion, not presence. A validation moved to the LAST line of
# the function satisfies "contains limiter_entry_yaml" while producing exactly
# the half-configured cluster the case exists to prevent -- mutation-tested, it
# printed ok.
CASE_FAIL_AT="$FAIL"
if declare -F enable_physical_limiter >/dev/null; then
    body="$(declare -f enable_physical_limiter)"
    # The CALL, not the name. Matching the name ANYWHERE was satisfied by putting
    # it in a log string near the top while the real call moved to the last line
    # -- mutation-tested, that printed ok. A call is the first word of a command,
    # so anchor on that.
    validate_at="$(printf '%s\n' "$body" \
        | grep -nE '^[[:space:]]*(limiter_entry_yaml|policy_declared_limiters)[[:space:]]' \
        | head -1 | cut -d: -f1)"
    # Anything that touches the cluster. kubectl is the only way this function
    # writes, so the first kubectl line is the point of no return.
    first_write="$(printf '%s\n' "$body" | grep -nE 'kubectl|pl_grant_|pl_set_limiter' | head -1 | cut -d: -f1)"
    if [ -z "$validate_at" ]; then
        fail "enable_physical_limiter never validates the limiter entry; a rejected WVA_QUOTAS would abort it after the namespace and grants exist"
    elif [ -z "$first_write" ]; then
        fail "enable_physical_limiter appears to touch no cluster state at all; this case can no longer tell early from late"
    elif [ "$validate_at" -lt "$first_write" ]; then
        ok "enable_physical_limiter validates the budget before it changes anything"
    else
        fail "enable_physical_limiter validates at line $validate_at of its body but first writes at line $first_write: a rejected WVA_QUOTAS would abort after the namespace and grants exist"
    fi
else
    fail "enable_physical_limiter is not defined -- physical_limiter.sh did not source"
fi

# ---------------------------------------------------------------------------
# The mirror above is only worth anything while it matches the Go it mirrors.
# ---------------------------------------------------------------------------
VALIDATE="$ROOT/internal/config/quota_limiter.go"
CASE_FAIL_AT="$FAIL"
if [ ! -f "$VALIDATE" ]; then
    fail "cannot find $VALIDATE to cross-check against"
else
    # The two numbers limiter_entry_yaml HARDCODES, read back from the Go that
    # gives them meaning. A counting tripwire stood here before and was blind to
    # every drift worth catching: it counted `errs = append(errs` lines, so
    # changing MaxQuotaValue or QuotaUnlimited moved nothing, and swapping one
    # rule for another kept the total at 14. Mutation-tested -- all three
    # printed ok.
    if grep -qE 'MaxQuotaValue += +1 +<< +20' "$VALIDATE"; then
        ok "MaxQuotaValue is still 1<<20, the 1048576 this script refuses above"
    else
        fail "MaxQuotaValue is no longer 1<<20; the 1048576 hardcoded in limiter_entry_yaml now rejects values the controller would accept, or passes ones it would not:
     $(grep -n 'MaxQuotaValue' "$VALIDATE" | head -2)"
    fi
    if grep -qE 'QuotaUnlimited += +-1' "$VALIDATE"; then
        ok "QuotaUnlimited is still -1, the no-cap sentinel this script accepts above"
    else
        fail "QuotaUnlimited is no longer -1; limiter_entry_yaml still emits -1 for 'no cap':
     $(grep -n 'QuotaUnlimited' "$VALIDATE" | head -2)"
    fi
fi

# The name limiter_entry_yaml emits must be one validateLimiters() accepts. This
# is the rule a counting tripwire could never see: a new rejection there -- on
# the name, the scope, a field combination -- would make every install emit an
# entry the controller throws the whole policy away over, and nothing in this
# file would notice.
CASE_FAIL_AT="$FAIL"
SATURATION="$ROOT/internal/config/saturation_scaling.go"
WVA_SCOPE=namespace WVA_WATCH_NS=tenant-a WVA_QUOTAS='H200=8' emit quota
emitted_name="$(printf '%s\n' "$OUT" | yq -o=json '.' 2>/dev/null | jq -r '.[0].name // ""')"
if [ -z "$emitted_name" ]; then
    fail "could not read back the emitted limiter name"
elif [ ! -f "$SATURATION" ]; then
    fail "cannot find $SATURATION to check the emitted name against"
elif grep -q "\"$emitted_name\"" "$SATURATION"; then
    fail "validateLimiters() now names '$emitted_name' literally -- the name limiter_entry_yaml emits may be special-cased or rejected there:
     $(grep -n "\"$emitted_name\"" "$SATURATION" | head -2)"
else
    ok "the emitted name '$emitted_name' is not singled out by validateLimiters()"
fi

# How many cases ran. Without it, deleting a refuse_quota line removes a case
# and the check still prints OK -- and the ok() suppression means the count of
# ok lines is not comparable against a known-good run either.
CASE_FAIL_AT="$FAIL"
CASES_EXPECTED=30
if [ "$CASES" -ne "$CASES_EXPECTED" ]; then
    fail "$CASES cases ran, not $CASES_EXPECTED. A case was added or removed; update CASES_EXPECTED deliberately rather than letting coverage drift out."
else
    ok "all $CASES cases ran"
fi

if [ "$FAIL" -ne 0 ]; then
    echo "limiter declaration check FAILED"
    exit 1
fi
echo "limiter declaration OK"
