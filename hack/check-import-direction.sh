#!/usr/bin/env bash
#
# Fails when a pipeline package imports one above it.
#
# The engine-structure proposal (https://github.com/ev-shindin/llm-scaler/pull/87)
# gives the dependency order of the pipeline: engine > policy > plan > analyze > signals > collect / actuate >
# decision > domain. A package may import only what is below it in that
# order, plus the infrastructure leaves (config, constants, metrics, logging,
# prometheus, accelerator, gpunodes, kueue, inferenceengine, variant,
# datastore, utils/...), which are importable from anywhere and are not
# checked here. Packages outside the listed layers (controller, warmpool,
# cmd) sit above the pipeline and are not checked either.
#
# Non-test imports only: a test may import anything.
#
# The layers name TODAY'S packages; when a package moves under the proposal
# its new path replaces the old one here, and the rule stays.
set -u
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

MODULE="$(sed -n 's/^module //p' go.mod)"
[ -n "$MODULE" ] || { echo "FATAL: cannot read the module path from go.mod" >&2; exit 1; }
command -v go >/dev/null 2>&1 || { echo "FATAL: go is required to run this check" >&2; exit 1; }

# Higher number = higher layer. A package may import a package with a
# STRICTLY lower number, never a higher or an equal one in another package
# of the same layer? Equal is allowed: analyzers import each other's
# neighbours (aggregation) and the two drivers share nothing, but siblings
# within a layer are not the concern of this check.
layer_of() {
    case "$1" in
        internal/engines/steadystate|internal/engines/scalefromzero) echo 7 ;;
        internal/policy) echo 6 ;;
        internal/engines/allocation|internal/engines/allocation/*) echo 5 ;;
        internal/engines/analyzers/*|internal/engines/executor|internal/engines/variantmeta) echo 4 ;;
        internal/engines/aggregation|internal/engines/common|internal/signals/*) echo 3 ;;
        internal/collector|internal/collector/*) echo 2 ;;
        internal/actuator|internal/scaler|internal/registry) echo 2 ;;
        internal/decision) echo 1 ;;
        internal/domain) echo 0 ;;
        *) echo "" ;;
    esac
}

# One line per package: "<path> <import> <import> ...", module prefix stripped.
listing="$(go list -f '{{.ImportPath}} {{join .Imports " "}}' ./internal/... 2>/dev/null | sed "s|$MODULE/||g")" || {
    echo "FATAL: go list failed" >&2; exit 1
}
[ -n "$listing" ] || { echo "FATAL: go list returned nothing; the check ran against no packages" >&2; exit 1; }

checked=0; bad=0
while read -r pkg imports; do
    from="$(layer_of "$pkg")"
    [ -n "$from" ] || continue
    checked=$((checked + 1))
    for imp in $imports; do
        case "$imp" in internal/*) ;; *) continue ;; esac
        to="$(layer_of "$imp")"
        [ -n "$to" ] || continue
        if [ "$to" -gt "$from" ]; then
            echo "UPWARD  $pkg (layer $from) imports $imp (layer $to)"
            bad=$((bad + 1))
        fi
    done
done <<< "$listing"

[ "$checked" -gt 0 ] || { echo "FATAL: no layered package was found; the layer table no longer matches the tree" >&2; exit 1; }
if [ "$bad" -gt 0 ]; then
    echo "$bad upward import(s) across $checked layered package(s); see the engine-structure proposal (PR #87)" >&2
    exit 1
fi
echo "import direction OK ($checked layered packages, no upward edge)"
