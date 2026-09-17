# Reuse guide — building a project on this image

<!-- GO-LIVE: delete this comment and the blockquote below at the same time as README's Status block, once the first release is published (docs/operations.md, "Going live: the first publication", step 7). -->
> **Status: pre-release.** No image has been published yet. The contract
> below is the contract the code is tested against, but no tag exists on
> the registry until the watcher's first publication, so every `FROM` and
> every `image:` naming a published tag in this guide will fail until
> then.

This image is a **domain controller and nothing else**. It is built to be
the base another project stands on: a school domain replacing a Windows
server, a lab, an appliance. This guide is the contract that project
builds on — what it may rely on, what it must not change, what it has to
bring itself, and which half of each answer is proven by a test.

**This guide restates the contract; it does not define it.**
[`docs/adaptation-profile.md`](adaptation-profile.md) is authoritative —
its **Runtime contract** section for the variables, the modes, the exit
codes and the health check, and **B.3** / **B.6** for the deployment
constraints and the known limitations. Where the two disagree, the
profile wins and this file is the bug.

**Sections 1 to 4 and 7 are covered by tests, and name them** (§10.6) on a
*Covered by:* line. **Sections 5 and 6 are out of band**: they document
how a downstream project drives this image from outside, which nothing in
*this* repository's suite exercises, and they say so in those words rather
than implying coverage. Test IDs are frozen and mapped row by row in
[`docs/traceability.md`](traceability.md).

Related: [`README.md`](../README.md) for the quickstart and the
configuration reference, [`docs/deployment-guide.md`](deployment-guide.md)
for the deployment itself, [`docs/update-guide.md`](update-guide.md) for
pinning and upgrades.

---

<a id="what-this-image-is-and-is-not"></a>

## 1. What this image is, and what it is not

### What it is

One Samba Active Directory **domain controller**: the directory over LDAP
and LDAPS, the Kerberos KDC (Samba's bundled Heimdal), DNS for the realm,
the `sysvol` and `netlogon` shares, signed NTP for domain members, and DRS
replication with other DCs of the same domain. The ports it declares are
the ones that role serves — 53, 88, 123/udp, 135, 137-139, 389, 445, 464,
636, 3268, 3269.

That is the whole of it. Everything below is a role this image
deliberately does not fill, each with the reason and with what to run
instead.

### It is not a file server

The Samba Team's own guidance is that a DC should not be used as a file
server. Their wiki lists the operational reasons, and two of them are
decisive for anything a school would actually deploy: a DC should only
serve files if it is the *only* Samba instance in the domain — so the
moment you add a second DC the option is gone — and using POSIX ACLs with
shares on a Samba DC, in the wiki's words, "does not work". Their
recommendation is to set up a Samba **domain member** with the file shares
([wiki.samba.org, *Setting up Samba as an Active Directory Domain
Controller*, section "Using the Domain Controller as a File Server"](https://wiki.samba.org/index.php/Setting_up_Samba_as_an_Active_Directory_Domain_Controller#Using_the_Domain_Controller_as_a_File_Server_%28Optional%29)).

This image follows that: it serves `sysvol` and `netlogon` — which an AD
DC must serve — and defines no other share. Measured on a provisioned DC
(2026-09-17, `samba-ad-dc:dev`), `smbclient -L` lists exactly `sysvol`,
`netlogon` and `IPC$`.

`SAMBA_GLOBAL_OPTIONS` cannot add one, and the construction that stops it
is a refusal rather than an accident of formatting: the variable writes
each setting as an indented `key = value` line inside `[global]`, and it
refuses at load time (exit 10) any parameter name outside the charset a
real smb.conf parameter uses — letters, digits, spaces and `: * . _ -`, as
in `idmap config * : backend`. A name carrying a bracket is what would
otherwise escape: samba's ini parser reads any line whose first non-blank
character is `[` as a new **section** and discards the rest of the line, so
`[myshare] path = /tmp` would have opened a share (measured, 2026-09-17:
`testparm -s` reported `myshare` as a service with no path, and exited 0 —
so nothing downstream would have refused it either). Adding a share to a DC
therefore means hand-editing `smb.conf` on the configuration volume —
against the advice of the people who wrote Samba, on the one host in the
domain whose failure takes authentication down with it. **Run a
domain-member file server instead**, as its own container or host, joined
to this domain.

### It is not a print server

Printing is compiled out, not merely unconfigured: the build passes
`--disable-cups --disable-iprint` (Dockerfile), and the runtime image
carries no CUPS administration binary (measured: `command -v lpadmin`
finds nothing). A print server is a separate role on a domain member.

### It is not a DHCP server

No DHCP daemon is installed (measured: `command -v dhcpd` finds nothing),
and none is planned — SPEC §5.1 image minimality means a DC image ships
what a DC needs. Run DHCP where you already run it, and point its clients'
DNS at the DC (§8 and
[`docs/windows-client-validation.md`](windows-client-validation.md)).

### It does not replicate sysvol between DCs

Samba implements no DFS-R, so with more than one DC the *directory*
replicates but **group policy content does not**. A GPO edited on one DC
is invisible to members that authenticate against another. This is a
stated limitation (profile
[B.6](adaptation-profile.md#b6-known-limitations-stated-per-124), SPEC
§12.4); the manual procedure, itself a sketch and covered by no test, is
[deployment §8](deployment-guide.md#8-sysvol-replication-is-manual). An
integrated, tested mechanism is committed roadmap for v2.

### A real Windows client has never joined it in CI

There are no Windows runners in the public pipeline, so the join is proven
at protocol level (Kerberos, Kerberized SMB, NTLM, DNS SRV, LDAPS) and not
with a real client. That is B.6, and it is the reason
[`docs/windows-client-validation.md`](windows-client-validation.md)
exists: a downstream project validates it once, on its own hardware, and
records the result.

### The smaller boundaries

- **SambaGPG** (`password hash gpg key ids`) is unavailable: the build
  passes `--without-gpgme` to keep the GnuPG suite — including the
  network-capable `dirmngr` — out of the image closure (B.6).
- **SMB-over-QUIC** is compiled in because Samba 4.24 offers no switch to
  remove it, and is left unconfigured; 443/udp is not exposed (B.6).
- **No `nsupdate`**: dynamic DNS goes through `samba_dnsupdate
  --use-samba-tool`, which the entrypoint pins. A configuration that needs
  the external updater is unsupported (B.6).
- **No `rsync`** (measured), which is why the sysvol procedure above runs
  its copy from a helper container rather than from the DC.

### What a full "Windows server replacement" therefore looks like

| Role | Where it runs |
|---|---|
| Domain controller — directory, Kerberos, DNS, sysvol/netlogon, time | **this image** |
| File shares, with working Windows ACLs | a Samba **domain member**, separate image or host |
| Print server | a domain member |
| DHCP | wherever you run DHCP today; its clients' DNS must point at a DC |
| GPO editing | a Windows workstation with RSAT, against this DC |

Each companion joins the domain this image serves. None of them belongs in
this repository — the image stays the general base, and a downstream
project composes the rest around it.

---

<a id="deriving-an-image"></a>

## 2. Deriving an image

```dockerfile
FROM ghcr.io/esitc-paris/samba-ad-dc:4.24.7-r1

COPY school-defaults.conf /usr/share/school-domain/
ENV SCHOOL_SITE=paris
```

Pin an immutable `X.Y.Z-rN` tag or a digest, not a branch tag and never
`latest` — a derived image inherits the base's *content*, so an unpinned
base makes your build unreproducible
([update guide §1](update-guide.md#1-pinning)).

The tag above is an example of the *shape*, not a tag you can pull today:
no image has been published yet (see the Status block at the top), the
first one will carry whatever Samba version the watcher publishes, and its
revision may well be `-r2` rather than `-r1` — a rebuild of the same Samba
version bumps `rN` without changing `X.Y.Z`.

### What you inherit, without writing a line of it

`TestDerivedImageInheritsContract` builds the smallest possible derived
image — one `COPY`, one `ENV` — provisions a real domain on it and asserts
the following, so this list is measured rather than promised:

| Inherited | Value |
|---|---|
| `ENTRYPOINT` | `["/usr/bin/tini","--","/usr/local/bin/entrypoint"]` |
| `HEALTHCHECK` | `["CMD","/usr/local/bin/entrypoint","healthcheck"]` |
| `VOLUME` | `{"/etc/samba":{},"/var/lib/samba":{}}` — both, and only both |
| `ENV` | `KRB5_CONFIG=/var/lib/samba/private/krb5.conf` |
| Labels | `org.opencontainers.image.version` and `org.esitc-paris.spec-version`, equal to the base's |
| Behaviour | the image's own health verdict, the same `entrypoint --version`, and the refusals — `SAMBA_MODE=run` on a volume holding no domain still exits **21** |

`EXPOSE` is inherited the same way; it is documentation rather than
behaviour, and the test does not assert it.

The last row is the one worth reading twice. A derived image that declares
everything correctly can still ship an entrypoint that behaves
differently; the exit code is what notices.

### What a derived image must not change

- **The entrypoint.** tini is PID 1 and the entrypoint is its child. An
  `ENTRYPOINT` of your own stops zombie reaping, or SIGTERM handling, or
  both — and the container still starts, still serves the domain, and has
  quietly stopped being something docker can supervise.
- **The volumes.** Adding a `VOLUME` line does not replace the set, it
  extends it; losing one gives an operator who forgot a `-v` an anonymous
  volume where the directory database should be.
- **The `SAMBA_*` contract.** Baking a `SAMBA_MODE` or a
  `SAMBA_ADMIN_PASSWORD_FILE` into the image with `ENV` is legal and is
  usually a mistake: it turns a deployment-time decision into a
  build-time one, and `SAMBA_MODE=provision` left in an image is how a
  volume that failed to mount becomes a brand new empty domain. Exit 20
  and exit 21 exist to catch that; do not spend them.
- **The rootfs at runtime.** A derived image may write anywhere it likes
  at **build** time; at **run** time the documented profile is
  `--read-only` with a fixed tmpfs set (deployment
  [§1.8](deployment-guide.md#18-the-hardened-profile)). Content your
  derivation adds belongs in the image, not in a directory it expects to
  create on first boot.

### What a derived image must re-declare

- **`org.opencontainers.image.base.name` and `.base.digest`.** They are
  inherited verbatim, and they describe *this image's* base — the Debian
  image it was built from — not this image. Left alone they are simply
  false about your build. Set them to the samba-ad-dc tag and digest you
  built `FROM`.
- **`org.opencontainers.image.source`, `.version`, `.revision`,
  `.created`.** Inherited unchanged, they say your image is this Samba
  version, built from this repository, at this commit. If your consumers
  read those labels — and the point of publishing them is that they do —
  re-declare them for your project and keep the base's Samba version
  somewhere of your own.

### Adding packages is your §5.1 problem, not this image's

SPEC §5.1 minimality is a property each image owns. Every package a
derived build installs is one its project justifies, monitors and
CVE-tracks: this repository's SBOM, its CVE ledger and its watcher all
describe the base image, and none of them will ever see your layer. The
same goes for the base's own guarantees — signature, provenance
attestation, reproducible inputs — which cover what you built *from*, not
what you built.

(*Covered by:* `TestDerivedImageInheritsContract`.)

---

<a id="declarative-configuration"></a>

## 3. Declarative configuration: `SAMBA_GLOBAL_OPTIONS`

Everything in `[global]` that no other variable owns goes into this one
variable, one `key = value` per line, reconciled into `smb.conf` on every
start before any daemon reads it. The full rules are
[deployment §3.5](deployment-guide.md#35-declarative-global-settings);
what a downstream project needs to know to design around it is here.

**Three worked examples.**

*A multi-homed host.* A DC with more than one address should answer on the
one the realm's DNS hands out, and only there:

```yaml
      SAMBA_GLOBAL_OPTIONS: |
        interfaces = 192.0.2.10 127.0.0.1
        bind interfaces only = yes
```

Keep the loopback in the list: the image's own health check probes DNS,
LDAP and SMB on `127.0.0.1`
([deployment §6.1](deployment-guide.md#61-health)), so a DC bound away
from loopback is reported unhealthy while it serves members perfectly.

*Require SMB encryption.*

```yaml
      SAMBA_GLOBAL_OPTIONS: |
        smb encrypt = required
```

This one changes how the DC answers the health check's **anonymous** SMB
probe, which is exactly the class of setting §3.5 warns about. Change it
alone, and watch the container's health verdict after the restart —
`docker inspect -f '{{.State.Health.Status}}' dc1` — before you change
anything else.

*Pin LDAP strong auth rather than inherit it.*

```yaml
      SAMBA_GLOBAL_OPTIONS: |
        ldap server require strong auth = yes
```

Measured against this image (`testparm` on a DC-role configuration,
2026-09-17): the parameter is already `Yes` by default, so this entry
changes nothing today. It is worth writing anyway if your project's policy
is that security-relevant values are *stated* rather than inherited from
whatever the next Samba release defaults to — which is the honest reason
to write it, and the only one.

**Four rules that shape a downstream design.**

1. **Keys another variable owns are refused** with exit 10, naming the
   owner: `realm`, `workgroup`, `netbios name`, `ad dc functional level`,
   `dns forwarder`, the three `tls *` files (§4), and `server role`, `dns
   update command`, `ntp signd socket directory` and `include`, which the
   image manages itself. Design your configuration around the variables,
   not around the file.
2. **Samba's own parser is the gate, on every path.** On a restart the
   rewrite is checked with `testparm`, and a rejected one is rolled back to
   the previous bytes before the boot refuses with exit 10, quoting
   testparm. On the **first** boot — a `provision` or a `join` — the same
   settings are checked *before* samba-tool is asked to create anything, so
   a bad entry refuses with exit 10 and an empty volume rather than with a
   half-created domain; the configuration samba-tool then generates is put
   through the same gate before any daemon starts. A configuration
   generator in a downstream project therefore fails *loudly and
   recoverably* at every point, which is what makes generating this
   variable safe.
3. **A deprecation warning is not a rejection.** A parameter Samba still
   accepts but has deprecated makes testparm print a `WARNING` and load
   the file anyway; the entrypoint copies that line to the container log
   and carries on. Watch your logs for it across a branch upgrade — it is
   the notice that a later Samba may remove the setting you depend on.
4. **Removing an entry does not remove the setting.** The reconciliation
   adds and replaces; it cannot tell a line it wrote last boot from one
   you put in `smb.conf` yourself. To undo a setting, write the value you
   want explicitly. The only exception is the three `tls *` keys of §4,
   which the image owns and therefore removes.
5. **Never put a secret in it.** Every setting this variable applies is
   echoed into the container log with its value — that is how an operator
   finds out which line changed their DC — and the log outlives the
   container. Secrets reach this image as **files** (§6.1), never as
   environment values, and a `[global]` parameter whose value is a
   password does not belong here.

A parameter name is matched the way samba matches it, ignoring case **and**
all whitespace: `maxlogsize`, `Max Log Size` and `max  log  size` are one
setting, both for the owned-key refusals of rule 1 and for recognising the
line already in `smb.conf`.

(*Covered by:* `TestGlobalOptionsApplied`.)

---

<a id="bring-your-own-tls"></a>

## 4. Bring your own TLS material

Unset, the DC serves LDAPS with the certificate Samba generates for
itself. Set, the three variables name PEM files **inside the container**:

```yaml
      SAMBA_TLS_CERT_FILE: /run/secrets/tls/cert.pem
      SAMBA_TLS_KEY_FILE: /run/secrets/tls/key.pem
      SAMBA_TLS_CA_FILE: /run/secrets/tls/ca.pem
```

What a downstream project has to design around:

- **Absolute paths only**, and **all three or none** — either is exit 10.
- **The private key must be mode `0600` and owned by uid 0**; the
  certificate and the CA are public material and are not mode-checked
  (`0644` is the usual choice). This is Samba's rule, it is fatal, and the
  entrypoint refuses with exit 11 rather than letting the DC die twenty
  lines into Samba's own output.
- **Deliver it declaratively where you can.** Swarm `secrets:` long syntax
  sets `uid`/`gid`/`mode` on the materialised file, and a Kubernetes
  `secret` volume sets `defaultMode` with a per-item `mode`; only a plain
  bind mount forces a root-owned `0600` file on the host. The three forms
  are written out in
  [deployment §4.5](deployment-guide.md#bring-your-own-certificate).
- **The certificate must carry the DC's FQDN** in its subject alternative
  name — the name the realm's DNS hands out, not the container's short
  name.
- **It is reversible.** Unset the three and recreate the container: the
  entrypoint takes the three `tls *` lines back out of `smb.conf` (one log
  line each) and Samba autogenerates its own self-signed material on the
  next start, at `0600`, even on a DC that never had any. Measured, and
  the reason this is an extension point rather than a one-way door.
- **Renewal is a restart.** Replace the files at the same paths and
  recreate the container; nothing reloads a certificate in place and
  nothing watches its expiry. Expiry monitoring is yours.

(*Covered by:* `TestCustomTLSMaterial`.)

---

<a id="operator-api"></a>

## 5. The operator API

> **Out of band — not covered by this image's suite.** Everything in this
> section is *how a downstream project drives the image from outside*.
> This repository tests the image; a downstream project tests its own
> automation. The commands below were each run by hand once, on
> 2026-09-17, against `samba-ad-dc:dev` (Samba 4.24.7, arm64) with a
> single-DC domain `AD.EXAMPLE.COM` on a docker bridge network, and the
> results are reported as measurements of that one run — not as a
> contract, and not as anything CI will notice breaking.

There is no hook mechanism and there will not be one: SPEC §6.7 keeps the
entrypoint's shell-outs to Samba's own CLIs, so a downstream project's
automation runs **beside** the container, not inside its startup path.
Two shapes, both using only `samba-tool`:

### Form A — `docker exec`, for a human at a terminal

```sh
docker exec dc1 samba-tool user create alice --random-password
docker exec dc1 samba-tool group add teachers
docker exec dc1 samba-tool group addmembers teachers alice
docker exec dc1 samba-tool ou create 'OU=Staff,DC=ad,DC=example,DC=com'
docker exec dc1 samba-tool domain passwordsettings set --min-pwd-length=12
```

All five were measured to succeed against the healthy, running DC.
`samba-tool` talks to the local `sam.ldb` directly, so no credentials are
needed inside the container, and `KRB5_CONFIG` is set in the image so that
an exec'd command inherits the realm's Kerberos configuration rather than
rediscovering it over DNS.

**`samba-tool gpo …` is the exception, and the reason is DNS.** Every
`gpo` subcommand first locates a DC over CLDAP using the *realm* name. A
container whose `/etc/resolv.conf` points at docker's embedded resolver —
the default — cannot resolve it, and the command dies before it starts:

```text
ERROR(runtime): uncaught exception - ('Could not find a DC for domain',
  NTSTATUSError(3221225524, 'The object name is not found.'))
```

Measured, on a DC that was healthy and serving the domain at that very
moment. Whether `docker exec … samba-tool gpo` works therefore depends
entirely on what that container resolves through; form B does not. The
measurement and its bounds are recorded in profile
[B.6](adaptation-profile.md#b6-known-limitations-stated-per-124).

### Form B — a one-off container of the same image, for automation

The image's entrypoint refuses an argv it does not understand — measured:

```text
$ docker run --rm samba-ad-dc:dev samba-tool user list
ERROR: the entrypoint was called with the argument(s) ["samba-tool" "user"
"list"], which it does not understand; run the image with no arguments to
start the domain controller, or with the single argument "healthcheck" to
probe a running one            # exit 10
```

So an automation container **overrides the entrypoint**. This is the same
shape the backup runbook already uses
([deployment §7.1](deployment-guide.md#71-take-the-backup)):

```sh
docker run --rm --network dc_net --dns 192.0.2.10 \
  -e PASSWD_FILE=/run/secrets/admin_password \
  -v "$PWD/secrets/admin_password:/run/secrets/admin_password:ro" \
  -v dc1-conf:/etc/samba:ro \
  --read-only --tmpfs /run --tmpfs /tmp --tmpfs /var/cache/samba \
  --tmpfs /run/lock \
  --cap-drop ALL --security-opt no-new-privileges:true \
  --entrypoint samba-tool \
  ghcr.io/esitc-paris/samba-ad-dc:4.24.7-r1 \
  user create alice --random-password \
    -H ldap://dc1.ad.example.com -U Administrator
```

That command was run as written — with the constrained flags, which a
one-off does not get to skip — and it succeeded. It holds **no
capabilities at all**: `samba-tool` speaking LDAP over the network needs
none, unlike the DC itself. Five parts are load-bearing, each measured:

1. **`--dns <a DC's address>`.** The one-off has to resolve the realm, for
   the reason form A just failed on.
2. **`-H ldap://<the DC's FQDN>`** for anything that writes to the
   directory. Without it `samba-tool` opens the *local* `sam.ldb`, which a
   one-off does not have: `Unable to open tdb
   '/var/lib/samba/private/sam.ldb': No such file or directory`. That
   failure is also the safety property — an automation container that
   mounts no state volume cannot touch a DC's database by accident, and it
   should not mount one.
3. **`-v dc1-conf:/etc/samba:ro`** for the `gpo` subcommands. Without the
   DC's `smb.conf`, `gpo create` was measured to fail at its SMB step —
   `ERROR: Error connecting to 'dc1.ad.example.com' using SMB` — even with
   realm DNS and `-H`; with the configuration volume mounted read-only,
   `gpo create`, `gpo setlink` and `gpo listall` all succeeded, `create`
   returning its new GUID and `setlink` attaching it to an OU. Why exactly
   the file is needed was not chased further: `gpo` writes the policy
   directory into sysvol over SMB, and what it builds that connection from
   is that file. Mounting the DC's configuration volume read-only is safe;
   mounting its **state** volume while the DC runs is not the documented
   pattern and nothing here tests it.
4. **`--tmpfs /run/lock`.** Without it, and on a read-only rootfs, the
   `gpo` subcommands print `directory_create_or_exist: mkdir failed on
   directory /var/lock/samba: No such file or directory` three times and
   then work anyway (`/var/lock` is a symlink to `/run/lock`, which a
   fresh `/run` tmpfs does not contain). The mount costs nothing and
   removes three lines that look like a failure and are not.
5. **`PASSWD_FILE`.** Samba's credentials library reads the password from
   the file this variable names, so the secret stays a file — the same
   rule as `SAMBA_ADMIN_PASSWORD_FILE`
   ([deployment §1.5](deployment-guide.md#15-secrets-files-only)).
   Measured to work. Never `-U 'Administrator%password'`: argv is visible
   to every process on the host. `--random-password` keeps the *new*
   account's password out of argv the same way — and does not print it
   (measured: the output is `User 'alice' added successfully` and nothing
   else), so the account needs `samba-tool user setpassword`, which
   prompts, or a `--must-change-at-next-login` flow before anyone can use
   it.

### Which operations need the DC stopped

| Operation | DC |
|---|---|
| users, groups, OUs, computers, password policy, GPOs, DNS records, SPNs, delegation | **running** (and healthy) |
| `samba-tool domain backup offline` | **stopped** — [deployment §7.1](deployment-guide.md#71-take-the-backup) |
| `samba-tool domain backup restore` | **stopped**, into fresh volumes — [deployment §7.3](deployment-guide.md#73-restore-into-a-fresh-instance) |
| `dbcheck` in maintenance mode (`SAMBA_MODE=maintenance`) | **stopped** — [deployment §6.4](deployment-guide.md#64-database-consistency) |

A read-only `docker exec dc1 samba-tool dbcheck` on the running DC is the
exception in the last row: it is documented, and it is not the invasive
form.

### The commands exist; the automation is yours

Verified present in this image's `samba-tool` (4.24.7) by running each
with `--help`: `user create`, `user setpassword`, `user list`,
`user delete`, `group add`, `group addmembers`, `ou create`,
`domain passwordsettings set`, `gpo create`, `gpo setlink`, `gpo list`,
`dns add`, `computer create`, `delegation`, `spn`, `dsacl`.

**Bulk import** is a loop, not a feature: read a CSV in the one-off
container of form B, call `samba-tool user create … -H ldap://…` per row,
and make the loop idempotent — `user create` fails on an account that
already exists, which is correct and which your script has to expect.
**GPO *content*** — the policy settings inside a GPO — is edited with
RSAT from a Windows workstation; `samba-tool gpo` creates, links and lists
them.

None of this is covered by a test in this repository. A downstream project
that automates any of it owns the test that proves its automation still
works after an upgrade.

---

<a id="a-compose-pattern"></a>

## 6. A compose pattern for a downstream project

> **Out of band**, for the same reason as §5: what this shows is a
> downstream project's own composition. Nothing here is exercised by this
> repository's suite.

```yaml
services:
  dc1:
    image: ghcr.io/esitc-paris/samba-ad-dc:4.24.7-r1
    container_name: dc1
    hostname: dc1
    environment:
      # FIRST BOOT ONLY. `provision` creates the domain; once it exists,
      # change this to `run` and recreate the container. The two failures
      # this avoids are both loud and both one-line: `run` on empty
      # volumes exits 21 (no state to start), and `provision` left here
      # exits 20 on every later start (the volume is already
      # initialized) — deployment guide §3.4.
      SAMBA_MODE: provision
      SAMBA_REALM: AD.EXAMPLE.COM
      SAMBA_DOMAIN: AD
      SAMBA_DNS_FORWARDER: 192.0.2.53
      SAMBA_ADMIN_PASSWORD_FILE: /run/secrets/admin_password
    secrets: [admin_password]
    volumes:
      - dc1-state:/var/lib/samba
      - dc1-conf:/etc/samba
    networks:
      dc_net:
        ipv4_address: 192.0.2.10
    # the hardened profile — deployment guide §1.8
    read_only: true
    tmpfs: [/run, /tmp, /var/cache/samba]
    cap_drop: [ALL]
    cap_add: [SYS_ADMIN, NET_BIND_SERVICE, CHOWN, FOWNER, SETUID, SETGID]
    security_opt: ["no-new-privileges:true"]
    restart: unless-stopped

  # The project's own automation. Same image, different entrypoint.
  init:
    image: ghcr.io/esitc-paris/samba-ad-dc:4.24.7-r1
    # REQUIRED: the image's entrypoint refuses an argv it does not
    # understand (exit 10). An automation container overrides it.
    entrypoint: ["/bin/sh", "/srv/seed.sh"]
    depends_on:
      dc1:
        condition: service_healthy
    environment:
      # The password stays a file; samba-tool reads it from this path.
      PASSWD_FILE: /run/secrets/admin_password
      DC: dc1.ad.example.com
    secrets: [admin_password]
    volumes:
      - ./seed.sh:/srv/seed.sh:ro
      - ./users.csv:/srv/users.csv:ro
      - dc1-conf:/etc/samba:ro          # only needed by `samba-tool gpo`
    networks: [dc_net]
    dns: [192.0.2.10]                    # resolve the realm through the DC
    read_only: true
    # The same writable set the measured command in §5 carries — including
    # /run/lock, which is what keeps `samba-tool gpo` quiet on a read-only
    # rootfs.
    tmpfs: [/run, /tmp, /var/cache/samba, /run/lock]
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    restart: "no"                        # a one-off, not a service

secrets:
  admin_password:
    file: ./secrets/admin_password

volumes:
  dc1-state:
  dc1-conf:

networks:
  dc_net:
    driver: macvlan
    driver_opts: {parent: eth0}
    ipam:
      config:
        - subnet: 192.0.2.0/24
          gateway: 192.0.2.1
```

The `init` service waits on `service_healthy`, so on that first
`docker compose up` it runs against the domain `dc1` has just provisioned.
Switch `SAMBA_MODE` to `run` before the next one: nothing re-runs the
provision, but leaving it there costs every later start an exit 20.

`seed.sh` is the project's, not this image's:

```sh
#!/bin/sh
set -eu
while IFS=, read -r user given surname; do
  samba-tool user create "$user" --random-password \
    --given-name="$given" --surname="$surname" \
    -H "ldap://$DC" -U Administrator \
    || echo "skipping $user"      # already exists: expected on a re-run
done < /srv/users.csv
```

Four things about this pattern are worth stating plainly:

- **`condition: service_healthy` waits for the image's own health check**,
  which is the right signal: it probes DNS, LDAP and SMB rather than
  merely observing that the process started. The DC's `start_period` is
  180 s because a first-boot provision on a cold volume legitimately takes
  minutes; compose waits it out.
- **`restart: "no"` makes it a one-off.** It runs again on every
  `docker compose up`, so the script has to be idempotent — the `||` above
  is the whole of it, and a real project should check before it creates
  rather than swallowing every error.
- **Secrets are files, everywhere.** `SAMBA_ADMIN_PASSWORD_FILE` for the
  DC, `PASSWD_FILE` for `samba-tool`. A plain `SAMBA_ADMIN_PASSWORD` is
  refused with exit 10, and a password in argv is visible to the whole
  host.
- **The init service is not a sidecar.** It holds no capabilities, mounts
  no state volume, and is gone the moment its script exits. If it needs
  the DC's `sam.ldb`, the design is wrong: it should be talking to
  `ldap://` instead.

---

<a id="more-than-one-dc"></a>

## 7. More than one domain controller

A second DC is `SAMBA_MODE: join` with `SAMBA_JOIN_PASSWORD_FILE`, on its
own volumes, resolving through the first DC — the service definition,
what the join writes into `smb.conf`, and the both-directions replication
check are
[deployment §5](deployment-guide.md#5-scale-out-an-additional-domain-controller).
Three things decide whether it works at all:

- **Every DC resolves through a DC**, because a replication partner is
  addressed by a `<objectGUID>._msdcs.<realm>` CNAME only the directory's
  own DNS answers.
- **And that DNS needs an upstream.** Samba's internal DNS with no
  forwarder takes 4-8 seconds to fail a name it does not serve (measured),
  which is long enough to time out the Kerberos-sealed bind that carries
  replication. `SAMBA_DNS_FORWARDER` is not optional here (B.3).
- **Sysvol does not replicate** (§1). Until the v2 mechanism lands, the
  manual copy in [deployment §8](deployment-guide.md#8-sysvol-replication-is-manual)
  is operator-owned, and a downstream project that relies on GPOs should
  plan for it — or keep a single DC, which is also what the Samba wiki
  suggests for anyone who insists on serving files from one.

Upgrade order across several DCs, and the rule that a rollback is a
restore rather than a tag downgrade, are
[update guide §4](update-guide.md#4-multi-instance-rollout-order) and
[§7](update-guide.md#7-rollback).

(*Covered by:* `TestJoinReplicationBothWays`.)

---

<a id="windows-clients"></a>

## 8. Windows clients

Nothing in this project's CI joins a real Windows client (B.6). The
protocol equivalents are tested — `TestKerberosKinit`,
`TestKerberizedSMB`, `TestNTLMAuth`, `TestDNSSRVRecords`,
`TestLDAPSCertificate` — and a real client is validated out of band, once,
by the project that deploys the domain.

The procedure, the checklist and where the result is recorded are
[`docs/windows-client-validation.md`](windows-client-validation.md).

---

## Section → test map (§10.6)

| Section | E2E test |
|---|---|
| §1 What this image is, and what it is not | *(none — boundaries and stated limitations, B.6 / SPEC §12.4)* |
| §2 Deriving an image | `TestDerivedImageInheritsContract` |
| §3 Declarative configuration | `TestGlobalOptionsApplied` |
| §4 Bring your own TLS material | `TestCustomTLSMaterial` |
| §5 The operator API | *(none — out of band; a downstream project tests its own automation)* |
| §6 A compose pattern | *(none — out of band; a downstream project's own composition)* |
| §7 More than one domain controller | `TestJoinReplicationBothWays` |
| §8 Windows clients | *(none — B.6; out-of-band validation, see the procedure)* |
