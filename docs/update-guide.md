# Update guide

How to keep a deployment of this image current: what to pin, how to hear
that a release exists, how to apply a patch update and a branch upgrade,
what end-of-life means for a tag you run, and what to do when an update
has to be undone.

This is the guide SPEC.md §10.3 requires. The behaviour it describes is
the one in the **Runtime contract** of
[`docs/adaptation-profile.md`](adaptation-profile.md); the publication
side — what the watcher decides and when — is that file's **Release
cycle** section and [`docs/operations.md`](operations.md). Where the two
disagree with this guide, they win and this file is a bug.

Per §10.6 every section below names the E2E test that covers it, as
`Covered by:` with the test ID from [`docs/traceability.md`](traceability.md).
Where no test covers a statement, the section says so rather than
implying coverage. Test IDs are a frozen contract, not an implementation
detail: they do not get renamed.

Registries:

- `ghcr.io/esitc-paris/samba-ad-dc` — source of truth.
- `docker.io/esitcparis/samba-ad-dc` — mirror, published when the release
  had Docker Hub credentials. A release whose notes carry no Docker Hub
  digest was not mirrored.

---

## 1. Pinning

### The tags, and which of them move

| Tag | Mutable? | Points at |
|---|---|---|
| `X.Y.Z-rN` | **no** — immutable, never re-pushed (§3.2) | exactly one build; a correction increments `-rN` |
| `X.Y.Z` | yes | the latest `-rN` of that upstream patch release |
| `X.Y` | yes | the latest release of that branch |
| `X` | yes | the latest release of the **default (newest) branch** |
| `latest` | yes | the same as `X` — and **not production-usable** (§3.3) |

`X` and `latest` follow the default branch, which means they change
*branch* when a new upstream series becomes the newest one. That is the
property that makes them unsuitable for a domain controller: a routine
pull would apply a branch upgrade nobody decided.

Every release publishes its digests in the GitHub Release body — GHCR
always, Docker Hub when the mirror was pushed (§3.4).

### By profile

**Most deployments — pin the branch: `X.Y`.** For example
`ghcr.io/esitc-paris/samba-ad-dc:4.24`. You receive every patch release
and every rebuild of that branch (CVE fixes reach you as rebuilds, not
only as upstream releases), and you never receive a branch change without
deciding it. This is the recommended default.

**Change-controlled or regulated environments — pin the digest.**

```sh
ghcr.io/esitc-paris/samba-ad-dc@sha256:<index digest from the release notes>
```

A digest names one artifact and can never be moved under you; §3.4
requires this guide to recommend it for production, and it is the right
default wherever an image entering the estate is an auditable event.
`X.Y.Z-rN` is the weaker sibling of the same idea — immutable by policy
(§3.2) rather than by construction — and is readable at a glance, which a
digest is not. Both have the same consequence: **fixes stop arriving by
themselves.** The project's commitment is publishing quickly; with a
digest pin, applying is entirely yours, so pair it with the notification
setup in §2 and a scheduled review.

**Discouraged — `X` and `latest`.** `latest` is documented as not
production-usable (§3.3). `X` is not marked unusable but carries the same
branch-following behaviour, and for a stateful DC a branch that arrives by
surprise is worse than one that arrives late — see §7: what an image
update writes to the volume cannot be undone by pulling the old tag back.
(`edge` is permitted by §3.3 but is **not published by this repository**;
every published tag comes from a released upstream tarball.)

The README's tag table is the one-screen version of this section, and it
names the digest as the production pin. The two agree: a branch pin is a
deliberate trade — you accept that a rebuild arrives on its own, in
exchange for CVE fixes that do not wait for a change window. Where an
image entering the estate must be an auditable event, that trade is not
available and the pin is a digest.

*Covered by:* no E2E test — tag mutability is a registry property, not a
container behaviour. It is enforced at publication: `release.yml` refuses
a tag GHCR already carries, and `post-push-verify.yml` (§8.5) re-reads the
published image and checks the digest the release notes pin.

---

## 2. Learning that an update exists

- **GitHub Releases** — watch the repository (*Watch → Custom →
  Releases*). Every published tag has a release, and its body carries the
  embedded Samba version, the image digests, the trigger cause and the
  `cosign verify` command for that exact digest.
- **Atom feed** — `https://github.com/ESITC-Paris/samba-ad-dc/releases.atom`,
  for a feed reader or any notifier that takes a URL. No account needed.
- **CHANGELOG** — [`CHANGELOG.md`](../CHANGELOG.md), newest first: tag,
  Samba version, fixed CVEs, image changes and the trigger cause
  (`samba-release | pkg-update | base-digest | manual`) per §10.4. Every
  revision states why it exists.
- **The compatibility matrix** in [`README.md`](../README.md) is the
  always-current statement of which branches are published and what
  upstream says about each. Read it before planning anything in §5 or §6.
- **A registry notifier or dependency bot** — a tool that watches a tag
  and *tells you*, or opens a pull request against your deployment
  repository. Configure it to notify or propose. Do not configure it to
  apply: see §8.

What to expect of the cadence: the watcher runs hourly and dispatches a
release only when the image is certain to differ from the last published
one. A new upstream patch release additionally serves a 24-hour soak
before it is built, so it normally appears within about a day of upstream
publishing it; a pre-announced security release skips the soak when a
maintainer dispatches the watcher with `security_release=true`. Rebuilds
for a moved base image or a moved package closure carry no soak at all.

*Covered by:* no E2E test — this is the publication side of the project.
The decision procedure is specified in the **Release cycle** section of
`docs/adaptation-profile.md` and tested by the unit suites under
`scripts/` (`watch_test.py`, `catalog_test.py`).

---

## 3. Patch update, step by step

A *patch update* keeps the branch and changes the image: a new upstream
patch release (`4.24.7 → 4.24.8`) or a new revision of the same version
(`4.24.7-r1 → 4.24.7-r2`, typically a CVE-driven rebuild). A branch
change is §5.

The domain lives entirely in two volumes — `/var/lib/samba` (directory
database, private keys, sysvol) and `/etc/samba` (the generated
configuration). Nothing in the container's own filesystem is state, so an
update is: stop the old container, start the new image on the same two
volumes. **One container at a time per volume set** — the directory
database is single-writer, and two containers on one volume is data
loss, not a race you can win.

**1. Read the entry.** The release body and the CHANGELOG say what
changed and why. A `samba-release` cause is an upstream version change; a
`pkg-update` or `base-digest` cause is a rebuild of the same Samba.

**2. Back up first.** Take an offline backup before every update, however
routine. The procedure — `samba-tool domain backup offline` from a
stopped DC, and the restore that goes with it — is the backup/restore
runbook in the deployment guide:
[`docs/deployment-guide.md#backup-and-restore-runbook`](deployment-guide.md#backup-and-restore-runbook).
This backup is what §7 restores from; without it there is no rollback.

*Covered by:* `TestOfflineBackupRestore`.

**3. Verify the signature before the image reaches production.** Take the
digest from the release notes:

```sh
cosign verify ghcr.io/esitc-paris/samba-ad-dc@sha256:<digest> \
  --certificate-identity-regexp 'https://github.com/ESITC-Paris/samba-ad-dc/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The release body carries this same command pre-filled with its own
digest. SBOM and provenance attestations are attached to the image.

**4. Pull, then recreate the container on the same volumes.** With
compose, change the tag or digest in the file and `docker compose up -d`:
compose stops the old container and starts the new one on the declared
volumes. Change nothing else in the same step — an update that also moves
a variable cannot be diagnosed if it misbehaves.

Production containers run `SAMBA_MODE=run`, which refuses to start when
the state volume is absent (exit 21) rather than silently provisioning a
second, empty domain over a missing mount. If your deployment still says
`auto`, the update is a good moment to fix that.

**5. What the new image does on its first start — the marker.**
`/var/lib/samba/.image-state.json` records the Samba version that last
wrote the volume. On start the entrypoint compares it with the version
the image ships:

- **marker < image (an upgrade)** → `samba-tool dbcheck` runs *first*. On
  failure the container exits **23** and samba is never started. On
  success the marker is moved forward to the image's version and the
  daemons start. `initialized_at` is carried over, not restamped: it
  records when the domain was created, not when it was last checked.
- **marker == image** (a revision rebuild) → a plain restart. No dbcheck;
  nothing is written to the marker.
- **marker > image** → refusal, exit **22**. That is §7.
- **state present, marker absent** (a volume from before this image) → a
  warning, a dbcheck, and the marker is adopted.

So on an upgrade the log carries, in order:

```
the volume was written by an older samba: checking the database before starting
... (0 errors)
marker moved forward to samba <version>
```

Nothing about this is opt-in or configurable: the check is the upgrade
path.

**6. Watch health, not the process.** The image's `HEALTHCHECK` is an
application-level probe — the `_ldap._tcp.<realm>` SRV record on
localhost, an anonymous rootDSE read on LDAP, and an SMB share
enumeration — every 30 s, 3 retries, after a 180 s start period.

```sh
docker inspect --format '{{.State.Health.Status}}' <container>
```

Wait for `healthy` before touching the next DC (§4). A container that
never leaves `starting` and a container that flips to `unhealthy` are
both reasons to read the logs before doing anything else.

**7. Verify the directory, briefly.** `samba-tool user list` answers
whether the database opened; on a multi-DC domain, `samba-tool drs
showrepl` is the check that matters and §4 says when to run it.

**If the pull fails with no matching manifest for your architecture:**
the release may have used the §9.6 Mode 2 degraded mode, which publishes
one architecture. The release notes disclose it and name the missing
architecture. That tag is immutable — recovery is the *next* revision,
published complete; stay on the revision you have until it appears.

*Covered by:* `TestUpgradeFromLastPublished` — a domain provisioned by the
last published tag, started by the candidate image on the very same
volumes: the pre-upgrade user still resolves, the dbcheck line and the
`(0 errors)` and marker-moved lines appear, and `initialized_at` is
preserved. `TestIdempotentRestart` covers the same-version case (a
restart that changes no state).

---

## 4. Multi-instance rollout order

For a domain with more than one DC, the update is the §3 procedure
repeated, with three rules around it.

**One DC at a time.** Never recreate two DCs concurrently. A domain that
is momentarily one revision apart is normal and replicates; a domain
where two DCs are simultaneously down is one that cannot serve
authentication and cannot converge.

**The PDC emulator goes last.** Update the DCs that hold no FSMO role
first, then the role holders, and the PDC-emulator holder last of all. If
the new revision misbehaves on the first DC, the role holder is still on
the revision you know. Find the holders with:

```sh
samba-tool fsmo show
```

*(`samba-tool fsmo show` is upstream Samba's own command; no test in this
repository exercises it.)*

**Check replication between each one.** After a DC reports `healthy`, and
before starting on the next, confirm the links in both directions — on
the DC you just updated and on one partner:

```sh
samba-tool drs showrepl
```

What you want to see is inbound neighbours with **zero consecutive
failures** on both sides. A directory serves stale reads perfectly
happily, so "the users are still there" does not prove replication
survived; the failure counter does. A quick end-to-end confirmation is to
create an object on one DC and read it back on the other, then delete it.

**A note on sysvol.** Samba provides no DFS-R, so group-policy content
under sysvol does not replicate by itself (a stated limitation, B.6). An
image update neither improves nor worsens this, but if you run a manual
sysvol synchronisation from the PDC-emulator holder, re-run it after the
rollout.

*Covered by:* `TestJoinReplicationBothWays` covers the verification step —
two DCs, an object propagated in each direction, and `samba-tool drs
showrepl` asserted to report partners with zero consecutive failures on
both. The rollout *order* itself is not covered by a test: the E2E suite
upgrades a single DC (`TestUpgradeFromLastPublished`), and the ordering
rule is an operational precaution, not an image behaviour.

---

## 5. Branch upgrade

A *branch upgrade* changes the upstream series: `4.23 → 4.24`. Mechanically
it is the §3 procedure with the pin changed from `X.(Y-1)` to `X.Y`, and
it is the one start where the automatic dbcheck genuinely has work to do.
What differs is the support boundary.

**Supported and tested, exactly two paths:**

- *Intra-branch* — the branch's previously published `X.Y.Z-rN` to the tag
  being released. Tested on **every** release of any branch.
- *Previous branch → current* — `X.(Y-1)` to `X.Y`. Tested on every
  release of the current branch.

Both are the same test: `TestUpgradeFromLastPublished`, with the path
decided by the image `E2E_UPGRADE_FROM` names. First recorded evidence for
the cross-branch path (local arm64, 2026-09-16): a domain provisioned
under Samba 4.23.12 started under 4.24.7, the pre-upgrade user still
resolvable, dbcheck clean, marker moved forward, `initialized_at` carried
over.

**Unsupported: skipping a branch** (4.22 → 4.24), and downgrading in any
form. Nothing *blocks* a skipped-branch start mechanically — the guard in
front of the database refuses an older Samba, not a distant newer one — so
"unsupported" here means untested and unclaimed, not prevented. If you are
two branches behind, go one branch at a time: 4.22 → 4.23, verify and let
it run, then 4.23 → 4.24.

**On a multi-DC domain** the §4 rules apply unchanged, and one more: do
not leave the domain split across two Samba branches for longer than the
rollout takes. Finish it, then verify replication in both directions
again.

**Functional level.** An image upgrade does **not** raise the domain or
forest functional level, and this guide does not ask you to. Two distinct
things carry that name:

- `SAMBA_FUNCTION_LEVEL` applies at **provision and join only**. Its value
  is mirrored onto the `ad dc functional level` parameter in the generated
  `smb.conf`, which lives on the `/etc/samba` volume — so every later
  start, including every upgrade, keeps the level the domain was created
  at. Setting the variable on an existing DC changes nothing.
- The **domain's** functional level is a property of the directory. Raising
  it is a separate administrative act with its own compatibility
  consequences, not a side effect of an image update, and no test in this
  repository exercises it.

Do not edit that smb.conf parameter *below* the domain's level: samba
refuses to start at all.

*Covered by:* `TestUpgradeFromLastPublished` (both supported paths, per the
bound stated in B.6 of `docs/adaptation-profile.md`). No test covers a
skipped-branch upgrade — that is the point of calling it unsupported. The
cross-branch CI run is described in `docs/operations.md`.

---

## 6. End of life

Branch lifecycle is **upstream's**, relayed here rather than decided
(§9.4). The README compatibility matrix carries one row per published
branch and a status that is positional — newest branch first:

| Status in the matrix | What it means for you |
|---|---|
| `current` | the default branch; owns the `X` and `latest` aliases |
| `maintenance` | fully published, still receiving upstream fixes |
| `security fixes only` | published on every upstream release, but upstream now ships security fixes only |
| `discontinued (EOL)` | upstream has ended it |

**The deprecation notice.** When upstream publishes a release candidate
for a series newer than the newest branch in the catalog, that is
upstream's own end-of-life signal for the oldest branch still supported.
It is relayed immediately: the oldest row gains a
`— deprecation pending (X.Y rc published)` suffix, and one issue is
opened in this repository. In practice this lands several weeks before the
effective end of life, following upstream's release-candidate cadence.

**A notice is not a removal.** A branch under deprecation-pending keeps
receiving *every* patch release and every rebuild until upstream actually
ends it. Nothing is withdrawn, and the suffix disappears by itself if the
candidate stops applying.

**When upstream does end it**, the branch leaves the catalog and the
matrix says `discontinued (EOL)`. Published tags of that series **stay
published** — removing a branch stops future builds, it never unpublishes
anything — but they stop being rebuilt, which means they stop receiving
CVE fixes. An EOL tag that still pulls is not a maintained tag.

**What to do as an operator:** treat the deprecation-pending suffix as
the signal to schedule §5, not as something to note and revisit later.
Its whole purpose is to hand you those weeks. A tag must never silently
stop receiving rebuilds (§9.4), so the matrix is the authoritative place
to look; B.7 in `docs/adaptation-profile.md` is a dated snapshot for a
reader of that file.

*Covered by:* no E2E test — lifecycle relaying is a publication-side
policy, not a container behaviour. The rendering rules and the
deprecation suffix are covered by the unit suite
(`scripts/catalog_test.py`, `scripts/watch_test.py`).

---

## 7. Rollback

**Rollback is restore-based. It is never a tag downgrade.** This is not a
preference; it is what the image enforces and what the data model
requires.

### Why a tag downgrade is not a rollback

The state volume records the Samba version that wrote it. Start an image
whose Samba is *older* than that marker and the container refuses, with
exit code **22** and one line on stderr:

```
ERROR: the state volume was written by Samba X but this image provides Samba Y; deploy an image tag providing Samba X or newer, or restore a backup of the volume taken on Samba Y
```

(`X` is the marker's version, `Y` is the version this image ships.) The
refusal happens **before** anything is opened or checked: no dbcheck runs
with the older binary, which is precisely the hazard the guard exists to
prevent. A refused downgrade costs you a failed start, not a database.

The refusal is also not the whole story. Where it does *not* fire — a
revision rollback `-r2 → -r1`, same Samba version — rolling the image back
still does not roll the directory back. An upgrade start that ran a
dbcheck, and anything the newer Samba wrote afterwards, has already
happened on the volume. The image is not the state.

### The procedure

1. **Stop the DC.** Leave the volumes alone.
2. **Restore the backup you took before the update** (§3, step 2), into
   **fresh** volumes, following the restore half of the runbook:
   [`docs/deployment-guide.md#backup-and-restore-runbook`](deployment-guide.md#backup-and-restore-runbook).
   A restore is not a copy of the old volume over the new one; it
   relocates state, re-registers the realm's records, and cannot reuse the
   backed-up DC's own name.
3. **Start the image tag the backup was taken on** — the one whose Samba
   version matches what the restored data expects. Keeping the exact
   `X.Y.Z-rN` of the running deployment written down is what makes this
   step unambiguous, which is another argument for immutable pins (§1).
4. **Verify**: healthy, `samba-tool user list`, and on a multi-DC domain
   `samba-tool drs showrepl` in both directions.

On a multi-DC domain there is a second option that is often better than a
restore — demote the damaged DC and re-join it from a healthy partner, so
the surviving directory is the source of truth rather than a backup with
an age. This repository does not test that path; `TestJoinReplicationBothWays`
covers a join into a healthy domain, which is its main ingredient, but the
demote-and-rejoin sequence as a recovery procedure is unclaimed here.

*Covered by:* `TestDowngradeRefused` — a real provisioned domain whose
marker is edited forward, started by the image: exit 22, both versions
named in the message, and no initialization phrase anywhere in the log
(the guard refused before touching anything). `TestOfflineBackupRestore` —
backup and restore into a fresh instance with object-level verification.

### Exit codes you may meet during an update

| Code | Meaning | Where it comes up |
|---|---|---|
| `21` | run mode with absent state | the volume was not mounted — fix the mount, do not provision |
| `22` | downgrade refusal | §7, above |
| `23` | database consistency check failure | the automatic dbcheck on an upgrade start failed; samba was not started |

The full table is in the **Runtime contract** of
`docs/adaptation-profile.md`. Exit codes are immutable once released.

---

## 8. Automatic appliers are discouraged

**The project's reactivity is in publishing fast; applying remains a
supervised user decision.** §10.3 states this for every image built to
this specification, and a domain controller is the case it was written
for: do not point Watchtower, or any equivalent auto-updater, at this
container.

Three concrete reasons, all of them consequences of sections above:

1. **It is stateful and single-writer.** An update is stop-then-start on
   a live directory database. An unattended restart during a replication
   window, or one that races a second container onto the same volume, is
   not recoverable by restarting again.
2. **The upgrade start can legitimately fail.** A marker-forward start
   runs `samba-tool dbcheck` and exits **23** if it is not clean. That is
   the guard doing its job, and it needs a human to read the output — an
   applier will simply report a crash-looping container.
3. **You cannot take it back by re-pulling.** Rollback is restore-based
   (§7), and a restore needs a backup taken *before* the update. An
   applier that updates unattended has, by construction, updated without
   one.

To that add the mutable-alias hazard from §1: an applier watching `X` or
`latest` will one day apply a *branch* upgrade.

What to automate instead — all of it safely:

- **Detection**: the Atom feed, a registry notifier, or a bot that opens a
  pull request against your deployment repository (§2).
- **The backup** that precedes the update (§3, step 2).
- **The verification** that follows it: health status, `samba-tool drs
  showrepl`, an object round-trip.

Leave the decision to apply, and the order in which DCs are touched, to a
person.

*Covered by:* no E2E test — this is a policy statement. The behaviours it
rests on are covered: `TestUpgradeFromLastPublished` (the dbcheck on an
upgrade start), `TestDowngradeRefused` (why re-pulling is not a rollback),
`TestOfflineBackupRestore` (what a rollback needs).

---

## Section → test map (§10.6)

| Section | E2E test |
|---|---|
| §1 Pinning | *(none — registry property; publication-side gates)* |
| §2 Learning that an update exists | *(none — publication side)* |
| §3 Patch update | `TestUpgradeFromLastPublished`, `TestIdempotentRestart`, `TestOfflineBackupRestore` |
| §4 Multi-instance rollout order | `TestJoinReplicationBothWays` (verification step; order itself untested) |
| §5 Branch upgrade | `TestUpgradeFromLastPublished` |
| §6 End of life | *(none — publication-side policy)* |
| §7 Rollback | `TestDowngradeRefused`, `TestOfflineBackupRestore` |
| §8 Automatic appliers | *(none — policy; rests on the three tests above)* |

`TestUpgradeFromLastPublished` skips loudly while `E2E_UPGRADE_FROM` is
unset — the first release of a branch has no published tag to upgrade
from (§8.3) — and skips again when the named tag ships the same Samba,
where there is no upgrade to observe. Both skips name the variable.
