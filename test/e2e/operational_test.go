package e2e

// The operational row of the B.5 matrix: what happens to a domain
// controller AFTER it exists — it is restarted, it is backed up and
// restored, it is upgraded, and it is stopped from being downgraded.
//
// Every test function name here is a stable traceability ID (see the
// package comment in main_test.go) and MUST NOT be renamed.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/esitc-paris/samba-ad-dc/test/e2e/harness"
)

// Container names. Fixed rather than generated, for the reason documented
// on harness.UniqueName: a DC's container name is also its host name, and a
// failure message that names `restart-dc1` is worth more than one that
// names `dc-4711-3`. They are unique within the package by construction.
const (
	restartFirst  = "restart-dc1"
	restartSecond = "restart-dc1-again"

	backupSource   = "backup-dc1"
	backupRestored = "backup-dc2"
	backupRunner   = "backup-offline"
	backupLister   = "backup-list"
	restoreRunner  = "backup-restore"

	downgradeDC     = "downgrade-dc1"
	downgradeEditor = "downgrade-marker"
	downgradeRun    = "downgrade-run"

	upgradeFirst  = "upgrade-dc1"
	upgradeSecond = "upgrade-dc1-candidate"
)

// stopTimeout is the SIGTERM grace an operator gives the container. It is
// above the entrypoint's own shutdown budget (§6.3) on purpose: the point
// of the assertion is that the container stops itself in time, not that
// docker eventually SIGKILLs it.
const stopTimeout = 12 * time.Second

// markerPath is the image-state marker inside the state volume. Its path
// and its JSON shape are part of the documented behaviour contract
// (adaptation profile, Runtime contract), which is why the tests below read
// it as data rather than grepping the logs for a version.
const markerPath = "/var/lib/samba/.image-state.json"

// exitDowngrade is the exit code the runtime contract reserves for a state
// volume written by a newer Samba than the image provides.
const exitDowngrade = 22

// futureVersion is the version written into a marker to simulate a volume
// that came from a newer image. It is deliberately absurd: no real tag can
// ever collide with it, so the downgrade guard is exercised without the test
// having to know anything about the release timeline.
const futureVersion = "99.0.0"

// backupDir is where the one-off backup container mounts the volume its
// tarball goes into.
const backupDir = "/backup"

// upgradeFromEnv names the image the upgrade test upgrades FROM. Empty
// means there is no published tag to upgrade from yet (§8.3).
const upgradeFromEnv = "E2E_UPGRADE_FROM"

// marker is the image-state marker as the tests read it back.
type marker struct {
	SambaVersion  string `json:"samba_version"`
	InitializedAt string `json:"initialized_at"`
	LastMode      string `json:"last_mode"`
}

// readMarker reads and parses the marker out of a running container.
//
// It is read through the container rather than off the host because the
// state volume is a docker volume: on any machine where the daemon is not
// the local kernel — Docker Desktop, a remote context, CI — there is no host
// path to read.
func readMarker(t *testing.T, container string) marker {
	t.Helper()
	out := harness.Exec(t, container, "cat", markerPath)
	var m marker
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("the marker %s in %s is not the documented JSON object (%v):\n%s",
			markerPath, container, err, out)
	}
	if m.SambaVersion == "" {
		t.Fatalf("the marker %s in %s records no samba_version:\n%s", markerPath, container, out)
	}
	return m
}

// initPhrases are the log lines the entrypoint prints when it initializes or
// re-validates a volume. A start that was supposed to touch nothing must
// print NONE of them: that is what "idempotent" means here, and asserting
// only that the container came back healthy would pass just as happily on a
// container that had quietly re-provisioned the domain.
var initPhrases = []string{
	"provisioning a new domain",
	"joining realm",
	"running samba-tool dbcheck",
	"volume adopted",
	"marker moved forward",
}

// mustNotContain fails when any of the phrases appears in the output.
func mustNotContain(t *testing.T, what, out string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(out, u) {
			t.Fatalf("%s: unexpected %q in output:\n%s", what, u, out)
		}
	}
}

// ---------------------------------------------------------------------
// B.5: restart
// ---------------------------------------------------------------------

// TestIdempotentRestart asserts the property an orchestrator depends on: a
// domain controller that is stopped and started again on the same volumes
// comes back as the SAME domain controller, and the second start changes
// nothing.
//
// Four things are asserted, and each one alone would be satisfied by a
// broken image:
//   - the container exits 0 when asked to stop (§6.3);
//   - the replacement container reaches the image's own health verdict;
//   - a user created before the stop is still there afterwards;
//   - the marker matches field-for-field over the documented marker fields,
//     and the second boot printed none of the lines an initialization or an
//     adoption prints.
func TestIdempotentRestart(t *testing.T) {
	net := harness.Network(t)

	dc := harness.StartDC(t, net, restartFirst, "provision", map[string]string{
		"SAMBA_REALM":  harness.Realm,
		"SAMBA_DOMAIN": harness.Domain,
	}, harness.AdminSecret(t))
	harness.WaitHealthy(t, dc.Name, harness.HealthTimeout)

	// --random-password keeps the account's password off every command line
	// the harness might echo into a failure message; nothing here needs it.
	harness.Exec(t, dc.Name, "samba-tool", "user", "create", "carol", "--random-password")
	before := readMarker(t, dc.Name)

	// --- the clean stop -------------------------------------------------
	//
	// The contract asserted here is the CONTAINER's: a domain controller
	// asked to stop has not failed, so `docker stop` must leave exit code 0.
	//
	// INVESTIGATION (Phase 2 hand-off, resolved here). Samba itself does NOT
	// exit 0 on SIGTERM — it exits 127, and the entrypoint's supervisor
	// records that in the container log as
	//
	//	entrypoint: samba stopped (samba --foreground ...: exit status 127)
	//
	// 127 here is neither the shell's "command not found" (no shell is
	// involved: the entrypoint execs samba directly through os/exec) nor
	// 128+SIGTERM=143 (samba is not killed BY the signal). It is a literal
	// constant in samba's own handler. Disassembling `sig_term` in
	// /usr/sbin/samba of the image under test (samba 4.24.6, source4/smbd/
	// server.c) shows exactly:
	//
	//	getpgrp(); getpid(); if equal -> kill(-getpgrp(), SIGTERM)
	//	mov w0, #0x7f            ; 127
	//	bl  _exit
	//
	// so the top-level `samba` process, on SIGTERM, optionally forwards the
	// signal to its process group and then hard-exits 127 by design. The
	// forwarding is guarded by "am I my own process-group leader", which is
	// precisely what the `--no-process-group` flag the entrypoint passes
	// prevents — without it samba would SIGTERM the whole container process
	// group, PID 1 included, behind the supervisor's back.
	//
	// The consequence for this suite: samba's 127 is an implementation
	// detail of a third party and is deliberately NOT asserted on. What is
	// asserted is the documented, container-level contract — the entrypoint
	// treats an orderly stop as success and the container exits 0 — plus the
	// supervisor's own ordered-shutdown trace, which is ours to keep.
	if code := harness.Stop(t, dc.Name, stopTimeout); code != 0 {
		t.Fatalf("`docker stop` left container %s with exit code %d, want 0: a DC asked to "+
			"stop has not failed (§6.3). Note that samba's own exit code on SIGTERM is 127 "+
			"by design and must never reach the container's:\n--- logs ---\n%s",
			dc.Name, code, harness.Logs(t, dc.Name))
	}
	//
	// Only the common tail of the shutdown announcement is asserted. Two
	// handlers see the same SIGTERM — the signal context main installs
	// before the long-running work, and the supervisor's own — and whichever
	// wins the supervisor's select decides whether the line reads "received
	// terminated: " or "shutdown requested: ". That race is harmless (both
	// lead to the same ordered stop) and pinning either wording would make
	// this test flaky for no gain.
	shutdown := harness.Logs(t, dc.Name)
	mustContain(t, "shutdown trace of "+dc.Name, shutdown,
		"stopping samba, then chronyd",
		"samba stopped",
		"chronyd stopped")

	// The ORDER is the contract, not merely the presence of both lines: the
	// directory has to leave the network before the time service it depends
	// on, and a supervisor that stopped chronyd first would print exactly the
	// same three lines. Read off the log because that is the only place the
	// sequence is observable from outside the container.
	if i, j := strings.Index(shutdown, "samba stopped"), strings.Index(shutdown, "chronyd stopped"); i > j {
		t.Fatalf("chronyd stopped before samba did (offsets %d and %d): the supervisor must "+
			"stop the domain controller first and its time service second (§6.3)\n%s",
			j, i, shutdown)
	}

	// --- the restart ----------------------------------------------------
	//
	// A NEW container on the SAME volumes, which is what an orchestrator
	// does — it does not `docker start` a corpse, it schedules a fresh one.
	// Its name (and therefore its host name) differs from the first
	// container's on purpose: the DC's identity lives in `netbios name` in
	// the smb.conf on the configuration volume, not in the container's host
	// name, and a restart that only worked when the two matched would be a
	// trap for every operator using generated container names.
	again := harness.StartDC(t, net, restartSecond, "run", nil,
		harness.WithVolumes(dc.StateVolume, dc.ConfVolume))
	harness.WaitHealthy(t, again.Name, harness.HealthTransitionTimeout)

	// The directory, not the container: carol survived the stop.
	harness.Exec(t, again.Name, "samba-tool", "user", "show", "carol")

	// Field-for-field over the documented marker fields (samba_version,
	// initialized_at, last_mode): a start on a volume this image already
	// owns must touch none of them.
	if after := readMarker(t, again.Name); after != before {
		t.Fatalf("the marker changed across a restart: was %+v, is now %+v; "+
			"a start on a volume this image already owns must touch nothing", before, after)
	}
	mustNotContain(t, "restart logs of "+again.Name, harness.Logs(t, again.Name), initPhrases...)
}

// ---------------------------------------------------------------------
// B.5: offline backup and restore
// ---------------------------------------------------------------------

// restoreHealthTimeout is the budget for the RESTORED DC to reach healthy.
// It is larger than harness.HealthTransitionTimeout because a restored
// volume costs two things an ordinary restart does not: the entrypoint runs
// a full dbcheck before adopting it, and samba has to re-register the
// realm's SRV records from scratch (see the note in the test body) before
// the health probe's first question can be answered at all.
const restoreHealthTimeout = 6 * time.Minute

// backupWorstCase is what this test can consume if every budget is spent:
// the source DC's provision, THREE one-off containers each bounded by
// harness.ExitTimeout (the backup, the listing that proves its tarball
// exists, and the restore), the restored DC's health transition, and the
// one test-client run that measures time against it. It is well past `go
// test`'s 10-minute default, which is why the test refuses to start under
// one — see requireDeadline in replication_test.go.
const backupWorstCase = harness.HealthTimeout + 3*harness.ExitTimeout + restoreHealthTimeout +
	harness.ClientTimeout

// TestOfflineBackupRestore asserts the disaster-recovery procedure end to
// end: an offline backup taken from the stopped volumes of one DC is
// restored into fresh volumes, and a container started on those volumes
// serves the SAME domain, with the objects that were in it.
//
// Everything is done the way the operator documentation has to describe it —
// one-off containers of the image under test running `samba-tool` directly,
// never a shell inside a live DC.
//
// Four findings from getting this to work are recorded in the body rather
// than in a report, because they are the difference between a procedure that
// works and one that looks like it should:
//   - the restore cannot reuse the backed-up DC's own name;
//   - the restore leaves the realm's SRV records unregistered;
//   - the restored tree relocates state, cache and sysvol under
//     /var/lib/samba/state — but not the MS-SNTP signing socket, which the
//     restore leaves at samba's default;
//   - the restored volume carries no marker at the path the entrypoint
//     reads, so the container adopts it — which is the documented behaviour
//     for a foreign volume, and is asserted here rather than worked around.
func TestOfflineBackupRestore(t *testing.T) {
	requireDeadline(t, backupWorstCase)
	net := harness.Network(t)

	// The forwarder is set explicitly rather than left to samba's provision
	// (which copies the first nameserver out of /etc/resolv.conf, i.e.
	// docker's embedded resolver, by accident). It ends up in the smb.conf
	// that the backup carries, and the restored DC depends on it — see the
	// resolv.conf note further down.
	src := harness.StartDC(t, net, backupSource, "provision", map[string]string{
		"SAMBA_REALM":         harness.Realm,
		"SAMBA_DOMAIN":        harness.Domain,
		"SAMBA_DNS_FORWARDER": dockerEmbeddedResolver,
	}, harness.AdminSecret(t))
	harness.WaitHealthy(t, src.Name, harness.HealthTimeout)

	harness.Exec(t, src.Name, "samba-tool", "user", "create", "dave", "--random-password")
	imageVersion := readMarker(t, src.Name).SambaVersion

	// `domain backup offline` locks the databases itself and is documented
	// to work on a running DC, but the procedure this image ships is the
	// quiescent one: stop first, exactly as for maintenance mode.
	if code := harness.Stop(t, src.Name, stopTimeout); code != 0 {
		t.Fatalf("stopping %s before the backup: exit code = %d, want 0", src.Name, code)
	}

	// --- the backup -----------------------------------------------------
	//
	// The state and configuration volumes are mounted READ-WRITE, and cannot
	// be otherwise: `samba-tool domain backup offline` takes exclusive locks
	// on the databases it copies — which is precisely why a read-only
	// snapshot of a live volume is not a valid backup of an AD database and
	// why the profile's B.6 position points operators at this command rather
	// than at `cp` or a filesystem snapshot.
	backupVol := harness.Volume(t)
	code, logs := harness.RunDCExpectExit(t, net, backupRunner, "", nil,
		harness.WithVolumes(src.StateVolume, src.ConfVolume),
		harness.WithRunArgs("-v", backupVol+":"+backupDir),
		harness.WithEntrypoint("samba-tool",
			"domain", "backup", "offline", "--targetdir="+backupDir))
	if code != 0 {
		t.Fatalf("samba-tool domain backup offline: exit code = %d, want 0\n%s", code, logs)
	}
	mustContain(t, "offline backup", logs, "Backup succeeded.")

	// The tarball is asserted to EXIST, separately and immediately, rather
	// than being discovered missing by the restore's shell glob three steps
	// later. "Backup succeeded." is samba's opinion; this is the artefact.
	// No WithVolumes: this container has no business seeing the domain's
	// state, so it gets throwaway volumes of its own and only the backup
	// volume — read-only, which IS possible here because reading a tarball
	// takes no locks.
	code, logs = harness.RunDCExpectExit(t, net, backupLister, "", nil,
		harness.WithRunArgs("-v", backupVol+":"+backupDir+":ro"),
		harness.WithEntrypoint("ls", "-l", backupDir))
	if code != 0 {
		t.Fatalf("listing the backup volume after a backup that reported success: "+
			"exit code = %d, want 0\n%s", code, logs)
	}
	mustContain(t, "the backup volume", logs, "samba-backup-")

	// --- the restore ----------------------------------------------------
	//
	// Into a FRESH pair of volumes: a restore that overwrote the volumes it
	// was taken from would prove nothing about recovering a lost DC.
	//
	// FINDING 1 — the new server name cannot be the old one. `samba-tool
	// domain backup restore` requires --newservername, and it ADDS that DC's
	// account to the restored database BEFORE removing the DCs the backup
	// came from. Passing the backed-up DC's own name therefore fails with
	// "Entry CN=<NAME>,OU=Domain Controllers,... already exists" (observed).
	// A restore is consequently always a restore ONTO A NEW DC NAME; the
	// domain, its SID and its objects are what survive, which is what the
	// object-level assertion below checks.
	//
	// FINDING 2 — the target directory must be empty, and a fresh volume is
	// not. The samba package ships empty /var/lib/samba/private and
	// /var/lib/samba/bind-dns directories, and docker copies whatever the
	// image has at a volume's mount point into a new volume, so the restore
	// refuses with "Target directory is not empty". `rmdir` on exactly those
	// two paths is the fix and is also a safety net: rmdir fails on a
	// non-empty directory, so this command can never eat a volume that
	// actually holds a domain.
	//
	// The restored tree is self-contained under the target directory,
	// including its own etc/smb.conf with all paths rewritten to match — so
	// the last step is to put that file where the image reads it from.
	stateVol, confVol := harness.Volume(t), harness.Volume(t)
	restore := strings.Join([]string{
		"set -e",
		"rmdir /var/lib/samba/private /var/lib/samba/bind-dns",
		"samba-tool domain backup restore" +
			" --backup-file=\"$(ls " + backupDir + "/samba-backup-*.tar.bz2)\"" +
			" --targetdir=/var/lib/samba" +
			" --newservername=" + backupRestored,
		"cp /var/lib/samba/etc/smb.conf /etc/samba/smb.conf",
	}, "\n")

	//
	// FINDING 4 — on branch 4.22 this one container needs a capability the
	// running DC does not. Samba 4.22.11's restore reaches the sysvol
	// NT-ACL step through smbd and fails there with `py_smbd_mkdir:
	// mkdirat error=13 (Permission denied)`, exiting 255, under the very
	// capability set every other container in this suite runs green with;
	// adding DAC_OVERRIDE makes it pass, and 4.23.12 and 4.24.7 need
	// nothing added at all (measured locally, arm64, 2026-09-16). The
	// capability set a *running DC* is tested under is therefore left
	// alone on every branch — nothing showed it has to change — and the
	// widening is scoped to this short-lived restore container, through
	// E2E_RESTORE_CAPS, by the caller that knows which branch is under
	// test (release.yml sets it for 4.22). Unset, which is the default and
	// what 4.23/4.24 run with, changes nothing here.
	restoreOpts := []harness.Opt{
		harness.WithVolumes(stateVol, confVol),
		harness.WithRunArgs("-v", backupVol+":"+backupDir+":ro"),
		harness.WithEntrypoint("sh", "-c", restore),
	}
	if caps, ok := harness.RestoreCaps(); ok {
		t.Logf("restore container capability override in effect (E2E_RESTORE_CAPS): %v", caps)
		restoreOpts = append(restoreOpts, harness.WithCaps(caps...))
	}

	code, logs = harness.RunDCExpectExit(t, net, restoreRunner, "", nil, restoreOpts...)
	if code != 0 {
		t.Fatalf("samba-tool domain backup restore: exit code = %d, want 0\n%s", code, logs)
	}
	mustContain(t, "restore", logs, "Backup file successfully restored to /var/lib/samba")

	// --- the restored domain controller ---------------------------------
	//
	// FINDING 3 — the restore strips the realm's service records. It removes
	// every Server object other than the new one, and with them the SRV and
	// CNAME records that pointed at the DC the backup came from; nothing
	// adds records for the new name, because that is samba_dnsupdate's job
	// at first start. Until it runs, `_ldap._tcp.<realm>` has no answer —
	// which is the first thing this image's health probe asks for, so the
	// container stays `starting` forever.
	//
	// samba_dnsupdate can only do that job if the DC resolves through
	// ITSELF: left on docker's embedded resolver it asks 127.0.0.11 for a
	// name only samba is authoritative for, that resolver forwards to the
	// DC's `dns forwarder` (which the provision set to 127.0.0.11), and the
	// query loops until it times out (observed: 17 s per name, then the
	// update aborts). Bind-mounting a resolv.conf pointing at 127.0.0.1 —
	// the same trick, for the same reason, as in TestJoinReplicationBothWays
	// — sends those lookups to samba directly and lets the registration
	// complete. This is a procedural requirement of the restore, not a test
	// convenience, and belongs in the operator documentation.
	restored := harness.StartDC(t, net, backupRestored, "run", nil,
		harness.WithVolumes(stateVol, confVol),
		harness.WithBind(selfResolvConf(t), "/etc/resolv.conf", true))
	harness.WaitHealthy(t, restored.Name, restoreHealthTimeout)

	// Object-level verification (B.5): the domain that came back is the one
	// that was backed up, not an empty one wearing its name.
	harness.Exec(t, restored.Name, "samba-tool", "user", "show", "dave")

	// FINDING 4 — the restored tree is not laid out like a provisioned one,
	// but the relocation stops short of the MS-SNTP signing socket. Measured
	// (B.6): the restored smb.conf moves `state directory` and the sysvol
	// share one level down, to /var/lib/samba/state and
	// /var/lib/samba/state/sysvol, and leaves the other two flat —
	// `cache directory = /var/lib/samba/cache`, a sibling of state and not a
	// child of it, and `lock directory = /var/lib/samba`. That is why
	// operator documentation must derive those paths from smb.conf rather
	// than hardcode them: no single rule maps a provisioned path to its
	// restored one. It does NOT set `ntp signd
	// socket directory`, and samba's compile-time default for that parameter
	// does not track `state directory` — so a restored DC keeps the signing
	// socket exactly where a provisioned one does. This was measured, after
	// the opposite was suspected; it is recorded here because "the restore
	// moves everything under state/" is the plausible wrong conclusion, and
	// the next reader will draw it from the sysvol path two lines up.
	//
	// The wiring is still asserted on this DC rather than taken on faith:
	// the restore is the one path that rewrites smb.conf wholesale, so it is
	// the one most likely to move this parameter in a future samba release —
	// and this assertion reads the DC's own value, so it will keep holding
	// if that happens.
	assertSignedNTPWiring(t, restored.Name)

	// ...and the time service really answers on the restored DC, not merely
	// that its configuration looks right.
	assertServesTime(t, net, restored.IP)

	// The adoption path, asserted rather than tolerated. `domain backup
	// offline` archives the marker as part of the state directory, so the
	// restore puts it back at /var/lib/samba/state/.image-state.json — NOT
	// at the path the entrypoint reads. The container therefore sees samba
	// state with no marker of its own, which is exactly the "foreign volume"
	// case: check the database, then claim it. That is the documented
	// behaviour for a restored volume and operators must be told to expect
	// the dbcheck on the first boot after a restore.
	mustContain(t, "restored DC logs", harness.Logs(t, restored.Name),
		"the volume holds samba state but no .image-state.json marker: "+
			"checking the database before adopting it",
		"(0 errors)",
		"volume adopted: marker written for samba "+imageVersion)

	if got := readMarker(t, restored.Name); got.SambaVersion != imageVersion {
		t.Fatalf("after adoption the marker records samba_version %q, want the image's %q",
			got.SambaVersion, imageVersion)
	}
}

// ---------------------------------------------------------------------
// B.5 / §8.3: upgrade from the last published image
// ---------------------------------------------------------------------

// upgradeWorstCase is this test's budget when it does not skip: fetching the
// published image it upgrades FROM, a provision on that image, and a health
// transition that includes a full dbcheck.
//
// The pull is counted because it is real time this test can spend before it
// has started anything: E2E_UPGRADE_FROM names a registry reference, and on
// a cold CI runner harness.EnsureImage will actually go and get it.
const upgradeWorstCase = harness.PullTimeout + harness.HealthTimeout + harness.HealthTransitionTimeout

// TestUpgradeFromLastPublished asserts that an existing domain survives the
// image being replaced: the volume a published image initialized is started
// by the candidate image, which checks the database first and then moves the
// marker forward.
//
// It skips while no published tag exists (§8.3, first release of a branch).
// The skip is loud and names the variable, because a test that quietly does
// nothing is indistinguishable from one that quietly passes.
func TestUpgradeFromLastPublished(t *testing.T) {
	from := strings.TrimSpace(os.Getenv(upgradeFromEnv))
	if from == "" {
		t.Skip("first release of branch: no published tag yet (SPEC §8.3); " +
			"set " + upgradeFromEnv + " to the last published image ref to run this test")
	}
	requireDeadline(t, upgradeWorstCase)

	harness.EnsureImage(t, from)
	net := harness.Network(t)

	old := harness.StartDC(t, net, upgradeFirst, "provision", map[string]string{
		"SAMBA_REALM":  harness.Realm,
		"SAMBA_DOMAIN": harness.Domain,
	}, harness.AdminSecret(t), harness.WithImage(from))
	harness.WaitHealthy(t, old.Name, harness.HealthTimeout)

	harness.Exec(t, old.Name, "samba-tool", "user", "create", "erin", "--random-password")
	before := readMarker(t, old.Name)

	if code := harness.Stop(t, old.Name, stopTimeout); code != 0 {
		t.Fatalf("stopping the old-image DC %s: exit code = %d, want 0", old.Name, code)
	}

	// The candidate image — harness.Image(), i.e. no WithImage — on the very
	// volumes the published one wrote.
	upgraded := harness.StartDC(t, net, upgradeSecond, "run", nil,
		harness.WithVolumes(old.StateVolume, old.ConfVolume))
	harness.WaitHealthy(t, upgraded.Name, harness.HealthTransitionTimeout)

	// The domain survived, whichever path the entrypoint took.
	harness.Exec(t, upgraded.Name, "samba-tool", "user", "show", "erin")

	after := readMarker(t, upgraded.Name)
	if after.SambaVersion == before.SambaVersion {
		// Not a failure: an image rebuild that ships the same Samba is a
		// legitimate release, and the entrypoint is documented to start such
		// a volume as is. There is simply no upgrade to observe, and
		// asserting the dbcheck path here would report a defect where the
		// behaviour is correct.
		t.Skipf("%s=%s ships the same Samba (%s) as the image under test, so this start is "+
			"a plain restart and not an upgrade; point %s at a tag with an older Samba to "+
			"exercise the upgrade path",
			upgradeFromEnv, from, before.SambaVersion, upgradeFromEnv)
	}

	mustContain(t, "upgrade logs", harness.Logs(t, upgraded.Name),
		"the volume was written by an older samba: checking the database before starting",
		"(0 errors)",
		"marker moved forward to samba "+after.SambaVersion)

	// initialized_at answers "when was this domain created", so an upgrade
	// must carry it over rather than restamp it — that is the one field
	// which distinguishes a marker moved forward from a marker rewritten.
	if after.InitializedAt != before.InitializedAt {
		t.Fatalf("the upgrade restamped initialized_at from %q to %q; it records when the "+
			"domain was created, not when it was last checked",
			before.InitializedAt, after.InitializedAt)
	}
}

// ---------------------------------------------------------------------
// B.5: the downgrade guard
// ---------------------------------------------------------------------

// downgradeWorstCase is this test's budget if every one of them is spent: a
// provision, then TWO one-off containers each bounded by harness.ExitTimeout
// — the marker edit, and the refusal itself. That is past `go test`'s
// 10-minute default, so this test refuses to start under one rather than
// being killed mid-provision by a panic that skips every cleanup.
const downgradeWorstCase = harness.HealthTimeout + 2*harness.ExitTimeout

// TestDowngradeRefused asserts the guard that protects a directory database
// from being opened by an older Samba than the one that wrote it: the
// container refuses to start, with the documented exit code and a message
// that names BOTH versions and the way out.
//
// The volume is a real, provisioned domain — only its marker is edited — so
// what is exercised is the guard in front of a working DC, not a synthetic
// file the entrypoint would have rejected for some other reason.
func TestDowngradeRefused(t *testing.T) {
	requireDeadline(t, downgradeWorstCase)
	net := harness.Network(t)

	dc := harness.StartDC(t, net, downgradeDC, "provision", map[string]string{
		"SAMBA_REALM":  harness.Realm,
		"SAMBA_DOMAIN": harness.Domain,
	}, harness.AdminSecret(t))
	harness.WaitHealthy(t, dc.Name, harness.HealthTimeout)

	// The image's own Samba version, read from the marker it wrote: the
	// message asserted below quotes it, and hard-coding it here would make
	// this test go red on every Samba bump for no reason.
	imageVersion := readMarker(t, dc.Name).SambaVersion
	if code := harness.Stop(t, dc.Name, stopTimeout); code != 0 {
		t.Fatalf("stopping %s before editing its marker: exit code = %d, want 0", dc.Name, code)
	}

	// The marker is edited with the image's own python3, in a one-off
	// container on the state volume — the same way an operator would have to
	// reach into a docker volume, and without inventing a second definition
	// of the marker's JSON shape.
	edit := `import json, pathlib
p = pathlib.Path("` + markerPath + `")
m = json.loads(p.read_text())
m["samba_version"] = "` + futureVersion + `"
p.write_text(json.dumps(m, indent=2) + "\n")
print(p.read_text())
`
	code, logs := harness.RunDCExpectExit(t, net, downgradeEditor, "", nil,
		harness.WithVolumes(dc.StateVolume, dc.ConfVolume),
		harness.WithEntrypoint("python3", "-c", edit))
	if code != 0 {
		t.Fatalf("rewriting the marker to %s: exit code = %d, want 0\n%s", futureVersion, code, logs)
	}
	mustContain(t, "the rewritten marker", logs, `"samba_version": "`+futureVersion+`"`)

	// A start against a volume that now claims to come from the future.
	code, logs = harness.RunDCExpectExit(t, net, downgradeRun, "run", nil,
		harness.WithVolumes(dc.StateVolume, dc.ConfVolume))
	if code != exitDowngrade {
		t.Fatalf("starting on a volume marked Samba %s: exit code = %d, want %d "+
			"(the runtime contract's downgrade refusal)\n%s",
			futureVersion, code, exitDowngrade, logs)
	}

	// The exit code alone is not the contract: §6.5 requires the message to
	// say what was wrong AND what to do about it. Both versions have to
	// appear, or an operator cannot tell which image to deploy.
	mustContain(t, "downgrade refusal", logs,
		"ERROR: the state volume was written by Samba "+futureVersion+
			" but this image provides Samba "+imageVersion,
		"deploy an image tag providing Samba "+futureVersion+" or newer",
		"restore a backup of the volume taken on Samba "+imageVersion)

	// And it must have refused BEFORE touching anything: a guard that runs a
	// dbcheck with the older Samba first has already done the damage it
	// exists to prevent.
	mustNotContain(t, "downgrade refusal", logs, initPhrases...)
}
