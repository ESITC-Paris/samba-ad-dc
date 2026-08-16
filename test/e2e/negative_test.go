package e2e

// The negative row of the B.5 matrix: every case in which the image is
// documented to REFUSE rather than to act — a secret that is not mounted,
// a secret handed over the wrong way, an initialization aimed at a volume
// that already holds a domain, and a start aimed at a volume that holds
// none.
//
// Two things are asserted for each, and neither one alone is the contract
// (SPEC §6.5, adaptation profile "Runtime contract"):
//
//   - the documented EXIT CODE, read from the container itself, because
//     that is what an orchestrator and a CI job branch on; and
//   - the documented MESSAGE, read from the container's logs, because
//     §6.5 requires one `ERROR: ` line naming the cause AND the remedy —
//     an exit code with no way out is a refusal an operator cannot fix.
//
// The message assertions quote FRAGMENTS of the documented wording — the
// variable, the path, the remedy verb — rather than whole lines: the
// contract is that the operator is told which knob to turn, not that the
// sentence is punctuated a particular way.
//
// A refusal is also a promise about TIME. Every case here is decided
// before any daemon starts and before samba-tool is ever executed, so each
// container is held to a wall-clock budget (failFastBudget); a "refusal"
// that arrives after minutes of work has already spent the operator's
// deployment window, and in the provision-over-state case it would mean
// the guard ran after the damage.
//
// Every test function name here is a stable traceability ID (see the
// package comment in main_test.go) and MUST NOT be renamed.

import (
	"testing"
	"time"

	"github.com/esitc-paris/samba-ad-dc/test/e2e/harness"
)

// The exit codes of the runtime contract that this file covers. They are
// spelled out here, as literals with names, rather than imported from the
// entrypoint: the E2E suite is a black-box test of the published contract,
// and sharing a constant with the code under test would let a renumbering
// pass unnoticed on both sides at once. (adaptation profile, "Exit codes";
// immutable once released.)
const (
	exitConfigError = 10 // configuration error
	exitSecretError = 11 // missing/unreadable secret file
	exitStateExists = 20 // provision/join refused over existing state
	exitStateAbsent = 21 // run mode with absent state
)

// Container names. Fixed rather than generated, for the reason documented
// on harness.UniqueName: a failure that names `refuse-nofile` is worth more
// than one that names `dc-4711-3`. They are unique within the package by
// construction.
//
// A NAME THAT PROVISIONS MUST BE AT MOST 15 CHARACTERS, and must not have a
// hyphen in the 15th position. A container's name is its host name, and
// `samba-tool domain provision` derives the NetBIOS name from the host name
// by truncating it to the 15-byte NetBIOS limit — so `negative-state-dc1`
// (the first name this test carried) provisioned a DC calling itself
// `negative-state-`, whose SRV and A records are `negative-state-.ad.e2e.
// test`. A DNS label may not end in a hyphen, so every resolver that
// validates names — including the one inside this image's own health probe,
// which then reported "DNS response contained records which contain invalid
// names" — refuses the answer, and the container never becomes healthy. The
// symptom is a DC that provisions perfectly and then fails its health check
// forever, which reads exactly like an image defect. Only `refuseDC` below
// actually provisions; the rest exit before samba ever starts and are
// therefore unconstrained, but they are kept short for consistency.
const (
	refuseNoFile  = "refuse-nofile"
	refuseBadFile = "refuse-badfile"

	refuseEnvAdmin = "refuse-env-admin"
	refuseEnvJoin  = "refuse-env-join"

	refuseDC        = "refuse-dc1"
	refuseProvision = "refuse-provision"
	refuseJoin      = "refuse-join"
	refuseDCRestart = "refuse-dc1-again"

	refuseRun   = "refuse-run"
	refuseMaint = "refuse-maint"
)

// failFastBudget is the wall clock every refusal in this file has to fit
// into, measured around the whole `docker run` + `docker wait` cycle (so
// it charges the refusal for docker's own overhead as well, which is the
// conservative direction).
//
// It is deliberately far above what these paths cost — they are a getenv
// sweep, one stat of the state volume and a printf, i.e. well under a
// second of actual work — because the assertion is not a benchmark. What
// it must catch is a refusal that stopped being fail-fast: one that starts
// a daemon, runs a dbcheck, or waits on a network timeout before deciding.
// Any of those overshoot 30 s by a wide margin, and none of them can hide
// under it.
const failFastBudget = 30 * time.Second

// missingSecretPath is where the admin password WOULD have been mounted.
// It is the exact path harness.WithSecret uses, so the second subtest of
// TestMissingSecretFailsFast reproduces the real operator mistake — the
// variable is set, the `-v` was forgotten — rather than an invented path.
const missingSecretPath = "/run/secrets/admin-password"

// notASecret is the value the plain-environment test puts into the
// variables the image must refuse.
//
// It is not a password and never protects anything: the container it is
// handed to is documented to exit before it reads it. The literal is
// distinctive on purpose so that TestPlainEnvSecretRejected can also
// assert the second half of §6.1 — that a value handed over the wrong way
// is refused WITHOUT being echoed into the logs, where a real operator's
// real password would then sit in every CI artifact.
const notASecret = "plainly-not-a-secret-31415"

// refuseWithin runs a container that is expected to refuse, and holds it to
// the fail-fast budget. It returns the exit code and logs exactly like
// harness.RunDCExpectExit does.
//
// The budget is reported with t.Errorf rather than t.Fatalf: a refusal that
// is correct but slow and a refusal that is fast but wrong are different
// defects, and stopping at the first would hide the second.
func refuseWithin(t *testing.T, net, name, mode string, env map[string]string, opts ...harness.Opt) (int, string) {
	t.Helper()

	start := time.Now()
	code, logs := harness.RunDCExpectExit(t, net, name, mode, env, opts...)
	if took := time.Since(start); took > failFastBudget {
		t.Errorf("container %s took %s to refuse, over the %s fail-fast budget; "+
			"this refusal is decided before any daemon starts and must not wait on one\n"+
			"--- logs ---\n%s",
			name, took.Round(time.Second), failFastBudget, logs)
	}
	return code, logs
}

// mustExit fails when the container did not exit with the documented code,
// and says which contract line it violated.
func mustExit(t *testing.T, name string, got, want int, contract, logs string) {
	t.Helper()
	if got != want {
		t.Fatalf("container %s exited %d, want %d (%s)\n--- logs ---\n%s",
			name, got, want, contract, logs)
	}
}

// ---------------------------------------------------------------------
// B.5: the secret has to be a mounted file, and it has to be there
// ---------------------------------------------------------------------

// TestMissingSecretFailsFast asserts the two ways an operator can fail to
// deliver the Administrator password, and that the image tells them apart:
// forgetting the VARIABLE is a configuration error (10), while pointing the
// variable at a file that is not there is a secret error (11).
//
// The distinction is the whole point of having two codes. Both messages
// have to name the thing the operator got wrong — the variable in the first
// case, the path in the second — and both have to arrive within seconds,
// because a container that hangs on a missing secret looks to an
// orchestrator exactly like a container that is still provisioning.
func TestMissingSecretFailsFast(t *testing.T) {
	net := harness.Network(t)

	// The realm is supplied in both subtests so the refusal cannot be
	// blamed on anything else: the ONLY thing wrong with these containers
	// is the secret.
	env := map[string]string{
		"SAMBA_REALM":  harness.Realm,
		"SAMBA_DOMAIN": harness.Domain,
	}

	t.Run("variable_unset", func(t *testing.T) {
		// No harness.AdminSecret: nothing points at a password at all.
		code, logs := refuseWithin(t, net, refuseNoFile, "provision", env)
		mustExit(t, refuseNoFile, code, exitConfigError,
			"provision mode without SAMBA_ADMIN_PASSWORD_FILE is a configuration error", logs)

		mustContain(t, "missing-variable refusal", logs,
			"ERROR: SAMBA_ADMIN_PASSWORD_FILE is required in provision mode but is not set",
			// The remedy: not just "it is missing" but what to do (§6.5).
			"mount the initial Administrator password as a file",
			"point SAMBA_ADMIN_PASSWORD_FILE at it")

		// Nothing may have been initialized on the way to refusing.
		mustNotContain(t, "missing-variable refusal", logs, initPhrases...)
	})

	t.Run("file_absent", func(t *testing.T) {
		// The variable is set the way the documentation says — and the
		// bind mount it names was forgotten. This is the failure mode the
		// separate exit code 11 exists for: the configuration is right,
		// the delivery is not.
		absent := map[string]string{
			"SAMBA_REALM":               harness.Realm,
			"SAMBA_DOMAIN":              harness.Domain,
			"SAMBA_ADMIN_PASSWORD_FILE": missingSecretPath,
		}
		code, logs := refuseWithin(t, net, refuseBadFile, "provision", absent)
		mustExit(t, refuseBadFile, code, exitSecretError,
			"a secret file that cannot be read is exit 11, not 10", logs)

		mustContain(t, "unreadable-secret refusal", logs,
			// The path, quoted, so the operator can compare it with their
			// `-v` argument character by character.
			`ERROR: secret file "`+missingSecretPath+`" cannot be read`,
			"no such file or directory",
			"mount the file into the container",
			"readable by the container user")

		mustNotContain(t, "unreadable-secret refusal", logs, initPhrases...)
	})
}

// ---------------------------------------------------------------------
// B.5 / §6.1: passwords are never accepted from the environment
// ---------------------------------------------------------------------

// TestPlainEnvSecretRejected asserts the rule that makes the `*_FILE`
// convention worth having: an image that merely PREFERRED files would still
// leak every password into `docker inspect`, the daemon's log and every
// orchestrator's state store, because the plain variable would keep on
// working. So the plain variable must be refused outright, and the refusal
// must point at the file variant that replaces it.
//
// Both halves of the pair are covered, and both are set in PROVISION mode
// on purpose — including SAMBA_JOIN_PASSWORD, which provision mode does not
// otherwise care about. The check is documented to run before anything else
// (§6.1, "refuse before anything else"), and a container that only rejected
// the variable its own mode happens to read would leave the other one live
// in the environment of a running DC.
func TestPlainEnvSecretRejected(t *testing.T) {
	net := harness.Network(t)

	cases := []struct {
		name      string
		container string
		plainVar  string
		fileVar   string
	}{
		{"admin", refuseEnvAdmin, "SAMBA_ADMIN_PASSWORD", "SAMBA_ADMIN_PASSWORD_FILE"},
		{"join", refuseEnvJoin, "SAMBA_JOIN_PASSWORD", "SAMBA_JOIN_PASSWORD_FILE"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{
				"SAMBA_REALM":  harness.Realm,
				"SAMBA_DOMAIN": harness.Domain,
				c.plainVar:     notASecret,
			}
			// The CORRECT secret is mounted as well. Without it, a
			// provision would be refused anyway for want of
			// SAMBA_ADMIN_PASSWORD_FILE — with the same exit code — and
			// this test would pass on an image that ignored the plain
			// variable completely. With it, the container is valid in
			// every respect except the one under test.
			code, logs := refuseWithin(t, net, c.container, "provision", env, harness.AdminSecret(t))
			mustExit(t, c.container, code, exitConfigError,
				"a password in the environment is a configuration error the image refuses (§6.1)", logs)

			mustContain(t, c.plainVar+" refusal", logs,
				"ERROR: "+c.plainVar+" is set in the environment",
				"passwords are never accepted that way",
				// The remedy names BOTH sides of the fix: what to unset,
				// and what to use instead.
				"unset "+c.plainVar,
				"pass the password via "+c.fileVar,
				"mounted secret file")

			// The second half of §6.1: refusing is not enough if the
			// value is echoed on the way out. A password handed over the
			// wrong way must not end up in the container log, which is
			// the one place every CI system archives by default.
			mustNotContain(t, c.plainVar+" refusal", logs, notASecret)

			mustNotContain(t, c.plainVar+" refusal", logs, initPhrases...)
		})
	}
}

// ---------------------------------------------------------------------
// B.5: an initialization mode never overwrites an existing domain
// ---------------------------------------------------------------------

// stateGuardWorstCase is what TestProvisionOverStateRefused can consume if
// every budget is spent: one provision, two refusals, and the restart that
// proves the domain is still there.
const stateGuardWorstCase = harness.HealthTimeout + 2*harness.ExitTimeout + harness.HealthTransitionTimeout

// TestProvisionOverStateRefused asserts the guard that stands between a
// mistyped `SAMBA_MODE` and a destroyed directory: a volume that already
// holds a domain is never re-initialized, in provision mode or in join
// mode, and the refusal exits 20 with a message that offers both ways
// forward (start what is there, or delete it deliberately).
//
// The second half is what makes this a §8.2 test rather than an exit-code
// check. An image could return 20 from a guard placed AFTER `samba-tool
// domain provision` had already begun writing, and every assertion about
// the code and the message would still pass over the wreckage. So the same
// volumes are then started in run mode and interrogated: the user created
// before the refusal is still in the directory, and the marker is
// byte-for-byte the one the provision wrote. That — not the exit code — is
// the property an operator is actually relying on.
func TestProvisionOverStateRefused(t *testing.T) {
	requireDeadline(t, stateGuardWorstCase)
	net := harness.Network(t)

	dc := harness.StartDC(t, net, refuseDC, "provision", map[string]string{
		"SAMBA_REALM":  harness.Realm,
		"SAMBA_DOMAIN": harness.Domain,
	}, harness.AdminSecret(t))
	harness.WaitHealthy(t, dc.Name, harness.HealthTimeout)

	// The witness. --random-password keeps it off every command line the
	// harness might echo into a failure message; nothing here needs to know
	// it, only that the account survives.
	harness.Exec(t, dc.Name, "samba-tool", "user", "create", "grace", "--random-password")
	before := readMarker(t, dc.Name)

	// The DC is stopped before its volumes are handed to another container:
	// two sambas writing one sam.ldb would corrupt the very state this test
	// is about to prove intact, and the failure would look like the image's.
	if code := harness.Stop(t, dc.Name, stopTimeout); code != 0 {
		t.Fatalf("stopping %s before re-running an initialization mode: exit code = %d, want 0",
			dc.Name, code)
	}

	// --- provision, aimed at the existing domain -------------------------
	code, logs := refuseWithin(t, net, refuseProvision, "provision", map[string]string{
		"SAMBA_REALM":  harness.Realm,
		"SAMBA_DOMAIN": harness.Domain,
	}, harness.WithVolumes(dc.StateVolume, dc.ConfVolume), harness.AdminSecret(t))
	mustExit(t, refuseProvision, code, exitStateExists,
		"provision over an initialized volume is refused with 20", logs)

	mustContain(t, "provision-over-state refusal", logs,
		"ERROR: SAMBA_MODE=provision would initialize a new domain "+
			"but the volume already holds samba state",
		// The evidence, named as a path the operator can check themselves.
		"/var/lib/samba/private/sam.ldb exists",
		// Two remedies, because the operator's intent decides which:
		// keep the domain, or discard it on purpose.
		"set SAMBA_MODE=run to keep and start the existing domain",
		"delete the state volume first")

	// Refused BEFORE acting, not after: none of the lines an
	// initialization prints may appear.
	mustNotContain(t, "provision-over-state refusal", logs, initPhrases...)

	// --- join, aimed at the same existing domain -------------------------
	//
	// The contract covers join and provision with one code and one message
	// ("provision/join refused over existing state"), and the volumes are
	// already in the right condition, so the second half of that row costs
	// one container instead of a second provision. The join secret has to
	// be mounted: without it the configuration check would refuse with 10
	// before the state guard was ever reached, and this would silently stop
	// testing the state guard.
	code, logs = refuseWithin(t, net, refuseJoin, "join", map[string]string{
		"SAMBA_REALM": harness.Realm,
	}, harness.WithVolumes(dc.StateVolume, dc.ConfVolume), harness.JoinSecret(t))
	mustExit(t, refuseJoin, code, exitStateExists,
		"join over an initialized volume is refused with 20, like provision", logs)

	mustContain(t, "join-over-state refusal", logs,
		"ERROR: SAMBA_MODE=join would initialize a new domain "+
			"but the volume already holds samba state",
		"set SAMBA_MODE=run to keep and start the existing domain")
	mustNotContain(t, "join-over-state refusal", logs, initPhrases...)

	// --- the state the refusals protected --------------------------------
	//
	// A new container on the SAME volumes, in the mode the refusal message
	// told the operator to use. If either refusal had written anything, this
	// is where it shows: as a missing user, a moved marker, or a DC that
	// never comes up at all.
	again := harness.StartDC(t, net, refuseDCRestart, "run", nil,
		harness.WithVolumes(dc.StateVolume, dc.ConfVolume))
	harness.WaitHealthy(t, again.Name, harness.HealthTransitionTimeout)

	harness.Exec(t, again.Name, "samba-tool", "user", "show", "grace")

	if after := readMarker(t, again.Name); after != before {
		t.Fatalf("the marker changed across the refused initializations: was %+v, is now %+v; "+
			"a refusal must leave the volume exactly as it found it", before, after)
	}
	mustNotContain(t, "logs of "+again.Name, harness.Logs(t, again.Name), initPhrases...)
}

// ---------------------------------------------------------------------
// B.5: a start mode never invents the state it is missing
// ---------------------------------------------------------------------

// TestRunModeWithoutStateRefused asserts the mirror image of the guard
// above, and the one that catches the single most common deployment
// mistake there is: the volume was not mounted, or was mounted at the wrong
// path.
//
// Refusing is the only safe answer. A run mode that quietly provisioned
// instead would hand back a container that is healthy, serves a realm with
// the right name, and is a DIFFERENT domain — a new SID, no accounts, and
// no relationship to the one whose volume went missing. The message must
// therefore lead with the volume, because that is what the operator has to
// go and find.
//
// Maintenance mode is covered in the same breath: it needs an initialized
// domain for the same reason and is documented to refuse with the same
// code, and a `dbcheck` that ran against an empty directory and reported
// success would be worse than one that refused.
func TestRunModeWithoutStateRefused(t *testing.T) {
	net := harness.Network(t)

	for _, c := range []struct {
		mode      string
		container string
	}{
		{"run", refuseRun},
		{"maintenance", refuseMaint},
	} {
		t.Run(c.mode, func(t *testing.T) {
			// Fresh volumes: no WithVolumes, so the harness creates and
			// removes an empty pair — which is precisely a volume that
			// was never initialized, or one that was never mounted.
			code, logs := refuseWithin(t, net, c.container, c.mode, nil)
			mustExit(t, c.container, code, exitStateAbsent,
				c.mode+" mode on a volume holding no domain is refused with 21", logs)

			mustContain(t, c.mode+"-without-state refusal", logs,
				"ERROR: SAMBA_MODE="+c.mode+" needs an initialized domain "+
					"but the volume holds no samba state",
				"/var/lib/samba/private/sam.ldb is missing",
				// The volume remedy, first — the usual cause is a mount
				// that is not there.
				"mount the /var/lib/samba volume that holds the domain state",
				// And the deliberate alternative, for a volume that
				// really is new.
				"set SAMBA_MODE=provision or join once to initialize it")

			// It must not have initialized anything on the way out: that
			// is the entire difference between this refusal and the
			// silent re-provision it exists to prevent.
			mustNotContain(t, c.mode+"-without-state refusal", logs, initPhrases...)
		})
	}
}
