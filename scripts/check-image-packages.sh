#!/bin/sh
# Prove SPEC §5.1 for a built image: every Debian package installed in it
# is accounted for.
#
#   sh scripts/check-image-packages.sh <image> [runtime-base] [manifest]
#
# An image's package set must be exactly
#
#   packages(base image)  ∪  apt closure of runtime-packages.txt
#
# The check computes the first two sets by asking dpkg inside the image
# and inside the bare base, and the third by asking apt itself, inside the
# bare base, what installing the manifest would pull in
# (`apt-get install --dry-run`, whose `Inst` lines are the real resolution
# including virtual packages and alternatives — a recursive
# `apt-cache depends` walk both over- and under-approximates that).
#
# Anything in the image that none of those sets explains is reported and
# the script exits 1.
#
# CI flake note: the apt closure is resolved against the LIVE Debian package
# index at the moment the check runs, not against a snapshot. An upstream
# dependency change (a package gaining or losing a Depends) can therefore
# flip this check red or green with no change in this repository. A failure
# that appears without a relevant commit is a signal to look upstream first,
# not automatically a regression in the change under test.
set -eu

IMAGE=${1:-}
if [ -z "$IMAGE" ]; then
    echo "usage: $0 <image> [runtime-base-ref] [manifest]" >&2
    exit 2
fi

here=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
MANIFEST=${3:-$here/runtime-packages.txt}
[ -f "$MANIFEST" ] || { echo "no manifest: $MANIFEST" >&2; exit 2; }

# The base must be the exact digest-pinned ref the runtime stage uses;
# take it from the Dockerfile so the two can never drift.
BASE=${2:-}
if [ -z "$BASE" ]; then
    BASE=$(sed -n 's/^ARG RUNTIME_BASE=\(.*\)$/\1/p' "$here/Dockerfile" | head -1)
fi
[ -n "$BASE" ] || { echo "cannot determine RUNTIME_BASE" >&2; exit 2; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# `--entrypoint` so the check keeps working once Phase 2 installs one.
pkgs_of() {
    docker run --rm --entrypoint dpkg-query "$1" \
        -W -f '${Package} ${db:Status-Status}\n' \
        | awk '$2 == "installed" { print $1 }' | sort -u
}

# The manifest the image carries is the operator-visible claim (SPEC §5.1);
# if it is not the file this check reasons about, the whole check is about
# a different image than the one shipped. Hash both inside a container so
# the comparison does not depend on the host having sha256sum.
echo "==> shipped manifest matches $MANIFEST"
want=$(docker run --rm -i --entrypoint sha256sum "$IMAGE" < "$MANIFEST" | awk '{ print $1 }')
got=$(docker run --rm --entrypoint sha256sum "$IMAGE" \
        /usr/share/samba-ad-dc/runtime-packages.txt 2>/dev/null | awk '{ print $1 }') || {
    echo "FAIL: $IMAGE ships no /usr/share/samba-ad-dc/runtime-packages.txt" >&2
    exit 1
}
if [ "$want" != "$got" ]; then
    echo "FAIL: the manifest inside $IMAGE differs from $MANIFEST" >&2
    echo "      repo:   $want" >&2
    echo "      image:  $got" >&2
    exit 1
fi

echo "==> package set of image: $IMAGE"
pkgs_of "$IMAGE" > "$work/image"
echo "==> package set of base:  $BASE"
pkgs_of "$BASE" > "$work/base"

# Requested packages, comments stripped.
sed 's/#.*//' "$MANIFEST" | tr -s '[:space:]' '\n' | grep -v '^$' | sort -u \
    > "$work/requested"

echo "==> apt closure of $(wc -l < "$work/requested" | tr -d ' ') requested packages"
# shellcheck disable=SC2046  # word splitting of the package list is wanted
docker run --rm -i --entrypoint sh "$BASE" -c '
    set -eu
    apt-get update -qq >/dev/null
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        --dry-run $(cat) 2>&1
' < "$work/requested" > "$work/dryrun" || {
    echo "FAIL: apt could not resolve the manifest against the base image:" >&2
    tail -20 "$work/dryrun" >&2
    exit 1
}
awk '/^Inst /{ print $2 }' "$work/dryrun" | sort -u > "$work/closure"

# Everything the image has that the base did not.
comm -23 "$work/image" "$work/base" > "$work/added"
# ... minus what the manifest and its closure explain.
sort -u "$work/closure" "$work/requested" > "$work/explained"
comm -23 "$work/added" "$work/explained" > "$work/unexplained"

added=$(wc -l < "$work/added" | tr -d ' ')
echo "==> $added package(s) added on top of the base image"

if [ -s "$work/unexplained" ]; then
    echo
    echo "FAIL: packages installed in $IMAGE that neither the base image nor"
    echo "      $MANIFEST (or its apt closure) explains — SPEC §5.1:"
    sed 's/^/        /' "$work/unexplained"
    exit 1
fi

# A requested package that never made it into the image means the manifest
# lies about the image; that is equally a §5.1 failure.
comm -23 "$work/requested" "$work/image" > "$work/missing"
if [ -s "$work/missing" ]; then
    echo
    echo "FAIL: packages listed in $MANIFEST but absent from $IMAGE:"
    sed 's/^/        /' "$work/missing"
    exit 1
fi

echo "OK: every package in $IMAGE is either in the base image or in the"
echo "    apt closure of $MANIFEST, and every listed package is present."
