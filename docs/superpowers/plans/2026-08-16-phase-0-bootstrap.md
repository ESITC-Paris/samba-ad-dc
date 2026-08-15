# Phase 0 — Repository Bootstrap Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps
> use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the empty `samba-ad-dc` directory into a licensed,
CI-connected GitHub repository with a green §8.1 lint gate, seeded
governance documents, and the Samba release key vendored with two-source
out-of-band pinning evidence.

**Architecture:** Pure scaffolding — no image, no Go code yet. One lint
workflow that lints whatever exists (so it stays valid as later phases
add Dockerfile/scripts/Go), governance docs copied or derived from the
vendored spec, and a cryptographic-key vendoring procedure whose evidence
is committed alongside the key.

**Tech Stack:** git, GitHub CLI (`gh`), GitHub Actions (`ubuntu-24.04`),
yamllint, shellcheck, hadolint, gpg, curl.

**Spec:** `SPEC.md` v1.2 (vendored). Master roadmap:
`docs/superpowers/plans/2026-08-16-samba-ad-dc-roadmap.md` (§0 decisions
D1–D6 resolved; this plan implements roadmap Phase 0).

## Global Constraints

- All content in English (SPEC.md normative header).
- Repo license **Apache-2.0** (§2.4).
- Non-affiliation notice in README (§2.1); no Samba logo anywhere (§2.2).
- Key pinning: out-of-band against **two independent sources**, evidence
  recorded; key rotation only ever via reviewed PR (§4.4).
- Lint gate (§8.1): hadolint, shellcheck, YAML validation — blocking.
- Namespace: `ghcr.io/esitc-paris/samba-ad-dc` (roadmap D1).
- Nothing is published in this phase; no registry interaction at all.

**Working conventions for every task:** commit messages in imperative
English (`chore: …`, `docs: …`, `ci: …`); run the local lint commands of
Task 3 before each commit once Task 3 lands.

---

### Task 1: Local repository, license, hygiene files

**Files:**
- Create: `.gitignore`, `LICENSE`
- (Already present: `SPEC.md`, `docs/superpowers/plans/*.md`)

**Interfaces:**
- Produces: a git repo on branch `main` that later tasks commit into.

- [ ] **Step 1: Initialize the repository**

```bash
cd /Users/nicolas/IT/docker/samba-ad-dc
git init -b main
```

- [ ] **Step 2: Write `.gitignore`**

```gitignore
# Go build outputs (entrypoint, e2e — from Phase 2 on)
/entrypoint/entrypoint
*.test
# Local scratch
*.log
.DS_Store
# Local-only compose overrides
docker-compose.override.yml
```

- [ ] **Step 3: Fetch the Apache-2.0 license text verbatim**

```bash
curl -fsSL https://www.apache.org/licenses/LICENSE-2.0.txt -o LICENSE
head -3 LICENSE   # expect: "Apache License / Version 2.0, January 2004"
```

- [ ] **Step 4: First commit**

```bash
git add SPEC.md LICENSE .gitignore docs/
git commit -m "chore: bootstrap repository with vendored spec v1.2, Apache-2.0 license, roadmap"
```

- [ ] **Step 5: Verify**

Run: `git log --oneline` → exactly 1 commit; `git status` → clean.

---

### Task 2: GitHub repository under esitc-paris

**Files:** none (remote-side task).

**Interfaces:**
- Consumes: local repo from Task 1.
- Produces: `github.com/esitc-paris/samba-ad-dc` (public), remote
  `origin`, private vulnerability reporting enabled (consumed by Task 4's
  SECURITY.md link).

- [ ] **Step 1: Check auth and org access**

```bash
gh auth status
gh api orgs/esitc-paris --jq .login   # expect: esitc-paris
```

If either fails, STOP and report to the maintainer (auth or org
membership is input only they can provide).

- [ ] **Step 2: Create the public repo and push**

```bash
gh repo create esitc-paris/samba-ad-dc --public \
  --description "Production-grade Samba AD DC container image (source-built, bundled Heimdal). Independent community build - not affiliated with the Samba Team." \
  --source . --push
```

- [ ] **Step 3: Enable private vulnerability reporting; trim unused surfaces**

```bash
gh api -X PUT repos/esitc-paris/samba-ad-dc/private-vulnerability-reporting
gh repo edit esitc-paris/samba-ad-dc --enable-wiki=false --enable-projects=false
```

- [ ] **Step 4: Verify**

Run: `gh repo view esitc-paris/samba-ad-dc --json url,visibility,defaultBranchRef`
Expected: public, default branch `main`. Open
`https://github.com/esitc-paris/samba-ad-dc/security/advisories` →
"Report a vulnerability" button present.

---

### Task 3: CI lint gate (§8.1)

**Files:**
- Create: `.github/workflows/ci.yml`, `.yamllint.yaml`

**Interfaces:**
- Produces: workflow name `CI`, job id `lint` — later phases append jobs
  (`unit`, `build`, `e2e`) to this same file; `.yamllint.yaml` is the
  repo-wide YAML lint config.

- [ ] **Step 1: Write `.yamllint.yaml`**

```yaml
extends: default
rules:
  line-length:
    max: 120
  document-start: disable
  truthy:
    check-keys: false   # allow the GitHub Actions `on:` key
  comments:
    min-spaces-from-content: 1
```

- [ ] **Step 2: Resolve action pin SHAs**

Pin actions by commit SHA (supply-chain hygiene consistent with §4):

```bash
gh api repos/actions/checkout/commits/v5 --jq .sha
```

Record the SHA; use it in Step 3 with the tag as a trailing comment. If
the `v5` ref does not resolve, resolve `v4` instead and use that
tag/SHA pair.

- [ ] **Step 3: Write `.github/workflows/ci.yml`**

```yaml
name: CI

on:
  pull_request:
  push:
    branches: [main]

permissions:
  contents: read

jobs:
  lint:
    name: Lint
    runs-on: ubuntu-24.04
    steps:
      - name: Checkout
        uses: actions/checkout@<SHA-FROM-STEP-2>  # v5
      - name: yamllint
        run: pipx run yamllint --strict .
      - name: shellcheck
        run: |
          mapfile -t files < <(git ls-files '*.sh')
          if [ "${#files[@]}" -gt 0 ]; then
            shellcheck "${files[@]}"
          else
            echo "no shell scripts yet"
          fi
      - name: hadolint
        run: |
          if [ -f Dockerfile ]; then
            docker run --rm -i ghcr.io/hadolint/hadolint < Dockerfile
          else
            echo "no Dockerfile yet"
          fi
```

(`<SHA-FROM-STEP-2>` is the only substitution; everything else is
verbatim. The hadolint image gets digest-pinned in the Phase 1 plan when
the Dockerfile appears and the step starts doing real work.)

- [ ] **Step 4: Run the linters locally before pushing**

```bash
pipx run yamllint --strict .
```

Expected: exit 0. Fix any findings (including in this plan's own YAML
files) before continuing.

- [ ] **Step 5: Commit, push, verify CI is green**

```bash
git add .github/workflows/ci.yml .yamllint.yaml
git commit -m "ci: add blocking lint gate (yamllint, shellcheck, hadolint)"
git push
gh run watch --exit-status "$(gh run list --workflow CI --limit 1 --json databaseId --jq '.[0].databaseId')"
```

Expected: run concludes `success`. If it fails, fix and re-push — this
task is not done until the gate is green on `main`.

---

### Task 4: SECURITY.md and governance directories

**Files:**
- Create: `SECURITY.md`, `docs/exceptions/README.md`,
  `docs/postmortems/README.md`

**Interfaces:**
- Consumes: private vulnerability reporting enabled in Task 2.
- Produces: §10.5 and §11.2/§9.5 scaffolding referenced by later docs.

- [ ] **Step 1: Write `SECURITY.md`**

```markdown
# Security Policy

## Reporting a vulnerability

Report vulnerabilities **privately** via GitHub private vulnerability
reporting:
<https://github.com/esitc-paris/samba-ad-dc/security/advisories/new>.
Do not open public issues for security reports.

## Response times

- Acknowledgment: within **2 business days**.
- Initial assessment and severity classification: within **7 days**.

## Scope

- **This repository** (Dockerfile, entrypoint, CI, watcher integration):
  report here.
- **Samba itself**: report upstream to the Samba Team
  (<https://www.samba.org/samba/security/>). This project does not patch
  Samba; it republishes upstream fixes under its service-level
  commitments (fixable CRITICAL: 48 h; HIGH: 7 days — see SPEC.md §9.2).

## Published-image security

Every published image ships with a cosign signature, SBOM, and SLSA
provenance; verification instructions are in the README. Known
unfixable CVEs are tracked in `security/cve-exceptions.yaml` with review
dates (SPEC.md §5.4).
```

- [ ] **Step 2: Write `docs/exceptions/README.md`**

```markdown
# Specification exceptions (SPEC.md §11.2)

Any deviation from a MUST requirement lives here as one file per
exception: written, motivated, dated, and **time-limited**, named
`YYYY-MM-DD-<short-slug>.md` and containing: the spec clause deviated
from, the technical motivation, the expiry date, and the exit condition.

Currently: **no active exceptions.** (The Annex B.6 shell-entrypoint
exception was never needed: the entrypoint is implemented in Go from the
first release.)
```

- [ ] **Step 3: Write `docs/postmortems/README.md`**

```markdown
# SLO post-mortems (SPEC.md §9.5)

Every missed service-level objective is recorded here publicly, one file
per incident, named `YYYY-MM-DD-<short-slug>.md`: what was missed (SLO
and architecture, since accounting is per architecture), timeline, root
cause, and corrective action.

Currently: none.
```

- [ ] **Step 4: Commit and verify**

```bash
git add SECURITY.md docs/exceptions/README.md docs/postmortems/README.md
git commit -m "docs: add security policy and governance scaffolding"
git push
```

Verify: `https://github.com/esitc-paris/samba-ad-dc/security/policy`
renders SECURITY.md; CI green.

---

### Task 5: README skeleton with non-affiliation notice (§2.1)

**Files:**
- Create: `README.md`, `CHANGELOG.md`

**Interfaces:**
- Produces: README section headings that Phase 5 fills and
  `docs/traceability.md` will reference; the notice text reused verbatim
  in OCI labels (Phase 1).

- [ ] **Step 1: Write `README.md`**

```markdown
# samba-ad-dc

Production-grade container image for a **Samba Active Directory Domain
Controller**, built from verified upstream source with Samba's bundled
Heimdal Kerberos.

> **Status: pre-release.** No image has been published yet. Everything
> below the status line describes the target state and is completed
> before the first release; empty sections are intentionally present as
> the documented contract (SPEC.md §10.1).

## Non-affiliation notice

This is an independent community build. It is **not affiliated with,
endorsed by, or supported by the Samba Team or the Samba project**. The
Samba name is used solely to describe the packaged software. Samba
itself is © the Samba Team, licensed GPL-3.0-or-later; this build
repository is licensed Apache-2.0 (see `LICENSE`).

## Quickstart

*(Completed in Phase 5 — working copy-paste compose example: macvlan
network, `cap_drop: ALL` plus the CI-established capability set,
read-only rootfs, `*_FILE` secrets.)*

## Configuration reference

*(Completed in Phase 5 — exhaustive environment variable table from the
entrypoint contract in `docs/adaptation-profile.md`.)*

## Non-negotiable deployment constraints

See `docs/adaptation-profile.md` (authoritative): no NAT (macvlan/ipvlan
or host networking only), xattr+ACL-capable filesystem for
`/var/lib/samba` (NFS unsupported), host time discipline, file-based
secrets only.

## Volumes and backup

*(Completed in Phase 5 — volume list and pointers to the backup/restore
runbook in the deployment guide.)*

## Tags, pinning and support policy

Primary tags are `X.Y.Z-rN` (immutable); aliases `X.Y.Z`, `X.Y`, `X`.
`latest` is **not** production-usable. Production deployments should pin
by digest. Full policy: `docs/update-guide.md`.

## Verifying images

*(Completed in Phase 4 — `cosign verify` command with the expected
identity, plus SBOM/provenance inspection commands.)*

## Compatibility matrix

| Upstream branch | Maintained tags | Upstream support status |
|-----------------|-----------------|-------------------------|
| *(populated at first release; kept current by the watcher)* | | |

## Security

See `SECURITY.md`. This project conforms to the vendored publishing
specification (`SPEC.md`); conformance is asserted per image via the
`org.esitc-paris.spec-version` OCI label.
```

- [ ] **Step 2: Write `CHANGELOG.md`**

```markdown
# Changelog

All releases of the `samba-ad-dc` image, newest first. Each entry
records: image tag, embedded Samba version, fixed CVEs, image changes,
and the trigger cause (`samba-release | pkg-update | base-digest |
manual`) per SPEC.md §10.4.

*No releases yet.*
```

- [ ] **Step 3: Commit and verify**

```bash
git add README.md CHANGELOG.md
git commit -m "docs: add README skeleton with non-affiliation notice and changelog"
git push
```

Verify: repo landing page renders the notice above the fold; CI green.

---

### Task 6: Adaptation profile (living copy of Annex B)

**Files:**
- Create: `docs/adaptation-profile.md` (derived from `SPEC.md` Annex B)

**Interfaces:**
- Produces: the authoritative living profile. Phase 2 adds the
  entrypoint behavior contract (env vars, exit codes) to it; Phase 3
  replaces the capability *hypothesis* with the bisection *result*.

- [ ] **Step 1: Create the file from Annex B**

Copy the full text of `SPEC.md` Annex B (sections B.1–B.7) into
`docs/adaptation-profile.md`, then apply exactly these deltas:

1. New header at the top:

```markdown
# Adaptation profile — samba-ad-dc (SPEC.md §12)

Living copy of SPEC.md Annex B. **This file is authoritative** for the
image's current state; Annex B inside the vendored SPEC.md stays frozen
at ratification. Divergences from Annex B are listed in the changelog at
the bottom of this file.
```

2. In B.6, replace the first bullet (the "§6.7 exception
   (time-limited)" bullet about the shell entrypoint) with:

```markdown
- **§6.7: not applicable.** The entrypoint is implemented in Go from the
  first release; no shell interim ever ships and no §11.2 exception is
  required (see `docs/exceptions/README.md`).
```

3. Append at the bottom:

```markdown
## Profile changelog

- 2026-08-16: created from SPEC.md v1.2 Annex B; B.6 shell-entrypoint
  exception removed (Go entrypoint committed from first release, roadmap
  decision D2).
```

- [ ] **Step 2: Commit and verify**

```bash
git add docs/adaptation-profile.md
git commit -m "docs: seed living adaptation profile from Annex B (Go entrypoint from day one)"
git push
```

Verify: diff `docs/adaptation-profile.md` against SPEC.md Annex B — the
only differences are the three deltas above; CI green.

---

### Task 7: Vendor the Samba release key with two-source pinning (§4.4)

**Files:**
- Create: `keys/samba-release-key.asc`, `keys/PINNING.md`

**Interfaces:**
- Produces: the key file the Phase 1 Dockerfile builder stage imports for
  `gpg --verify` of release tarballs; PINNING.md as the §4.4 audit
  evidence.

- [ ] **Step 1: Fetch source 1 — samba.org download server (TLS)**

```bash
mkdir -p keys /private/tmp/claude-501/-Users-nicolas-IT-docker-samba-ad-dc/92fe2542-7396-4286-913a-c29f0af01d4d/scratchpad/keypin
cd /private/tmp/claude-501/-Users-nicolas-IT-docker-samba-ad-dc/92fe2542-7396-4286-913a-c29f0af01d4d/scratchpad/keypin
curl -fsSL -o source1.asc https://download.samba.org/pub/samba/samba-pubkey.asc
gpg --show-keys --with-fingerprint --with-colons source1.asc | awk -F: '/^fpr/ {print $10}' | tee fpr1.txt
```

- [ ] **Step 2: Fetch source 2 — Debian samba packaging upstream signing key
  (independent infrastructure: salsa.debian.org)**

```bash
curl -fsSL -o source2.asc "https://salsa.debian.org/samba-team/samba/-/raw/master/debian/upstream/signing-key.asc"
gpg --show-keys --with-fingerprint --with-colons source2.asc | awk -F: '/^fpr/ {print $10}' | tee fpr2.txt
```

If this URL 404s (Debian branch layout changes), locate
`debian/upstream/signing-key.asc` in the current default branch of
`salsa.debian.org/samba-team/samba` and use that raw URL; record the URL
actually used. If no such file exists anymore, use the Samba key as
referenced by a samba-announce mailing-list archive message
(lists.samba.org) as the second source, and record that instead.

- [ ] **Step 3: Compare fingerprints — HARD STOP on mismatch**

```bash
sort -u fpr1.txt > s1; sort -u fpr2.txt > s2
comm -12 s1 s2
```

Expected: at least the primary key fingerprint common to both sources.
**If the primary fingerprints do not match: STOP. Do not commit any
key. Report both fingerprints and both URLs to the maintainer** — a
mismatch is either a layout misunderstanding or a compromise, and only a
human decides which.

- [ ] **Step 4: Functional check — the key verifies a current release**

```bash
LATEST=$(curl -fsSL https://download.samba.org/pub/samba/stable/ | grep -oE 'samba-[0-9]+\.[0-9]+\.[0-9]+\.tar\.gz' | sort -Vu | tail -1)
VER=${LATEST#samba-}; VER=${VER%.tar.gz}
curl -fsSLO "https://download.samba.org/pub/samba/stable/samba-${VER}.tar.gz"
curl -fsSLO "https://download.samba.org/pub/samba/stable/samba-${VER}.tar.asc"
gpg --import source1.asc
gunzip -k "samba-${VER}.tar.gz"
gpg --verify "samba-${VER}.tar.asc" "samba-${VER}.tar"
```

Expected: `Good signature`, key fingerprint equal to the pinned one.
(Samba signs the **uncompressed** tarball — this gunzip step is why, and
this fact carries into the Phase 1 Dockerfile.)

- [ ] **Step 5: Vendor the key and write `keys/PINNING.md`**

```bash
cp source1.asc /Users/nicolas/IT/docker/samba-ad-dc/keys/samba-release-key.asc
```

`keys/PINNING.md` (fill the bracketed values with the actual outputs
from Steps 1–4):

```markdown
# Samba release key — out-of-band pinning evidence (SPEC.md §4.4)

- Pinned: 2026-08-16
- Key file: `samba-release-key.asc`
- Primary key fingerprint: `[fingerprint from Step 3]`
- User ID(s): `[uid lines from gpg --show-keys]`

## Independent sources (fingerprints matched)

1. `https://download.samba.org/pub/samba/samba-pubkey.asc` (samba.org
   download server, TLS) — fetched 2026-08-16.
2. `[exact URL used in Step 2]` (Debian samba packaging, salsa.debian.org
   — independent infrastructure and maintainership) — fetched 2026-08-16.

## Functional verification

`samba-[VER].tar.asc` verified against this key over the uncompressed
tarball on 2026-08-16: Good signature, fingerprint matched.

## Rotation policy

Key rotation happens ONLY via a reviewed pull request that references
the upstream rotation announcement and repeats this two-source procedure
(SPEC.md §4.4). The watcher never rotates keys.
```

- [ ] **Step 6: Commit and verify**

```bash
cd /Users/nicolas/IT/docker/samba-ad-dc
git add keys/
git commit -m "chore: vendor Samba release key with two-source out-of-band pinning evidence"
git push
```

Verify: `gpg --show-keys keys/samba-release-key.asc` fingerprint equals
the one recorded in PINNING.md; CI green.

---

## Phase exit gate (from the roadmap)

- [ ] CI (`lint` job) green on `main` for the final commit.
- [ ] `keys/PINNING.md` records two independent, matching fingerprint
      sources and a successful tarball verification.
- [ ] Repo renders: license, security policy, README notice.
- [ ] `docs/adaptation-profile.md` diverges from Annex B only by the
      three documented deltas.

On completion: report the exit-gate status to the maintainer and propose
starting the Phase 1 plan (source-built image).
