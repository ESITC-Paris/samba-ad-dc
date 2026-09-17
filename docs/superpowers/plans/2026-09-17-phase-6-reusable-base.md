# Phase 6 — Reusable Base Contract Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps
> use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `samba-ad-dc` a deliberately reusable base: a downstream
project (first known consumer: a school domain replacing a Windows
server) can derive an image from it, declare its Samba configuration
declaratively, bring its own TLS material, and drive administration
through a documented operator API — without patching this repository —
and every one of those extension points is proven by the E2E suite and
documented with its test ID.

**Architecture:** Two small, generic extension points are added to the
Go entrypoint under the existing contract discipline: `SAMBA_GLOBAL_OPTIONS`
(declarative `[global]` settings, applied idempotently through the
existing `withGlobalSetting` and validated with `testparm`), and
`SAMBA_TLS_CERT_FILE` / `SAMBA_TLS_KEY_FILE` / `SAMBA_TLS_CA_FILE` (operator
TLS material for LDAPS, expressed through the same mechanism). The
reuse contract itself is a document (`docs/reuse-guide.md`) whose every
claim is backed by a test or explicitly labelled as out of band. A
derived-image test proves that `FROM` this image inherits the whole
runtime contract. What the base does NOT do (file serving, printing,
DHCP, sysvol replication, real Windows-client validation) is stated as
boundaries with the recommended companion roles.

**Tech Stack:** Go (entrypoint + E2E), docker, POSIX sh, Markdown.

**Spec:** `SPEC.md` §§6.1, 6.2, 6.5, 6.7, 8.2, 10, 12; `docs/adaptation-profile.md`
(authoritative; Runtime contract + B.5/B.6); `docs/traceability.md`
(17 frozen IDs — new rows are ADDED, none renamed).

## Global Constraints

- Every new environment variable enters the Runtime contract table of the
  profile with modes, default and meaning, is validated in `config.Load`,
  refuses with exit 10 and a cause+remedy message, and never carries a
  secret (§6.1: file paths only).
- "A restart never modifies existing state" (§6.2) is about the
  directory state on `/var/lib/samba`. Declarative `[global]` options are
  configuration, reconciled on every start, idempotently, with one log
  line per change — the profile states this precisely.
- The entrypoint's shell-outs stay limited to Samba's own CLIs (§6.7):
  `testparm` validates the resulting configuration; no operator hook
  scripts are executed by the entrypoint (a downstream project runs its
  automation through `docker exec` / a sidecar — documented, not
  invented).
- Keys the entrypoint owns are refused in `SAMBA_GLOBAL_OPTIONS` (exit 10):
  `realm`, `workgroup`, `netbios name`, `server role`, `dns update command`,
  `ad dc functional level`, `dns forwarder`, `ntp signd socket directory`,
  `tls certfile`, `tls keyfile`, `tls cafile` (the TLS trio has its own
  variables), `include`.
- Every new E2E test is a new row in `docs/traceability.md` AND is cited
  in the guides (check (f)); `scripts/check-traceability.sh` stays green.
  Test IDs (new): `TestGlobalOptionsApplied`, `TestCustomTLSMaterial`,
  `TestDerivedImageInheritsContract`.
- Comments explain WHY, truthful and measured; English; commit style and
  trailers as before; stage by explicit path; push `main` at the end of
  each task and watch CI to green (`gh run watch … --exit-status`).
- Local docker is the test bed (`samba-ad-dc:dev` = 4.24.7; client image
  present). Never `docker system prune`.

---

### Task 1: `SAMBA_GLOBAL_OPTIONS` — declarative `[global]` settings

**Files:**
- Modify: `entrypoint/internal/config/config.go` (+`_test.go`),
  `entrypoint/internal/run/run.go` (+`_test.go`), `docs/adaptation-profile.md`
  (Runtime contract table + a "Declarative configuration" paragraph + B.5
  row + changelog), `docs/traceability.md` (new row), `README.md`
  (variable table row), `test/e2e/nominal_test.go` (new test).

**Interfaces (produced):**
- `SAMBA_GLOBAL_OPTIONS`: newline-separated `key = value` entries (compose
  `|` block scalar); blank lines and lines starting with `#` ignored; a
  line without `=` → exit 10; keys normalised (lower-case, single spaces);
  owned keys (see Global Constraints) → exit 10 naming the variable that
  owns them. Config field `GlobalOptions []GlobalOption{Key, Value}` in
  declaration order (duplicates: last wins, logged).
- Application: (a) provision: each entry passed as `--option=key = value`
  (the existing mechanism) AND reconciled after provision like join; (b)
  join: appended to `joinedConfSettings`; (c) every start (`ActStart`,
  `ActDBCheckThenStart`): `ensureGlobalOptions` runs before the daemons,
  using `withGlobalSetting` per entry, atomic rewrite, one log line per
  add/replace with the value, nothing logged when unchanged; (d) after
  any rewrite, `testparm -s -l <smb.conf>` must succeed, else the ORIGINAL
  file is restored and the boot refuses with exit 10 quoting testparm's
  first error line and the offending variable. Maintenance mode does not
  apply options.
- Log line format: `entrypoint: SAMBA_GLOBAL_OPTIONS: added "smb encrypt" = "required" in /etc/samba/smb.conf` / `replaced …`.

- [ ] **Step 1: config tests first** (`config_test.go`): parses two entries; ignores blanks/comments; normalises `Smb   Encrypt` → `smb encrypt`; refuses a line without `=` (exit 10, message names `SAMBA_GLOBAL_OPTIONS`); refuses each owned key with a message naming the owning variable (`SAMBA_REALM`, `SAMBA_DNS_FORWARDER`, `SAMBA_FUNCTION_LEVEL`, `SAMBA_TLS_*`, or "managed by the image"); duplicate key keeps the last value. Run red, implement, green.
- [ ] **Step 2: run tests** (`run_test.go`, fake runner): provision args carry `--option=key = value` per entry; join settings include them; start path calls `output:testparm` after a rewrite and NOT when unchanged; a failing testparm restores the original bytes and returns refusal 10 with the testparm line; idempotence (second start: no rewrite, no log). Extend the existing `TestSuperviseSignalWithRealProcesses`-style real-process test only if the fake cannot prove the restore (it can — file bytes).
- [ ] **Step 3: E2E `TestGlobalOptionsApplied`** (own DC, not the shared fixture): provision with `SAMBA_GLOBAL_OPTIONS` = `smb encrypt = required` + `log level = 1 auth:3` (two harmless, observable settings); assert `testparm -s --parameter-name="smb encrypt"` → `required` inside the DC; restart the container with a CHANGED value (`desired`) and assert the change applied after restart and the log carries the `replaced` line; a third start with an invalid key (`this is not a parameter = 1`) must exit 10 with the testparm-derived message and leave the previous smb.conf intact (assert via `docker cp` before/after). Use `harness.RunDCExpectExit` for the refusal and `WithVolumes` to reuse the volumes.
- [ ] **Step 4: docs** — profile Runtime contract row + "Declarative configuration" paragraph (what it is for, owned keys, testparm gate, restart semantics under §6.2), B.5 nominal clause "declarative global options" + traceability row `N9`, README variable row, deployment guide §3 short subsection citing the test. `sh scripts/check-traceability.sh` green.
- [ ] **Step 5: gates + commit** `feat(entrypoint): SAMBA_GLOBAL_OPTIONS — declarative, validated [global] settings`; push; CI green.

---

### Task 2: Operator TLS material for LDAPS

**Files:**
- Modify: `entrypoint/internal/config/config.go` (+test), `entrypoint/internal/run/run.go` (+test), profile, traceability, README, `docs/deployment-guide.md` §4.5, `test/e2e/nominal_test.go`.

**Interfaces:**
- `SAMBA_TLS_CERT_FILE`, `SAMBA_TLS_KEY_FILE`, `SAMBA_TLS_CA_FILE`: absolute paths INSIDE the container (bind-mounted by the operator, read-only). All three or none (exit 10 otherwise); each must exist and be readable at start (exit 11 — they are secret material, same class as `*_FILE`); the key file must not be world-readable? (Samba warns but works; do NOT enforce, document). They map to `tls certfile`, `tls keyfile`, `tls cafile` through the same reconciliation as Task 1 (owned keys, applied on provision/join/start). Unset → Samba's self-signed material (today's behaviour).

- [ ] **Step 1: config + run tests first**, then implement (reuse Task 1's machinery: the three settings are prepended to the reconciled list).
- [ ] **Step 2: E2E `TestCustomTLSMaterial`**: in the CLIENT container generate a CA + server cert for the DC's FQDN with `openssl` (the client image has `openssl` via ca-certificates? verify; if absent add `openssl` to `test/client/Dockerfile` with a WHY comment), copy them to a host temp dir, bind-mount into a fresh DC at `/etc/samba/tls/{cert,key,ca}.pem` (`/etc/samba` is a volume — bind a subdirectory instead, e.g. `/run/secrets/tls/*` read-only), provision with the three variables; assert from the client: `ldapsearch -H ldaps://<fqdn>` with `LDAPTLS_CACERT=<our ca>` succeeds and the presented certificate's issuer is OUR CA (`openssl s_client -connect <ip>:636 -servername <fqdn> </dev/null | openssl x509 -noout -issuer`); negative control: with the DC's own generated CA it fails (`ldapsearch` `TLS_REQCERT=demand`). Missing key file → exit 11 with a message naming `SAMBA_TLS_KEY_FILE`.
- [ ] **Step 3: docs** — profile rows + paragraph; B.5 clause + traceability `N10`; README rows; deployment guide §4.5 "Bring your own certificate" subsection citing the test; commit `feat(entrypoint): operator TLS material for LDAPS (SAMBA_TLS_*_FILE)`; push; CI green.

---

### Task 3: Derived-image proof

**Files:** Create `test/e2e/derived_test.go`; modify `docs/traceability.md`, profile B.5, `docs/reuse-guide.md` (Task 4 creates it — coordinate: this task adds the citation line if the file exists, else Task 4 does).

- [ ] **Step 1: `TestDerivedImageInheritsContract`**: `docker build` (via the harness's docker helpers, in a temp dir) a derived image `FROM <E2E_IMAGE>` that adds one file (`/usr/share/samba-ad-dc/derived-marker`) and one extra env (`DERIVED=1`) and nothing else; start it with `harness.StartDC(... harness.WithImage(derived))` in provision mode; assert: reaches healthy, the marker file exists, `entrypoint --version` reports the base Samba version, `docker inspect` shows the inherited `Healthcheck`, `Volumes` (both), `Entrypoint` (`tini -- entrypoint`), and `KRB5_CONFIG` env; then `RunDCExpectExit` with `SAMBA_MODE=run` on empty volumes → exit 21 (refusals inherited). Remove the derived image in cleanup (harness-owned label).
- [ ] **Step 2: traceability row `R2` (reuse) + B.5 clause "derived image inherits the runtime contract"; commit `test(e2e): a derived image inherits the whole runtime contract`; push; CI green.

---

### Task 4: The reuse contract — `docs/reuse-guide.md`, boundaries, Windows validation procedure

**Files:** Create `docs/reuse-guide.md`, `docs/windows-client-validation.md`; modify `README.md` (Documentation list + a "Building on this image" pointer), `docs/adaptation-profile.md` (B.6 boundaries restated for downstream projects; B.5 back-links), `docs/traceability.md` (cite the three new IDs from the reuse guide), `docs/superpowers/plans/2026-08-16-samba-ad-dc-roadmap.md` (Phase 6 note: reusable-base contract; v2 sysvol sync unchanged).

`docs/reuse-guide.md` sections (each with `Covered by:` or "out of band"):
1. **What this image is and is not** — an AD DC role only; the Samba
   Team's own guidance: no file serving on a DC beyond SYSVOL/NETLOGON
   (quote the wiki: not recommended, POSIX ACLs unsupported on a DC),
   no printing (CUPS compiled out), no DHCP, no sysvol replication
   between DCs (B.6), Windows-client join not exercised in CI (B.6).
   Recommended companions for a full "Windows server replacement":
   a domain-member file server (separate image/role), a print server,
   a DHCP server — each joined to this domain.
2. **Deriving an image** — `FROM ghcr.io/esitc-paris/samba-ad-dc:<X.Y>`;
   what is inherited (ENTRYPOINT/HEALTHCHECK/VOLUME/ENV/EXPOSE/labels);
   what a derived image must not change (entrypoint, volumes, the
   `SAMBA_*` contract); adding packages is the derived project's §5.1
   responsibility; `org.opencontainers.image.base.*` labels must be
   re-declared by the derived build. Covered by `TestDerivedImageInheritsContract`.
3. **Declarative configuration** — `SAMBA_GLOBAL_OPTIONS` with three
   worked examples (interfaces/bind interfaces only for multi-homed hosts;
   `smb encrypt = required`; `ldap server require strong auth`), the
   owned-keys rule and the testparm gate. Covered by `TestGlobalOptionsApplied`.
4. **Bring your own TLS** — `SAMBA_TLS_*_FILE`. Covered by `TestCustomTLSMaterial`.
5. **Operator API** — everything a downstream project automates goes
   through `docker exec <dc> samba-tool …` (or LDAP/Kerberos over the
   network) after the container is `healthy`: users/groups/OUs
   (`samba-tool user create`, `group add`, `ou create`), bulk import
   pattern (a loop over a CSV from a sidecar/one-off container of the
   SAME image with `--entrypoint samba-tool` against the volumes — state
   which operations need the DC running vs stopped), password policy
   (`samba-tool domain passwordsettings set`), GPOs (`samba-tool gpo
   create/setlink/…`; RSAT for content), delegation. Mark: "out of band —
   not covered by this image's suite; the downstream project tests its
   own automation".
6. **Compose pattern for a downstream project** — the DC service plus an
   `init` one-off that waits for `condition: service_healthy` and runs
   the project's `samba-tool` script; a `depends_on` example; secrets as
   files.
7. **Multi-DC** — join, DNS wiring, the sysvol caveat and the manual
   `rsync` procedure pointer (deployment guide §8).
8. **Windows clients** — pointer to `docs/windows-client-validation.md`.

`docs/windows-client-validation.md`: a generic, out-of-band checklist
(clearly headed "not executed by this project's CI — §12.4 limitation"):
prerequisites (client DNS = the DC, time within 5 min, the FQDN), join
(System → Domain), first domain logon, `gpupdate /force` with a test GPO
created by `samba-tool gpo create`, password change via Ctrl+Alt+Del
(kpasswd 464), `nltest /dsgetdc:`, `klist`, SMB signing/encryption
expectations for Windows 11 24H2, what to record and where (a dated
file under `docs/validations/`), and how a result feeds B.6.

- [ ] Write; `sh scripts/check-traceability.sh` green (the reuse guide cites the three new IDs; widen `GUIDES` if the checker does not read new files by default — read the script); links verified; commit `docs(reuse): the reuse contract, boundaries and the Windows-client validation procedure`; push; CI green.

---

## Phase exit gate

- [ ] Three new E2E tests green locally and in CI on both arches; traceability 20 ↔ 20 (17 + 3).
- [ ] Profile Runtime contract carries the four new variables with exit codes; B.5 and B.6 updated; README variable table current.
- [ ] `docs/reuse-guide.md` answers "how do I build a school domain on this" without touching this repo; boundaries stated plainly.
- [ ] `origin/main` green.
