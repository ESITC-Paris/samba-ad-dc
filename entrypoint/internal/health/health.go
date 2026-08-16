// Package health answers one question for the container's HEALTHCHECK: is
// this domain controller actually serving?
//
// A process that is running proves nothing — samba can be up while its DNS
// server never answered, while LDAP is not listening, or while no share is
// exported. The check therefore exercises the three protocols a domain member
// needs, in the order a member uses them: DNS to find the DC, LDAP to read
// the directory, SMB to reach SYSVOL. It passes only when all three answer.
package health

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/run"
)

// The probes talk to the loopback address only: the health check asks whether
// this container serves, never whether the network around it works.
const (
	dnsAddr     = "127.0.0.1:53"
	ldapURL     = "ldap://127.0.0.1:389"
	smbHost     = "127.0.0.1"
	smbListFlag = "-L"
)

// defaultSMBConf is where samba-tool writes the configuration this package
// reads the realm from.
const defaultSMBConf = "/etc/samba/smb.conf"

// probeTimeout bounds a probe when the caller set no deadline of its own.
// When the caller did set one — the HEALTHCHECK gives the whole command 10 s
// (Task 6) — every probe runs under that one deadline, so the time an earlier
// probe spends comes out of the same budget rather than extending the total.
const probeTimeout = 3 * time.Second

// minProbeTimeout keeps a probe from being handed a zero or negative
// deadline when the caller's budget is already spent: a non-positive dialer
// timeout means "no timeout" to net.Dialer, which would hang the health
// check instead of failing it.
const minProbeTimeout = 100 * time.Millisecond

// Prober runs the three probes. DNSProbe and LDAPProbe are fields so unit
// tests can exercise the orchestration — the order, the short-circuit and the
// messages — without a running DC; the real implementations are covered by
// the Phase 3 end-to-end health test, which is the only place a real domain
// controller exists.
type Prober struct {
	Runner      run.Runner
	SMBConfPath string
	Smbclient   string
	DNSProbe    func(ctx context.Context, realm string) error
	LDAPProbe   func(ctx context.Context) error
}

// New returns a Prober wired with the real network probes.
func New(r run.Runner, smbConfPath string) *Prober {
	if smbConfPath == "" {
		smbConfPath = defaultSMBConf
	}
	return &Prober{
		Runner:      r,
		SMBConfPath: smbConfPath,
		Smbclient:   run.DefaultBinaries().Smbclient,
		DNSProbe:    dnsProbe,
		LDAPProbe:   ldapProbe,
	}
}

// Check probes the running DC and returns nil only when every probe passed.
// This is what the HEALTHCHECK command calls.
func Check(ctx context.Context, r run.Runner, smbConfPath string) error {
	return New(r, smbConfPath).Check(ctx)
}

// Check runs the probes in dependency order and stops at the first failure:
// once DNS is silent, an LDAP error adds noise, not information. The returned
// error names the failing probe, what it asked for and what it got.
func (p *Prober) Check(ctx context.Context) error {
	data, err := os.ReadFile(p.SMBConfPath)
	if err != nil {
		return fmt.Errorf("the samba configuration %q cannot be read (%s); the domain controller has not been initialized yet, or /etc/samba is not readable",
			p.SMBConfPath, oneLine(err.Error()))
	}
	realm, err := parseRealm(string(data))
	if err != nil {
		return fmt.Errorf("%s in %q; the file was not written by samba-tool, or the volume holds a foreign configuration",
			err, p.SMBConfPath)
	}

	if err := p.DNSProbe(ctx, realm); err != nil {
		return fmt.Errorf("DNS probe failed: the SRV record _ldap._tcp.%s is not answered on %s (%s); samba's internal DNS server is not serving yet — check the samba log for its startup",
			realm, dnsAddr, oneLine(err.Error()))
	}
	if err := p.LDAPProbe(ctx); err != nil {
		return fmt.Errorf("LDAP probe failed: the rootDSE cannot be read on %s (%s); the directory is not accepting queries — check the samba log",
			ldapURL, oneLine(err.Error()))
	}
	if err := p.smbProbe(ctx); err != nil {
		return fmt.Errorf("SMB probe failed: shares cannot be listed on %s (%s); the file server is not exporting SYSVOL and NETLOGON — check the samba log",
			smbHost, oneLine(err.Error()))
	}
	return nil
}

// smbProbe lists the shares this DC exports. It is bounded like the other
// two probes: a hung smbclient must fail the health check, not outlive it.
func (p *Prober) smbProbe(ctx context.Context) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return p.Runner.Run(ctx, p.Smbclient, smbListFlag, smbHost, "-N")
}

// parseRealm extracts the realm from a smb.conf. It is a small hand-rolled
// parser rather than a full ini reader on purpose: the only thing needed is
// the one key samba-tool always writes, and a dependency-free parser cannot
// disagree with samba about the rest of the file.
func parseRealm(conf string) (string, error) {
	for _, line := range strings.Split(conf, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
			continue
		}
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		if !strings.EqualFold(strings.Join(strings.Fields(k), " "), "realm") {
			continue
		}
		if realm := strings.TrimSpace(v); realm != "" {
			return realm, nil
		}
		return "", fmt.Errorf("the realm setting is empty")
	}
	return "", fmt.Errorf("no realm setting was found")
}

// dnsProbe asks samba's own DNS server for the SRV record every domain member
// uses to find a DC. The resolver is pinned to the loopback DNS port, so the
// answer can only come from this container.
func dnsProbe(ctx context.Context, realm string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, network, dnsAddr)
		},
	}
	_, records, err := resolver.LookupSRV(ctx, "ldap", "tcp", realm)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return fmt.Errorf("the lookup succeeded but returned no records")
	}
	return nil
}

// ldapProbe reads the rootDSE anonymously: it proves the directory answers
// without needing a credential inside the health check, which would mean
// mounting a secret for the sole purpose of being healthy.
func ldapProbe(ctx context.Context) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	timeout := remaining(ctx)
	conn, err := ldap.DialURL(ldapURL, ldap.DialWithDialer(&net.Dialer{Timeout: timeout}))
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetTimeout(timeout)

	// The server-side time limit is whole seconds, and 0 means "no limit"
	// to an LDAP server: a sub-second budget must round up to 1, never
	// truncate to unlimited.
	req := ldap.NewSearchRequest(
		"", ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		1, max(1, int(timeout.Seconds())), false,
		"(objectClass=*)", []string{"defaultNamingContext"}, nil,
	)
	res, err := conn.Search(req)
	if err != nil {
		return err
	}
	if len(res.Entries) == 0 {
		return fmt.Errorf("the rootDSE search returned no entry")
	}
	return nil
}

// withTimeout bounds a probe that the caller left unbounded.
func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, probeTimeout)
}

// remaining is how long a probe may take, never zero or negative: a
// non-positive timeout disables the timeout in net.Dialer, which would turn
// an exhausted budget into an unbounded wait.
func remaining(ctx context.Context) time.Duration {
	timeout := probeTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	if timeout < minProbeTimeout {
		return minProbeTimeout
	}
	return timeout
}

// oneLine collapses an error so a health message stays one line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
