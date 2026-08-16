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

- **§6.7 shell-entrypoint exception: not required** — the entrypoint is Go
  from the first release, so no shell interim ever ships and no §11.2
  exception is filed (see `docs/exceptions/README.md`). §6.7 itself is
  satisfied; evidence: the unit-test gate runs inside the image build
  (Dockerfile `gobuild` stage) and in CI (`unit` job), and the behavior
  contract is the **Runtime contract** section below.

- **chronyd runs as root inside the container** (SIGTERM delivery without
  `CAP_KILL`; ntp_signd socket access). **REVISIT at the Phase 3
  capability bisection:** the alternative is `CAP_KILL` in the capability
  set plus signing-socket group permissions; `TestSignedNTPWiring` is the
  acceptance test either way.

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
| `SAMBA_DNS_FORWARDER` | provision | none | upstream DNS forwarder IP |
| `SAMBA_DNS_BACKEND` | provision, join | `SAMBA_INTERNAL` | only `SAMBA_INTERNAL` supported in v1 |
| `SAMBA_FUNCTION_LEVEL` | provision | `2016` | AD functional level; provision also mirrors it onto the `ad dc functional level` smb.conf parameter for `2012`, `2012_R2` and `2016` (see below) |
| `SAMBA_LOG_LEVEL` | all | `1` | samba debug level |
| `SAMBA_CHRONY` | auto/provision/join/run | `on` | serve MS-SNTP signed time (`on|off`) |
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

`chronyd` is started with `-d -x -f /etc/chrony/chrony.conf`: `-x` so it
never disciplines the host's clock (B.3), `-d` so it logs to the
container's stderr. The configuration is baked into the image (read-only
rootfs) and wires `ntpsigndsocket /var/lib/samba/ntp_signd` for MS-SNTP
signing; 123/udp is exposed. `SAMBA_CHRONY=off` runs the DC without it.

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
