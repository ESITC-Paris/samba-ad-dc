#!/bin/sh
# THE definition of the package-closure hash (SPEC §9bis.1.c).
#
#   sh scripts/pkg-closure-hash.sh <runtime-base-ref@digest> [manifest]
#
# Prints 16 hex characters on stdout and nothing else, so callers can
# capture it with `$( )`. That value is what versions.yaml carries as
# `pkg_index_hash`, what .build-state.json remembers per branch, what CI
# injects as --build-arg PKG_INDEX_HASH, and what the watcher compares
# against to decide whether a rebuild is owed. One implementation, so the
# seed, the probe and a local check can never disagree about what "the
# package closure changed" means.
#
# The hash is taken over the UNION of two apt dry-runs performed inside
# the pinned base image, against the package index as it exists at the
# moment of the probe:
#
#   1. `apt-get -y --dry-run upgrade`
#      What a security update to a package the BASE IMAGE already carries
#      looks like. The runtime stage now runs this same upgrade, so it is
#      a genuine build input.
#   2. `apt-get -y --no-install-recommends --dry-run install <manifest>`
#      The closure the manifest pulls in on top of the base.
#
# Both are needed, and neither subsumes the other. `install` alone never
# mentions an already-installed base package — apt does not upgrade a
# package that already satisfies the request — so a CVE fix to gzip or
# perl-base would leave the hash untouched while changing what ships,
# which is precisely the blind spot §9bis.1.c forbids. `upgrade` alone
# says nothing about packages not installed yet, i.e. the whole manifest.
#
# Rendering: each `Inst <name> [<old>] (<new> ...)` line becomes
# `<name> <version>`, where <version> is the version apt would END UP
# with — the first parenthesised field, brackets stripped. Version and
# not just name on purpose: a rebuild is owed when a package moves to a
# new version, and a name-only digest is blind to exactly the event this
# hash exists to catch. Sorted and deduplicated so apt's resolution order
# — which is not stable across index refreshes — cannot move the hash on
# its own. sha256, first 16 hex: short enough to read in a diff and in a
# layer-busting `echo`, wide enough that a collision is not a concern for
# a set of a few hundred lines.
#
# The probe reads the LIVE package index, so the same base digest yields
# different hashes on different days. That is the point: the hash tracks
# the index, the digest tracks the base. Neither replaces the other, and
# a build records both.
set -eu

BASE=${1:-}
if [ -z "$BASE" ]; then
    echo "usage: $0 <runtime-base-ref@digest> [manifest]" >&2
    exit 2
fi

here=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
MANIFEST=${2:-$here/runtime-packages.txt}
[ -f "$MANIFEST" ] || { echo "no manifest: $MANIFEST" >&2; exit 2; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Requested packages, comments stripped — the same reduction the
# Dockerfile's `sed 's/#.*//'` performs on the shipped manifest, so the
# hash is computed over the list that is actually installed.
sed 's/#.*//' "$MANIFEST" | tr -s '[:space:]' '\n' | grep -v '^$' | sort -u \
    > "$work/requested"
[ -s "$work/requested" ] || {
    echo "FATAL: $MANIFEST reduced to zero packages — the hash would be" >&2
    echo "       computed over the upgrade set alone and silently wrong" >&2
    exit 1
}

# Both dry-runs in ONE container against ONE `apt-get update`: two
# containers could straddle a mirror refresh and produce a hash over two
# different package indexes, which is a value that never existed.
# stderr is kept with the output so an apt failure is readable; the
# `Inst` extraction below is anchored, so warnings cannot be mistaken for
# resolution lines.
# shellcheck disable=SC2016  # $(cat) is for the container's shell, not ours
if ! docker run --rm -i --entrypoint sh "$BASE" -c '
    set -eu
    apt-get update -qq >/dev/null
    requested=$(cat)
    apt-get -y --dry-run upgrade 2>&1
    # shellcheck disable=SC2086
    DEBIAN_FRONTEND=noninteractive apt-get -y --no-install-recommends \
        --dry-run install $requested 2>&1
' < "$work/requested" > "$work/dryruns" 2>&1; then
    echo "FATAL: apt could not resolve against $BASE:" >&2
    tail -20 "$work/dryruns" >&2
    exit 1
fi

# `Inst zlib1g [1:1.3.dfsg+really1.3.1-1] (1:1.3.dfsg+really1.3.1-1+b1 Debian:...)`
#                    $2                              $3 or $4
# The end-state version is the first parenthesised field: $3 when the
# package is new (no `[old]` bracket), $4 when it is an upgrade.
awk '
    /^Inst / {
        name = $2
        ver = ""
        for (i = 3; i <= NF; i++) {
            if (substr($i, 1, 1) == "(") { ver = substr($i, 2); break }
        }
        sub(/\)$/, "", ver)
        if (ver == "") next
        print name " " ver
    }
' "$work/dryruns" | sort -u > "$work/closure"

[ -s "$work/closure" ] || {
    echo "FATAL: neither dry-run produced an Inst line against $BASE — apt's" >&2
    echo "       output format changed, or the manifest resolved to nothing;" >&2
    echo "       hashing an empty set would be a stable, meaningless value" >&2
    tail -20 "$work/dryruns" >&2
    exit 1
}

# sha256sum is coreutils on Linux and absent on macOS, where the tool is
# `shasum -a 256`; both print the digest as the first field.
if command -v sha256sum >/dev/null 2>&1; then
    sum=$(sha256sum < "$work/closure")
else
    sum=$(shasum -a 256 < "$work/closure")
fi

printf '%s\n' "$sum" | cut -c1-16
