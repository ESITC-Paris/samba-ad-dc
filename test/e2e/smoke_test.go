package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/esitc-paris/samba-ad-dc/test/e2e/harness"
)

// TestHarnessSmoke is the harness's own test: it exercises every part of
// the harness that the rest of the matrix depends on — network creation,
// a secret file, a DC started under the full constrained profile with
// fresh volumes, the health wait, a samba-tool exec, a clean stop, and
// the test-client image — and asserts protocol/contract-level facts
// rather than "the container started" (§8.2).
func TestHarnessSmoke(t *testing.T) {
	net := harness.Network(t)

	dc := harness.StartDC(t, net, "dc1", "provision", map[string]string{
		"SAMBA_REALM":  harness.Realm,
		"SAMBA_DOMAIN": harness.Domain,
	}, harness.AdminSecret(t))

	harness.WaitHealthy(t, dc.Name, harness.HealthTimeout)

	// The DC really provisioned the realm the harness asked for: read it
	// back through samba's own CLI, which names it as a naming context.
	out := harness.Exec(t, dc.Name, "samba-tool", "domain", "level", "show")
	if !strings.Contains(out, harness.BaseDN()) {
		t.Fatalf("samba-tool domain level show does not name the domain %s:\n%s",
			harness.BaseDN(), out)
	}

	// The test-client image is usable from the same network: these are
	// the protocol clients every later task drives the DC with.
	ver := harness.Client(t, net, nil, "sh", "-c", "dig -v 2>&1; smbclient --version")
	for _, want := range []string{"DiG", "Version"} {
		if !strings.Contains(ver, want) {
			t.Fatalf("test-client tooling missing %q in:\n%s", want, ver)
		}
	}

	// A container asked to stop exits 0 (Runtime contract, §6.3).
	code := harness.Stop(t, dc.Name, 15*time.Second)
	if code != 0 {
		t.Fatalf("clean stop: container exit code = %d, want 0", code)
	}
}
