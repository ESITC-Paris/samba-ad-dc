# Phase 4 — Release Automation Implementation Plan (catalog, watcher, publishing)

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps
> use checkbox (`- [ ]`) syntax for tracking.
>
> This plan SUPERSEDES `2026-08-16-phase-4-publishing.md` and absorbs
> roadmap Phase 6 (the watcher) and the catalog half of Phase 7. Roadmap
> decision **D4 (separate watcher repository) is REVERSED** by maintainer
> instruction on 2026-09-16: the release cycle follows exactly the model of
> `github.com/ESITC-Paris/unbound-distroless` — an in-repository
> `upstream-check.yml` workflow, dispatched hourly by an external cron on
> ESITC infrastructure, that verifies, pins, commits, tags and dispatches
> `release.yml`, which publishes only after every gate is green on both
> architectures. A clone of that reference repository is available for
> implementers at the path given in each task brief; copy its *shapes*
> (job layout, action pins, guard comments), never its Unbound specifics.

**Goal:** A merge-free, fully automatic release cycle: every supported
Samba stable branch is in the catalog; an hourly check detects a new
upstream patch release, a base-image digest change or a runtime-package
delta, verifies it against the vendored key, commits the new pins with a
`vX.Y.Z-rN` git tag, and dispatches a release that rebuilds natively on
amd64 and arm64 with ALL gates (lint, unit, build, secrets, CVE, full
E2E, upgrade test), publishes the multi-arch manifest to GHCR (source of
truth) and Docker Hub (mirror), signs it with cosign keyless, attaches
SBOM and SLSA provenance, creates the GitHub Release, and is then
re-verified post-push. A failed gate publishes nothing and opens an issue
assigned to the maintainer.

**Architecture:** `versions.yaml` (one entry per branch) is the pin
contract and `.build-state.json` (one entry per branch) the watcher's
memory. One Python tool, `scripts/catalog.py`, is the single reader and
writer of both files and of everything derived from them (tags, aliases,
release notes, README compatibility matrix, CHANGELOG entries); every
workflow calls it instead of parsing YAML in shell. `scripts/watch.py`
holds the watcher's decision logic as pure, unit-tested functions; the
`upstream-check.yml` workflow is its thin shell. `release.yml` follows
the unbound-distroless three-job shape (`prepare` → native `build`
matrix → `merge`), extended with the SPEC-specific gates (E2E suite,
upgrade test, package closure, labels, degraded modes). `ci.yml` gains
the same secrets and CVE gates so pull requests are held to the same bar.
`post-push-verify.yml` (§8.5) pulls the published digests and verifies
signature and attestations.

**Tech Stack:** GitHub Actions on native runners (`ubuntu-24.04`,
`ubuntu-24.04-arm`), docker buildx (`push-by-digest`, `imagetools`),
cosign v2 keyless (GitHub OIDC), BuildKit SBOM + provenance attestations,
`actions/attest-build-provenance`, trivy (vuln + secret scanners),
gitleaks, Python 3 (stdlib + PyYAML, preinstalled on runners), `gh` CLI.

**Spec:** `SPEC.md` v1.2 §§3, 4.1–4.6, 5.3, 5.4, 6.6, 8.3–8.5, 9, 9bis,
10.4; Annex B / `docs/adaptation-profile.md` (authoritative). Roadmap:
`2026-08-16-samba-ad-dc-roadmap.md` Phases 4, 6, 7 (with D4 reversed).

## Global Constraints

- Registries: GHCR `ghcr.io/esitc-paris/samba-ad-dc` is the source of
  truth; Docker Hub `docker.io/esitcparis/samba-ad-dc` is the mirror
  (§4.6). The image name is `${{ vars.IMAGE_NAME || 'samba-ad-dc' }}` in
  every workflow so a staging drill can retarget it.
- Image tags: `X.Y.Z-rN` immutable primary; aliases `X.Y.Z`, `X.Y`
  always; `X` and `latest` ONLY when the branch is `default_branch`
  (§3.1, §3.3, §3.6). An existing `X.Y.Z-rN` on GHCR aborts the release
  with the message `tag <t> already exists on <registry>; bump revision
  (-rN) in versions.yaml` (§3.2). Revision numbering starts at **1** for
  every new upstream version (the catalog already uses `revision: 1`).
- Git tags are `vX.Y.Z-rN`; the branch entry is `X.Y` of the tag.
- Catalog: every upstream-supported stable branch is present in
  `versions.yaml` (§3.6, B.7). On 2026-09-16 those are `4.22` (latest
  4.22.11, security-fixes-only), `4.23` (4.23.12, maintenance), `4.24`
  (4.24.7, current) — `4.25.0rc2` exists under `pub/samba/rc/`, so the
  deprecation of `4.22` is pending (§9.4).
- Every pin is a digest or a hash; nothing resolves `latest` at build
  time (§4.1, §4.4). Base refs in `versions.yaml` carry `@sha256:`.
- Upstream artifacts are verified fail-closed: `samba-X.Y.Z.tar.asc`
  over the **gunzipped** tar, against `keys/samba-release-key.asc`,
  asserting `VALIDSIG` on fingerprint
  `81F5E2832BD2545A1897B713AA99442FB680B620` and rejecting
  `EXPKEYSIG|REVKEYSIG|ERRSIG|BADSIG` — exactly the Dockerfile's
  procedure — BEFORE any sha256 is recorded (§4.4, §9bis.8.a).
- Necessity criterion (§9bis.8): a release is dispatched only when a
  build input changed — upstream version, base digest (runtime, builder,
  gobuild), or the runtime-package closure hash — or when the catalog's
  current `X.Y.Z-rN` has never been published (first publication /
  missed dispatch self-heal). Index republication with no version delta
  triggers nothing.
- Soak (§9bis.5): a NEW upstream version is released only after
  `RELEASE_SOAK_HOURS` (repo variable, default **24**) have elapsed since
  the watcher first saw it; revision rebuilds have no soak; the dispatch
  input `security_release=true` sets the soak to 0 for that run.
- Watcher scheduling (§9bis.7): `upstream-check.yml` has ONLY
  `workflow_dispatch`; no `schedule:`. Hourly dispatch comes from an
  external cron (documented in `docs/operations.md`).
- Watcher politeness and idempotency (§9bis.2, §9bis.3): conditional
  requests where the source supports them, `--retry 3`, per-branch state
  persisted in `.build-state.json` and committed with the bump; the same
  event never dispatches twice.
- New upstream series (minor bump, e.g. `4.25.0` GA) and branch removal
  (§9bis.4 human approval, §9.4 relay): the watcher never edits the
  catalog for these; it opens ONE deduplicated issue assigned to the
  maintainer (`euca01`) with the ready-to-paste `versions.yaml` entry
  and the lifecycle consequence, and it updates the README compatibility
  matrix status column in the same commit as any bump.
- CVE gate (§5.4): `trivy image --scanners vuln --ignore-unfixed
  --severity HIGH,CRITICAL --exit-code 1`; the full unfiltered JSON
  report is always uploaded as an artifact; `security/cve-exceptions.yaml`
  entries with a past `review_by` block the build.
- Secrets gate (§5.3): gitleaks over the repository (full history) and
  `trivy image --scanners secret --exit-code 1` over the built image.
- Multi-arch (§6.6): both arches build and run the FULL gate set on
  native runners; QEMU appears only in the §9.6 `emulated` degraded
  mode, and every activation of a degraded mode is disclosed in the
  release notes and the run summary.
- Upgrade test (§8.3): `release.yml` resolves the last published tag of
  the branch (GHCR) into `E2E_UPGRADE_FROM`; when a previous branch
  (`X.(Y-1)`) is published it ALSO runs `TestUpgradeFromLastPublished`
  once more with the previous branch's latest tag (the documented
  cross-branch path, B.6). Absent tags skip loudly (first release).
- Publishing credentials live only in GitHub Actions (§4.5): GHCR via
  `GITHUB_TOKEN` (`packages: write`), Docker Hub via repository secrets
  `DOCKERHUB_USERNAME` / `DOCKERHUB_TOKEN`; when those secrets are
  absent the mirror steps are SKIPPED with a `::warning::` naming them,
  never failed silently and never faked.
- Notifications: failures and publications open/comment issues assigned
  to `euca01`, deduplicated by title (unbound-distroless shape).
- Every third-party action is pinned to a full commit SHA with the tag
  in a trailing comment; the pins to use are in
  `.superpowers/sdd/action-pins-reference.txt` (already verified in the
  reference repository). Existing pins in `ci.yml` (`actions/checkout`,
  `actions/setup-go`) stay as they are unless a task says otherwise.
- All emitted text (commits, notes, issues, notices) is English. Commit
  messages follow the repository's conventional style and trailers
  (`git log -3 --format=%B`).
- Lint gates stay green: `hadolint`, `shellcheck`, `yamllint --strict`,
  `actionlint`, `gofmt`/`go vet`, `scripts/check-traceability.sh`,
  `scripts/check-pins-consistency.sh`. Python tooling is stdlib +
  PyYAML only, formatted so `python3 -m py_compile` and the unittest
  suite pass; no third-party pip packages.
- The local docker daemon is the test bed for everything that can be
  tested locally (image builds, E2E, scanners, catalog tooling). Disk is
  tight (~14 GB free): never `docker system prune`; remove only images
  and volumes you created; keep `samba-ad-dc:dev` and
  `samba-ad-dc-e2e-client:dev`.

---

## File structure (locked)

```
versions.yaml                      # + branches 4.22, 4.23; 4.24 -> 4.24.7; refreshed base digests
.build-state.json                  # NEW: per-branch watcher memory (see Task 4)
security/cve-exceptions.yaml       # NEW: §5.4 ledger (empty list + schema comment)
scripts/catalog.py                 # NEW: catalog/state reader+writer, tags, notes, matrix, changelog
scripts/catalog_test.py            # NEW: unittest
scripts/watch.py                   # NEW: watcher decision logic + upstream/base/pkg probes
scripts/watch_test.py              # NEW: unittest (pure functions; probes mocked)
scripts/check-cve-exceptions.py    # NEW: review_by expiry gate
scripts/cve-ledger.py              # NEW (Task 4): syncs the exceptions ledger from a trivy JSON report, per package
scripts/pkg-closure-hash.sh        # NEW (Task 3b): THE package-closure hash definition (upgrade ∪ install dry-runs)
.gitleaksignore                    # NEW (Task 3b): allowlist for the public key fingerprint in keys/PINNING.md
scripts/verify-upstream-tarball.sh # NEW: download+gpg+sha256 for one version (shared by watcher and Task 2)
scripts/check-pins-consistency.sh  # unchanged semantics (default branch ↔ Dockerfile ARG defaults)
.github/workflows/ci.yml           # + gitleaks, trivy vuln+secret, cve-exceptions gate, dispatch, concurrency
.github/workflows/upstream-check.yml   # NEW: the watcher shell
.github/workflows/release.yml          # NEW
.github/workflows/post-push-verify.yml # NEW
docs/operations.md                 # NEW: scheduling, supervision, manual operations, secrets to configure
docs/adaptation-profile.md         # amendments (D4 reversal, watcher model, catalog, cross-branch upgrade)
docs/superpowers/plans/2026-08-16-samba-ad-dc-roadmap.md  # D4 marked REVERSED with pointer here
CHANGELOG.md                       # format note; entries written by the watcher at bump time
README.md                          # compatibility matrix block between markers (content rendered by catalog.py)
```

---

### Task 1: Catalog tooling (`scripts/catalog.py`) and CVE-exception gate

**Files:**
- Create: `scripts/catalog.py`, `scripts/catalog_test.py`,
  `scripts/check-cve-exceptions.py`, `security/cve-exceptions.yaml`
- Modify: `CHANGELOG.md` (format note only), `README.md` (add the two
  matrix marker comments around the existing empty table)

**Interfaces (produced; every later task calls these exactly):**

```
python3 scripts/catalog.py get <branch> <key>          # samba_version|revision|tarball_sha256|base.builder|base.runtime|base.gobuild|pkg_index_hash
python3 scripts/catalog.py branches                    # one branch per line, ascending (4.22 4.23 4.24)
python3 scripts/catalog.py default                     # default_branch
python3 scripts/catalog.py tag <branch>                # X.Y.Z-rN
python3 scripts/catalog.py git-tag <branch>            # vX.Y.Z-rN
python3 scripts/catalog.py aliases <branch>            # one image tag per line: X.Y.Z-rN X.Y.Z X.Y [X latest]
python3 scripts/catalog.py branch-of-tag vX.Y.Z-rN     # X.Y ; exit 2 when the tag does not match the catalog entry's version/revision
python3 scripts/catalog.py set <branch> <key> <value>  # in-place edit preserving comments/order (targeted text substitution, then re-parse to assert)
python3 scripts/catalog.py bump-version <branch> <X.Y.Z> <sha256>   # samba_version+tarball_sha256, revision=1
python3 scripts/catalog.py bump-revision <branch>      # revision+1
python3 scripts/catalog.py state get <branch> <key>    # .build-state.json: runtime_digest|builder_digest|gobuild_digest|pkg_index_hash|published_tag|pending.version|pending.first_seen
python3 scripts/catalog.py state set <branch> <key> <value>
python3 scripts/catalog.py state clear-pending <branch>
python3 scripts/catalog.py render-matrix [--rc-series X.Y]  # markdown table body rows for README (see below)
python3 scripts/catalog.py update-readme-matrix [--rc-series X.Y]  # rewrites the block between <!-- matrix:start --> and <!-- matrix:end --> in README.md
python3 scripts/catalog.py changelog-entry <branch> <cause> [--notes "..."]  # prepends an entry to CHANGELOG.md; refuses a duplicate tag (exit 3)
python3 scripts/catalog.py release-notes <branch> --cause <c> [--degraded none|emulated|staggered --pending-arch <a>] --digest-ghcr <d> [--digest-hub <d>]  # markdown body for the GitHub Release
```

Lifecycle status column (`render-matrix`): branches sorted descending;
newest = `current`, next = `maintenance`, third = `security fixes only`,
any older = `discontinued (EOL)`; if `--rc-series` is given and is
greater than the newest catalog branch, the OLDEST supported branch's
status gets the suffix ` — deprecation pending (X.Y rc published)`.
Matrix columns: `Upstream branch | Latest image tag | Aliases | Upstream
support status`.

`.build-state.json` shape:

```json
{
  "branches": {
    "4.24": {
      "runtime_digest": "sha256:…", "builder_digest": "sha256:…", "gobuild_digest": "sha256:…",
      "pkg_index_hash": "…", "published_tag": "4.24.7-r1",
      "pending": {"version": "4.24.8", "first_seen": "2026-09-16T17:00:00Z"}
    }
  }
}
```

`published_tag` is the last `X.Y.Z-rN` the watcher dispatched a release
for (Task 4 sets it at dispatch; Task 5's release verifies it exists).
`pending` is absent when nothing is soaking.

CHANGELOG entry format (prepended under the `# Changelog` header and its
intro paragraph, newest first):

```
## 4.24.7-r1 — 2026-09-16

- Samba: 4.24.7 (branch 4.24)
- Trigger: samba-release
- Image changes: <notes or "none">
- Fixed CVEs: see the GitHub Release
- Digests, signature and attestations: https://github.com/esitc-paris/samba-ad-dc/releases/tag/v4.24.7-r1
```

Causes are exactly `samba-release | pkg-update | base-digest | manual |
first-publication` (§10.4 plus the two this plan adds).

`security/cve-exceptions.yaml`:

```yaml
# Unfixable-CVE ledger (SPEC §5.4). One entry per CVE WITHOUT an available
# fixed version; the build gate ignores unfixed CVEs already, so this file
# is documentation and a review clock, not a suppression list.
#   - cve: CVE-YYYY-NNNNN
#     component: <package or bundled component>
#     branches: ["4.24"]          # catalog branches affected
#     reason: <why no fix is available / why it does not apply>
#     introduced: <image tag first shipped with it>
#     review_by: YYYY-MM-DD       # scripts/check-cve-exceptions.py fails CI when this date has passed
exceptions: []
```

- [ ] **Step 1: Write `scripts/catalog_test.py` first** (unittest, uses a temp copy of a fixture `versions.yaml` and `.build-state.json` written by the test): cases for `tag`/`git-tag`/`aliases` (default vs non-default branch), `branch-of-tag` (match, version mismatch → exit 2, revision mismatch → exit 2, bad format → exit 2), `bump-version` resets revision to 1, `bump-revision`, `set` preserves comments and other keys byte-for-byte outside the edited line, `render-matrix` statuses for 3 and 4 branches with and without `--rc-series`, `changelog-entry` prepend + duplicate refusal (exit 3), `release-notes` contains the tag, both digests, the cosign verify command with `--certificate-identity-regexp 'https://github.com/ESITC-Paris/samba-ad-dc/.*'` and `--certificate-oidc-issuer https://token.actions.githubusercontent.com`, and the degraded-mode disclosure paragraph only when `--degraded` is not `none`.
- [ ] **Step 2: Run** `python3 -m unittest scripts/catalog_test.py -v` → fails (module missing).
- [ ] **Step 3: Implement `scripts/catalog.py`** (argparse subcommands; `import yaml` guarded with `sys.exit("PyYAML is required: apt install python3-yaml / pip install pyyaml")`; edits to `versions.yaml` are line-targeted regex substitutions within the branch block followed by a re-parse assertion so comments survive; `.build-state.json` is rewritten whole with `indent=2` and a trailing newline).
- [ ] **Step 4: Run the tests** → all pass. Also `python3 scripts/catalog.py tag 4.24` on the real catalog prints `4.24.6-r1`.
- [ ] **Step 5: `scripts/check-cve-exceptions.py`** — reads the ledger, exits 1 listing every entry whose `review_by < today` (UTC), exits 0 with `cve exceptions: N entr(y|ies), none expired` otherwise; add 3 unittest cases in `scripts/catalog_test.py` (past, today, future) by importing it as a module.
- [ ] **Step 6: README markers** — wrap the existing compatibility-matrix table in `<!-- matrix:start -->` / `<!-- matrix:end -->` lines and run `python3 scripts/catalog.py update-readme-matrix` so the table reflects the catalog. `CHANGELOG.md`: replace `*No releases yet.*` with one sentence saying entries are written automatically by the release watcher at bump time and that digests live in GitHub Releases; keep the §10.4 sentence.
- [ ] **Step 7: Lints** — `shellcheck` untouched; `python3 -m py_compile scripts/*.py`; `pipx run yamllint==1.38.0 --strict .` (or note if pipx is absent).
- [ ] **Step 8: Commit** `feat(release): catalog tooling, CVE-exception ledger and gate`.

---

### Task 2: Catalog activation — three branches, current pins, local proof

**Files:**
- Create: `scripts/verify-upstream-tarball.sh`
- Modify: `versions.yaml`, `Dockerfile` (ARG defaults only, to stay in
  sync with the default branch), `.build-state.json` (create via
  `catalog.py state set`), `docs/adaptation-profile.md` (B.6/B.7 notes),
  `test/client/Dockerfile` (base digest comment/pin)

**Interfaces:**
- Consumes: Task 1 (`catalog.py set/bump-version/state set/update-readme-matrix`).
- Produces: `sh scripts/verify-upstream-tarball.sh <X.Y.Z> [outdir]` →
  downloads `samba-X.Y.Z.tar.gz` and `.tar.asc` from
  `https://download.samba.org/pub/samba/stable/`, verifies exactly as the
  Dockerfile does (gunzip, gpg status file, `VALIDSIG` on the pinned
  fingerprint, reject `EXPKEYSIG|REVKEYSIG|ERRSIG|BADSIG`), prints
  `sha256=<hex>` on stdout on success, exits 1 otherwise; uses a throwaway
  `GNUPGHOME`; `--retry 3 --retry-delay 5`.

- [ ] **Step 1: Write `scripts/verify-upstream-tarball.sh`** (POSIX sh, shellcheck-clean, fingerprint constant at the top mirroring `keys/PINNING.md`). Run it for `4.24.7`, `4.23.12`, `4.22.11`; record the three sha256 values.
- [ ] **Step 2: Refresh base digests**: `docker buildx imagetools inspect debian:trixie-slim --format '{{println .Manifest.Digest}}' | head -1` (currently `sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132`) and the same for `golang:1.24-trixie`. Update `versions.yaml` for ALL branches (`base.builder`, `base.runtime`, `base.gobuild`) and the Dockerfile `ARG BUILDER_BASE/RUNTIME_BASE/GOBUILD_BASE/BASE_DIGEST` defaults, and the `test/client/Dockerfile` FROM digest. `sh scripts/check-pins-consistency.sh` must pass.
- [ ] **Step 3: Catalog entries**: `catalog.py bump-version 4.24 4.24.7 <sha>`; add `4.23` (4.23.12) and `4.22` (4.22.11) entries with `revision: 1`, the same base digests and `pkg_index_hash: "bootstrap"` — add them by editing `versions.yaml` by hand in the same shape as `4.24`, with a comment line per branch stating its upstream lifecycle status on 2026-09-16 (current / maintenance / security fixes only) and that status is relayed by the watcher. Update Dockerfile `ARG SAMBA_VERSION` (all three stages) and `ARG SAMBA_TARBALL_SHA256` to 4.24.7. `check-pins-consistency.sh` green.
- [ ] **Step 4: `.build-state.json`**: for each branch `state set` `runtime_digest`, `builder_digest`, `gobuild_digest` to the refreshed digests and `pkg_index_hash` to the value Task 4's probe will compute — compute it now with the same command Task 4 specifies (see Task 4 Step 3 "pkg closure hash") and store it; `published_tag` unset (nothing published yet).
- [ ] **Step 5: Build the default branch locally**: `docker build --build-arg PKG_INDEX_HASH=$(python3 scripts/catalog.py get 4.24 pkg_index_hash) -t samba-ad-dc:dev .` (replaces the 4.24.6 image; ~40 min, background). Then `cd test/e2e && E2E_IMAGE=samba-ad-dc:dev E2E_CLIENT_IMAGE=samba-ad-dc-e2e-client:dev go test ./... -v -count=1 -timeout 45m` → 21 pass, 1 skip.
- [ ] **Step 6: Build branch 4.23 locally** with explicit build args: `docker build --build-arg SAMBA_VERSION=4.23.12 --build-arg SAMBA_TARBALL_SHA256=<sha> --build-arg PKG_INDEX_HASH=<hash> -t samba-ad-dc:4.23-dev .`. If `./configure` rejects an option on 4.23 or 4.22, make the Dockerfile handle it with a version-conditional shell expression **only for the option that differs**, documented in the Dockerfile comment with the observed configure error; do not fork the Dockerfile. Run the full E2E suite against `samba-ad-dc:4.23-dev` (same command, `E2E_IMAGE=samba-ad-dc:4.23-dev`) → 21 pass, 1 skip.
- [ ] **Step 7: Cross-branch upgrade proof**: `E2E_IMAGE=samba-ad-dc:dev E2E_UPGRADE_FROM=samba-ad-dc:4.23-dev go test ./... -run 'TestUpgradeFromLastPublished$' -v -count=1 -timeout 30m` → PASS (state written by 4.23.12 starts cleanly on 4.24.7 with data intact). Record the runtime.
- [ ] **Step 8: Build branch 4.22 locally** (`samba-ad-dc:4.22-dev`) and run the E2E **smoke subset** `-run 'TestProvision$|TestKerberosKinit|TestIdempotentRestart|TestDBConsistency'` (full suite optional if time allows; state which). Then remove `samba-ad-dc:4.22-dev` (disk) but KEEP `samba-ad-dc:4.23-dev` for Task 5's local dry-run of the cross-branch step.
- [ ] **Step 9: Profile amendments** (`docs/adaptation-profile.md`): B.7 lists the three branches with their 2026-09-16 status and the rule for the matrix; B.6 "Cross-branch upgrade tests" entry becomes: intra-branch on every release, previous-branch→current on every release of the current branch (Step 7 evidence), further paths unsupported; profile changelog entry dated 2026-09-16.
- [ ] **Step 10: Commit** in two commits: `feat(catalog): activate branches 4.22, 4.23, 4.24 at current upstream patch levels; refresh base digests` (versions.yaml, Dockerfile, client Dockerfile, .build-state.json, script) and `docs(profile): catalog and cross-branch upgrade evidence`.

---

### Task 3: Secrets and CVE gates in `ci.yml`

**Files:**
- Modify: `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: Task 1 `scripts/check-cve-exceptions.py`.
- Produces: the exact gate step block that Task 5 copies verbatim into
  `release.yml` (keep it as one contiguous, clearly delimited group of
  steps titled `# --- security gates (SPEC §5.3, §5.4) ---`).

- [ ] **Step 1: `on:` gains `workflow_dispatch:`**; `concurrency.cancel-in-progress` becomes `${{ github.event_name == 'pull_request' }}` with the comment from the superseded plan (a push must not cancel a main-branch run once releases hang off main).
- [ ] **Step 2: gitleaks in the `lint` job** — `docker run --rm -v "${PWD}:/repo:ro" ghcr.io/gitleaks/gitleaks:<tag>@<digest> detect --source /repo --no-banner --redact` (resolve the current tag's digest with `docker buildx imagetools inspect`, pin both). Full history is scanned (`actions/checkout` with `fetch-depth: 0` for this job).
- [ ] **Step 3: trivy in the `build` job**, after "Package closure check": (a) full unfiltered report `trivy image --format json --output trivy-full-${{ matrix.arch }}.json samba-ad-dc:ci` uploaded with `actions/upload-artifact` (pin from the reference file) `if: always()`, retention 90 days; (b) gate `trivy image --scanners vuln --ignore-unfixed --severity HIGH,CRITICAL --exit-code 1 samba-ad-dc:ci`; (c) `trivy image --scanners secret --exit-code 1 samba-ad-dc:ci`; (d) `python3 scripts/check-cve-exceptions.py`. Use the `aquasecurity/trivy-action` pin from the reference file for (b) and (c), or install the trivy binary once and call it — one approach, stated in a comment. Cache the trivy DB with `cache: true` where the action offers it.
- [ ] **Step 4: Local proof** — run (b), (c), (d) with the local trivy 0.71.2 against `samba-ad-dc:dev`; paste the tail of each output in the report. A fixable HIGH/CRITICAL finding is real work: check whether a newer base digest or a Debian update fixes it, do NOT suppress; report it as a concern if unresolved.
- [ ] **Step 5: Lints** — `actionlint` via the pinned container (`docker run --rm -v "${PWD}:/repo:ro" -w /repo rhysd/actionlint:1.7.12@sha256:b1934ee5f1c509618f2508e6eb47ee0d3520686341fec936f3b79331f9315667 -color`) and yamllint.
- [ ] **Step 6: Commit** `ci: blocking secrets and fixable-CVE gates on both architectures`.

---

### Task 3b: Clear the fixable-CVE gate and make base-package updates a build input

Added 2026-09-16 after Task 3 measured the §5.4 gate RED on the 4.24.7
image: 41 fixable HIGH/CRITICAL findings (3 CRITICAL). Three root causes:
(1) packages inherited from `debian:trixie-slim` (gzip, perl-base,
libsqlite3-0, libpcre2-8-0, …) are never upgraded — the runtime stage
only `apt-get install`s the manifest, so a Debian security update to a
base package reaches the image only when Debian republishes the base
image; (2) the Go toolchain `golang:1.24-trixie` carries stdlib CVEs fixed
in Go ≥ 1.25.13; (3) `golang.org/x/crypto` (transitive dependency of
go-ldap) needs ≥ 0.52.0.

**Files:**
- Modify: `Dockerfile` (runtime apt layer; `GOBUILD_BASE` ARG), `versions.yaml` (`base.gobuild` for all branches; `pkg_index_hash` regenerated), `.build-state.json` (hash regenerated), `entrypoint/go.mod` + `go.sum`, `scripts/check-image-packages.sh` (accept upgraded base packages — it compares names, verify), `docs/adaptation-profile.md` (B.8), `.github/workflows/ci.yml` (gitleaks allowlist moved out), `.gitleaksignore` (new, root).
- Create: `scripts/pkg-closure-hash.sh` — THE definition of the package-closure hash, used by Task 2's seed, Task 4's probe and any local check.

**Interfaces:**
- `sh scripts/pkg-closure-hash.sh <runtime-base-ref@digest> [manifest]` → prints the 16-hex hash. Definition: inside the base image, `apt-get update -qq`, then the `^Inst ` lines of `apt-get -y --dry-run upgrade` UNION the `^Inst ` lines of `apt-get -y --no-install-recommends --dry-run install <manifest>`, rendered as `<name> <version>` (parentheses stripped), sorted unique, `sha256sum`, first 16 hex. Both dry-runs are needed: `install` alone never lists an upgrade of an already-installed base package, and a security fix to such a package MUST be a build input (§9bis.1.c) even when the base digest has not moved.

- [ ] **Step 1: Dockerfile runtime layer.** In the single `RUN echo "pkg-index=${PKG_INDEX_HASH}" && apt-get update && …` add `apt-get -y upgrade` (no `dist-upgrade`: no new packages, no removals — upgrade is what the hash's first dry-run models) BEFORE the manifest install, in the same RUN, with a WHY comment naming the three packages that motivated it and the §9bis.1.c reasoning. Builder stage: leave as is (build-time only; its CVEs never ship).
- [ ] **Step 2: Go toolchain.** Resolve `golang:1.25-trixie` digest (`docker buildx imagetools inspect golang:1.25-trixie --format '{{println .Manifest.Digest}}' | head -1`); set it in `versions.yaml` `base.gobuild` for all three branches and the Dockerfile `ARG GOBUILD_BASE` default + its comment; `cd entrypoint && go get golang.org/x/crypto@latest && go mod tidy` (keep `go 1.24.0` in go.mod unless tidy requires more — if it does, say so); `gofmt -l . && go vet ./... && go test -race ./...` green. `check-pins-consistency.sh` green.
- [ ] **Step 3: `scripts/pkg-closure-hash.sh`** (POSIX, shellcheck-clean) per the interface; run it against the pinned runtime digest; write the value into `versions.yaml` `pkg_index_hash` (all branches) and `.build-state.json` (all branches) with `catalog.py`.
- [ ] **Step 4: `.gitleaksignore`** at the repo root carrying the `keys/PINNING.md` fingerprint finding (gitleaks fingerprint format), with a comment line explaining it is a public key fingerprint; remove the inline allowlist from `ci.yml` so the step is a plain `detect`.
- [ ] **Step 5: Rebuild and prove.** `docker build --build-arg PKG_INDEX_HASH=$(python3 scripts/catalog.py get 4.24 pkg_index_hash) -t samba-ad-dc:dev .` then: `trivy image --scanners vuln --ignore-unfixed --severity HIGH,CRITICAL --exit-code 1 samba-ad-dc:dev` → exit 0 (paste the summary); `trivy image --scanners secret --exit-code 1` → 0; `sh scripts/check-image-packages.sh samba-ad-dc:dev` → OK (if it fails because upgraded base packages differ in version, the script compares names — investigate and fix the script, never the check's meaning); gitleaks via the pinned container → 0 findings; full E2E on the rebuilt image → 17 pass, 1 skip. Rebuild `samba-ad-dc:4.23-dev` the same way (its build args from `catalog.py get 4.23 …`) and run the smoke subset + `TestLDAPSCertificate` once; keep it.
- [ ] **Step 6: B.8** (reproducibility) gains a paragraph: the runtime layer upgrades the base's own packages to the state of the package index at build time; the SBOM records resolved versions; the closure hash covers both the manifest closure and the base-package upgrades, so "same commit + same base digest + same hash ⇒ same content" still holds. Profile changelog entry.
- [ ] **Step 7: Commit** `fix(image): upgrade base packages in the runtime layer, Go 1.25 toolchain, x/crypto bump — clears the fixable-CVE gate` and `feat(watch): package-closure hash covers base-package upgrades`.

---

### Task 4: The watcher — `scripts/watch.py` and `upstream-check.yml`

**Files:**
- Create: `scripts/watch.py`, `scripts/watch_test.py`,
  `.github/workflows/upstream-check.yml`
- Modify: `docs/adaptation-profile.md` (new section "Release cycle"),
  `README.md` (badge line for the three workflows)

**Interfaces:**
- Consumes: Task 1 (`catalog.py` everything), Task 2 (`verify-upstream-tarball.sh`, `.build-state.json`).
- Produces: `python3 scripts/watch.py plan --now <ISO> --soak-hours <h> [--security] --observe <json>` → JSON list of decisions; `python3 scripts/watch.py observe` → the observation JSON (network); `python3 scripts/watch.py apply <decision-json>` → edits catalog/state/CHANGELOG/README, prints the git tag(s) to create and the branches to dispatch.

**Decision logic (pure, in `watch.py`, table-tested):**

For each catalog branch `X.Y` with observation
`{latest_patch, runtime_digest, builder_digest, gobuild_digest, pkg_index_hash, published_tags:[…]}`:

1. `latest_patch` = highest `X.Y.Z` in the stable directory listing for
   that `X.Y` (regex `samba-X\.Y\.(\d+)\.tar\.gz`, so rc's never match).
   Monotonic guard: consider it only if strictly greater than the
   catalog's `samba_version` (`sort -V` semantics via tuple compare).
2. If greater: if `pending.version != latest_patch` → set pending
   `{version, first_seen=now}` and stop for this branch (soaking);
   else if `now - first_seen >= soak` (soak 0 when `--security`) →
   **action=version** (`cause=samba-release`); else stop (still soaking;
   log remaining time).
3. Else if any of the three base digests differs from state →
   **action=revision** (`cause=base-digest`).
4. Else if `pkg_index_hash` differs from state → **action=revision**
   (`cause=pkg-update`).
5. Else if the catalog's current `X.Y.Z-rN` is not in `published_tags`
   → **action=publish** (`cause=first-publication`, no catalog edit,
   tag created if missing) — the self-heal for first publication and
   for a lost dispatch.
6. Else **action=none**.

Series detection (once per run, not per branch): the highest `X.Y` in
the stable listing that is not a catalog branch → decision
`new_series` (issue). The highest `X.Y` under `pub/samba/rc/` greater
than the newest catalog branch → `rc_series` (matrix suffix +
deprecation-pending issue for the oldest branch). Both issues are
deduplicated by title (`New upstream series X.Y — catalog decision
required`, `Deprecation pending: branch X.Y (X.Y' rc published)`).

**Observation probes (in `watch.py observe`, thin and retried):**
- Stable listing: `https://download.samba.org/pub/samba/stable/` with
  `If-Modified-Since`/`ETag` stored in state under `sources.stable`
  (304 → reuse the cached version list stored in state).
- RC listing: same for `pub/samba/rc/`.
- Base digests: `docker buildx imagetools inspect <ref-without-digest>
  --format '{{println .Manifest.Digest}}' | head -1`, 3 attempts, empty
  → hard failure (never write an empty digest).
- pkg closure hash: `sh scripts/pkg-closure-hash.sh <base.runtime ref>`
  (Task 3b owns the definition: upgrade dry-run ∪ install dry-run).
  Computed on the runner's own architecture (amd64) and documented as
  such in the profile (Debian stable versions are architecture-uniform
  except binNMUs; a binNMU-only delta is caught by the base digest).
- Published tags: `gh api "/orgs/esitc-paris/packages/container/${IMAGE_NAME}/versions" --paginate --jq '.[].metadata.container.tags[]'`
  (404/empty → no tags; requires `packages: read`).

**CVE advisory (§9bis.1.d, §9bis.8.c — advisory only, never a
dispatch):** once per run, for every branch with a `published_tag`,
fetch the published image's SBOM (`docker buildx imagetools inspect
$GHCR_IMAGE:<published_tag> --format '{{json .SBOM}}'` → write the
`SPDX` document to a file) and run `trivy sbom --severity HIGH,CRITICAL
--format json` on it. The ledger `security/cve-exceptions.yaml` is then
SYNCHRONISED by tooling, not by hand (ruling R-ledger, 2026-09-16: the
4.24.7 image carries ~288 unfixed CVEs, 77 HIGH/CRITICAL — one entry per
CVE is not maintainable): `python3 scripts/cve-ledger.py sync <branch>
<trivy-json>` rewrites the branch's entries as ONE entry per affected
package (`package`, `installed`, `branches`, `cves: [ids…]`, `severity`
= worst, `reason: "no fixed version available in Debian trixie /
upstream"`, `introduced: <tag>`, `review_by` = first-seen + 90 days,
preserved across syncs; entries whose package no longer has unfixed
HIGH/CRITICAL findings are removed). The sync's diff is committed in the
watcher's state-only commit. A NEW package entry (not merely a new CVE
on an existing one) opens/comments ONE deduplicated issue per branch
titled `CVE advisory: branch X.Y (<tag>)` listing the new packages and
CVE ids. `scripts/check-cve-exceptions.py` (Task 1) keeps enforcing
`review_by` expiry — renewing a date is a human review, done by editing
the entry. `cve-ledger.py` gets unit tests (add / carry-over of
review_by / removal / new-package detection). CVEs WITH a fixed version
need no advisory: the fix arrives as a package or base delta and is
caught by steps 3–4. Failure of this step is logged and does not fail
the run (advisory).

**`apply` for action=version:** `verify-upstream-tarball.sh <v>` →
`catalog.py bump-version` → `state clear-pending` → update digests and
hash in state → `changelog-entry <branch> samba-release` → tag
`vX.Y.Z-r1`. For `revision`: `bump-revision`, state digests/hash,
`changelog-entry <branch> <cause>`, tag. For `publish`: no edit, tag if
absent. Always: `update-readme-matrix [--rc-series]`, `state set
published_tag`. Output: `tags=<space-separated>` and
`dispatch=<branch:tag …>` to `$GITHUB_OUTPUT`.

**State-only commits.** A branch that just entered (or is still in) its
soak has no bump, yet `pending.first_seen` MUST survive to the next
hourly run: whenever `apply` changed `.build-state.json` (or the README
matrix, e.g. an rc suffix) and produced NO tag, the workflow still
commits those files with `chore(watch): state update [skip ci]` and
pushes `main` — no tag, no dispatch. The same holds for the ETag /
Last-Modified cache under `sources.*`. Idempotency follows: an unchanged
state file yields no commit.

**Workflow (`upstream-check.yml`):** `on: workflow_dispatch` with inputs
`security_release` (boolean, default false) and `dry_run` (boolean:
compute and print the plan, change nothing). `permissions: contents:
write, actions: write, issues: write, packages: read`. `concurrency:
upstream-check`, no cancel. Steps: checkout (SHA pin, `fetch-depth: 0`
not needed), setup buildx (pin), `observe` → `plan` (soak hours from
`${{ vars.RELEASE_SOAK_HOURS || '24' }}`) → print plan in the job
summary → if not dry run and any action ≠ none: `apply`, `git add
versions.yaml .build-state.json CHANGELOG.md README.md`, commit
`chore: <tags> (<causes>) [skip ci]` as `github-actions[bot]`, `git push
--atomic origin main <tags…>`, then for each tag `gh workflow run
release.yml --ref <tag> -f tag=<tag> -f trigger_cause=<cause>` with the
5-attempt retry loop from the reference workflow; issues for new/rc
series; the failure-notification step (dedup by title, assignee
`euca01`). Job timeout 20 min.

- [ ] **Step 1: `scripts/watch_test.py`** — table tests for `decide()` covering: greater patch first seen (→ pending, no action), still soaking, soak elapsed (→ version), `--security` (→ version immediately), equal version + digest change (→ revision base-digest), pkg hash change (→ revision pkg-update), everything equal but tag unpublished (→ publish), everything published (→ none), an OLDER "latest" reported (→ ignored, warning string present), rc excluded from patch detection, series detection with and without rc, version tuple compare (`4.24.10 > 4.24.9`), pkg-hash function over a fixed `Inst` text, listing parser over a saved HTML fixture (copy 40 lines of the real listing into the test).
- [ ] **Step 2: Run** → fails. **Step 3: Implement `watch.py`** (probes behind functions that take an injectable `fetch`/`run` so tests never touch the network). **Step 4: Tests green**; `python3 scripts/watch.py observe` against the real network prints a sane observation (4.24 latest_patch 4.24.7, digests non-empty); `python3 scripts/watch.py plan --observe <that> --now <now>` yields `publish` for all three branches (nothing published yet). Paste both in the report.
- [ ] **Step 5: Write `upstream-check.yml`**; `actionlint` + yamllint green.
- [ ] **Step 6: `docs/adaptation-profile.md`** new section **Release cycle** (after the Runtime contract): what is watched, the decision order above, soak and its variables, the necessity criterion, what needs a human (new series, removal), the pkg-hash architecture note, where state lives; profile changelog entry; B.1's "the watcher monitors the Samba security announcement channel" sentence is amended to say the watcher monitors the release directory hourly and the maintainer triggers `security_release=true` on a pre-announced window (the announce list is not machine-parsed in v1 — stated as a B.6 limitation with the §9bis.1.a armed mode as roadmap).
- [ ] **Step 7: Roadmap** — add a dated note under D4: `REVERSED 2026-09-16 by maintainer instruction — in-repo watcher workflow modelled on unbound-distroless; see 2026-09-16-phase-4-release-automation.md`. Mark `2026-08-16-phase-4-publishing.md` superseded in its first line.
- [ ] **Step 8: Commit** `feat(watch): hourly upstream/base/package watcher with soak, necessity criterion and self-healing first publication`.

---

### Task 5: `release.yml`

**Files:**
- Create: `.github/workflows/release.yml`
- Modify: `test/e2e/operational_test.go` ONLY if the cross-branch run needs an env knob it lacks (it does not: `E2E_UPGRADE_FROM` is an image ref; the test is agnostic to the branch) — expected: no change.

**Interfaces:** consumes `catalog.py` (`branch-of-tag`, `get`, `aliases`, `default`, `release-notes`), the Task 3 gate block, `scripts/check-image-packages.sh`, `scripts/check-pins-consistency.sh --print pkg_index_hash` is NOT used here (multi-branch): pins come from `catalog.py get <branch> …`.

**Shape (binding):**

```
on:
  push: { tags: ['v*'] }
  workflow_dispatch:
    inputs:
      tag:            {required: true}                       # vX.Y.Z-rN
      trigger_cause:  {type: choice, default: manual, options: [samba-release, pkg-update, base-digest, manual, first-publication]}
      degraded_mode:  {type: choice, default: none, options: [none, emulated, staggered]}
      staggered_arch: {type: choice, default: amd64, options: [amd64, arm64]}   # the arch that IS available
      staging:        {type: boolean, default: false}        # publish to <image>-staging on GHCR only; no GitHub Release, no mirror, no CHANGELOG
permissions: {contents: write, packages: write, id-token: write, attestations: write, issues: write}
concurrency: {group: release-${{ inputs.tag || github.ref_name }}, cancel-in-progress: false}
env: { GHCR_IMAGE: ghcr.io/esitc-paris/${{ vars.IMAGE_NAME || 'samba-ad-dc' }}, DOCKERHUB_IMAGE: docker.io/esitcparis/${{ vars.IMAGE_NAME || 'samba-ad-dc' }} }
jobs:
  prepare:   validate tag format ^v\d+\.\d+\.\d+-r\d+$; checkout the tag (or main when staging+dispatch); branch=catalog.py branch-of-tag; outputs: branch, tag, image_tag (no v), aliases (json list), samba_version, sha256, bases, pkg_index_hash, is_default; immutability check: `docker buildx imagetools inspect $GHCR_IMAGE:<image_tag>` MUST fail unless staging (staging retargets IMAGE to <image>-staging and skips the check); resolve E2E_UPGRADE_FROM = highest X.Y.Z-rN tag of this branch on GHCR (empty when none) and E2E_UPGRADE_FROM_PREV = highest tag of branch X.(Y-1) when it exists in the catalog and on GHCR; compute mirror_enabled = secrets present (a step that echoes `${{ secrets.DOCKERHUB_USERNAME != '' && secrets.DOCKERHUB_TOKEN != '' }}`).
  build:     matrix {amd64: ubuntu-24.04, arm64: ubuntu-24.04-arm}; skipped per degraded_mode (staggered: only staggered_arch runs; emulated: the other arch runs on ubuntu-24.04 with docker/setup-qemu-action pinned — same steps, no gate waived); timeout 150; steps: checkout tag; pin sanity (`catalog.py get` values non-empty); setup-go from entrypoint/go.mod → gofmt/vet/`go test -race ./...`; build `samba-ad-dc:release` with ALL build args from prepare (SAMBA_VERSION, SAMBA_TARBALL_SHA256, BUILDER_BASE, RUNTIME_BASE, GOBUILD_BASE, BASE_NAME, BASE_DIGEST, PKG_INDEX_HASH, VCS_REF=$GITHUB_SHA, CREATED=<run start ISO>); Smoke + Label assertion + Package closure check (copied from ci.yml, image name adjusted); security gates block (Task 3, verbatim); build client image; E2E full suite with E2E_IMAGE/E2E_CLIENT_IMAGE/E2E_UPGRADE_FROM (`go test ./... -v -count=1 -timeout 60m`); cross-branch: `if: needs.prepare.outputs.upgrade_from_prev != ''` → `go test ./... -run 'TestUpgradeFromLastPublished$' -count=1 -timeout 30m` with E2E_UPGRADE_FROM=<prev>; login GHCR (+ Docker Hub if mirror_enabled); push by digest: `docker buildx build --platform linux/<arch> --output type=image,"name=$GHCR_IMAGE",push-by-digest=true,name-canonical=true,push=true --sbom=true --provenance=mode=max <same build args> .` (BuildKit reuses the local build cache; state in a comment that the digest pushed is rebuilt from the same inputs and the E2E-tested image is bit-identical in content per B.8); export digest → artifact `digests-<arch>`.
  merge:     needs [prepare, build]; download digests; expect 2 files (or 1 when staggered — assert exactly the expected count); login; `docker buildx imagetools create` with `-t $GHCR_IMAGE:<alias>` for every alias from prepare (respecting is_default for X and latest); resolve index digest; cosign (installer pin, `cosign-release: v2.6.5`) `cosign sign --yes $GHCR_IMAGE@<digest>`; `actions/attest-build-provenance` subject-name $GHCR_IMAGE subject-digest; mirror (if mirror_enabled && !staging): imagetools create same aliases on $DOCKERHUB_IMAGE, sign, attest, dockerhub-description sync with README links rewritten to absolute (reference repo step); GitHub Release (if !staging): tag_name, body from `catalog.py release-notes <branch> --cause <c> --degraded <m> --pending-arch <a> --digest-ghcr <d> [--digest-hub <d>]`, `generate_release_notes: false`; job summary with the verify command; notify issue "Published <tag>" (if !staging).
  notify-failure: as reference, title `Release pipeline failed for <tag>`.
```

Rules to encode with comments: a tag pushed by the watcher's
`GITHUB_TOKEN` does not trigger `push: tags`, so the watcher dispatches;
the `push: tags` trigger stays for human-pushed tags. The
`staging` path never touches `published_tag` or CHANGELOG.

- [ ] **Step 1: Write `release.yml`** following the shape; every `run:` step that reads workflow inputs does so through `env:` (never inline `${{ }}` inside shell — the reference repo's rule).
- [ ] **Step 2: Local dry-run of the cross-branch step**: `cd test/e2e && E2E_IMAGE=samba-ad-dc:dev E2E_CLIENT_IMAGE=samba-ad-dc-e2e-client:dev E2E_UPGRADE_FROM=samba-ad-dc:4.23-dev go test ./... -run 'TestUpgradeFromLastPublished$' -v -count=1 -timeout 30m` → PASS (re-proves Task 2 Step 7 against the exact command the workflow runs). Then `docker rmi samba-ad-dc:4.23-dev`.
- [ ] **Step 3: Local dry-run of the push-by-digest command** against a local registry is NOT required; instead `docker buildx build --platform linux/arm64 --output type=oci,dest=/tmp/samba-ad-dc.oci --sbom=true --provenance=mode=max <build args> .` must succeed (cache hit, ~1 min) and `tar -tf /tmp/samba-ad-dc.oci | head` lists `index.json`; delete the tar.
- [ ] **Step 4: `actionlint` + yamllint** green. **Step 5: Commit** `feat(release): gated native multi-arch release with signing, attestations, mirror and degraded modes`.

---

### Task 6: `post-push-verify.yml` (§8.5)

**Files:** Create `.github/workflows/post-push-verify.yml`.

- Trigger `workflow_run: {workflows: [Release], types: [completed]}` +
  `workflow_dispatch: {inputs: {tag: {required: true}}}`. Runs only
  `if: github.event.workflow_run.conclusion == 'success' || github.event_name == 'workflow_dispatch'`.
- Resolve the tag: from the dispatch input, or from the triggering run's
  `head_branch` (the tag ref) — assert `^v…-r\d+$`; staging runs are
  detected by the triggering run's display title containing `staging`
  (release.yml sets `run-name: Release ${{ inputs.tag || github.ref_name }}${{ inputs.staging && ' (staging)' || '' }}`) and verify the staging image instead.
- Steps: `docker buildx imagetools inspect $GHCR_IMAGE:<image_tag>`
  → assert BOTH `linux/amd64` and `linux/arm64` platforms present
  (unless the Release's notes say staggered — read the release body via
  `gh release view --json body` and accept a single platform only when
  it contains `Degraded mode: staggered`); `docker pull` each platform
  by digest; `cosign verify` with the identity regexp and issuer; `gh
  attestation verify oci://$GHCR_IMAGE@<digest> --owner esitc-paris`;
  `docker buildx imagetools inspect --format '{{json .SBOM}}'` and
  `'{{json .Provenance}}'` non-empty for each platform; mirror: same
  checks on Docker Hub when the release notes list a Docker Hub digest.
- On any failure: issue `Post-push verification failed: <tag>` assigned
  to `euca01`, deduplicated.
- [ ] Write, lint, commit `feat(release): automated post-push verification (SPEC §8.5)`.

---

### Task 7: Operations documentation and profile closure

**Files:** Create `docs/operations.md`; modify `SECURITY.md` (path of
the exceptions ledger already named — verify), `docs/adaptation-profile.md`
(changelog), `README.md` (a short "Release automation" section linking
`docs/operations.md`; the full README is Phase 5).

`docs/operations.md` content (mirror the reference repo's operations.md,
adapted): scheduling model (external hourly dispatch, why no
`schedule:`); one-time setup — fine-grained PAT (Actions: read/write on
this repo only), the trigger script `/usr/local/bin/samba-ad-dc-upstream-check`
(same shape as the reference, API path
`repos/ESITC-Paris/samba-ad-dc/actions/workflows/upstream-check.yml/dispatches`),
cron line `23 * * * *` with `MAILTO`; repository configuration checklist:
secrets `DOCKERHUB_USERNAME`/`DOCKERHUB_TOKEN` (mirror), variables
`IMAGE_NAME` (unset in production; `samba-ad-dc-staging` during a
drill), `RELEASE_SOAK_HOURS` (default 24); supervision layers (cron
mail, issues, Releases watch); manual operations (`gh workflow run
upstream-check.yml`, `-f security_release=true`, `-f dry_run=true`,
re-run a release, staging drill procedure, adding a new series when the
issue arrives, removing an EOL branch); degraded modes (§9.6) how to
dispatch `emulated`/`staggered` and the disclosure they produce; the
first-publication path (publish action) and what to expect on the first
hourly run.

- [ ] Write, lint (yamllint ignores md; check links exist), commit `docs(ops): release-cycle operations guide`.

---

## Phase exit gate

- [ ] `python3 -m unittest discover -s scripts -p '*_test.py'` green;
      `actionlint`, yamllint, shellcheck, hadolint green; go unit suite
      green; traceability and pins checks green.
- [ ] Local: 4.24.7 image built, full E2E green (21 pass, 1 skip);
      4.23.12 image built, full E2E green; cross-branch upgrade
      4.23→4.24 green; 4.22.11 build + smoke green.
- [ ] Local: trivy vuln gate, trivy secret gate, cve-exceptions gate
      green against the 4.24.7 image; gitleaks clean on the repository.
- [ ] `watch.py observe` + `plan` produce `publish` ×3 on the real
      catalog; `apply` dry-run leaves a clean tree.
- [ ] GitHub-side execution (CI run on both arches, staging drill,
      post-push verification, capbisect confirmation) is Phase 5's
      bring-up plan, not this one.
