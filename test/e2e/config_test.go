package e2e

// The declarative-configuration row of the B.5 matrix: what an operator can
// put into the DC's `[global]` section without editing a file on a volume,
// and what the image does with an entry samba cannot parse.
//
// Every test function name here is a stable traceability ID (see the
// package comment in main_test.go) and MUST NOT be renamed.

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/esitc-paris/samba-ad-dc/test/e2e/harness"
)

// Container names. Fixed rather than generated, and within the 15-character
// NetBIOS limit the harness enforces (deployment guide §1.7).
const (
	globalOptsFirst  = "globalopt-dc1"
	globalOptsSecond = "globalopt-dc2"
	globalOptsBad    = "globalopt-bad"
)

// smbConfPath is the configuration file the DC reads, on the configuration
// volume — which is what makes a bad rewrite outlive the container that made
// it, and why the entrypoint puts the previous bytes back before refusing.
const smbConfPath = "/etc/samba/smb.conf"

// The two settings the test declares.
//
// Both are deliberately INERT: they change how much the DC writes to its own
// log and nothing else. That is a requirement, not a convenience — the image
// health-checks itself by probing DNS, LDAP and SMB on the loopback address,
// so a setting that changed how the DC answers there (`smb encrypt =
// required` is the obvious one: the SMB probe connects anonymously) would
// make this test fail for a reason that has nothing to do with whether the
// declarative block works.
//
// maxLogSize is the one the restart changes; deadtime is the one it leaves
// alone, so the same run proves both halves — a change applied and an
// unchanged setting left untouched.
//
// Every value here is deliberately NOT samba's default, and that is the whole
// discriminating power of the provision half of this test. The defaults in
// this image, measured with `testparm --parameter-name` on a configuration
// that sets neither, are `max log size = 5000` and `deadtime = 10080`; a test
// that declared those would read them back from a DC that ignored the
// variable entirely and still pass.
//
// Both are also read back FAITHFULLY by testparm, which not every parameter
// is: `log level` would have been the obvious second setting, but testparm
// sets its own debug level on its own command line, so it reports `1`
// whatever the file says. A test asserting on that would be asserting on
// testparm, not on the DC.
const (
	maxLogSizeKey    = "max log size"
	maxLogSizeFirst  = "4000"
	maxLogSizeSecond = "8000"

	deadtimeKey   = "deadtime"
	deadtimeValue = "20160"

	// The samba defaults the values above must differ from, named here so
	// that a future edit which happens to pick one is obvious.
	maxLogSizeDefault = "5000"
	deadtimeDefault   = "10080"
)

// globalOptionsWorstCase is what this test can consume if every budget is
// spent: one provision, one restart on an already initialized volume, and one
// container that is expected to refuse, plus the two stops. It is checked
// against the deadline before anything is started, for the reason
// requireDeadline documents: being killed by `go test` skips every teardown.
const globalOptionsWorstCase = harness.HealthTimeout + harness.HealthTransitionTimeout +
	harness.ExitTimeout + 2*stopTimeout + time.Minute

// globalSetting reads one [global] parameter back through samba's own
// parser, inside the container.
//
// testparm is asked rather than the file grepped on purpose: what matters is
// not that a line exists in smb.conf but that samba READS the value the
// operator asked for. stderr carries testparm's banner and is dropped here,
// because `docker exec` merges the two streams and the banner would end up
// inside the value.
func globalSetting(t *testing.T, container, key string) string {
	t.Helper()
	out := harness.Exec(t, container, "sh", "-c",
		"testparm -s -l --parameter-name='"+key+"' "+smbConfPath+" 2>/dev/null")
	return strings.TrimSpace(out)
}

// TestGlobalOptionsApplied covers B.5 row N9: `SAMBA_GLOBAL_OPTIONS` is
// applied to the DC's `[global]` section, reconciled on every start, and
// gated by samba's own parser.
//
// Three things are asserted, and each one alone would be satisfied by a
// broken image:
//
//   - a provision carrying the variable produces a DC whose own parser reads
//     the declared values back;
//   - a restart with a CHANGED value applies the change and says so, while
//     the setting that did not change is left alone — configuration is
//     reconciled on every start (§6.2: the state is the directory, the
//     options are configuration), and an image that only applied the block at
//     provision time would leave an operator with no way to change it on the
//     DC they already have;
//   - an entry samba cannot parse stops the boot with exit 10 AND leaves the
//     configuration file byte-for-byte as it was. The last half is the one
//     that matters most: smb.conf lives on a volume, so an image that wrote
//     the rejected settings and then refused would break every later start,
//     including the one made right after removing the offending line.
func TestGlobalOptionsApplied(t *testing.T) {
	requireDeadline(t, globalOptionsWorstCase)
	// The provision half below asserts that samba reads the declared values
	// back. That assertion is only worth anything while the values differ
	// from what a DC would report having never seen the variable, so the
	// premise is checked rather than trusted to a comment.
	if maxLogSizeFirst == maxLogSizeDefault || maxLogSizeSecond == maxLogSizeDefault || deadtimeValue == deadtimeDefault {
		t.Fatalf("this test declares a samba default (%s=%s/%s, %s=%s): it would pass against an "+
			"image that ignored SAMBA_GLOBAL_OPTIONS entirely",
			maxLogSizeKey, maxLogSizeFirst, maxLogSizeSecond, deadtimeKey, deadtimeValue)
	}
	net := harness.Network(t)

	// A comment and a blank line are part of the input on purpose: the
	// variable is written as a compose `|` block scalar, so this is what it
	// really looks like in an operator's file.
	declared := "# how big this DC's log may get, and when it drops an idle connection\n\n" +
		maxLogSizeKey + " = " + maxLogSizeFirst + "\n" +
		deadtimeKey + " = " + deadtimeValue + "\n"

	dc := harness.StartDC(t, net, globalOptsFirst, "provision", map[string]string{
		"SAMBA_REALM":          harness.Realm,
		"SAMBA_DOMAIN":         harness.Domain,
		"SAMBA_GLOBAL_OPTIONS": declared,
	}, harness.AdminSecret(t))
	harness.WaitHealthy(t, dc.Name, harness.HealthTimeout)

	if got := globalSetting(t, dc.Name, maxLogSizeKey); got != maxLogSizeFirst {
		t.Fatalf("after a provision declaring %q = %q, samba reads %q back from %s:\n--- logs ---\n%s",
			maxLogSizeKey, maxLogSizeFirst, got, smbConfPath, harness.Logs(t, dc.Name))
	}
	if got := globalSetting(t, dc.Name, deadtimeKey); got != deadtimeValue {
		t.Fatalf("after a provision declaring %q = %q, samba reads %q back from %s",
			deadtimeKey, deadtimeValue, got, smbConfPath)
	}

	if code := harness.Stop(t, dc.Name, stopTimeout); code != 0 {
		t.Fatalf("`docker stop` left container %s with exit code %d, want 0\n--- logs ---\n%s",
			dc.Name, code, harness.Logs(t, dc.Name))
	}

	// --- the change ------------------------------------------------------
	//
	// A NEW container on the SAME volumes, which is what an orchestrator
	// does when the variable changes, with one of the two settings edited
	// and the other left exactly as it was.
	changed := maxLogSizeKey + " = " + maxLogSizeSecond + "\n" + deadtimeKey + " = " + deadtimeValue + "\n"
	again := harness.StartDC(t, net, globalOptsSecond, "run", map[string]string{
		"SAMBA_GLOBAL_OPTIONS": changed,
	}, harness.WithVolumes(dc.StateVolume, dc.ConfVolume))
	harness.WaitHealthy(t, again.Name, harness.HealthTransitionTimeout)

	if got := globalSetting(t, again.Name, maxLogSizeKey); got != maxLogSizeSecond {
		t.Fatalf("after a restart declaring %q = %q, samba still reads %q back:\n--- logs ---\n%s",
			maxLogSizeKey, maxLogSizeSecond, got, harness.Logs(t, again.Name))
	}
	restartLogs := harness.Logs(t, again.Name)
	mustContain(t, "restart logs of "+again.Name, restartLogs,
		`SAMBA_GLOBAL_OPTIONS: replaced "`+maxLogSizeKey+`" = "`+maxLogSizeSecond+`" in `+smbConfPath)

	// Idempotence, on the real thing: the setting that did not change must
	// not be rewritten. A reconciliation that cannot recognise its own work
	// would rewrite smb.conf on every boot and fill the log with changes
	// nobody made.
	if strings.Contains(restartLogs, `"`+deadtimeKey+`"`) {
		t.Fatalf("the restart re-applied %q, which did not change: the reconciliation is not "+
			"idempotent against the value samba itself wrote\n--- logs ---\n%s", deadtimeKey, restartLogs)
	}

	if code := harness.Stop(t, again.Name, stopTimeout); code != 0 {
		t.Fatalf("`docker stop` left container %s with exit code %d, want 0", again.Name, code)
	}

	// --- the entry samba rejects ----------------------------------------
	//
	// Read the file off the volume BEFORE the refusing boot, so the
	// comparison afterwards is against the actual bytes rather than against
	// what this test believes they were.
	before := harness.CopyFrom(t, again.Name, smbConfPath)

	code, logs := harness.RunDCExpectExit(t, net, globalOptsBad, "run", map[string]string{
		"SAMBA_GLOBAL_OPTIONS": "this is not a parameter = 1",
	}, harness.WithVolumes(dc.StateVolume, dc.ConfVolume))
	if code != exitConfigError {
		t.Fatalf("a [global] entry samba cannot parse exited %d, want %d (configuration error)\n--- logs ---\n%s",
			code, exitConfigError, logs)
	}
	// The message has to carry all three: what refused it (testparm, in its
	// own words), which variable to fix, and the fact that the file was put
	// back.
	mustContain(t, "refusal of "+globalOptsBad, logs,
		"SAMBA_GLOBAL_OPTIONS",
		"testparm",
		"this is not a parameter")

	after := harness.CopyFrom(t, globalOptsBad, smbConfPath)
	if !bytes.Equal(before, after) {
		t.Fatalf("the refused boot left %s modified on the volume:\n--- before ---\n%s\n--- after ---\n%s",
			smbConfPath, before, after)
	}
}
