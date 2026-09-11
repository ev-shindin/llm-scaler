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

# The tools, named before anything is asserted. Without this a missing jq made
# every case that parses output fail, first among them "name is empty -- the
# controller rejects the entry and drops the whole policy" -- fifteen confident
# accusations against the code for an absent binary. Both siblings in this Make
# target already guard; this one did not.
for tool in yq jq; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "FATAL: $tool is required by this check, and is missing. Nothing below would be about the code."
        exit 2
    }
done

if ! declare -F limiter_entry_yaml >/dev/null; then
    echo "FAIL limiter_entry_yaml is not defined -- the library did not source"
    exit 1
fi

CASES=0
# A case is counted where it BEGINS, by case_begin, not derived from whether it
# passed or failed. Two previous attempts derived it and both were wrong in
# opposite directions: counting in both fail() and ok() double-counted a failing
# multi-assert case ("FAIL 35 cases ran, not 30"), and moving the increment
# behind ok()'s suppression under-counted one ("32 cases ran, not 33"). Either
# way a real failure printed a SECOND, untrue verdict accusing the reader of
# editing the test. A marker at the start of each case cannot depend on its
# outcome.
case_begin() { CASES=$((CASES + 1)); CASE_FAIL_AT="$FAIL"; }
fail() { echo "FAIL $*"; FAIL=$((FAIL + 1)); }
# A case ends in one summarising ok, but its assertions are separate statements,
# so a failed one does not stop the ok from printing after it. Suppress it when
# anything failed since this case began -- an `ok` under four FAILs describing
# the same output is how a negative control gets read as half-passing.
ok()   { [ "$FAIL" -eq "${CASE_FAIL_AT:-0}" ] || return 0; echo "ok   $*"; }

# emit <type> -- runs limiter_entry_yaml with the environment already set by the
# caller, into $OUT/$RC/$ERR. Never lets a non-zero status abort the harness: a
# refusal is the expected result for half these cases. Every case starts here, so
# this is also where the per-case failure mark is taken.
emit() {
    case_begin
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

# The CLUSTER-policy key. `make enable-physical-limiter` publishes ONE policy
# that every controller on the cluster reads, so a namespace-scoped budget there
# must use the reserved per-unlisted-namespace key: keyed on a single namespace,
# that namespace gets the budget and every other one gets ZERO, and the command
# meant to bound the cluster stops scaling on it instead.
#
# It also has to work with NO WVA_NS and no WVA_WATCH_NS, because the Makefile
# recipe passes neither. That combination hard-failed the documented command.
case_begin
( unset WVA_NS WVA_WATCH_NS WVA_SCOPE
  WVA_QUOTAS='H200=8' limiter_entry_yaml quota default )     >"$WORK/clusterkey" 2>"$WORK/clustererr"
clusterrc=$?
if [ "$clusterrc" -ne 0 ]; then
    fail "the cluster-policy path refused a valid budget with no WVA_NS set, which is exactly how the Makefile recipe calls it: $(cat "$WORK/clustererr")"
elif [ "$(yq -o=json '.' "$WORK/clusterkey" 2>/dev/null | jq -r '.[0].namespaceQuotas.default.H200 // "absent"')" != "8" ]; then
    fail "the cluster-policy path did not key on the reserved \`default\`: $(cat "$WORK/clusterkey")"
else
    ok "the cluster-policy path keys on the reserved \`default\`, with no namespace of its own"
fi

# The key is an ARGUMENT, so nothing in the operator's environment can set it.
# It was an environment variable for one commit, and that made it the
# highest-precedence input to the document while being the only input with no
# validation: `WVA_QUOTA_NS_KEY='evil: 1'` emitted YAML no parser accepts, and a
# merely wrong one left the managed namespace unlisted -- a budget of zero for
# everything, reported as success.
case_begin
( export WVA_QUOTA_NS_KEY=hijacked
  WVA_SCOPE=namespace WVA_WATCH_NS=tenant-a WVA_QUOTAS='H200=8'       limiter_entry_yaml quota ) >"$WORK/envkey" 2>/dev/null
if grep -q 'hijacked' "$WORK/envkey"; then
    fail "an exported WVA_QUOTA_NS_KEY reached the policy; the namespace key must come only from the caller's argument: $(cat "$WORK/envkey")"
else
    ok "no environment variable can set the namespace key"
fi

# And an invalid key argument is refused rather than written verbatim.
case_begin
( WVA_QUOTAS='H200=8' limiter_entry_yaml quota 'evil: 1' ) >"$WORK/badkey" 2>/dev/null
if [ $? -eq 0 ] || [ -s "$WORK/badkey" ]; then
    fail "an invalid namespace key was accepted: $(cat "$WORK/badkey")"
else
    ok "an invalid namespace key is refused rather than written into the document"
fi

# An INVALID WVA_SCOPE must stop the build, not fall through to a default. It
# fell through: wva_install_scope reports through log_error, whose exit dies in
# the command substitution, so install_scope came back empty, the namespace
# branch was not taken, and the entry was keyed `default` -- on a path with no
# `set -e`, which then published it and printed SUCCESS.
case_begin
( WVA_SCOPE=bogus WVA_NS=wva-system WVA_QUOTAS='H200=8' limiter_entry_yaml quota )     >"$WORK/badscope" 2>/dev/null
if [ $? -eq 0 ] || [ -s "$WORK/badscope" ]; then
    fail "an invalid WVA_SCOPE produced an entry instead of stopping: $(cat "$WORK/badscope")"
else
    ok "an invalid WVA_SCOPE stops the build rather than falling through to a key"
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
case_begin
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
case_begin
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
case_begin
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
case_begin
STUB="$WORK/stub"; mkdir -p "$STUB"
# Records every write and answers reads with whatever CURRENT_POLICY holds.
cat > "$STUB/kubectl" <<'STUBEOF'
#!/usr/bin/env bash
# A payload is recorded as a SENTINEL line, not raw.
#
# `--from-literal=default=` with an EMPTY value is the worst write this function
# can make -- it is the blanked data.default that was published to every target
# namespace -- and recording it raw left a zero-byte file, which `[ -s ]` reads
# as "nothing was written". The assertion was blind to precisely the catastrophe
# it exists to catch.
# BOTH write shapes. pl_set_limiter creates a missing ConfigMap with
# --from-literal and PATCHES an existing one -- it has to, because the
# create|apply pipeline's manifest carries only data.default and apply's
# three-way merge then deletes every other key in it. A stub that knew only the
# create form reported the patch path as writing nothing.
prev=""
for a in "$@"; do
  case "$a" in
    --from-literal=default=*) printf 'WROTE[%s]\n' "${a#--from-literal=default=}" >> "$KWROTE" ;;
    -p) : ;;
    *) if [ "$prev" = "-p" ]; then
         # {"data":{"default":"<policy>"}} -- unwrap to the same sentinel the
         # create form produces, so the assertions do not care which was used.
         printf 'WROTE[%s]\n' "$(printf '%s' "$a" | jq -r '.data.default // ""' 2>/dev/null)" >> "$KWROTE"
       fi ;;
  esac
  prev="$a"
done
# Every invocation too, so a case can tell "composed a document" from "delivered
# it": dropping the `| kubectl apply -f -` off the end of the pipeline leaves a
# function that builds the right ConfigMap and ships it nowhere.
printf 'CALL[%s]\n' "$*" >> "$KCALLS"
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
    CALLS="$WORK/calls"; : > "$CALLS"
    (
        PATH="$STUB:$PATH"; export PATH
        KWROTE="$WROTE"; export KWROTE
        KCALLS="$CALLS"; export KCALLS
        # "fresh" is the cluster with NO policy ConfigMap yet -- the common case
        # on a first `make enable-physical-limiter`, and one this harness never
        # ran because CURRENT_POLICY was always set.
        if [ "${2:-}" = "fresh" ]; then
            unset CURRENT_POLICY
        else
            CURRENT_POLICY="scaleUpThreshold: 0.85"; export CURRENT_POLICY
        fi
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
    calls="$(cat "$CALLS")"
    if [ "$PLRC" -ne 0 ]; then
        fail "pl_set_limiter refused a VALID budget: $(tail -1 "$WORK/plerr")"
    elif ! printf '%s' "$wrote" | grep -q 'install-quota'; then
        fail "pl_set_limiter wrote a policy with no named limiter entry, which the controller rejects: $wrote"
    elif ! printf '%s' "$wrote" | grep -q 'scaleUpThreshold'; then
        fail "pl_set_limiter dropped the rest of the policy while declaring the limiter: $wrote"
    # DELIVERY, not just composition. Dropping the `| kubectl apply -f -` from
    # the end of the pipeline leaves a function that builds the right ConfigMap
    # and ships it nowhere, and asserting only on the built document passed it.
    # apply OR patch: an existing ConfigMap is PATCHED (the create|apply
    # pipeline's manifest carries only data.default, and apply's three-way merge
    # then deletes every other key), a missing one is created and applied.
    elif ! printf '%s' "$calls" | grep -qE 'CALL\[(apply|patch)'; then
        fail "pl_set_limiter composed a ConfigMap and never delivered it; the policy would never reach the cluster. Calls seen: $calls"
    elif ! printf '%s' "$calls" | grep -q 'wva-scaling-policy-config'; then
        fail "pl_set_limiter did not name the wva-scaling-policy-config ConfigMap: $calls"
    elif ! printf '%s' "$calls" | grep -q 'wva-policy'; then
        fail "pl_set_limiter wrote to a namespace other than the one it was given: $calls"
    else
        ok "pl_set_limiter writes a named entry, keeps the rest, and applies it where it was told"
    fi

    # THE ONE THAT MATTERS. A budget the builder refuses must leave the policy
    # ALONE. Removing the status check here wrote an EMPTY data.default to every
    # target namespace and returned 0, and the command then announced the limiter
    # in force -- which is what the two source-pattern versions of this case
    # failed to catch.
    #
    # Asserted on the stub's SENTINEL, not on the size of what it recorded. An
    # empty --from-literal left a zero-byte file, `[ -s ]` read that as "nothing
    # was written", and the case was blind to the one write that matters most.
    case_begin
    run_pl 'H200=12abc'
    wrote="$(cat "$WROTE")"
    if [ "$PLRC" -eq 0 ]; then
        fail "pl_set_limiter returned 0 on a budget the builder refuses; enable_physical_limiter would go on to announce a limiter it did not write"
    elif printf '%s' "$wrote" | grep -q 'WROTE\[\]'; then
        fail "pl_set_limiter wrote an EMPTY policy over data.default -- the blanked-policy failure itself"
    elif [ -n "$wrote" ] && ! printf '%s' "$wrote" | grep -q 'scaleUpThreshold'; then
        fail "pl_set_limiter wrote a policy anyway, and it is not the one it started from: [$wrote]"
    elif [ -n "$wrote" ]; then
        fail "pl_set_limiter wrote to the ConfigMap despite refusing the budget: [$wrote]"
    else
        ok "a refused budget leaves the cluster policy untouched, and stops the command"
    fi

    # THE CALL SITE, not the library. The three cases above prove the library
    # HONOURS a key it is given; none of them proved pl_set_limiter GIVES it one.
    # Deleting the `default` argument from the call was green on all 35 cases
    # while, with WVA_NS exported -- the state of anyone who just ran the
    # installer -- it wrote `namespaceQuotas: {<WVA_NS>: ...}` into the policy
    # every controller on the cluster reads: that one namespace gets the budget
    # and every other gets ZERO, under "in force for every WVA on this cluster".
    #
    # WVA_WATCH_NS is set here deliberately. If the call site stops passing
    # `default`, the installer's own rule picks it up, and the emitted key
    # changes to tenant-a -- which is exactly the regression.
    case_begin
    run_pl 'H200=8'
    wrote="$(cat "$WROTE")"
    # Grepped, not reparsed: the sentinel wraps a MULTI-LINE payload, so
    # `WROTE[` opens on one line and `]` closes on another and no per-line
    # extraction gets the document back. The two key names are all this needs.
    if printf '%s' "$wrote" | grep -q 'tenant-a:'; then
        fail "the cluster policy was keyed on the managed namespace instead of the reserved \`default\`: every OTHER namespace then reads a budget of zero and stops scaling up. Wrote: $wrote"
    elif ! printf '%s' "$wrote" | grep -q 'default:'; then
        fail "the cluster policy carries no namespace key at all: $wrote"
    else
        ok "the cluster-policy call site keys the budget on the reserved \`default\`"
    fi

    # AN EXISTING ConfigMap must be PATCHED, never re-applied from a manifest
    # that carries only data.default.
    #
    # Measured against a real apiserver: the create|apply pipeline renders a
    # manifest with one key, and apply's three-way merge deletes every key that
    # was in last-applied-configuration and is not in the new manifest -- so a
    # ConfigMap carrying a per-model override entry beside `default` came back
    # with the override GONE, exit 0, "the limiter is now in force for every WVA
    # on this cluster". Those override entries are the shape
    # config/base/manager/scaling-policy-configmap.yaml documents.
    #
    # The stub cannot model apply's merge, so what is asserted is the CHOICE:
    # an existing ConfigMap is patched by name, not re-created.
    case_begin
    run_pl 'H200=8'
    calls="$(cat "$CALLS")"
    if printf '%s' "$calls" | grep -q 'CALL\[create configmap'; then
        fail "pl_set_limiter re-CREATED an existing ConfigMap; kubectl apply then deletes every other data key in it, and the per-model override entries beside \`default\` are lost. Calls: $calls"
    elif ! printf '%s' "$calls" | grep -q 'CALL\[patch configmap'; then
        fail "pl_set_limiter neither patched nor created: $calls"
    else
        ok "an existing policy ConfigMap is patched by key, so its other entries survive"
    fi

    # A FRESH cluster: no policy ConfigMap yet, which is the first thing
    # `make enable-physical-limiter` meets and which this harness never ran,
    # because CURRENT_POLICY was always set. pl_set_limiter substitutes
    # `limiters: []` for the missing read; dropping that fallback leaves the
    # merge with empty input.
    case_begin
    run_pl 'H200=8' fresh
    wrote="$(cat "$WROTE")"
    if [ "$PLRC" -ne 0 ]; then
        fail "pl_set_limiter failed on a cluster with no policy ConfigMap yet: $(tail -1 "$WORK/plerr")"
    elif ! printf '%s' "$wrote" | grep -q 'install-quota'; then
        fail "pl_set_limiter wrote no limiter entry on a fresh cluster: [$wrote]"
    else
        ok "a cluster with no policy ConfigMap yet gets one with the entry in it"
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
case_begin
if declare -F enable_physical_limiter >/dev/null; then
    body="$(declare -f enable_physical_limiter)"
    # The CALL, not the name. Matching the name ANYWHERE was satisfied by putting
    # it in a log string near the top while the real call moved to the last line
    # -- mutation-tested, that printed ok. A call is the first word of a command,
    # so anchor on that.
    # An assignment PREFIX is still a call -- `WVA_QUOTA_NS_KEY=default
    # limiter_entry_yaml ...` is how the cluster path passes the key -- so the
    # pattern allows any number of VAR=value words before the command.
    validate_at="$(printf '%s\n' "$body" \
        | grep -nE '^[[:space:]]*([A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*[[:space:]]+)*(limiter_entry_yaml|policy_declared_limiters)[[:space:]]' \
        | head -1 | cut -d: -f1)"
    # Anything that touches the cluster. kubectl is the only way this function
    # writes, so the first kubectl line is the point of no return.
    # A kubectl CALL, not the word. Unanchored, this matched
    # `for tool in kubectl jq yq` -- the preflight that names the tools -- and
    # reported the validation as happening after a write that is a loop header.
    # Same anchoring the validate_at pattern needs, and for the same reason.
    first_write="$(printf '%s\n' "$body" \
        | grep -nE '^[[:space:]]*([A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*[[:space:]]+)*(kubectl|pl_grant_[a-z_]*|pl_set_limiter)[[:space:]]' \
        | head -1 | cut -d: -f1)"
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
case_begin
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
case_begin
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
case_begin
CASES_EXPECTED=39
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
