# Traceability — B.5 matrix ↔ E2E tests ↔ documentation

This file is the single place where the three sides of SPEC §8.2 / §10.6
are tied together:

- **§8.2** requires the E2E suite to cover *all* documented use cases of
  the image, and requires the mapping to be **bidirectional**: every
  documented use case names the test that covers it, and every test
  answers to a documented use case. An untested feature is an
  undocumented feature, hence unsupported.
- **§10.6** requires every guide section to carry a reference to the E2E
  test that covers it. The three guides now exist — `README.md`
  (§10.1), `docs/deployment-guide.md` (§10.2) and `docs/update-guide.md`
  (§10.3) — and each carries its citations inline; the two `docs/`
  guides close with a *Section → test map* table as well, the README
  does not. The **doc section** column below is the reverse index: for
  each row, the guide section that documents that use case.
  The row set and the test IDs did not change when the column was
  filled in; they were the same 17 Phase 3 froze. **N9** and **N10** are
  the rows added since, both in Phase 6, each together with the test that
  covers it.

The rows come from **B.5 “Documented use cases → E2E matrix”** in
`docs/adaptation-profile.md`. The mapping is **one row ↔ one test ID**,
in both directions.

## Test IDs are frozen

The Go function names in `test/e2e/*_test.go` **are** the traceability
IDs. They are a published contract, not an implementation detail:

- **Do not rename a test function.** A rename silently breaks every
  reference to it — in this file, in the three guides, and in any
  postmortem or issue that cites a failing test by name. Check (f) of
  the script catches the guides; nothing catches a postmortem.
- **Do not delete a test without deleting its B.5 row**, and do not add
  a B.5 row without adding its test. Either half alone is a matrix gap,
  which §8.2 requires to be documented as a limitation (§12.4).
- Adding a test means adding a row here in the same commit.

`scripts/check-traceability.sh` enforces exactly this, and runs in CI's
`lint` job. Its check (f) extends the same discipline to the guides, in
both directions: every ID in the table below is cited by at least one
guide, and every backticked `` `Test…` `` token in the three guides is
either a row of the table or one of the exempt infrastructure tests.
Matching is on the whole backticked ID — `TestProvision` is a prefix of
`TestProvisionOverStateRefused`, and a substring match would report the
shorter one as cited whenever the longer one is.

It is a *structural* check (names on both sides line up); it cannot tell
whether a test actually exercises what its row claims, nor whether the
section an anchor points at is the right one. That part is the
reviewer's job.

## Matrix

| B.5 matrix row | E2E test ID | file | doc section |
|---|---|---|---|
| **N1** — provision (nominal single-DC bring-up: realm, domain, marker) | `TestProvision` | `test/e2e/nominal_test.go` | [deployment §3 The first domain controller](deployment-guide.md#3-the-first-domain-controller) |
| **N2** — Kerberos authentication (`kinit` against the realm's KDC) | `TestKerberosKinit` | `test/e2e/nominal_test.go` | [deployment §4.2 Kerberos](deployment-guide.md#42-kerberos) |
| **N3** — Kerberized SMB (ticket-authenticated share access) | `TestKerberizedSMB` | `test/e2e/nominal_test.go` | [deployment §4.3 Kerberized SMB](deployment-guide.md#43-kerberized-smb) |
| **N4** — NTLM authentication path | `TestNTLMAuth` | `test/e2e/nominal_test.go` | [deployment §4.4 NTLM](deployment-guide.md#44-ntlm) |
| **N5** — DNS SRV records served for the realm | `TestDNSSRVRecords` | `test/e2e/nominal_test.go` | [deployment §4.1 DNS SRV records](deployment-guide.md#41-dns-srv-records) |
| **N6** — LDAPS with certificate | `TestLDAPSCertificate` | `test/e2e/nominal_test.go` | [deployment §4.5 LDAPS with the DC's own certificate](deployment-guide.md#45-ldaps-with-the-dcs-own-certificate) |
| **N7** — signed-NTP wiring (MS-SNTP, `SAMBA_CHRONY`) | `TestSignedNTPWiring` | `test/e2e/nominal_test.go` | [deployment §4.6 Time](deployment-guide.md#46-time) |
| **N8** — database consistency (`dbcheck`, maintenance mode) | `TestDBConsistency` | `test/e2e/nominal_test.go` | [deployment §6.4 Database consistency](deployment-guide.md#64-database-consistency) |
| **N9** — declarative `[global]` options (`SAMBA_GLOBAL_OPTIONS`): applied, reconciled on restart, refused when samba's parser rejects them | `TestGlobalOptionsApplied` | `test/e2e/config_test.go` | [deployment §3.5 Declarative `[global]` settings](deployment-guide.md#35-declarative-global-settings) |
| **N10** — operator-supplied LDAPS material (`SAMBA_TLS_CERT_FILE` / `SAMBA_TLS_KEY_FILE` / `SAMBA_TLS_CA_FILE`): served and verified by a client trusting only that CA; incomplete trio and unusable file refused | `TestCustomTLSMaterial` | `test/e2e/tls_test.go` | [deployment §4.5 Bring your own certificate](deployment-guide.md#bring-your-own-certificate) |
| **R1** — additional-DC join with bidirectional directory replication, verified by object propagation both ways | `TestJoinReplicationBothWays` | `test/e2e/replication_test.go` | [deployment §5 Scale-out: an additional DC](deployment-guide.md#5-scale-out-an-additional-domain-controller) |
| **O1** — idempotent restart without state loss | `TestIdempotentRestart` | `test/e2e/operational_test.go` | [deployment §3.4 Switch to `SAMBA_MODE=run`](deployment-guide.md#34-switch-to-samba_moderun-and-why) |
| **O2** — offline backup **and** restore into a fresh instance, with object-level verification | `TestOfflineBackupRestore` | `test/e2e/operational_test.go` | [deployment §7 Backup and restore runbook](deployment-guide.md#7-backup-and-restore-runbook) |
| **O3** — upgrade from the last published tag of the branch, data intact (§8.3) | `TestUpgradeFromLastPublished` | `test/e2e/operational_test.go` | [update §3 Patch update, step by step](update-guide.md#3-patch-update-step-by-step) |
| **O4** — explicit downgrade refusal | `TestDowngradeRefused` | `test/e2e/operational_test.go` | [update §7 Rollback](update-guide.md#7-rollback) |
| **X1** — missing secret **file** fails fast with an actionable message | `TestMissingSecretFailsFast` | `test/e2e/negative_test.go` | [deployment §1.5 Secrets: files only](deployment-guide.md#15-secrets-files-only) |
| **X2** — secret offered as a plain environment variable refused (the “secret *file*” half of the same B.5 clause; runtime contract → *Environment variables*, §6.1) | `TestPlainEnvSecretRejected` | `test/e2e/negative_test.go` | [deployment §1.5 Secrets: files only](deployment-guide.md#15-secrets-files-only) |
| **X3** — provision over existing state refused | `TestProvisionOverStateRefused` | `test/e2e/negative_test.go` | [deployment §3.4 Switch to `SAMBA_MODE=run`](deployment-guide.md#34-switch-to-samba_moderun-and-why) |
| **X4** — run mode without state refused | `TestRunModeWithoutStateRefused` | `test/e2e/negative_test.go` | [deployment §3.4 Switch to `SAMBA_MODE=run`](deployment-guide.md#34-switch-to-samba_moderun-and-why) |

19 B.5 rows ↔ 19 tests.

The **doc section** links are relative to this file's directory, and each
names the *primary* section — the one whose subject is that use case.
Several rows legitimately share one (§1.5 documents both secret refusals,
§3.4 both state refusals and the restart), and several use cases are
touched by a second guide as well; the forward index is the *Section →
test map* closing each of the two `docs/` guides, plus the README's
inline `Covered by:` lines, and check (f) of
`scripts/check-traceability.sh` holds the citation set to exactly this
table's, in both directions.

### Cross-cutting properties, deliberately not rows

B.5 closes with “*all of the above executed on both architectures with
the read-only rootfs configuration*”. That is a property **of every row**,
not a row of its own, and it is enforced structurally rather than by a
test of its own:

- **Read-only rootfs + dropped capabilities** — every DC the suite starts
  goes through `harness.StartDC`, which applies the constrained profile:
  `--read-only`, the tmpfs set (`harness.DefaultTmpfs`), `--cap-drop
  ALL` plus `harness.DefaultCaps`, and `--security-opt
  no-new-privileges:true` (`harness.DefaultSecurityOpt`). That is the
  same profile §1.8 of the deployment guide publishes, flag for flag. No
  test can opt out, so there is nothing to cover separately. The
  capability set itself is derived and recorded by `test/capbisect/` (see
  B.5's neighbours in `docs/adaptation-profile.md`).
- **Both architectures** — the CI `build` job is a matrix over `amd64`
  and `arm64` native runners and runs the whole suite on each leg. The
  architecture axis is a CI dimension, not a test.

### Infrastructure tests (exempt from the matrix)

Two functions in `test/e2e` match `^func Test` but are **not** B.5 rows,
and `scripts/check-traceability.sh` exempts them by name. They must not
appear in the table above:

- `TestMain` (`test/e2e/main_test.go`) — the suite's entry point:
  preflight (docker CLI + image under test present), leftover sweep, and
  package-lifetime teardown. It asserts nothing about the product.
- `TestHarnessSmoke` (`test/e2e/smoke_test.go`) — the harness's test of
  *itself*: network creation, secret files, a constrained DC start, the
  health wait, `samba-tool` exec, clean stop, test-client image. It
  exists so that a broken harness fails as a harness failure instead of
  as sixteen confusing product failures. Its subject is the test code,
  not the image's documented behaviour.

If a third infrastructure test is ever added, it goes in this list *and*
in the script's exemption list — the script fails if an exempt name no
longer names a real function, so the two cannot drift apart.

## Known gaps

None. Every B.5 row has a test.

Two tests skip *loudly* under conditions the spec anticipates, which is a
coverage caveat rather than a gap:

- `TestUpgradeFromLastPublished` skips while `E2E_UPGRADE_FROM` is unset
  — the first release of a branch has no published tag to upgrade from
  (§8.3) — and skips again if the named tag ships the same Samba, where
  there is no upgrade to observe. Both skips name the variable.
- `TestJoinReplicationBothWays` refuses to start under a `go test`
  deadline too short for it, rather than being killed mid-run.
