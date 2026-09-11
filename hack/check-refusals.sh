#!/usr/bin/env bash
#
# Finds refusals that cannot refuse.
#
# log_error is `echo >&2; exit 1` (deploy/lib/common.sh). Inside a COMMAND
# SUBSTITUTION that exit ends the subshell and nothing else -- the caller gets an
# empty string and carries on. Every deploy path that matters runs either with no
# `set -e` at all (the Makefile invokes `bash -c` for enable-physical-limiter) or
# assigns the result to a `local`, which masks the status too.
#
# This is not a hypothetical. It is the single most repeated defect in this area,
# and each instance shipped with a comment beside it asserting the opposite:
#
#   * limiter_entry_yaml called as `$(...)` with no status check -- the caller
#     patched `limiters: null` over the policy.
#   * pl_controller_namespaces calling log_error from inside
#     `namespaces="$(pl_controller_namespaces)"` -- the command printed
#     "[ERROR] could not list WVA controllers ... Refusing to publish a bound it
#     cannot deliver" and then "[SUCCESS] The limiter is now in force for every
#     WVA on this cluster", exit 0.
#   * wva_install_scope the same way, so a bogus WVA_SCOPE printed its refusal
#     and the entry was emitted anyway.
#
# Three separate review rounds each found a fresh instance, twice in code written
# to fix the previous one. A reviewer catches the next instance once; this
# catches the class.
#
# THE RULE: a function that refuses by exiting must be called where its exit
# reaches the caller -- plainly, or as an `if`/`&&`/`||` operand -- or the call
# site must check the status itself (`|| exit`, `|| return`, `|| log_error`).
# A bare `$(f ...)` from an exiting function is the bug.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAIL=0

# KNOWN, and not approved -- a debt list, as `<file> <function>` pairs.
#
# The pattern is repo-wide and predates this check; fixing all of it is its own
# piece of work, and a check that fails on main is a check nobody runs. So this
# ratchets: anything NOT listed here fails, and a listed pair that no longer
# occurs also fails, so the list cannot quietly rot into approval.
#
# Every one of these is a real hazard of the same shape. Two of them --
# wva_install_scope and wva_overlay_dir -- are the ones that actually bit:
# a bogus WVA_SCOPE printed its refusal from inside `$( )` and the caller
# carried on. Shrink this list; do not grow it.
KNOWN=(
    "deploy/lib/cleanup.sh wva_overlay_dir"
    "deploy/lib/common.sh wva_install_scope"
    "deploy/lib/common.sh wva_overlay_dir"
    "deploy/lib/dashboard.sh wva_dashboard_resolve_ns"
    "deploy/lib/infra_monitoring.sh wva_install_scope"
    "deploy/lib/infra_wva.sh wva_overlay_dir"
    "deploy/lib/install_core.sh wva_install_scope"
    "deploy/lib/install_core.sh wva_overlay_dir"
    "deploy/lib/prereqs.sh wva_install_scope"
    "deploy/lib/prereqs.sh wva_overlay_dir"
    "deploy/lib/scaledobject.sh so_pause_select"
    "deploy/lib/single_install.sh wva_install_scope"
    "deploy/lib/verify.sh wva_install_scope"
    "deploy/warmpool.sh warmpool_group_manifest"
    "deploy/warmpool.sh warmpool_manifest"
)
SEEN=()
is_known() {
    local pair="$1" k
    for k in "${KNOWN[@]}"; do [ "$k" = "$pair" ] && return 0; done
    return 1
}
note() {   # note <file> <fn> <line> <text> <shape>
    local pair="$1 $2"
    SEEN+=("$pair")
    if is_known "$pair"; then return 0; fi
    echo "FAIL $1:$3 $5 $2, whose refusal is an exit that the caller never sees."
    echo "     Add '|| exit 1' (or call it plainly), or the refusal is only a message."
    echo "       $4"
    FAIL=1
}
FILES=()
while IFS= read -r f; do FILES+=("$f"); done < <(
    ls "$ROOT"/deploy/lib/*.sh "$ROOT"/deploy/*.sh 2>/dev/null
)
[ "${#FILES[@]}" -gt 0 ] || { echo "FAIL no deploy shell libraries found to check"; exit 1; }

# Functions whose body calls log_error, i.e. whose refusal is an exit.
exiting=()
for f in "${FILES[@]}"; do
    while IFS= read -r fn; do
        # awk prints the function name when its body contains log_error.
        exiting+=("$fn")
    done < <(awk '
        /^[a-zA-Z_][a-zA-Z0-9_]*\(\) \{/ { name=$1; sub(/\(\).*/, "", name); body=""; depth=1; next }
        name != "" { body = body "\n" $0 }
        name != "" && /^\}/ { if (body ~ /log_error/) print name; name=""; body="" }
    ' "$f")
done
# De-duplicate.
readarray -t exiting < <(printf '%s\n' "${exiting[@]}" | sort -u | grep -v '^$')

if [ "${#exiting[@]}" -lt 3 ]; then
    echo "FAIL only ${#exiting[@]} exiting functions found; this repo has many."
    echo "     The extractor stopped recognising function definitions, so the scan below"
    echo "     would report nothing regardless of the code."
    exit 1
fi

echo "Refusing functions (exit via log_error): ${#exiting[@]}"

# Call sites in a context that swallows the exit.
for f in "${FILES[@]}"; do
    rel="${f#"$ROOT"/}"
    for fn in "${exiting[@]}"; do
        # `$(fn` or `$(fn)` -- a command substitution calling it.
        while IFS= read -r hit; do
            line="${hit%%:*}"
            text="${hit#*:}"
            # A checked call is fine: the caller acts on the status. `if
            # var="$(f)"` and `if ! var="$(f)"` are checks -- the whole point of
            # the fix applied to two of these -- as is any `|| <handler>`.
            case "$text" in
                *"|| exit"*|*"|| return"*|*"|| log_error"*|*"|| fail"*) continue ;;
                *"if "*|*"if! "*|*"while "*|*"until "*) continue ;;
            esac
            # A definition line, or a comment, is not a call.
            case "$text" in
                *"#"*"$fn"*) continue ;;
            esac
            note "$rel" "$fn" "$line" "${text#"${text%%[![:space:]]*}"}" "calls, inside a command substitution,"
        done < <(grep -nE "\\\$\\($fn( |\\))" "$f" || true)

        # A pipeline stage is a subshell too.
        while IFS= read -r hit; do
            line="${hit%%:*}"
            text="${hit#*:}"
            case "$text" in
                *"#"*) continue ;;
            esac
            note "$rel" "$fn" "$line" "${text#"${text%%[![:space:]]*}"}" "runs, in a pipeline (a subshell),"
        # A single `|`, never `||`: `f >/dev/null || exit 1` is a CHECK, and
        # matching it as a pipeline was this check's own first false positive.
        done < <(grep -nE "^[[:space:]]*$fn[[:space:]][^|]*[^|]\|[^|]" "$f" || true)
    done
done

# A listed pair that no longer occurs has been FIXED, and leaving it listed hides
# the next one that appears in the same place.
for k in "${KNOWN[@]}"; do
    still=0
    for seen in "${SEEN[@]:-}"; do [ "$seen" = "$k" ] && still=1 && break; done
    if [ "$still" -eq 0 ]; then
        echo "FAIL '$k' is on the known list but no longer occurs -- remove it, so the list keeps meaning what it says."
        FAIL=1
    fi
done

if [ "$FAIL" -ne 0 ]; then
    echo "refusal check FAILED"
    exit 1
fi
echo "  ${#KNOWN[@]} known call sites carry this hazard already; none was added."
echo "refusal check OK (every exiting function is called where its exit is reached, or its status is checked)"
