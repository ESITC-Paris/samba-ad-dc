# samba-ad-dc

Production-grade container image for a **Samba Active Directory Domain
Controller**, built from verified upstream source with Samba's bundled
Heimdal Kerberos. Every currently supported upstream stable branch is
published simultaneously, each image is signed and carries an SBOM and a
provenance attestation, and publication is driven by an hourly watcher
rather than by hand — so a Samba patch release, a moved base image or a
changed package closure becomes a new, verifiable tag without a
maintainer in the loop.

[![CI](https://github.com/ESITC-Paris/samba-ad-dc/actions/workflows/ci.yml/badge.svg)](https://github.com/ESITC-Paris/samba-ad-dc/actions/workflows/ci.yml)
[![Release](https://github.com/ESITC-Paris/samba-ad-dc/actions/workflows/release.yml/badge.svg)](https://github.com/ESITC-Paris/samba-ad-dc/actions/workflows/release.yml)
[![Upstream check](https://github.com/ESITC-Paris/samba-ad-dc/actions/workflows/upstream-check.yml/badge.svg)](https://github.com/ESITC-Paris/samba-ad-dc/actions/workflows/upstream-check.yml)

Images: `ghcr.io/esitc-paris/samba-ad-dc`
([GHCR package](https://github.com/ESITC-Paris/samba-ad-dc/pkgs/container/samba-ad-dc)).

## Non-affiliation notice

This is an independent community build. It is **not affiliated with,
endorsed by, or supported by the Samba Team or the Samba project**. The
Samba name is used solely to describe the packaged software. Samba
itself is © the Samba Team, licensed GPL-3.0-or-later; this build
repository is licensed Apache-2.0 (see `LICENSE`).

## Status

<!-- GO-LIVE: delete this comment and the blockquote below once the first release is published (docs/operations.md, "Going live: the first publication", step 7). -->
> **Status: pre-release.** No image has been published yet. The contract
> documented below is the contract the code is tested against, but no tag
> exists on the registry until the watcher's first publication, so every
> command naming a published tag will fail until then.

## Quickstart

One domain controller, provisioning a new domain on first boot. The
example is complete: it is what a first DC actually needs, not a
skeleton. Replace every value marked `CHANGE ME`.

`docker-compose.yml`:

```yaml
services:
  dc1:
    # The container name is the DC's NetBIOS computer name: 15 characters
    # maximum, and it cannot be changed after the domain is provisioned.
    container_name: dc1
    hostname: dc1
    # A branch tag tracks every patch release of that branch. For
    # production, pin an immutable `X.Y.Z-rN` tag or a digest instead —
    # see "Tags, pinning and support policy" below. Never `latest`.
    image: ghcr.io/esitc-paris/samba-ad-dc:4.24
    environment:
      # First boot only. After the domain exists, change this to `run`:
      # `run` refuses to start on absent state (exit 21), which is what
      # protects you from silently re-provisioning an empty domain onto a
      # volume that failed to mount. Leaving `provision` here makes every
      # later start fail with exit 20 instead.
      SAMBA_MODE: provision
      SAMBA_REALM: AD.EXAMPLE.COM          # CHANGE ME
      SAMBA_DOMAIN: AD                     # CHANGE ME (NetBIOS name)
      # Not optional: samba's internal DNS with no upstream takes seconds
      # to fail a name it does not serve, which is long enough to time out
      # Kerberos. Point it at a resolver that is NOT this DC.
      SAMBA_DNS_FORWARDER: 192.0.2.53      # CHANGE ME
      # Secrets are accepted only as files. A plain SAMBA_ADMIN_PASSWORD
      # in the environment is refused with exit 10.
      SAMBA_ADMIN_PASSWORD_FILE: /run/secrets/admin_password
    secrets:
      - admin_password
    # No NAT: the DC registers its own IP in its own DNS, and Kerberos and
    # dynamic RPC reference it. Publishing ports on a bridge is
    # unsupported; macvlan (below) or host networking are the two
    # supported topologies.
    networks:
      dc_net:
        ipv4_address: 192.0.2.10           # CHANGE ME (this DC's own IP)
    # The AD DC cannot run as a non-root user (it writes `security.*`
    # extended attributes and binds privileged ports), so it runs as root
    # with a measured, minimal capability set. `--privileged` is never
    # required and must not be used.
    cap_drop: [ALL]
    cap_add: [SYS_ADMIN, NET_BIND_SERVICE, CHOWN, FOWNER, SETUID, SETGID]
    read_only: true
    security_opt: ["no-new-privileges:true"]
    tmpfs:
      - /run
      - /tmp
      - /var/cache/samba
    volumes:
      - samba_data:/var/lib/samba
      - samba_conf:/etc/samba
    restart: unless-stopped
    # No `healthcheck:` here on purpose — the image ships its own
    # application-level HEALTHCHECK (DNS SRV, LDAP rootDSE, SMB share
    # enumeration on loopback) and compose inherits it. Its 180 s start
    # period is deliberate: a first-boot provision on a cold volume
    # legitimately takes minutes.

secrets:
  admin_password:
    # Create this file before `up`, mode 0600. It holds the initial
    # domain Administrator password and nothing else.
    file: ./secrets/admin_password

networks:
  dc_net:
    driver: macvlan
    driver_opts:
      parent: eth0                         # CHANGE ME (host NIC)
    ipam:
      config:
        - subnet: 192.0.2.0/24             # CHANGE ME
          gateway: 192.0.2.1               # CHANGE ME
          ip_range: 192.0.2.8/29           # CHANGE ME (addresses for DCs)

volumes:
  samba_data:   # /var/lib/samba — directory database, Kerberos secrets, sysvol, TLS
  samba_conf:   # /etc/samba — generated configuration
```

Write the secret, then bring it up:

```sh
mkdir -p secrets && umask 077 && printf '%s' 'CHANGE-ME-strong-passphrase' > secrets/admin_password
```

1. Start the DC and provision the domain:

   ```sh
   docker compose up -d
   ```

2. Wait for the container to report healthy (a first-boot provision takes
   minutes; the probe is the image's own):

   ```sh
   until [ "$(docker inspect -f '{{.State.Health.Status}}' dc1)" = healthy ]; do sleep 10; done
   ```

3. From a domain member that resolves through this DC, get a ticket:

   ```sh
   kinit Administrator@AD.EXAMPLE.COM && klist
   ```

Then edit `SAMBA_MODE` to `run` and `docker compose up -d` again. The
full walkthrough — verifying the image before the first run, reading the
first-boot log, checking every protocol from a member, adding a second
DC — is in [`docs/deployment-guide.md`](docs/deployment-guide.md).

Covered by: `TestProvision`, `TestKerberosKinit`.

## Configuration reference

The binding contract is the **Runtime contract** section of
[`docs/adaptation-profile.md`](docs/adaptation-profile.md), which is
authoritative; the tables below are a copy of it. Exit codes are
**immutable once released**.

### Environment variables

| Variable | Modes | Default | Meaning |
|---|---|---|---|
| `SAMBA_MODE` | all | `auto` | `auto\|provision\|join\|run\|maintenance` |
| `SAMBA_REALM` | provision, join (and auto reaching them) | — required | Kerberos realm / AD DNS domain, e.g. `AD.EXAMPLE.COM` |
| `SAMBA_DOMAIN` | provision | first label of realm | NetBIOS domain name |
| `SAMBA_ADMIN_PASSWORD_FILE` | provision | — required | file with the initial Administrator password |
| `SAMBA_JOIN_USERNAME` | join | `Administrator` | account used to join |
| `SAMBA_JOIN_PASSWORD_FILE` | join | — required | file with the join account password |
| `SAMBA_DNS_FORWARDER` | provision, join | none | upstream DNS forwarder IP (see below — not optional for a DC that resolves through itself) |
| `SAMBA_DNS_BACKEND` | provision, join | `SAMBA_INTERNAL` | only `SAMBA_INTERNAL` supported in v1 |
| `SAMBA_FUNCTION_LEVEL` | provision, join | `2016` | AD functional level; also mirrored onto the `ad dc functional level` smb.conf parameter for `2012`, `2012_R2` and `2016` — by provision through `--option`, by join through the post-join edit |
| `SAMBA_LOG_LEVEL` | all | `1` | samba debug level |
| `SAMBA_CHRONY` | auto/provision/join/run | `on` | serve MS-SNTP signed time (`on\|off`) |
| `SAMBA_MAINTENANCE_OP` | maintenance | `check` | `check` (dbcheck) or `repair` (dbcheck --fix --yes) |

Secrets are accepted **only** through the `*_FILE` variables. Setting a
plain `SAMBA_ADMIN_PASSWORD` or `SAMBA_JOIN_PASSWORD` in the environment
is refused with exit 10 and a message naming the `_FILE` variant; no
secret value is ever logged.

**`SAMBA_DNS_FORWARDER` is not a nicety on a multi-DC domain.** Samba's
internal DNS with no upstream takes *seconds* to fail a query it is not
authoritative for (measured at 4-8 s against this image) instead of
answering immediately. A DC that resolves through itself — which a
multi-DC domain requires, because a replication partner is addressed by a
`_msdcs` CNAME only the directory's own DNS can answer — then pays that
stall on every Kerberos bind, and the sealed DRSUAPI bind that carries
replication times out before it completes. Set it to a resolver that is
**not** this DC, or the domain replicates erratically or not at all.

`KRB5_CONFIG` is set in the image to `/var/lib/samba/private/krb5.conf`,
the Kerberos configuration provision and join generate for the realm, so
that an operator's `docker exec ... samba-tool` inherits the realm's
Kerberos configuration rather than rediscovering it over DNS.

### Modes

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
  failure).

State is present exactly when `/var/lib/samba/private/sam.ldb` exists. A
marker at `/var/lib/samba/.image-state.json` records the Samba version
that wrote the state: a volume written by a **newer** Samba is refused
with exit 22, an older one triggers `samba-tool dbcheck` before the
daemons start and the marker is moved forward only if it passes. A
marker whose version cannot be parsed is a configuration error, refused
with exit 10 before any check runs — unlike an *absent* marker, which is
adopted after a dbcheck, because a volume that never carried one is a
pre-existing volume rather than a damaged one. A restart never modifies
existing state.

### Argv

| Argument | Meaning |
|---|---|
| *(none)* | the container's `ENTRYPOINT`: load, observe, decide, initialize if needed, supervise |
| `healthcheck` | the image's `HEALTHCHECK` probe |
| `--version` | print the Samba version embedded at build time (`-ldflags -X main.sambaVersion=<v>`) and exit `0` |

Anything else is refused with exit 10.

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
`;`**, written to stderr and prefixed `ERROR: `. Logs go to
stdout/stderr exclusively; samba runs `--foreground --no-process-group
--debug-stdout`.

Covered by: `TestPlainEnvSecretRejected`, `TestMissingSecretFailsFast`,
`TestRunModeWithoutStateRefused`, `TestProvisionOverStateRefused`.

## Non-negotiable deployment constraints

Copied from B.3 of [`docs/adaptation-profile.md`](docs/adaptation-profile.md)
(authoritative).

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
  `SAMBA_DNS_FORWARDER` to a resolver that is not this DC.
- **Filesystem:** the volume backing `/var/lib/samba` requires xattr and
  POSIX ACL support (ext4/xfs); NFS unsupported.
- **Time:** the container serves signed NTP (MS-SNTP via chrony, wired to
  Samba's signing socket) to domain members but does not discipline the
  clock by default — host time synchronization is the operator's
  responsibility (option exists to grant CAP_SYS_TIME instead).
- **Secrets:** file-based only (`*_FILE`); the domain administrator
  password is never accepted via plain environment variable.

**Root and capabilities.** The DC cannot run as a non-root user: it
writes extended attributes in the `security.*` namespace, which requires
`CAP_SYS_ADMIN`, and it binds privileged ports (53, 88, 389, 445, 464,
636). The mitigation is a minimal capability set — `SYS_ADMIN`,
`NET_BIND_SERVICE`, `CHOWN`, `FOWNER`, `SETUID`, `SETGID` on top of
`cap_drop: ALL` — **established by capability bisection**, not assumed
(driver `test/capbisect/bisect.sh`, reports
`test/capbisect/results-amd64.txt` and `test/capbisect/results-arm64.txt`
from the CI bisection that confirmed the set on both architectures, what
each removal breaks tabulated in B.2). **Do not run this image with
`--privileged`:** it is never required, and it discards the whole point
of the measured set. The read-only root filesystem is CI-proven; the
writable paths are the two volumes plus tmpfs at `/run`, `/tmp` and
`/var/cache/samba`.

## Volumes and backup

Two volumes, both required, both on a filesystem with xattr and POSIX ACL
support (ext4/xfs — **NFS is unsupported**):

| Mount point | Holds |
|---|---|
| `/var/lib/samba` | the directory database (`sam.ldb`), Kerberos secrets, sysvol, the DC's TLS material, the NTP signing socket, the chrony drift file and the image state marker `.image-state.json` |
| `/etc/samba` | the `smb.conf` generated by provision or join |

Everything else in the container is disposable: the root filesystem is
read-only and `/run`, `/tmp` and `/var/cache/samba` are tmpfs, so nothing
of value can accumulate outside those two volumes.

**Backup is offline backup.** `samba-tool domain backup offline` is the
primary, CI-tested path: it needs no administrator credentials, which is
what makes it automatable. Online backup is a documented alternative that
does need them. Restoring is not a file copy — `samba-tool domain backup
restore` writes a *self-contained tree* whose `smb.conf` points into it,
so a restored DC keeps most of its state one level down
(`/var/lib/samba/state`, `/var/lib/samba/cache`, sysvol under
`state/sysvol`). Any procedure that names a path must therefore derive it
from `smb.conf` — `testparm -s --parameter-name="path"
--section-name=sysvol` — rather than hardcode a provisioned DC's layout.
On branch 4.22 only, the one-off container that runs the *restore* needs
`--cap-add DAC_OVERRIDE`; the DC that afterwards serves the restored
domain does not, and 4.23 and 4.24 need nothing added at all.

The step-by-step runbook, including the four findings above in the order
you meet them, is in the deployment guide:
[`docs/deployment-guide.md#backup-and-restore-runbook`](docs/deployment-guide.md#backup-and-restore-runbook).

Covered by: `TestOfflineBackupRestore`.

## Tags, pinning and support policy

| Tag | Points at | Production |
|---|---|---|
| `X.Y.Z-rN` | one immutable build of Samba `X.Y.Z`; **never re-pushed** — a correction increments `N` | yes — this or a digest |
| `X.Y.Z` | the latest `-rN` of that patch release | moves |
| `X.Y` | the latest patch release of that branch | moves — convenient, acceptable |
| `X` | the latest release of the **default** (newest) branch | moves across patch *and* minor versions |
| `latest` | the same as `X` — the default branch's latest release | **no** |
| `edge` | not published by this repository | — |

`latest` is **not production-usable** and is documented that way
deliberately: it crosses minor versions without asking, so a
`docker compose pull` can move a domain controller between Samba branches
— a path this project tests exactly one step of and never backwards.
`edge` (a build from `main`) is permitted by the specification but is not
built here; every published tag comes from a released upstream tarball.

**Pin production by digest**, or by an immutable `X.Y.Z-rN` tag. Every
release publishes its index digest in the release notes. A digest is the
only pin that cannot move under you, and it is what makes the signature
and attestation checks below mean something about the bytes you are
actually running.

Every upstream-supported stable branch is published simultaneously and
every patch release of every branch is published — no version skipping.
Branch lifecycle is relayed, never decided here: a release candidate for a
newer series turns into a deprecation notice on the oldest supported
branch, and a branch upstream has ended leaves the catalog. Which upgrade
paths are supported (intra-branch, and previous branch → current branch —
and nothing else), how to roll one out across multiple DCs, and why a
rollback is a restore rather than a tag downgrade, are in
[`docs/update-guide.md`](docs/update-guide.md).

## Verifying images

The signature is keyless (Sigstore/Fulcio), so the identity is a URL, not
a key. The owner is spelled `ESITC-Paris`: cosign matches the certificate
identity **case-sensitively**, and the display casing is what lands in
the certificate.

```sh
cosign verify ghcr.io/esitc-paris/samba-ad-dc:<tag> \
  --certificate-identity-regexp 'https://github.com/ESITC-Paris/samba-ad-dc/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

```sh
gh attestation verify oci://ghcr.io/esitc-paris/samba-ad-dc@<digest> \
  --repo ESITC-Paris/samba-ad-dc
```

`--repo` rather than `--owner`: an attestation from any other repository
of the organisation is not evidence about this image.

The pipeline signs and re-verifies with **cosign v2.6.5**, which is the
version these signatures are known to verify under. `brew install cosign`
now gives v3; a v3 verification is expected to work and has not been
exercised here, so if one fails, try v2.6.5 before concluding the image is
bad. `docs/deployment-guide.md` §2.1 has the detail.

Both attestations are attached to the image in the registry and can be
read back without pulling it:

```sh
docker buildx imagetools inspect ghcr.io/esitc-paris/samba-ad-dc:<tag> --format '{{json .SBOM}}'
docker buildx imagetools inspect ghcr.io/esitc-paris/samba-ad-dc:<tag> --format '{{json .Provenance}}'
```

Every published image is re-verified from outside the release pipeline by
`.github/workflows/post-push-verify.yml`, which runs these same checks
against the registry after the push.

## Compatibility matrix

<!-- matrix:start -->
| Upstream branch | Latest image tag | Aliases | Upstream support status |
|-----------------|------------------|---------|-------------------------|
| 4.24 | 4.24.7-r1 | `4.24.7`, `4.24`, `4`, `latest` | current |
| 4.23 | 4.23.12-r1 | `4.23.12`, `4.23` | maintenance |
| 4.22 | 4.22.11-r1 | `4.22.11`, `4.22` | security fixes only — deprecation pending (4.25 rc published) |
<!-- matrix:end -->

This table is generated from `versions.yaml` by
`python3 scripts/catalog.py update-readme-matrix` — do not edit it by
hand.

## Release automation

Publication is automated end to end: an hourly watcher
(`.github/workflows/upstream-check.yml`) probes upstream releases, the
pinned base images and the package closure, and dispatches
`.github/workflows/release.yml` only when the image is certain to differ
from the last published one. What it decides is documented in
[`docs/adaptation-profile.md`](docs/adaptation-profile.md) ("Release
cycle"); how it is scheduled, supervised and driven by hand is in
[`docs/operations.md`](docs/operations.md).

## Documentation

- [`docs/deployment-guide.md`](docs/deployment-guide.md) — constraints,
  first DC, protocol checks from a member, adding a second DC, day-2
  operations, backup and restore.
- [`docs/update-guide.md`](docs/update-guide.md) — pinning strategies,
  patch and branch upgrades, multi-DC rollout order, rollback.
- [`docs/operations.md`](docs/operations.md) — running the release
  automation: scheduling, supervision, manual operations, triage.
- [`docs/adaptation-profile.md`](docs/adaptation-profile.md) —
  **authoritative**: the runtime contract, the deployment constraints, the
  known limitations and the release cycle.
- [`docs/traceability.md`](docs/traceability.md) — which E2E test covers
  which documented use case, in both directions.
- [`CHANGELOG.md`](CHANGELOG.md) — image tag ↔ upstream version ↔ git
  commit.
- [`SPEC.md`](SPEC.md) — the publishing specification this image conforms
  to.

## Security

See [`SECURITY.md`](SECURITY.md). This project conforms to the vendored
publishing specification ([`SPEC.md`](SPEC.md)); conformance is asserted
per image via the `org.esitc-paris.spec-version` OCI label.

## License

This build repository — the Dockerfile, the entrypoint, the CI and the
documentation — is licensed **Apache-2.0** (see [`LICENSE`](LICENSE)).

The **image content** is a different matter: it packages Samba, which is
© the Samba Team and licensed **GPL-3.0-or-later**, alongside its Debian
runtime dependencies under their own licenses. Each image therefore
carries `org.opencontainers.image.licenses=GPL-3.0-or-later`, describing
what is inside it rather than what built it, and the attached SBOM lists
every component and its license.
