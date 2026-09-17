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
safety net; the watcher polls upstream's release directory hourly (see
**Release cycle** below), and a pre-announced security release is published
without the 24-hour soak when the maintainer dispatches the check with
`security_release=true`. The announcement mailing list is **not**
machine-parsed in v1 and the §9bis.1.a armed mode is roadmap, both stated
as limitations in B.6. Layout is FHS so paths match distribution conventions.

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
  privileged ports (53, 88, 389, 445, 464, 636). Mitigation: a minimal
  capability set instead of `--privileged`, with `cap_drop: ALL` as the
  baseline in every example. The set is

  ```
  SYS_ADMIN  NET_BIND_SERVICE  CHOWN  FOWNER  SETUID  SETGID
  ```

  **established by capability bisection** — first locally (arm64,
  2026-08-16, image digest
  `sha256:198e52c9896718a6bcf273f58ab1d092800fbb63aa18a8a8b1d4e4862f27fb3e`,
  report `test/capbisect/results-arm64-2026-08-16.txt`), driver
  `test/capbisect/bisect.sh`. **CI has now confirmed it on both
  architectures.** The first `workflow_dispatch` of
  `.github/workflows/capbisect.yml` —
  <https://github.com/ESITC-Paris/samba-ad-dc/actions/runs/35153558018>,
  2026-09-16 — re-measured the shipped set on a native runner per
  architecture, and both legs ended `=> AGREE`: amd64 in
  `test/capbisect/results-amd64.txt` (image digest
  `sha256:8d1451a1ee46e33a533882b10a8ed2347b8a802d0748f479285ec97b0c15ece0`)
  and arm64 in `test/capbisect/results-arm64.txt` (image digest
  `sha256:ace92ca9ae2c34516f90f342bcfc59b79d803a762cbfc4d2f3f83bdfbfbf2f43`).
  The two kernels agreed capability for capability — the same five
  REQUIRED, the same `NET_BIND_SERVICE` DROPPABLE. That agreement is a
  result, not a formality: `.github/workflows/capbisect.yml` runs the two
  legs on native runners precisely because a capability check is kernel
  code and "is `SYS_ADMIN` required" is a question the two architectures
  are entitled to answer differently. Here they did not. Note also that
  `test/capbisect/results-arm64.txt` is now that CI report and no longer
  the local one; the local run is kept beside it under its date because
  it is the only committed measurement of `DAC_OVERRIDE`.

  This is no longer a hypothesis: each capability was removed from a real
  run of the E2E smoke subset (provision, `kinit`, restart) against that
  image, and what broke is what makes it "required".
  `test/e2e/harness.DefaultCaps` carries exactly this list, so every E2E
  run re-proves that the set is sufficient, and the driver fails when its
  own recommendation stops matching `DefaultCaps` — which is what lets a
  CI run report "B.2 is still true" rather than merely "the bisection
  completed".

  **What CI confirmation will and will not re-measure.** A bisection
  starts from the set the harness ships and removes one capability at a
  time, so a run of the shipped six-capability set re-measures those six
  and nothing else. `DAC_OVERRIDE`'s droppability was established against
  the older seven-capability set and is *not* revisited by such a run: a
  capability that is no longer in the set cannot be dropped from it. To
  re-open that question — after a Samba major, say, or a change to what
  the entrypoint does at boot — run the driver with the older set
  restored:

  ```
  CAPBISECT_SET='SYS_ADMIN,NET_BIND_SERVICE,CHOWN,FOWNER,DAC_OVERRIDE,SETUID,SETGID' \
      sh test/capbisect/bisect.sh
  ```

  which is exactly how `test/capbisect/results-arm64-2026-08-16.txt` was
  produced — and why that dated report is kept rather than superseded.
  The two reports the CI run wrote cover the shipped six and nothing
  else, so the dated one is the only committed evidence for the
  `DAC_OVERRIDE` row of the table below.

  | capability | verdict | what removing it does |
  | --- | --- | --- |
  | `SYS_ADMIN` | required | provision aborts setting the sysvol NT ACL: `set_nt_acl_conn: fset_nt_acl returned NT_STATUS_ACCESS_DENIED`, raised through `samba/provision/__init__.py:_setntacl` — the `security.NTACL` xattr cannot be written |
  | `CHOWN` | required | provision aborts, `INTERNAL ERROR: Security context active token stack underflow` |
  | `FOWNER` | required | the same operation and the same denial (`fset_nt_acl … NT_STATUS_ACCESS_DENIED`), surfacing one frame out in `samba/ntacls.py:setntacl` |
  | `SETUID` | required | `smbd: INTERNAL ERROR: failed to set uid`; SYSVOL/NETLOGON are never exported and the container never turns healthy |
  | `SETGID` | required | `smbd: INTERNAL ERROR: sys_setgroups failed`, samba exits |
  | `NET_BIND_SERVICE` | droppable **under the suite** — **retained** | see below |
  | `DAC_OVERRIDE` | droppable — **removed** | was in the pre-bisection hypothesis; the suite passes without it. Every process in the container runs as uid 0 over paths the entrypoint has already chowned to itself, so no discretionary check is left to override |
  | `DAC_READ_SEARCH` | not needed | the candidate named by the previous hypothesis; the verified minimal set passes without it |
  | `CAP_KILL` | not applicable | recorded, not measured: chronyd runs as root (B.6), so nothing has to be signalled across a uid boundary. It becomes measurable only if chronyd is made to drop privileges |

  `SYS_ADMIN` and `FOWNER` are consumed by the **same** operation —
  writing the NT ACL onto sysvol during provision — and removing either
  one alone breaks it with the same `NT_STATUS_ACCESS_DENIED`. They are
  two capabilities, not two mechanisms, and the table above records the
  denial each removal *produced*, not a kernel-level attribution of which
  permission check fired. That matters for maintenance: a Samba change to
  how sysvol ACLs are applied could plausibly move both verdicts at once,
  so neither should be reasoned about in isolation.

  **`NET_BIND_SERVICE` is the one entry the E2E suite cannot decide, and
  it is kept deliberately.** Docker sets
  `net.ipv4.ip_unprivileged_port_start=0` in the network namespace it
  creates for a container, so 53/88/389/445 are not privileged there and
  the suite — which runs on a user-defined bridge — passes without the
  capability no matter what the image needs. B.3 also supports **host
  networking**, where the container inherits the host's floor: 1024 on
  any ordinary Linux system. Measured directly (same report): with the
  floor forced back to 1024, a `bind()` of :389 inside this image is
  *denied* without the capability and *succeeds* with it. Dropping it
  would leave the suite green and the documented deployment broken, so it
  stays — and the reason it stays is a measurement, not caution.
- **§5.6 read-only rootfs: supported and CI-proven.** Writable paths:
  `/var/lib/samba` (persistent volume: directory database, Kerberos
  secrets, sysvol, TLS material, NTP signing socket), `/etc/samba`
  (persistent volume: generated configuration), `/run`, `/tmp` and
  `/var/cache/samba` (tmpfs). No writes under `/etc` at runtime.
  `/var/cache/samba` holds winbindd's `netsamlogon_cache.tdb`, which it
  opens on every boot: a tmpfs is the right answer because the content is
  a pure cache that must not survive a restart, and without it every
  start logs three `tdb_open_log`/`netsamlogon_cache_init` failures
  (measured: with the tmpfs those three lines disappear and nothing else
  in the boot log changes).
  Nothing may be baked into the image at those paths: a volume or tmpfs
  mount hides whatever the image holds there, so the entrypoint creates
  the runtime directories itself on every boot — `/run/samba`,
  `/run/lock/samba` and `/run/chrony`, mode `0755`, owned by root (the
  entrypoint's uid), before either daemon starts. `/run/chrony` is
  root-owned and needs no ownership handoff because chronyd runs as root
  in this image (B.6); chronyd's drift file is deliberately *not* there
  but on the persistent volume at `/var/lib/samba/chrony` (mode `0750`,
  created by the entrypoint for the same mount-hiding reason), so the
  clock estimate survives a restart instead of being lost with the
  tmpfs.
  The E2E harness starts **every** DC container on exactly this profile —
  `--read-only`, those three tmpfs mounts, `--cap-drop ALL` plus the
  capability set above, and `--security-opt no-new-privileges:true` — so
  the block the deployment guide publishes as *the hardened profile*
  (§1.8) and the block the suite runs are the same list of flags. The
  last of them is defence-in-depth rather than a measured requirement:
  nothing in the image escalates privilege at `exec` time, and the suite
  is green with or without it. It is applied anyway so that a change
  which ever needed a setuid helper is caught by a test instead of by a
  deployment following this profile.

### B.3 Non-negotiable deployment constraints

- **No NAT.** The DC registers its own IP in its own DNS; Kerberos and
  dynamic RPC reference it. Supported topologies: dedicated IP per DC via
  macvlan/ipvlan, or host networking. Port publishing on a bridge network
  is unsupported.
- **DNS: a DC resolves through a DC, and its own DNS needs an upstream.**
  Both halves are required together on a multi-DC domain. A replication
  partner is addressed by a `<objectGUID>._msdcs.<realm>` CNAME that only
  the directory's own DNS answers, so a DC pointed at anything else cannot
  replicate *from* its partners. And samba's internal DNS with no forwarder
  takes seconds — not milliseconds — to fail a name it does not serve,
  which is long enough to time out Kerberos and with it replication; set
  `SAMBA_DNS_FORWARDER` to a resolver that is not this DC. In-container
  Kerberos tooling depends on the same wiring (see Runtime contract:
  **In-container Kerberos**), and `KRB5_CONFIG` is set in the image so that
  an operator's `docker exec ... samba-tool` inherits the realm's Kerberos
  configuration rather than rediscovering it over DNS.
- **Filesystem:** the volume backing `/var/lib/samba` requires xattr and
  POSIX ACL support (ext4/xfs); NFS unsupported.
- **Time:** the container serves signed NTP (MS-SNTP via chrony, wired to
  Samba's signing socket) to domain members but does not discipline the
  clock by default — host time synchronization is the operator's
  responsibility (option exists to grant CAP_SYS_TIME instead).
  (Implemented in Phase 2 — see Runtime contract: **Time service**.)
- **Secrets:** file-based only (`*_FILE`); the domain administrator
  password is never accepted via plain environment variable.
  (Implemented in Phase 2 — see Runtime contract: **Environment
  variables**, secrets paragraph.)

### B.4 Entrypoint state machine (documented modes = tested modes)

`auto` (first-boot convenience: provisions or joins on empty state),
`provision` (new domain; refuses over existing state), `join` (additional
DC; refuses over existing state), `run` (production mode: refuses to start
if state is absent — protection against silent re-provisioning on a
missing volume; documentation instructs switching to it after
initialization), `maintenance` (database check/repair without starting the
daemon). Version guard: refuses to open state written by a newer Samba;
on upgrade, runs the database consistency check automatically.

*Implemented in Phase 2.* The binding, operator-facing form of this state
machine — variables, semantics, exit codes and the health check — is the
**Runtime contract** section below; it is the contract the code is tested
against, and B.4 above is its summary.

### B.5 Documented use cases → E2E matrix (§8.2)

Nominal: provision; Kerberos authentication (kinit) and Kerberized SMB;
NTLM authentication path; DNS SRV records served; LDAPS with certificate;
signed-NTP wiring; database consistency; declarative `[global]` options
applied, reconciled on a restart and refused when samba's own parser rejects
them (`TestGlobalOptionsApplied`). Additional-DC join with
bidirectional directory replication verified by object propagation both
ways. Operational: idempotent restart without state loss; offline backup
AND restore into a fresh instance with object-level verification; upgrade
from the last published tag of the branch with data intact; explicit
downgrade refusal. Negative: missing secret file fails fast with an
actionable message; provision over existing state refused; run mode
without state refused. All of the above executed on both architectures
with the read-only rootfs configuration.

Which test covers which clause is not left to the reader:
[`docs/traceability.md`](traceability.md) carries the row-by-row mapping
in both directions (§8.2), and `scripts/check-traceability.sh` fails CI
when a test or a row exists without its counterpart.

### B.6 Known limitations (stated per §12.4)

- **§6.7 shell-entrypoint exception: not required** — the entrypoint is Go
  from the first release, so no shell interim ever ships and no §11.2
  exception is filed (see `docs/exceptions/README.md`). §6.7 itself is
  satisfied; evidence: the unit-test gate runs inside the image build
  (Dockerfile `gobuild` stage) and in CI (`unit` job), and the behavior
  contract is the **Runtime contract** section below.

- **chronyd runs as root inside the container** (SIGTERM delivery without
  `CAP_KILL`; ntp_signd socket access). The alternative is `CAP_KILL` in
  the capability set plus signing-socket group permissions;
  `TestSignedNTPWiring` is the acceptance test either way. **REVISIT
  reached and closed (Phase 3 capability bisection, 2026-08-16):** the
  ruling stands unchanged, and `CAP_KILL` is recorded in B.2 as *not
  applicable* rather than measured. There is nothing to bisect while
  chronyd is root — a capability that governs signalling across a uid
  boundary cannot be shown necessary or unnecessary by a configuration
  that never crosses one. Making chronyd drop privileges is what would
  turn `CAP_KILL` into a measurable question, and that change would come
  with its own bisection run.

- **The Samba announcement mailing list is not machine-parsed (v1).**
  §9bis.1.a asks for two detection paths — the release directory *and* the
  announcement push channel, with an "armed" mode of tight conditional
  polling over a pre-announced window. What is implemented is the
  directory probe, hourly; `samba-announce` is read by a human. The
  consequence is bounded and is stated rather than implied: a security
  release is *detected* within the hour either way, because the tarball
  lands in the same directory the watcher already polls, and what the
  announcement would buy is skipping the soak automatically. Until then a
  maintainer does it explicitly — `gh workflow run upstream-check.yml -f
  security_release=true` sets the soak to 0 h — so the only cost of the
  gap is that an unattended security release waits out the same 24 hours
  as an ordinary one. Parsing the announce list and the armed polling mode
  are roadmap.

- **Sysvol replication is not provided by Samba** (no DFS-R): with
  multiple DCs, group policy content does not replicate by itself. An
  integrated, tested synchronization mechanism from the PDC-emulator
  holder is committed roadmap (v2); until then this is a documented
  limitation with a manual procedure.
- **Real Windows-client domain join is not exercised in CI** (no Windows
  runners in the public pipeline); protocol-level equivalents are tested.
  A non-blocking out-of-band validation with a real Windows client is a
  roadmap item.
- **Cross-branch upgrade coverage is bounded, and the bound is stated
  rather than implied.** Exactly two upgrade paths are tested and
  supported: *intra-branch* — the branch's previously published
  `X.Y.Z-rN` to the tag being released — on every release of any branch;
  and *previous branch → current branch* — `X.(Y-1)` to `X.Y` — on every
  release of the current branch. `TestUpgradeFromLastPublished` is the
  acceptance test for both; which of the two it exercises is decided
  entirely by the image `E2E_UPGRADE_FROM` names, so no second test
  exists. First evidence for the cross-branch path, local arm64,
  2026-09-16: a domain provisioned by `samba-ad-dc:4.23-dev`
  (Samba 4.23.12) started on the same volumes under `samba-ad-dc:dev`
  (Samba 4.24.7) — PASS in 27.2 s, the pre-upgrade user still resolvable,
  `dbcheck` clean, the state marker moved forward and `initialized_at`
  carried over rather than restamped.
  Every other path is **unsupported**: skipping a branch (4.22 → 4.24),
  and downgrading in any form. Nothing prevents a skipped-branch start
  mechanically — the guard in front of the database only refuses an
  *older* Samba (`TestDowngradeRefused`) — so "unsupported" here means
  untested and unclaimed, not blocked. An operator two branches behind
  upgrades one branch at a time.
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
- **The MS-SNTP signed-reply path is not exercised in CI.**
  `TestSignedNTPWiring` proves the wiring and the service: samba creates and
  serves the signing socket, chrony is configured against that same
  directory, and a client on the domain network gets a usable time
  measurement out of the DC. It does **not** prove a signed exchange — that
  needs a client authenticating as a domain machine account, which the
  protocol test-client is not. A regression in the signing path itself
  would therefore surface as a chrony log error rather than as a red test.
- **A restored DC does not have a provisioned DC's layout.**
  `samba-tool domain backup restore --targetdir=/var/lib/samba` writes a
  self-contained tree and an `smb.conf` whose paths point *into* it, so a
  restored DC keeps most of its state one level down. Measured on a restored
  DC that had reached healthy (`TestOfflineBackupRestore`): `state directory
  = /var/lib/samba/state`, `cache directory = /var/lib/samba/cache`, `lock
  directory = /var/lib/samba`, sysvol at `/var/lib/samba/state/sysvol` and
  netlogon under it.
  - *What does not move:* the MS-SNTP signing socket. The restored `smb.conf`
    does not set `ntp signd socket directory` at all, and samba's
    compile-time default for it does not track `state directory`, so it stays
    `/var/lib/samba/ntp_signd` — where a provisioned DC keeps it. This is
    recorded explicitly because the opposite was assumed during Phase 3 and
    is the natural conclusion to draw from the sysvol path; the measurement
    says otherwise. Signed NTP on a restored DC is asserted end to end by
    `TestOfflineBackupRestore` all the same.
  - *The caveat that remains:* paths a human or a script learned from a
    provisioned DC. samba itself is self-consistent — every path is read from
    `smb.conf`, which the restore rewrote — so nothing inside the container
    breaks. Operator documentation (Phase 5) must therefore derive locations
    from `smb.conf` rather than hardcode them: sysvol via
    `testparm -s --parameter-name="path" --section-name=sysvol` rather than
    `/var/lib/samba/sysvol`, and likewise for the state and cache
    directories. Any backup or GPO procedure that names a path must say
    which of the two layouts it assumes.

- **The rpc worker processes log a `reopen_one_log ... Read-only file
  system` complaint per boot**, attempting to open their own files under
  `/var/log/samba` (measured on a provision boot: 16 such failures —
  `samba-dcerpcd`, `rpcd_classic`, `rpcd_winreg`, `rpcd_lsad`,
  `rpcd_epmapper`, `rpcd_spoolss`, `rpcd_fsrvp`, `rpcd_mdssvc` — printed
  as 32 lines, each being a debug header plus its message). Accepted as
  cosmetic for v1: every real log line goes to stdout as §6.4 requires
  (samba runs `--debug-stdout`), the directory is deliberately absent from
  the writable set (B.2), and nothing is lost — the noise is the workers
  *reporting* that they will keep logging to stdout. Silencing it is
  deferred rather than attempted: the levers
  (`logging = `, a tmpfs at `/var/log/samba`, per-process log files) all
  redirect where diagnostics go, and the risk of quietly losing a class of
  log lines outweighs the cosmetic gain. Revisit if upstream separates the
  worker log-reopen path from the configured logging backend.

- **On branch 4.22 the offline RESTORE needs `CAP_DAC_OVERRIDE`; the
  running DC does not.** Measured locally on arm64, 2026-09-16, with
  `samba-ad-dc:4.22-dev` (Samba 4.22.11) under the constrained profile
  every E2E container runs in: `samba-tool domain backup restore` reaches
  the sysvol NT-ACL step, which goes through smbd, and dies there —

  ```text
  py_smbd_mkdir: mkdirat error=13 (Permission denied)
  ERROR(<class 'SystemError'>): uncaught exception - <built-in function mkdir> returned NULL without setting an exception
    File ".../samba/ntacls.py", line 631, in backup_restore
      smbd.mkdir(dst, session_info, service)
  ```

  — exiting 255, deterministically (3 runs of 3). The same restore
  succeeds with `DAC_OVERRIDE` added, and 4.23.12 and 4.24.7 need nothing
  added at all. The Phase 3 bisection that dropped `DAC_OVERRIDE` measured
  the 4.24 image, and a capability set is a property of an image.

  **Ruling (2026-09-16).** The B.2 capability set — the six measured
  capabilities — is unchanged on every branch, for every running DC,
  including a restored one: nothing showed that it has to change, and
  widening the set a DC *runs* under to accommodate a one-off recovery
  command would spend a real privilege permanently to buy a transient one.
  The widening is scoped to the restore container instead. **An operator
  restoring a 4.22 backup adds `--cap-add DAC_OVERRIDE` to the one-off
  container that runs `samba-tool domain backup restore`, and to nothing
  else** — not to the DC that afterwards serves the restored domain. On
  4.23 and 4.24 nothing is added at all.

  The E2E suite expresses exactly that through `E2E_RESTORE_CAPS`
  (`test/e2e/harness`): unset, the restore one-off gets the same
  capabilities as everything else; set, its comma-separated list applies
  to the restore one-off **only** — the backup one-off, the listing
  one-off and every DC keep `DefaultCaps`/`E2E_CAPS`.
  `TestOfflineBackupRestore` is the acceptance test on every branch, and
  the release workflow sets the variable for 4.22. What is proven per
  branch is therefore the procedure that branch's operators are told to
  run.

- **On branches 4.22 and 4.23 the DC's self-signed TLS certificate carries
  a byte-reversed serial, which is DER-negative about half the time.**
  Measured locally on arm64, 2026-09-16, by provisioning DCs from
  `samba-ad-dc:4.22-dev`, `:4.23-dev` and `:dev` and reading
  `/var/lib/samba/private/tls/cert.pem` with
  `openssl x509 -noout -serial`: 4.22.11 and 4.23.12 write the 32-bit
  generation time in **host byte order**, 4.24.7 writes it big-endian.

  The decisive capture is the pair taken inside one such window, eleven
  seconds apart. 4.23.12 produced `serial=-65235596`; `openssl` prints a
  negative serial as a signed **hex** magnitude, so those DER bytes are
  the two's complement of `0x65235596` over four bytes, i.e. `9ADCAA6A`.
  Reverse them and it is `0x6AAADC9A` — unix time 1789582490, inside the
  window the probe ran in. 4.24.7, eleven seconds later, produced
  `6AAADCA5` = unix 1789582501, big-endian and positive. Same clock, one
  branch storing it backwards.

  The consequence is mechanical: the leading DER byte of those serials is
  the *low* byte of the clock, which crosses 0x80 every 256 seconds, and a
  leading byte >= 0x80 makes the INTEGER negative. RFC 5280 §4.1.2.2
  requires a positive serial, so a strict parser refuses the certificate
  outright — Go's `crypto/x509` does.

  **What is not affected: anything a client does.** GnuTLS and OpenSSL
  accept a negative serial, so the TLS handshake succeeds, `ldapsearch`
  over `ldaps://` with `LDAPTLS_REQCERT=demand` against the DC's own CA
  succeeds, and no domain member sees a problem. The defect is in what the
  certificate *is*, not in what the DC does with it. 4.24 is unaffected —
  its leading byte stays below 0x80 until 2038.

  **Ruling (2026-09-16).** The limitation is recorded rather than worked
  around: it is upstream's, on branches that receive only bug and security
  fixes. `TestLDAPSCertificate` is made to survive it without giving up
  the property it exists to assert. Its in-process inspection tolerates a
  negative serial **and that one thing only** — any other parse failure is
  still fatal, and wherever the certificate parses (always, on 4.24) the
  CN check and the full chain verification run exactly as before. Where it
  does not, the test logs why and falls back on its other half: the
  over-the-wire `ldapsearch` pair, which verifies with `REQCERT=demand`
  against the DC's CA as the only trust anchor and proves, through its
  negative control, that the client is really checking. That half asserts
  the same two properties — issued by this DC's CA, valid for this DC's
  name — through the stack a domain member actually uses, so the coverage
  moves rather than disappears, and the test is deterministic on all three
  branches.

- **No `nsupdate` in the image** (`bind9-dnsutils` is not installed).
  Dynamic DNS updates therefore run through
  `samba_dnsupdate --use-samba-tool`, which the entrypoint pins (Phase 2);
  a configuration that would require the external updater is unsupported.

### B.7 Catalog

All Samba stable branches currently supported upstream are published
simultaneously (§3.6), each receiving every patch release; branch
lifecycle relayed per §9.4 (release candidates of a new series trigger the
deprecation notice for the oldest branch).

Activated branches, pinned in `versions.yaml`, with upstream's status **as
observed on 2026-09-16**:

| Branch | Upstream patch level pinned | Upstream status on 2026-09-16 |
|--------|-----------------------------|-------------------------------|
| 4.24 | 4.24.7 | current — default branch, owns the `4` and `latest` aliases |
| 4.23 | 4.23.12 | maintenance |
| 4.22 | 4.22.11 | security fixes only — deprecation pending (4.25 rc published) |

Each of those tarball checksums was produced by
`sh scripts/verify-upstream-tarball.sh <X.Y.Z>`, which repeats the
builder stage's own gunzip-then-OpenPGP chain against the pinned
fingerprint before the value is allowed into the catalog.

**The rule the published matrix follows.** The README matrix is generated
(`catalog.py render-matrix`) and the status column is *positional*, not
stored: branches sorted newest first, the first is `current`, the second
`maintenance`, the third `security fixes only`, and a fourth or older
would read `discontinued (EOL)`. The catalog's branch list is the only
input, so nothing here decides upstream's lifecycle — the watcher relays
it by adding a series or dropping the oldest, and a release candidate for
a newer series appends a deprecation-pending note to the oldest supported
row. The table above is a dated snapshot for a reader of this file, kept
in step by hand; the README matrix is the rendered, always-current
statement. They agree because both describe the same `versions.yaml`, not
because one is generated from the other.

The 4.22 row carries that deprecation-pending suffix as of 2026-09-16
because `samba-4.25.0rc2.tar.gz` is published under
`https://download.samba.org/pub/samba/rc/`. A release candidate for a
series newer than the newest catalog branch is upstream's own end-of-life
signal for the oldest branch it still supports, and §9.4 requires it to be
relayed as soon as it is detected — so it is relayed now, by hand
(`catalog.py update-readme-matrix --rc-series 4.25`), and by the watcher's
rc probe from Phase 4 on. It is a *notice*, not a removal: 4.22 keeps
receiving every patch release until upstream actually ends it, at which
point the branch leaves the catalog and the matrix says
`discontinued (EOL)`.

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

**The base image's own packages are part of that content.** The runtime
stage runs `apt-get upgrade` before it installs the manifest, so the
packages inherited from `debian:trixie-slim` — `gzip`, `perl-base`,
`libsqlite3-0`, `libpcre2-8-0` and the rest of the base's 78 — are
brought to the state of the package index at build time rather than left
at whatever the base image last shipped. Without it, a Debian security
update to a base package reached this image only when Debian republished
the base, because `apt-get install` of the manifest does not upgrade a
package that already satisfies the request: measured on 2026-09-16, that
left 12 fixable HIGH/CRITICAL CVEs in the 4.24.7 image, three of them
CRITICAL, whose fixes were already in the archive the build queries.
`upgrade` and not `dist-upgrade` on purpose — it installs no new package
and removes none, so the shipped package **set** is still exactly
`base ∪ apt closure of runtime-packages.txt`, which is what
`scripts/check-image-packages.sh` proves.

This does not weaken the claim above, it completes it. The third input,
the package-index hash, is defined by `scripts/pkg-closure-hash.sh` as the
union of two dry-runs inside the pinned base — the `upgrade` set *and* the
manifest's install closure — rendered as `<name> <version>` pairs. So the
base-package upgrades are inside the hash, not beside it: two builds that
agree on commit, base digest and hash resolve the same versions for the
base packages as well as for the manifest, and a security fix to a base
package moves the hash, busts the package layer (§9.3) and makes the
rebuild owed (§9bis.1.c) even though the base digest has not moved. The
resolved versions are recorded in the published SBOM exactly as before;
what changed is that the SBOM now records an upgraded base, and that the
hash can see it.

## Runtime contract

What the image accepts, what it does with it, and how it reports failure.
This section is the contract: the entrypoint's unit tests assert these
semantics and these exit codes, and the E2E matrix (B.5) exercises them
against a running container. Exit codes are **immutable once released**.

### Environment variables

| Variable | Modes | Default | Meaning |
|---|---|---|---|
| `SAMBA_MODE` | all | `auto` | `auto|provision|join|run|maintenance` |
| `SAMBA_REALM` | provision, join (and auto reaching them) | — required | Kerberos realm / AD DNS domain, e.g. `AD.EXAMPLE.COM` |
| `SAMBA_DOMAIN` | provision | first label of realm | NetBIOS domain name |
| `SAMBA_ADMIN_PASSWORD_FILE` | provision | — required | file with the initial Administrator password |
| `SAMBA_JOIN_USERNAME` | join | `Administrator` | account used to join |
| `SAMBA_JOIN_PASSWORD_FILE` | join | — required | file with the join account password |
| `SAMBA_DNS_FORWARDER` | provision, join | none | upstream DNS forwarder IP (see below — not optional for a DC that resolves through itself) |
| `SAMBA_DNS_BACKEND` | provision, join | `SAMBA_INTERNAL` | only `SAMBA_INTERNAL` supported in v1 |
| `SAMBA_FUNCTION_LEVEL` | provision, join | `2016` | AD functional level; also mirrored onto the `ad dc functional level` smb.conf parameter for `2012`, `2012_R2` and `2016` — by provision through `--option`, by join through the post-join edit (see below) |
| `SAMBA_LOG_LEVEL` | all | `1` | samba debug level |
| `SAMBA_CHRONY` | auto/provision/join/run | `on` | serve MS-SNTP signed time (`on|off`) |
| `SAMBA_GLOBAL_OPTIONS` | auto/provision/join/run (not maintenance) | none | newline-separated `key = value` smb.conf `[global]` settings, reconciled on every start and validated by `testparm` (see below) |
| `SAMBA_MAINTENANCE_OP` | maintenance | `check` | `check` (dbcheck) or `repair` (dbcheck --fix --yes) |

Secrets are accepted **only** through the `*_FILE` variables (§6.1).
Setting a plain `SAMBA_ADMIN_PASSWORD` or `SAMBA_JOIN_PASSWORD` in the
environment is refused with exit 10 and a message naming the `_FILE`
variant; no secret value is ever logged.

**`SAMBA_FUNCTION_LEVEL` is mirrored onto smb.conf.** Provision passes
`--function-level=<level>` *and*, for `2012`, `2012_R2` and `2016`,
`--option=ad dc functional level = <level>`, so the value lands in the
generated `smb.conf` and every later start keeps the level the domain was
created at. This is not cosmetic: since Samba 4.19 that parameter defaults
to `2008_R2`, and provision refuses outright when the requested domain and
forest level is higher than the DC's own level — so a provision at this
image's `2016` default fails without the mirror. Levels at or below the
default get no `--option` at all: the parameter does not accept `2000`,
`2003` or `2008` as values, and setting it there would turn a working
provision into a configuration error.

**A join gets the same settings, through the generated `smb.conf`.**
`samba-tool domain join` renders its own configuration file from a template
and has no `--option` passthrough, so the entrypoint edits that file once,
atomically, immediately after the join and before any daemon starts. Three
settings are forced in — `dns update command`, `ad dc functional level` (on
the same levels provision mirrors) and `dns forwarder` (whenever
`SAMBA_DNS_FORWARDER` is set) — each announced on its own log line with the
reason. Every edit is idempotent and scoped to `[global]`, and a `[global]`
entry carrying a *different* value is replaced rather than trusted: a
functional level below the domain's stops samba from starting at all, and a
forwarder pointing at an upstream that never answers is worse than none.
Without this, `SAMBA_DNS_FORWARDER` and `SAMBA_FUNCTION_LEVEL` would be
silently ignored on exactly the DC an operator cannot fix them on
afterwards.

**`SAMBA_DNS_FORWARDER` is not a nicety on a multi-DC domain.** Samba's
internal DNS with no upstream takes *seconds* to fail a query it is not
authoritative for (measured at 4-8 s against this image) instead of
answering immediately. A DC that resolves through itself — which a multi-DC
domain requires, because a replication partner is addressed by a `_msdcs`
CNAME only the directory's own DNS can answer — then pays that stall on
every Kerberos bind, and the sealed DRSUAPI bind that carries replication
times out before it completes. Set it to a resolver that is **not** this
DC, or the domain replicates erratically or not at all.

**Declarative configuration: `SAMBA_GLOBAL_OPTIONS`.** The variables above
each own one setting. Everything else an operator may legitimately want in
`[global]` — `log level`, `max log size`, an `idmap config` line — goes into
this one variable, as newline-separated `key = value` entries (a compose `|`
block scalar is the intended form). Blank lines and lines starting with `#`
or `;` — smb.conf's own two comment characters — are ignored; keys are
normalized to lower case and single spaces; a key set twice keeps its last
value and the repetition is logged. A line that
is not a `key = value` pair is refused with exit 10 rather than skipped — an
option that silently never reaches `smb.conf` is invisible until the day it
was supposed to matter.

*Owned keys are refused, naming their owner.* `realm` (`SAMBA_REALM`),
`workgroup` (`SAMBA_DOMAIN`), `netbios name` (the container hostname),
`ad dc functional level` (`SAMBA_FUNCTION_LEVEL`), `dns forwarder`
(`SAMBA_DNS_FORWARDER`), and `server role`, `dns update command`, `ntp
signd socket directory` and `include`, which the image manages itself.
`tls certfile` / `tls keyfile` / `tls cafile` are refused too, reserved for
the `SAMBA_TLS_CERT_FILE` / `SAMBA_TLS_KEY_FILE` / `SAMBA_TLS_CA_FILE`
variables that own them — refused from the moment this variable exists, so
that no deployment can come to depend on setting them by hand first. Each is exit 10 with a message
naming what to set instead: an entry quietly overriding one of them would
either contradict the variable that owns it or break the DC outright.

*Applied to `[global]`, before any daemon starts.* Provision passes every
entry to `samba-tool` as `--option=key = value`, so the file it generates
already carries them; and every start — the one that just provisioned or
joined included — reconciles them into the `smb.conf` on the configuration
volume in one atomic rewrite, before any daemon reads it. Each add or replace is one log
line — `entrypoint: SAMBA_GLOBAL_OPTIONS: added "max log size" = "10000"
in /etc/samba/smb.conf` — and a start that changes nothing writes nothing
and says nothing. Maintenance mode applies none of it: an operator reaching
for it is diagnosing a DC that will not run, and a mode that edited the
configuration on the way past would change what they are looking at.

*This is configuration, not state (§6.2).* The state a restart never
modifies is the directory database on the state volume; the `[global]`
settings are configuration, reconciled on every start, which is what makes
`SAMBA_GLOBAL_OPTIONS` editable on a DC that already exists. Changing the
variable and recreating the container is the whole procedure, and the log
says what changed.

*Reconciliation adds and replaces; it does not remove.* Deleting an entry
from the variable leaves the line it wrote in `smb.conf`, because the
entrypoint has no way to tell a setting it wrote last boot from one the
operator put there by hand, and guessing wrong would silently drop somebody
else's configuration. To undo a setting, give it the value you want —
samba's default, written out explicitly — or edit `/etc/samba/smb.conf` on
the configuration volume.

*Every rewrite is gated by samba's own parser.* After a rewrite the
entrypoint runs `testparm -s -l --debug-stdout <smb.conf>`; if it reports a
problem, the **previous bytes are written back** and the boot refuses with
exit 10, quoting testparm's own words. The edit is announced only after the
gate passes, so the log records what is in force and never a change that was
rolled back. The restore is the point: `smb.conf` lives on a volume, so a
rejected rewrite left in place would break every later start, including the
one made right after removing the offending entry.

What counts as a rejection is measured against Samba 4.24.7 in this image,
because the obvious reading of testparm's output is wrong in both
directions. An unknown parameter prints `Unknown parameter encountered: "…"`
and **exits 0**, so the exit code alone would let a typo through; an invalid
value prints `WARNING: Ignoring invalid value …` and exits 1. But a
*deprecated yet perfectly valid* parameter — `syslog only`, `lanman auth`,
`domain logons` and others — prints `WARNING: The "…" option is
deprecated` and also exits 0, so treating `WARNING` as a verdict would
refuse to boot a DC whose configuration samba loads without complaint. The rule is therefore:
a non-zero exit, or the unknown-parameter line. Everything else testparm
says is copied to the container log and the boot continues. All of it is
DEBUG output, which samba writes to stderr — hence `--debug-stdout`, which
is what makes the diagnostics readable by the entrypoint.
*Covered by:* `TestGlobalOptionsApplied`.

**`KRB5_CONFIG` is set in the image** to
`/var/lib/samba/private/krb5.conf`, the Kerberos configuration both
provision and join generate for the realm. See the Runtime contract's
**In-container Kerberos** below.

### Argv

| Argument | Meaning |
|---|---|
| *(none)* | the container's `ENTRYPOINT`: load, observe, decide, initialize if needed, supervise |
| `healthcheck` | the image's `HEALTHCHECK` probe (see below) |
| `--version` | print the Samba version embedded at build time (`-ldflags -X main.sambaVersion=<v>`) and exit `0` |

Anything else is refused with exit 10. `--version` exists for the build
and CI guards: the version guard on the state volume is only as
trustworthy as that injected value, so CI asserts that what the entrypoint
reports matches the version pinned in `versions.yaml` — a binary built
without the flag reports `unset` and is refused by the guard rather than
silently trusted.

### Mode semantics (B.4)

- `auto`: state present → behave as `run`; state absent → `join` if
  `SAMBA_JOIN_PASSWORD_FILE` is set, else `provision`.
- `provision`: state present → exit 20. Else `samba-tool domain
  provision`, write marker, start daemons.
- `join`: state present → exit 20. Else `samba-tool domain join ... DC`,
  write marker, start daemons.
- `run`: state absent → exit 21 ("volume missing or not mounted —
  mount the /var/lib/samba volume, or run an initialization mode").
  Else guards, then start daemons.
- `maintenance`: state absent → exit 21. Else run dbcheck (or --fix),
  print summary, exit without starting daemons (0 on clean, 23 on
  failure). The §7.2 version guards below apply in maintenance mode too:
  a volume written by a newer Samba is refused with 22 and a malformed
  marker with 10, *before* any dbcheck runs. That is the point rather
  than an oversight — `dbcheck --fix` driven by an older Samba against a
  database written by a newer one is the exact hazard the downgrade
  guard exists for, and "repair" is the mode an operator reaches for
  when something is already wrong.

### State & guards

- State present ⇔ `/var/lib/samba/private/sam.ldb` exists.
- Marker `/var/lib/samba/.image-state.json`:
  `{"samba_version": "4.24.6", "initialized_at": "<RFC3339>", "last_mode": "provision"}`.
- Marker version > image version ⇒ exit 22 ("state was written by Samba
  X — deploy image tag X or newer, or restore a backup taken on this
  version").
- Marker version < image version ⇒ upgrade path: `samba-tool dbcheck`
  first; failure ⇒ exit 23; success ⇒ marker updated to image version,
  then start.
- State present but marker absent (foreign/pre-existing volume): log a
  warning, run dbcheck (failure ⇒ 23), adopt by writing the marker.
- Timestamps come from the clock at runtime; version from a var set at
  build (`-ldflags -X main.sambaVersion=<v>`).
- A restart never modifies existing state (§6.2): the marker is written
  only after a successful initialization or a successful upgrade check.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | success |
| `10` | configuration error |
| `11` | missing/unreadable secret file |
| `20` | provision/join refused over existing state |
| `21` | run mode with absent state |
| `22` | downgrade refusal |
| `23` | database consistency check failure |
| `30` | samba runtime failure |

Every failure message is **one line, cause then remedy, separated by
`;`**, written to stderr and prefixed `ERROR: ` (§6.5). Logs go to stdout/stderr exclusively
(§6.4); samba runs `--foreground --no-process-group --debug-stdout`.

### Process model and shutdown

`tini` is PID 1 (`ENTRYPOINT ["/usr/bin/tini", "--",
"/usr/local/bin/entrypoint"]`) and reaps what Samba's process model
leaves behind. The entrypoint starts `chronyd` first and `samba` second;
SIGTERM (or SIGINT) stops **samba first, then chronyd**, and a container
asked to stop exits `0` (§6.3).

The two stops share **one 10 s budget**, they do not each get their own:
§6.3 gives the container 10 s to stop samba *and* chrony, so per-daemon
windows would add up past what the contract allows and the container
would be SIGKILLed by the runtime mid-shutdown. samba takes whatever it
needs of the budget first and chronyd gets the remainder. **Everything
fits inside that one budget**, the SIGKILL escalation for a daemon that
ignored SIGTERM included; a daemon that cannot even be reaped is left to
the init process rather than blocking the other's stop.

Losing chronyd alone does not take the DC down — signed NTP stops being
served and the event is logged.

### Health check

```
HEALTHCHECK --interval=30s --timeout=10s --start-period=180s --retries=3
    CMD ["/usr/local/bin/entrypoint", "healthcheck"]
```

`entrypoint healthcheck` is an **application-level** probe, not a process
check (§5.5). It reads the realm from `/etc/samba/smb.conf` and then, on
the loopback address only, asks the three protocols a domain member uses,
in the order it uses them: the `_ldap._tcp.<realm>` SRV record on
`127.0.0.1:53`, an anonymous rootDSE read on `ldap://127.0.0.1:389`, and
a share enumeration with `smbclient -L 127.0.0.1 -N`. It exits `0` only
when all three answer and `1` otherwise — docker's healthy/unhealthy
values, never the refusal codes above. The 180 s start period is
deliberate: a first-boot provision on a cold volume legitimately takes
minutes, and a container declared unhealthy mid-provision would be
restarted into a half-initialized state.

### Time service

`chronyd` is started with `-d -x -f /run/chrony/chrony.conf`: `-x` so it
never disciplines the host's clock (B.3), `-d` so it logs to the
container's stderr. 123/udp is exposed. `SAMBA_CHRONY=off` runs the DC
without it.

That configuration is **generated at daemon-start time**, not baked. The
image bakes a *template* at `/etc/chrony/chrony.conf` (read-only rootfs);
before starting the daemon the entrypoint copies it to
`/run/chrony/chrony.conf` and rewrites the single line that cannot be a
constant — `ntpsigndsocket` — to whatever this DC's own `smb.conf`
declares as `ntp signd socket directory`, read back through `testparm`.
The template's value (`/var/lib/samba/ntp_signd`, samba's compile-time
default) is the fallback when `testparm` gives no usable answer, and the
fallback is logged. Everything else in the template is copied verbatim,
drift and pid paths included.

The reason is that the directory is **not** the image's to decide: `ntp
signd socket directory` is an ordinary `smb.conf` parameter and `/etc/samba`
is a volume the operator owns. Nothing in samba or in chrony reconciles the
two files, so a baked `chrony.conf` would keep naming samba's default on a
DC whose `smb.conf` says otherwise — and say nothing about it, because
chrony opens the signing socket lazily, only when a request carrying an
authenticator arrives. Silent failure is the one failure mode a time service
must not have, so the entrypoint asks rather than assumes.

Note what this does *not* address, since the restore path was suspected and
then measured: a DC restored with `samba-tool domain backup restore` keeps
this socket exactly where a provisioned one does (B.6). The restore
relocates state, cache, lock and sysvol, but leaves this parameter unset,
and samba's default for it does not track `state directory`.

### In-container Kerberos

The image ships **no `/etc/krb5.conf`** and sets
`KRB5_CONFIG=/var/lib/samba/private/krb5.conf` instead — the file both
`samba-tool domain provision` and `samba-tool domain join` generate for the
realm and print the location of.

This matters for anything Kerberos run *inside* the container, including an
operator's own `docker exec ... samba-tool drs showrepl`. Without it the
bundled Heimdal discovers the realm through DNS, walking `_kerberos.` up
the parent domains of the host name — names the directory is not
authoritative for. On a DC that resolves through its own internal DNS those
queries take seconds each (see `SAMBA_DNS_FORWARDER` above), several per
bind, and the Kerberos-sealed DRSUAPI bind that carries replication times
out before they finish. The generated file sets `dns_lookup_realm = false`
and removes the walk.

It is an image `ENV`, not something the entrypoint exports, because
`docker exec` inherits the image environment and **not** the environment of
PID 1 — that is the only form which also reaches commands an operator runs
in the container. Pointing at the file before the first provision created
it is harmless: Kerberos falls back to its built-in defaults.

## Release cycle

The release cycle is automated end to end and the automation lives in this
repository: `.github/workflows/upstream-check.yml` is the watcher,
`scripts/watch.py` is what it decides, `scripts/catalog.py` is what it
edits, and `.github/workflows/release.yml` is what it dispatches. SPEC
§9bis ratified an out-of-repo daemon and roadmap decision D4 placed it in
a separate repository; that decision was **reversed on 2026-09-16** in
favour of an in-repo workflow. The contract is unchanged — the difference
is that the state the watcher remembers is a tracked file anyone can read,
and every decision it makes is a commit with a cause in its message.

### What is watched

Four sources, probed once per run by `watch.py observe`:

| Source | Probe | SPEC |
|--------|-------|------|
| Upstream releases | `https://download.samba.org/pub/samba/stable/`, tarball names only, one `latest_patch` per catalog branch | §9bis.1.a |
| Upstream candidates | `https://download.samba.org/pub/samba/rc/`, tarball names only | §9bis.1.e |
| Base images | `docker buildx imagetools inspect <tag>` for each of `base.builder`, `base.runtime`, `base.gobuild` | §9bis.1.c |
| Package closure | `scripts/pkg-closure-hash.sh <base.runtime>` | §9bis.1.c |

Plus one probe of our own publications: the tags already on GHCR
(`gh api /orgs/esitc-paris/packages/container/<image>/versions`), which is
what makes the first publication and a lost dispatch self-healing rather
than a manual step. A package that has never been published answers 404,
which reads as "no tags"; every other failure is a failure.

Both listings are requested conditionally (`If-None-Match` /
`If-Modified-Since`) and the validators are stored under `sources.*` in
`.build-state.json`, per §9bis.2. Measured 2026-09-16:
`download.samba.org` sends neither `ETag` nor `Last-Modified` on these
listings and answers a conditional request with a full 200, so the cache
is inert today — the request is made anyway because it costs nothing and
starts working by itself the day the server grows a validator. The
listings are fetched gzip-encoded (319 KB plain, 14 KB gzipped).

Two probes are deliberately **not** made. The Samba announcement mailing
list is not read by any code (B.6), and no CVE feed is polled: a CVE
without an available fix can never trigger a build, and one with a fix
arrives as a package-closure delta, which is already probed.

### The decision, in order

For every catalog branch, first match wins:

1. **A strictly greater upstream patch release, past its soak** →
   `action=version`, `cause=samba-release`. Strictly greater under numeric
   comparison (`4.24.10` is above `4.24.9`), and release candidates are
   excluded by construction: the regex requires `.tar.gz` immediately
   after the patch number. A reported version *below* the pin is ignored
   with a warning and never acted on — a partial listing must not be able
   to publish a downgrade as `:latest`.
2. **A moved base-image digest** → `action=revision`, `cause=base-digest`.
3. **A moved package-closure hash** → `action=revision`,
   `cause=pkg-update`.
4. **The catalog's current `X.Y.Z-rN` is not on the registry** →
   `action=publish`, `cause=first-publication`. Nothing is edited: the tag
   is created if missing and the release is dispatched. This is the state
   of every branch until the first release, and the reason no bootstrap
   procedure exists.
5. Otherwise → `action=none`.

One rule runs before 2 and 3: a branch the watcher has **never observed**
— no digests and no closure hash in `.build-state.json` — and whose
catalog tag is **not yet published** has nothing to compare against, so
its observation is recorded without being called a change and the run
falls through to 4, whose first publication is the build those values
describe. Once the tag is published the exemption ends: an empty state
entry is then compared like any other and yields a `revision`. It has to,
because a decision's recorded observation is written into `versions.yaml`
as well as into `.build-state.json` — recording it quietly on a published
branch would pin a base that no published image was built from and lose
that cycle's rebuild.

An empty answer from a probe is never a change: an empty digest compares
unequal to the stored one, and writing it would make every later run bump
again. `observe` fails the run instead, and `decide` refuses to act on one
as a second line of defence.

### Soak, and the two ways past it

§9bis.5 ratifies a **24-hour soak** on a new upstream version, to absorb
upstream retags and withdrawn releases. The first run that sees a higher
patch release records `pending: {version, first_seen}` in
`.build-state.json` and does nothing else; the release is decided on the
first run at least `RELEASE_SOAK_HOURS` (repository variable, default 24)
later. A newer version appearing mid-soak restarts the clock on the new
version rather than inheriting the old one's; a version that disappears
from the listing is forgotten.

Two things bypass it. A **security release** does: dispatching the watcher
with `security_release=true` sets the soak to 0 h (§9bis.5). And a
**maintainer** does, by editing `versions.yaml` and pushing a tag by hand —
`release.yml` keeps its `push: tags` trigger for exactly that.

### The necessity criterion (§9bis.8)

A release is dispatched only when the image is **certain** to differ from
the last published one: a new upstream version, a new base digest, or a
new package closure — nothing else. The two halves of the package
criterion are both load-bearing and are documented at the probe itself
(`scripts/pkg-closure-hash.sh`): the hash covers the union of an `upgrade`
dry-run and an `install` dry-run, so a security fix to a package the base
image already carries moves it, and a republication of the index that
changes no version does not (§9bis.8.b).

A detected upstream release is **confirmed against the authoritative
source before anything is written** (§9bis.8.a):
`scripts/verify-upstream-tarball.sh` downloads the tarball and its
signature and verifies it against the pinned release key, and its output
is where the new `tarball_sha256` comes from. A checksum is a
trusted-forever pin; it is never minted from an unverified download.

### What the watcher writes, and where

`.build-state.json` is the watcher's memory: per branch the three digests,
the package-closure hash, the tag last dispatched and any version still
soaking; at the top level, the conditional-request cache per source. It is
machine-owned and rewritten whole, and every value in it is a fact about
the outside world — the cache holds the validators and the parsed version
list and nothing else, deliberately not a "last checked" timestamp. A
field that moves because time passed would make this file differ on every
run, which is a commit and a push to `main` every hour saying nothing.

`versions.yaml` is the pin contract, and the watcher edits it through
`catalog.py` — the same entry point a human uses — so the file's comments
survive and every write is re-parsed before it is accepted. A version bump
also moves the Dockerfile's `ARG` defaults for the **default branch**,
because those defaults mirror that branch's pins so a bare `docker build .`
reproduces the pinned build, and `scripts/check-pins-consistency.sh`
asserts every pair. The catalog and its mirror therefore move in one
commit or CI is red on the watcher's own push. `ARG PKG_INDEX_HASH` is
excluded from that mirror on purpose: its default is a bare fallback and
CI injects the catalog value.

**State-only commits.** A branch that is soaking, a refreshed source cache
and a CVE-ledger sync all change tracked files without owing a release. The
watcher commits them as `chore(watch): state update [skip ci]` and pushes
`main` with no tag and no dispatch — without which `first_seen` would be
lost and the soak would restart on every hourly run. An unchanged tree
yields no commit, which is what makes the hourly run idempotent (§9bis.3)
and is asserted by a unit test: two `observe` runs against identical
answers leave the state file byte-identical.

**A tag that already exists is never re-created.** The `publish` decision
is replayed every hour until the image is on the registry, so the tag it
names may already be there from a run whose dispatch was lost. `apply`
splits the decided tags into the ones the remote carries and the ones it
does not (`git ls-remote --tags`); only the new ones are created and
pushed, and all of them are dispatched — "the tag exists" and "the image
is published" are different facts, and only the second one was checked.
Without that split the self-heal would work exactly once: the second
attempt would push a tag name the remote already has, `--atomic` would
reject `main` along with it, and every later run would fail, for every
branch. Each tag is also dispatched on its own, so one branch whose
dispatch will not take cannot hold back another branch's release.

That tolerance is `publish`-only. A `version` or `revision` bump that
lands on a tag the remote already carries is refused by name: a bump
claims to be producing a new release, so an existing tag means the catalog
is behind the remote, and a published tag's contents are immutable (§3.2).
The refusal states the two ways out — delete the stale tag if nothing was
published under it, or bump the revision past it.

### What needs a human

- **A new upstream series.** When the highest series in the stable listing
  is not a catalog branch, the watcher opens one deduplicated issue,
  `New upstream series X.Y — catalog decision required`, and stops there.
  Adding a series is a commitment to publish it on every release (§3.6).
- **Removing a branch.** Never automatic. A release candidate for a newer
  series is relayed as a *notice* — the README matrix gains the §9.4
  deprecation-pending suffix on the oldest branch and one issue is opened
  — and that branch keeps receiving every patch release until upstream
  actually ends it.
- **Renewing a CVE review date.** The ledger is synchronised by
  `scripts/cve-ledger.py` and the dates in it are never moved by a sync;
  `scripts/check-cve-exceptions.py` fails the build once one has passed
  (§5.4).
- **Anything the watcher refused.** Every refusal is a failed run, and a
  failed run opens (or comments on) one issue assigned to the maintainer.

### The CVE ledger is synchronised, not hand-written

§9bis.1.d makes the CVE feed **advisory**: a finding with no available fix
can never trigger a build. Once per run, for every branch whose current tag
is published, the watcher reads that image's published SBOM — a few MB,
against ~540 MB for the image — scans it with the same trivy release the
gates use, and runs `scripts/cve-ledger.py sync`, which rewrites
`security/cve-exceptions.yaml` as **one entry per affected package**, not
per CVE. Ruling R-ledger (2026-09-16), on a measurement: the 4.24.7 image
carries 287 findings with no available fix, 77 of them HIGH/CRITICAL, over
27 packages. One entry per CVE is a document nobody re-reads, and a ledger
nobody re-reads is the permanent suppression list §5.4 forbids. A package
appearing for the first time opens one deduplicated issue per branch; a new
CVE on a package already listed does not. Failure of this step is logged
and never fails the run.

### The package-closure hash is measured on one architecture

The probe runs on the watcher's runner, which is amd64, against the pinned
runtime base. Debian's package versions are architecture-uniform apart from
binNMUs, so an arm64-only binNMU is invisible to it. The consequence is
narrow and worth stating plainly: the hash decides **when** a rebuild is
owed, never **what** gets installed. Every release builds both
architectures natively against the index as it exists at build time, so an
arm64-only package movement ships with the next rebuild whatever triggered
it — it just does not trigger one by itself.

### Scheduling

There is no `schedule:` trigger. §9bis.7 requires a real execution
guarantee at the polling frequency and GitHub's cron provides none — runs
are delayed or dropped under load, and the trigger is disabled after 60
days of repository inactivity. The watcher is dispatched hourly through the
API by a timer on our own infrastructure (`docs/operations.md`). The
trigger does not need to be trusted, only the executor (§4.5): the worst a
compromised trigger can do is start runs that publicly pass or fail the §8
gates.

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
- 2026-08-16: Phase 2 — the entrypoint state machine of B.4 is
  implemented and wired as the image's ENTRYPOINT and HEALTHCHECK. New
  **Runtime contract** section records the binding form of that contract
  (environment variables, mode semantics, state and version guards, exit
  codes, process model and shutdown order, health check, time service);
  it is copied verbatim from the Phase 2 plan's behavior contract, and the
  unit suite is what holds code and profile to it. Two facts learned from
  the first real provision are folded in: `python3-markdown` is a runtime
  dependency of `samba-tool domain provision` (ForestUpdate/DomainUpdate
  read the MS update tables out of markdown), and chronyd runs as root
  because the B.2 capability set excludes CAP_KILL — a privilege-dropping
  chronyd cannot be stopped in order, nor reach samba's signing socket.
- 2026-08-16: Phase 2 final-review amendments. B.6 restates the §6.7
  ruling precisely (the shell-entrypoint *exception* is what is not
  required; §6.7 itself is satisfied, with the in-image and CI unit-test
  gates as evidence) and the root-chronyd ruling stops being presented as
  settled: it now carries an explicit REVISIT at the Phase 3 capability
  bisection, with `CAP_KILL` plus signing-socket group permissions as the
  named alternative. B.2 gains the `/run/chrony` ownership and permission
  story; B.3 cross-references the Phase 2 implementation of its Time and
  Secrets constraints. Runtime contract gains the `--version` argv, the
  `ad dc functional level` mirror behind `SAMBA_FUNCTION_LEVEL` (without
  which provision at the 2016 default is refused since Samba 4.19), the
  exact refusal-message shape (one line, cause then remedy, `;`-separated)
  and the statement that the §7.2 version guards apply in maintenance mode
  too.
- 2026-08-16: Phase 3 — B.2 writable paths gain tmpfs `/var/cache/samba`,
  resolving the Phase 2 hand-off about winbindd's `netsamlogon_cache`
  under a read-only rootfs. Decided by A/B provision on the constrained
  profile (arm64, local): without the tmpfs the boot log carries three
  `netsamlogon_cache` failures, with it zero, and a normalized diff of the
  two boot logs shows no other difference; the DC reached healthy in both.
  The E2E harness starts every DC container with that tmpfs, so the
  three-path writable set is now proven on every run.
- 2026-08-16: Phase 3 — **the B.2 capability set stops being a hypothesis
  and becomes a measurement.** `test/capbisect/bisect.sh` re-runs the E2E
  smoke subset (provision, `kinit`, restart) once per capability with that
  capability removed, then re-runs it with only the ones that proved
  required; report in `test/capbisect/results-arm64-2026-08-16.txt` (local
  arm64, image digest `sha256:198e52c9…`; committed at the time as
  `results-arm64.txt`, and kept under its date when the CI report took
  that name over — see the 2026-09-16 entry below). Two changes to what the profile said:
  `DAC_OVERRIDE` leaves the set (the suite passes without it — everything
  runs as uid 0 over paths the entrypoint already chowned to itself), and
  `DAC_READ_SEARCH`, previously carried as a candidate to bisect, is
  recorded as not needed. `NET_BIND_SERVICE` measured droppable and was
  **kept anyway**, on evidence rather than caution: docker sets
  `net.ipv4.ip_unprivileged_port_start=0` in a container's network
  namespace, so the suite could never need it, while a `bind()` of :389 in
  this image with the floor back at the kernel default is denied without
  the capability and succeeds with it — and B.3 supports host networking,
  where that floor is the host's. The B.6 REVISIT on root-chronyd is
  closed with the ruling unchanged: `CAP_KILL` is not applicable while
  chronyd does not drop privileges, so it is recorded rather than
  measured. `harness.DefaultCaps` now carries the established set, so
  every E2E run re-proves its sufficiency, and the driver ends by
  comparing its own recommendation to `DefaultCaps` and exiting non-zero
  when they differ — so a dispatched CI run answers "is B.2 still true",
  not merely "did the bisection finish". CI confirmation on both
  architectures awaits the first `workflow_dispatch` run of
  `.github/workflows/capbisect.yml`.
- 2026-09-16: Phase 3 final wave. **The chrony configuration becomes a
  template, and a Phase 3 hypothesis is withdrawn.** The suspicion that
  drove this change — that `samba-tool domain backup restore` moves the
  MS-SNTP signing socket to `/var/lib/samba/state/ntp_signd`, leaving
  signed NTP silently dead on every restored DC — was **falsified** by the
  restore test itself: the restored `smb.conf` does not set `ntp signd
  socket directory`, samba's compile-time default for it does not track
  `state directory`, and the live socket sits at `/var/lib/samba/ntp_signd`
  like a provisioned DC's. There was no restore bug. The change is retained
  on a different and verifiable argument: that parameter is the operator's
  to set on the `/etc/samba` volume, nothing reconciles `smb.conf` with
  `chrony.conf`, and chrony opens the socket lazily — so a baked path fails
  silently rather than loudly. The entrypoint therefore generates
  `/run/chrony/chrony.conf` from the baked template before starting
  chronyd, rewriting `ntpsigndsocket` to what this DC's own configuration
  declares (read back with `testparm -s -l`, falling back to the template's
  value and logging when it cannot be read), and starts chronyd with `-f`
  on the generated file — see **Time service**. `TestSignedNTPWiring` and
  `TestOfflineBackupRestore` both assert the two files agree on the DC's
  own value and that the service answers. B.6 gains the restored-layout
  entry, restated around what was measured: state, cache, lock and sysvol
  relocate under `/var/lib/samba/state`, the signing socket does not, and
  what needs the caveat is operator documentation, which must derive those
  paths from `smb.conf` rather than hardcode them. B.6 also records the
  accepted `reopen_one_log ... Read-only file system` noise from the
  smbd/rpcd workers (cosmetic; all real logs go to stdout per §6.4;
  silencing deferred because every lever risks losing diagnostics). B.5
  gains a back-link to `docs/traceability.md`.
- 2026-09-16: Phase 4 — **the catalog stops being a single branch.** B.7
  lists the three branches activated at their current upstream patch
  levels (4.24.7, 4.23.12, 4.22.11), each pinned in `versions.yaml` from a
  checksum produced by the new `scripts/verify-upstream-tarball.sh`, and
  states the rule the README matrix follows: the lifecycle column is
  positional over the catalog's branch list, so upstream's status is
  relayed and never decided here. B.6's cross-branch upgrade entry stops
  being a promise and becomes a bounded claim with evidence: intra-branch
  on every release, previous-branch → current on every release of the
  current branch, everything else untested and unclaimed — proven for
  4.23.12 → 4.24.7 locally on arm64 (PASS, 27.2 s, data intact, marker
  moved forward). Both older branches built and ran the suite unchanged:
  no `./configure` option this image passes is rejected by 4.23 or 4.22,
  so the Dockerfile stayed single. One product difference was found doing
  it and is recorded in B.6 rather than worked around: 4.22.11 and 4.23.12
  write the self-signed TLS certificate's serial in host byte order, which
  makes it a negative DER INTEGER for about half of every 256-second
  window; 4.24.7 writes it big-endian. A second, on 4.22 alone:
  `samba-tool domain backup restore` needs `CAP_DAC_OVERRIDE` there, which
  the B.2 set — measured on the 4.24 image — does not grant. Both are
  ruled on in B.6, and both rulings are deliberately narrow. The
  capability set a running DC is tested and documented under does not move
  on any branch: what gains `DAC_OVERRIDE` is the one-off container that
  performs a 4.22 restore — expressed in the suite as the new
  `E2E_RESTORE_CAPS`, and in the operator procedure as one extra
  `--cap-add` on that one command. `TestLDAPSCertificate` tolerates a
  negative serial and nothing else, keeping its full in-process inspection
  wherever the certificate parses and falling back to its over-the-wire
  half where it does not, so the property stays asserted on all three
  branches and the test is deterministic on all three.
  B.7 also relays upstream's §9.4 deprecation signal for 4.22: 4.25.0rc2
  is published, so the 4.22 row — here and in the rendered README matrix
  (`update-readme-matrix --rc-series 4.25`) — carries "deprecation
  pending". It is a notice, not a removal; 4.22 keeps receiving every
  patch release until upstream actually ends it.
- 2026-09-16: Phase 4 — **base-package updates become a build input, and
  the §5.4 fixable-CVE gate goes green.** Measured on the 4.24.7 image of
  that morning, the gate
  (`trivy image --scanners vuln --ignore-unfixed --severity HIGH,CRITICAL`)
  reported 41 fixable HIGH/CRITICAL findings, three of them CRITICAL, from
  three independent causes; all three are fixed at the source rather than
  excepted, and nothing was suppressed. (1) Twelve findings came from
  packages the image inherits from `debian:trixie-slim` and never
  upgraded — `apt-get install` of the manifest does not touch a package
  that already satisfies the request, so `gzip`, `perl-base` (all three
  CRITICALs), `libsqlite3-0` and `libpcre2-8-0` stayed at the base's
  versions while their fixes sat in the archive the build already queries.
  The runtime stage now runs `apt-get upgrade` before the manifest
  install, in the same layer; B.8 states why that does not weaken the
  reproducibility claim and why it is `upgrade` and never `dist-upgrade`.
  (2) Nineteen came from the Go stdlib compiled into the entrypoint
  binary: the toolchain pin, not the `go.mod` floor, decides which stdlib
  ships, so `base.gobuild` moves from `golang:1.24-trixie` (stdlib
  1.24.13) to a `golang:1.25-trixie` digest resolving to go1.25.14. (3)
  Ten came from `golang.org/x/crypto`, indirect via `go-ldap`, bumped
  v0.48.0 → v0.55.0 — the newest release the 1.25 toolchain accepts, and
  well above the v0.52.0 the advisories require. `scripts/pkg-closure-hash.sh`
  is added as THE definition of `pkg_index_hash`, shared by the Phase 2
  seed, the Phase 6 watcher probe and any local check: the union of an
  `upgrade` dry-run and an `install` dry-run inside the pinned base, as
  `<name> <version>` pairs, sha256, first 16 hex. Both halves are
  load-bearing — an `install`-only hash is blind to exactly the
  base-package security fix that §9bis.1.c makes a rebuild trigger. The
  gate now reports 0/0 on both the Debian and the gobinary target, and
  `check-image-packages.sh` still passes unchanged, because `upgrade`
  moves versions and never the package set.
- 2026-09-16: Phase 4 — **the release cycle becomes a workflow in this
  repository.** New **Release cycle** section: what is watched (the two
  upstream directory listings, the three base-image digests, the package
  closure, and our own published tags), the decision order, the soak and
  the two ways past it, the necessity criterion, what the watcher never
  decides, and where its state lives. Roadmap decision D4 (watcher in a
  separate repository) is reversed and the reversal is dated in the
  roadmap: the state the watcher remembers is now a tracked file, and every
  decision is a commit carrying its cause. Three things the profile now
  says plainly that it did not before. (1) B.1's claim that the watcher
  monitors the Samba announcement channel is corrected to what is actually
  implemented — an hourly probe of the release directory — and the
  announcement list is recorded in B.6 as not machine-parsed in v1, with
  the bound on that gap measured rather than asserted: detection is
  unaffected, only the automatic soak bypass is. (2) The package-closure
  hash is measured on the runner's architecture (amd64) alone; the section
  states what that does and does not mean — the hash decides *when* a
  rebuild is owed, never *what* is installed, and both architectures are
  always built natively against the index of the day. (3) The §5.4 ledger
  stops being hand-written: `scripts/cve-ledger.py` synchronises
  `security/cve-exceptions.yaml` from the published image's SBOM as one
  entry per affected package, because the 4.24.7 image carries 287
  findings with no available fix (77 HIGH/CRITICAL) over 27 packages and a
  per-CVE ledger of that size is the suppression list §5.4 forbids. The
  review dates are never moved by a sync — renewing one is a human review,
  and `check-cve-exceptions.py` is still the clock.
- 2026-09-16: Phase 4 — **the operational half of the release cycle is
  written down.** New `docs/operations.md` is the counterpart to the
  Release cycle section above: that section says what the automation
  decides, and the new file says how it is started, configured, watched
  and driven by hand — the external hourly trigger (fine-grained PAT,
  `/usr/local/bin/samba-ad-dc-upstream-check`, the cron line) and why
  there is no `schedule:` trigger to replace it, the repository secrets
  and variables with what happens when each is absent, the four
  supervision layers, every manual operation with its exact command
  (`security_release`, `dry_run`, re-dispatching a release, the staging
  rehearsal, adding a series, retiring one, re-measuring the capability
  set), the two §9.6 degraded modes with the disclosure each produces and
  what recovery from Mode 2 is, and a triage table. The Release cycle
  section's forward reference to `docs/operations.md` is no longer
  dangling. The first-publication path gets a procedure rather than a
  promise: the three catalog branches are all in the `publish` state, so
  going live is adding the mirror secrets (optional), starting the timer,
  and watching three releases — plus the one thing no workflow can do,
  deleting the README's pre-release notice, which is a plain blockquote
  and therefore a hand edit. SECURITY.md's §5.4 paragraph stops being a
  path and a date and states the ledger's actual shape: one entry per
  affected package, synchronised from the published SBOM by the watcher
  through `scripts/cve-ledger.py sync`, with `check-cve-exceptions.py` as
  the clock a sync never moves. README gains a short Release automation
  section pointing at both documents; its full rewrite is Phase 5. The
  guide also documents `post-push-verify.yml` from the workflow itself
  rather than from the plan — including the two properties of its
  `workflow_run` trigger a maintainer has to know before the first
  release: it fires only for `release.yml` as it exists on the default
  branch, and only for a Release run that concluded `success`.
- 2026-09-16: Phase 5 — **`no-new-privileges` stops being a documentation
  claim and becomes a tested one.** Every deployment example the guides
  publish carries `security_opt: ["no-new-privileges:true"]`, B.2 above
  describes the constrained profile, and `docs/traceability.md` stated
  that `harness.StartDC` applied it — but the harness emitted no
  `--security-opt` at all. The setting is now `DefaultSecurityOpt` in
  `test/e2e/harness`, applied to every **DC** container the suite starts —
  every container running the image under test goes through `StartDC` /
  `RunDCExpectExit`, the one-offs included. The separate test client is a
  different image and deliberately outside the profile: it is a fixture,
  not a thing the guides tell an operator to run. The whole suite was
  re-run under it locally on arm64 against `samba-ad-dc:dev`: 17 pass,
  1 skip (`TestUpgradeFromLastPublished`, with `E2E_UPGRADE_FROM` unset),
  0 fail. It is defence-in-depth rather than a measured minimum —
  the suite is green either way — and that is exactly why it belongs in
  the harness: a change that ever needed a setuid helper must fail in CI
  rather than on a deployment that followed §1.8 of the deployment guide.
  The alternative, deleting the setting from the guides, was rejected:
  nothing measured it as harmful, and "tested = documented" is satisfied
  by testing it.
- 2026-09-16: Phase 5 — the traceability map's **doc section** column is
  filled: 17 anchors into the three guides, each verified by computing
  the GitHub slug of the heading it names. `scripts/check-traceability.sh`
  gains check (f), which holds the guides' citations to exactly the
  matrix's ID set in both directions (every mapped ID cited by at least
  one guide; every backticked `Test…` token in the guides a mapped ID or
  an exempt infrastructure test), matching whole backticked IDs so that
  `TestProvision` cannot stand in for `TestProvisionOverStateRefused`.
- 2026-09-16: Phase 5 — one correction to the Phase 4 entry above: the
  README's pre-release notice is *not* a bare blockquote any more. It
  carries a `<!-- GO-LIVE: … -->` marker naming the going-live step that
  deletes it, so the step in `docs/operations.md` is still a hand edit,
  but a greppable one — and it now says to delete the marker *and* the
  blockquote, because deleting only the marker is the failure it exists
  to prevent.
- 2026-09-16: Phase 5 — **the B.2 capability set is confirmed by CI on
  both architectures**, which closes the "CI confirmation … is pending"
  qualifier the Phase 3 entry above left open. The first
  `workflow_dispatch` of `.github/workflows/capbisect.yml`
  (<https://github.com/ESITC-Paris/samba-ad-dc/actions/runs/35153558018>)
  bisected the shipped set on a native runner per architecture and both
  legs ended `=> AGREE`, capability for capability: `SYS_ADMIN`, `CHOWN`,
  `FOWNER`, `SETUID`, `SETGID` REQUIRED and `NET_BIND_SERVICE` DROPPABLE
  on amd64 and on arm64 alike. Nothing in the set changed as a result —
  the value of the run is that the claim is now measured on the kernel
  the image ships against rather than on a maintainer's laptop, and that
  a capability question the two architectures were entitled to answer
  differently they did not. Reports committed as
  `test/capbisect/results-amd64.txt` (new) and
  `test/capbisect/results-arm64.txt` (refreshed from the CI leg).
  **Refreshing the arm64 report moved evidence, so four citations were
  re-pointed in the same commit.** The CI bisection starts from
  `harness.DefaultCaps`, which no longer contains `DAC_OVERRIDE`, so its
  reports do not measure it; the local run that did is
  `test/capbisect/results-arm64-2026-08-16.txt`, already committed and
  byte-identical to the pre-refresh `results-arm64.txt`. B.2's provenance
  sentence, B.2's `CAPBISECT_SET` recipe ("exactly how the committed
  report was produced"), §1.2 of the deployment guide and the
  `harness.DefaultCaps` doc comment all named `results-arm64.txt` for a
  local 2026-08-16 run; each now names the dated report for that, and the
  two CI reports for the confirmation. Had they been left alone, the
  refresh would have silently re-dated a citation and orphaned the only
  `DAC_OVERRIDE` measurement in the tree.
- 2026-09-17: Phase 5 — **closure.** The three operator guides are
  complete and cross-checked against the tests
  (`docs/deployment-guide.md` §10.2, `docs/update-guide.md` §10.3,
  `docs/operations.md`), traceability holds in both directions including
  the guides, `no-new-privileges` is in the harness, and the B.2
  capability set is CI-measured on both architectures — each recorded in
  its own entry above. What this entry adds is the rehearsal and the four
  things it and the final review found.

  **The staging drill.** The whole publication path was rehearsed on
  2026-09-17 against `v4.24.7-r1` with `staging=true`:
  [release run 35158262620](https://github.com/ESITC-Paris/samba-ad-dc/actions/runs/35158262620)
  (18 min; both native legs ~17 min 40 s) and the
  [post-push verification](https://github.com/ESITC-Paris/samba-ad-dc/actions/runs/35159720483)
  it triggered, both green, over the staging package alone — no public
  artefact, no tag, no release. Every §8 gate ran on both architectures
  except the upgrade gates, which skipped for the documented
  first-publication reason (no `samba-ad-dc` package on GHCR to upgrade
  from, SPEC §8.3). The upgrade path is therefore the one part of the
  pipeline still unexercised, and the first real publication is what
  exercises it. Recorded with its numbers in `docs/operations.md`.

  **Two follow-ups the drill could not close**, both blocked on a laptop
  `gh` token's scopes rather than on anything in the repository, and both
  now written into the go-live procedure as maintainer steps: the
  `samba-ad-dc-staging` package still exists and must be deleted
  (`delete:packages`), and — the finding that matters more — GHCR created
  that package **private by default**. Nobody chose that. Until the
  maintainer flips `samba-ad-dc` to public after the first release,
  anonymous `docker pull` and `cosign verify` fail and every verification
  instruction this repository publishes is wrong. Post-push verification
  cannot catch it: it runs with `GITHUB_TOKEN` and reads a private
  package happily.

  **One watcher rule tightened.** `decide()` exempted any branch with no
  state entry from the digest comparison, recording what it saw "without
  calling it a change". That is right only while the branch still owes
  its first publication. `apply()` writes a decision's observation into
  `versions.yaml` as well as into `.build-state.json`, so on a branch
  whose tag is already published the exemption re-pinned `base.*` and
  `pkg_index_hash` under `action: none` — the catalog would claim a base
  the published image was never built from, and that cycle's rebuild
  would be lost, because the next run compares against the values the
  previous one just wrote. The exemption now ends at the first
  publication; the Release cycle section above states the rule and why.

  **Two smaller corrections.** `test/e2e/go.mod` declared `go 1.24.0`
  under a `go.work` declaring `1.25.0`, which is what the open
  `actions/setup-go` v7 pull request fails on; and the comment on
  `TestOfflineBackupRestore`'s FINDING 4 described a restore layout the
  B.6 measurement contradicts — `cache directory` lands at
  `/var/lib/samba/cache`, a sibling of `state` and not a child of it, and
  only `state directory` and the sysvol share move down. The cosign
  guidance is also now truthful about what was measured: the pipeline
  signs and re-verifies with v2.6.5, a v3 CLI is *expected* to verify
  those signatures and this project has not exercised it — the deployment
  guide previously asserted that v3 would fail, which nothing had tested.

- 2026-09-17: Phase 6 — **`SAMBA_GLOBAL_OPTIONS`**, a declarative
  `[global]` block. The Runtime contract gains the variable and the
  *Declarative configuration* section above: what it accepts, the keys it
  refuses (each naming the variable or the mechanism that owns them), the
  three points it is applied at, and the `testparm` gate that restores the
  previous `smb.conf` before refusing with exit 10. Two facts about
  `testparm` are recorded there because they decide the shape of the gate
  and neither is documented by samba: an **unknown parameter exits 0**, so
  the exit code alone cannot be the verdict, and both diagnostics are DEBUG
  output on stderr, so the check passes `--debug-stdout` to read them —
  measured against Samba 4.24.7 in this image. B.5 gains the clause and
  `docs/traceability.md` row **N9** (`TestGlobalOptionsApplied`), the first
  row added since Phase 3 froze the set at 17.
