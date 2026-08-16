package health

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestRemainingNeverReturnsANonPositiveTimeout(t *testing.T) {
	t.Run("no deadline falls back to the probe timeout", func(t *testing.T) {
		if got := remaining(context.Background()); got != probeTimeout {
			t.Errorf("remaining = %s, want %s", got, probeTimeout)
		}
	})

	t.Run("an exhausted budget is clamped to a positive floor", func(t *testing.T) {
		// A zero or negative timeout means "no timeout" to net.Dialer: an
		// exhausted budget must fail the probe, never unbound it.
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		if got := remaining(ctx); got != minProbeTimeout {
			t.Errorf("remaining = %s, want the %s floor", got, minProbeTimeout)
		}
	})

	t.Run("a live deadline is used as is", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		got := remaining(ctx)
		if got <= minProbeTimeout || got > time.Second {
			t.Errorf("remaining = %s, want roughly the caller's second", got)
		}
	})
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
// main calls for the HEALTHCHECK subcommand. It stays hermetic: the
// configuration is unreadable, which fails before any probe dials anything,
// so the test asserts the delegation without depending on what does or does
// not answer on the loopback address of the machine running it.
func TestCheckFunctionDelegates(t *testing.T) {
	r := &fakeRunner{}
	path := filepath.Join(t.TempDir(), "absent", "smb.conf")

	err := Check(context.Background(), r, path)
	if err == nil {
		t.Fatal("expected an error when the configuration cannot be read")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the configuration file", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("the wrapper probed anyway: %v", r.calls)
	}
}

// TestCheckReportsWhereItProbed pins the addresses in the messages: an
// operator reading a failed HEALTHCHECK must see that the check is about
// this container's loopback, not about the network.
func TestCheckReportsWhereItProbed(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(*Prober, *probeLog, *fakeRunner)
		want    []string
	}{
		{
			name:    "dns",
			arrange: func(_ *Prober, l *probeLog, _ *fakeRunner) { l.dnsErr = errors.New("i/o timeout") },
			want:    []string{"DNS", "127.0.0.1:53", "_ldap._tcp.AD.EXAMPLE.COM"},
		},
		{
			name:    "ldap",
			arrange: func(_ *Prober, l *probeLog, _ *fakeRunner) { l.ldapErr = errors.New("connection refused") },
			want:    []string{"LDAP", "ldap://127.0.0.1:389"},
		},
		{
			name:    "smb",
			arrange: func(_ *Prober, _ *probeLog, r *fakeRunner) { r.err = errors.New("NT_STATUS_CONNECTION_REFUSED") },
			want:    []string{"SMB", "127.0.0.1"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{}
			p, log := testProber(t, r, validConf)
			tc.arrange(p, log, r)

			err := p.Check(context.Background())
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, frag := range tc.want {
				if !strings.Contains(err.Error(), frag) {
					t.Errorf("error %q does not contain %q", err, frag)
				}
			}
		})
	}
}

// TestSMBProbeIsBounded asserts the SMB probe is given a deadline like the
// other two: a hung smbclient must fail the health check, not outlive it.
func TestSMBProbeIsBounded(t *testing.T) {
	r := &deadlineRunner{}
	p, _ := testProber(t, r, validConf)

	if err := p.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !r.hadDeadline {
		t.Error("the SMB probe ran with an unbounded context")
	}
}

// deadlineRunner records whether the context it received carried a deadline.
type deadlineRunner struct {
	hadDeadline bool
}

func (d *deadlineRunner) Run(ctx context.Context, name string, args ...string) error {
	_, d.hadDeadline = ctx.Deadline()
	return nil
}

func (d *deadlineRunner) Start(ctx context.Context, name string, args ...string) (run.Proc, error) {
	return nil, errors.New("the health check never starts daemons")
}
