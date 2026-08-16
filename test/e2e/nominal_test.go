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

	out := harness.Exec(t, f.DC.Name, "samba-tool", "domain", "level", "show")
	mustContain(t, "samba-tool domain level show", out,
		harness.BaseDN(),
		"Domain function level",
		"Forest function level")
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

// TestSignedNTPWiring asserts the MS-SNTP wiring the image claims: samba
// creates and serves a signing socket, chrony is configured against that
// exact directory (the two ends are configured in different files, so
// asserting they agree is the whole point), and a client on the domain
// network gets a usable time measurement out of the DC.
//
// This is the acceptance test named by the chrony-as-root ruling (B.6): a
// chronyd that could not reach the signing socket, or that refused to serve
// its undisciplined clock, fails here.
func TestSignedNTPWiring(t *testing.T) {
	f := provisionedDC(t)
	const signdDir = "/var/lib/samba/ntp_signd"

	// The socket file name is samba's business; what the contract needs is
	// a socket in that directory.
	listing := harness.Exec(t, f.DC.Name, "sh", "-c", "ls -l "+signdDir)
	var sockets int
	for _, line := range strings.Split(listing, "\n") {
		if strings.HasPrefix(line, "s") {
			sockets++
		}
	}
	if sockets == 0 {
		t.Fatalf("no socket in %s; samba is not serving MS-SNTP signing requests:\n%s",
			signdDir, listing)
	}

	// samba's effective configuration, read through its own parser.
	got := strings.TrimSpace(harness.Exec(t, f.DC.Name, "sh", "-c",
		`testparm -s --parameter-name="ntp signd socket directory" 2>/dev/null`))
	if got != signdDir {
		t.Fatalf("smb.conf `ntp signd socket directory` = %q, want %q", got, signdDir)
	}

	// ...and chrony's, which must name the same directory or the signing
	// hand-off silently does nothing.
	chronyConf := harness.Exec(t, f.DC.Name, "sh", "-c",
		"grep '^ntpsigndsocket' /etc/chrony/chrony.conf")
	if strings.TrimSpace(chronyConf) != "ntpsigndsocket "+signdDir {
		t.Fatalf("chrony.conf ntpsigndsocket = %q, want %q",
			strings.TrimSpace(chronyConf), signdDir)
	}

	// And the service answers: a one-shot query (never disciplining the
	// client's clock) that produces a measurement.
	out := harness.Client(t, f.Net, clientEnv(f), "sh", "-c",
		`timeout 60 chronyd -Q -t 30 "server `+f.DC.IP+` iburst"`)
	mustContain(t, "chronyd -Q against the DC", out, "System clock wrong by")
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
