# Deployment guide (SPEC §10.2)

From zero to a working Samba Active Directory Domain Controller: the
constraints that decide whether a deployment can work at all, the
verification you do before the first `docker run`, the first DC, a
protocol check from a member, a second DC, day-2 operation, and the
backup/restore runbook.

**This guide restates the contract; it does not define it.**
[`docs/adaptation-profile.md`](adaptation-profile.md) is authoritative —
its **Runtime contract** section for variables, modes, exit codes, the
health check, the time service and in-container Kerberos, and its **B.2 /
B.3 / B.6** sections for capabilities, deployment constraints and known
limitations. Where the two ever disagree, the profile wins and this file
is the bug.

**Every section names the E2E test that covers it** (§10.6), on a
*Covered by:* line carrying the test function names. Those IDs are frozen
and mapped row by row in [`docs/traceability.md`](traceability.md).
Where a procedure is *not* covered by a test, the section says so in
those words and names the limitation it comes from — there is no third
category. The commands below are the commands the tests run; where this
guide offers a more convenient variant, it says which of the two is the
proven one.

Related: [`README.md`](../README.md) for the quickstart and the full
configuration reference, [`docs/update-guide.md`](update-guide.md) for
pinning, updates and rollback, [`docs/operations.md`](operations.md) for
the release pipeline.

---

## 1. Constraints first

*Covered by:* `TestProvision`

Read this section before writing a compose file. Every item below is a
hard constraint: a deployment that violates one does not degrade, it
fails — usually later, and usually looking like something else.

### 1.1 No NAT: macvlan/ipvlan, or host networking

A domain controller registers **its own IP address** in **its own DNS**,
and Kerberos tickets and dynamic RPC endpoints reference that address.
A translated address is therefore a wrong address for every member that
looks it up.

Supported topologies (B.3):

- **a dedicated IP per DC on a macvlan or ipvlan network** — the primary
  form, used by every example in this guide;
- **host networking** — the DC uses the host's addresses directly.

**Port publishing on a bridge network is unsupported.** `-p 389:389` and
its friends are NAT: the DC would advertise a container-internal address
that no member can reach, while the published ports would answer for a
host name the directory knows nothing about.

`--privileged` is **never** correct for this image. It is not a shortcut
around the capability set below; see §1.2.

### 1.2 Capabilities: six, and no more

The DC cannot run as a non-root user (§5.2 deviation, B.2): it writes
`security.*` extended attributes, which requires `CAP_SYS_ADMIN`, and it
binds privileged ports. The answer is `cap_drop: ALL` plus a minimal set,
never `--privileged`:

```
SYS_ADMIN  NET_BIND_SERVICE  CHOWN  FOWNER  SETUID  SETGID
```

This set is **measured, not assumed**: it was established by capability
bisection (local arm64, 2026-08-16, driver `test/capbisect/bisect.sh`,
report `test/capbisect/results-arm64-2026-08-16.txt`) and confirmed on
both architectures by the CI bisection of 2026-09-16 (reports
`test/capbisect/results-amd64.txt` and `test/capbisect/results-arm64.txt`,
both `=> AGREE`), and the E2E suite re-proves
its sufficiency on every run because `test/e2e/harness.DefaultCaps`
carries exactly this list. What breaks when each one is removed is
tabulated in [B.2](adaptation-profile.md#b2-deviations-from-generic-requirements-112-justifications).

Two entries deserve a note:

- **`NET_BIND_SERVICE` is kept even though the test suite passes without
  it.** Docker sets `net.ipv4.ip_unprivileged_port_start=0` in the network
  namespace it creates, so no port is privileged inside a container on a
  bridge. Under **host networking** the container inherits the host's
  floor of 1024, and a `bind()` of :389 was measured as *denied* without
  the capability and *succeeding* with it. Dropping it would leave the
  suite green and the documented deployment broken.
- **`DAC_OVERRIDE` is deliberately absent** from a running DC on every
  branch. The single exception is the one-off container that runs
  `samba-tool domain backup restore` **on branch 4.22** — see §7.4.

`security_opt: ["no-new-privileges:true"]` is set in every example here,
and **the E2E suite runs its DCs under it too**:
`harness.DefaultSecurityOpt` puts `--security-opt
no-new-privileges:true` on every DC container the suite starts — the
running DCs and the `samba-tool` one-offs alike — so the profile these
examples publish is the profile the tests exercise, flag for flag. (The
suite's throwaway client container is deliberately outside that profile:
it plays a domain member, which is not what this image is.)

It remains defence-in-depth rather than a measured requirement — nothing
in the image escalates privilege at `exec` time, so the suite would be
equally green without it. What the flag buys is that this stays true: a
future change that needed a setuid helper turns the suite red instead of
turning an operator's DC red on the profile this guide told them to use.

### 1.3 Filesystem: xattr and POSIX ACLs

The volume backing `/var/lib/samba` **must** support extended attributes
and POSIX ACLs — ext4 or xfs. **NFS is unsupported** (B.3). The
directory's NT ACLs live in the `security.NTACL` xattr; a filesystem that
silently drops it produces a domain that provisions and then misbehaves.

`/etc/samba` is a second, small persistent volume: it holds the generated
`smb.conf`, which is where the DC's `netbios name` lives. Losing it is
losing the DC's identity, not just its configuration.

Nothing else is writable. The image runs with a read-only root filesystem
and tmpfs at `/run`, `/tmp` and `/var/cache/samba` (B.2). `/var/cache/samba`
is not optional decoration: winbindd opens `netsamlogon_cache.tdb` there
on every boot, and without the tmpfs every start logs three
`tdb_open_log`/`netsamlogon_cache_init` failures.

### 1.4 Time: the host disciplines the clock, the DC serves it

The container **serves** signed NTP (MS-SNTP, chrony wired to samba's
signing socket) to domain members, and **does not discipline the host's
clock**: `chronyd` runs with `-x`. Keeping the host in sync is the
operator's responsibility (B.3). Kerberos tolerates a few minutes of skew
and nothing more, so a DC on an undisciplined host stops authenticating
anyone.

`SAMBA_CHRONY=off` runs the DC without the time service. Losing chronyd
at runtime does not take the DC down — signed NTP stops being served and
the event is logged.

### 1.5 Secrets: files only

Passwords are accepted **only** through the `*_FILE` variables:
`SAMBA_ADMIN_PASSWORD_FILE`, `SAMBA_JOIN_PASSWORD_FILE`. Setting a plain
`SAMBA_ADMIN_PASSWORD` or `SAMBA_JOIN_PASSWORD` in the environment is
refused with **exit 10** and a message naming the `_FILE` variant; no
secret value is ever logged.
(*Covered by:* `TestPlainEnvSecretRejected`,
`TestMissingSecretFailsFast`.)

The file must be **readable by uid 0 inside the container**. Everything in
the container runs as root and `DAC_OVERRIDE` is dropped, so a root-owned
`0400` file is fine, while a file owned by some other uid needs a
world-readable mode — there is no capability left to override the
discretionary check. An unreadable or empty secret file is **exit 11**,
distinct from the configuration error above.

Compose `secrets:` land at `/run/secrets/<name>`, nested inside the `/run`
tmpfs. That nesting is exactly what the E2E harness does (it bind-mounts
each secret read-only at `/run/secrets/<name>` over the same tmpfs), so
the pattern is proven rather than assumed.

### 1.6 DNS: a DC resolves through a DC, and its own DNS needs an upstream

Both halves are required together, and each has a measured reason.

- **A DC must resolve through a DC.** A replication partner is addressed
  by a `<objectGUID>._msdcs.<realm>` CNAME that only the directory's own
  DNS answers. A DC pointed at anything else provisions fine, serves
  clients fine, and can never pull a change *from* its partners.
- **Its own DNS needs a forwarder.** Samba's internal DNS with no upstream
  takes **4–8 seconds** (measured against this image) to fail a name it is
  not authoritative for, instead of milliseconds. The image ships no
  `/etc/krb5.conf`, so Heimdal discovers the realm by walking `_kerberos.`
  up the parent domains — several non-authoritative queries per bind — and
  the Kerberos-sealed DRSUAPI bind that carries replication times out
  before they finish. **Set `SAMBA_DNS_FORWARDER` to a resolver that is
  not this DC.**

The image sets `KRB5_CONFIG=/var/lib/samba/private/krb5.conf` as an image
`ENV` so that an operator's own `docker exec ... samba-tool` inherits the
realm's Kerberos configuration instead of rediscovering it over DNS
(`docker exec` inherits the *image* environment, not PID 1's).

**A caveat about `dns:` on bridge networks.** On a user-defined bridge,
docker always puts its own resolver in the container's `resolv.conf` and
treats `--dns` / compose `dns:` as *that resolver's* upstream. Pointing a
DC at itself that way makes the embedded resolver forward to samba while
samba's `dns forwarder` forwards back to the embedded resolver — a loop
that hangs exactly like having no forwarder at all (observed: 17 s per
name, then the operation aborts). The E2E suite therefore bind-mounts a
`resolv.conf` containing `nameserver 127.0.0.1` over `/etc/resolv.conf`.
That form works on any network type and is the one the tests exercise;
on a macvlan network the plain `dns:` key is the ordinary way to do it.

### 1.7 Container names: at most 15 characters

A DC container's name is its host name, and samba derives the DC's
**NetBIOS name** from it by truncating to 15 bytes. A 16-character name
with a hyphen in position 16 truncates to something ending in `-`, which
is not a legal DNS label: the provision registers a host record the
image's own health probe cannot resolve, and the container sits at
`starting` until it times out with a DNS error that points at samba's DNS
server rather than at the name that caused it.

**Keep any container that may initialize a domain — `provision`, `join`,
or `auto` on an empty volume — to 15 characters or fewer.** A `run`- or
`maintenance`-mode container reads the name back out of `smb.conf` and
never consults its own host name, so a replacement container may be named
more descriptively.

### 1.8 The hardened profile

Every example in this guide carries this block. It is quoted here once and
referred to afterwards as *the hardened profile*.

```yaml
    read_only: true
    tmpfs:
      - /run
      - /tmp
      - /var/cache/samba
    cap_drop: [ALL]
    cap_add: [SYS_ADMIN, NET_BIND_SERVICE, CHOWN, FOWNER, SETUID, SETGID]
    security_opt: ["no-new-privileges:true"]
```

The `docker run` form, used by the one-off containers in §6 and §7:

```sh
  --read-only \
  --tmpfs /run --tmpfs /tmp --tmpfs /var/cache/samba \
  --cap-drop ALL \
  --cap-add SYS_ADMIN --cap-add NET_BIND_SERVICE --cap-add CHOWN \
  --cap-add FOWNER --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges:true
```

### 1.9 Pin the image

Production deployments pin an immutable `X.Y.Z-rN` tag or a digest. The
`X.Y`, `X` and `latest` aliases move; `latest` is **not** production-usable.
The pinning strategies and the reasoning are in
[`docs/update-guide.md`](update-guide.md#by-profile).

---

## 2. Verify before the first run

*No test covers this section.* Signature and provenance verification is a
property of the **published** image, which the E2E suite never touches: it
runs against a locally built image under test. The registry-side
equivalent is `.github/workflows/post-push-verify.yml` (SPEC §8.5), which
re-reads every published image from the registry with no access to
anything the release produced and asserts exactly the two checks below,
plus platform-set equality and non-empty SBOM/provenance attestations. See
[`docs/operations.md`](operations.md#post-push-verification).

Do this **before** the first `docker run`, and again whenever you move to
a new digest.

### 2.1 The cosign signature

```sh
cosign verify ghcr.io/esitc-paris/samba-ad-dc:4.24.7-r1 \
  --certificate-identity-regexp 'https://github.com/ESITC-Paris/samba-ad-dc/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

**The owner's casing in the identity regexp is load-bearing.** The
certificate SAN is the signing job's `job_workflow_ref`, which carries the
GitHub account's *display* casing — `ESITC-Paris` — not the lowercase
spelling the registry path uses. A regexp written with the registry's
casing matches nothing and the verification fails on a correctly signed
image.

**Which cosign.** The pipeline signs with **cosign v2.6.5**
(`sigstore/cosign-installer` pinned to that release in `release.yml`), and
`post-push-verify.yml` verifies every published image with that same
v2.6.5 — so v2.6.5 is the one version these signatures are known to verify
under. `brew install cosign` now installs v3.

Verifying a v2-produced keyless signature with a **v3** CLI is expected to
work: upstream states that v3 is backwards-compatible with v2 layouts (the
incompatibility runs the other way — a v2 CLI cannot verify a signature v3
produced in the new bundle format). That is upstream's claim, not this
project's measurement: no workflow here has been run against a v3 CLI. So
if a v3 verification of a correctly signed image fails, install v2.6.5 and
retry before concluding anything about the image:

```sh
cosign version   # what you have
# v2.6.5 release assets: https://github.com/sigstore/cosign/releases/tag/v2.6.5
```

### 2.2 The GitHub provenance attestation

```sh
cosign verify ... # (above) — then, over the same index digest:
gh attestation verify oci://ghcr.io/esitc-paris/samba-ad-dc@<digest> \
  --repo ESITC-Paris/samba-ad-dc
```

`--repo` rather than `--owner`: an attestation from any other repository
of the organisation is not evidence about this image, and the tighter form
costs nothing. `gh` prints its result only on a terminal — in a script the
exit code is the whole signal.

Resolve `<digest>` from the tag you intend to run, and then **deploy that
digest**, not the tag you resolved it from:

```sh
docker buildx imagetools inspect ghcr.io/esitc-paris/samba-ad-dc:4.24.7-r1 \
  --format '{{println .Manifest.Digest}}'
```

---

## 3. The first domain controller

*Covered by:* `TestProvision`, `TestIdempotentRestart`

### 3.1 The compose file

Replace every value marked `REPLACE`. The DC is `dc1` — 3 characters, well
inside the §1.7 limit.

```yaml
networks:
  addc:
    driver: macvlan
    driver_opts:
      parent: eth0                 # REPLACE: host NIC carrying the DC's VLAN
    ipam:
      config:
        - subnet: 192.0.2.0/24     # REPLACE
          gateway: 192.0.2.1       # REPLACE

secrets:
  admin_password:
    file: ./secrets/admin_password # REPLACE: root-owned, 0400, never in git

volumes:
  dc1-state:
  dc1-conf:

services:
  dc1:
    # Pin an immutable tag or a digest in production (§1.9).
    image: ghcr.io/esitc-paris/samba-ad-dc:4.24.7-r1
    container_name: dc1
    hostname: dc1
    networks:
      addc:
        ipv4_address: 192.0.2.10   # REPLACE
    # A DC resolves through a DC: the first one resolves through itself
    # (§1.6). On a bridge network this key is NOT the container's resolver
    # — bind-mount a resolv.conf instead.
    dns:
      - 192.0.2.10                 # REPLACE: this DC's own address
    environment:
      # First boot only. Switch to `run` once the domain exists (§3.4).
      SAMBA_MODE: provision
      SAMBA_REALM: AD.EXAMPLE.COM  # REPLACE
      SAMBA_DOMAIN: AD            # REPLACE (NetBIOS name; defaults to the realm's first label)
      # Not optional (§1.6): a resolver that is NOT this DC.
      SAMBA_DNS_FORWARDER: 192.0.2.53  # REPLACE
      SAMBA_ADMIN_PASSWORD_FILE: /run/secrets/admin_password
    secrets:
      - admin_password
    volumes:
      - dc1-state:/var/lib/samba
      - dc1-conf:/etc/samba
    # --- the hardened profile (§1.8) ---
    read_only: true
    tmpfs:
      - /run
      - /tmp
      - /var/cache/samba
    cap_drop: [ALL]
    cap_add: [SYS_ADMIN, NET_BIND_SERVICE, CHOWN, FOWNER, SETUID, SETGID]
    security_opt: ["no-new-privileges:true"]
    # The image's own HEALTHCHECK is inherited; do not override it (§6.1).
    stop_grace_period: 15s
    restart: unless-stopped
```

`SAMBA_FUNCTION_LEVEL` defaults to `2016` and is mirrored onto the
generated `smb.conf` — the domain comes up at the level that was asked
for, and every later start keeps it. The full variable table is in
[`README.md`](../README.md#configuration-reference) and, authoritatively,
in the profile's Runtime contract.

### 3.2 Bring it up

```sh
docker compose up -d dc1
docker compose logs -f dc1
```

### 3.3 What the first boot looks like

The provision lines the entrypoint prints, in order (every entrypoint line
is prefixed `entrypoint: `):

```
entrypoint: provisioning a new domain AD in realm AD.EXAMPLE.COM (functional level 2016)
entrypoint: domain AD provisioned
entrypoint: chrony will use the signing socket directory this DC's smb.conf declares: ntpsigndsocket /var/lib/samba/ntp_signd
```

then samba's own output. A first-boot provision on a cold volume
legitimately takes **minutes**, which is why the health check's start
period is 180 s — a container declared unhealthy mid-provision would be
restarted into a half-initialized state.

**Expect noise you should not act on.** Each boot prints a
`reopen_one_log ... Read-only file system` complaint per rpc worker
(measured on a provision boot: 16 failures from `samba-dcerpcd`,
`rpcd_classic`, `rpcd_winreg`, `rpcd_lsad`, `rpcd_epmapper`,
`rpcd_spoolss`, `rpcd_fsrvp`, `rpcd_mdssvc`, printed as 32 lines). This is
**accepted as cosmetic** (B.6): `/var/log/samba` is deliberately outside
the writable set, samba runs `--debug-stdout`, and nothing is lost — the
noise is the workers reporting that they will keep logging to stdout.

Wait for the image's own verdict rather than for a log line:

```sh
docker inspect --format '{{.State.Health.Status}}' dc1
# starting ... then: healthy
```

Confirm the domain is really there:

```sh
docker exec dc1 samba-tool domain level show
# Domain function level: (Windows) 2016
# Forest function level: (Windows) 2016
```

### 3.4 Switch to `SAMBA_MODE=run`, and why

Once the domain exists, change the service's environment:

```yaml
      SAMBA_MODE: run
```

and recreate the container (`docker compose up -d dc1`).

`run` **refuses to start when the state volume is absent**, with exit 21:

> `SAMBA_MODE=run needs an initialized domain but the volume holds no
> samba state (/var/lib/samba/private/sam.ldb is missing); mount the
> /var/lib/samba volume that holds the domain state, or set
> SAMBA_MODE=provision or join once to initialize it`

That refusal is the point. A misconfigured volume — a renamed volume, a
mount that did not happen, a host that lost its storage — would otherwise
be indistinguishable from a first boot, and `provision` or `auto` would
cheerfully create a **brand-new empty domain** over it. `run` turns a
silent data loss into a container that will not start.
(*Covered by:* `TestRunModeWithoutStateRefused`.)

In the other direction, `provision` and `join` refuse over existing state
with exit 20 (*Covered by:* `TestProvisionOverStateRefused`), so a
forgotten `SAMBA_MODE=provision` does not destroy the domain either. `run`
is still the right setting: it refuses in *both* of the situations that
lose data, and `auto` refuses in only one of them.

A restart in `run` mode changes nothing: a user created before a stop is
still there afterwards, the marker `/var/lib/samba/.image-state.json` is
identical field for field, and the second boot prints none of the
initialization or adoption lines. The replacement container may have a
different name — the DC's identity lives in `netbios name` in `smb.conf`,
not in the container's host name.
(*Covered by:* `TestIdempotentRestart`.)

### 3.5 Declarative `[global]` settings

The variables in §3.1 each own one setting. Anything else you want in the
DC's `[global]` section goes into **`SAMBA_GLOBAL_OPTIONS`**, one
`key = value` per line — a compose block scalar is the intended form:

```yaml
      SAMBA_GLOBAL_OPTIONS: |
        # a bigger log file, and a second log class for authentication
        max log size = 10000
        log level = 1 auth:3
```

Blank lines and comment lines — starting with `#` or `;`, smb.conf's own two
comment characters — are ignored; spacing and case in the key do not matter;
a key set twice keeps its last value and says so in the log. A line that is
not a `key = value` pair is refused with exit 10 rather than skipped,
because an option that silently never reaches `smb.conf` is invisible until
the day it was supposed to matter.

**It is reconciled on every start, not only at provision time.** Edit the
variable and recreate the container; the entrypoint rewrites `[global]`
atomically before any daemon reads the file, and announces each change:

```
entrypoint: SAMBA_GLOBAL_OPTIONS: replaced "max log size" = "20000" in /etc/samba/smb.conf
```

A start that changes nothing writes nothing and says nothing. Maintenance
mode applies none of it.

**Removing an entry does not remove the setting.** The reconciliation adds
and replaces; it never deletes, because it cannot tell a line it wrote last
boot from one you put in `smb.conf` yourself. To undo a setting, give it the
value you want — samba's default, written out explicitly, e.g.
`max log size = 5000` — or edit `/etc/samba/smb.conf` on the configuration
volume.

**What you put here reaches the running DC.** The image health-checks
itself by probing DNS, LDAP and SMB on the loopback address (§6.1), and the
SMB probe connects *anonymously*; a `[global]` setting that changes how the
DC answers there can leave the container reported unhealthy while it is
still serving members. Change one setting at a time and watch
`docker inspect --format '{{.State.Health.Status}}' dc1` after the restart.

**Settings another variable owns are refused**, with a message naming what
to set instead: `realm`, `workgroup`, `netbios name`, `ad dc functional
level`, `dns forwarder`, the `tls *` files (reserved for the `SAMBA_TLS_*`
variables), and `server role`, `dns update command`, `ntp signd socket
directory` and `include`, which the image manages itself.

**Every rewrite is checked by samba's own parser.** The entrypoint runs
`testparm` over the result; if it rejects the file, your previous `smb.conf`
is written back and the container refuses to start with exit 10, quoting
testparm:

> `testparm rejects the [global] settings SAMBA_GLOBAL_OPTIONS declares
> (Unknown parameter encountered: "this is not a parameter");
> /etc/samba/smb.conf has been put back to what it held before this start,
> so fix or remove the offending entry in SAMBA_GLOBAL_OPTIONS and start
> the container again`

Putting the file back is what makes the failure recoverable: `smb.conf`
lives on a volume, so a rejected rewrite left in place would break every
later start — including the one you make right after fixing the variable.
The edit is announced in the log only once the check has passed, so what you
read there is what the DC is running with.

A *warning* is not a rejection. A parameter samba still accepts but has
deprecated — `syslog only`, `lanman auth` and friends — makes testparm
print `WARNING: The "…" option is deprecated` and load the file anyway; the
entrypoint copies that line to the container log and carries on. What stops
a boot is a parameter samba does not know at all, or a value it cannot
parse.
(*Covered by:* `TestGlobalOptionsApplied`.)

---

## 4. Protocol check from a member

*Covered by:* `TestKerberosKinit`, `TestKerberizedSMB`,
`TestNTLMAuth`, `TestLDAPSCertificate`,
`TestDNSSRVRecords`, `TestSignedNTPWiring`

These checks run **from another container on the domain network**, not
inside the DC: a working loopback inside the DC would hide a broken
listener. The E2E suite builds a throwaway client image for exactly this
(`test/client/Dockerfile`, not a published artifact) from the same Debian
base as the DC, with `heimdal-clients`, `smbclient`, `ldap-utils`,
`bind9-dnsutils`, `chrony` and `ca-certificates`. Any domain member with
those tools will do.

Run the client with the DC as its resolver — Kerberos and SMB both start
from the realm's DNS records:

```sh
docker run --rm -it --network <your-addc-network> --dns 192.0.2.10 \
  -e PW \
  <your-client-image> sh
```

Throughout, `AD.EXAMPLE.COM` is the realm, `AD` the NetBIOS domain
and `dc1.ad.example.com` the DC.

### 4.1 DNS SRV records

```sh
dig +short SRV _ldap._tcp.ad.example.com @192.0.2.10
dig +short SRV _kerberos._udp.ad.example.com @192.0.2.10
dig +short dc1.ad.example.com @192.0.2.10
```

The first two must name **this DC on ports 389 and 88** — `dig +short`
prints `<prio> <weight> <port> <target>.` — and the target must itself
resolve to the DC's address. SRV records whose target does not resolve are
decoration.

### 4.2 Kerberos

```sh
printf %s "$PW" | kinit --password-file=STDIN Administrator@AD.EXAMPLE.COM
klist
```

`klist` must show `Administrator@AD.EXAMPLE.COM` and a
`krbtgt/AD.EXAMPLE.COM@AD.EXAMPLE.COM` ticket.

**Heimdal's `--password-file=STDIN` is the reason the password never
reaches a command line** — not an argv, not a process listing, not
`docker inspect`. Use it.

The negative control matters as much as the positive one: the same command
with a wrong password must fail with `Password incorrect`. A failure for
any *other* reason (no KDC found, broken DNS, clock skew) also exits
non-zero and would leave "the KDC authenticates" unproven.

### 4.3 Kerberized SMB

With a ticket in the cache, and **no password on the command line**:

```sh
smbclient --use-kerberos=required //dc1.ad.example.com/netlogon -c ls
```

Expect a directory listing ending in `blocks of size`. The negative
control: the same command with an empty ticket cache must fail with
`Could not find a suitable mechtype in NEG_TOKEN_INIT` — with Kerberos
required and nothing cached, the client has no mechanism left to offer.

### 4.4 NTLM

The legacy path a non-Kerberos member still uses:

```sh
smbclient --use-kerberos=off //dc1.ad.example.com/netlogon \
  -U "AD\\Administrator%$PW" -c ls
```

Expect `blocks of size`; a wrong password must give `NT_STATUS_LOGON_FAILURE`.

This is the form the test runs, and it puts the password in the container's
process listing for the life of the command. Interactively, drop the
`%$PW` and let `smbclient` prompt — that variant is equivalent but is
*not* what the test exercises.

### 4.5 LDAPS with the DC's own certificate

Copy the DC's CA out, get it into the member, and use it as the **only**
trust anchor. The certificate is public material, so it travels as an
ordinary file — nothing here is secret.

On the host:

```sh
docker cp dc1:/var/lib/samba/private/tls/ca.pem ./ca.pem
```

Start the member with that file mounted — the `docker run` from the §4
preamble, plus one bind:

```sh
docker run --rm -it --network <your-addc-network> --dns 192.0.2.10 \
  -e PW \
  -v "$PWD/ca.pem:/tmp/ca.pem:ro" \
  <your-client-image> sh
```

and from inside it:

```sh
LDAPTLS_CACERT=/tmp/ca.pem LDAPTLS_REQCERT=demand \
  ldapsearch -H ldaps://dc1.ad.example.com:636 -x -s base -b '' \
  defaultNamingContext dnsHostName
```

Expect `defaultNamingContext: DC=ad,DC=example,DC=com`, `dnsHostName:
dc1.ad.example.com` and `result: 0 Success`. The negative control — the
same query with `REQCERT=demand` and **no** `LDAPTLS_CACERT` — must fail,
or the client is not verifying anything.

**On branches 4.22 and 4.23 the DC's self-signed certificate carries a
byte-reversed serial**, which is DER-negative about half the time (those
branches write the 32-bit generation time in host byte order; 4.24 writes
it big-endian). RFC 5280 requires a positive serial, so a **strict** parser
refuses the certificate outright — Go's `crypto/x509` does. **Nothing a
client does is affected:** GnuTLS and OpenSSL accept it, the handshake
succeeds, and the `ldapsearch` above passes on all three branches. If your
own tooling parses the certificate with a strict library, this is the
cause; the limitation is upstream's and is recorded in B.6.

#### Bring your own certificate

Everything above describes the certificate samba generates for itself. To
serve LDAPS with material a real CA issued — the usual reason being that
members must trust the DC without being handed a one-off CA — mount the three
PEM files into the container and name them:

```yaml
services:
  dc1:
    environment:
      SAMBA_TLS_CERT_FILE: /run/secrets/tls/cert.pem
      SAMBA_TLS_KEY_FILE: /run/secrets/tls/key.pem
      SAMBA_TLS_CA_FILE: /run/secrets/tls/ca.pem
    volumes:
      - ./tls:/run/secrets/tls:ro
```

The paths are **absolute paths inside the container**, and a relative one is
refused with exit 10: samba resolves a relative TLS path against the private
directory on the state volume, the entrypoint would resolve it against its own
working directory, and a check that passed on a file samba never opens would
be worse than no check. `/run` is a tmpfs and a bind mounted under it works —
it is where this image already puts every `*_FILE` secret. Do not put the
material on the `/etc/samba` or `/var/lib/samba` volumes: those belong to the
DC, and material you renew from outside has no business living on one.

**All three or none.** Setting one or two is refused with exit 10 naming the
ones that are missing. It is refused rather than half-applied because a
partial trio does not fail loudly: whatever you leave unset keeps samba's own
default path under the state volume, so the DC would build its chain from two
different places — and come up healthy while doing it. Rather than have you
discover which certificate is actually on the wire, the entrypoint says no.

**The private key must be `chmod 600` and owned by the container user
(uid 0).** This is samba's rule and it is fatal, not advisory: a key at any
other mode — `0400` included — makes samba refuse to start its LDAP server,
citing CVE-2013-4476, and the whole domain controller then terminates. Docker
preserves the host file's ownership across a bind mount, so on Linux a key
you generated as yourself arrives owned by *your* uid and is refused however
carefully you set its mode. On the host:

```sh
sudo chown 0:0 ./tls/key.pem
sudo chmod 600 ./tls/key.pem
chmod 644 ./tls/cert.pem ./tls/ca.pem
```

The certificate and the CA are public material; their mode is not checked.
The entrypoint verifies all of this **before** it starts anything and refuses
with exit 11 — the same class as a missing password file, because one of the
three is a private key — naming the variable, the path and the command to
run. It never prints the content.

**Delivering a root-owned `0600` key without `sudo`.** Both orchestrators can
do it declaratively, and that is the better answer wherever it is available:

*Docker Swarm* — the long form of `secrets:` sets owner and mode on the file
it materialises, so nothing on the host has to be root-owned:

```yaml
services:
  dc1:
    secrets:
      - source: dc-tls-key
        target: /run/secrets/tls/key.pem
        uid: "0"
        gid: "0"
        mode: 0600
      - source: dc-tls-cert
        target: /run/secrets/tls/cert.pem
        mode: 0644
      - source: dc-tls-ca
        target: /run/secrets/tls/ca.pem
        mode: 0644
```

*Kubernetes* — a `secret` volume, with the key's mode set on its item. The
container runs as root in this image, so the file is owned by uid 0 already:

```yaml
      volumes:
        - name: dc-tls
          secret:
            secretName: dc-tls
            defaultMode: 0644
            items:
              - key: key.pem
                path: key.pem
                mode: 0600
              - key: cert.pem
                path: cert.pem
              - key: ca.pem
                path: ca.pem
```

*Plain Compose or `docker run`* — a bind mount carries the host file's owner
and mode through unchanged, which is why this is the one case that needs the
`chown`/`chmod` above. `secrets:` with a `file:` source in non-Swarm Compose
is a bind mount too, and behaves the same way.

**The certificate has to match the name clients dial.** Its subject
alternative name must carry the DC's FQDN (`dc1.ad.example.com`), the name
the realm's DNS hands out; a certificate for the container's short name or
its address will fail verification in exactly the `ldapsearch` above.

**What happens when.** At **provision** the three settings are passed to
`samba-tool domain provision`, so the very first `smb.conf` already names
your material and the DC never serves a self-signed certificate at all — with
the trio set, samba generates none (`/var/lib/samba/private/tls` stays
empty). On **every later start**, provision-, join- and run-mode alike, the
three settings are reconciled into `/etc/samba/smb.conf` the same way §3.5's
declarative settings are, and each change is one log line. Maintenance mode
touches none of it.

**To go back** to the certificate samba makes for itself, unset the three
variables and recreate the container. The entrypoint then takes the three
`tls *` lines back out of `/etc/samba/smb.conf` — one log line each,
`removed "tls keyfile" from /etc/samba/smb.conf: SAMBA_TLS_*_FILE are unset` —
and samba autogenerates its own self-signed material on the next start, at
`0600`, even on a DC that never had any. Measured: `Attempting to autogenerate
TLS self-signed keys for https for hostname '…'` / `TLS self-signed keys
generated OK`, and `ldapsearch` over `ldaps://` works against the newly
generated CA. Removal is the one thing the reconciliation does that §3.5's
declarative settings do not get, and deliberately so: these three keys are the
image's own, which is what makes taking them out safe.

**To renew**, replace the files with the new material at the same paths and
recreate the container. Nothing here reloads a certificate in a running
server, and nothing watches its expiry — that is your monitoring, not this
image's. Because the paths did not change, the reconciliation writes nothing
and samba simply reads the new files at startup.

(*Covered by:* `TestCustomTLSMaterial`.)

### 4.6 Time

```sh
chronyd -Q -t 30 "server 192.0.2.10 iburst"
```

Expect a line containing `System clock wrong by`. `-Q` measures without
disciplining the client's clock.

**What this proves and what it does not.** It proves the DC serves time
from a clock it is not allowed to discipline, and (on the DC side) that
samba creates the MS-SNTP signing socket and chrony is configured against
that exact directory. It does **not** prove a *signed* reply: chrony opens
the signing socket lazily, only for a request carrying an authenticator,
which requires a client authenticating as a domain machine account. A
regression inside the signing path itself surfaces as a chrony log error,
not as a failed check here (B.6).

On the DC, the two ends must name the same directory — and they live in
two files nothing reconciles:

```sh
docker exec dc1 sh -c 'testparm -s --parameter-name="ntp signd socket directory"'
docker exec dc1 grep '^ntpsigndsocket' /run/chrony/chrony.conf
```

`/run/chrony/chrony.conf` is generated at daemon start from the baked
template at `/etc/chrony/chrony.conf`, with that one line rewritten to
whatever this DC's own `smb.conf` declares.

---

## 5. Scale-out: an additional domain controller

*Covered by:* `TestJoinReplicationBothWays`

### 5.1 The compose service

Added to the file from §3.1:

```yaml
secrets:
  join_password:
    file: ./secrets/join_password  # REPLACE: the join account's password

volumes:
  dc2-state:
  dc2-conf:

services:
  dc2:
    image: ghcr.io/esitc-paris/samba-ad-dc:4.24.7-r1   # the SAME tag as dc1
    container_name: dc2
    hostname: dc2
    networks:
      addc:
        ipv4_address: 192.0.2.11   # REPLACE
    # A joining DC resolves through a DC: dc1. This is a requirement, not a
    # convenience — the realm's SRV records are how the join finds the DC
    # to join. dc2 needs no forwarder of its own because every lookup it
    # makes is answered by dc1, which has one (§1.6).
    dns:
      - 192.0.2.10                 # REPLACE: dc1's address
    environment:
      SAMBA_MODE: join             # switch to `run` after the join (§3.4)
      SAMBA_REALM: AD.EXAMPLE.COM  # REPLACE
      # SAMBA_JOIN_USERNAME defaults to Administrator.
      SAMBA_JOIN_PASSWORD_FILE: /run/secrets/join_password
    secrets:
      - join_password
    volumes:
      - dc2-state:/var/lib/samba
      - dc2-conf:/etc/samba
    # --- the hardened profile (§1.8) ---
    read_only: true
    tmpfs: [/run, /tmp, /var/cache/samba]
    cap_drop: [ALL]
    cap_add: [SYS_ADMIN, NET_BIND_SERVICE, CHOWN, FOWNER, SETUID, SETGID]
    security_opt: ["no-new-privileges:true"]
    stop_grace_period: 15s
    restart: unless-stopped
```

If you instead point `dc2` at **itself** as its resolver, it needs its own
`SAMBA_DNS_FORWARDER` for the reason in §1.6 — a DC that resolves through
itself and has no upstream stalls 4–8 s per non-authoritative name and
replication times out.

### 5.2 What the join does to `smb.conf`

`samba-tool domain join` renders its own configuration file from a
template and has **no `--option` passthrough**, so the entrypoint edits
that file once, atomically, immediately after the join and before any
daemon starts. Three settings are forced in, each announced on its own log
line with the reason:

- `ad dc functional level` — mirrored from `SAMBA_FUNCTION_LEVEL`, so the
  joined DC does not advertise samba's `2008_R2` default inside a 2016
  domain;
- `dns forwarder` — whenever `SAMBA_DNS_FORWARDER` is set;
- `dns update command` — pinned to `samba_dnsupdate --use-samba-tool`,
  because the image ships no `nsupdate` (B.6).

Every edit is idempotent and scoped to `[global]`, and an existing entry
carrying a *different* value is replaced rather than trusted.

### 5.3 Bring it up and wait

```sh
docker compose up -d dc2
docker inspect --format '{{.State.Health.Status}}' dc2
```

**A join takes materially longer than a provision.** It replicates every
naming context of the domain over DRS before the entrypoint even starts
samba; the E2E suite budgets **8 minutes** for the joining DC to reach
healthy on a cold volume. Expect these lines:

```
entrypoint: joining realm AD.EXAMPLE.COM as a domain controller with account Administrator
entrypoint: joined realm AD.EXAMPLE.COM
```

Then switch `dc2` to `SAMBA_MODE: run` (§3.4) and recreate it.

### 5.4 Verify replication in both directions

Healthy containers are not evidence: a directory serves stale reads
happily, and a link that exists one way is the classic half-broken
multi-DC domain. Check both halves.

**Object propagation, both ways.** Create on each side, read on the other:

```sh
docker exec dc1 samba-tool user create alice --random-password
docker exec dc2 samba-tool user create bob   --random-password

docker exec dc2 samba-tool user show alice   # dc1 -> dc2
docker exec dc1 samba-tool user show bob     # dc2 -> dc1
```

**The two directions are not symmetric, and the difference is normal.**
`dc1 -> dc2` rides the connection the join itself created and arrives
within seconds. `dc2 -> dc1` has to wait for dc1's KCC to build the
reverse connection object and run its first pull over it — **measured at
about 3 minutes** on an idle machine. Only after that does dc2 hold dc1 in
its notify list and later changes arrive in seconds. Poll; do not conclude
from one failed read.

**The links themselves**, on each DC:

```sh
docker exec dc1 samba-tool drs showrepl
docker exec dc2 samba-tool drs showrepl
```

Read it this way:

- each DC must list the **other** under `==== INBOUND NEIGHBORS ====`,
  printed as `<site>\<NETBIOS NAME>` in upper case. An empty INBOUND list
  means that DC can never learn anything from the domain;
- every `N consecutive failure(s)` counter, in **both** sections, must be
  `0`. The counter — not the presence of the word "failed" — is the
  verdict: samba keeps printing the last failed attempt of a partner that
  has since recovered;
- an empty `==== OUTBOUND NEIGHBORS ====` list is a normal *transient*
  state on a freshly joined DC: a DC appears in its partner's notify list
  only once that partner has registered itself there.

**Functional-level parity.** Two independent checks, because either alone
can be satisfied by a broken domain:

```sh
docker exec dc1 samba-tool domain level show
docker exec dc2 samba-tool domain level show
# identical output — these values live in the directory and replicate

docker exec dc1 sh -c 'testparm -s --parameter-name="ad dc functional level"'
docker exec dc2 sh -c 'testparm -s --parameter-name="ad dc functional level"'
# identical — this one comes from smb.conf and is NOT replicated
```

A join that did not mirror the level leaves dc2 advertising `2008_R2`
inside a 2016 domain — which both DCs then report identically as a lowered
"lowest function level of a DC", so the first check alone cannot see it.

**Sysvol does not replicate.** Group policy content is not covered by any
of the above; see §8.

---

## 6. Day-2 basics

*Covered by:* `TestDBConsistency`

### 6.1 Health

The image's `HEALTHCHECK` is an **application-level** probe, not a process
check:

```
HEALTHCHECK --interval=30s --timeout=10s --start-period=180s --retries=3
```

`entrypoint healthcheck` reads the realm from `/etc/samba/smb.conf` and
then asks, **on the loopback address only**, the three protocols a domain
member uses, in the order it uses them:

1. the `_ldap._tcp.<realm>` SRV record on `127.0.0.1:53`;
2. an anonymous rootDSE read on `ldap://127.0.0.1:389`;
3. a share enumeration with `smbclient -L 127.0.0.1 -N`.

It exits `0` only when all three answer, and `1` otherwise — docker's
healthy/unhealthy values, never the entrypoint's refusal codes. **Do not
override it** with a process check: the failure modes that matter here
(DNS not serving the realm, LDAP up but the directory unreadable, smbd not
exporting the shares) all leave the process running.

Read the history, not just the status:

```sh
docker inspect --format '{{json .State.Health}}' dc1 | python3 -m json.tool
```

`.Log` holds the last probe runs with their exit codes and output — which
of the three questions failed is in there, and it is the difference
between "DNS is broken" and "the DC is down".

The 180 s start period is deliberate: a first-boot provision on a cold
volume legitimately takes minutes (§3.3), and a restored volume costs even
more (§7.5).

### 6.2 Logs

**Everything goes to stdout/stderr; there are no log files to collect.**
Samba runs `--foreground --no-process-group --debug-stdout`, and every
entrypoint line is prefixed `entrypoint: `. `SAMBA_LOG_LEVEL` (default
`1`) sets samba's debug level.

```sh
docker compose logs -f dc1
```

The `reopen_one_log ... Read-only file system` lines on every boot are
**expected and accepted** (§3.3, B.6) — 16 failures printed as 32 lines.
Do not "fix" them by making `/var/log/samba` writable: the levers that
would silence them all redirect where diagnostics go, and quietly losing a
class of log lines is a worse outcome than cosmetic noise.

A clean stop prints, in this order:

```
entrypoint: received terminated: stopping samba, then chronyd   # or "shutdown requested: ..."
entrypoint: samba stopped ...
entrypoint: chronyd stopped
```

The **order** is the contract: the directory leaves the network before the
time service it depends on. The container exits `0` — a DC asked to stop
has not failed. Samba's own exit code on SIGTERM is `127` by design and
never reaches the container's. The two stops share **one 10 s budget**, so
give the container a `stop_grace_period` above it (the examples use 15 s).

### 6.3 Time

Nothing to do day to day beyond §1.4: keep the **host** disciplined, and
the DC serves MS-SNTP to members on 123/udp. Verify the service from a
member with `chronyd -Q` (§4.6) and the wiring on the DC with the two
`testparm`/`grep` commands there.

### 6.4 Database consistency

Two forms, and the difference matters.

**On the running DC**, read-only, any time:

```sh
docker exec dc1 samba-tool dbcheck
# ... (0 errors)
```

**From a maintenance-mode container on the stopped volume** — the
documented procedure, and the one to use before or after anything
invasive:

```sh
docker compose stop dc1

docker run --rm \
  -e SAMBA_MODE=maintenance \
  -e SAMBA_MAINTENANCE_OP=check \
  -v dc1-state:/var/lib/samba \
  -v dc1-conf:/etc/samba \
  --read-only \
  --tmpfs /run --tmpfs /tmp --tmpfs /var/cache/samba \
  --cap-drop ALL \
  --cap-add SYS_ADMIN --cap-add NET_BIND_SERVICE --cap-add CHOWN \
  --cap-add FOWNER --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges:true \
  ghcr.io/esitc-paris/samba-ad-dc:4.24.7-r1

docker compose start dc1
```

Expected output:

```
entrypoint: running samba-tool dbcheck
... (0 errors)
entrypoint: database check completed with no errors; maintenance mode does not start the domain controller
```

Maintenance mode **exits without starting the DC**: `0` on a clean check,
**23** on failure. `SAMBA_MAINTENANCE_OP=repair` runs `dbcheck --fix
--yes` instead. Take a backup (§7) before a repair.

Two properties of maintenance mode that are deliberate:

- **The volume must be quiescent.** Stop the DC first; the mode is
  documented against a stopped volume and that is what is tested.
- **The version guards run first.** A volume written by a newer Samba is
  refused with **22** and a malformed marker with **10**, *before* any
  dbcheck. `dbcheck --fix` driven by an older Samba against a newer
  database is exactly the hazard the downgrade guard exists for, and
  "repair" is the mode an operator reaches for when something is already
  wrong. (*Covered by:* `TestDowngradeRefused`.)

**Scope caveat.** Bare `samba-tool dbcheck` — which is what maintenance
mode runs — checks the **default naming context only**. The schema, the
configuration and the DNS partitions are covered by `--cross-ncs`, which
the shipped procedure does not run. Run it by hand on a stopped volume if
you have reason to suspect those partitions; that variant is not covered
by a test.

### 6.5 Upgrades

Replacing the image on an existing volume runs the version guard and, on a
newer Samba, a full `dbcheck` before the DC starts. The procedure, the
multi-DC rollout order and the rollback rule (restore-based, never a tag
downgrade) are in
[`docs/update-guide.md`](update-guide.md#3-patch-update-step-by-step).

---

<a id="backup-and-restore-runbook"></a>

## 7. Backup and restore runbook

*Covered by:* `TestOfflineBackupRestore`

**Offline backup is the primary, CI-tested path** — credential-free and
automation-friendly. Online backup is a documented alternative requiring
administrator credentials and is not covered by a test.

**A filesystem snapshot or `cp` of a live volume is not a valid backup of
an AD database.** `samba-tool domain backup offline` takes exclusive locks
on the databases it copies; that is precisely why the procedure below
points at it rather than at a copy, and why the volumes are mounted
**read-write** for the backup.

Everything runs as **one-off containers of the image**, never as a shell
inside a live DC.

**What this runbook assumes, and why the names change.** The worked
example restores the **single-DC domain of §3** — `dc1` on
`dc1-state`/`dc1-conf` — into a new DC called `dc3`, on new volumes
`dc3-state`/`dc3-conf`, at a new address. The identity is deliberately
*not* `dc2`'s: §5 gave that name and those volumes to the joined DC, and
reusing either here is a way to lose a working domain rather than recover
one.

- Reusing the **name** fails the way finding 1 below describes — `Entry
  CN=DC2,OU=Domain Controllers,... already exists` — because the restore
  adds the new DC's account before removing the backed-up ones.
- Reusing the **volumes** is worse: it would restore over a live replica's
  state. The `rmdir` in §7.3 is the safety net for exactly this — it fails
  on a non-empty directory, so the command cannot eat a volume that holds
  a domain — but a safety net is not a plan.

On a multi-DC domain, choose a name no DC in the directory holds and a
volume pair nothing is using, and read §7.2 before running anything.

### 7.1 Take the backup

```sh
docker compose stop dc1

docker volume create dc1-backup

docker run --rm \
  -v dc1-state:/var/lib/samba \
  -v dc1-conf:/etc/samba \
  -v dc1-backup:/backup \
  --read-only \
  --tmpfs /run --tmpfs /tmp --tmpfs /var/cache/samba \
  --cap-drop ALL \
  --cap-add SYS_ADMIN --cap-add NET_BIND_SERVICE --cap-add CHOWN \
  --cap-add FOWNER --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges:true \
  --entrypoint samba-tool \
  ghcr.io/esitc-paris/samba-ad-dc:4.24.7-r1 \
  domain backup offline --targetdir=/backup

docker compose start dc1
```

Expect `Backup succeeded.` — and then **verify the artefact exists**,
immediately, rather than discovering it missing during a restore:

```sh
docker run --rm \
  -v dc1-backup:/backup:ro \
  --read-only --tmpfs /run --tmpfs /tmp --tmpfs /var/cache/samba \
  --cap-drop ALL --security-opt no-new-privileges:true \
  --entrypoint ls \
  ghcr.io/esitc-paris/samba-ad-dc:4.24.7-r1 -l /backup
# samba-backup-<realm>-<timestamp>.tar.bz2
```

`Backup succeeded.` is samba's opinion; the listing is the evidence. Copy
the tarball off the host — a backup on the same machine as the DC is not a
disaster-recovery plan.

### 7.2 Restore: what changes, and what does not

Read this before running §7.3. Four findings from making the restore
actually work; each is the difference between a procedure that works and
one that looks like it should.

**1 — The restore cannot reuse the backed-up DC's name.**
`samba-tool domain backup restore` requires `--newservername`, and it
**adds that DC's account to the restored database before removing the DCs
the backup came from**. Passing the old name fails with `Entry
CN=<NAME>,OU=Domain Controllers,... already exists`. **A restore is always
a restore onto a new DC name.** The domain, its SID and its objects are
what survive.

**2 — The target directory must be empty, and a fresh volume is not.**
The samba package ships empty `/var/lib/samba/private` and
`/var/lib/samba/bind-dns` directories, and docker copies whatever the
image has at a volume's mount point into a new volume — so the restore
refuses with `Target directory is not empty`. `rmdir` on exactly those two
paths is the fix, and it is also a safety net: `rmdir` fails on a
non-empty directory, so the command can never eat a volume that actually
holds a domain.

**3 — The restore strips the realm's service records, and the restored DC
must resolve through itself to re-register them.** The restore removes
every Server object other than the new one, and with them the SRV and
CNAME records that pointed at the old DC. Nothing adds records for the new
name — that is `samba_dnsupdate`'s job at first start. Until it runs,
`_ldap._tcp.<realm>` has no answer, which is the *first* thing the health
probe asks, so the container stays `starting` forever.

`samba_dnsupdate` can only do that job if the DC resolves through
**itself**. Left on a resolver that forwards back to the DC's own
`dns forwarder`, the query loops until it times out (observed: 17 s per
name, then the update aborts). This is a **procedural requirement of the
restore**, not a test convenience.

**4 — The restored DC does not have a provisioned DC's layout.** The
restored `smb.conf` points `state directory` and the sysvol share **into
`/var/lib/samba/state`**. It does not move everything, and the difference
is measured rather than inferred: `cache directory` and `lock directory`
stay exactly where a provisioned DC has them. Measured on a restored DC
that had reached healthy:

| parameter | provisioned DC | restored DC |
|---|---|---|
| `state directory` | `/var/lib/samba` | `/var/lib/samba/state` |
| `cache directory` | `/var/lib/samba/cache` | `/var/lib/samba/cache` — unchanged |
| `lock directory` | `/var/lib/samba` | `/var/lib/samba` — unchanged |
| sysvol `path` | `/var/lib/samba/sysvol` | `/var/lib/samba/state/sysvol` |
| `ntp signd socket directory` | `/var/lib/samba/ntp_signd` | **`/var/lib/samba/ntp_signd`** — unchanged |

samba itself is self-consistent: every path is read from `smb.conf`, which
the restore rewrote, so nothing inside the container breaks. **The caveat
is for paths a human or a script learned from a provisioned DC.** Derive
them, never hardcode them:

```sh
docker exec <dc> sh -c 'testparm -s --parameter-name="path" --section-name=sysvol'
docker exec <dc> sh -c 'testparm -s --parameter-name="state directory"'
docker exec <dc> sh -c 'testparm -s --parameter-name="cache directory"'
```

Any backup or GPO procedure that names a path must say which of the two
layouts it assumes.

The MS-SNTP signing socket is the deliberate exception in that table: the
restored `smb.conf` does not set `ntp signd socket directory` at all, and
samba's compile-time default for it does **not** track `state directory`.
This is recorded explicitly because the opposite is the natural conclusion
to draw from the sysvol path, and the measurement says otherwise.

### 7.3 Restore into a fresh instance

Into **fresh volumes**. A restore that overwrote the volumes it was taken
from would prove nothing about recovering a lost DC.

Use **the same image tag the backup was taken with** — that is what the
test exercises, and it is what keeps the restored volume openable: a
volume written by a newer Samba than the image provides is refused with
exit 22 (§6.4).

```sh
docker volume create dc3-state
docker volume create dc3-conf

docker run --rm \
  -v dc3-state:/var/lib/samba \
  -v dc3-conf:/etc/samba \
  -v dc1-backup:/backup:ro \
  --read-only \
  --tmpfs /run --tmpfs /tmp --tmpfs /var/cache/samba \
  --cap-drop ALL \
  --cap-add SYS_ADMIN --cap-add NET_BIND_SERVICE --cap-add CHOWN \
  --cap-add FOWNER --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges:true \
  --entrypoint sh \
  ghcr.io/esitc-paris/samba-ad-dc:4.24.7-r1 -c '
set -e
rmdir /var/lib/samba/private /var/lib/samba/bind-dns
samba-tool domain backup restore \
  --backup-file="$(ls /backup/samba-backup-*.tar.bz2)" \
  --targetdir=/var/lib/samba \
  --newservername=dc3
cp /var/lib/samba/etc/smb.conf /etc/samba/smb.conf
'
```

Expect `Backup file successfully restored to /var/lib/samba`.

The last line is not optional: the restored tree is self-contained under
the target directory, **including its own `etc/smb.conf` with every path
rewritten to match** (finding 4). Copying it to `/etc/samba/smb.conf` is
what puts it where the image reads it from.

`--newservername=dc3` must be a name **no DC in the domain already
holds** (finding 1) — `dc1` and, if you followed §5, `dc2` are both taken
— and, since this container will later be started as a DC, at most 15
characters (§1.7). `dc3-state` and `dc3-conf` must likewise be volumes
nothing is using.

### 7.4 Branch 4.22 only: `--cap-add DAC_OVERRIDE` on the restore

**On branch 4.22, and only on the one-off restore container above, add:**

```sh
  --cap-add DAC_OVERRIDE \
```

Samba 4.22.11's restore reaches the sysvol NT-ACL step through smbd and
dies there under the capability set every other container runs green with
(measured locally, arm64, 2026-09-16, deterministic over 3 runs of 3):

```text
py_smbd_mkdir: mkdirat error=13 (Permission denied)
ERROR(<class 'SystemError'>): uncaught exception - <built-in function mkdir> returned NULL without setting an exception
  File ".../samba/ntacls.py", line 631, in backup_restore
    smbd.mkdir(dst, session_info, service)
```

exiting 255. The same restore succeeds with `DAC_OVERRIDE` added; **4.23
and 4.24 need nothing added at all.**

**Add it to nothing else.** Not to the DC that afterwards serves the
restored domain, not to the backup container, not to the listing. Widening
the set a DC *runs* under to accommodate a one-off recovery command would
spend a real privilege permanently to buy a transient one. The E2E suite
expresses exactly this scoping through `E2E_RESTORE_CAPS`, which applies
to the restore one-off only.

### 7.5 Start the restored DC

Give it a `resolv.conf` pointing at itself (finding 3). Bind-mounting the
file works on any network type and is what the test does:

```sh
printf 'nameserver 127.0.0.1\n' > ./restored-resolv.conf
```

```yaml
  dc3:
    image: ghcr.io/esitc-paris/samba-ad-dc:4.24.7-r1
    container_name: dc3
    hostname: dc3
    environment:
      SAMBA_MODE: run              # the domain already exists on the volume
    volumes:
      - dc3-state:/var/lib/samba
      - dc3-conf:/etc/samba
      - ./restored-resolv.conf:/etc/resolv.conf:ro
    networks:
      addc:
        ipv4_address: 192.0.2.12   # REPLACE
    # --- the hardened profile (§1.8), unchanged: no DAC_OVERRIDE here ---
    read_only: true
    tmpfs: [/run, /tmp, /var/cache/samba]
    cap_drop: [ALL]
    cap_add: [SYS_ADMIN, NET_BIND_SERVICE, CHOWN, FOWNER, SETUID, SETGID]
    security_opt: ["no-new-privileges:true"]
    stop_grace_period: 15s
```

**Expect a slow, noisy first boot, and expect the dbcheck.** The E2E suite
budgets **6 minutes** for a restored DC to reach healthy — more than a
plain restart, because two things happen that a restart does not do: the
entrypoint runs a full `dbcheck` before adopting the volume, and samba has
to re-register the realm's SRV records from scratch before the health
probe's first question can be answered at all.

`samba-tool domain backup offline` archives the marker as part of the
state directory, so the restore puts it back at
`/var/lib/samba/state/.image-state.json` — **not** at the path the
entrypoint reads. The container therefore sees samba state with no marker
of its own, which is exactly the documented "foreign volume" case: check
the database, then claim it. The log says so:

```
entrypoint: the volume holds samba state but no .image-state.json marker: checking the database before adopting it
... (0 errors)
entrypoint: volume adopted: marker written for samba 4.24.7
```

### 7.6 Verify the restore

```sh
# the domain that came back is the one that was backed up, not an empty
# one wearing its name
docker exec dc3 samba-tool user show <a-user-you-know-existed>

# the health verdict
docker inspect --format '{{.State.Health.Status}}' dc3

# signed-NTP wiring survived the smb.conf rewrite (§4.6)
docker exec dc3 sh -c 'testparm -s --parameter-name="ntp signd socket directory"'
docker exec dc3 grep '^ntpsigndsocket' /run/chrony/chrony.conf

# and the time service really answers, from a member (§4.6)
chronyd -Q -t 30 "server 192.0.2.12 iburst"
```

Sysvol content is **not** verified by any of this; see §8.

---

## 8. Sysvol replication is manual

**No test covers this section.** It is a stated limitation (B.6, SPEC
§12.4), and the procedure below is a **sketch**: it has not been exercised
by the E2E suite or measured, and it is offered as a starting point rather
than as a supported procedure. An integrated, tested synchronization
mechanism is committed roadmap (v2).

**Samba does not implement DFS-R.** With more than one DC, the *directory*
replicates — §5.4 proves that in both directions — but **group policy
content under sysvol does not replicate by itself**. A GPO edited on one
DC is invisible to members that happen to authenticate against another.

The conventional workaround is a one-way copy from the **PDC-emulator**
holder to every other DC, preserving extended attributes and ACLs, on a
schedule.

**Find the PDC emulator:**

```sh
docker exec dc1 samba-tool fsmo show
# PdcEmulationMasterRole owner: CN=NTDS Settings,CN=<DC>,...
```

**Derive the sysvol path on each DC** — do not hardcode it, for the reason
in §7.2 finding 4 (a restored DC keeps sysvol under
`/var/lib/samba/state/sysvol`):

```sh
docker exec <dc> sh -c 'testparm -s --parameter-name="path" --section-name=sysvol'
```

**The copy itself cannot run inside this image.** The DC image ships no
`rsync` (SPEC §5.1 image minimality). Run it from a helper container or
from the host, mounting both DCs' state volumes, with a tool that
preserves **extended attributes and POSIX ACLs** — `rsync -aXA
--delete` or equivalent. Dropping the xattrs drops the NT ACLs, which is
worse than not copying at all.

**After the copy, reset the NT ACLs on the receiving DC**, which *is* a
command the image ships:

```sh
docker exec <receiving-dc> samba-tool ntacl sysvolreset
```

Until the roadmap item lands, treat this as operator-owned: schedule it,
monitor it, and verify a GPO change is visible on every DC before you rely
on it.

---

## Appendix: exit codes

Immutable once released. Every failure message is one line, cause then
remedy, separated by `;`, on stderr, prefixed `ERROR: `.

| Code | Meaning |
|---|---|
| `0` | success |
| `10` | configuration error |
| `11` | secret material missing, unreadable, or (the TLS private key) not mode `0600` and owned by the container user |
| `20` | provision/join refused over existing state |
| `21` | run mode with absent state |
| `22` | downgrade refusal |
| `23` | database consistency check failure |
| `30` | samba runtime failure |

The authoritative table, with the modes and the state machine it belongs
to, is the profile's
[Runtime contract](adaptation-profile.md#runtime-contract).

---

## Section → test map (§10.6)

| Section | E2E test |
|---|---|
| §1 Constraints first | `TestProvision` (the constrained profile is what every DC in the suite runs under); `TestPlainEnvSecretRejected`, `TestMissingSecretFailsFast` for §1.5 |
| §2 Verify before the first run | *(none — a property of the published image; `post-push-verify.yml`, SPEC §8.5)* |
| §3 The first domain controller | `TestProvision`, `TestIdempotentRestart`; `TestRunModeWithoutStateRefused`, `TestProvisionOverStateRefused` for §3.4; `TestGlobalOptionsApplied` for §3.5 |
| §4 Protocol check from a member | `TestDNSSRVRecords`, `TestKerberosKinit`, `TestKerberizedSMB`, `TestNTLMAuth`, `TestLDAPSCertificate`, `TestSignedNTPWiring`; `TestCustomTLSMaterial` for §4.5 *Bring your own certificate* |
| §5 Scale-out: an additional DC | `TestJoinReplicationBothWays` |
| §6 Day-2 basics | `TestDBConsistency`; `TestDowngradeRefused` for the maintenance-mode guards; `TestUpgradeFromLastPublished` for §6.5 |
| §7 Backup and restore runbook | `TestOfflineBackupRestore` |
| §8 Sysvol replication is manual | *(none — stated limitation, B.6 / SPEC §12.4)* |
