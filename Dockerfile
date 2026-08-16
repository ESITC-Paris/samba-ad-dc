# syntax=docker/dockerfile:1
#
# Samba AD DC — built from GPG-verified upstream source.
#
# Every pin below mirrors versions.yaml (branch 4.24); CI asserts they
# stay in sync. Nothing here resolves a "latest" anything.
ARG BUILDER_BASE=debian:trixie-slim@sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258
ARG RUNTIME_BASE=debian:trixie-slim@sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258

# ---------------------------------------------------------------------------
# builder — compiles Samba with bundled Heimdal into DESTDIR=/dest
# ---------------------------------------------------------------------------
FROM ${BUILDER_BASE} AS builder

# bash (not dash) so `set -o pipefail` is honoured inside RUN pipelines.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ARG SAMBA_VERSION=4.24.6
ARG SAMBA_TARBALL_SHA256=810cc955acb367e9bde556dccfb50db177a02b7c553aa1629a0b905fa7616267
# Primary key of the Samba Distribution Verification Key (keys/PINNING.md).
ARG SAMBA_SIGNING_FINGERPRINT=81F5E2832BD2545A1897B713AA99442FB680B620

# Build toolchain for the AD DC role only: no printing (cups/iprint), no
# cluster FS (ceph/gluster), no avahi, no systemd, no PAM, no winexe
# cross-compilers. libkrb5-dev is deliberately absent: Samba must build
# against its own bundled Heimdal, never system MIT Kerberos.
# libldap-dev is required by --with-ads (configure aborts without it) and
# pulls no MIT Kerberos development package.
# Package versions are not pinned here; reproducibility comes from the
# digest-pinned base image plus versions.yaml's pkg_index_hash.
# hadolint ignore=DL3008
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      bison flex perl libparse-yapp-perl rpcsvc-proto pkgconf \
      gcc g++ make python3 python3-dev \
      python3-dnspython python3-markdown \
      libacl1-dev libarchive-dev libattr1-dev libblkid-dev libbsd-dev \
      libcap-dev libcrypt-dev libgnutls28-dev libgpgme11-dev libicu-dev \
      libjansson-dev libkeyutils-dev libldap-dev liblmdb-dev libpopt-dev \
      libreadline-dev libtasn1-6-dev libtirpc-dev zlib1g-dev \
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
# The vendored key is BINARY OpenPGP despite the .asc name — plain
# `gpg --import` handles it; never --dearmor it.
RUN set -eux; \
    curl -fsSLO "https://download.samba.org/pub/samba/stable/samba-${SAMBA_VERSION}.tar.gz"; \
    curl -fsSLO "https://download.samba.org/pub/samba/stable/samba-${SAMBA_VERSION}.tar.asc"; \
    echo "${SAMBA_TARBALL_SHA256}  samba-${SAMBA_VERSION}.tar.gz" | sha256sum -c -; \
    gpg --batch --import /tmp/samba-release-key.asc; \
    gunzip "samba-${SAMBA_VERSION}.tar.gz"; \
    gpg --batch --status-fd 1 --verify \
        "samba-${SAMBA_VERSION}.tar.asc" "samba-${SAMBA_VERSION}.tar" > /tmp/gpg-status.txt; \
    grep -q "VALIDSIG .* ${SAMBA_SIGNING_FINGERPRINT}" /tmp/gpg-status.txt; \
    tar -xf "samba-${SAMBA_VERSION}.tar"; \
    rm -f "samba-${SAMBA_VERSION}.tar" "samba-${SAMBA_VERSION}.tar.asc" /tmp/gpg-status.txt

WORKDIR /tmp/samba-${SAMBA_VERSION}

# FHS layout under /usr with config in /etc and state in /var; AD DC role
# left enabled (no --without-ad-dc) and Python left enabled (samba-tool
# needs it). Kerberos: no --with-system-mitkrb5 and no MIT headers in the
# image, so waf falls back to the bundled Heimdal — asserted below.
# vfs_snapper is dropped because it hard-requires dbus-1, which an AD DC
# has no use for; configure aborts otherwise.
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
      --disable-cups \
      --disable-iprint \
      --with-acl-support \
      --with-ads \
      --with-shared-modules='!vfs_snapper' \
    ; \
    make -j"$(nproc)"; \
    make install DESTDIR=/dest; \
    if ldd /dest/usr/sbin/samba | grep -q 'libkrb5\.so\.3'; then \
      echo "FATAL: system MIT Kerberos linked into samba" >&2; exit 1; \
    fi; \
    LD_LIBRARY_PATH="/dest/usr/lib/$(gcc -dumpmachine):/dest/usr/lib/$(gcc -dumpmachine)/samba" \
      /dest/usr/sbin/samba --version
