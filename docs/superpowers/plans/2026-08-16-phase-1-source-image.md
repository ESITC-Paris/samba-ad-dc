# Phase 1 — Source-Built Image Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps
> use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `docker build` of this repo produces a runnable Samba AD DC
rootfs image (FHS layout, bundled Heimdal) from the GPG-verified 4.24.6
source tarball on digest-pinned Debian trixie-slim, with all mandatory
OCI labels — no entrypoint logic yet.

**Architecture:** Two-stage Dockerfile. `builder`: digest-pinned
trixie-slim + build toolchain, downloads the tarball from
download.samba.org, verifies GPG (vendored key from Phase 0) AND the
sha256 pinned in `versions.yaml`, configures with `--enable-fhs`,
compiles, installs into a DESTDIR. `runtime`: independently digest-pinned
trixie-slim + the justified runtime package list (installed in a single
layer cache-busted by `ARG PKG_INDEX_HASH`), copies the DESTDIR, carries
labels. The runtime package list is DERIVED (ldd closure → dpkg owners),
not guessed, and a check script proves the image contains nothing outside
that list plus its apt dependency closure.

**Tech Stack:** Docker BuildKit, debian:trixie-slim (digest-pinned per
stage), Samba 4.24.6 (waf build), gpg, python3.

**Spec:** `SPEC.md` v1.2 — §§2.5, 4.1, 4.4, 5.1, 9.3, B.1. Roadmap:
`docs/superpowers/plans/2026-08-16-samba-ad-dc-roadmap.md` Phase 1.

## Global Constraints

- Base pinned by digest in BOTH stages, independently (§4.1).
- Tarball verified by GPG against `keys/samba-release-key.asc` AND by
  the sha256 recorded in `versions.yaml` (§4.4). Samba signs the
  UNCOMPRESSED tarball: gunzip before `gpg --verify`.
- Bundled Heimdal — the build MUST NOT link MIT krb5 (B.1); FHS layout.
- `ARG PKG_INDEX_HASH` sits immediately before the single runtime
  `apt-get install` layer and nothing else (§9.3).
- Every runtime package justified in `runtime-packages.txt` (§5.1).
- Mandatory OCI labels (§2.5): source, version, revision, licenses
  (`GPL-3.0-or-later`), description, created, base.name, base.digest,
  plus `org.esitc-paris.spec-version=1.2`.
- No secrets in any layer (§5.3). All content in English.
- Local iteration is allowed (§4.5 allows dev builds); nothing is pushed
  to any registry in this phase.
- hadolint must pass (Phase 0 CI gate now bites on the Dockerfile).

---

### Task 1: versions.yaml + tarball checksum pinning

**Files:**
- Create: `versions.yaml`

**Interfaces:**
- Produces: `versions.yaml` schema consumed by the Dockerfile build args,
  CI (Phase 4), and the watcher (Phase 6):
  ```yaml
  default_branch: "4.24"
  branches:
    "4.24":
      samba_version: "4.24.6"
      revision: 1                # the N in X.Y.Z-rN
      tarball_sha256: "<hex>"
      base:
        builder: "debian:trixie-slim@sha256:<hex>"
        runtime: "debian:trixie-slim@sha256:<hex>"
      pkg_index_hash: "bootstrap" # replaced by the watcher from Phase 6 on
  ```

- [ ] **Step 1: Resolve the current trixie-slim digest**

```bash
docker buildx imagetools inspect debian:trixie-slim --format '{{json .Manifest.Digest}}'
```

Record the digest (one digest for the multi-arch manifest list; both
stages start from it today and diverge only when a bot PR updates one).

- [ ] **Step 2: Download and doubly verify the 4.24.6 tarball**

```bash
cd "$(mktemp -d)"
curl -fsSLO https://download.samba.org/pub/samba/stable/samba-4.24.6.tar.gz
curl -fsSLO https://download.samba.org/pub/samba/stable/samba-4.24.6.tar.asc
gpg --import /Users/nicolas/IT/docker/samba-ad-dc/keys/samba-release-key.asc
gunzip -k samba-4.24.6.tar.gz
gpg --verify samba-4.24.6.tar.asc samba-4.24.6.tar   # MUST: Good signature, pinned fingerprint
shasum -a 256 samba-4.24.6.tar.gz                    # record: goes into versions.yaml
```

If the signature is not good or the fingerprint differs from
`keys/PINNING.md`: STOP, report — do not pin a checksum you could not
verify the provenance of.

- [ ] **Step 3: Write `versions.yaml`** with the schema above and the
  recorded digest + sha256. `yamllint --strict versions.yaml` passes.

- [ ] **Step 4: Commit**

```bash
git add versions.yaml
git commit -m "feat: add versions catalog with Samba 4.24.6 pins (branch 4.24)"
```

---

### Task 2: Builder stage — compile Samba 4.24.6 (bundled Heimdal, FHS)

**Files:**
- Create: `Dockerfile` (builder stage only in this task; a temporary
  `FROM` smoke target at the end is fine)

**Interfaces:**
- Consumes: `versions.yaml` pins (hardcoded as `ARG` defaults matching
  it; CI later injects them from the file — a CI check in Task 4 asserts
  Dockerfile ARG defaults == versions.yaml).
- Produces: builder stage named `builder` with Samba installed under
  `/dest` (DESTDIR), used by Task 3's runtime stage.

- [ ] **Step 1: Write the builder stage**

Required structure (values from Task 1; adjust the build-dep list only
with a recorded justification in the commit message if configure
demands it):

```dockerfile
# syntax=docker/dockerfile:1
ARG BUILDER_BASE=debian:trixie-slim@sha256:<from versions.yaml>
ARG RUNTIME_BASE=debian:trixie-slim@sha256:<from versions.yaml>

FROM ${BUILDER_BASE} AS builder
ARG SAMBA_VERSION=4.24.6
ARG SAMBA_TARBALL_SHA256=<from versions.yaml>
# Build toolchain for the AD DC role only: no printing (cups), no
# cluster FS (ceph/gluster), no avahi, no systemd, no PAM, no QUIC,
# no winexe cross-compilers.
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      bison flex perl libparse-yapp-perl rpcsvc-proto pkgconf \
      gcc g++ make python3 python3-dev \
      python3-dnspython python3-markdown \
      libacl1-dev libarchive-dev libattr1-dev libblkid-dev libbsd-dev \
      libcap-dev libcrypt-dev libgnutls28-dev libgpgme11-dev libicu-dev \
      libjansson-dev libkeyutils-dev liblmdb-dev libpopt-dev \
      libreadline-dev libtasn1-6-dev libtirpc-dev zlib1g-dev \
      xsltproc docbook-xsl docbook-xml \
      ca-certificates curl gpg gpg-agent \
    && rm -rf /var/lib/apt/lists/*
COPY keys/samba-release-key.asc /tmp/samba-release-key.asc
RUN set -eux; \
    cd /tmp; \
    curl -fsSLO "https://download.samba.org/pub/samba/stable/samba-${SAMBA_VERSION}.tar.gz"; \
    curl -fsSLO "https://download.samba.org/pub/samba/stable/samba-${SAMBA_VERSION}.tar.asc"; \
    echo "${SAMBA_TARBALL_SHA256}  samba-${SAMBA_VERSION}.tar.gz" | sha256sum -c -; \
    gpg --import /tmp/samba-release-key.asc; \
    gunzip -k "samba-${SAMBA_VERSION}.tar.gz"; \
    gpg --status-fd 1 --verify "samba-${SAMBA_VERSION}.tar.asc" "samba-${SAMBA_VERSION}.tar" \
      | grep -q "VALIDSIG .* 81F5E2832BD2545A1897B713AA99442FB680B620"; \
    tar -xf "samba-${SAMBA_VERSION}.tar"
RUN set -eux; \
    cd "/tmp/samba-${SAMBA_VERSION}"; \
    ./configure \
      --enable-fhs \
      --prefix=/usr \
      --sysconfdir=/etc \
      --localstatedir=/var \
      --libdir="/usr/lib/$(gcc -dumpmachine)" \
      --with-piddir=/run/samba \
      --without-pam \
      --without-systemd \
      --disable-cups \
      --disable-iprint \
      --without-quic \
      --with-acl-support \
      --with-ads \
    ; \
    make -j"$(nproc)"; \
    make install DESTDIR=/dest; \
    "/dest/usr/bin/smbd" --version || true
```

Notes that bind the implementer:
- `keys/samba-release-key.asc` is BINARY OpenPGP despite the .asc
  extension (Phase 0 finding, recorded in keys/PINNING.md): plain
  `gpg --import` handles it; never add `--dearmor`.
- If a configure flag above is rejected by 4.24's waf (flag names drift
  between Samba versions), replace it with the current equivalent and
  record old→new in the commit message. The INTENT is binding: FHS
  paths, no PAM, no systemd, no cups/iprint, no QUIC, bundled Heimdal.
- Do NOT add `--with-system-mitkrb5` or install `libkrb5-dev` under any
  circumstance (B.1).
- If configure reports a missing mandatory dependency, add the -dev
  package to the builder list (builder-only; runtime closure is Task 3's
  derivation) and note it in the commit message.

- [ ] **Step 2: Build the builder stage locally (native arm64)**

```bash
docker build --target builder -t samba-ad-dc:builder-smoke .
```

Expected: configure summary shows Heimdal (not MIT), build completes.
This is the long step (tens of minutes); do not interrupt it.

- [ ] **Step 3: Assert bundled Heimdal, not MIT**

```bash
docker run --rm samba-ad-dc:builder-smoke sh -c \
  'ldd /dest/usr/sbin/samba | grep -i krb'
```

Expected: Heimdal libs from the samba private lib dir (paths under
/usr/lib/**/samba or libkrb5 from the DESTDIR private tree); NOTHING
resolving to a system libkrb5 from an installed MIT package (there is
none in the builder list — a hit means configure pulled something
unexpected: STOP and report).

- [ ] **Step 4: hadolint + commit**

```bash
docker run --rm -i ghcr.io/hadolint/hadolint < Dockerfile
git add Dockerfile
git commit -m "feat: builder stage compiling Samba 4.24.6 from GPG-verified source (bundled Heimdal, FHS)"
```

---

### Task 3: Runtime stage — derived package closure, labels, smoke

**Files:**
- Create: `runtime-packages.txt`, `scripts/derive-runtime-packages.sh`,
  `scripts/check-image-packages.sh`
- Modify: `Dockerfile` (append runtime stage)

**Interfaces:**
- Consumes: `builder` stage `/dest` tree.
- Produces: final image (default target) named by CI as
  `ghcr.io/esitc-paris/samba-ad-dc`; `runtime-packages.txt` (one
  `package  # why` line each) consumed by the watcher (§9bis.1.c) and by
  `PKG_INDEX_HASH`; check scripts run by CI in Task 4.

- [ ] **Step 1: Write `scripts/derive-runtime-packages.sh`**

Derivation, not guessing (§5.1): run inside the builder image —

```bash
#!/bin/sh
# Derives the runtime Debian package set for the Samba DESTDIR tree:
# ldd every ELF under /dest, keep libraries NOT provided by the DESTDIR
# itself, map each to its owning dpkg package, print unique package list.
set -eu
find /dest -type f \( -perm -u+x -o -name '*.so*' \) -exec sh -c \
  'file -b "$1" | grep -q ELF && ldd "$1" 2>/dev/null' _ {} \; \
  | awk '/=>/ {print $3}' | sort -u \
  | grep -v '^/dest' \
  | while read -r lib; do dpkg -S "$(readlink -f "$lib")" 2>/dev/null | cut -d: -f1; done \
  | sort -u
```

(Needs `file` added to the builder apt list — builder-only, justified:
derivation tooling.)

- [ ] **Step 2: Run it, then author `runtime-packages.txt`**

```bash
docker run --rm samba-ad-dc:builder-smoke sh /scripts/derive-runtime-packages.sh
```

Take the output list, and for each package write a line
`<package>  # <why: which samba component links/needs it>`. Then ADD the
non-library runtime requirements, each justified:
- `python3` + the python3 lib packages the derivation found — samba-tool
- `python3-dnspython`  # samba_dnsupdate, samba-tool dns
- `chrony`             # MS-SNTP signed time service (B.3)
- `tini`               # PID 1 zombie reaping (§6.3)
- `ca-certificates`    # LDAPS/TLS trust roots
- `openssl`            # operator cert inspection in maintenance mode
Anything the derivation did NOT emit and that is not on the justified
list above must not appear.

- [ ] **Step 3: Append the runtime stage to `Dockerfile`**

```dockerfile
FROM ${RUNTIME_BASE} AS runtime
ARG SAMBA_VERSION=4.24.6
ARG PKG_INDEX_HASH=bootstrap
ARG VCS_REF=dev
ARG CREATED=1970-01-01T00:00:00Z
ARG BASE_NAME=debian:trixie-slim
ARG BASE_DIGEST=sha256:<from versions.yaml>
# PKG_INDEX_HASH busts exactly this layer when the watcher detects a
# runtime-package delta (SPEC §9.3) — keep it on the same RUN.
COPY runtime-packages.txt /usr/share/samba-ad-dc/runtime-packages.txt
RUN echo "pkg-index=${PKG_INDEX_HASH}" \
    && apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       $(sed 's/#.*//' /usr/share/samba-ad-dc/runtime-packages.txt) \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /dest/ /
RUN samba --version && samba-tool --version
LABEL org.opencontainers.image.source="https://github.com/esitc-paris/samba-ad-dc" \
      org.opencontainers.image.version="${SAMBA_VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.licenses="GPL-3.0-or-later" \
      org.opencontainers.image.description="Samba Active Directory Domain Controller, built from verified upstream source with bundled Heimdal. Independent community build - not affiliated with the Samba Team." \
      org.opencontainers.image.created="${CREATED}" \
      org.opencontainers.image.base.name="${BASE_NAME}" \
      org.opencontainers.image.base.digest="${BASE_DIGEST}" \
      org.esitc-paris.spec-version="1.2"
VOLUME ["/var/lib/samba", "/etc/samba"]
EXPOSE 53 53/udp 88 88/udp 135 137/udp 138/udp 139 389 389/udp 445 464 464/udp 636 3268 3269
# ENTRYPOINT arrives in Phase 2; interim:
CMD ["samba", "--version"]
```

- [ ] **Step 4: Write `scripts/check-image-packages.sh`**

Proves §5.1 (run by CI; local now): every dpkg package in the final
image is either (a) in the base image's package set, or (b) in
`runtime-packages.txt`, or (c) in the apt dependency closure of (b).
Implementation: `dpkg-query -W -f '${Package}\n'` in the final image vs
in bare `${RUNTIME_BASE}`; the difference set must be ⊆ closure of
`runtime-packages.txt` computed with
`apt-cache depends --recurse --no-recommends --no-suggests`.
Exit 1 with the offending package names otherwise.

- [ ] **Step 5: Full local build + checks**

```bash
docker build -t samba-ad-dc:dev .
docker run --rm samba-ad-dc:dev samba --version      # == 4.24.6
docker run --rm samba-ad-dc:dev samba-tool --help    # exits 0
sh scripts/check-image-packages.sh samba-ad-dc:dev   # passes
docker run --rm -i ghcr.io/hadolint/hadolint < Dockerfile
shellcheck scripts/*.sh
```

- [ ] **Step 6: Commit**

```bash
git add Dockerfile runtime-packages.txt scripts/
git commit -m "feat: runtime stage with derived package closure, OCI labels, package-set check"
```

---

### Task 4: CI build job (native matrix, consistency checks)

**Files:**
- Modify: `.github/workflows/ci.yml` (append `build` job; harden `lint`)
- Create: `scripts/check-pins-consistency.sh`

Carried review debt from Phase 0 (fold in while touching ci.yml):
- Pin the hadolint image by digest in the lint step (resolve with
  `docker buildx imagetools inspect ghcr.io/hadolint/hadolint:v2.14.0`
  or current release tag; use `tag@sha256:...` with the tag in a
  comment) — and use the SAME pinned reference in every local hadolint
  command in this plan.
- Pin yamllint: `pipx run yamllint==<current version> --strict .`
  (resolve current with `pipx run yamllint --version` locally).
- Add `timeout-minutes: 10` to the `lint` job and `timeout-minutes: 90`
  to the `build` job.

**Interfaces:**
- Consumes: workflow `CI` / job `lint` from Phase 0.
- Produces: job id `build` that Phase 3 extends with E2E steps.

- [ ] **Step 1: Write `scripts/check-pins-consistency.sh`** — asserts the
  Dockerfile ARG defaults (`SAMBA_VERSION`, `SAMBA_TARBALL_SHA256`,
  base digests) equal the `default_branch` entry in `versions.yaml`
  (parse with `python3 -c 'import yaml'` — PyYAML present on runners; ship
  a `python3 - <<EOF` inline parser to avoid a dependency locally).
  Exit 1 naming each mismatch.

- [ ] **Step 2: Append the `build` job to `ci.yml`**

```yaml
  build:
    name: Build (${{ matrix.arch }})
    needs: lint
    strategy:
      matrix:
        include:
          - arch: amd64
            runner: ubuntu-24.04
          - arch: arm64
            runner: ubuntu-24.04-arm
    runs-on: ${{ matrix.runner }}
    steps:
      - name: Checkout
        uses: actions/checkout@<same pinned SHA as lint>  # v5
      - name: Pin consistency
        run: sh scripts/check-pins-consistency.sh
      - name: Build image
        run: docker build -t samba-ad-dc:ci .
      - name: Smoke
        run: |
          docker run --rm samba-ad-dc:ci samba --version
          docker run --rm samba-ad-dc:ci samba-tool --help > /dev/null
      - name: Package closure check
        run: sh scripts/check-image-packages.sh samba-ad-dc:ci
```

- [ ] **Step 3: Verify locally** — `pipx run yamllint --strict .`,
  `shellcheck scripts/*.sh`, `sh scripts/check-pins-consistency.sh`.
  (CI execution itself is deferred with GitHub auth, same as Phase 0.)

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/ci.yml scripts/check-pins-consistency.sh
git commit -m "ci: native-matrix build job with pin-consistency and package-closure gates"
```

---

## Phase exit gate (roadmap Phase 1)

- [ ] Local `docker build` (arm64 native) green; `samba --version` says
      4.24.6; Heimdal (not MIT) confirmed.
- [ ] Package-closure check passes; every runtime package justified.
- [ ] hadolint, shellcheck, yamllint all pass locally.
- [ ] amd64 CI leg + push-triggered runs: DEFERRED until gh auth
      (recorded in the Phase 0 ledger; first CI run after auth must be
      green on both arches before Phase 1 is declared closed).
