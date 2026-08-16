package health

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/run"
)

// ---------------------------------------------------------------------------
// fake runner
// ---------------------------------------------------------------------------

// fakeRunner records the smbclient invocation and returns a scripted result.
type fakeRunner struct {
	calls [][]string
	err   error
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	return f.err
}

func (f *fakeRunner) Start(ctx context.Context, name string, args ...string) (run.Proc, error) {
	f.calls = append(f.calls, append([]string{"start:" + name}, args...))
	return nil, errors.New("the health check never starts daemons")
}

// smbConf writes a smb.conf holding body and returns its path.
func smbConf(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "smb.conf")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// testProber wires a prober whose network probes are recorded stubs.
func testProber(t *testing.T, r run.Runner, conf string) (*Prober, *probeLog) {
	t.Helper()
	log := &probeLog{}
	p := New(r, smbConf(t, conf))
	p.DNSProbe = func(ctx context.Context, realm string) error {
		log.dns = append(log.dns, realm)
		return log.dnsErr
	}
	p.LDAPProbe = func(ctx context.Context) error {
		log.ldap++
		return log.ldapErr
	}
	return p, log
}

// probeLog records what the stubbed probes were asked to do.
type probeLog struct {
	dns     []string
	ldap    int
	dnsErr  error
	ldapErr error
}

const validConf = "[global]\n\trealm = AD.EXAMPLE.COM\n\tserver role = active directory domain controller\n"

// ---------------------------------------------------------------------------
// realm parsing
// ---------------------------------------------------------------------------

func TestParseRealm(t *testing.T) {
	tests := []struct {
		name    string
		conf    string
		want    string
		wantErr bool
	}{
		{
			name: "normal generated file",
			conf: validConf,
			want: "AD.EXAMPLE.COM",
		},
		{
			name: "extra spaces around the assignment",
			conf: "[global]\n   realm    =    AD.EXAMPLE.COM   \n",
			want: "AD.EXAMPLE.COM",
		},
		{
			name: "no spaces at all",
			conf: "[global]\nrealm=AD.EXAMPLE.COM\n",
			want: "AD.EXAMPLE.COM",
		},
		{
			name: "key case does not matter",
			conf: "[global]\n\tRealm = ad.example.com\n",
			want: "ad.example.com",
		},
		{
			name:    "missing realm is an error",
			conf:    "[global]\n\tworkgroup = AD\n",
			wantErr: true,
		},
		{
			name:    "commented realm does not count",
			conf:    "[global]\n\t# realm = AD.EXAMPLE.COM\n\t; realm = OTHER.EXAMPLE\n",
			wantErr: true,
		},
		{
			name:    "empty value is an error",
			conf:    "[global]\n\trealm =\n",
			wantErr: true,
		},
		{
			name:    "empty file is an error",
			conf:    "",
			wantErr: true,
		},
		{
			name: "a key that merely starts with realm is not the realm",
			conf: "[global]\n\trealmx = WRONG\n\trealm = AD.EXAMPLE.COM\n",
			want: "AD.EXAMPLE.COM",
		},
		{
			name: "the first realm wins",
			conf: "[global]\n\trealm = AD.EXAMPLE.COM\n\trealm = LATER.EXAMPLE\n",
			want: "AD.EXAMPLE.COM",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRealm(tc.conf)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got realm %q", got)
				}
				if !strings.Contains(err.Error(), "realm") {
					t.Errorf("error %q does not name the missing setting", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("parseRealm = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// orchestration
// ---------------------------------------------------------------------------

func TestCheckAllProbesPass(t *testing.T) {
	r := &fakeRunner{}
	p, log := testProber(t, r, validConf)

	if err := p.Check(context.Background()); err != nil {
		t.Fatalf("Check: unexpected error: %v", err)
	}
	if len(log.dns) != 1 || log.dns[0] != "AD.EXAMPLE.COM" {
		t.Errorf("DNS probe asked for %v, want one lookup of AD.EXAMPLE.COM", log.dns)
	}
	if log.ldap != 1 {
		t.Errorf("LDAP probe ran %d times, want 1", log.ldap)
	}
	if len(r.calls) != 1 {
		t.Fatalf("runner calls = %v, want exactly one smbclient call", r.calls)
	}
	want := []string{"smbclient", "-L", "127.0.0.1", "-N"}
	if strings.Join(r.calls[0], " ") != strings.Join(want, " ") {
		t.Errorf("smbclient call = %v, want %v", r.calls[0], want)
	}
}

func TestCheckDNSFailureStopsAtTheFirstProbe(t *testing.T) {
	r := &fakeRunner{}
	p, log := testProber(t, r, validConf)
	log.dnsErr = errors.New("no such host")

	err := p.Check(context.Background())
	if err == nil {
		t.Fatal("expected an error when the DNS probe fails")
	}
	if !strings.Contains(err.Error(), "DNS") {
		t.Errorf("error %q does not name the failing probe", err)
	}
	if !strings.Contains(err.Error(), "no such host") {
		t.Errorf("error %q does not carry the underlying cause", err)
	}
	if !strings.Contains(err.Error(), "AD.EXAMPLE.COM") {
		t.Errorf("error %q does not name what was looked up", err)
	}
	if log.ldap != 0 || len(r.calls) != 0 {
		t.Errorf("later probes ran after the first failure: ldap=%d smb=%v", log.ldap, r.calls)
	}
}

func TestCheckLDAPFailureNamesLDAP(t *testing.T) {
	r := &fakeRunner{}
	p, log := testProber(t, r, validConf)
	log.ldapErr = errors.New("connection refused")

	err := p.Check(context.Background())
	if err == nil {
		t.Fatal("expected an error when the LDAP probe fails")
	}
	if !strings.Contains(err.Error(), "LDAP") {
		t.Errorf("error %q does not name the failing probe", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error %q does not carry the underlying cause", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("the SMB probe ran after LDAP failed: %v", r.calls)
	}
}

func TestCheckSMBFailureNamesSMB(t *testing.T) {
	r := &fakeRunner{err: errors.New("smbclient: exit status 1")}
	p, _ := testProber(t, r, validConf)

	err := p.Check(context.Background())
	if err == nil {
		t.Fatal("expected an error when smbclient fails")
	}
	if !strings.Contains(err.Error(), "SMB") {
		t.Errorf("error %q does not name the failing probe", err)
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("error %q does not carry the underlying cause", err)
	}
}

func TestCheckUnreadableConfigFails(t *testing.T) {
	r := &fakeRunner{}
	p, log := testProber(t, r, validConf)
	p.SMBConfPath = filepath.Join(t.TempDir(), "absent", "smb.conf")

	err := p.Check(context.Background())
	if err == nil {
		t.Fatal("expected an error when smb.conf cannot be read")
	}
	if !strings.Contains(err.Error(), p.SMBConfPath) {
		t.Errorf("error %q does not name the file", err)
	}
	if len(log.dns) != 0 || log.ldap != 0 || len(r.calls) != 0 {
		t.Error("probes ran without a realm")
	}
}

func TestCheckMissingRealmFails(t *testing.T) {
	r := &fakeRunner{}
	p, _ := testProber(t, r, "[global]\n\tworkgroup = AD\n")

	err := p.Check(context.Background())
	if err == nil {
		t.Fatal("expected an error when smb.conf carries no realm")
	}
	if !strings.Contains(err.Error(), p.SMBConfPath) {
		t.Errorf("error %q does not name the file the realm was expected in", err)
	}
}

func TestCheckUsesTheConfiguredSmbclientBinary(t *testing.T) {
	r := &fakeRunner{}
	p, _ := testProber(t, r, validConf)
	p.Smbclient = "/opt/samba/bin/smbclient"

	if err := p.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if r.calls[0][0] != "/opt/samba/bin/smbclient" {
		t.Errorf("smbclient binary = %q, want the configured one", r.calls[0][0])
	}
}

// TestNewInstallsRealProbes guards the wiring: the package-level Check must
// reach the real network probes, not nil function fields.
func TestNewInstallsRealProbes(t *testing.T) {
	p := New(&fakeRunner{}, "/etc/samba/smb.conf")
	if p.DNSProbe == nil {
		t.Error("New left DNSProbe nil")
	}
	if p.LDAPProbe == nil {
		t.Error("New left LDAPProbe nil")
	}
	if p.Smbclient == "" {
		t.Error("New left the smbclient binary unset")
	}
	if p.SMBConfPath != "/etc/samba/smb.conf" {
		t.Errorf("SMBConfPath = %q", p.SMBConfPath)
	}
}

// TestCheckFunctionDelegates covers the exported convenience wrapper that
// main calls for the HEALTHCHECK subcommand.
func TestCheckFunctionDelegates(t *testing.T) {
	r := &fakeRunner{}
	path := smbConf(t, validConf)
	// The real DNS and LDAP probes cannot reach a DC from a unit test, so
	// this asserts only that the wrapper fails rather than panicking, and
	// that it names the first failing probe.
	err := Check(context.Background(), r, path)
	if err == nil {
		t.Skip("a DC answers on this host; the E2E health test covers the passing path")
	}
	if !strings.Contains(err.Error(), "DNS") && !strings.Contains(err.Error(), "LDAP") {
		t.Errorf("error %q names no probe", err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("error %q does not say where it probed", err)
	}
}
