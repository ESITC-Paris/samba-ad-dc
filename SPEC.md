# Enterprise Production-Grade Container Image Publishing Specification

**Version: 1.2 — Status: ratified**
*(1.1: §5.4 fixable-CVE definition clarified; §6.7 entrypoint
implementation language added. 1.2: §6.6 degraded modes and §9.6 platform
incident policy added.)*
**Language: English (normative). All repository content, release notes, notices and error messages produced under this specification are written in English.**

This document is the normative reference applicable to EVERY image published
under the project's brand. An image that does not satisfy every MUST
requirement is not published. The key words MUST / MUST NOT / SHOULD / MAY
are to be interpreted as described in RFC 2119.

External frameworks taken into account: SLSA v1.0, OpenSSF Scorecard,
CIS Docker Benchmark, NIST SP 800-190, Docker Official Images program
requirements, OCI Image Specification (annotations).

---

## 1. Purpose and scope

Produce container images for open source software whose upstream does not
publish a maintained official image, at a quality level suitable for
large-scale production use: full traceability, verifiable security,
predictable behavior, and a sustained maintenance commitment.

Scope covers: image content, build process, distribution, documentation,
lifecycle, and maintenance commitments. It does not cover operation on the
end user's side.

Guiding principles:

- **Event-driven, never speculative.** Nothing is built or published unless
  it is certain to be necessary (§9, §9bis).
- **Upstream decides, we relay.** No editorial decisions on software
  lifecycle; upstream status is applied as-is and communicated loudly (§9.4).
- **Tested = documented = supported.** These three sets are identical by
  construction (§8.2, §12).
- **Verifiable over declared.** Every quality claim maps to an automated,
  public check.

---

## 2. Identity, trademarks and legal compliance

2.1. Every image MUST display, in its README and OCI labels, an explicit
     non-affiliation notice with the upstream vendor/project.

2.2. Image names MUST be descriptive (`<org>/<software>-<role>`) and
     MUST NOT suggest official status. Upstream logos MUST NOT be used
     without written permission.

2.3. The license of every embedded component MUST be identified in the
     SBOM. Upstream license obligations (notices, GPL sources) MUST be
     satisfied — for modified GPL/LGPL components, the corresponding
     sources MUST be published or referenced.

2.4. Licensing (ratified): build repositories (Dockerfiles, scripts, CI,
     documentation) are licensed **Apache-2.0**; the OCI
     `org.opencontainers.image.licenses` label describes the **image
     content** and therefore carries the embedded software's license
     (e.g. `GPL-3.0-or-later` for Samba).

2.5. Mandatory OCI labels: `org.opencontainers.image.source`, `.version`,
     `.revision` (git SHA), `.licenses`, `.description`, `.created`,
     `.base.name`, `.base.digest`, plus the specification-conformance label
     defined in §11.1.

---

## 3. Versioning and tags

3.1. The primary tag MUST reflect the embedded upstream version, suffixed
     with an image revision: `X.Y.Z-rN`. Aliases `X.Y.Z`, `X.Y`, `X` MUST
     exist and point to the latest matching `-rN`.

3.2. A published `X.Y.Z-rN` tag is immutable: it MUST NOT be re-pushed.
     Any correction increments `-rN`.

3.3. `latest` MUST NOT be documented as production-usable; if it exists, it
     points to the latest stable version of the default branch. `edge`
     (built from `main`) MAY exist and MUST be marked non-production.

3.4. Every release MUST publish the image digest, and documentation MUST
     recommend digest pinning for production.

3.5. The mapping image tag ↔ upstream version ↔ build-repo git commit MUST
     be auditable (changelog + labels).

3.6. Catalog coverage: ALL upstream-supported stable branches MUST be
     published simultaneously (one entry per branch in the repository's
     versions file), and EVERY patch release of every branch MUST be
     published — no version skipping. `X.Y` tags track their branch; `X`
     and `latest` track the default (newest) branch. A new upstream branch
     MUST be added to the catalog within 10 business days of its first
     stable release; removal follows the lifecycle policy in §9.4.

---

## 4. Supply chain

4.1. The base image MUST be pinned by digest in the Dockerfile and updated
     through an automated, traceable mechanism (bot pull requests, never
     silent manual edits). Where builder and runtime stages are decoupled,
     each pin is independent and both follow this rule.

4.2. Every published image MUST ship with: an SBOM (SPDX or CycloneDX)
     attached in the registry, a provenance attestation (**SLSA Build
     Level 2**, ratified as the v1 target; Level 3 is a later goal), and a
     publicly verifiable cosign signature (keyless OIDC). The verification
     command MUST appear in the README.

4.3. Builds MUST be reproducible in the weak sense: same commit + same base
     digests ⇒ same software content (timestamped metadata may differ).
     Bit-for-bit reproducibility is a SHOULD-level goal.

4.4. No artifact MUST be downloaded at build time without integrity
     verification: a pinned checksum, or an upstream signature verified
     against a release key vendored in the repository. Key pinning MUST be
     performed out-of-band against at least two independent sources, and
     key rotation is a reviewed pull request referencing the upstream
     announcement.

4.5. Builds MUST run exclusively on auditable public CI infrastructure not
     controlled by the maintainers; no image is ever pushed from a
     workstation or private server. Local machines MAY be used for
     development iteration only.

4.6. Registries (ratified): **GHCR is the source of truth**; Docker Hub is
     maintained as a public mirror for discoverability. Signatures, SBOMs
     and attestations are published to the source of truth; the mirror
     references it.

---

## 5. Image content security

5.1. Minimal base (ratified: minimal Debian slim of the current stable
     release; distroless is NOT used where the software requires an
     interpreter and administration tooling). Every installed package MUST
     be justifiable — if nobody knows why it is there, it goes.

5.2. The main process MUST run as non-root, EXCEPT where technically
     impossible; the impossibility MUST be justified in the image's
     adaptation profile (§12). In that case the minimal capability list
     MUST be established by testing (capability bisection in CI, not by
     copying claims), documented, and provided in every deployment
     example. `--privileged` MUST NOT appear in documentation other than
     to forbid it.

5.3. No secret, key, default password or credential MUST be present in any
     layer (automated scan, e.g. gitleaks/trufflehog, on the final image).

5.4. Blocking vulnerability scan in CI: the gate applies ONLY to CVEs for
     which a **fixed version is available** from the relevant supplier —
     the distribution vendor for distribution-installed packages, or the
     upstream project for source-built components. Any such fixable
     HIGH/CRITICAL CVE ⇒ build failure. CVEs WITHOUT an available fixed
     version MUST NOT block the build (rebuilding would change nothing —
     consistent with §9bis.8.c): they are recorded in a versioned
     exceptions file with justification and a review date, and the arrival
     of the fixed version is itself a build trigger (§9bis.1.b/c).
     Scanners MUST therefore run in ignore-unfixed mode for the blocking
     gate, with the full (unfiltered) report retained as a build artifact.

5.5. A functional HEALTHCHECK MUST be defined: an application-level probe
     exercising the service's actual protocols, never a mere process
     check.

5.6. The image MUST support a read-only root filesystem where the software
     allows it, with writable paths explicitly delegated to declared
     volumes and tmpfs mounts; the E2E suite MUST run with the read-only
     configuration so that support is proven on every build, not declared.
     Where read-only rootfs is impossible, writable paths MUST be
     documented in the adaptation profile.

---

## 6. Runtime behavior

6.1. Configuration via documented environment variables; every secret MUST
     be consumed from a file (`*_FILE` convention) or a secrets manager.
     Secrets in environment variables MUST NOT be supported.

6.2. The entrypoint MUST be an explicit, idempotent state machine:
     a restart never modifies existing state; initialization operations
     (provision, join, migration) are distinct modes that refuse to run
     over existing state; a run-only mode MUST exist that refuses to start
     when expected state is absent (protection against silently
     re-initializing on a missing volume), and documentation MUST instruct
     production deployments to use it after initialization; a maintenance
     mode SHOULD exist for integrity checks and repairs without starting
     the daemon.

6.3. Clean shutdown: SIGTERM MUST trigger an orderly shutdown within the
     standard grace period (10 s by default, documented if longer). PID 1
     MUST reap zombie processes (tini or equivalent).

6.4. Logs to stdout/stderr exclusively; a structured format SHOULD be
     offered. No undocumented internal log files, and no log volumes.

6.5. Entrypoint exit codes and error messages MUST be actionable
     (cause + remedy), never a silent failure. Protective refusals are
     covered by negative tests (§8.2).

6.6. Multi-arch: linux/amd64 and linux/arm64 MUST be published under the
     same tag (manifest list) and MUST be fully tested on both, built on
     **native runners** for each architecture. Emulated builds are
     forbidden for published artifacts under normal operation; the sole
     exception is the security-driven degraded mode defined in §9.6, whose
     conditions (identical gates, identical trust boundary, mandatory
     disclosure) are strict and auditable.

6.7. Entrypoint implementation language: shell is acceptable only for
     trivial glue (**50 lines or fewer**, excluding comments and blank
     lines). Beyond that threshold, the entrypoint state machine MUST be
     implemented in a compiled language (**Go or Rust**), delivered as a
     statically linked binary built and verified by the same pipeline as
     the image (§4), with:
     - unit tests covering every state transition and every protective
       refusal of the state machine, run as a §8 gate;
     - shell-outs limited to the packaged software's own administration
       CLIs (which remain the supported interface for operations such as
       provisioning or integrity checks);
     - identical behavior contract: modes, refusals, exit codes and error
       messages remain those documented in the adaptation profile, and the
       E2E suite (§8.2) is the acceptance test for any reimplementation.
     A shell entrypoint exceeding the threshold in an existing image is a
     §11.2 exception: written, dated, and time-limited, with the rewrite
     as its exit condition.

---

## 7. Persistence, migration, backup

7.1. Data volumes MUST be explicitly declared and documented (content,
     required filesystem features such as xattr/ACL support where
     applicable).

7.2. Anti-regression guard: the image MUST refuse to start on state written
     by an upstream version newer than itself (downgrade = potential data
     corruption). The refusal message MUST state the remedy.

7.3. Schema/format migrations on upgrade MUST be either automatic and
     CI-tested (the N-1 → N upgrade test is mandatory) or explicitly
     manual with a runbook.

7.4. Every stateful image MUST document both a consistent backup procedure
     AND a restore procedure, and both MUST be exercised by the E2E suite.
     A hot volume snapshot is not a valid backup unless proven otherwise
     for the specific software.

---

## 8. Testing and CI (publication gates)

No image is pushed if any of these gates fails:

8.1. Lint: hadolint (Dockerfile), shellcheck (scripts), YAML validation.

8.2. Complete, blocking E2E suite: every candidate build MUST pass, on
     every published architecture, an end-to-end suite covering ALL
     documented use cases of the image —
     **nominal** (every entrypoint mode, every feature the documentation
     promises), **negative** (every protective refusal: missing secret,
     state overwrite, downgrade, missing state in run mode), and
     **operational** (idempotent restart, backup AND restore, upgrade path
     from the last published image).
     Traceability is required: every section of the user guides MUST
     reference the test that covers it, and vice versa — an untested
     feature is an undocumented feature, hence unsupported. A mere
     container start is not a test. Known gaps in the matrix MUST be
     listed as limitations in the image's documentation (§12).

8.3. Upgrade test: state produced by the last published tag of the same
     branch MUST start cleanly on the candidate (auto-skipped only for the
     very first release of a branch). Cross-branch upgrade paths that the
     documentation describes MUST also be tested.

8.4. Blocking CVE scan (§5.4) and secrets scan (§5.3).

8.5. Post-push verification, automated: pull by digest, cosign
     verification, presence of SBOM and provenance.

---

## 9. Maintenance: service-level commitments (ratified)

These values are contractual toward users; missed SLOs are handled per
§9.5.

9.1. Upstream release (patch or minor) → tag available: **5 business
     days**. In practice the automated pipeline targets under one hour for
     pre-announced security releases; the SLO covers detection or CI
     failures.

9.2. Fixable CRITICAL CVE (base or embedded software) → rebuild published:
     **48 hours**. HIGH: **7 days**.

9.3. No scheduled builds: every publication is triggered by a detected
     event (§9bis). Scheduled runs exist only for DETECTION (watcher
     polls), never to build "just in case". Corollary: build-cache
     invalidation MUST be deterministic and tied to the real state of the
     sources — the distribution package-index hash detected by the watcher
     is passed as a build argument and invalidates exactly the
     runtime-package installation layer, nothing more, nothing less.

9.4. Lifecycle: branch lifecycle is upstream's, applied as-is — no
     editorial decision. A branch is published while upstream supports it,
     removed when upstream discontinues it. The obligation is relaying,
     not deciding: the watcher detects upstream end-of-life signals
     (release candidates of a new series, discontinuation announcements)
     and AUTOMATICALLY publishes a deprecation notice upon detection (in
     practice several weeks before effective EOL, following the upstream
     release-candidate cadence), then the EOL notice when the branch
     leaves the catalog. A tag must never silently stop receiving
     rebuilds: the README compatibility matrix reflects each published
     branch's upstream status at all times.

9.5. Any missed SLO MUST be recorded publicly (short post-mortem in the
     repository). SLO accounting is **per architecture**.

9.6. Platform incidents and degraded publication modes. The §9.2 SLOs
     depend on public CI runner availability, notably native arm64 pools.
     When a platform incident (outage, sustained queueing) threatens a
     SECURITY-DRIVEN publication, the following modes apply, in order, and
     ONLY for security-driven builds — routine releases simply wait:

     **Mode 1 — emulated build on hosted infrastructure.** The affected
     architecture MAY be built under emulation (QEMU) on the public hosted
     CI. Conditions, all mandatory: the build runs on the same public,
     maintainer-uncontrolled infrastructure — the §4.5 trust boundary is
     unchanged, since emulation degrades speed, not provenance; EVERY §8
     gate runs identically, including the full E2E suite under emulation —
     no gate is waived; the release notes and changelog disclose the
     emulated build explicitly. If the E2E suite cannot run reliably under
     emulation, Mode 1 is unavailable and Mode 2 applies.

     **Mode 2 — architecture-staggered publication.** The available
     architecture(s) are published immediately as `X.Y.Z-rN` with a
     reduced manifest; the release notes state which architectures are
     pending and why. When the platform recovers, the complete multi-arch
     manifest is published as `X.Y.Z-r(N+1)` — tag immutability (§3.2) is
     preserved: the partial manifest is never amended in place — and the
     branch aliases move to it. Users on the pending architecture remain
     on the previous revision and are told so plainly, with the fix's
     availability tracked in the release notes.

     **Explicitly rejected: standby self-hosted runners.** A maintainer-
     controlled arm64 machine would restore speed at the cost of the §4.5
     trust boundary — the property the entire provenance story rests on.
     No SLO justifies weakening it.

     Every activation of a degraded mode MUST be logged with its trigger
     (platform incident reference), the mode used, and its resolution. An
     architecture that misses the §9.2 SLO despite degraded modes receives
     the §9.5 post-mortem.

---

## 9bis. Automated release detection (watch system)

An automated system ("the watcher") monitors every source that requires
monitoring, for each catalog image; builds are triggered by detected events
only.

9bis.1. **Monitored sources** — for each image, the watcher MUST track:
   a) new upstream version: release directory listings, git tags or Atom
      feeds, AND the upstream announcement mailing list where one exists
      (push channel); pre-announced security releases put the watcher in
      an "armed" mode of tight conditional polling on the announced
      window;
   b) new version of the embedded package when the image is built from a
      distribution's packages (in that case THIS trigger is authoritative,
      not the upstream release);
   c) new base-image digest, AND new version of any runtime package
      installed by the image: the watcher monitors the distribution's
      package index (InRelease/Packages of the stable and security suites,
      conditional requests) restricted to each image's runtime package
      list, and maintains per (image, branch) the hash of that versioned
      list — any delta triggers a build carrying that hash as a build
      argument (§9.3);
   d) publication of a CVE affecting an SBOM component (OSV feed /
      distribution security tracker) — advisory only, see 9bis.8.c;
   e) upstream lifecycle signals (release candidates of a new series,
      discontinuation announcements) feeding the §9.4 relay.

9bis.2. Detection MUST be polite toward sources: conditional requests
   (ETag / If-Modified-Since), feeds rather than scraping, rate-limit
   compliance. A failed poll MUST be retried and logged, never silent.

9bis.3. The watcher MUST be idempotent: it persists last-seen state per
   source and never triggers two builds for the same event.

9bis.4. Detection ≠ publication. A trigger produces a bump pull request
   (version/digest pin changed, changelog pre-filled with the trigger
   cause) which passes ALL gates in §8. Merge policy (ratified):
   - base rebuild, `-rN` bump, upstream patch, CVE fix: **auto-merge and
     publish if CI is green**;
   - upstream minor or major version: **human approval required**.

9bis.5. Soak delay (ratified): outside security fixes, a new upstream
   version is published after a **24-hour** soak period to absorb upstream
   retags and withdrawn releases. **Security fixes bypass the delay
   (0 hours).**

9bis.6. Watching the watcher: if the watcher has produced no heartbeat for
   more than 2 hours, an alert MUST be raised (dead-man switch). §9 SLOs
   are measured from the source event, not from its detection: the watcher
   is a means, not an excuse.

9bis.7. Scheduling: the watcher requires a real execution guarantee at its
   polling frequency. GitHub Actions cron does not provide one (runs are
   delayed or skipped under load): scheduling MUST be external (a machine
   of ours running a timer, or a cloud scheduler) triggering CI workflows
   via the API — the trigger does not need to be trusted, only the
   executor (§4.5): a compromised trigger can at worst start builds that
   publicly pass or fail the §8 gates.

9bis.8. **Necessity criterion**: a build is triggered only when it is
   CERTAIN to produce an image different from the last published one. A
   build is necessary if and only if at least one build input changed:
   embedded upstream version, versioned runtime package list, or
   base-image digest. The watcher MUST verify this criterion before any
   dispatch:
   a) a detected event is first CONFIRMED against the authoritative
      source — an announcement email is not sufficient: the watcher
      verifies that the release artifact and its signature exist on the
      upstream download server and that the version is strictly greater
      than the published one;
   b) for packages, the hash covers the (package, version) list filtered
      to the image's runtime packages — never the raw index, whose
      republications without version changes must trigger nothing;
   c) the CVE feed is ADVISORY: a CVE without an available fix never
      triggers a build (rebuilding would change nothing) — it feeds the
      exceptions file (§5.4) and user communication; the effective trigger
      is the arrival of the fixed package or release, already covered by
      (a)/(b);
   d) only manual dispatch (reason=manual) bypasses the criterion, and it
      is logged as such.

9bis.9. Implementation (ratified): the watcher is an **in-house daemon**
   (lightweight, state persisted in a git repository, timers on a machine
   of ours), because computing the filtered runtime-package hash of
   9bis.1.c and the lifecycle relay of 9bis.1.e are outside off-the-shelf
   tools' scope. All content the watcher writes (release notes,
   deprecation and EOL notices, changelog entries) is in English.

---

## 10. Minimum documentation per image

Each image repository MUST contain:

10.1. **README** — working copy-paste quickstart; exhaustive configuration
   variable table; non-negotiable deployment constraints (network,
   capabilities, filesystem); volumes and backup pointers; tag/pinning and
   support policy; the signature verification command; the non-affiliation
   notice; the compatibility matrix (upstream versions ↔ maintained tags ↔
   upstream support status, kept current by the watcher).

10.2. **Deployment guide** — from zero to a working instance: constraints
   first, verification of signatures before first run, initial setup,
   scale-out where applicable, day-2 basics (health, backup, time, logs).

10.3. **Update guide** — pinning strategies mapped to user profiles (branch
   pin recommended; immutable digest for regulated environments; `latest`
   discouraged and why); how to learn that an update exists (releases
   feed, notifier tools); step-by-step patch update; multi-instance
   rollout order; branch upgrade; EOL handling; rollback procedure
   (restore-based, never tag downgrade). Auto-appliers are explicitly
   discouraged for stateful critical services: the project's reactivity is
   in publishing fast; applying remains a supervised user decision.

10.4. **CHANGELOG** per release: upstream version, fixed CVEs, image
   changes, and the trigger cause (`samba-release | pkg-update |
   base-digest | manual` …) — generated from the watcher's necessity
   evidence, so every revision's existence is justified and no revision
   exists without cause.

10.5. **SECURITY.md** — private reporting channel and response times.

10.6. Every guide section carries a reference to the E2E test that covers
   it (§8.2 traceability).

---

## 11. Governance of this specification

11.1. This document is versioned; every published image references the
   specification version it conforms to via an OCI label
   (`org.<org>.spec-version`), and the specification is vendored in each
   image repository so audits are self-contained.

11.2. Any deviation from a MUST requirement requires a written, motivated,
   dated and time-limited exception in the image's repository.

11.3. This specification is reviewed whenever a new image is added to the
   catalog. Rules that turn out to be software-specific are moved to the
   relevant adaptation profile (§12) rather than kept as false generics.

---

## 12. Per-image adaptation profile

The specification is generic; each piece of software has hard constraints
and impossibilities. Every catalog image MUST be accompanied by an
**adaptation profile** (a document in its repository) containing:

12.1. Deviations from generic requirements and their technical
   justification (e.g. "non-root impossible because …"), each mapped to
   the §11.2 exception mechanism where applicable.

12.2. The exhaustive list of documented use cases — this list DEFINES the
   §8.2 E2E matrix.

12.3. Non-negotiable deployment constraints (network topology, required
   kernel/filesystem features, capability set established by testing).

12.4. Known limitations and untested areas, stated plainly.

---

## Annex A — Release checklist (every release PR)

- [ ] Tag conforms to `X.Y.Z-rN`; aliases updated; digest published
- [ ] Base image(s) pinned by digest and current
- [ ] SBOM + provenance + signature verified post-push (§8.5)
- [ ] CI green: lint; full E2E suite (all documented use cases, both
      architectures, read-only rootfs configuration); upgrade test;
      CVE scan; secrets scan
- [ ] CHANGELOG filled: upstream version, CVEs, changes, trigger cause
- [ ] README / compatibility matrix current
- [ ] CVE exceptions reviewed (no expired review dates)
- [ ] Adaptation profile still accurate (deviations, limitations)

---

## Annex B — Adaptation profile: Samba Active Directory Domain Controller

First instance of a §12 profile. Governs the `samba-ad-dc` image.

### B.1 Build strategy

Built **from verified upstream source** (GPG-verified release tarball
against a vendored, out-of-band-pinned release key), not from distribution
packages, in order to: (a) meet the §9 SLOs independently of distribution
packaging schedules, and (b) build the AD DC role with Samba's **bundled
Heimdal** Kerberos — the configuration upstream recommends for this role,
whereas distribution packages typically build it against MIT KRB5, which
upstream still flags as experimental for the AD DC. Consequence, stated
plainly: security integration responsibility is ours, with no distribution
safety net; the watcher monitors the Samba security announcement channel
directly, and pre-announced security releases use the armed detection mode
(§9bis.1.a). Layout is FHS so paths match distribution conventions.

### B.2 Deviations from generic requirements (§11.2 justifications)

- **§5.2 non-root: impossible.** The AD DC writes extended attributes in
  the `security.*` namespace, which requires `CAP_SYS_ADMIN`; it binds
  privileged ports (53, 88, 389, 445, 464, 636). Mitigation: minimal
  capability set instead of `--privileged`, established by CI capability
  bisection (working hypothesis: SYS_ADMIN, NET_BIND_SERVICE, CHOWN,
  FOWNER, DAC_OVERRIDE, SETUID, SETGID; candidates to bisect include
  DAC_READ_SEARCH), with `cap_drop: ALL` as the baseline in every example.
- **§5.6 read-only rootfs: supported and CI-proven.** Writable paths:
  `/var/lib/samba` (persistent volume: directory database, Kerberos
  secrets, sysvol, TLS material, NTP signing socket), `/etc/samba`
  (persistent volume: generated configuration), `/run` and `/tmp`
  (tmpfs). No writes under `/etc` at runtime.

### B.3 Non-negotiable deployment constraints

- **No NAT.** The DC registers its own IP in its own DNS; Kerberos and
  dynamic RPC reference it. Supported topologies: dedicated IP per DC via
  macvlan/ipvlan, or host networking. Port publishing on a bridge network
  is unsupported.
- **Filesystem:** the volume backing `/var/lib/samba` requires xattr and
  POSIX ACL support (ext4/xfs); NFS unsupported.
- **Time:** the container serves signed NTP (MS-SNTP via chrony, wired to
  Samba's signing socket) to domain members but does not discipline the
  clock by default — host time synchronization is the operator's
  responsibility (option exists to grant CAP_SYS_TIME instead).
- **Secrets:** file-based only (`*_FILE`); the domain administrator
  password is never accepted via plain environment variable.

### B.4 Entrypoint state machine (documented modes = tested modes)

`auto` (first-boot convenience: provisions or joins on empty state),
`provision` (new domain; refuses over existing state), `join` (additional
DC; refuses over existing state), `run` (production mode: refuses to start
if state is absent — protection against silent re-provisioning on a
missing volume; documentation instructs switching to it after
initialization), `maintenance` (database check/repair without starting the
daemon). Version guard: refuses to open state written by a newer Samba;
on upgrade, runs the database consistency check automatically.

### B.5 Documented use cases → E2E matrix (§8.2)

Nominal: provision; Kerberos authentication (kinit) and Kerberized SMB;
NTLM authentication path; DNS SRV records served; LDAPS with certificate;
signed-NTP wiring; database consistency. Additional-DC join with
bidirectional directory replication verified by object propagation both
ways. Operational: idempotent restart without state loss; offline backup
AND restore into a fresh instance with object-level verification; upgrade
from the last published tag of the branch with data intact; explicit
downgrade refusal. Negative: missing secret file fails fast with an
actionable message; provision over existing state refused; run mode
without state refused. All of the above executed on both architectures
with the read-only rootfs configuration.

### B.6 Known limitations (stated per §12.4)

- **§6.7 exception (time-limited):** the current entrypoint state machine
  is implemented in shell and exceeds the 50-line threshold. Rewrite in Go
  or Rust is committed for v1, with the existing E2E suite as the
  acceptance contract (identical modes, refusals, exit codes and
  messages); the shell version MUST NOT ship in a published release
  without this exception being formally recorded per §11.2.

- **Sysvol replication is not provided by Samba** (no DFS-R): with
  multiple DCs, group policy content does not replicate by itself. An
  integrated, tested synchronization mechanism from the PDC-emulator
  holder is committed roadmap (v2); until then this is a documented
  limitation with a manual procedure.
- **Real Windows-client domain join is not exercised in CI** (no Windows
  runners in the public pipeline); protocol-level equivalents are tested.
  A non-blocking out-of-band validation with a real Windows client is a
  roadmap item.
- **Cross-branch upgrade tests** (e.g. 4.21 → 4.22) are committed alongside
  multi-branch activation; intra-branch upgrades are tested on every
  build.
- Backup strategy: **offline backup is the primary, CI-tested path**
  (credential-free, automation-friendly); online backup is a documented
  alternative requiring administrator credentials.

### B.7 Catalog

All Samba stable branches currently supported upstream are published
simultaneously (§3.6), each receiving every patch release; branch
lifecycle relayed per §9.4 (release candidates of a new series trigger the
deprecation notice for the oldest branch).

---

*End of specification v1.0.*
