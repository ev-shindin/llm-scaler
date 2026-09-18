#!/usr/bin/env bash
#
# The GPU product label keys are written down three times, and all three have to
# agree.
#
#   internal/constants/constants.go   ProductLabel + ProductLabelAliases -- what
#                                     the CONTROLLER resolves a node through
#   deploy/lib/accelerator_labels.py  what warmpool.sh plan and sizing read
#   deploy/warmpool.sh                what `create` PINS a pool on
#   deploy/lib/accelerator_nodes.sh   where the per-node DaemonSets (prepull,
#                                     weights) land by default
#
# They cannot share one source: the controller is Go, the planning tools run
# without a Go toolchain, and the create path is shell that runs from a release
# tarball. So they are copies, and this fails when they drift.
#
# Drift here does not look like a bug. A key present in the controller and
# missing from the create path produced a pool pinned on a label no node
# carried: two Pods Pending forever on a cluster with 24 free GPUs, and a
# scheduler message that reads as a full cluster. A key missing from the
# planning tools produced `5 node(s) of: unknown  8x0 GiB GPU` about five 8-GPU
# H200 nodes. Both were found by running them on a cluster that was not GKE or
# GFD, which is not something CI does.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_FILE="$ROOT/internal/constants/constants.go"
PY_FILE="$ROOT/deploy/lib/accelerator_labels.py"
SH_FILE="$ROOT/deploy/warmpool.sh"
NODES_FILE="$ROOT/deploy/lib/accelerator_nodes.sh"

for f in "$GO_FILE" "$PY_FILE" "$SH_FILE" "$NODES_FILE"; do
    [ -f "$f" ] || { echo "missing $f" >&2; exit 1; }
done

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Go: the ProductLabel of each vendor, plus every string inside an
# ProductLabelAliases block.
#
# Extract from the line FIRST, then update the block state: an aliases list
# written on one line both opens and closes it, and closing first skipped the
# only key on it.
tr -d '\r' < "$GO_FILE" | awk '
    {
        line = $0
        opens = (line ~ /ProductLabelAliases:/)
        if (line ~ /ProductLabel:/ || opens || inblock) {
            while (match(line, /"[^"]+"/)) {
                key = substr(line, RSTART + 1, RLENGTH - 2)
                if (key ~ /\//) print key
                line = substr(line, RSTART + RLENGTH)
            }
        }
        if (opens)                    inblock = ($0 !~ /}/)
        else if (inblock && $0 ~ /}/) inblock = 0
    }
' | sort -u > "$tmp/go"

# Python: the strings inside PRODUCT_KEYS = [ ... ].
tr -d '\r' < "$PY_FILE" | awk '
    /^PRODUCT_KEYS = \[/ { inblock = 1; next }
    inblock && /^\]/     { inblock = 0 }
    inblock {
        line = $0
        while (match(line, /"[^"]+"/)) {
            key = substr(line, RSTART + 1, RLENGTH - 2)
            if (key ~ /\//) print key
            line = substr(line, RSTART + RLENGTH)
        }
    }
' | sort -u > "$tmp/py"

# Shell: the strings inside the jq key array in accelerator_label_key.
#
# Same order for the same reason: the last key shares its line with the `]`
# that ends the array.
tr -d '\r' < "$SH_FILE" | awk '
    /^accelerator_label_key\(\)/ { infunc = 1; next }
    infunc {
        line = $0
        while (match(line, /"[^"]+"/)) {
            key = substr(line, RSTART + 1, RLENGTH - 2)
            if (key ~ /\//) print key
            line = substr(line, RSTART + RLENGTH)
        }
        if ($0 ~ /as \$keys/) infunc = 0
    }
' | sort -u > "$tmp/sh"

# The DaemonSet lib: the strings inside ACCELERATOR_PRODUCT_KEYS=( ... ).
tr -d '\r' < "$NODES_FILE" | awk '
    /^ACCELERATOR_PRODUCT_KEYS=\(/ { inblock = 1; next }
    inblock && /^\)/            { inblock = 0 }
    inblock {
        line = $0
        while (match(line, /"[^"]+"/)) {
            key = substr(line, RSTART + 1, RLENGTH - 2)
            if (key ~ /\//) print key
            line = substr(line, RSTART + RLENGTH)
        }
    }
' | sort -u > "$tmp/nodes"

# An empty extraction and an identical one produce the same silent pass, so
# every side has to have found something before any of them are compared.
status=0
for side in go py sh nodes; do
    n="$(wc -l < "$tmp/$side")"
    if [ "$n" -lt 2 ]; then
        echo "ERROR: extracted $n key(s) from the $side side -- the parser found" >&2
        echo "       nothing, which would compare equal to any other empty side." >&2
        status=1
    fi
done
[ "$status" -eq 0 ] || exit 1

report() {
    # $1 name, $2 file, $3 other name, $4 other file
    missing="$(comm -23 "$2" "$4")"
    if [ -n "$missing" ]; then
        echo "ERROR: keys in $1 but not in $3:" >&2
        printf '  %s\n' $missing >&2
        status=1
    fi
}

report "constants.go" "$tmp/go" "accelerator_labels.py" "$tmp/py"
report "accelerator_labels.py" "$tmp/py" "constants.go" "$tmp/go"
report "constants.go" "$tmp/go" "warmpool.sh" "$tmp/sh"
report "warmpool.sh" "$tmp/sh" "constants.go" "$tmp/go"
report "constants.go" "$tmp/go" "accelerator_nodes.sh" "$tmp/nodes"
report "accelerator_nodes.sh" "$tmp/nodes" "constants.go" "$tmp/go"

if [ "$status" -ne 0 ]; then
    cat >&2 <<'MSG'

Add the key to all three, or remove it from all three. Only list a key a
provider DOCUMENTS as naming the GPU model -- a plausible-looking one that
carries something else silently mis-attributes every node that has it.
MSG
    exit 1
fi

echo "accelerator label keys agree ($(wc -l < "$tmp/go") keys)"
