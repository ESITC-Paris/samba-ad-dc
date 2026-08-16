# Phase 4 — Publishing Pipeline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task.

**Goal:** A merge to `main` that changes `versions.yaml` (or a manual
dispatch) publishes, for each affected branch, a fully gated multi-arch
release to GHCR — immutable `X.Y.Z-rN` tag + aliases, cosign keyless
signature, syft SPDX SBOM, SLSA Build L2 provenance — mirrored to Docker
Hub, then automatically verified post-push; plus the §9.6 degraded modes
implemented behind explicit inputs, and the secrets/CVE gates wired as
blocking steps everywhere an image is produced.

**Architecture:** Four workflows. `release.yml`: `prepare` job parses
`versions.yaml`, computes tags, enforces §3.2 immutability (a tag that
already exists on GHCR aborts the release), then per-arch native jobs
rebuild with ALL gates (lint, unit, build, secrets, CVE, full E2E),
push per-arch digests, and a `publish` job assembles the manifest list,
tags, signs, attests, and mirrors. `post-push-verify.yml` runs on
release completion: pull-by-digest, cosign verify, SBOM+provenance
presence; failure opens an issue. `ci.yml` gains the secrets and CVE
gates so PRs are held to the same bar. Degraded modes (§9.6) are
explicit `workflow_dispatch` inputs with mandatory disclosure text
threaded into the release notes.

**Tech Stack:** GitHub Actions (native amd64/arm64 runners), docker
buildx, cosign (keyless, GitHub OIDC), syft, trivy, gitleaks,
trufflehog, `actions/attest-build-provenance`, GHCR + Docker Hub.

**Spec:** `SPEC.md` §§3.1–3.5, 4.2, 4.5, 4.6, 5.3, 5.4, 8.4, 8.5, 9.6.
Roadmap Phase 4.

## Global Constraints

- GHCR (`ghcr.io/esitc-paris/samba-ad-dc`) is the source of truth;
  Docker Hub (`docker.io/esitcparis/samba-ad-dc` — namespace confirmed
  before first mirror push) references it (§4.6).
- `X.Y.Z-rN` immutable: existing tag ⇒ abort with actionable error
  (§3.2). Aliases `X.Y.Z`, `X.Y` always; `X` and `latest` only when the
  branch == `default_branch` in versions.yaml (§3.3/§3.6).
- Publish credentials exist ONLY in the `release` GitHub environment;
  nothing publishes from outside Actions (§4.5).
- Cosign keyless with GitHub OIDC; the expected identity is this repo's
  release.yml workflow on refs/heads/main; the verify command goes into
  README (Phase 5 finalizes wording, this phase records the exact
  command in the workflow's job summary output).
- SBOM: syft SPDX-JSON, attached via cosign attest (predicate type
  spdxjson). Provenance: `actions/attest-build-provenance` (SLSA Build
  L2) (§4.2).
- Trivy gate: `--ignore-unfixed --severity HIGH,CRITICAL --exit-code 1`;
  FULL unfiltered report always uploaded as artifact (§5.4);
  `security/cve-exceptions.yaml` review-date expiry check blocks when
  any date has passed.
- Secrets gate: gitleaks over the repo history + trufflehog filesystem
  scan over the exported image rootfs (§5.3).
- Degraded modes only via explicit dispatch inputs; every activation
  logged in the run summary + changelog (§9.6).
- All workflow-emitted text in English. Action refs pinned by SHA.
  Trailers on all commits (as previous phases).

---

### Task 1: Release metadata tooling

**Files:**
- Create: `scripts/release-meta.py` (stdlib-only Python), `scripts/check-cve-exceptions.py`, `security/cve-exceptions.yaml`
- Test: `scripts/release-meta_test.py` (run with `python3 -m unittest`)

**Interfaces:**
- `release-meta.py <versions.yaml> <branch>` prints JSON:
  `{"samba_version","revision","tag","aliases":[...],"builder_base","runtime_base","runtime_digest","pkg_index_hash","is_default_branch"}`
  where `tag` = `X.Y.Z-rN`, aliases per §3 rules above. Pure stdlib
  YAML subset parser is NOT acceptable — use `import yaml` guarded with
  a clear error (PyYAML is preinstalled on GitHub runners; local: `pipx
  run --spec pyyaml python`? No — locally run via
  `python3 -c 'import yaml'` check; document `pip install pyyaml` as
  the local dev requirement in the script docstring).
- `security/cve-exceptions.yaml` initial content: empty list with
  schema comment (`- cve: CVE-XXXX-YYYY / component / reason /
  review_by: YYYY-MM-DD / introduced: tag`).
- `check-cve-exceptions.py` exits 1 if any `review_by` < today, listing
  the expired entries.

TDD: unittest cases for tag/alias computation (default vs non-default
branch), unknown branch → error, revision formatting, expiry check
(past/today/future dates).

Commit `feat(release): release metadata and CVE-exception tooling`.

---

### Task 2: Secrets + CVE gates in ci.yml

**Files:**
- Modify: `.github/workflows/ci.yml`

Add to the `build` job after the image build (both arches):
- gitleaks: pinned action or
  `docker run --rm -v "$PWD:/repo" ghcr.io/gitleaks/gitleaks:<tag>@<digest> detect --source /repo --no-banner` (repo scan).
- trufflehog image-fs scan: `docker create` the built image, `docker
  export` to tar, scan with pinned trufflehog container
  (`filesystem` mode over the extracted tree), fail on verified
  findings; document the runtime cost in a comment.
- trivy: pinned container, `image --ignore-unfixed --severity
  HIGH,CRITICAL --exit-code 1 samba-ad-dc:ci`; ALWAYS
  `trivy image --format json -o trivy-full.json` (no filters) first and
  `actions/upload-artifact` it; then `python3
  scripts/check-cve-exceptions.py`.
Local verification: run each scanner container against `samba-ad-dc:dev`
locally and record results (trivy WILL likely list unfixed CVEs — the
gate must stay green because of --ignore-unfixed; a fixable HIGH/CRIT
finding at this point is real work: update versions.yaml pins/rebuild,
do not suppress).

Commit `ci: blocking secrets and fixable-CVE gates on both architectures`.

---

### Task 3: release.yml

**Files:**
- Create: `.github/workflows/release.yml`

Structure (binding; exact YAML authored in-task, actions SHA-pinned):
- Triggers: `push: {branches: [main], paths: [versions.yaml]}` and
  `workflow_dispatch` inputs: `branch` (required for dispatch),
  `trigger_cause` (choice: samba-release|pkg-update|base-digest|manual),
  `pkg_index_hash` (optional override), `degraded_mode`
  (choice: none|emulated|staggered, default none),
  `staggered_arch` (choice: amd64|arm64, which arch IS available).
- Job `prepare` (ubuntu-24.04): checkout; determine affected branch(es)
  — on push, diff versions.yaml against the previous commit to find
  changed branch entries; on dispatch, the input; run release-meta.py →
  outputs matrix JSON; **immutability check**: `docker buildx
  imagetools inspect ghcr.io/esitc-paris/samba-ad-dc:<tag>` must FAIL
  (tag absent) else abort with "tag exists — bump revision (-rN)".
- Jobs `build-amd64` / `build-arm64`: `runs-on` native
  (`ubuntu-24.04` / `ubuntu-24.04-arm`); skipped when degraded_mode
  =staggered and this arch != staggered_arch; when degraded_mode
  =emulated and this arch is the unavailable one, runs on ubuntu-24.04
  with QEMU setup (identical steps otherwise — no gate waived);
  environment: `release`. Steps: full lint + unit + build with ARGs
  from prepare outputs (VCS_REF=github.sha, CREATED=run timestamp,
  PKG_INDEX_HASH — MANDATORY: resolve via
  `check-pins-consistency.sh --print pkg_index_hash` exactly as ci.yml
  does; the Dockerfile default is a bootstrap fallback and a published
  image must never carry it) + secrets gate + trivy gate + full E2E
  (`go test ./test/e2e/`) + login GHCR (GITHUB_TOKEN packages:write) +
  push by digest only (`docker push --quiet` of a per-arch tag then
  capture digest, or buildx `--output type=registry,push-by-digest`),
  output the digest.
- Job `publish` (needs both builds; runs when at least the required
  set per degraded_mode succeeded): `docker buildx imagetools create`
  manifest list from per-arch digests → tag + aliases; cosign sign
  (keyless) the manifest digest; syft SBOM from the manifest (per-arch
  SBOMs attached individually); `actions/attest-build-provenance` for
  the pushed digests; mirror: login Docker Hub (secrets DOCKERHUB_USER/
  DOCKERHUB_TOKEN), `imagetools create` same manifest under
  `docker.io/esitcparis/samba-ad-dc` tags; write run summary: digest,
  verify command, trigger_cause, degraded-mode disclosure if any.
- Concurrency: `release-${{ inputs.branch || 'auto' }}`, no
  cancel-in-progress (a running release finishes).
- ci.yml follow-ups while touching workflows (Phase 1 review carry):
  change ci.yml concurrency to
  `cancel-in-progress: ${{ github.event_name == 'pull_request' }}` so a
  push cannot cancel a main-branch run once releases hang off main, and
  add `workflow_dispatch:` to ci.yml triggers (manual first-run
  validation).
- Permissions: minimum per job (`contents: read`, `packages: write`,
  `id-token: write`, `attestations: write` only where needed).

Verification without auth: `actionlint` + yamllint + a dry parse of
release-meta.py against versions.yaml. The LIVE dry run is Task 6.

Commit `feat(release): gated multi-arch release pipeline with degraded modes`.

---

### Task 4: post-push-verify.yml

**Files:**
- Create: `.github/workflows/post-push-verify.yml`

- Trigger: `workflow_run` on release.yml `completed` (+
  `workflow_dispatch` with tag input for manual re-verification).
- Job (ubuntu-24.04): resolve tag→digest via imagetools; pull by digest
  for BOTH arch platforms; `cosign verify` with
  `--certificate-identity-regexp` matching this repo's release.yml on
  main and `--certificate-oidc-issuer https://token.actions.githubusercontent.com`;
  assert SBOM attestation and provenance attestation present
  (`cosign verify-attestation` / `gh attestation verify`); on ANY
  failure: `gh issue create` titled "Post-push verification failed:
  <tag>" with the failing check (needs `issues: write`).

Commit `feat(release): automated post-push verification (§8.5)`.

---

### Task 5: Update guide hooks + CHANGELOG automation

**Files:**
- Create: `scripts/changelog-entry.py`
- Modify: `CHANGELOG.md` (machinery note only, no fake entries)

- `changelog-entry.py <tag> <trigger_cause> <upstream_version>
  [--cves ...] [--notes ...]` prepends a formatted entry to
  CHANGELOG.md; unit-tested (idempotence: refuses duplicate tag entry —
  supports §3.2 immutability).
- Used by: watcher bump PRs (Phase 6) and manual release PRs; release
  PR checklist references Annex A.

Commit `feat(release): changelog entry tooling`.

---

### Task 6: Staging dry run (REQUIRES gh auth — defer if absent)

Run the complete pipeline against a staging package name:
1. Temporarily dispatch release.yml from a branch with package name
   override? NO — simpler: repo variable `IMAGE_NAME` (default
   `samba-ad-dc`), set to `samba-ad-dc-staging` for the drill, revert
   after. release.yml reads it (`vars.IMAGE_NAME || 'samba-ad-dc'`).
2. Dispatch with trigger_cause=manual; confirm: both arch builds green,
   E2E green in CI on BOTH arches, manifest published, cosign verify
   passes from a clean machine, post-push-verify green, mirror push
   green (Docker Hub secrets must exist; if not yet created, mirror
   step must no-op with a loud warning rather than fail — implement
   that behavior in Task 3).
3. Delete staging package; record the drill in the ledger +
   docs/postmortems/README.md? No — drills aren't postmortems; record
   in the Phase 4 ledger and the run summary only.

---

## Phase exit gate (roadmap Phase 4)

- [ ] actionlint/yamllint/hadolint/shellcheck green on all workflows &
      scripts; unit tests for tooling green.
- [ ] Scanner gates run green locally against samba-ad-dc:dev.
- [ ] Staging dry run complete with §8.5 verification (auth-gated; if
      auth still absent, everything else done and the dry run is the
      single open item, tracked in the ledger).
- [ ] Annex A checklist walk on the staging release recorded.
