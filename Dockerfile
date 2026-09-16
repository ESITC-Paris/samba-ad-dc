# syntax=docker/dockerfile:1
#
# Samba AD DC — built from GPG-verified upstream source.
#
# Every pin below mirrors versions.yaml (branch 4.24); CI asserts they
# stay in sync. Nothing here resolves a "latest" anything.
ARG BUILDER_BASE=debian:trixie-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132
ARG RUNTIME_BASE=debian:trixie-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132
# golang:1.24-trixie — entrypoint/go.mod declares `go 1.24.0`, so 1.24 is
# the floor, and trixie matches the runtime base so the toolchain and the
# image agree on their libc even though the binary is built CGO_ENABLED=0.
ARG GOBUILD_BASE=golang:1.24-trixie@sha256:5835f052b784aa39f2fe9070def3568605c8bc3fcd810f10402066348b61e716

# ---------------------------------------------------------------------------
# builder — compiles Samba with bundled Heimdal into DESTDIR=/dest
# ---------------------------------------------------------------------------
FROM ${BUILDER_BASE} AS builder

# bash (not dash) so `set -o pipefail` is honoured inside RUN pipelines.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ARG SAMBA_VERSION=4.24.7
ARG SAMBA_TARBALL_SHA256=45b7747a47452eff2b2159a44cc63eb43690d339fd1069088e023a015fed06c7
# Primary key of the Samba Distribution Verification Key (keys/PINNING.md).
ARG SAMBA_SIGNING_FINGERPRINT=81F5E2832BD2545A1897B713AA99442FB680B620

# Build toolchain for the AD DC role only: no printing (cups/iprint), no
# cluster FS (ceph/gluster), no avahi, no systemd, no PAM, no winexe
# cross-compilers. libkrb5-dev is deliberately absent: Samba must build
# against its own bundled Heimdal, never system MIT Kerberos.
# libldap-dev is required by --with-ads (configure aborts without it) and
# pulls no MIT Kerberos development package.
# readelf, used by the MIT guardrail below, comes from binutils, which gcc
# already depends on — no separate entry needed.
# Deliberately NOT listed, each verified to leave the build byte-identical
# in what it links: libblkid-dev, libkeyutils-dev (no shipped ELF names
# libblkid/libkeyutils in its DT_NEEDED) and libtasn1-6-dev (nothing links
# libtasn1 directly; libgnutls28-dev pulls it in anyway).
# libgpgme11-dev is also deliberately absent: its only consumer is the
# opt-in SambaGPG feature of password_hash.so, and libgpgme at runtime
# drags the whole GnuPG suite (11 packages, including the network-capable
# dirmngr) into the image — not justifiable under SPEC §5.1 for a DC.
# Dropping the package is not enough: with the AD DC role enabled waf
# treats a missing gpgme as fatal, so the opt-out is stated explicitly as
# --without-gpgme below.
# Package versions are not pinned here: the build's INPUTS are pinned (base
# image digest, tarball hash, package-index hash) and the resolved package
# versions are recorded in the published SBOM (SPEC §4.3 interpretation in
# the adaptation profile).
# hadolint ignore=DL3008
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      bison flex perl libparse-yapp-perl rpcsvc-proto pkgconf \
      gcc g++ make python3 python3-dev \
      python3-dnspython python3-markdown \
      libacl1-dev libarchive-dev libattr1-dev libbsd-dev \
      libcap-dev libcrypt-dev libgnutls28-dev libicu-dev \
      libjansson-dev libldap-dev liblmdb-dev libpopt-dev \
      libreadline-dev libtirpc-dev zlib1g-dev \
      xsltproc docbook-xsl docbook-xml \
      ca-certificates curl gpg gpg-agent \
    && rm -rf /var/lib/apt/lists/*

COPY keys/samba-release-key.asc /tmp/samba-release-key.asc

WORKDIR /tmp

# Provenance chain, in this order:
#   1. sha256 pins the exact compressed artifact we fetched;
#   2. gunzip;
#   3. OpenPGP signature over the *uncompressed* tar (that is what
#      upstream signs), asserting the pinned primary key fingerprint.
# The status output goes to a file rather than through a pipe so that
# `grep -q` cannot SIGPIPE gpg under `set -o pipefail`.
# Two assertions ride on that status file: the positive one (VALIDSIG on the
# pinned fingerprint) and a negative one rejecting EXPKEYSIG/REVKEYSIG/
# ERRSIG/BADSIG. The negative test is written as an `if ... exit 1` block,
# NOT as `! grep -q ...`: a command whose status is inverted with `!` is
# explicitly exempt from `set -e`, so the `!` form would never fail the
# build no matter what gpg reported.
# The vendored key is BINARY OpenPGP despite the .asc name — plain
# `gpg --import` handles it; never --dearmor it.
RUN set -eux; \
    curl -fsSLO --retry 3 --retry-delay 5 \
        "https://download.samba.org/pub/samba/stable/samba-${SAMBA_VERSION}.tar.gz"; \
    curl -fsSLO --retry 3 --retry-delay 5 \
        "https://download.samba.org/pub/samba/stable/samba-${SAMBA_VERSION}.tar.asc"; \
    echo "${SAMBA_TARBALL_SHA256}  samba-${SAMBA_VERSION}.tar.gz" | sha256sum -c -; \
    gpg --batch --import /tmp/samba-release-key.asc; \
    gunzip "samba-${SAMBA_VERSION}.tar.gz"; \
    gpg --batch --status-fd 1 --verify \
        "samba-${SAMBA_VERSION}.tar.asc" "samba-${SAMBA_VERSION}.tar" > /tmp/gpg-status.txt; \
    grep -q "VALIDSIG .* ${SAMBA_SIGNING_FINGERPRINT}$" /tmp/gpg-status.txt; \
    if grep -qE '^\[GNUPG:\] (EXPKEYSIG|REVKEYSIG|ERRSIG|BADSIG)' /tmp/gpg-status.txt; then \
      echo "FATAL: gpg reported a rejected signature status:" >&2; \
      grep -E '^\[GNUPG:\] (EXPKEYSIG|REVKEYSIG|ERRSIG|BADSIG)' /tmp/gpg-status.txt >&2; \
      exit 1; \
    fi; \
    tar -xf "samba-${SAMBA_VERSION}.tar"; \
    rm -f "samba-${SAMBA_VERSION}.tar" "samba-${SAMBA_VERSION}.tar.asc" /tmp/gpg-status.txt

WORKDIR /tmp/samba-${SAMBA_VERSION}

# FHS layout under /usr with config in /etc and state in /var; AD DC role
# left enabled (no --without-ad-dc) and Python left enabled (samba-tool
# needs it). Kerberos: no --with-system-mitkrb5 and no MIT headers in the
# image, so waf falls back to the bundled Heimdal — asserted by the
# guardrail scan further down.
# vfs_snapper is dropped because it hard-requires dbus-1, which an AD DC
# has no use for; configure aborts otherwise.
# 4.24 has no --without-quic: SMB-over-QUIC and its bundled ngtcp2 are
# unconditional; unconfigured at runtime (see adaptation profile B.6).
RUN set -eux; \
    ./configure \
      --enable-fhs \
      --prefix=/usr \
      --sysconfdir=/etc \
      --localstatedir=/var \
      --libdir="/usr/lib/$(gcc -dumpmachine)" \
      --with-piddir=/run/samba \
      --without-pam \
      --without-systemd \
      --without-gpgme \
      --disable-cups \
      --disable-iprint \
      --with-acl-support \
      --with-ads \
      --with-shared-modules='!vfs_snapper' \
    ; \
    make -j"$(nproc)"; \
    make install DESTDIR=/dest

# Prune what the runtime image has no use for, BEFORE the guardrail scan
# below, so the scan runs on the tree that actually ships and proves the
# prune regressed nothing. Exactly two classes go: the fuzz/torture test
# drivers (smbtorture, gentest, locktest, masktest — developer tools, never
# invoked by a DC) and the build-time-only development artifacts (public
# headers, pkg-config files). No private .so is touched: Samba dlopen()s
# modules by path at runtime, so "unreferenced" says nothing about
# "unused". Every path is asserted present before removal — a silent
# no-op prune after an upstream layout change would quietly put the test
# drivers back in the image.
RUN set -eux; \
    triplet="$(gcc -dumpmachine)"; \
    before=$(du -sb /dest | cut -f1); \
    for p in /dest/usr/bin/smbtorture \
             /dest/usr/bin/gentest \
             /dest/usr/bin/locktest \
             /dest/usr/bin/masktest \
             /dest/usr/include \
             "/dest/usr/lib/${triplet}/pkgconfig"; do \
      if [ ! -e "$p" ]; then \
        echo "FATAL: prune target absent (upstream install layout changed?): $p" >&2; \
        exit 1; \
      fi; \
      rm -rf "$p"; \
    done; \
    after=$(du -sb /dest | cut -f1); \
    echo "prune: /dest ${before} -> ${after} bytes (delta $((before - after)))"

# The invariant this guardrail enforces: no ELF Samba built may name a
# system MIT Kerberos library in its OWN DT_NEEDED, i.e. Samba's Kerberos is
# the bundled Heimdal and nothing else. MIT reached *transitively* is
# expected and allowed — Debian's libtirpc needs libgssapi_krb5 for
# RPCSEC_GSS — so the assertion is deliberately about direct entries only.
# Every step writes to a file and is tested with a plain command status; no
# security decision rides on a pipe status that pipefail could turn into an
# unrelated 141. The scan is proven non-vacuous by two positive assertions
# (the result file is non-empty and contains the samba binary) before the
# forbidden-name test runs. It scans the pruned tree on purpose: what is
# asserted is what ships.
#
# SC3045 (`read -d` is undefined in POSIX sh) does not apply: this stage's
# SHELL is bash, which hadolint's shellcheck pass does not take into
# account. NUL-delimited iteration is what makes the scan safe.
# hadolint ignore=SC3045
RUN set -eux; \
    : > /tmp/elf-needed.txt; \
    find /dest -type f -print0 > /tmp/dest-files.bin; \
    while IFS= read -r -d '' f; do \
      readelf -dW "$f" > /tmp/readelf-out.txt 2>/dev/null || continue; \
      awk -v F="$f" '/\(NEEDED\)/ { l = $0; sub(/.*\[/, "", l); sub(/\].*/, "", l); print F " " l }' \
        /tmp/readelf-out.txt >> /tmp/elf-needed.txt; \
    done < /tmp/dest-files.bin; \
    test -s /tmp/elf-needed.txt; \
    grep -q '^/dest/usr/sbin/samba ' /tmp/elf-needed.txt; \
    echo "ELF scan: $(grep -c '' /tmp/elf-needed.txt) direct NEEDED entries under /dest"; \
    if grep -E ' (libkrb5\.so\.3|libgssapi_krb5\.so\.2|libkrb5support\.so\.0|libk5crypto\.so\.3|libk5crypto3\.so\.3)$' \
         /tmp/elf-needed.txt > /tmp/mit-direct.txt; then \
      echo "FATAL: Samba-built ELF with a direct NEEDED entry on system MIT Kerberos:" >&2; \
      cat /tmp/mit-direct.txt >&2; \
      exit 1; \
    fi; \
    rm -f /tmp/elf-needed.txt /tmp/dest-files.bin /tmp/readelf-out.txt /tmp/mit-direct.txt; \
    LD_LIBRARY_PATH="/dest/usr/lib/$(gcc -dumpmachine):/dest/usr/lib/$(gcc -dumpmachine)/samba" \
      /dest/usr/sbin/samba --version

# The base image ships /var/run and /var/lock as symlinks into /run, while
# DESTDIR staged them as real (and empty) directories; pouring that tree
# onto / fails with "cannot copy to non-directory". Fold them into their
# real location here, so the runtime stage's COPY stays a single,
# relocation-free /dest/ -> / (nothing but empty directories moves).
RUN set -eux; \
    mkdir -p /dest/run/samba /dest/run/lock; \
    mv /dest/var/lock/samba /dest/run/lock/samba; \
    rmdir /dest/var/lock /dest/var/run/samba /dest/var/run

# ---------------------------------------------------------------------------
# gobuild — compiles and unit-tests the Go entrypoint
# ---------------------------------------------------------------------------
# The §6.7 gate travels with the image: gofmt, go vet and the whole unit
# suite run here, so an image can only exist if the state machine's
# transition/refusal matrix passed. CI runs the same commands on the host
# (the `unit` job) for a fast, readable failure; this stage is what makes it
# impossible to ship around them.
FROM ${GOBUILD_BASE} AS gobuild

ARG SAMBA_VERSION=4.24.7

WORKDIR /src

# Manifests first: the module download layer is then reused across every
# source-only change.
COPY entrypoint/go.mod entrypoint/go.sum ./
RUN go mod download

COPY entrypoint/ ./

# CGO_ENABLED=0: the binary must run as PID 1's payload with no dependency
# on the runtime image's libc version. -trimpath keeps build paths out of
# it; -s -w drop the symbol table and DWARF (the entrypoint is debugged from
# its logs, not from a core dump inside a container).
# The Samba version is injected here and nowhere else: the version guard
# compares the marker on the state volume against this value, so a binary
# built without the flag must — and does — refuse to start.
RUN set -eux; \
    unformatted="$(gofmt -l .)"; \
    if [ -n "${unformatted}" ]; then \
      echo "FATAL: gofmt would rewrite these files:" >&2; \
      echo "${unformatted}" >&2; \
      exit 1; \
    fi; \
    go vet ./...; \
    go test ./...; \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.sambaVersion=${SAMBA_VERSION}" \
      -o /out/entrypoint ./cmd/entrypoint

# ---------------------------------------------------------------------------
# runtime — the shipped image: base + derived package closure + /dest
# ---------------------------------------------------------------------------
FROM ${RUNTIME_BASE} AS runtime

ARG SAMBA_VERSION=4.24.7
# The hash of the distribution package index the runtime closure was
# resolved against (SPEC §9bis.1.c); the watcher edits it in versions.yaml
# and CI injects it with --build-arg. The default below is a bare fallback
# for a plain `docker build .` — it is deliberately NOT asserted equal to
# versions.yaml, so nothing here claims to mirror the catalog.
# BuildKit invalidates a layer from the instruction that REFERENCES an ARG,
# not from the ARG declaration, and the only instruction referencing this
# one is the apt RUN below; the ARGs and COPY in between therefore do not
# weaken the §9.3 "busts exactly this layer" property — do not reorder.
ARG PKG_INDEX_HASH=bootstrap
ARG VCS_REF=dev
ARG CREATED=1970-01-01T00:00:00Z
ARG BASE_NAME=debian:trixie-slim
ARG BASE_DIGEST=sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132

# The manifest is shipped inside the image so that what an operator can
# read is exactly what was installed (SPEC §5.1).
COPY runtime-packages.txt /usr/share/samba-ad-dc/runtime-packages.txt

# PKG_INDEX_HASH is echoed on this RUN and nowhere else: it busts exactly
# this layer when the watcher detects a runtime-package delta (SPEC §9.3),
# "nothing more, nothing less".
# Package versions are not pinned here for the same reason as in the
# builder: the inputs are pinned (base image digest, PKG_INDEX_HASH) and the
# resolved package versions are recorded in the published SBOM (SPEC §4.3
# interpretation in the adaptation profile).
# hadolint ignore=DL3008,SC2046
RUN echo "pkg-index=${PKG_INDEX_HASH}" \
    && apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       $(sed 's/#.*//' /usr/share/samba-ad-dc/runtime-packages.txt) \
    && rm -rf /var/lib/apt/lists/*

# No relocation: the build was configured --prefix=/usr and the binaries
# carry RPATHs into /usr/lib/<triplet>/samba, so the DESTDIR tree is
# poured onto / exactly as it was staged.
COPY --from=builder /dest/ /

# Smoke: both the C binaries and the python tooling must run with nothing
# but this image's own packages — no LD_LIBRARY_PATH, no PYTHONPATH.
RUN samba --version \
    && samba-tool --version \
    && samba-tool --help > /dev/null

COPY --from=gobuild /out/entrypoint /usr/local/bin/entrypoint

# The chrony configuration TEMPLATE. /etc/chrony is NOT a volume (only
# /etc/samba and /var/lib/samba are) and the rootfs is read-only, which is
# exactly why this file can live here and survive — and why it is a template
# rather than the file the daemon reads: one line of it, the MS-SNTP signing
# socket directory, is an smb.conf parameter the operator may set on the
# /etc/samba volume, so the entrypoint generates the effective configuration
# at /run/chrony/chrony.conf from this file plus what the DC's own smb.conf
# declares, and points chronyd there.
# Nothing is created under /var/lib/samba at build time: it is a volume, and
# anything baked there is masked the moment one is mounted. The entrypoint
# creates the runtime directories (/run/samba, /run/lock/samba, /run/chrony)
# and /var/lib/samba/chrony itself, at startup.
COPY chrony/chrony.conf /etc/chrony/chrony.conf

# Smoke the entrypoint, and with it the -ldflags injection: a binary that
# reported the wrong Samba version would pass every unit test and then
# refuse to open the state volume at the worst possible moment.
RUN set -eux; \
    reported="$(/usr/local/bin/entrypoint --version)"; \
    echo "${reported}"; \
    case "${reported}" in \
      *"${SAMBA_VERSION}"*) ;; \
      *) echo "FATAL: entrypoint reports '${reported}', expected samba ${SAMBA_VERSION}" >&2; exit 1 ;; \
    esac

LABEL org.opencontainers.image.source="https://github.com/esitc-paris/samba-ad-dc" \
      org.opencontainers.image.version="${SAMBA_VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.licenses="GPL-3.0-or-later" \
      org.opencontainers.image.description="Samba Active Directory Domain Controller, built from verified upstream source with bundled Heimdal. Independent community build - not affiliated with the Samba Team." \
      org.opencontainers.image.created="${CREATED}" \
      org.opencontainers.image.base.name="${BASE_NAME}" \
      org.opencontainers.image.base.digest="${BASE_DIGEST}" \
      org.esitc-paris.spec-version="1.2"

# Both `samba-tool domain provision` and `samba-tool domain join` generate a
# Kerberos configuration for the realm at this path and print where they put
# it. Nothing reads it unless it is pointed at: the image ships no
# /etc/krb5.conf, so without this every Kerberos bind made INSIDE the
# container falls back to DNS realm discovery — the bundled Heimdal walks
# `_kerberos.` up the parent domains of the host name, none of which the
# directory is authoritative for. On a DC that resolves through its own
# internal DNS (which a multi-DC domain requires) those queries take seconds
# instead of milliseconds, and the Kerberos-sealed DRSUAPI bind that carries
# replication times out before the walk finishes; `samba-tool drs showrepl`
# run by an operator in the container simply hangs. The generated file sets
# `dns_lookup_realm = false`, which removes the walk entirely.
#
# It is an image ENV rather than something the entrypoint exports, because
# `docker exec` inherits the image environment and NOT the environment of
# PID 1: this is the only form that also reaches an operator's own commands.
# Pointing at a file that does not exist yet (before the first provision) is
# harmless — Kerberos falls back to its built-in defaults, which is what it
# does today with no file at all.
ENV KRB5_CONFIG=/var/lib/samba/private/krb5.conf

VOLUME ["/var/lib/samba", "/etc/samba"]

# DNS(53), Kerberos(88), NTP(123), EPM(135), NetBIOS(137-139), LDAP(389),
# SMB(445), kpasswd(464), LDAPS(636), Global Catalog(3268/3269).
# 123/udp is the MS-SNTP signed time service chrony serves to domain
# members (adaptation profile B.3).
EXPOSE 53 53/udp 88 88/udp 123/udp 135 137/udp 138/udp 139 389 389/udp 445 464 464/udp 636 3268 3269

# tini is PID 1 and reaps the zombies Samba's process model leaves behind;
# `--` makes it forward signals to the entrypoint, which turns SIGTERM into
# the orderly stop of samba then chronyd (§6.3).
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint"]

# An application-level probe, not a process check: it asks DNS, LDAP and SMB
# on the loopback address whether this container is actually serving the
# domain (§5.5). The start period is generous because a first-boot provision
# legitimately takes minutes on a cold volume, and a container declared
# unhealthy mid-provision would be restarted into a half-initialized state.
HEALTHCHECK --interval=30s --timeout=10s --start-period=180s --retries=3 \
    CMD ["/usr/local/bin/entrypoint", "healthcheck"]
