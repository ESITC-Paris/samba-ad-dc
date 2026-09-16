# Phase 5 — Documentation Set and GitHub Bring-up Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps
> use checkbox (`- [ ]`) syntax for tracking. Depends on
> `2026-09-16-phase-4-release-automation.md` being complete.

**Goal:** The §10 documentation set is complete and traceable to the E2E
suite in both directions; the repository is live on GitHub with CI green
on both architectures; the release pipeline has been exercised end to end
against a staging package name and verified per §8.5; the capability set
is confirmed by CI on both architectures; the only remaining step to the
first public release is the maintainer's hourly trigger (or one manual
dispatch), which this plan documents but does not perform.

**Architecture:** Documentation is written from the authoritative
contract (`docs/adaptation-profile.md`, Runtime contract + B sections),
not from memory: every environment variable, exit code, mode and
constraint in the guides is a copy of the profile's wording, and every
guide section carries the E2E test ID that covers it. The traceability
checker is extended so a test ID that no guide cites, or a guide citing a
test that does not exist, fails CI (§10.6). GitHub bring-up pushes
`main`, configures the repository variables, and drives the workflows
with `gh`; every wait is a bounded `gh run watch`.

**Tech Stack:** Markdown, `scripts/check-traceability.sh` (POSIX sh),
`gh` CLI (authenticated as `euca01`), GitHub Actions.

**Spec:** `SPEC.md` §§8.2, 8.5, 10.1–10.6, 12, Annex A; `docs/adaptation-profile.md`
(authoritative); roadmap Phases 5 and 7.

## Global Constraints

- The adaptation profile is the source of truth; guides may restate but
  never contradict it. Exit codes, variables and messages are copied from
  the Runtime contract verbatim.
- Every guide section (README included) that documents a behaviour
  carries `Covered by: \`TestX\`` naming a test in
  `docs/traceability.md`; the `doc section` column of that map is filled
  with `file#anchor` for all 17 rows (no `pending (Phase 5)` left).
- `cap_drop: [ALL]` + `cap_add: [SYS_ADMIN, NET_BIND_SERVICE, CHOWN,
  FOWNER, SETUID, SETGID]`, `read_only: true`, tmpfs `/run`, `/tmp`,
  `/var/cache/samba`, volumes `/var/lib/samba` and `/etc/samba`,
  `security_opt: [no-new-privileges:true]` in EVERY deployment example;
  `--privileged` appears only in a sentence forbidding it (§5.2).
- Network examples use macvlan (primary) or host networking; bridge
  port-publishing is stated unsupported (B.3). `SAMBA_DNS_FORWARDER` is
  set in every multi-DC example and explained.
- Secrets in examples are files (`*_FILE`), never env values (§6.1).
- Production examples pin `X.Y.Z-rN` or a digest; `latest` is explicitly
  discouraged with the reason (§3.3, §10.3).
- The cosign verification command in the README is exactly:
  `cosign verify ghcr.io/esitc-paris/samba-ad-dc:<tag> --certificate-identity-regexp 'https://github.com/ESITC-Paris/samba-ad-dc/.*' --certificate-oidc-issuer https://token.actions.githubusercontent.com`
  plus `gh attestation verify oci://ghcr.io/esitc-paris/samba-ad-dc@<digest> --owner esitc-paris`.
- README carries: non-affiliation notice (kept), badges, quickstart,
  exhaustive variable table, constraints, volumes and backup pointer,
  tags/pinning/support policy, verification, compatibility matrix
  (rendered by `catalog.py`), security pointer, release automation
  pointer (§10.1).
- Restored-DC layout caveat (B.6) appears in the backup/restore runbook
  with the `testparm` command that derives paths; NetBIOS 15-character
  container-name rule appears in the deployment guide; join requires
  the DC to resolve through a DC (B.3).
- Everything in English. Commit style and trailers as before.
- GitHub side: remote `origin` = `https://github.com/ESITC-Paris/samba-ad-dc.git`;
  push only `main` (and, during the staging drill, nothing else); never
  force-push; never delete a tag; the staging package
  `samba-ad-dc-staging` is deleted at the end of the drill and nothing
  else is deleted from GHCR; `IMAGE_NAME` is never set (the drill uses
  `staging=true` alone). The FIRST REAL RELEASE IS NOT TRIGGERED BY
  THIS PLAN (maintainer decision boundary).

---

### Task 1: README (§10.1)

**Files:** Modify `README.md`.

Sections, in order: title + one-paragraph description; badges (CI,
Release, Upstream check — workflow badge URLs on
`github.com/ESITC-Paris/samba-ad-dc/actions/workflows/<file>/badge.svg`; GHCR
image link); **Non-affiliation notice** (existing text); **Status**
(pre-release line stays until the first release; the watcher's first
publication removes it — add a one-line HTML comment telling Phase 7's
operator to delete the paragraph); **Quickstart** — a complete
`docker-compose.yml`: macvlan network with `parent`, `subnet`, `gateway`
placeholders that are clearly marked as values to replace, one DC
service `dc1` (name ≤ 15 chars), `image:
ghcr.io/esitc-paris/samba-ad-dc:4.24` with a comment on pinning, env
`SAMBA_MODE=provision` for first boot and the comment to switch to `run`,
`SAMBA_REALM`, `SAMBA_DOMAIN`, `SAMBA_DNS_FORWARDER`,
`SAMBA_ADMIN_PASSWORD_FILE=/run/secrets/admin_password`, `secrets:` with
a file-based secret, the hardening block from Global Constraints, the
two named volumes, `healthcheck` inherited note; then the three commands
to bring it up, wait for `healthy`, and `kinit` from a member. Covered by
`TestProvision`, `TestKerberosKinit`. **Configuration reference** — the
full variable table from the Runtime contract (copy) + the argv table +
the exit-code table. Covered by `TestPlainEnvSecretRejected`,
`TestMissingSecretFailsFast`, `TestRunModeWithoutStateRefused`,
`TestProvisionOverStateRefused`. **Non-negotiable deployment
constraints** (B.3 copy). **Volumes and backup** — the two volumes, what
each holds, filesystem requirements, pointer to the deployment guide's
backup/restore runbook. Covered by `TestOfflineBackupRestore`. **Tags,
pinning and support policy** — the tag table (as reference repo), the
`latest`/`edge` rule, digest pinning recommendation, link to
`docs/update-guide.md`. **Verifying images** — the two commands from
Global Constraints and the SBOM/provenance inspection commands
(`docker buildx imagetools inspect … --format '{{json .SBOM}}'`).
**Compatibility matrix** (markers + rendered content, untouched by
hand). **Release automation** — three sentences and a link to
`docs/operations.md`. **Security** (existing). **License** — Apache-2.0
repo, GPL-3.0-or-later image content.

- [ ] Write; check every internal link target exists; `python3 scripts/catalog.py update-readme-matrix` leaves the file unchanged (the table is already rendered); commit `docs(readme): complete §10.1 README`.

---

### Task 2: Deployment guide (§10.2)

**Files:** Create `docs/deployment-guide.md`.

Sections (each with `Covered by:`): 1 Constraints first (B.3, capability
set with the B.2 evidence link, filesystem, time, secrets, NetBIOS name
rule) — `TestProvision`; 2 Verify before first run (cosign + attestation
commands) — no test (state: verified by `post-push-verify.yml`, §8.5);
3 First domain controller (provision; compose from README; what the
first boot logs look like; switching to `SAMBA_MODE=run` and why) —
`TestProvision`, `TestIdempotentRestart`; 4 Protocol check from a member
(kinit, smbclient -k, ldapsearch over LDAPS with the DC certificate, dig
SRV, chronyd -Q) — `TestKerberosKinit`, `TestKerberizedSMB`,
`TestNTLMAuth`, `TestLDAPSCertificate`, `TestDNSSRVRecords`,
`TestSignedNTPWiring`; 5 Scale-out: additional DC (join; resolver must
be the first DC; `SAMBA_DNS_FORWARDER`; `SAMBA_JOIN_PASSWORD_FILE`;
functional-level mirror; verifying replication both ways with
`samba-tool drs showrepl` and an object created on each side) —
`TestJoinReplicationBothWays`; 6 Day-2: health (what the HEALTHCHECK
probes, reading `docker inspect` health log), logs (stdout only; the
accepted `reopen_one_log` noise), time (host discipline; MS-SNTP served),
database consistency (`SAMBA_MODE=maintenance`, `check`/`repair`) —
`TestDBConsistency`; 7 Backup and restore runbook — offline backup with
stopped volumes (`samba-tool domain backup offline`), restore into a
fresh instance (`samba-tool domain backup restore --newservername …
--targetdir /var/lib/samba`), the four findings from the test (name
cannot be reused, SRV records need re-registration / self-resolution,
restored layout under `/var/lib/samba/state` and the `testparm -s
--parameter-name=path --section-name=sysvol` derivation, marker
adoption) — `TestOfflineBackupRestore`; 8 Sysvol replication is manual
(B.6) — procedure sketch with `rsync` from the PDC emulator, no test
(limitation, §12.4).

- [ ] Write; commit `docs(deploy): deployment guide with E2E traceability (§10.2)`.

---

### Task 3: Update guide (§10.3)

**Files:** Create `docs/update-guide.md`.

Sections: pinning strategies by profile (branch pin `X.Y` recommended;
immutable `X.Y.Z-rN` or digest for regulated environments; `latest`
discouraged and why); learning about updates (GitHub Releases watch,
Atom feed URL `https://github.com/ESITC-Paris/samba-ad-dc/releases.atom`);
patch update step by step (pull, recreate, watch health, `dbcheck` runs
automatically on upgrade — the marker logic) — `TestUpgradeFromLastPublished`;
multi-DC rollout order (non-PDC first, PDC emulator last, one at a time,
replication check between) — `TestJoinReplicationBothWays`; branch
upgrade (N-1 → N supported and tested; skipping a branch unsupported;
functional-level note) — `TestUpgradeFromLastPublished` (cross-branch run
described in `docs/operations.md`); EOL handling (matrix statuses, what
the deprecation issue/notice means); rollback = restore-based, never tag
downgrade, with the exact refusal message and exit 22 quoted —
`TestDowngradeRefused`; auto-appliers discouraged for a stateful DC (with
the sentence from §10.3).

- [ ] Write; commit `docs(update): update guide (§10.3)`.

---

### Task 4: Traceability closure (§10.6)

**Files:** Modify `docs/traceability.md`, `scripts/check-traceability.sh`.

- Fill the `doc section` column: `README.md#…`, `docs/deployment-guide.md#…`,
  `docs/update-guide.md#…` anchors (GitHub-style slugs of the headings).
- Extend the checker with check (f): every test ID in the map appears as
  `` `TestX` `` in at least one of the three guide files, and every
  `` `Test…` `` token in the guides is a mapped ID (or an exempt infra
  test) — both directions, same reporting style as (a)–(e). Add a
  mutation proof to the report (remove one citation → red).
- Update the map's intro paragraph (no longer "guides do not exist yet").
- [ ] Commit `test: traceability covers the guides in both directions (§10.6)`.

---

### Task 5: GitHub bring-up

**Files:** none in-repo beyond what CI reveals.

- [ ] `git remote add origin https://github.com/ESITC-Paris/samba-ad-dc.git`; `git push -u origin main` (the repository is empty and public; this is the roadmap's Phase 0 Task 2, deferred until `gh` auth).
- [ ] Repository settings via `gh api`: default branch `main`; Actions permissions already `all` with default workflow permissions `write` (verified); set variable `RELEASE_SOAK_HOURS=24` explicitly (`gh variable set RELEASE_SOAK_HOURS --body 24`); enable Dependabot alerts if not on (`gh api -X PUT repos/ESITC-Paris/samba-ad-dc/vulnerability-alerts`); description and topics (`gh repo edit --description "Production-grade Samba Active Directory Domain Controller image — source-built, bundled Heimdal, signed, auto-updated" --add-topic samba,active-directory,docker,domain-controller`).
- [ ] Watch the push-triggered CI run to completion: `gh run watch <id> --exit-status` (budget: up to 2.5 h; poll with `gh run watch`, never a sleep loop). Both `Build (amd64)` and `Build (arm64)` must pass INCLUDING the E2E suite. Any red is a real finding: fix in a commit (the usual implementer rules), push, watch again. Record run URLs.
- [ ] Dispatch the capability bisection: `gh workflow run capbisect.yml` and watch it (up to 3 h; it runs in parallel with Task 6 — start it first). Download both `capbisect-stable-*` artifacts; commit them as `test/capbisect/results-amd64.txt` and (refreshed) `results-arm64.txt`; amend B.2's "CI confirmation pending" paragraph with the run URL and verdict; commit `test(capbisect): CI-confirmed capability set on amd64 and arm64`.

---

### Task 6: Staging drill (Phase 4 exit gate, §8.5)

- [ ] Do NOT set `IMAGE_NAME` (ruling 2026-09-16: `release.yml` adds the `-staging` suffix itself when `staging=true`; setting both would target `samba-ad-dc-staging-staging`). `IMAGE_NAME` exists only to rename the product.
- [ ] Dispatch `gh workflow run release.yml --ref main -f tag=$(python3 scripts/catalog.py git-tag 4.24) -f trigger_cause=manual -f staging=true`; watch to completion (budget 2.5 h). Both native builds must pass every gate; `merge` must publish `ghcr.io/esitc-paris/samba-ad-dc-staging:<aliases>` and sign + attest it.
- [ ] Watch the `post-push-verify.yml` run it triggers; it must pass. Also verify from this machine: `cosign verify ghcr.io/esitc-paris/samba-ad-dc-staging:<tag> …` (install cosign via `brew install cosign` if absent) and `gh attestation verify oci://…@<digest> --owner esitc-paris`.
- [ ] Annex A checklist walk against the staging release, recorded in the ledger (each box with evidence: run URL, digest, command output).
- [ ] Clean up: delete the staging package (`gh api -X DELETE /orgs/ESITC-Paris/packages/container/samba-ad-dc-staging` — confirm the exact name first with `gh api /orgs/ESITC-Paris/packages?package_type=container`). Nothing else is deleted.
- [ ] `docs/operations.md`: add a dated "Drills" line naming the run.

---

### Task 7: Closure

- [ ] `docs/adaptation-profile.md` changelog entry for Phase 5 (guides, traceability closure, CI confirmation of B.2, staging drill); B.2 paragraph updated; README status line reviewed.
- [ ] Final commit + push; `gh run watch` the resulting CI run to green.
- [ ] Write the go-live handover into `docs/operations.md` "First publication" (already drafted in Phase 4 Task 7 — verify it says exactly what the maintainer must do: add Docker Hub secrets, install the hourly trigger, or run `gh workflow run upstream-check.yml` once; expected outcome: three `publish` actions, three releases).

## Phase exit gate

- [ ] CI green on both architectures on `main` (run URL recorded).
- [ ] Staging release published, signed, attested, verified by
      `post-push-verify.yml` and from a clean machine; staging package
      deleted.
- [ ] Capability set CI-confirmed on both architectures; B.2 amended.
- [ ] Traceability check green in both directions including the guides.
- [ ] No real release published; handover documented.
