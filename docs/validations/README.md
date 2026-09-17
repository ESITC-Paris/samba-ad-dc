# Validation records

One file per out-of-band validation run — a procedure a human executed on
real hardware that this project's CI cannot execute itself. Today there is
exactly one such procedure,
[`docs/windows-client-validation.md`](../windows-client-validation.md),
and it exists because "a real Windows client joins the domain" is a stated
limitation (profile
[B.6](../adaptation-profile.md#b6-known-limitations-stated-per-124), SPEC
§12.4) rather than a test.

A record here is **evidence, not documentation**. It says what one person
observed, on one day, against one image digest. It is never edited
afterwards to reflect a later run: a second run gets a second file.

## File name

```text
docs/validations/<YYYY-MM-DD>-<client>.md
```

`<client>` is a short slug for what was validated — `win11-24h2`,
`win10-22h2`, `server2022`. The date is the day the run happened, not the
day it was written up.

## Required fields

Copy this skeleton. Leave nothing as a placeholder: a field with no answer
is `NOT RUN` or `UNKNOWN`, which is information, while an unfilled
template is not.

```markdown
# Windows-client validation — <client>, <YYYY-MM-DD>

| | |
|---|---|
| Procedure | docs/windows-client-validation.md (revision <git sha>) |
| Image tag | ghcr.io/esitc-paris/samba-ad-dc:<X.Y.Z-rN> |
| Image digest | sha256:… |
| Samba version | <from the image's org.opencontainers.image.version label> |
| Realm / domain | <REALM> / <NETBIOS> |
| Topology | single DC | two DCs | … , macvlan | host networking |
| Client | <edition>, build <winver output> |
| Validated by | <name> |
| Verdict | PASS | PASS WITH FINDINGS | FAIL |

## Results

| Step | Result | Notes |
|---|---|---|
| 1. `nltest /dsgetdc:` | PASS/FAIL/NOT RUN | … |
| 2. Domain join | | |
| 3. First domain logon (`klist`) | | |
| 4. Group policy (`gpupdate /force`, `gpresult /r`) | | |
| 5. Password change (kpasswd 464) | | |
| 6. SMB signing / encryption | | Dialect, Signed, Encrypted as observed |
| 7. Deployment-specific checks | | |

## Output

<the actual command output, verbatim, in fenced blocks — at minimum for
every step that did not pass>

## What did not work

<findings, including the ones that turned out to be the validator's own
mistake: the next person makes the same one>

## Consequences

<none — or: issue #N against this repository, or a new B.6 limitation with
the measurement quoted>
```

## Why the digest matters more than the tag

A branch tag moves with every patch release, so "validated on `:4.24`" is
not a statement about anything six weeks later. The digest is what makes a
record citable, and it is also what a downstream project compares against
when it decides whether its own validation still applies. Get it with:

```sh
docker image inspect --format '{{index .RepoDigests 0}}' \
  ghcr.io/esitc-paris/samba-ad-dc:4.24.7-r1
```
