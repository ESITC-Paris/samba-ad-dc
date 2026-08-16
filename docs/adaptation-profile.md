# Adaptation profile — samba-ad-dc (SPEC.md §12)

Living copy of SPEC.md Annex B. **This file is authoritative** for the
image's current state; Annex B inside the vendored SPEC.md stays frozen
at ratification. Divergences from Annex B are listed in the changelog at
the bottom of this file.

## Annex B — Adaptation profile: Samba Active Directory Domain Controller

First instance of a §12 profile. Governs the `samba-ad-dc` image.

### B.1 Build strategy

Built **from verified upstream source** (GPG-verified release tarball
against a vendored, out-of-band-pinned release key), not from distribution
packages, in order to: (a) meet the §9 SLOs independently of distribution
packaging schedules, and (b) build the AD DC role with Samba's **bundled
Heimdal** Kerberos — the configuration upstream recommends for this role,
whereas distribution packages typically build it against MIT KRB5, which
upstream still flags as experimental for the AD DC. Consequence, stated
plainly: security integration responsibility is ours, with no distribution
safety net; the watcher monitors the Samba security announcement channel
directly, and pre-announced security releases use the armed detection mode
(§9bis.1.a). Layout is FHS so paths match distribution conventions.

**What "bundled Heimdal" does and does not mean for the shipped image.**
The shipped image contains MIT krb5 runtime libraries (`libkrb5.so.3`,
`libgssapi_krb5.so.2`, `libkrb5support.so.0`, `libk5crypto.so.3`)
transitively, pulled by Debian's `libtirpc` (RPCSEC_GSS). Samba itself is
built against its bundled Heimdal: no Samba-built ELF names a system MIT
library in its own `DT_NEEDED`, and the build asserts this invariant at
image-build time. The invariant is therefore about *linkage*, not about
the absence of MIT files from the filesystem — an image scan that reports
MIT krb5 packages is reporting the truth, and is not evidence that Samba
uses them.

**Vendored third-party components.** Two upstream-vendored trees are
compiled into this image: Samba's **bundled Heimdal** Kerberos (above) and
the **bundled ngtcp2 / ngtcp2-crypto-gnutls** stack that backs SMB-over-QUIC
(B.6). Neither is a Debian package, so distribution CVE feeds and the
image scanner do not see these in-tree components at all: the Samba
release and security announcement channel is the **sole** monitoring path
for them, and the watcher treats a Samba security release as covering
them.

### B.2 Deviations from generic requirements (§11.2 justifications)

- **§5.2 non-root: impossible.** The AD DC writes extended attributes in
  the `security.*` namespace, which requires `CAP_SYS_ADMIN`; it binds
  privileged ports (53, 88, 389, 445, 464, 636). Mitigation: minimal
  capability set instead of `--privileged`, established by CI capability
  bisection (working hypothesis: SYS_ADMIN, NET_BIND_SERVICE, CHOWN,
  FOWNER, DAC_OVERRIDE, SETUID, SETGID; candidates to bisect include
  DAC_READ_SEARCH), with `cap_drop: ALL` as the baseline in every example.
- **§5.6 read-only rootfs: supported and CI-proven.** Writable paths:
  `/var/lib/samba` (persistent volume: directory database, Kerberos
  secrets, sysvol, TLS material, NTP signing socket), `/etc/samba`
  (persistent volume: generated configuration), `/run` and `/tmp`
  (tmpfs). No writes under `/etc` at runtime.

### B.3 Non-negotiable deployment constraints

- **No NAT.** The DC registers its own IP in its own DNS; Kerberos and
  dynamic RPC reference it. Supported topologies: dedicated IP per DC via
  macvlan/ipvlan, or host networking. Port publishing on a bridge network
  is unsupported.
- **Filesystem:** the volume backing `/var/lib/samba` requires xattr and
  POSIX ACL support (ext4/xfs); NFS unsupported.
- **Time:** the container serves signed NTP (MS-SNTP via chrony, wired to
  Samba's signing socket) to domain members but does not discipline the
  clock by default — host time synchronization is the operator's
  responsibility (option exists to grant CAP_SYS_TIME instead).
- **Secrets:** file-based only (`*_FILE`); the domain administrator
  password is never accepted via plain environment variable.

### B.4 Entrypoint state machine (documented modes = tested modes)

`auto` (first-boot convenience: provisions or joins on empty state),
`provision` (new domain; refuses over existing state), `join` (additional
DC; refuses over existing state), `run` (production mode: refuses to start
if state is absent — protection against silent re-provisioning on a
missing volume; documentation instructs switching to it after
initialization), `maintenance` (database check/repair without starting the
daemon). Version guard: refuses to open state written by a newer Samba;
on upgrade, runs the database consistency check automatically.

### B.5 Documented use cases → E2E matrix (§8.2)

Nominal: provision; Kerberos authentication (kinit) and Kerberized SMB;
NTLM authentication path; DNS SRV records served; LDAPS with certificate;
signed-NTP wiring; database consistency. Additional-DC join with
bidirectional directory replication verified by object propagation both
ways. Operational: idempotent restart without state loss; offline backup
AND restore into a fresh instance with object-level verification; upgrade
from the last published tag of the branch with data intact; explicit
downgrade refusal. Negative: missing secret file fails fast with an
actionable message; provision over existing state refused; run mode
without state refused. All of the above executed on both architectures
with the read-only rootfs configuration.

### B.6 Known limitations (stated per §12.4)

- **§6.7: not applicable.** The entrypoint is implemented in Go from the
  first release; no shell interim ever ships and no §11.2 exception is
  required (see `docs/exceptions/README.md`).

- **Sysvol replication is not provided by Samba** (no DFS-R): with
  multiple DCs, group policy content does not replicate by itself. An
  integrated, tested synchronization mechanism from the PDC-emulator
  holder is committed roadmap (v2); until then this is a documented
  limitation with a manual procedure.
- **Real Windows-client domain join is not exercised in CI** (no Windows
  runners in the public pipeline); protocol-level equivalents are tested.
  A non-blocking out-of-band validation with a real Windows client is a
  roadmap item.
- **Cross-branch upgrade tests** (e.g. 4.21 → 4.22) are committed alongside
  multi-branch activation; intra-branch upgrades are tested on every
  build.
- Backup strategy: **offline backup is the primary, CI-tested path**
  (credential-free, automation-friendly); online backup is a documented
  alternative requiring administrator credentials.
- **SMB-over-QUIC is compiled in and cannot be compiled out.** Samba 4.24
  provides no configure switch to disable it; the code (and its bundled
  ngtcp2) is built unconditionally. It is left **unconfigured by default**
  and 443/udp is not exposed, so nothing listens for QUIC in the shipped
  image. Monitoring for the bundled ngtcp2 follows B.1 (Samba channel only).
- **SambaGPG is unsupported by this image.** The build passes
  `--without-gpgme`, so the `password hash gpg key ids` feature is not
  available. Decision per SPEC §5.1: dropping it removes the entire GnuPG
  suite — including a network-capable daemon (`dirmngr`) — from the image
  closure, which is worth more than an opt-in password-store variant.
- **No `nsupdate` in the image** (`bind9-dnsutils` is not installed).
  Dynamic DNS updates therefore run through
  `samba_dnsupdate --use-samba-tool`, which the entrypoint pins (Phase 2);
  a configuration that would require the external updater is unsupported.

### B.7 Catalog

All Samba stable branches currently supported upstream are published
simultaneously (§3.6), each receiving every patch release; branch
lifecycle relayed per §9.4 (release candidates of a new series trigger the
deprecation notice for the oldest branch).

### B.8 Reproducibility (SPEC §4.3 interpretation)

What the build guarantees is that all **inputs** are pinned: the base image
digest, the source tarball hash (plus its OpenPGP signature), and the
distribution package-index hash, which is carried as a build argument whose
only job is to bust the package-installation layer (§9.3). Resolved Debian
package **versions** are not pinned in the Dockerfile; they are *recorded*,
at build time, in the SBOM published with the image. The property claimed
is therefore: **same commit + same base digest + same package index state
⇒ same software content.** Bit-identical image digests across rebuilds are
not claimed, and pinning every apt version in the Dockerfile is explicitly
rejected — it would make security rebuilds a manual edit instead of a
rebuild.

## Profile changelog

- 2026-08-16: created from SPEC.md v1.2 Annex B; B.6 shell-entrypoint
  exception removed (Go entrypoint committed from first release, roadmap
  decision D2).
- 2026-08-16: Phase 1 final-review amendments — B.1 states the actual
  Kerberos invariant (MIT krb5 present transitively via libtirpc; the
  assertion is about direct `DT_NEEDED` linkage) and names the vendored
  third-party trees (bundled Heimdal, bundled ngtcp2/ngtcp2-crypto-gnutls)
  whose sole monitoring path is the Samba release channel; B.6 gains the
  SMB-over-QUIC, SambaGPG (`--without-gpgme`) and no-`nsupdate`
  limitations; new B.8 states the §4.3 reproducibility interpretation
  (pinned inputs, SBOM-recorded resolved versions).
