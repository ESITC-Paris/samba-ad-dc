#!/bin/sh
# Prove the shared security-gate block is byte-identical in ci.yml and
# release.yml.
#
#   sh scripts/check-gate-block.sh [ci.yml] [release.yml]
#
# SPEC §5.3/§5.4 gate a pull request and a tag with the same checks. The
# cheapest way to guarantee that is one block of steps, copied verbatim
# into both workflows — which is exactly the kind of duplication that
# rots, silently, the first time someone tightens one copy. This is the
# check that makes the copy a contract: the range between
#
#   # --- security gates (SPEC §5.3, §5.4) ---
#   # --- end security gates ---
#
# must be the same bytes in both files, or this exits 1 with the diff.
#
# A missing marker is also a failure: a block that cannot be located is
# not a block that was proven equal.
set -eu

here=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
CI=${1:-$here/.github/workflows/ci.yml}
RELEASE=${2:-$here/.github/workflows/release.yml}

BEGIN='# --- security gates (SPEC §5.3, §5.4) ---'
END='# --- end security gates ---'

fail() {
    echo "gate block: $1" >&2
    exit 1
}

# `sed -n '/a/,/b/p'` prints to end of file when the closing marker is
# absent, so both markers are counted first rather than trusted.
extract() {
    file=$1
    out=$2
    [ -f "$file" ] || fail "$file does not exist"
    for marker in "$BEGIN" "$END"; do
        count=$(grep -cF "$marker" "$file" || true)
        [ "$count" = "1" ] || \
            fail "$file contains the marker '$marker' $count times, expected 1"
    done
    awk -v begin="$BEGIN" -v end="$END" '
        index($0, begin) { inside = 1 }
        inside           { print }
        index($0, end)   { inside = 0 }
    ' "$file" > "$out"
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

extract "$CI" "$tmp/ci"
extract "$RELEASE" "$tmp/release"

if ! diff -u "$tmp/ci" "$tmp/release" > "$tmp/diff"; then
    echo "gate block: the security-gate block differs between" >&2
    echo "  $CI" >&2
    echo "  $RELEASE" >&2
    echo "Both must run the same gates: a tag is gated by exactly what a" >&2
    echo "pull request was gated by (SPEC §5.3, §5.4)." >&2
    cat "$tmp/diff" >&2
    exit 1
fi

echo "gate block: identical in both workflows ($(wc -l < "$tmp/ci" | tr -d ' ') lines)"
