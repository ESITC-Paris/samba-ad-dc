package e2e

// The nominal single-DC matrix (SPEC Annex B.5): the behaviour a freshly
// provisioned domain controller is documented to have, proven over the
// protocols a real domain member speaks — Kerberos, SMB, DNS, LDAPS and
// NTP — from a separate container, plus the two things only the DC itself
// can answer for (its own configuration and its database).
//
// Every test function name here is a stable traceability ID (see the
// package comment in main_test.go) and MUST NOT be renamed.

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/esitc-paris/samba-ad-dc/test/e2e/harness"
)

// ---------------------------------------------------------------------
// the shared fixture
// ---------------------------------------------------------------------

// fixtureName is deliberately not "dc1": the harness smoke test owns that
// name, and `docker run --name` is a shared namespace — a second container
// claiming the name would silently destroy the first (StartDC force-removes
// a leftover of the same name). Every assertion below derives the host name
// it talks to from dc.FQDN, so the name is free to be unique.
const fixtureName = "nominal-dc1"

// nominalFixture is the provisioned DC every read-only test in this file
// shares, together with the network it lives on.
type nominalFixture struct {
	Net string
	DC  *harness.DC
}

var (
	fixtureOnce sync.Once
	fixture     *nominalFixture
)

// provisionedDC returns the domain controller this file's read-only tests
// share: provisioned once per package run, healthy, and alive until
// TestMain drains the package-lifetime teardown registry.
//
// Provisioning costs about a minute; doing it per test would multiply that
// by eight for no added coverage, because none of the tests that use it
// modifies the directory. The tests that do change state (TestDBConsistency
// here, and the operational matrix elsewhere) provision their own DC.
//
// Failure handling: the harness fails the calling test from inside the
// sync.Once, which leaves the fixture nil. Later tests then report that the
// fixture is unavailable and point at the first failure rather than
// producing a second, misleading one.
func provisionedDC(t *testing.T) *nominalFixture {
	t.Helper()

	fixtureOnce.Do(func() {
		net := harness.SharedNetwork(t)
		dc := harness.StartDC(t, net, fixtureName, "provision",
			map[string]string{
				"SAMBA_REALM":  harness.Realm,
				"SAMBA_DOMAIN": harness.Domain,
			},
			harness.Shared(),
			// Not harness.AdminSecret: its file lives in t.TempDir(), which
			// is removed when the first test to reach this code ends, while
			// this container must outlive it.
			harness.WithSecret("SAMBA_ADMIN_PASSWORD_FILE", "admin-password",
				sharedSecret(t, harness.AdminPassword)),
		)
		harness.WaitHealthy(t, dc.Name, harness.HealthTimeout)
		fixture = &nominalFixture{Net: net, DC: dc}
	})

	if fixture == nil {
		t.Fatalf("the shared provisioned DC could not be created; " +
			"the first test that needed it failed with the real cause")
	}

	// A shared container's teardown runs at AtExit, long after the test that
	// failed has finished, so harness.Shared() cannot dump its logs the way
	// t.Cleanup does for an owned one. Registering the dump here — on EVERY
	// test that uses the fixture, not just the one that created it — puts the
	// DC's log next to the failure that needs it, while the container is still
	// alive. It costs nothing on a green run.
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("--- docker logs %s (shared fixture) ---\n%s",
				fixture.DC.Name, harness.Logs(t, fixture.DC.Name))
		}
	})
	return fixture
}

// sharedSecret writes a throwaway secret to a file whose lifetime is the
// package run (t.TempDir would take it away when the first test ends) and
// returns its host path, ready to be bind-mounted.
func sharedSecret(t *testing.T, value string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "e2e-shared-secret")
	if err != nil {
		t.Fatalf("creating the fixture secret directory: %v", err)
	}
	harness.AtExit(func() { _ = os.RemoveAll(dir) })

	path := filepath.Join(dir, "secret")
	// 0644 for the same reason harness.Secret uses it: the container's
	// capability set is under test and must not need DAC_OVERRIDE to read
	// a file the harness mounted.
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil { //nolint:gosec // throwaway test secret
		t.Fatalf("writing the fixture secret: %v", err)
	}
	return path
}

// clientEnv is the environment a test client gets: the DC as its resolver
// (Kerberos and SMB both start from the realm's DNS records) and the
// throwaway password in a variable, so that no test ever puts the password
// on a command line the harness would echo into a failure message.
func clientEnv(f *nominalFixture) map[string]string {
	return map[string]string{
		harness.ClientDNSEnv: f.DC.IP,
		"E2E_PW":             harness.AdminPassword,
	}
}

// kinitCmd is the one way this file obtains a ticket: Heimdal's kinit reads
// the password from stdin, so it never reaches an argv or a process listing.
const kinitCmd = `printf %s "$E2E_PW" | kinit --password-file=STDIN Administrator@` + harness.Realm

func mustContain(t *testing.T, what, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Fatalf("%s: missing %q in output:\n%s", what, w, out)
		}
	}
}

// ---------------------------------------------------------------------
// B.5: provisioning
// ---------------------------------------------------------------------

// defaultFunctionLevel is the level the image provisions at when
// SAMBA_FUNCTION_LEVEL is not set, which is how the fixture starts its DC.
// It is spelled the way `samba-tool domain level show` prints it.
const defaultFunctionLevel = "(Windows) 2016"

// TestProvision asserts that a first boot on an empty volume really creates
// the domain it was asked for: the container reaches healthy, the
// entrypoint reports the provision as completed, and samba's own CLI reads
// the realm back out of the database as a naming context at the requested
// functional level.
func TestProvision(t *testing.T) {
	f := provisionedDC(t)

	// Healthy is the image's own application-level verdict (§6.4), not a
	// "the process is up" proxy: re-asserting it here is what ties this
	// test to the fixture rather than to a side effect of another test.
	harness.WaitHealthy(t, f.DC.Name, 30*time.Second)

	logs := harness.Logs(t, f.DC.Name)
	mustContain(t, "container logs", logs,
		fmt.Sprintf("provisioning a new domain %s in realm %s", harness.Domain, harness.Realm),
		fmt.Sprintf("domain %s provisioned", harness.Domain))

	// The levels are asserted by VALUE, not merely present: the whole point
	// of the SAMBA_FUNCTION_LEVEL mirror onto `ad dc functional level` is
	// that the domain comes up at the level that was asked for, and a check
	// that only looked for the label would pass at samba's 2008_R2 default.
	out := harness.Exec(t, f.DC.Name, "samba-tool", "domain", "level", "show")
	mustContain(t, "samba-tool domain level show", out,
		harness.BaseDN(),
		"Domain function level: "+defaultFunctionLevel,
		"Forest function level: "+defaultFunctionLevel)
}

// ---------------------------------------------------------------------
// B.5: DNS
// ---------------------------------------------------------------------

// TestDNSSRVRecords asserts that the records a domain member discovers its
// DC with are provisioned AND served: a client on the domain network asks
// the DC's DNS for the LDAP and Kerberos SRV records of the realm and gets
// this DC back, on the right ports, with a resolvable A record behind it.
//
// It deliberately asserts nothing about dynamic updates: samba_dnsupdate
// cannot complete against a docker bridge (Phase 2 hand-off), and what a
// member actually needs is the records being answered — which is what this
// checks.
func TestDNSSRVRecords(t *testing.T) {
	f := provisionedDC(t)
	zone := strings.ToLower(harness.Realm)
	env := clientEnv(f)

	for _, tc := range []struct {
		record string
		port   string
	}{
		{"_ldap._tcp." + zone, "389"},
		{"_kerberos._udp." + zone, "88"},
	} {
		out := harness.Client(t, f.Net, env,
			"dig", "+short", "SRV", tc.record, "@"+f.DC.IP)
		// dig +short prints "<prio> <weight> <port> <target>."
		mustContain(t, "dig SRV "+tc.record, out, " "+tc.port+" "+f.DC.FQDN+".")
	}

	// The SRV target must itself resolve, or the records are decoration.
	out := harness.Client(t, f.Net, env, "dig", "+short", f.DC.FQDN, "@"+f.DC.IP)
	if got := strings.TrimSpace(out); got != f.DC.IP {
		t.Fatalf("dig %s: got %q, want the DC address %q", f.DC.FQDN, got, f.DC.IP)
	}
}

// ---------------------------------------------------------------------
// B.5: Kerberos
// ---------------------------------------------------------------------

// TestKerberosKinit asserts the KDC issues tickets: a client with no
// configuration beyond the DC as its resolver finds the KDC through the
// realm's SRV records, authenticates Administrator, and ends up holding a
// TGT — and that the KDC rejects a wrong password, without which "kinit
// succeeded" would prove nothing about authentication.
func TestKerberosKinit(t *testing.T) {
	f := provisionedDC(t)

	out := harness.Client(t, f.Net, clientEnv(f), "sh", "-c", kinitCmd+" && klist")
	mustContain(t, "kinit + klist", out,
		"Administrator@"+harness.Realm,
		"krbtgt/"+harness.Realm+"@"+harness.Realm)

	bad := clientEnv(f)
	bad["E2E_PW"] = harness.AdminPassword + "-wrong"
	code, out := harness.ClientErr(t, f.Net, bad, "sh", "-c", kinitCmd)
	if code == 0 {
		t.Fatalf("kinit with a wrong password succeeded; the KDC is not authenticating:\n%s", out)
	}
	// The REASON has to be the password, not the client failing to find a
	// KDC, a broken DNS answer or a clock skew — all of which also exit
	// non-zero and would leave "the KDC rejects bad passwords" unproven.
	// Heimdal renders the KDC's preauthentication failure as this line.
	mustContain(t, "kinit with a wrong password", out, "Password incorrect")
}

// TestKerberizedSMB asserts that the ticket is worth something: after
// kinit, smbclient reaches the netlogon share with Kerberos *required* —
// no password anywhere on that command line — and the same command with no
// ticket in the cache fails, which is what makes the success a proof that
// GSSAPI/SPNEGO over SMB works end to end.
func TestKerberizedSMB(t *testing.T) {
	f := provisionedDC(t)
	share := "//" + f.DC.FQDN + "/netlogon"

	out := harness.Client(t, f.Net, clientEnv(f), "sh", "-c",
		kinitCmd+" && smbclient --use-kerberos=required "+share+" -c ls")
	mustContain(t, "kerberized smbclient ls", out, "blocks of size")

	code, out := harness.ClientErr(t, f.Net, clientEnv(f), "sh", "-c",
		"smbclient -N --use-kerberos=required "+share+" -c ls")
	if code == 0 {
		t.Fatalf("smbclient --use-kerberos=required succeeded with an empty ticket cache; "+
			"the share is not actually Kerberos-protected:\n%s", out)
	}
	// As with kinit above, the REASON is asserted and not just the exit
	// code — otherwise an unreachable DC would "prove" the same thing. With
	// Kerberos required and nothing in the cache the client has no mechanism
	// left to offer, and SPNEGO says so before a session setup is even
	// attempted. That is a client-side refusal by construction: it is the
	// absence of the ticket, not a DC-side rejection, and it is exactly the
	// difference from the successful run above.
	mustContain(t, "kerberized smbclient with an empty ticket cache", out,
		"Could not find a suitable mechtype in NEG_TOKEN_INIT")
}

// ---------------------------------------------------------------------
// B.5: NTLM
// ---------------------------------------------------------------------

// TestNTLMAuth asserts the NTLM path a legacy member still uses: with
// Kerberos switched off, a DOMAIN\user session setup succeeds against
// netlogon and a wrong password is refused with the documented status.
func TestNTLMAuth(t *testing.T) {
	f := provisionedDC(t)
	share := "//" + f.DC.FQDN + "/netlogon"
	cmd := func(pw string) string {
		return `smbclient --use-kerberos=off ` + share +
			` -U "` + harness.Domain + `\\Administrator%` + pw + `" -c ls`
	}

	out := harness.Client(t, f.Net, clientEnv(f), "sh", "-c", cmd(`$E2E_PW`))
	mustContain(t, "NTLM smbclient ls", out, "blocks of size")

	code, out := harness.ClientErr(t, f.Net, clientEnv(f), "sh", "-c",
		cmd(`$E2E_PW-wrong`))
	if code == 0 {
		t.Fatalf("NTLM session setup succeeded with a wrong password:\n%s", out)
	}
	mustContain(t, "NTLM with a wrong password", out, "NT_STATUS_LOGON_FAILURE")
}

// ---------------------------------------------------------------------
// B.5: LDAPS
// ---------------------------------------------------------------------

// TestLDAPSCertificate asserts that LDAPS is served with a certificate that
// actually verifies — the property a member's TLS stack enforces and that
// `-x` with verification off would hide.
//
// Two independent halves:
//   - on the host, the DC's generated certificate is parsed and verified
//     against its own CA for the DC's host name, so a wrong CN, a missing
//     SAN or an expired certificate fails here with a precise message;
//   - from the client container, ldapsearch reads the rootDSE over
//     ldaps://, with LDAPTLS_REQCERT=demand and the copied CA as its only
//     trust anchor, and the same query without that CA must fail.
//
// The check runs container-side on purpose: the DC's address is on a docker
// bridge, which a macOS host cannot route to.
func TestLDAPSCertificate(t *testing.T) {
	f := provisionedDC(t)

	caPEM := harness.CopyFrom(t, f.DC.Name, "/var/lib/samba/private/tls/ca.pem")
	certPEM := harness.CopyFrom(t, f.DC.Name, "/var/lib/samba/private/tls/cert.pem")

	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatalf("the DC's cert.pem is not PEM:\n%s", certPEM)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parsing the DC's certificate: %v", err)
	}
	if cert.Subject.CommonName != f.DC.FQDN {
		t.Fatalf("certificate CN = %q, want the DC host name %q",
			cert.Subject.CommonName, f.DC.FQDN)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatalf("the DC's ca.pem holds no usable certificate:\n%s", caPEM)
	}
	if _, err := cert.Verify(x509.VerifyOptions{
		DNSName:   f.DC.FQDN,
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("the DC's certificate does not verify against its own CA for %s: %v",
			f.DC.FQDN, err)
	}

	// The certificate is public material, so it travels to the client in an
	// environment variable and is written out there: the client image gets
	// no bind mounts, and nothing secret is involved.
	env := clientEnv(f)
	env["E2E_CA_PEM"] = string(caPEM)
	const writeCA = `printf %s "$E2E_CA_PEM" > /tmp/ca.pem; `
	search := "ldapsearch -H ldaps://" + f.DC.FQDN + ":636 -x -s base -b '' " +
		"defaultNamingContext dnsHostName"

	out := harness.Client(t, f.Net, env, "sh", "-c",
		writeCA+"LDAPTLS_CACERT=/tmp/ca.pem LDAPTLS_REQCERT=demand "+search)
	mustContain(t, "ldaps rootDSE", out,
		"defaultNamingContext: "+harness.BaseDN(),
		"dnsHostName: "+f.DC.FQDN,
		"result: 0 Success")

	code, out := harness.ClientErr(t, f.Net, env, "sh", "-c",
		"LDAPTLS_REQCERT=demand "+search)
	if code == 0 {
		t.Fatalf("ldaps succeeded without the DC's CA in the trust store; "+
			"the client is not verifying the certificate:\n%s", out)
	}
}

// ---------------------------------------------------------------------
// B.5 / B.6: signed NTP
// ---------------------------------------------------------------------

// TestSignedNTPWiring asserts the MS-SNTP *wiring* the image claims, and is
// deliberate about where that stops.
//
// What it proves: samba creates and serves the signing socket; chrony is
// configured against that exact directory (the two ends are configured in
// different files that nothing else reconciles, so asserting they agree is
// the point); and chronyd is actually serving time to the domain network,
// from a clock it is not allowed to discipline.
//
// What it does NOT prove: that a *signed* reply is produced. chrony opens
// the signing socket lazily, only when a request arrives carrying an
// authenticator, and producing one requires a client authenticating as a
// domain machine account — which the protocol test-client is not. The
// measurement below therefore succeeds whether or not signing works. That
// boundary is recorded in the profile's B.6 known limitations; a regression
// inside the signing path itself would show up as a chrony log error, not
// as a failure here.
//
// It remains the acceptance test named by the chrony-as-root ruling (B.6):
// chronyd unable to open the socket directory, or refusing to serve its
// undisciplined clock, fails here.
func TestSignedNTPWiring(t *testing.T) {
	f := provisionedDC(t)

	// The two ends agree, and they agree on the directory THIS DC declares
	// rather than on a constant — see assertSignedNTPWiring.
	signdDir := assertSignedNTPWiring(t, f.DC.Name)
	if signdDir != provisionedSigndDir {
		t.Fatalf("a provisioned DC keeps the signing socket in %q, not %q",
			provisionedSigndDir, signdDir)
	}

	// The generated configuration is a copy of the template, so the
	// template's own value has to stay samba's default: it is what a DC
	// gets when testparm cannot be read.
	template := strings.TrimSpace(harness.Exec(t, f.DC.Name, "sh", "-c",
		"grep '^ntpsigndsocket' "+chronyTemplatePath))
	if template != "ntpsigndsocket "+provisionedSigndDir {
		t.Fatalf("the baked template's fallback is %q, want %q",
			template, "ntpsigndsocket "+provisionedSigndDir)
	}

	// And the service answers: a one-shot query (never disciplining the
	// client's clock) that produces a measurement. Unauthenticated, per the
	// boundary in this test's doc comment.
	assertServesTime(t, f.Net, f.DC.IP)
}

// Where the MS-SNTP signing socket lives, and where the two configurations
// that have to agree about it are. The generated file is what chronyd is
// started with (`-f`); the template is only its fallback default.
const (
	provisionedSigndDir  = "/var/lib/samba/ntp_signd"
	chronyTemplatePath   = "/etc/chrony/chrony.conf"
	chronyGeneratedPath  = "/run/chrony/chrony.conf"
	chronySigndDirective = "ntpsigndsocket"
)

// assertSignedNTPWiring asserts, on the named DC container, that samba is
// serving the MS-SNTP signing socket in the directory its own smb.conf
// declares and that the chrony configuration chronyd was started with names
// that same directory. It returns the directory.
//
// Nothing in samba or in chrony reconciles those two files, which is why
// asserting they agree is the point — and why the DC's OWN value is read
// first instead of a constant being checked twice. `ntp signd socket
// directory` is an smb.conf parameter on a volume the operator owns, so a
// helper that hardcoded the default would pass on exactly the DCs where
// nothing can go wrong and say nothing about the ones where it can.
func assertSignedNTPWiring(t *testing.T, container string) string {
	t.Helper()

	// samba's effective configuration, read through its own parser — the
	// same question the entrypoint asks when it generates chrony.conf.
	signdDir := strings.TrimSpace(harness.Exec(t, container, "sh", "-c",
		`testparm -s --parameter-name="ntp signd socket directory" 2>/dev/null`))
	if !strings.HasPrefix(signdDir, "/") {
		t.Fatalf("`ntp signd socket directory` on %s read back as %q, which is not a path", container, signdDir)
	}

	// samba names the socket after the service; asserting the exact path
	// rather than "something socket-shaped is in the directory" is what makes
	// a failure say which file is missing.
	signdSocket := signdDir + "/socket"
	code, out := harness.ExecErr(t, container, "sh", "-c",
		"test -S "+signdSocket+" || { ls -l "+signdDir+"; exit 1; }")
	if code != 0 {
		t.Fatalf("%s is not a socket; samba is not serving MS-SNTP signing "+
			"requests. Contents of %s:\n%s", signdSocket, signdDir, out)
	}

	// ...and chrony's, which must name the same directory or the signing
	// hand-off silently does nothing. The GENERATED file is read, because
	// that is the one chronyd was started with.
	generated := strings.TrimSpace(harness.Exec(t, container, "sh", "-c",
		"grep '^"+chronySigndDirective+"' "+chronyGeneratedPath))
	if want := chronySigndDirective + " " + signdDir; generated != want {
		t.Fatalf("%s in %s = %q, want %q — chrony would hand signing requests "+
			"to a socket samba never creates", chronySigndDirective, chronyGeneratedPath, generated, want)
	}
	return signdDir
}

// assertServesTime asserts that a client on the domain network gets a usable
// measurement out of the DC at ip: a one-shot chronyd that never disciplines
// the client's clock. Unauthenticated, per the boundary in
// TestSignedNTPWiring's doc comment.
func assertServesTime(t *testing.T, net, ip string) {
	t.Helper()
	measured := harness.Client(t, net, map[string]string{harness.ClientDNSEnv: ip}, "sh", "-c",
		`timeout 60 chronyd -Q -t 30 "server `+ip+` iburst"`)
	mustContain(t, "chronyd -Q against the DC at "+ip, measured, "System clock wrong by")
}

// ---------------------------------------------------------------------
// B.5 / B.4: database consistency and maintenance mode
// ---------------------------------------------------------------------

// TestDBConsistency asserts that a domain this image provisions passes
// samba's own consistency check, both while the DC runs and — the
// documented operator procedure — from a maintenance-mode container on the
// stopped volume.
//
// It provisions its own DC rather than using the shared fixture: running
// dbcheck as the entrypoint's maintenance mode requires the volume to be
// quiescent, and stopping the shared fixture would sabotage every other
// test in this file.
func TestDBConsistency(t *testing.T) {
	net := harness.Network(t)
	dc := harness.StartDC(t, net, "dbcheck-dc", "provision", map[string]string{
		"SAMBA_REALM":  harness.Realm,
		"SAMBA_DOMAIN": harness.Domain,
	}, harness.AdminSecret(t))
	harness.WaitHealthy(t, dc.Name, harness.HealthTimeout)

	// Scope note: bare `samba-tool dbcheck` checks the DEFAULT naming context
	// only — `--cross-ncs` (schema, configuration, the DNS partitions) is not
	// run here, and deliberately so: this mirrors exactly what the
	// entrypoint's maintenance mode does, and a test that checked more than
	// the shipped procedure would pass while the procedure stayed blind.
	mustContain(t, "samba-tool dbcheck on the running DC",
		harness.Exec(t, dc.Name, "samba-tool", "dbcheck"), "(0 errors)")

	// A container asked to stop exits 0 (§6.3) — and the volume is now
	// quiescent, which is the precondition maintenance mode documents.
	if code := harness.Stop(t, dc.Name, 30*time.Second); code != 0 {
		t.Fatalf("stopping the DC before maintenance: exit code = %d, want 0", code)
	}

	// Maintenance mode on that same volume: it checks the database, says so,
	// starts no domain controller, and exits 0.
	code, logs := harness.RunDCExpectExit(t, net, "dbcheck-maintenance", "maintenance", nil,
		harness.WithVolumes(dc.StateVolume, dc.ConfVolume))
	if code != 0 {
		t.Fatalf("maintenance mode: exit code = %d, want 0\n%s", code, logs)
	}
	mustContain(t, "maintenance-mode logs", logs,
		"running samba-tool dbcheck",
		"(0 errors)",
		"database check completed with no errors",
		"maintenance mode does not start the domain controller")
	if strings.Contains(logs, "samba --foreground") {
		t.Fatalf("maintenance mode started the domain controller:\n%s", logs)
	}
}
