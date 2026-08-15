# samba-ad-dc v1 Implementation Roadmap (master plan)

> **For agentic workers:** This is the MASTER ROADMAP: it locks the
> decomposition, build order, CI architecture and watcher design. Each
> phase below gets its own detailed executable plan
> (`docs/superpowers/plans/YYYY-MM-DD-phase-N-*.md`) written when the
> phase starts, executed via superpowers:subagent-driven-development or
> superpowers:executing-plans. Do not implement directly from this
> document alone.

**Goal:** Publish a production-grade, spec-conformant `samba-ad-dc`
multi-arch container image (source-built Samba AD DC, bundled Heimdal)
with full supply-chain attestation, a blocking E2E gate, and an
event-driven release watcher.

**Architecture:** Two subsystems. (1) The image repository (this
directory): multi-stage source build of Samba on digest-pinned Debian
slim, a Go entrypoint state machine, an E2E suite that is the publication
gate, and GitHub Actions pipelines building natively on amd64 + arm64 and
publishing signed/attested images to GHCR (Docker Hub mirror). (2) A
separate watcher repository: an in-house Go daemon on a maintainer
machine (timers) that detects release/package/base/lifecycle events,
applies the necessity criterion, and dispatches bump PRs via the GitHub
API — CI remains the only executor that publishes.

**Tech Stack:** Docker/BuildKit (buildx), Debian stable slim (trixie),
Samba built from source (bundled Heimdal, FHS layout), Go ≥1.24
(entrypoint + E2E runner + watcher), chrony, tini, GitHub Actions
(`ubuntu-24.04` + `ubuntu-24.04-arm` native runners), cosign (keyless),
syft (SBOM), trivy (CVE gate), gitleaks/trufflehog (secrets gate),
hadolint/shellcheck/yamllint (lint gate), GHCR + Docker Hub.

**Spec:** `SPEC.md` (v1.2, ratified — vendored per §11.1). Annex B is the
binding adaptation profile for this image.

## Global Constraints

(Verbatim from SPEC.md; every phase plan inherits these.)

- Tags: `X.Y.Z-rN` immutable primary; aliases `X.Y.Z`, `X.Y`, `X`;
  `latest` never documented as production-usable (§3.1–3.3).
- All upstream-supported stable branches published simultaneously, every
  patch release, no skipping (§3.6).
- Base images pinned by digest, updated only via bot PRs (§4.1).
- Every published image: SBOM + SLSA Build Level 2 provenance + cosign
  keyless signature; verify command in README (§4.2).
- Build-time downloads GPG-verified against a vendored, out-of-band
  pinned key (two independent sources) (§4.4, B.1).
- Builds only on public CI; never from a workstation (§4.5). GHCR is
  source of truth; Docker Hub mirrors (§4.6).
- CVE gate: blocking only for fixable HIGH/CRITICAL (`--ignore-unfixed`);
  full report kept as artifact; unfixed CVEs go to a versioned exceptions
  file with review dates (§5.4).
- Root is permitted (B.2 §5.2 exception) but the capability set is
  established by CI bisection; `cap_drop: ALL` baseline in every example;
  `--privileged` forbidden in docs (§5.2).
- Read-only rootfs is the E2E configuration on every build; writable
  paths: `/var/lib/samba`, `/etc/samba` (volumes), `/run`, `/tmp`
  (tmpfs) (§5.6, B.2).
- Secrets via `*_FILE` only; admin password never via plain env (§6.1,
  B.3).
- Entrypoint: Go static binary (shell >50 lines is forbidden without a
  §11.2 exception — v1 ships Go directly, no shell interim) (§6.7, B.6).
- Entrypoint modes: `auto`, `provision`, `join`, `run`, `maintenance`;
  version guard with downgrade refusal; documented exit codes (§6.2,
  §7.2, B.4).
- linux/amd64 + linux/arm64, native runners, same tag manifest list;
  emulation only under §9.6 degraded modes (§6.6).
- SIGTERM orderly shutdown ≤10 s documented; PID 1 reaps zombies (tini)
  (§6.3). Logs to stdout/stderr only (§6.4).
- E2E matrix = B.5, executed on both architectures, read-only rootfs;
  doc-section ↔ test traceability both ways (§8.2, §10.6).
- No scheduled builds; package-index hash passed as build arg
  invalidating exactly the runtime-package layer (§9.3, §9bis.1.c).
- SLOs: release→tag 5 business days; fixable CRITICAL 48 h, HIGH 7 d;
  per-architecture accounting; public post-mortems (§9.1, §9.2, §9.5).
- Repo license Apache-2.0; OCI `licenses` label `GPL-3.0-or-later`
  (§2.4). All content in English (normative header).
- OCI labels per §2.5 + `org.esitc-paris.spec-version` (§11.1).

---

## 0. Open decisions requiring your approval

**RESOLVED 2026-08-16:** D1 answered by the maintainer
(`ghcr.io/esitc-paris/samba-ad-dc`); D2–D6 defaults adopted.

| # | Decision | Resolution | Rationale |
|---|----------|------------|-----------|
| D1 | Org / namespace | **`esitc-paris`**: `ghcr.io/esitc-paris/samba-ad-dc`, label `org.esitc-paris.spec-version`. Docker Hub forbids hyphens in namespaces — proposed mirror `docker.io/esitcparis/samba-ad-dc`, to confirm before Phase 4 | §2.2 naming and §11.1 label |
| D2 | Entrypoint language | **Go** | Heavy shell-outs to `samba-tool`/`samba` favor Go's `os/exec` ergonomics; one language shared with E2E runner and watcher; trivial static linking (`CGO_ENABLED=0`) |
| D3 | E2E harness | **Go test binary driving the Docker CLI + compose files** | Same toolchain as entrypoint unit tests; test names become stable traceability IDs (§10.6); structured assertions beat bats string-matching for LDAP/Kerberos checks |
| D4 | Watcher location | **Separate repo (`esitc-paris/release-watcher`), watcher state in a dedicated branch of that repo** | It serves the whole future catalog, not one image (§9bis is generic); separate release cadence; image repo stays auditable as pure build inputs |
| D5 | Initial catalog scope | **Bring-up on the newest upstream stable branch only; activate remaining supported branches in Phase 7 before first public announcement** | §3.6 requires all branches *published*; doing bring-up on one branch first is a repo-internal staging matter as long as nothing is announced/published before Phase 7 completes |
| D6 | Dead-man switch (§9bis.6) | **healthchecks.io free tier ping from the watcher timer** | External to our machine (a machine down = alert), 2 h grace configurable, zero infra |

---

## 1. Subsystem decomposition

Per the writing-plans scope check, this spec is two independently
shippable subsystems:

1. **`samba-ad-dc` image repo** (this directory, Phases 0–5, 7): produces
   a locally buildable, fully gated, publishable image. Testable on its
   own at every phase boundary.
2. **`release-watcher` repo** (Phase 6): produces a daemon that turns
   upstream events into bump PRs against subsystem 1. Testable on its own
   against recorded fixtures; integration test = one end-to-end bump PR.

Dependency: the watcher consumes subsystem 1's `versions.yaml` and
`runtime-packages.txt` contracts (defined in Phase 1) and its
`release.yml` dispatch API (defined in Phase 4). The watcher can start
only after Phase 4; everything else is strictly ordered as below.

## 2. Image repository file structure

Locked now so every phase plan targets exact paths:

```
samba-ad-dc/
├── SPEC.md                        # vendored spec (done)
├── LICENSE                        # Apache-2.0 (§2.4)
├── README.md                      # §10.1
├── SECURITY.md                    # §10.5
├── CHANGELOG.md                   # §10.4
├── docs/
│   ├── adaptation-profile.md      # §12 / Annex B, maintained copy
│   ├── deployment-guide.md        # §10.2
│   ├── update-guide.md            # §10.3
│   ├── traceability.md            # doc section ↔ E2E test ID map (§10.6)
│   ├── exceptions/                # §11.2 written exceptions (empty at v1)
│   ├── postmortems/               # §9.5 missed-SLO records
│   └── superpowers/plans/         # this roadmap + phase plans
├── versions.yaml                  # one entry per branch: samba_version,
│                                  # revision, tarball sha256, base digests,
│                                  # pkg_index_hash  (watcher contract)
├── runtime-packages.txt           # justified runtime apt package list,
│                                  # one per line with a why-comment (§5.1);
│                                  # watcher filter input (§9bis.1.c)
├── keys/samba-release-key.asc     # vendored upstream release key (§4.4)
│   └── PINNING.md                 # out-of-band pinning evidence, 2 sources
├── security/cve-exceptions.yaml   # §5.4 unfixed-CVE ledger w/ review dates
├── Dockerfile                     # multi-stage: builder → runtime
├── entrypoint/                    # Go module
│   ├── go.mod
│   ├── cmd/entrypoint/main.go
│   └── internal/
│       ├── config/    # env + *_FILE loading, validation
│       ├── state/     # state detection, version marker, guards
│       ├── modes/     # auto | provision | join | run | maintenance
│       ├── run/       # samba + chrony supervision, SIGTERM handling
│       └── health/    # `entrypoint healthcheck` subcommand (§5.5)
├── test/
│   ├── e2e/                       # Go test module (D3)
│   │   ├── go.mod
│   │   ├── harness/               # container/compose driver, LDAP/KRB clients
│   │   └── *_test.go              # one file per B.5 matrix row group
│   ├── compose/
│   │   ├── single-dc.yaml         # read-only rootfs, cap_drop ALL + set (§5.6)
│   │   ├── two-dc.yaml            # join + replication topology
│   │   └── upgrade.yaml
│   └── capbisect/bisect.sh        # capability bisection driver (§5.2)
└── .github/workflows/
    ├── ci.yml                     # PR gates: lint, unit, build, E2E (amd64+arm64)
    ├── release.yml                # tag/publish pipeline (dispatch from watcher/merge)
    ├── post-push-verify.yml       # §8.5
    └── capbisect.yml              # manual/dispatch capability bisection
```

## 3. Build order (phases)

Each phase ends with working, independently verifiable software and a
review checkpoint. No phase starts before its predecessor's exit gate is
green.

### Phase 0 — Repository bootstrap
**Deliverable:** a linted, licensed, CI-connected empty shell.
- `git init`, default branch `main`; create GitHub repo under `esitc-paris` (D1).
- LICENSE (Apache-2.0), SECURITY.md, README skeleton with
  non-affiliation notice (§2.1), `docs/adaptation-profile.md` seeded from
  Annex B.
- `ci.yml` with the §8.1 lint gate only: hadolint, shellcheck, yamllint,
  `gofmt`/`go vet` placeholders — green on an empty repo.
- Vendor + pin the Samba release key: fetch from two independent sources
  (samba.org download server over TLS, and the key as referenced in
  historical samba-announce archives / Debian keyring), record both
  fingerprint provenances in `keys/PINNING.md` (§4.4).
**Exit gate:** CI green on `main`; PINNING.md shows two matching
independent fingerprint sources.

### Phase 1 — Source-built image (build correctness before behavior)
**Deliverable:** `docker build` produces a runnable Samba AD DC rootfs
image on amd64 and arm64, with no entrypoint logic yet (`CMD ["samba",
"--help"]`-level smoke only).
- `versions.yaml` schema + first entry (newest stable branch, D5);
  `runtime-packages.txt` with per-package justification comments.
- Dockerfile builder stage: digest-pinned `debian:trixie-slim`, build
  deps, tarball download from `download.samba.org`, `gpg --verify`
  against the vendored key AND sha256 pin from `versions.yaml`, FHS
  configure (`--enable-fhs --prefix=/usr --sysconfdir=/etc
  --localstatedir=/var`), bundled Heimdal (default; explicitly NOT
  `--with-system-mitkrb5`).
- Runtime stage: separate digest pin; `ARG PKG_INDEX_HASH` placed
  immediately before the single `apt-get install` of
  `runtime-packages.txt` so the hash invalidates exactly that layer
  (§9.3); chrony + tini installed; all §2.5 OCI labels +
  `org.esitc-paris.spec-version`.
- CI: build job matrix `[ubuntu-24.04, ubuntu-24.04-arm]`, native only
  (§6.6); hadolint now bites.
**Exit gate:** both arch builds green in CI; `samba --version` matches
`versions.yaml`; image contains no package not in `runtime-packages.txt`
plus its dependency closure (checked by a CI script).

### Phase 2 — Go entrypoint state machine (TDD, the §6.7 gate)
**Deliverable:** static `entrypoint` binary implementing B.4 with full
unit-test coverage of every transition and refusal.
- Behavior contract fixed in `docs/adaptation-profile.md` FIRST (it is
  the acceptance contract): modes `auto|provision|join|run|maintenance`
  via `SAMBA_MODE` (default `auto`); config via documented env vars;
  secrets only via `*_FILE` (e.g. `SAMBA_ADMIN_PASSWORD_FILE`) — plain
  `SAMBA_ADMIN_PASSWORD` is rejected with an actionable error (§6.1).
- Exit-code table (locked here, tested in unit + E2E, documented in
  README): `0` success; `10` configuration error; `11` missing/unreadable
  secret file; `20` provision/join refused over existing state; `21` run
  mode with absent state; `22` downgrade refusal (state newer than
  image); `23` database consistency check failure; `30` samba runtime
  failure. Every message: cause + remedy (§6.5).
- State marker `/var/lib/samba/.image-state.json` (samba version,
  provisioned-at, mode history) written only on successful init; version
  guard compares it to the built-in version (§7.2); on upgrade, runs
  `samba-tool dbcheck` automatically before start (B.4).
- Shell-outs restricted to `samba-tool`, `samba`, `chronyd` (§6.7).
- `entrypoint healthcheck` subcommand: DNS SRV query for
  `_ldap._tcp.<realm>` against 127.0.0.1:53 + LDAP rootDSE read + SMB
  negotiation — real protocols, not a process check (§5.5); wired as
  Dockerfile `HEALTHCHECK`.
- PID chain: `tini → entrypoint → samba` (samba in foreground); SIGTERM
  forwarded, orderly stop within 10 s (§6.3); samba/chrony logs to
  stdout/stderr (§6.4).
- Unit tests: every state transition and every refusal, with a faked
  filesystem and faked CLI runner; `go test` added to `ci.yml` as a
  blocking gate.
**Exit gate:** unit coverage report shows every mode × state combination
exercised; binary built in the Docker builder stage (same pipeline, §6.7)
and image smoke-runs `provision` manually.

### Phase 3 — E2E suite (the real publication gate)
**Deliverable:** `go test ./test/e2e/...` runs the full B.5 matrix
against a locally built image, under the read-only rootfs + capability
config, and fails meaningfully.
- Harness: Go test module driving `docker compose` on
  `test/compose/*.yaml`; every compose file: `read_only: true`, tmpfs
  `/run` `/tmp`, `cap_drop: [ALL]` + `cap_add:` current set (§5.6, B.2).
- Test files map 1:1 to B.5 rows; test function names are the
  traceability IDs recorded in `docs/traceability.md`:
  - nominal: `TestProvision`, `TestKerberosKinit`, `TestKerberizedSMB`,
    `TestNTLMAuth`, `TestDNSSRVRecords`, `TestLDAPSCertificate`,
    `TestSignedNTPWiring`, `TestDBConsistency`, `TestJoinReplicationBothWays`
  - operational: `TestIdempotentRestart`, `TestOfflineBackupRestore`
    (object-level verification into a fresh instance),
    `TestUpgradeFromLastPublished` (auto-skip on first release, §8.3),
    `TestDowngradeRefused`
  - negative: `TestMissingSecretFailsFast`, `TestProvisionOverStateRefused`,
    `TestRunModeWithoutStateRefused` (asserting the Phase 2 exit codes)
- CI: E2E job per architecture on native runners, blocking in `ci.yml`.
- Capability bisection (`capbisect.yml`, manual dispatch): drops each
  candidate cap in turn, runs the nominal subset, emits the proven
  minimal set; result committed to `docs/adaptation-profile.md` (§5.2 —
  established by testing, not copied).
**Exit gate:** full matrix green on both architectures in CI;
`docs/traceability.md` has no unmapped test and no unmapped doc section
(enforced by a CI script from here on); bisection result committed.

### Phase 4 — Publishing pipeline (supply chain)
**Deliverable:** a merge to `main` that changes `versions.yaml` publishes
a fully attested multi-arch release to GHCR + Docker Hub mirror.
- `release.yml`: triggered by merge of a release PR or
  `workflow_dispatch` (inputs: `trigger_cause`, `pkg_index_hash`,
  `mode: normal|emulated|staggered` for §9.6 — degraded modes are
  implemented now, used never, disclosed always).
  Jobs: `build-amd64` (native) and `build-arm64` (native) → each runs
  lint, unit, build, secrets scan (gitleaks on repo + trufflehog on the
  exported image filesystem, §5.3), trivy `--ignore-unfixed --severity
  HIGH,CRITICAL --exit-code 1` with full unfiltered report as artifact
  (§5.4) + expiry check on `security/cve-exceptions.yaml`, full E2E →
  push per-arch image by digest → `publish` job assembles the manifest
  list, tags `X.Y.Z-rN` + aliases, signs with cosign keyless (GitHub
  OIDC), attaches syft SPDX SBOM and SLSA L2 provenance
  (`actions/attest-build-provenance`), mirrors tags to Docker Hub with
  README pointing back to GHCR (§4.6).
- `post-push-verify.yml` (§8.5): triggered by release completion — pull
  both arches by digest, `cosign verify` with the expected identity,
  assert SBOM + provenance present; failure raises an issue.
- CHANGELOG entry generated in the release PR from `trigger_cause`
  (§10.4); README compatibility matrix updated by the same tooling.
**Exit gate:** one real end-to-end dry run to a **staging package name**
(`esitc-paris/samba-ad-dc-staging`) passes §8.5 verification; Annex A checklist
walks clean.

### Phase 5 — Documentation set (§10)
**Deliverable:** README, deployment guide, update guide complete; every
guide section carries its E2E test ID; a features-without-tests sweep
either adds tests or moves the feature to "known limitations".
- README: quickstart (compose snippet with macvlan + caps + volumes +
  `*_FILE` secrets), full env var table (from Phase 2 contract),
  constraints (B.3), verify command, non-affiliation notice, tag policy,
  compatibility matrix (§10.1).
- Deployment guide §10.2; update guide §10.3 (rollback = restore-based,
  never tag downgrade; auto-appliers discouraged).
**Exit gate:** traceability CI check green in both directions; docs
reviewed by you.

### Phase 6 — Watcher daemon (separate repo; design in §5 below)
**Deliverable:** daemon running on your machine under systemd timers,
producing correct bump PRs against fixtures and one live end-to-end bump
PR that auto-merges and publishes (staging name acceptable for the drill).
**Exit gate:** recorded-fixture test suite green (each 9bis.1 source ×
event/no-event); idempotency proven (same fixture twice → one PR);
necessity criterion negative cases (index republication with no version
delta → no dispatch); dead-man alert fires when timers are stopped 2 h.

### Phase 7 — Catalog activation and first public release
**Deliverable:** all upstream-supported stable branches in
`versions.yaml` (§3.6, B.7), cross-branch upgrade tests for documented
paths (§8.3, B.6), first real releases published for every branch,
watcher armed on all of them.
**Exit gate:** Annex A checklist green per branch; §8.5 verification green
per branch per architecture. Project is live; SLO clock starts.

Deferred beyond v1 (recorded in adaptation profile, already in B.6):
sysvol replication mechanism (v2), real Windows-client join validation
(out-of-band), SLSA L3, bit-for-bit reproducibility.

## 4. CI architecture (summary)

Trust model (§4.5): the only credentials that can publish live in GitHub
Actions environments (`release` environment, OIDC to GHCR/cosign; Docker
Hub token as environment secret). The watcher machine holds only a
fine-grained PAT able to open PRs and dispatch workflows — a compromised
watcher can at worst open noisy PRs that must still pass public gates.

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| `ci.yml` | PRs, pushes to `main` | §8.1 lint, entrypoint unit tests, native build + full E2E on both arch matrices — the merge gate |
| `release.yml` | merge of release PR / dispatch | rebuild both arches natively, all gates again, assemble manifest, tag, sign, attest, mirror; §9.6 degraded modes behind explicit inputs |
| `post-push-verify.yml` | after release | §8.5 pull-by-digest + cosign + SBOM/provenance presence |
| `capbisect.yml` | manual dispatch | §5.2 capability bisection, result committed via PR |

Native runners: `ubuntu-24.04` (amd64), `ubuntu-24.04-arm` (arm64) —
public GitHub-hosted, free for public repos, satisfying §6.6 and §4.5
simultaneously. QEMU appears nowhere except the §9.6 Mode 1 path.

## 5. Watcher design (`release-watcher` repo, Phase 6)

Single Go daemon (`watcherd`) + systemd timer units on your machine;
state persisted in git (§9bis.9). Config `watcher.yaml` lists images →
branches → sources. Per-source modules behind one interface
(`Poll(ctx, lastState) (events, newState, error)`), all using conditional
HTTP (ETag/If-Modified-Since) with retry + logging (§9bis.2):

- **samba-release** (9bis.1.a): `https://download.samba.org/pub/samba/stable/`
  directory listing + samba-announce mailing-list archive as push-ish
  channel; **armed mode**: when a pre-announced security window is known
  (flagged manually or parsed from announce), a tighter timer (1–2 min
  conditional polls) activates for that window.
- **pkg-index** (9bis.1.c): InRelease + Packages of trixie and
  trixie-security, filtered to `runtime-packages.txt` from the image
  repo; state = hash of the sorted (package, version) list; delta ⇒
  event carrying the new hash (becomes `PKG_INDEX_HASH` build arg).
  Raw-index republication with no version delta produces no event
  (§9bis.8.b).
- **base-digest** (9bis.1.c): HEAD manifest of the pinned
  `debian:trixie-slim` tag per arch.
- **osv-advisory** (9bis.1.d): OSV + Debian security tracker for SBOM
  components — advisory only: writes/updates `security/cve-exceptions.yaml`
  PRs and user-facing notices, never dispatches builds (§9bis.8.c).
- **lifecycle** (9bis.1.e): `pub/samba/rc/` listing + announce archive
  for new-series RCs and discontinuation notices → automatic deprecation
  and EOL notices + README matrix PRs (§9.4).

Pipeline per event: **confirm** against the authoritative source
(artifact + signature exist, version strictly greater — §9bis.8.a) →
**necessity check** (would a build input change? §9bis.8) → **soak**
(24 h for non-security upstream versions, 0 for security — §9bis.5;
implemented as a persisted pending-event queue, not a sleep) →
**dispatch**: open bump PR (edit `versions.yaml`, changelog stub with
trigger cause) via GitHub API; label `automerge` per §9bis.4 policy
(patch/rebuild/CVE auto-merge on green; minor/major waits for you).
Idempotency: event key = (source, identity, version/digest) persisted in
the state branch before dispatch (§9bis.3). Heartbeat: every timer run
pings the dead-man endpoint (D6) and commits a heartbeat timestamp
(§9bis.6). All emitted text in English (§9bis.9).

## 6. Risks and mitigations

- **Samba source build duration** (~30–60 min/arch): mitigated by
  BuildKit layer caching keyed on (base digest, tarball hash) — correct
  by §9.3 since those are exactly the build inputs; ccache is NOT used
  (cache opacity vs weak reproducibility §4.3).
- **`CAP_SYS_ADMIN` under GH-hosted runners**: containers run via the
  runner's root Docker daemon; granting caps to test containers is
  allowed. Validated in Phase 1 smoke before Phase 3 depends on it.
- **Two-DC replication test flakiness in CI**: bounded waits with
  explicit replication-status polling (`samba-tool drs showrepl`), not
  sleeps; flakes get quarantined-with-issue, never retried-silently.
- **arm64 runner availability** (§9.6): degraded modes implemented (not
  improvised) in Phase 4.
- **Key pinning**: if the two out-of-band sources disagree, stop and
  investigate — Phase 0 exit gate blocks on agreement.
- **`X.Y.Z-rN` vs Docker Hub semver sorting quirks**: aliases (`X.Y.Z`,
  `X.Y`) are the user-facing pins; documented in the update guide.

## 7. Spec coverage map (self-review)

§1 principles → phase ordering + §9bis design. §2 → Phase 0 (license,
notice) + Phase 1 (labels, names). §3 → Phase 4 (tags/aliases/digests) +
Phase 7 (§3.6 catalog). §4 → Phase 0 (key), Phase 1 (pins, verified
download), Phase 4 (SBOM/provenance/signature, CI-only, GHCR+mirror).
§5 → Phase 1 (base, packages), Phase 2 (healthcheck), Phase 3 (read-only
E2E, capbisect), Phase 4 (CVE + secrets gates). §6 → Phase 2 (modes,
codes, signals, logs, Go §6.7 — no shell interim, so B.6's §11.2
exception is never needed), Phase 4 (§6.6 native multi-arch + §9.6
modes). §7 → Phase 2 (guards) + Phase 3 (backup/restore/upgrade tests).
§8 → Phases 0/3/4 gates + post-push verify. §9/§9bis → Phase 6 watcher +
Phase 4 dispatch inputs; SLOs start at Phase 7. §10 → Phase 5 (+
CHANGELOG machinery in Phase 4). §11 → SPEC.md vendored (done), label in
Phase 1, exceptions dir Phase 0. §12/Annex B → adaptation profile seeded
Phase 0, capability set Phase 3, contract Phase 2, limitations kept
current each phase. Known intentional deferrals: B.6 roadmap items
(sysvol v2, Windows-client validation, SLSA L3).
