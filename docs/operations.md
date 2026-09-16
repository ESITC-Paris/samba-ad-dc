# Operations

How the release cycle is scheduled, supervised and driven by hand.

What the automation *decides* — the sources it watches, the decision order,
the soak, the necessity criterion, what it refuses to decide alone — is in
[`adaptation-profile.md`, section "Release cycle"](adaptation-profile.md#release-cycle),
and is not repeated here. This file is the other half: the external timer
that starts it, the repository settings it needs, how a human watches it,
and every manual operation with the exact command.

Repository: `ESITC-Paris/samba-ad-dc`.
Registries: `ghcr.io/esitc-paris/samba-ad-dc` (source of truth, SPEC §3.1)
and `docker.io/esitcparis/samba-ad-dc` (mirror).

## Scheduling model

The watcher (`.github/workflows/upstream-check.yml`) has **no GitHub
`schedule:` trigger**. It is dispatched hourly from ESITC infrastructure
through the `workflow_dispatch` API. Rationale (SPEC §9bis.7 requires a
real execution guarantee at the polling frequency, which Actions cron does
not provide):

- external dispatch is exempt from GitHub's 60-day inactivity auto-disable
  of scheduled workflows;
- it is not subject to GitHub scheduler congestion — cron triggers are
  delayed or skipped under load;
- the decision logic, the secrets and the publishing all stay inside the
  audited repository. The external component is a dumb, replaceable
  trigger, and it does not need to be trusted — only the executor does
  (§4.5). The worst a compromised trigger can do is start runs that
  publicly pass or fail the §8 gates.

## One-time setup

### 1. Create a fine-grained GitHub token

GitHub → Settings → Developer settings → Fine-grained personal access
tokens:

- **Repository access:** only `ESITC-Paris/samba-ad-dc`
- **Permissions:** Actions → *Read and write* (nothing else)
- Note the expiration date — token renewal is an operational duty. Expiry
  is detected by the trigger layer below (HTTP 401 → cron mail).

### 2. Install the trigger on a server

`/usr/local/bin/samba-ad-dc-upstream-check`:

```sh
#!/bin/sh
# Triggers the samba-ad-dc release watcher on GitHub.
# The token file must contain the fine-grained PAT, mode 600, owner root.
set -eu

TOKEN_FILE=/etc/samba-ad-dc/github-token
API=https://api.github.com/repos/ESITC-Paris/samba-ad-dc/actions/workflows/upstream-check.yml/dispatches

HTTP_CODE=$(curl -sS -o /tmp/samba-ad-dc-upstream-check.err -w '%{http_code}' \
  --retry 3 --retry-delay 10 \
  -X POST \
  -H "Authorization: Bearer $(cat "$TOKEN_FILE")" \
  -H "Accept: application/vnd.github+json" \
  "$API" -d '{"ref":"main"}')

if [ "$HTTP_CODE" != "204" ]; then
  echo "upstream-check dispatch failed (HTTP $HTTP_CODE):" >&2
  cat /tmp/samba-ad-dc-upstream-check.err >&2
  exit 1
fi
```

Both workflow inputs default to `false`, so the bare `{"ref":"main"}` body
is a normal hourly check: 24 h soak, nothing forced, everything applied.

```sh
sudo install -m 755 samba-ad-dc-upstream-check /usr/local/bin/
sudo install -d -m 700 /etc/samba-ad-dc
sudo sh -c 'umask 077; printf "%s\n" "github_pat_XXXX" > /etc/samba-ad-dc/github-token'
```

### 3. Cron entry

```cron
MAILTO=admin@esitc-paris.fr
23 * * * * /usr/local/bin/samba-ad-dc-upstream-check
```

The script is silent on success, so cron mails only on failure (bad or
expired token, network or API error). The minute is arbitrary and only
needs to be stable; keeping it off the hour avoids the API's busiest
minute.

### 4. Repository configuration checklist

| Kind | Name | Value | Effect if absent |
|------|------|-------|------------------|
| Secret | `DOCKERHUB_USERNAME` | Docker Hub account with push rights on `esitcparis/samba-ad-dc` | Mirror skipped with a `::warning::`; the release still succeeds — GHCR is the source of truth |
| Secret | `DOCKERHUB_TOKEN` | Docker Hub access token with push rights on that repository | as above |
| Variable | `IMAGE_NAME` | **unset in production.** Set only for a drill that must retarget the package itself (see [Staging drill](#staging-drill)) | defaults to `samba-ad-dc` in `upstream-check.yml`, `release.yml` and `post-push-verify.yml` |
| Variable | `RELEASE_SOAK_HOURS` | **unset in production.** The §9bis.5 soak in hours | defaults to `24` |

Both Docker Hub secrets are checked together: the mirror runs only when
*both* are non-empty and the run is not a staging rehearsal.

No other secret is needed. Signing is keyless (OIDC), attestations and
GHCR pushes use the run's `GITHUB_TOKEN`, and the watcher's issue, tag and
dispatch operations use the same token.

## Going live: the first publication

The repository publishes its first images without a bootstrap procedure,
because "the catalog's current tag is not on the registry" is an ordinary
watcher decision (`action=publish`, `cause=first-publication`) rather than
a special case. On the first hourly run the three catalog branches are all
in that state, so the run decides `publish` three times.

1. **Optional — add the Docker Hub secrets.** Without them the first three
   releases publish to GHCR only and warn; the mirror can be added later
   and picks up from the next release onward. Nothing back-fills it.
2. **Start the clock.** Either install the cron trigger above, or dispatch
   one run by hand:

   ```sh
   gh workflow run upstream-check.yml --repo ESITC-Paris/samba-ad-dc
   ```

3. **Watch what that run does.** It creates and pushes `v4.24.7-r1`,
   `v4.23.12-r1` and `v4.22.11-r1`, then dispatches `release.yml` once per
   tag with `-f trigger_cause=first-publication`. Each tag is dispatched
   on its own, so one dispatch that will not take cannot hold back the
   others; the run fails at the end if any of them failed, and the next
   hourly run decides `publish` again for whatever did not make it.
4. **Watch the three release runs.** They are independent (the concurrency
   group is keyed on the tag) and each one runs the complete §8 gate set
   natively on both architectures before anything user-visible exists: the
   two `build` legs run in parallel and are budgeted 150 minutes each,
   `merge` 30. Expect the three runs to occupy four runners for a good
   part of an afternoon.
5. **Watch the three post-push verifications** (see below), which re-check
   the published artefacts from outside the pipeline.
6. **Delete the pre-release notice in `README.md`** once the first release
   exists — the paragraph beginning `> **Status: pre-release.**`. It is a
   plain blockquote with no HTML marker around it, so this is a hand edit,
   not a scripted one. (Phase 5 rewrites the README around it anyway.)

Nothing else changes state: a `publish` decision edits no tracked file by
design, so the watcher's own commit on that run is a state-only commit or
no commit at all.

## Supervision

Four independent layers, each of which fails loudly on its own:

1. **Trigger layer (the server).** Any non-204 response — including an
   expired token (HTTP 401) — makes the script exit non-zero and cron
   `MAILTO` reports it. Optionally add a dead-man's switch (a
   Healthchecks.io ping appended to the cron line) to also detect the
   server itself going quiet, which is the one failure this layer cannot
   report by itself.
2. **Watcher layer.** A failed `upstream-check` run opens the issue
   *"Upstream check is failing"* assigned to `euca01`, or comments on the
   existing one. Every refusal by `watch.py` (an untrusted probe, a branch
   missing from the catalog, a malformed observation) is a failed run and
   therefore reaches this layer.
3. **Release layer.** A failed `release.yml` run opens *"Release pipeline
   failed for `<tag>`"*, assigned and deduplicated the same way. A
   successful one opens *"Published `<tag>`"* — informational, close it
   after reading. Post-push verification failures open *"Post-push
   verification failed: `<tag>`"* — with a ` (staging)` suffix when the run
   verified a rehearsal.
4. **Publication layer.** Every release appears on the
   [releases page](https://github.com/ESITC-Paris/samba-ad-dc/releases)
   (subscribe with Watch → Custom → Releases) and in `CHANGELOG.md`.

Three signals are deliberately *not* failures and arrive as assigned
issues instead:

- *"New upstream series X.Y — catalog decision required"* and
  *"Deprecation pending: branch X.Y (Z.W rc published)"* — catalog
  decisions the automation must not make; see
  [What needs a human](adaptation-profile.md#what-needs-a-human).
- *"CVE advisory: branch `<X.Y>` (`<X.Y.Z-rN>`)"* — one per published
  branch, opened when a package the ledger did **not** list before gains
  an unfixed HIGH/CRITICAL finding in that branch's published image. A new
  CVE on a package already listed opens nothing and comments nothing — the
  ledger is still synced, but silently: otherwise every refresh of the
  vulnerability feed would reopen the same issue. It is
  informational, and there is nothing to build: a finding with no
  available fix cannot be resolved by rebuilding (§9bis.8.c), the sync has
  already written the entries into `security/cve-exceptions.yaml`, and
  each new one carries a 90-day `review_by`. What to do is read those
  entries: confirm each package still has no fixed version and record why
  the finding does not apply, or delete the entry once a fix ships — a fix
  arrives as a package-closure change and becomes a build trigger by
  itself. Extending a `review_by` is a human edit of the file; a sync
  never moves one, and `check-cve-exceptions.py` fails the build once one
  has passed. While the package list keeps moving the **open** issue is
  commented rather than recreated; close it when the review is done, and
  the next new package opens a fresh one.

The CVE advisory step is `continue-on-error` on purpose: it can never
trigger a build (§9bis.1.d), so its failure must not fail a run whose
release decisions were already made. It is therefore *not* a supervision
layer — a silent advisory step is a missing document, not a missing
release.

## Manual operations

All commands assume `gh auth login` on an account with write access. Add
`--repo ESITC-Paris/samba-ad-dc` when running from outside a clone.

### Run the watcher now

```sh
gh workflow run upstream-check.yml --repo ESITC-Paris/samba-ad-dc
```

### Pre-announced Samba security release

Samba announces security releases ahead of time. Dispatching with
`security_release=true` sets the soak to 0 h (§9bis.5), so the version is
published on this run instead of 24 h later:

```sh
gh workflow run upstream-check.yml --repo ESITC-Paris/samba-ad-dc \
  -f security_release=true
```

This changes *when*, never *what*: the decision order, the upstream
signature verification and every §8 gate are unchanged.

### See what it would do, change nothing

```sh
gh workflow run upstream-check.yml --repo ESITC-Paris/samba-ad-dc \
  -f dry_run=true
```

`dry_run` suppresses everything that writes: no commit, no tag, no
dispatch, no issue, and not even the conditional-request cache in
`.build-state.json`. The plan is printed to the job summary. The same
thing locally, without touching GitHub at all:

```sh
python3 scripts/watch.py observe --output observation.json --dry-run
python3 scripts/watch.py plan --observe observation.json --soak-hours 24
```

### Publish a tag by hand

A maintainer bypasses the watcher by pushing a tag: `release.yml` keeps its
`push: tags: ['v*']` trigger for exactly that. Edit `versions.yaml` through
`catalog.py`, commit, then:

```sh
git tag v4.24.8-r1 && git push origin v4.24.8-r1
```

### Re-run a release for an existing tag

A tag pushed by the watcher's own token does not trigger `release.yml`
(GitHub's recursion guard), which is why the watcher dispatches it
explicitly. The same dispatch re-runs a release whose dispatch was lost or
whose run failed for an external reason:

```sh
gh workflow run release.yml --repo ESITC-Paris/samba-ad-dc \
  --ref v4.24.7-r1 -f tag=v4.24.7-r1 -f trigger_cause=first-publication
```

`trigger_cause` is one of `samba-release`, `pkg-update`, `base-digest`,
`manual`, `first-publication`; it goes into the release notes and the
CHANGELOG, so it should say why the revision exists.

A re-run only succeeds if the tag is **not** already published: the §3.2
immutability check fails the run when `<image>:<X.Y.Z-rN>` resolves on
GHCR. A published tag is never amended in place — bump the revision
instead (`python3 scripts/catalog.py bump-revision <branch>`).

### Staging drill

A rehearsal of the whole publication path with no public artefact:

```sh
gh workflow run release.yml --repo ESITC-Paris/samba-ad-dc \
  --ref main -f tag=v4.24.7-r1 -f staging=true
```

What `staging=true` changes: the target package becomes
`ghcr.io/esitc-paris/samba-ad-dc-staging`, the immutability check is
skipped (so the drill is repeatable), and the Docker Hub mirror, the
GitHub Release, the CHANGELOG entry and the published-notification issue
are all skipped. The run title carries a ` (staging)` suffix, which is how
post-push verification tells a rehearsal from a publication. Everything
else — every §8 gate, both native architectures, the manifest merge,
cosign signing and the attestations — runs exactly as it does for a real
release.

A staging run is dispatched **before** the tag exists, which is the point
of it: it proves the pipeline against the tree `main` is about to tag, so
it checks out the dispatch ref rather than the tag. The tag argument must
still have the `vX.Y.Z-rN` shape and must match the catalog.

The `IMAGE_NAME` variable is a *different* knob: it retargets the package
name everywhere, including the watcher's published-tags probe, which
`staging=true` does not. Set it to `samba-ad-dc-staging` only for a drill
of the **watcher** end to end, and unset it afterwards. Do not combine the
two — `IMAGE_NAME=samba-ad-dc-staging` plus `-f staging=true` targets
`samba-ad-dc-staging-staging`.

### Post-push verification

`.github/workflows/post-push-verify.yml` (SPEC §8.5) runs automatically on
every successful `Release` run and re-reads the published image from the
registry with no access to anything the release produced. It publishes
nothing and repairs nothing: its output is a green run or an issue.

What it asserts: the manifest list publishes **exactly** the promised
platforms (set equality — an unpromised architecture is as much a defect
as a missing one); the index digest matches the one the release notes pin;
every platform image pulls by digest; `cosign verify` succeeds against an
identity under this repository and the GitHub Actions OIDC issuer; `gh
attestation verify --repo` accepts the provenance over the index digest;
and the BuildKit SBOM and provenance attestations are non-empty for every
platform. When the release notes carry a Docker Hub digest, the same
checks run against the mirror — no Hub digest means no mirror to verify,
which is the staging case and the no-credentials case.

The promised platform set comes from the release notes: both architectures
by default, one when the notes carry the §9.6 Mode 2 disclosure, whose text
names the architecture that is absent. Mode 1 (emulated) is deliberately
*not* an exception — it still ships both. A tag with no GitHub Release body
(a staging drill, or a deleted release) warns and falls back to the
strictest expectation.

To re-verify a tag by hand:

```sh
gh workflow run post-push-verify.yml --repo ESITC-Paris/samba-ad-dc \
  -f tag=v4.24.7-r1
```

Add `-f staging=true` to verify the `-staging` package instead. A failure
opens *"Post-push verification failed: `<tag>`"*, assigned to `euca01` and
deduplicated; a staging failure files its own issue with a ` (staging)`
suffix in the title, so it never dedups against a real one. The issue says
plainly what the situation is: nothing was changed or republished, the
image is live, and the decision to yank it is a human's.

Two consequences of the `workflow_run` trigger are worth knowing before the
first release. It fires only for `release.yml` **as it exists on the
default branch**, so a change to either workflow on a feature branch is not
exercised by it until merge; and the triggering Release must have concluded
`success`, since a failed release published nothing to verify and would
only add noise to the issue `release.yml` already filed.

### Add a new upstream series

The watcher opens *"New upstream series X.Y — catalog decision required"*
and stops: adding a branch commits this repository to publishing it on
every release (§3.6), which is a human decision. To act on it:

```sh
sh scripts/verify-upstream-tarball.sh 4.25.0     # GPG chain -> sha256
$EDITOR versions.yaml                            # new branch block, newest first
python3 scripts/catalog.py update-readme-matrix  # re-render the matrix
sh scripts/check-pins-consistency.sh             # Dockerfile ARG mirror
```

The matrix re-render is a convenience, not a contract: `apply` re-renders
it on every watcher run that acts, adding the §9.4 deprecation-pending
suffix the run a candidate is detected and removing it the run it stops
applying. `versions.yaml` is what must be right.

Adding the **newest** series also changes the default branch
(`default_branch:` in `versions.yaml`), which decides which release is
GitHub's "Latest", which one carries the `latest` image alias and which one
syncs the Docker Hub description. Open it as a pull request so CI gates the
change, then close the issue.

### Remove an end-of-life series

Never automatic, and never triggered by a release candidate alone: a newer
series' rc only earns the oldest branch a "deprecation pending" suffix in
the matrix (`update-readme-matrix --rc-series X.Y`), and that branch keeps
receiving every patch release until upstream actually ends it. When
upstream does:

```sh
$EDITOR versions.yaml                            # delete the branch block
python3 scripts/catalog.py update-readme-matrix
```

Published tags of that series stay published — removing a branch stops
future builds, it never unpublishes anything.

### Renew a CVE review date

`security/cve-exceptions.yaml` is synchronised by the watcher
(`scripts/cve-ledger.py sync`), one entry per affected package, and a sync
never moves a `review_by` date. `scripts/check-cve-exceptions.py` fails the
build once one has passed (§5.4). Renewing it is a human re-reading the
entry and either extending the date with fresh reasoning or deleting it —
by pull request, so CI gates it:

```sh
python3 scripts/check-cve-exceptions.py   # what is expired, if anything
```

### Re-measure the capability set

`.github/workflows/capbisect.yml` is `workflow_dispatch` only and costs
roughly an hour of runner time per architecture. Run it when the
capability set is in question — a new Samba major, a change to what the
entrypoint does at boot, a review that asks whether `SYS_ADMIN` is really
needed. Its output is a report artifact; the findings reach
`adaptation-profile.md` B.2 by a pull request citing the run.

```sh
gh workflow run capbisect.yml --repo ESITC-Paris/samba-ad-dc
```

### Re-run CI without a new commit

The security gates read a vulnerability database refreshed daily, so the
same commit is green today and red tomorrow. `ci.yml` accepts a manual
dispatch for that:

```sh
gh workflow run ci.yml --repo ESITC-Paris/samba-ad-dc --ref main
```

## Degraded publication modes (SPEC §9.6)

Two modes exist for one situation only: a **security-driven** publication
whose deadline (§9.2: fixable CRITICAL 48 h, HIGH 7 days) would otherwise
be missed because a runner architecture is unavailable. They are
`workflow_dispatch` inputs on `release.yml` and are never selected
automatically.

Both read `staggered_arch`, which names the architecture that **is**
available; the other one is the affected architecture in the disclosure.

**Mode 1 — emulated build.**

```sh
gh workflow run release.yml --repo ESITC-Paris/samba-ad-dc \
  --ref v4.24.8-r1 -f tag=v4.24.8-r1 -f trigger_cause=samba-release \
  -f degraded_mode=emulated -f staggered_arch=amd64
```

Both architectures are published. The unavailable one is built under QEMU
on the surviving pool's runner — *not* on a hardcoded amd64 runner, which
would put the emulated build on the pool just declared unavailable. No gate
is waived: the full E2E suite runs under emulation, which is why the leg is
slow. The release notes disclose it as Mode 1 and say what it does and does
not affect (build speed, not provenance).

**Mode 2 — architecture-staggered publication.**

```sh
gh workflow run release.yml --repo ESITC-Paris/samba-ad-dc \
  --ref v4.24.8-r1 -f tag=v4.24.8-r1 -f trigger_cause=samba-release \
  -f degraded_mode=staggered -f staggered_arch=amd64
```

The unavailable architecture is not built at all, and the manifest lists
one platform. `merge` asserts the digest count against what the mode
declared, so a normal run that quietly shipped one architecture, and a
Mode 2 run that quietly shipped two, both fail rather than publish. The
release notes disclose that users on the missing architecture remain on the
previous revision, and that this tag is immutable: recovery is the **next
revision**, published complete once the platform is back.

```sh
python3 scripts/catalog.py bump-revision 4.24    # then commit, tag, push
```

In both modes the disclosure text is generated by `catalog.py
release-notes --degraded <mode> --pending-arch <arch>`, so the release
notes and the CHANGELOG cannot disagree about what was degraded, and the
job summary carries the same statement.

## Triage

| Symptom | First look | Usual cause |
|---------|-----------|-------------|
| Cron mail, HTTP 401 | the PAT | expired fine-grained token — reissue, same scope |
| Cron mail, HTTP 404 | the API path and the token's repository scope | token scoped to the wrong repository, or workflow file renamed |
| No cron mail and no runs for hours | the trigger host | the server, not GitHub — this is what the dead-man's switch catches |
| *"Upstream check is failing"* | the linked run | a probe that could not be trusted (`watch.py` exit 2), or a push race on `main` |
| *"Release pipeline failed"* | the linked run's failed job | a §8 gate — nothing was published; the tag exists and can be re-dispatched once the cause is fixed |
| Release fails on the immutability check | GHCR tag list | the tag is already published; bump the revision |
| Mirror missing from the notes | the `prepare` job's warning | `DOCKERHUB_USERNAME` / `DOCKERHUB_TOKEN` unset |
| *"CVE advisory: branch …"* | the named packages in `security/cve-exceptions.yaml` | a component gained an unfixed HIGH/CRITICAL CVE — informational, no rebuild can fix it; confirm each entry or delete it once a fix ships |
| A `publish` decision every hour | GHCR, for the tag it names | the image genuinely is not there — the dispatch is being lost; the tag itself is not re-created and is not the problem |
