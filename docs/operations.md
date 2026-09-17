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
| Variable | `RELEASE_SOAK_HOURS` | set to `24` — or unset, which is the same thing. The §9bis.5 soak in hours | defaults to `24` |
| Setting | Dependabot alerts | **enabled** (`gh api repos/ESITC-Paris/samba-ad-dc/vulnerability-alerts` answers `204`) | `.github/dependabot.yml` still opens its weekly version bumps, but a CVE disclosed against a pinned action or Go module between two of them goes unannounced |
| Setting | Private vulnerability reporting | **enabled** (`gh api repos/ESITC-Paris/samba-ad-dc/private-vulnerability-reporting` → `{"enabled":true}`) | `SECURITY.md` sends reporters to `.../security/advisories/new`, which only members can open while this is off — leaving a public issue as the reporter's only channel, which is exactly what that file forbids |
| Setting | Actions permissions | `allowed_actions: all`; default `GITHUB_TOKEN` permissions **write** | Narrowing the allow-list stops the SHA-pinned third-party actions (`docker/*`, `sigstore/*`, `actions/attest-build-provenance`) from resolving. The token default is only a fallback here: every workflow declares its own `permissions:` block — `release.yml:45`, `upstream-check.yml:29`, `post-push-verify.yml:42` — so what would inherit this default is a workflow added later without one |
| Setting | Package visibility | `samba-ad-dc` **public**, set per package after the first release (step 6 of *Going live* below) or organisation-wide at *Organization settings* → *Packages* | Anonymous `docker pull`, `cosign verify` and `gh attestation verify` all fail, and no automated check notices: post-push verification runs with `GITHUB_TOKEN` and reads a private package happily |

Both Docker Hub secrets are checked together: the mirror runs only when
*both* are non-empty and the run is not a staging rehearsal.

The four `Setting` rows are repository configuration, not anything the
tree can hold itself to: no file in this repository can assert them, and
three of the four fail *silently* when they are wrong — a missed advisory,
a reporter with nowhere to report, an image nobody outside the
organisation can pull. Only the Actions row fails loudly. They are written
down here so that a repository restored, forked or transferred can be put
back into the state the rest of this document assumes, and each value
above is what the API answered on 2026-09-17.

`RELEASE_SOAK_HOURS` is a variable rather than a setting, and it is set to
`24`, which is the value `upstream-check.yml` already defaults to when it
is unset. Setting it changes nothing; it is recorded because the live
repository has it, and deleting it would also be correct.

No other secret is needed. Signing is keyless (OIDC), attestations and
GHCR pushes use the run's `GITHUB_TOKEN`, and the watcher's issue, tag and
dispatch operations use the same token.

## Going live: the first publication

The repository publishes its first images without a bootstrap procedure,
because "the catalog's current tag is not on the registry" is an ordinary
watcher decision (`action=publish`, `cause=first-publication`) rather than
a special case. On the first hourly run the three catalog branches are all
in that state, so the run decides `publish` three times.

One caveat on that expectation: the closure hashes committed in
`.build-state.json` were measured on 2026-09-16, and the Debian package
index moves on its own schedule. If it has moved since, the first hourly
run decides `revision` with cause `pkg-update` instead, and the first
published tags are `-r2` rather than `-r1`. That is correct behaviour, not
a fault — the image would genuinely differ from the one the committed hash
describes — and nothing about the rest of this section changes.

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
   `merge` 30. The budget is a ceiling, not an estimate — the 2026-09-17
   drill's legs took about 20 minutes each. Expect six runners busy at
   once if all three dispatch together.
5. **Watch the three post-push verifications** (see below), which re-check
   the published artefacts from outside the pipeline.
6. **Make the package public.** GHCR creates a new organisation package
   **private by default** — measured on 2026-09-17: the staging drill's
   `samba-ad-dc-staging` package came out private without anyone choosing
   that. A private package is invisible to an anonymous client, so
   `docker pull` and `cosign verify` both fail for everyone but the
   organisation until this is done, and every verification instruction in
   the README and the deployment guide is wrong until it is. It cannot be
   done before the first release, because the package does not exist yet.

   In the UI: the package page → *Package settings* → *Danger Zone* →
   *Change visibility* → **Public**. (An organisation owner can instead set
   the organisation's default package visibility to public beforehand, at
   *Organization settings* → *Packages*, which makes this step unnecessary
   rather than optional.)

   Then confirm it from outside, logged out of everything:

   ```sh
   docker logout ghcr.io
   docker buildx imagetools inspect ghcr.io/esitc-paris/samba-ad-dc:4.24
   ```

   `4.24` is the default branch's mutable alias rather than an immutable
   tag, so the command stays correct whichever revision publishes first —
   the section above already warns that the first tags may be `-r2`.

   The post-push verification passing is **not** evidence of this: it runs
   with the repository's `GITHUB_TOKEN` and can read a private package.
7. **Delete the pre-release notice in `README.md`** once the first release
   exists — the `<!-- GO-LIVE: ... -->` comment under `## Status` and the
   blockquote beginning `> **Status: pre-release.**` immediately below it.
   The comment is there to be found (`grep -n GO-LIVE README.md`) and says
   what to delete; removing it and leaving the blockquote is the mistake it
   exists to prevent, so delete both.
8. **Merge the open Dependabot pull requests**, each one only once CI is
   green on it. They were left open deliberately: a dependency bump that
   lands between the tag and the build is a change to what was tested.

   Three are open (`gh pr list --repo ESITC-Paris/samba-ad-dc`). #1
   (`actions/checkout` 5.1.0 → 7.0.1) and #2 (`go-ldap/ldap/v3` 3.4.13 →
   3.4.14) are green. #3 (`actions/setup-go` 5.6.0 → 7.0.0) is red, and
   only because its base commit predates `50126ad`: `setup-go` v6 began
   exporting `GOTOOLCHAIN=local`, and on that base `test/e2e/go.mod`
   declared `go 1.24.0` while `go.work` declares `go 1.25.0`, so the
   toolchain the action resolved was one language version below the
   workspace — `go: ../../go.work requires go >= 1.25.0 (running go
   1.24.0; GOTOOLCHAIN=local)` in both `Build` legs. `50126ad` raised the
   e2e module's floor to `1.25.0`; `@dependabot rebase` (or a manual
   rebase onto `main`) is the whole fix, and the run then goes green. Do
   not close it as broken and do not pin `setup-go` back — the bump is
   fine, the base is stale.
9. **Delete the staging package.** The drill left
   `ghcr.io/esitc-paris/samba-ad-dc-staging` behind (private, and it still
   exists at the time of writing). It is not deleted automatically and
   nothing reads it:

   ```sh
   gh api -X DELETE \
     /orgs/ESITC-Paris/packages/container/samba-ad-dc-staging
   ```

   That call needs `delete:packages` on the token `gh` is using — see
   *Token scopes for package work* below.
10. **Verify the published image the way a stranger would.** This has
    never been done. Every verification this repository has run happened
    inside GitHub Actions, with the run's own `GITHUB_TOKEN`; the drill's
    staging package was private and the laptop `gh` token carried no
    package scopes, so no local check ever ran. Until this step is taken,
    the verification instructions in `README.md` and
    `docs/deployment-guide.md` are untested against a real published tag —
    they are derived from the pipeline, not confirmed against it.

    From a shell logged out of everything, against the first published
    tag and the index digest its release notes pin:

    ```sh
    docker logout ghcr.io
    cosign version    # record this: it is what the result is evidence for

    cosign verify ghcr.io/esitc-paris/samba-ad-dc:<tag> \
      --certificate-identity-regexp 'https://github.com/ESITC-Paris/samba-ad-dc/.*' \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com

    gh attestation verify oci://ghcr.io/esitc-paris/samba-ad-dc@<digest> \
      --repo ESITC-Paris/samba-ad-dc
    ```

    Run them **verbatim as the README publishes them**, copied from that
    file rather than from here or from memory: what is under test is the
    published instruction, and a command a maintainer improved on the way
    past tests something no reader will ever type. If either behaves
    differently from what the guides describe, the guides are what is
    wrong — correct them, do not correct the transcript.

    The cosign version matters twice over. The pipeline signs and
    re-verifies with **v2.6.5**, while `brew install cosign` now gives v3;
    `README.md`, `docs/deployment-guide.md` §2.1 and the pin comment in
    `post-push-verify.yml` all say in so many words that a v3 verification
    is *expected* to work and has not been exercised. A v3 run of the
    commands above is that measurement. Record which version was used and
    replace the expectation with the result in all three places.

Nothing else changes state: a `publish` decision edits no tracked file by
design, so the watcher's own commit on that run is a state-only commit or
no commit at all.

**Expected shape of the whole thing.** Three `publish` decisions, three
tags, three `release.yml` runs, three GitHub Releases, three post-push
verifications. Measured against the 2026-09-17 drill, each release run is
about **20 minutes**: the two native legs run in parallel and took ~17 min
40 s each, with `Prepare` and `Publish` under a minute between them. The
150-minute per-leg budget is a ceiling for a slow runner, not an estimate.
The first publication will be slower than the drill by however long the
upgrade gates take, because those are the ones the drill could not run.

### Token scopes for package work

The workflows use the run's `GITHUB_TOKEN` and need nothing added. A
**laptop** `gh` token is a different matter: the default login scopes do
not include package access, so listing or deleting a package fails with a
403 that names the missing scope rather than a permission problem.

```sh
gh auth refresh -h github.com -s read:packages,delete:packages
```

That is the device-code flow — it prints a one-time code and opens
`github.com/login/device`. `read:packages` is what lists package versions
(and is what the watcher's probe uses from CI, through `GITHUB_TOKEN`);
`delete:packages` is needed only for the staging cleanup above and can be
dropped again afterwards with a second `gh auth refresh` naming the scopes
to keep.


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

**Drills.** Every rehearsal that has been run, newest first.

- **2026-09-17 — `v4.24.7-r1`, passed.**
  [Release run 35158262620](https://github.com/ESITC-Paris/samba-ad-dc/actions/runs/35158262620)
  (18 min: `Prepare` 7 s, both native legs ~17 min 40 s, `Publish` 37 s).
  Every §8 gate green on both architectures; `merge` created the five
  tags of `ghcr.io/esitc-paris/samba-ad-dc-staging` over two platform
  digests at index `sha256:1141895e45207bb3c3f3f05446f695bd4b26ab56731e6144b589393ef6b0f8e8`,
  signed it and attested it, and skipped the mirror, the release notes,
  the GitHub Release and the published-notification issue, while
  `prepare` skipped the §3.2 immutability check that makes a drill
  repeatable. The
  [post-push verification](https://github.com/ESITC-Paris/samba-ad-dc/actions/runs/35159720483)
  it triggered passed on the staging package it resolved from the run
  title's ` (staging)` suffix — which is a check run *inside* GitHub
  Actions with the repository's own `GITHUB_TOKEN`. **No artefact of this
  drill was ever verified from outside GitHub**: the staging package was
  private and the laptop `gh` token carried no package scopes, so the
  local check was refused before it could start. Step 10 of
  [Going live](#going-live-the-first-publication) is where that gap
  closes, and until it is taken the published verification instructions
  remain untested by anyone.
  Two gates did **not** run, for the documented first-publication reason
  rather than a defect: no `samba-ad-dc` package existed on GHCR yet, so
  `TestUpgradeFromLastPublished` skipped on both architectures (SPEC
  §8.3) and the cross-branch step was absent. The upgrade path is
  therefore the one part of the pipeline this drill did not exercise,
  and the first real publication is what will.
  **Left behind:** the `samba-ad-dc-staging` package still exists, and
  GHCR created it **private** — nobody chose that, which is the finding
  that put step 6 into [Going live](#going-live-the-first-publication).
  Deleting it is a maintainer action, not an automatic one, and it needs a
  `gh` token carrying `delete:packages`:
  `gh api -X DELETE /orgs/ESITC-Paris/packages/container/samba-ad-dc-staging`
  (see [Token scopes for package work](#token-scopes-for-package-work)).

### Post-push verification

`.github/workflows/post-push-verify.yml` (SPEC §8.5) runs automatically on
every successful `Release` run and re-reads the published image from the
registry with no access to anything the release produced. It publishes
nothing and repairs nothing: its output is a green run or an issue.

What it asserts: the manifest list publishes **exactly** the promised
platforms (set equality — an unpromised architecture is as much a defect
as a missing one); the index digest matches the one the release notes pin;
every platform image pulls by digest; `cosign verify` succeeds against an
identity under this repository and the GitHub Actions OIDC issuer (with
**cosign v2.6.5**, the same version `release.yml` signs with — a v3 CLI is
expected to verify these signatures too, but neither workflow has
exercised that); `gh attestation verify --repo` accepts the provenance
over the index digest;
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

Then seed `.build-state.json` for the new branch, in the same commit, with
the values the new block pins:

```sh
# The three digests are the ones you just pinned in the new branch block —
# copy each `base.*` ref's part after the `@`.
python3 scripts/catalog.py state set 4.25 runtime_digest sha256:<runtime>
python3 scripts/catalog.py state set 4.25 builder_digest sha256:<builder>
python3 scripts/catalog.py state set 4.25 gobuild_digest sha256:<gobuild>
# The closure hash is measured, not copied: the same script the probe runs.
python3 scripts/catalog.py state set 4.25 pkg_index_hash \
  "$(sh scripts/pkg-closure-hash.sh debian:trixie-slim@sha256:<runtime>)"
```

`decide()` already treats a branch with no state entry **whose tag is not
yet published** as "no prior observation" — it records what it sees
instead of reading it as a moved base digest, so a newly added branch
reaches the `publish` self-heal and gets its r1 rather than a spurious
`-r2`. That quiet path stops at the first publication on purpose: once the
branch's tag is on the registry, an empty state entry is compared like any
other and earns the branch a `revision`, because recording the observation
quietly there would re-pin `base.*` and `pkg_index_hash` in `versions.yaml`
under `action: none` — the catalog would claim a base the published image
was never built from, and the rebuild that cycle owed would be lost.
Seeding is the belt to that brace: it makes the first run's recorded values
reviewable in the pull request that adds the series, instead of arriving in
a bot commit an hour later, and it is what keeps a branch added after its
first publication (a re-seed, a hand-edited state file) from taking an
unnecessary `-r2`.

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
