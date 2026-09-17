package e2e

// Every test function name here is a stable traceability ID (see the
// package comment in main_test.go) and MUST NOT be renamed.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esitc-paris/samba-ad-dc/test/e2e/harness"
)

// Container names. Fixed for the reason documented on harness.UniqueName,
// and all within harness.NetBIOSNameLimit — including the two that are
// exempt from the guard — so that no name here depends on which side of
// that line it happens to fall.
const (
	derivedDC      = "derived-dc1"     // provisions: the 15-character limit is real for this one
	derivedRefuse  = "derived-refuse"  // expected to exit; never reaches samba
	derivedBaseVer = "derived-basever" // one-off `entrypoint --version` on the BASE image
)

// derivedRepo is the repository half of the tag the derived image is built
// under. The tag half is unique per run (harness.UniqueName), because two
// suites on one machine share the docker image namespace exactly the way
// they share the container one.
const derivedRepo = "samba-ad-dc-derived-e2e"

// derivedMarkerPath is where the derived image drops its one added file.
//
// /usr/share/samba-ad-dc is the image's own data directory (it already
// holds runtime-packages.txt), so this also demonstrates the thing a
// downstream project actually does: add content under a path the base
// image owns, on a rootfs that is READ-ONLY at runtime. A derived image
// may write there at BUILD time and must not expect to at run time.
const derivedMarkerPath = "/usr/share/samba-ad-dc/derived-marker"

// derivedMarkerText is the file's content. It names the test so that a
// human who finds this file inside a stray image knows where it came from.
const derivedMarkerText = "built by TestDerivedImageInheritsContract\n"

// derivedWorstCase is the budget this test declares to requireDeadline: the
// build, the provision that has to reach healthy, and TWO containers bounded
// by harness.ExitTimeout — the one-off `--version` on the base image, and the
// refusal at the end — plus a minute of slack.
//
// It is NOT the arithmetic sum of every timeout the test can open, and does
// not claim to be. The two `docker exec`s are each bounded by
// harness.ExecTimeout and the eight inspects by the harness's internal
// docker timeout, so a literal upper bound would be past half an hour — a
// number no run has ever approached (14 s in CI, both architectures) and one
// that would make the test refuse to start on a perfectly healthy suite,
// which is the failure requireDeadline exists to avoid, not to cause. What
// is counted is therefore the same thing globalOptionsWorstCase and
// customTLSWorstCase count: the container budgets, which are the only ones
// large enough to eat a binary deadline, plus explicit slack for the short
// commands in between.
const derivedWorstCase = harness.BuildTimeout + harness.HealthTimeout +
	2*harness.ExitTimeout + time.Minute

// ---------------------------------------------------------------------
// B.5: a derived image inherits the runtime contract
// ---------------------------------------------------------------------

// TestDerivedImageInheritsContract asserts the property the reuse guide
// sells: `FROM` this image and you get the whole runtime contract, not
// just the binaries.
//
// This is the row with the highest ratio of promise to evidence, because
// every part of the contract a derived image inherits is inherited
// SILENTLY. Nothing in a downstream project's Dockerfile mentions the
// healthcheck, the volumes, tini, the Kerberos configuration or the
// refusals — they are simply there, until the day a derived build adds an
// `ENTRYPOINT` of its own, or a `VOLUME` line that shadows one, and the
// image still starts, still serves the domain, and has quietly stopped
// being a domain controller docker can supervise. A downstream project
// would find that out in production; this test finds it out here.
//
// What is asserted, in order:
//
//   - the derived image builds at all, from the image under test as its
//     base, with nothing but one COPY and one ENV;
//   - it provisions a real domain and reaches the image's own health
//     verdict — the healthcheck is not merely declared, it works;
//   - the added layer is present (so the build really was derived, and
//     the assertions below are about a MODIFIED image rather than the
//     base one under another tag);
//   - `entrypoint --version` reports exactly what the base image reports,
//     which is what rules out the other way this test could pass
//     vacuously: a derived build that somehow replaced the entrypoint;
//   - the four declarations docker itself acts on — ENTRYPOINT,
//     HEALTHCHECK, VOLUME and ENV — survived the derivation, together
//     with the OCI labels a consumer identifies the image by;
//   - and the refusals came along too: run mode on a volume holding no
//     domain still exits 21 rather than inventing a domain, which is the
//     safety property of the contract and the one a downstream operator
//     is most likely to meet by accident (a volume that was not mounted).
//
// The last point is why the test does not stop at the inspect: an image
// can declare every one of those lines correctly and still ship an
// entrypoint that behaves differently. The exit code is behaviour.
func TestDerivedImageInheritsContract(t *testing.T) {
	requireDeadline(t, derivedWorstCase)

	derived := buildDerivedImage(t)
	net := harness.Network(t)

	dc := harness.StartDC(t, net, derivedDC, "provision", map[string]string{
		"SAMBA_REALM":  harness.Realm,
		"SAMBA_DOMAIN": harness.Domain,
	}, harness.WithImage(derived), harness.AdminSecret(t))
	harness.WaitHealthy(t, dc.Name, harness.HealthTimeout)

	// The added layer. Without this the whole test could be passing
	// against the base image under a second tag.
	if got := strings.TrimSpace(harness.Exec(t, dc.Name, "cat", derivedMarkerPath)); got != strings.TrimSpace(derivedMarkerText) {
		t.Fatalf("%s reads %q, want %q; the container is not running the derived image",
			derivedMarkerPath, got, strings.TrimSpace(derivedMarkerText))
	}

	// The same entrypoint binary, reporting the same Samba. The base's
	// answer is obtained from a one-off container of the image under test
	// rather than hard-coded: the version moves with every upstream
	// release, and a literal here would have to be edited by the same
	// person who would then not notice it had gone stale.
	code, logs := harness.RunDCExpectExit(t, net, derivedBaseVer, "", nil,
		harness.WithEntrypoint("/usr/local/bin/entrypoint", "--version"))
	mustExit(t, derivedBaseVer, code, 0, "`entrypoint --version` reports and exits 0", logs)
	baseVersion := strings.TrimSpace(logs)
	if baseVersion == "" {
		t.Fatalf("`entrypoint --version` on the image under test printed nothing")
	}

	if got := strings.TrimSpace(harness.Exec(t, dc.Name, "/usr/local/bin/entrypoint", "--version")); got != baseVersion {
		t.Errorf("the derived image's entrypoint reports %q, the base image's reports %q; "+
			"a derived image must not change the entrypoint it inherited", got, baseVersion)
	}

	assertInheritedImageConfig(t, dc.Name, derived)

	// And the behaviour, not just the declarations: run mode on an empty
	// volume pair is still refused with 21. Fresh volumes, because this is
	// precisely the operator whose `-v` was forgotten or misspelled.
	refuseCode, refuseLogs := refuseWithin(t, net, derivedRefuse, "run", nil,
		harness.WithImage(derived))
	mustExit(t, derivedRefuse, refuseCode, exitStateAbsent,
		"a derived image inherits the refusals: run mode on a volume holding no domain exits 21",
		refuseLogs)
	mustContain(t, "the derived image's run-without-state refusal", refuseLogs,
		"ERROR: SAMBA_MODE=run needs an initialized domain "+
			"but the volume holds no samba state",
		"/var/lib/samba/private/sam.ldb is missing")
	mustNotContain(t, "the derived image's run-without-state refusal", refuseLogs, initPhrases...)
}

// buildDerivedImage writes the smallest possible derived image into a
// temporary build context and builds it, returning the tag.
//
// Minimal on purpose: one COPY and one ENV, and NOT a line more. Every
// extra instruction would be another candidate explanation for a failure
// — the point of the test is that a derivation which changes nothing
// relevant changes nothing at all, and only a Dockerfile that plainly
// changes nothing relevant can prove that.
//
// The base is harness.Image(), so this builds FROM whatever the run is
// testing: `samba-ad-dc:dev` on a laptop, the `samba-ad-dc:ci` image the
// build job just produced on a runner. Neither is in a registry, and
// neither is pulled: docker resolves `FROM` against the local store first,
// and Preflight has already refused to run at all if it is not there.
func buildDerivedImage(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte(derivedMarkerText), 0o644); err != nil {
		t.Fatalf("writing the derived image's marker file: %v", err)
	}
	dockerfile := "FROM " + harness.Image() + "\n" +
		"COPY marker " + derivedMarkerPath + "\n" +
		"ENV DERIVED=1\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatalf("writing the derived image's Dockerfile: %v", err)
	}

	ref := derivedRepo + ":" + harness.UniqueName("run")
	harness.Build(t, ref, dir)
	return ref
}

// assertInheritedImageConfig checks the declarations docker acts on and a
// consumer reads, on the RUNNING container for the runtime ones and on the
// image itself for the labels.
//
// The runtime four are read off the container rather than the image
// because that is where they take effect: `.Config` of a container is the
// image's configuration as docker resolved it for this run, so a
// declaration that was inherited but then overridden at `docker run` time
// would be caught here and invisible on the image.
//
// Each expected value is spelled out as the literal JSON docker renders,
// rather than assembled from constants shared with the Dockerfile. This is
// a black-box test of a published contract, for the same reason
// negative_test.go spells out the exit codes: a value shared with the
// thing under test can be changed on both sides at once and never fail.
func assertInheritedImageConfig(t *testing.T, container, image string) {
	t.Helper()

	for _, c := range []struct{ what, format, want string }{
		{
			// tini is PID 1 and the entrypoint is its child: a derived
			// image that replaced either stops reaping zombies or stops
			// honouring SIGTERM, and both look fine until they do not.
			"ENTRYPOINT",
			"{{json .Config.Entrypoint}}",
			`["/usr/bin/tini","--","/usr/local/bin/entrypoint"]`,
		},
		{
			// WaitHealthy above proves a healthcheck runs and passes; this
			// proves it is the image's own, unchanged.
			"HEALTHCHECK",
			"{{json .Config.Healthcheck.Test}}",
			`["CMD","/usr/local/bin/entrypoint","healthcheck"]`,
		},
		{
			// Both, and only both. A derived image that lost one of these
			// would give an operator who did not pass an explicit `-v` an
			// anonymous volume for the other, or none at all.
			"VOLUME",
			"{{json .Config.Volumes}}",
			`{"/etc/samba":{},"/var/lib/samba":{}}`,
		},
	} {
		if got := harness.Inspect(t, "container", container, c.format); got != c.want {
			t.Errorf("the derived image's %s is %s, want %s; a derived image must not change it",
				c.what, got, c.want)
		}
	}

	// ENV is asserted by containment rather than equality: the harness adds
	// SAMBA_* variables of its own to every run, so the full list is a
	// property of this test and not of the image. What matters is that the
	// image's own variable survived — and that the derivation's variable is
	// there too, which is what makes this a derived image at all.
	env := harness.Inspect(t, "container", container, "{{json .Config.Env}}")
	mustContain(t, "the derived image's ENV", env,
		`"KRB5_CONFIG=/var/lib/samba/private/krb5.conf"`,
		`"DERIVED=1"`)

	// The labels are read off the IMAGE, not the container: a container's
	// .Config.Labels merges in the `--label` the harness passes to `docker
	// run`, so asserting there would be asserting partly on the harness.
	//
	// Compared against the base image's values rather than checked for
	// presence, because the claim is inheritance: a derived build that
	// re-declared its own version label would pass a presence check while
	// telling every consumer something different from what it is.
	for _, label := range []string{
		"org.esitc-paris.spec-version",
		"org.opencontainers.image.version",
	} {
		format := `{{index .Config.Labels "` + label + `"}}`
		base := harness.Inspect(t, "image", harness.Image(), format)
		if base == "" {
			t.Fatalf("the image under test carries no %s label; "+
				"there is nothing for a derived image to inherit", label)
		}
		if got := harness.Inspect(t, "image", image, format); got != base {
			t.Errorf("the derived image's %s label is %q, the base image's is %q",
				label, got, base)
		}
	}
}
