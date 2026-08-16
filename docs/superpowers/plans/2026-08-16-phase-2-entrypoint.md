# Phase 2 — Go Entrypoint State Machine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps
> use checkbox (`- [ ]`) syntax for tracking. TDD is REQUIRED for every
> Go task: the brief's test code is written and seen failing before the
> implementation code.

**Goal:** A statically linked Go `entrypoint` binary implementing the B.4
state machine (`auto|provision|join|run|maintenance`), the §7.2 version
guard, `*_FILE`-only secrets, actionable exit codes, signal-clean process
supervision of samba+chrony, and an application-level `healthcheck`
subcommand — built inside the image pipeline, wired as ENTRYPOINT and
HEALTHCHECK, with unit tests covering every state transition and every
protective refusal as a blocking CI gate (§6.7).

**Architecture:** Small packages with one responsibility each under
`entrypoint/`: `config` (env + secret-file loading and validation),
`state` (state detection, version marker, upgrade/downgrade guards),
`modes` (a PURE decision engine: (mode, observed state, config) → an
action plan or a typed refusal — this is where every transition/refusal
lives and is unit-tested exhaustively), `run` (process supervision:
chronyd + samba, SIGTERM orderly shutdown), `health` (DNS/LDAP/SMB
probes). `cmd/entrypoint/main.go` wires them and maps typed refusals to
exit codes. Shell-outs go through one injected `Runner` interface —
production runs `samba-tool`/`samba`/`smbclient`, tests record and fake.

**Tech Stack:** Go ≥1.24 (`CGO_ENABLED=0`), `github.com/go-ldap/ldap/v3`
(healthcheck rootDSE), stdlib elsewhere. Docker multi-stage: dedicated
`gobuild` stage (digest-pinned golang image) added to the Dockerfile.

**Spec:** `SPEC.md` v1.2 — §§5.5, 6.1–6.5, 6.7, 7.2, B.3, B.4. Roadmap
Phase 2. The behavior contract below is ALSO written into
`docs/adaptation-profile.md` (Task 6) — profile and code must agree.

## Global Constraints

- Secrets ONLY via `*_FILE` (§6.1): plain `SAMBA_ADMIN_PASSWORD` or
  `SAMBA_JOIN_PASSWORD` set in the environment ⇒ refusal (exit 10)
  telling the operator to use the `_FILE` variant. Never log secret
  values; never write them to disk outside samba's own state.
- Exit codes (contract, immutable once released): `0` success; `10`
  configuration error; `11` missing/unreadable secret file; `20`
  provision/join refused over existing state; `21` run mode with absent
  state; `22` downgrade refusal; `23` database consistency check
  failure; `30` samba runtime failure. Every failure message: cause +
  remedy, one line each, to stderr (§6.5).
- A restart never modifies existing state (§6.2); marker writes happen
  only after successful initialization or successful upgrade check.
- SIGTERM → orderly stop of samba then chrony within 10 s (§6.3);
  entrypoint runs under tini (PID 1, from Phase 1 image).
- Logs to stdout/stderr exclusively (§6.4): samba started with
  `--foreground --no-process-group --debug-stdout`.
- Shell-outs limited to samba's own CLIs + chronyd (§6.7).
- Go build happens in the image pipeline (same Dockerfile), not on the
  host (§6.7/§4.5); unit tests are a blocking CI gate.
- Read-only rootfs holds: writable paths only `/var/lib/samba`,
  `/etc/samba`, `/run`, `/tmp` (B.2). Chrony config is baked into the
  image (static); its runtime/drift dirs live under `/run/chrony` and
  `/var/lib/samba/chrony`.
- Commit trailers: end every commit message with a blank line then
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` and
  `Claude-Session: https://claude.ai/code/session_017W9EQ8xaPCLR1JKL1bsvRU`.

## Behavior contract (locked here; copied into the adaptation profile)

Environment variables:

| Variable | Modes | Default | Meaning |
|---|---|---|---|
| `SAMBA_MODE` | all | `auto` | `auto|provision|join|run|maintenance` |
| `SAMBA_REALM` | provision, join (and auto reaching them) | — required | Kerberos realm / AD DNS domain, e.g. `AD.EXAMPLE.COM` |
| `SAMBA_DOMAIN` | provision | first label of realm | NetBIOS domain name |
| `SAMBA_ADMIN_PASSWORD_FILE` | provision | — required | file with the initial Administrator password |
| `SAMBA_JOIN_USERNAME` | join | `Administrator` | account used to join |
| `SAMBA_JOIN_PASSWORD_FILE` | join | — required | file with the join account password |
| `SAMBA_DNS_FORWARDER` | provision | none | upstream DNS forwarder IP |
| `SAMBA_DNS_BACKEND` | provision, join | `SAMBA_INTERNAL` | only `SAMBA_INTERNAL` supported in v1 |
| `SAMBA_FUNCTION_LEVEL` | provision | `2016` | AD functional level |
| `SAMBA_LOG_LEVEL` | all | `1` | samba debug level |
| `SAMBA_CHRONY` | auto/provision/join/run | `on` | serve MS-SNTP signed time (`on|off`) |
| `SAMBA_MAINTENANCE_OP` | maintenance | `check` | `check` (dbcheck) or `repair` (dbcheck --fix --yes) |

Mode semantics (B.4):
- `auto`: state present → behave as `run`; state absent → `join` if
  `SAMBA_JOIN_PASSWORD_FILE` is set, else `provision`.
- `provision`: state present → exit 20. Else `samba-tool domain
  provision`, write marker, start daemons.
- `join`: state present → exit 20. Else `samba-tool domain join ... DC`,
  write marker, start daemons.
- `run`: state absent → exit 21 ("volume missing or not mounted —
  mount the /var/lib/samba volume, or run an initialization mode").
  Else guards, then start daemons.
- `maintenance`: state absent → exit 21. Else run dbcheck (or --fix),
  print summary, exit without starting daemons (0 on clean, 23 on
  failure).

State & guards:
- State present ⇔ `/var/lib/samba/private/sam.ldb` exists.
- Marker `/var/lib/samba/.image-state.json`:
  `{"samba_version": "4.24.6", "initialized_at": "<RFC3339>", "last_mode": "provision"}`.
- Marker version > image version ⇒ exit 22 ("state was written by Samba
  X — deploy image tag X or newer, or restore a backup taken on this
  version").
- Marker version < image version ⇒ upgrade path: `samba-tool dbcheck`
  first; failure ⇒ exit 23; success ⇒ marker updated to image version,
  then start.
- State present but marker absent (foreign/pre-existing volume): log a
  warning, run dbcheck (failure ⇒ 23), adopt by writing the marker.
- Timestamps come from the clock at runtime; version from a var set at
  build (`-ldflags -X main.sambaVersion=<v>`).

---

### Task 1: `config` package

**Files:**
- Create: `entrypoint/go.mod`, `entrypoint/internal/config/config.go`,
- Test: `entrypoint/internal/config/config_test.go`

**Interfaces:**
- Produces:
  ```go
  package config
  type Mode string // "auto","provision","join","run","maintenance"
  type Config struct {
      Mode Mode; Realm, Domain string
      AdminPasswordFile, JoinPasswordFile, JoinUsername string
      DNSForwarder, DNSBackend, FunctionLevel string
      LogLevel int; Chrony bool; MaintenanceOp string
  }
  // Load reads and validates from env (getenv injected for tests).
  // Returned error is a *Refusal carrying Code (10 or 11) and a
  // cause+remedy message.
  func Load(getenv func(string) string) (*Config, error)
  // ReadSecret reads and trims the file named by cfg field; missing or
  // unreadable file -> *Refusal{Code:11}.
  func ReadSecret(path string) (string, error)
  type Refusal struct { Code int; Msg string }
  func (r *Refusal) Error() string
  ```
- `Refusal` is THE typed error every later package reuses.

TDD cases (write failing first): invalid mode → 10; plain
`SAMBA_ADMIN_PASSWORD` set → 10 with remedy naming
`SAMBA_ADMIN_PASSWORD_FILE`; plain `SAMBA_JOIN_PASSWORD` → 10;
provision without realm → 10; provision without admin password file →
10 (message names the variable); unsupported `SAMBA_DNS_BACKEND` → 10;
domain defaulting from realm first label; log level non-integer → 10;
`ReadSecret` on missing file → 11; on file with trailing newline →
trimmed value; defaults all applied on empty env (mode auto, chrony on,
function level 2016, join username Administrator).

Steps: write test file → `go test ./internal/config/` fails (package
missing) → implement → tests pass → `gofmt -l .` empty, `go vet ./...`
clean → commit `feat(entrypoint): config package with *_FILE-only secret
loading`.

---

### Task 2: `state` package

**Files:**
- Create: `entrypoint/internal/state/state.go`
- Test: `entrypoint/internal/state/state_test.go`

**Interfaces:**
- Consumes: `config.Refusal`.
- Produces:
  ```go
  package state
  type Marker struct { SambaVersion, InitializedAt, LastMode string }
  type Observation struct { Present bool; Marker *Marker } // Marker nil if absent
  // Observe inspects dir (production: /var/lib/samba).
  func Observe(dir string) (Observation, error)
  func WriteMarker(dir string, m Marker) error   // 0600, atomic rename
  // CompareVersions returns -1/0/1 for a<b, a==b, a>b on X.Y.Z strings;
  // error on malformed input.
  func CompareVersions(a, b string) (int, error)
  ```

TDD cases: empty dir → Present false; dir with `private/sam.ldb` →
Present true, Marker nil when no marker file; marker round-trip
(write then observe); corrupt marker JSON → error (not a silent nil);
version compare table incl. `4.24.6` vs `4.24.10` (numeric, not
lexicographic), equal, malformed → error; WriteMarker is atomic (temp
file + rename; test: no partial file left on simulated failure path)
and 0600.

Steps: failing tests → implement → pass → vet/fmt → commit
`feat(entrypoint): state observation and version marker`.

---

### Task 3: `modes` decision engine (the transition/refusal matrix)

**Files:**
- Create: `entrypoint/internal/modes/modes.go`
- Test: `entrypoint/internal/modes/modes_test.go`

**Interfaces:**
- Consumes: `config.Config`, `config.Refusal`, `state.Observation`,
  `state.CompareVersions`.
- Produces:
  ```go
  package modes
  type ActionKind int
  const (
      ActProvision ActionKind = iota // then start daemons
      ActJoin                        // then start daemons
      ActStart                       // start daemons only
      ActDBCheckThenStart            // upgrade or adoption path
      ActMaintenance                 // dbcheck/repair, no daemons
  )
  type Plan struct { Kind ActionKind; AdoptMarker bool; Repair bool }
  // Decide is PURE: no I/O. imageVersion e.g. "4.24.6".
  func Decide(cfg *config.Config, obs state.Observation, imageVersion string) (Plan, *config.Refusal)
  ```

TDD: full matrix, one test per cell (names double as the §6.7 evidence):
- auto × absent × no join creds → ActProvision
- auto × absent × join creds → ActJoin
- auto × present (marker == image) → ActStart
- provision × absent → ActProvision; provision × present → refusal 20
  (message: state exists + how to keep it (`run`) or wipe it)
- join × absent → ActJoin; join × present → refusal 20
- run × absent → refusal 21 (message includes missing-volume remedy);
  run × present (marker == image) → ActStart
- any-start-mode × marker > image → refusal 22 (message names both
  versions + remedy)
- any-start-mode × marker < image → ActDBCheckThenStart
- present + marker nil (foreign volume) → ActDBCheckThenStart with
  AdoptMarker true
- maintenance × absent → refusal 21; maintenance × present → 
  ActMaintenance (Repair true iff SAMBA_MAINTENANCE_OP=repair)
- malformed marker version → refusal 10

Steps: failing tests (matrix table-driven) → implement → pass →
commit `feat(entrypoint): pure mode decision engine covering every
transition and refusal`.

---

### Task 4: `run` package — execution and supervision

**Files:**
- Create: `entrypoint/internal/run/run.go`, `entrypoint/internal/run/runner.go`
- Test: `entrypoint/internal/run/run_test.go`

**Interfaces:**
- Consumes: `modes.Plan`, `config.Config`, `state.WriteMarker`.
- Produces:
  ```go
  package run
  type Runner interface { // the single shell-out seam (§6.7)
      Run(ctx context.Context, name string, args ...string) error          // waits
      Start(ctx context.Context, name string, args ...string) (Proc, error) // daemon
  }
  type Proc interface { Signal(os.Signal) error; Wait() error }
  func NewExecRunner(stdout, stderr io.Writer) Runner
  // Execute performs the plan: provision/join via samba-tool, dbcheck,
  // marker writes, then Supervise (unless maintenance). Secrets are
  // passed to samba-tool via --option or stdin, NEVER argv-visible
  // password flags where samba-tool supports an alternative; where only
  // a flag exists, document it (container-local ps only).
  func Execute(ctx context.Context, r Runner, cfg *config.Config, plan modes.Plan, stateDir, imageVersion string) *config.Refusal
  // Supervise starts chronyd (if cfg.Chrony) then samba in foreground,
  // forwards SIGTERM/SIGINT, stops samba first then chronyd, returns
  // samba's exit as Refusal{30} on failure.
  func Supervise(ctx context.Context, r Runner, cfg *config.Config) *config.Refusal
  ```
- Exact commands (binding):
  - provision: `samba-tool domain provision --server-role=dc
    --use-rfc2307 --dns-backend=SAMBA_INTERNAL --realm=<R> --domain=<D>
    --function-level=<FL> [--option=dns forwarder=<IP>]` with the admin
    password supplied non-interactively (`--adminpass` read from the
    secret file — see argv note above).
  - join: `samba-tool domain join <realm> DC -U"<user>"
    --dns-backend=SAMBA_INTERNAL` password via stdin (`--password`
    alternative note applies).
  - dbcheck: `samba-tool dbcheck` (+ `--fix --yes` when Repair).
  - samba: `samba --foreground --no-process-group --debug-stdout
    -d <LogLevel>`.
  - chrony: `chronyd -d -x -f /etc/chrony/chrony.conf` (`-x`: never
    steps host clock — B.3; `-d`: foreground/stderr).

TDD (fake Runner records calls; fake Proc controllable): provision plan
runs samba-tool then writes marker then starts daemons in order
chronyd→samba; join same shape; ActStart runs no samba-tool;
ActDBCheckThenStart runs dbcheck before daemons, marker updated on
success, Refusal 23 when dbcheck fails (and no daemon started);
AdoptMarker writes marker with image version; maintenance runs dbcheck
and never Start; failed provision → no marker written (restart stays
initializable); SIGTERM during Supervise → samba signaled before
chronyd, both reaped, exit nil; samba exiting non-zero → Refusal 30.
For the signal test use real processes via NewExecRunner with
`/bin/sleep` stand-ins (inject binary names via cfg for testability) —
this is the one place fakes are not enough.

Commit `feat(entrypoint): plan execution and daemon supervision with
orderly shutdown`.

---

### Task 5: `health` package + subcommand

**Files:**
- Create: `entrypoint/internal/health/health.go`
- Test: `entrypoint/internal/health/health_test.go`

**Interfaces:**
- Consumes: `run.Runner` (for the smbclient probe), realm from
  generated config: read `/etc/samba/smb.conf` `realm =` line.
- Produces:
  ```go
  package health
  // Check probes the running DC (§5.5): (1) DNS SRV _ldap._tcp.<realm>
  // against 127.0.0.1:53 via net.Resolver custom Dial; (2) LDAP rootDSE
  // read on ldap://127.0.0.1:389 (go-ldap, anonymous); (3) SMB share
  // enumeration via `smbclient -L 127.0.0.1 -N` through Runner.
  // Returns nil only if ALL pass; error names the first failing probe.
  func Check(ctx context.Context, r run.Runner, smbConfPath string) error
  ```

TDD: realm parser (table: normal, spaces, missing → error); DNS probe
against an in-test stub DNS server (miekg-free: answer a canned SRV via
net.PacketConn — keep the stub minimal); LDAP probe against a listener
that speaks just enough BER for a rootDSE bind+search response is
excessive — instead inject the two network probes as function fields on
a Prober struct with defaults, unit-test the orchestration (all pass /
first fails / smbclient fails via fake Runner), and leave the real
DNS/LDAP paths to the Phase 3 E2E healthcheck test (which asserts
`docker inspect` health goes healthy — recorded in traceability).
Commit `feat(entrypoint): application-level health probes`.

---

### Task 6: main wiring, Dockerfile integration, contract docs

**Files:**
- Create: `entrypoint/cmd/entrypoint/main.go`, `chrony/chrony.conf`
- Modify: `Dockerfile`, `.github/workflows/ci.yml`,
  `docs/adaptation-profile.md`

**Interfaces:**
- Consumes: everything above.
- Produces: image whose ENTRYPOINT is
  `["/usr/bin/tini","--","/usr/local/bin/entrypoint"]`, HEALTHCHECK
  `CMD ["/usr/local/bin/entrypoint","healthcheck"]`, and the documented
  contract in the adaptation profile.

Steps:
1. `main.go`: arg `healthcheck` → health.Check path (timeout 10 s);
   otherwise Load → Observe → Decide → Execute; map `*Refusal` to
   `os.Exit(code)` after printing `ERROR: <cause>. Remedy: <remedy>` to
   stderr; version injected via `-ldflags "-X main.sambaVersion=..."`.
2. `chrony/chrony.conf` (baked, read-only-friendly):
   ```
   # MS-SNTP time service for domain members (SPEC B.3).
   ntpsigndsocket /var/lib/samba/ntp_signd
   allow all
   driftfile /var/lib/samba/chrony/drift
   pidfile /run/chrony/chronyd.pid
   # No servers configured: host disciplines the clock; chrony serves
   # local time to clients (stratum from local).
   local stratum 10
   port 123
   cmdport 0
   ```
3. Dockerfile: add digest-pinned `golang:1.24-trixie` (or current)
   `gobuild` stage: `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X
   main.sambaVersion=${SAMBA_VERSION}" ./cmd/entrypoint` AND `go test
   ./...` in the same stage (unit gate travels with the build, §6.7);
   runtime stage: COPY binary to /usr/local/bin/entrypoint, COPY
   chrony.conf to /etc/chrony/chrony.conf, `RUN mkdir -p
   /var/lib/samba/chrony`, ENTRYPOINT/HEALTHCHECK
   (`--interval=30s --timeout=10s --start-period=120s --retries=3`),
   drop the interim `CMD ["samba","--version"]`.
4. ci.yml: add `unit` job (needs lint): `cd entrypoint && gofmt -l .
   [test -z], go vet ./..., go test ./...` on ubuntu-24.04; build job
   already exercises the in-image `go test`.
5. Adaptation profile: append a "Runtime contract" section containing
   the env var table, mode semantics, and exit-code table from this
   plan's Behavior contract VERBATIM, and a profile-changelog line.
6. Local verification (manual smoke; the full matrix is Phase 3):
   ```bash
   docker build -t samba-ad-dc:dev .
   printf 'Passw0rd!X' > /tmp/adminpass  # test-only value
   docker run -d --name dc1 --hostname dc1 \
     --cap-drop ALL --cap-add SYS_ADMIN --cap-add NET_BIND_SERVICE \
     --cap-add CHOWN --cap-add FOWNER --cap-add DAC_OVERRIDE \
     --cap-add SETUID --cap-add SETGID \
     --read-only --tmpfs /run --tmpfs /tmp \
     -v dc1-data:/var/lib/samba -v dc1-conf:/etc/samba \
     -v /tmp/adminpass:/secrets/adminpass:ro \
     -e SAMBA_MODE=provision -e SAMBA_REALM=AD.EXAMPLE.TEST \
     -e SAMBA_ADMIN_PASSWORD_FILE=/secrets/adminpass \
     samba-ad-dc:dev
   docker logs -f dc1   # provision output then samba foreground
   docker inspect --format '{{.State.Health.Status}}' dc1  # → healthy
   docker stop -t 12 dc1 && docker rm dc1
   ```
   Then restart-on-state (`SAMBA_MODE=run` with same volumes → starts),
   and `run` with fresh volumes → exit 21.
7. Commit `feat(entrypoint): wire state machine as image entrypoint with
   healthcheck` (+ trailers).

---

## Phase exit gate (roadmap Phase 2)

- [ ] `go test ./...` green with every Decide matrix cell exercised;
      `go vet`, `gofmt` clean; tests run inside the Docker build.
- [ ] Manual smoke: provision → healthy → clean SIGTERM stop → `run`
      restart on same volumes → `run` refusal (21) on empty volumes —
      all under read-only rootfs + cap set.
- [ ] Adaptation profile contract == implemented behavior.
- [ ] hadolint/yamllint/shellcheck clean. CI execution deferred w/ auth.
