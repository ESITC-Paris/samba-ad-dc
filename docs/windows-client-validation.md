# Windows-client validation (out of band)

<!-- GO-LIVE: delete this comment and the blockquote below at the same time as README's Status block, once the first release is published (docs/operations.md, "Going live: the first publication", step 7). -->
> **Status: pre-release.** No image has been published yet, so the
> procedure below has to be run against an image you built yourself (or
> against the first published tag, once it exists); every command here
> naming a published tag will fail until then.

> **Not executed by this project's CI.** A real Windows-client domain join
> is a stated limitation — profile
> [B.6](adaptation-profile.md#b6-known-limitations-stated-per-124), SPEC
> §12.4 — because the public pipeline has no Windows runners. What CI
> proves is the protocol layer a Windows client uses: DNS SRV records
> (`TestDNSSRVRecords`), Kerberos (`TestKerberosKinit`), Kerberized SMB
> (`TestKerberizedSMB`), NTLM (`TestNTLMAuth`) and LDAPS
> (`TestLDAPSCertificate`). This file is the procedure for closing the
> remaining gap **by hand**, once, on the hardware a downstream project
> actually deploys — and for recording the result where it can be cited.

Nothing below is automated, and nothing below is a promise about a
particular Windows build. It is a checklist: run it, write down what
happened, including what did not work.

---

## 0. Before you start

| Prerequisite | Why | How to check on the client |
|---|---|---|
| The client's **only** DNS server is a DC of this domain | The join finds the DC through the realm's SRV records, which only the domain's DNS answers | `Get-DnsClientServerAddress` |
| Clock skew under **5 minutes** against the DC | Kerberos rejects a larger skew outright; the DC serves signed NTP ([deployment §4.6](deployment-guide.md#46-time)) | `w32tm /stripchart /computer:<dc-fqdn> /samples:3 /dataonly` |
| The DC's **FQDN** resolves to its address, and back | Kerberos and the LDAPS certificate are both about the name, not the address | `nslookup dc1.ad.example.com` and `nslookup <ip>` |
| The DC container is **healthy** | A DC that is up is not necessarily serving | `docker inspect -f '{{.State.Health.Status}}' dc1` |
| You have the domain Administrator password | The join needs it | — |

Record the Windows edition and build (`winver`, or `Get-ComputerInfo |
Select OsName, OsVersion, WindowsVersion`) before anything else: every
observation below is about *that* build.

---

## 1. Find the domain controller

```text
nltest /dsgetdc:ad.example.com
```

Expect the DC's name and address, and flags including `DS`, `LDAP`,
`KDC`, `TIMESERV` (or `GTIMESERV`), `WRITABLE` and `CLOSE_SITE`. Record
the whole block. A failure here is a DNS problem, not a join problem, and
it is where most first attempts stop.

## 2. Join the domain

*System → About → Rename this PC (advanced) → Change → Domain*, or:

```text
Add-Computer -DomainName ad.example.com -Credential ad\Administrator -Restart
```

Expect the welcome dialog, then a reboot. Record how long it took and any
warning shown.

On the DC afterwards:

```sh
docker exec dc1 samba-tool computer list
```

The client's machine account must be listed.

## 3. First domain logon

Log on as a domain user created for the test:

```sh
docker exec dc1 samba-tool user create testuser --random-password
```

(Set a known password with `samba-tool user setpassword testuser` — it
prompts, so the password does not go into a shell history or into argv.)

After logon, on the client:

```text
klist
```

Expect a TGT (`krbtgt/AD.EXAMPLE.COM`) and a service ticket for the DC's
`cifs/` SPN once the profile has loaded. Record the ticket list, the
encryption types, and whether the logon was Kerberos or fell back to NTLM
— `klist` empty after a successful logon is the signal that it fell back.

## 4. Group policy

Create and link a test GPO **from a one-off container**, not with
`docker exec`: every `gpo` subcommand locates a DC over CLDAP by realm
name, which the DC container's own resolver does not answer (profile
[B.6](adaptation-profile.md#b6-known-limitations-stated-per-124), and
[reuse guide §5](reuse-guide.md#operator-api) for the form that works).
The command below is that form, as measured:

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
  gpo create 'Validation Test Policy' -U Administrator
```

It prints the new GUID. Run the same container again with
`gpo setlink 'DC=ad,DC=example,DC=com' '{<that GUID>}' -U Administrator`
as its trailing arguments to link the policy at the domain root.

On the client:

```text
gpupdate /force
gpresult /r
```

Expect the policy to appear under *Applied Group Policy Objects*. Record
the output of `gpresult /r` in full.

**Edit the policy's *contents* from RSAT**, not from `samba-tool`:
`samba-tool gpo` creates, links and lists GPOs; the settings inside them
are a Windows-side job (Group Policy Management Console). Validating one
real setting — a drive mapping, a desktop restriction — is worth more than
validating an empty GPO, so do that and say which setting you used.

## 5. Password change from the client

Ctrl+Alt+Del → *Change a password*, as the test user.

This exercises **kpasswd** on port 464 (TCP and UDP), which the image
exposes. Record whether it succeeded and how long it took. Then verify
from the DC's side that the new password authenticates:

```sh
docker exec dc1 kinit testuser@AD.EXAMPLE.COM   # prompts
```

If the domain has a password policy set with `samba-tool domain
passwordsettings set`, try a password that *violates* it as well, and
record the message Windows shows: a policy that silently accepts anything
is worth catching here.

## 6. SMB signing and encryption

Access the DC's `sysvol` share from the client:

```text
net use \\dc1.ad.example.com\sysvol
Get-SmbConnection | Format-List ServerName, ShareName, Dialect, Signed, Encrypted
```

Record `Dialect`, `Signed` and `Encrypted` as observed. Two notes, both of
which are reasons to *record* rather than to expect:

- **Windows 11 24H2 changed the client-side default**: Microsoft
  documents that the SMB client requires signing on all connections by
  default from that build. A share that mounts is evidence the DC met it;
  a refusal is evidence of the opposite, and either is worth writing down.
- **This project has not measured what the DC negotiates.** `testparm`
  reports `server signing = default` on a DC-role configuration, i.e. the
  effective value comes from Samba's role default rather than from
  anything this image sets. The Samba wiki states that mandatory SMB
  signing is enforced on a DC — listed there among the reasons not to use
  one as a file server ([same page as the reuse guide
  §1](reuse-guide.md#what-this-image-is-and-is-not) cites) — which is
  upstream's statement, not a measurement taken here. If your deployment
  needs a stated value, set it explicitly through `SAMBA_GLOBAL_OPTIONS`
  ([reuse guide §3](reuse-guide.md#declarative-configuration)) and
  re-validate.

## 7. Anything your deployment actually depends on

The six steps above are the generic floor. A school domain replacing a
Windows server will care about at least: roaming profiles or folder
redirection (which need a **domain-member file server** — not the DC, see
[reuse guide §1](reuse-guide.md#what-this-image-is-and-is-not)), printer
deployment, and the second DC's behaviour when the first is down. Add
them to your own copy of this checklist and record them the same way.

---

## What to record, and where

Write one file per validation run:

```text
docs/validations/<YYYY-MM-DD>-<client>.md
```

for example `docs/validations/2026-10-02-win11-24h2.md`. The format and
the required fields are in
[`docs/validations/README.md`](validations/README.md). The essentials: the
image tag **and digest**, the Samba version, the Windows edition and
build, one line per step above with PASS / FAIL / NOT RUN and the actual
output, and — the field people skip — what *did not* work.

A run that found nothing is still worth a file. "Validated, no findings"
is a fact about a version pair; silence is not.

## How a result feeds back into the project

- **A clean run** does not remove the B.6 limitation. CI still has no
  Windows runner, so the statement "not exercised in CI" stays true. What
  changes is that B.6's Windows-client bullet can name the validation file
  and its date: the limitation remains, with evidence beside it that a
  real client worked once, on a stated build, against a stated image
  digest.
- **A failure** is a finding, and it belongs in the open rather than in
  the validator's head. If the cause is in this image, it is an issue
  against this repository, and the validation file is its evidence. If the
  cause is upstream Samba's or Microsoft's, it becomes a new B.6
  limitation with the measurement quoted — which is how every other entry
  in B.6 got there.
- **Either way the file is immutable.** A later run gets a later file; a
  result is a measurement of one day, one build and one digest, and
  editing it in place destroys exactly the thing it was written for.
