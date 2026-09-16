#!/bin/sh
# Verify an upstream Samba release tarball the way the image build does.
#
#   sh scripts/verify-upstream-tarball.sh <X.Y.Z> [outdir]
#
# Prints exactly one line on stdout — `sha256=<hex>`, the checksum of the
# shipped samba-X.Y.Z.tar.gz — and exits 0 only when the OpenPGP signature
# verifies against the pinned release key; exits 1 otherwise. Progress and
# every refusal go to stderr, so a caller can capture the line and cut the
# `sha256=` prefix off it without filtering anything else out.
#
# Why this exists: the sha256 in versions.yaml is the only pin that cannot
# be read off a registry, and the one a human is tempted to copy from a
# web page. This script is the procedure that produces it — the same
# gunzip-then-verify chain the Dockerfile builder stage runs — so a pin
# lands in the catalog only after the signature that justifies it was
# checked locally. It is NOT part of the image build: the build repeats
# the verification itself, against the same pinned fingerprint.
#
# With no outdir the download lands in a temporary directory that is
# removed on exit; pass one to keep the artifacts (e.g. to inspect a
# tarball that failed).
set -eu

# Primary key of the Samba Distribution Verification Key. Mirrors
# keys/PINNING.md and the Dockerfile's SAMBA_SIGNING_FINGERPRINT; rotating
# it is a reviewed change in all three places (SPEC §4.4).
FINGERPRINT=81F5E2832BD2545A1897B713AA99442FB680B620

MIRROR=https://download.samba.org/pub/samba/stable

here=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
KEYFILE=${KEYFILE:-$here/keys/samba-release-key.asc}

version=${1:-}
outdir=${2:-}

case "$version" in
    [0-9]*.[0-9]*.[0-9]*) ;;
    *) echo "usage: $0 <X.Y.Z> [outdir]" >&2; exit 2 ;;
esac
[ -f "$KEYFILE" ] || { echo "no release key: $KEYFILE" >&2; exit 2; }

# A throwaway GNUPGHOME: importing into the caller's keyring would both
# mutate their trust state and make the result depend on it. 0700 because
# gpg refuses a world-readable home.
gnupghome=$(mktemp -d "${TMPDIR:-/tmp}/samba-verify-gnupg.XXXXXX")
chmod 700 "$gnupghome"

keep_outdir=1
if [ -z "$outdir" ]; then
    outdir=$(mktemp -d "${TMPDIR:-/tmp}/samba-verify.XXXXXX")
    keep_outdir=0
fi
mkdir -p "$outdir"

cleanup() {
    gpgconf --homedir "$gnupghome" --kill all >/dev/null 2>&1 || true
    rm -rf "$gnupghome"
    [ "$keep_outdir" -eq 1 ] || rm -rf "$outdir"
}
trap cleanup EXIT INT TERM

# sha256sum is the coreutils name; shasum -a 256 is what a BSD/macOS host
# has. Both print `<hex>  <file>`.
sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | cut -d' ' -f1
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | cut -d' ' -f1
    else
        echo "no sha256sum and no shasum on PATH" >&2
        exit 2
    fi
}

tgz="$outdir/samba-${version}.tar.gz"
tar="$outdir/samba-${version}.tar"
asc="$outdir/samba-${version}.tar.asc"
status="$outdir/gpg-status.txt"

echo "fetching samba-${version} from ${MIRROR}" >&2
curl -fsSL --retry 3 --retry-delay 5 -o "$tgz" "${MIRROR}/samba-${version}.tar.gz"
curl -fsSL --retry 3 --retry-delay 5 -o "$asc" "${MIRROR}/samba-${version}.tar.asc"

# The checksum is taken over the COMPRESSED artifact, before gunzip: that
# is what versions.yaml pins and what the build downloads.
sha=$(sha256_of "$tgz")

# `gunzip -c` rather than `gunzip`: the .gz is kept so a caller that asked
# for an outdir gets the artifact the checksum above describes.
gunzip -c "$tgz" > "$tar"

gpg --homedir "$gnupghome" --batch --quiet --import "$KEYFILE"

# Same shape as the Dockerfile: status to a FILE, never through a pipe, so
# `grep -q` cannot SIGPIPE gpg; a positive assertion (VALIDSIG on the
# pinned fingerprint) and a negative one (no rejected status), the latter
# written as `if ... exit 1` because `! grep` is exempt from `set -e`.
if ! gpg --homedir "$gnupghome" --batch --status-fd 1 --verify \
        "$asc" "$tar" > "$status" 2>/dev/null; then
    echo "FATAL: gpg --verify failed for samba-${version}" >&2
    cat "$status" >&2
    exit 1
fi

if ! grep -q "VALIDSIG .* ${FINGERPRINT}\$" "$status"; then
    echo "FATAL: no VALIDSIG on the pinned fingerprint ${FINGERPRINT}" >&2
    cat "$status" >&2
    exit 1
fi

if grep -qE '^\[GNUPG:\] (EXPKEYSIG|REVKEYSIG|ERRSIG|BADSIG)' "$status"; then
    echo "FATAL: gpg reported a rejected signature status:" >&2
    grep -E '^\[GNUPG:\] (EXPKEYSIG|REVKEYSIG|ERRSIG|BADSIG)' "$status" >&2
    exit 1
fi

echo "ok: samba-${version}.tar.asc verifies against ${FINGERPRINT}" >&2
printf 'sha256=%s\n' "$sha"
