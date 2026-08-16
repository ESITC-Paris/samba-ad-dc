#!/bin/sh
# Prove the Dockerfile's ARG defaults still equal the pins in versions.yaml.
#
#   sh scripts/check-pins-consistency.sh          # check, exit 1 on drift
#   sh scripts/check-pins-consistency.sh --print <key>
#
# versions.yaml is the pin contract (SPEC §9bis): the release watcher edits
# it, and the Dockerfile carries the same values as ARG defaults so that a
# bare `docker build .` with no --build-arg reproduces the pinned build.
# Two places holding one value is a drift hazard, so CI asserts equality
# on every pair before it builds anything.
#
# Pairs asserted (default_branch entry of versions.yaml):
#
#   ARG SAMBA_VERSION          == branches[b].samba_version   (every occurrence)
#   ARG SAMBA_TARBALL_SHA256   == branches[b].tarball_sha256
#   ARG BUILDER_BASE           == branches[b].base.builder
#   ARG RUNTIME_BASE           == branches[b].base.runtime
#   ARG BASE_DIGEST            == digest(RUNTIME_BASE) == digest(base.runtime)
#
# BASE_DIGEST exists only to feed the org.opencontainers.image.base.digest
# label; it duplicates the digest already inside RUNTIME_BASE, which is
# exactly why it is checked against it rather than trusted.
#
# YAML parsing: PyYAML when python3 can import it (true on the GitHub
# runners and on most dev boxes) — a real parser, so quoting or ordering
# changes in versions.yaml cannot fool it. When PyYAML is absent we fall
# back to a deliberately narrow awk reader that only understands the flat
# two-level shape versions.yaml is committed in; it fails loudly (below)
# if any expected key comes back empty, so a shape change is a red build
# rather than a silent pass.
set -eu

here=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
DOCKERFILE=${DOCKERFILE:-$here/Dockerfile}
VERSIONS=${VERSIONS:-$here/versions.yaml}

[ -f "$DOCKERFILE" ] || { echo "no Dockerfile: $DOCKERFILE" >&2; exit 2; }
[ -f "$VERSIONS" ] || { echo "no versions catalog: $VERSIONS" >&2; exit 2; }

# --------------------------------------------------------------------------
# versions.yaml -> key=value lines
# --------------------------------------------------------------------------
read_versions_python() {
    python3 - "$VERSIONS" <<'PY'
import sys

import yaml

with open(sys.argv[1], "r", encoding="utf-8") as fh:
    doc = yaml.safe_load(fh)

branch = doc["default_branch"]
entry = doc["branches"][branch]
print("default_branch=%s" % branch)
print("samba_version=%s" % entry["samba_version"])
print("tarball_sha256=%s" % entry["tarball_sha256"])
print("base_builder=%s" % entry["base"]["builder"])
print("base_runtime=%s" % entry["base"]["runtime"])
PY
}

read_versions_awk() {
    branch=$(sed -n 's/^default_branch:[[:space:]]*"\{0,1\}\([^"#[:space:]]*\)"\{0,1\}.*/\1/p' \
        "$VERSIONS" | head -1)
    [ -n "$branch" ] || { echo "cannot read default_branch from $VERSIONS" >&2; exit 2; }
    echo "default_branch=$branch"
    awk -v want="$branch" '
        function val(s) {
            sub(/^[^:]*:[[:space:]]*/, "", s)
            sub(/[[:space:]]*#.*$/, "", s)
            gsub(/^[[:space:]]+|[[:space:]]+$/, "", s)
            gsub(/^"|"$/, "", s)
            return s
        }
        /^  [^ #]/ { key = $1; gsub(/[":]/, "", key); inblk = (key == want); inbase = 0; next }
        !inblk { next }
        /^    base:/ { inbase = 1; next }
        /^    [^ #]/ { inbase = 0 }
        /^    samba_version:/  { print "samba_version=" val($0) }
        /^    tarball_sha256:/ { print "tarball_sha256=" val($0) }
        inbase && /^      builder:/ { print "base_builder=" val($0) }
        inbase && /^      runtime:/ { print "base_runtime=" val($0) }
    ' "$VERSIONS"
}

if python3 -c 'import yaml' >/dev/null 2>&1; then
    versions=$(read_versions_python)
    parser=PyYAML
else
    versions=$(read_versions_awk)
    parser="awk fallback (PyYAML unavailable)"
fi

get() {
    printf '%s\n' "$versions" | sed -n "s/^$1=//p" | head -1
}

v_branch=$(get default_branch)
v_samba_version=$(get samba_version)
v_tarball_sha256=$(get tarball_sha256)
v_base_builder=$(get base_builder)
v_base_runtime=$(get base_runtime)

for pair in \
    "default_branch:$v_branch" \
    "samba_version:$v_samba_version" \
    "tarball_sha256:$v_tarball_sha256" \
    "base.builder:$v_base_builder" \
    "base.runtime:$v_base_runtime"
do
    if [ -z "${pair#*:}" ]; then
        echo "versions.yaml: no value for ${pair%%:*} (parser: $parser)" >&2
        exit 2
    fi
done

# --------------------------------------------------------------------------
# --print <key>: expose a resolved pin to callers (CI smoke tests)
# --------------------------------------------------------------------------
if [ "${1:-}" = "--print" ]; then
    key=${2:-}
    value=$(get "$key")
    if [ -z "$value" ]; then
        echo "unknown pin: ${key:-<missing>}" >&2
        echo "known: default_branch samba_version tarball_sha256 base_builder base_runtime" >&2
        exit 2
    fi
    printf '%s\n' "$value"
    exit 0
fi

# --------------------------------------------------------------------------
# Dockerfile ARG defaults
# --------------------------------------------------------------------------
errors=0

fail() {
    errors=$((errors + 1))
    echo "MISMATCH: $1" >&2
}

# Every `ARG <name>=<default>` occurrence, one per line (the value may
# legitimately appear in more than one build stage).
arg_defaults() {
    sed -n "s/^ARG $1=//p" "$DOCKERFILE" | sed 's/[[:space:]]*$//'
}

# digest_of <ref> -> the sha256:... part, or empty when the ref is not pinned.
digest_of() {
    case "$1" in
        *@sha256:*) printf '%s\n' "${1#*@}" ;;
        *) printf '\n' ;;
    esac
}

# check_arg <ARG name> <expected value> — asserts the ARG exists and that
# every occurrence of it carries the expected default.
check_arg() {
    name=$1
    expected=$2
    found=$(arg_defaults "$name")
    if [ -z "$found" ]; then
        fail "Dockerfile has no 'ARG $name=<default>'"
        return
    fi
    bad=0
    while IFS= read -r actual; do
        [ -n "$actual" ] || continue
        if [ "$actual" != "$expected" ]; then
            fail "ARG $name=$actual != versions.yaml $expected"
            bad=1
        fi
    done <<EOF
$found
EOF
    if [ "$bad" -eq 0 ]; then
        printf 'ok  ARG %-20s = %s\n' "$name" "$expected"
    fi
}

echo "versions.yaml branch $v_branch (parser: $parser)"

check_arg SAMBA_VERSION "$v_samba_version"
check_arg SAMBA_TARBALL_SHA256 "$v_tarball_sha256"
check_arg BUILDER_BASE "$v_base_builder"
check_arg RUNTIME_BASE "$v_base_runtime"

# The catalog's base refs must themselves be digest-pinned; a floating tag
# here would make every other assertion meaningless.
runtime_digest=$(digest_of "$v_base_runtime")
if [ -z "$runtime_digest" ]; then
    fail "versions.yaml base.runtime ($v_base_runtime) is not digest-pinned"
fi
if [ -z "$(digest_of "$v_base_builder")" ]; then
    fail "versions.yaml base.builder ($v_base_builder) is not digest-pinned"
fi

# BASE_DIGEST (the OCI label) must equal the digest already carried by
# RUNTIME_BASE in the Dockerfile *and* by base.runtime in the catalog.
dockerfile_runtime=$(arg_defaults RUNTIME_BASE | head -1)
dockerfile_runtime_digest=$(digest_of "$dockerfile_runtime")
if [ -n "$runtime_digest" ]; then
    check_arg BASE_DIGEST "$runtime_digest"
    if [ -n "$dockerfile_runtime_digest" ] && \
       [ "$dockerfile_runtime_digest" != "$runtime_digest" ]; then
        fail "Dockerfile RUNTIME_BASE digest ($dockerfile_runtime_digest) != \
versions.yaml base.runtime digest ($runtime_digest)"
    fi
fi

if [ "$errors" -ne 0 ]; then
    echo "" >&2
    echo "$errors pin(s) out of sync between $DOCKERFILE and $VERSIONS" >&2
    exit 1
fi

echo "all pins consistent"
