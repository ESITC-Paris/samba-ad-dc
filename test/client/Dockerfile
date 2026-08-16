# Protocol test-client image for the E2E suite (SPEC §8.2, Annex B.5).
#
# NOT a published artifact and not part of the product: it exists so the
# suite drives Kerberos, SMB, LDAP, DNS and NTP against the DC *over the
# network*, from a separate container, the way a real domain member does
# — instead of running every assertion inside the DC where a working
# loopback would hide a broken listener. §5.1's image-minimality rules
# scope published images, so the extra clients here cost the product
# nothing.
#
# The base is pinned to the exact digest versions.yaml carries for
# base.runtime, so client and DC share one userland: a protocol failure
# is then about the DC, never about a client from a different Debian.
FROM debian:trixie-slim@sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258

# What each package is for:
#   heimdal-clients   kinit/klist/kdestroy — Heimdal, matching the DC's
#                     bundled-Heimdal Kerberos (B.1). Its kinit reads the
#                     password from stdin with --password-file=STDIN, so
#                     no secret ever appears on a command line.
#   smbclient         Kerberized and NTLM SMB access.
#   ldap-utils        ldapsearch for LDAP/LDAPS/GC probing.
#   bind9-dnsutils    dig, for the SRV records a member discovers the DC
#                     with. (The DC image deliberately has no dnsutils —
#                     B.6 — which is precisely why the client needs it.)
#   chrony            chronyd -Q, a one-shot NTP query proving the signed
#                     time service actually answers.
#   ca-certificates   the trust store LDAPS verification starts from.
#
# DL3008 (pin apt versions) is ignored deliberately: the FROM digest above
# pins the package index these names resolve against, which is the same
# reproducibility interpretation the shipped image documents (B.8). Pinning
# versions here would only mean editing this file by hand every time Debian
# moves a client package, for a throwaway test image.
# hadolint ignore=DL3008
RUN apt-get update \
    && apt-get install --no-install-recommends -y \
        heimdal-clients \
        smbclient \
        ldap-utils \
        bind9-dnsutils \
        chrony \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

LABEL org.opencontainers.image.title="samba-ad-dc e2e test client" \
      org.opencontainers.image.description="Protocol clients (Kerberos, SMB, LDAP, DNS, NTP) for the samba-ad-dc E2E suite. Not published."

CMD ["/bin/bash"]
