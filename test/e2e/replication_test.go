package e2e

// The multi-DC row of the B.5 matrix: an additional domain controller
// joins an existing domain and the two replicate BOTH ways.
//
// Every test function name here is a stable traceability ID (see the
// package comment in main_test.go) and MUST NOT be renamed.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/esitc-paris/samba-ad-dc/test/e2e/harness"
)

// Names are fixed rather than generated because a DC's container name is
// also its host name in the directory and its A record in the realm's DNS
// zone: `repl-dc2` is what `samba-tool drs showrepl` prints as the
// replication partner. They are unique within the package for the reason
// documented on harness.UniqueName — `docker run --name` is one flat
// namespace and two tests sharing a name would fight over one container.
const (
	replPrimary = "repl-dc1"
	replJoiner  = "repl-dc2"
)

// joinTimeout is the budget for the joining DC to reach healthy. A join
// replicates every naming context of the domain over DRS before the
// entrypoint even starts samba, which on a cold volume takes minutes —
// materially longer than a provision — so this is deliberately above the
// harness's own HealthTimeout.
const joinTimeout = 8 * time.Minute

// Replication budgets. The two directions are NOT symmetric, and giving
// them one number would either make the fast one useless or the slow one
// flaky:
//
//   - outbound (dc1 -> dc2) rides the connection the join itself created,
//     so the change is pulled within seconds. 120 s is the plan's budget
//     and is pure slack for a loaded runner.
//   - inbound (dc2 -> dc1) has to wait for dc1's KCC to build the reverse
//     connection object and run its first pull over it; only after that
//     does dc2 hold dc1 in its notify list and later changes arrive in
//     seconds. Measured at ~3 min on an idle laptop, so the budget is
//     twice that. Shortening it does not make the test faster — it makes
//     it red.
const (
	outboundReplicationTimeout = 120 * time.Second
	inboundReplicationTimeout  = 6 * time.Minute
)

// worstCaseRuntime is what this test can consume if every budget above is
// spent: a provision, an 8-minute join and the two replication waits. It is
// what requireDeadline demands, and the reason the package doc insists on
// `-timeout 45m` for the suite — go test's 10-minute default cannot even
// hold this one test, and blowing it panics the binary past every cleanup.
const worstCaseRuntime = joinTimeout + outboundReplicationTimeout + inboundReplicationTimeout +
	harness.HealthTimeout

// dockerEmbeddedResolver is the address docker publishes its own DNS
// resolver on inside every container attached to a user-defined network.
// It is what the DC's internal DNS forwards to; see the comment in the
// test body for why that address in particular.
const dockerEmbeddedResolver = "127.0.0.11"

// requireDeadline fails BEFORE starting anything when the time left in the
// test binary cannot cover what this test is about to do.
//
// Failing early is the whole point. Started under the default 10-minute
// binary timeout, this test gets most of the way through a join and is then
// killed by a panic — which reads like a product defect, points at whatever
// happened to be running, and (because a panicking binary runs no
// t.Cleanup) leaves the containers and volumes behind. A Fatal here names
// the real cause and the fix. It is deliberately not a Skip: a silently
// skipped test is a B.5 matrix row that stopped being covered without
// anyone noticing.
func requireDeadline(t *testing.T, need time.Duration) {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		return // `-timeout 0`: no deadline at all, nothing to check
	}
	if left := time.Until(deadline); left < need {
		t.Fatalf("this test needs up to %s and only %s of the test binary's timeout is left; "+
			"rerun with a longer one, e.g. go test ./... -timeout 45m "+
			"(the default 10m cannot hold a DC join plus replication convergence)",
			need.Round(time.Second), left.Round(time.Second))
	}
}

// TestJoinReplicationBothWays asserts the property that makes a second DC
// worth running: a container started in join mode becomes a full replica
// of the domain the first one provisioned, and directory changes then flow
// in BOTH directions.
//
// The two DCs are built from the Go harness rather than a compose file on
// purpose: every container the suite starts must carry the harness
// ownership label and the constrained profile (read-only rootfs, tmpfs,
// cap-drop ALL + the capability set under test), and a compose file would
// be a second, silently diverging copy of that profile.
//
// What is asserted, in order:
//   - both DCs reach the image's own health verdict;
//   - the joining DC says it joined;
//   - a user created on dc1 appears on dc2 (bounded poll);
//   - a user created on dc2 appears on dc1 (bounded poll);
//   - `samba-tool drs showrepl` reports partners with zero consecutive
//     failures on BOTH DCs — a directory can serve stale reads happily, so
//     the propagation checks above do not by themselves prove the
//     replication links are healthy in both directions;
//   - both DCs report the SAME functional levels, and the domain's lowest
//     DC level has not been dragged down by the join (Phase 2 hand-off).
func TestJoinReplicationBothWays(t *testing.T) {
	requireDeadline(t, worstCaseRuntime)
	net := harness.Network(t)

	// The first DC resolves through ITSELF and forwards what it is not
	// authoritative for to docker's embedded resolver. Both halves are
	// load-bearing, and this is the one piece of wiring in the suite that
	// deserves a paragraph rather than a line.
	//
	// Why the DC must be its own resolver: a replication partner is
	// addressed as `<objectGUID>._msdcs.<realm>`, a CNAME that lives in the
	// directory's own DNS zone and that only samba can answer. Left on
	// docker's embedded resolver, dc1 provisions fine and serves clients
	// fine — and can never pull a change FROM dc2, because it cannot
	// resolve dc2 at all.
	//
	// Why the forwarder is not optional: samba's internal DNS has no
	// upstream by default, and a query it is not authoritative for then
	// takes 4-8 SECONDS to fail instead of milliseconds (measured). That is
	// invisible for ordinary lookups and fatal for Kerberos: the image
	// ships no /etc/krb5.conf, so Heimdal discovers the realm by walking
	// `_kerberos.` up the parent domains — several non-authoritative
	// queries per bind — and the Kerberos-sealed DRSUAPI bind that carries
	// replication times out before they finish. With a forwarder, the same
	// queries answer instantly and replication works.
	//
	// Why resolv.conf is bind-mounted instead of `docker run --dns`: on a
	// user-defined network docker ALWAYS puts its own resolver in
	// resolv.conf and treats --dns as that resolver's upstream. `--dns
	// <the DC>` would therefore make the embedded resolver forward to
	// samba while samba forwards back to the embedded resolver — a loop
	// that hangs exactly like having no forwarder. Writing resolv.conf
	// ourselves points the DC's own lookups straight at samba on
	// 127.0.0.1:53 (where the image's health probe already queries it) and
	// leaves the embedded resolver's upstream untouched, so the forwarding
	// chain terminates at the host.
	dc1 := harness.StartDC(t, net, replPrimary, "provision", map[string]string{
		"SAMBA_REALM":         harness.Realm,
		"SAMBA_DOMAIN":        harness.Domain,
		"SAMBA_DNS_FORWARDER": dockerEmbeddedResolver,
	}, harness.AdminSecret(t),
		harness.WithBind(selfResolvConf(t), "/etc/resolv.conf", true))
	harness.WaitHealthy(t, dc1.Name, harness.HealthTimeout)

	// The joining container resolves through dc1 — the same thing an
	// operator does when adding a DC to an existing domain, and a
	// requirement rather than a convenience: the realm's SRV records are
	// how the join finds the DC to join. dc2 needs no forwarder of its own
	// because every lookup it makes is answered by dc1, which has one.
	dc2 := harness.StartDC(t, net, replJoiner, "join", map[string]string{
		"SAMBA_REALM": harness.Realm,
	}, harness.JoinSecret(t), harness.WithDNS(dc1.IP))
	harness.WaitHealthy(t, dc2.Name, joinTimeout)

	mustContain(t, "joining container logs", harness.Logs(t, dc2.Name),
		"joining realm "+harness.Realm+" as a domain controller with account Administrator",
		"joined realm "+harness.Realm)

	// Both writes are made before either is waited for: they travel over
	// independent links, and overlapping the two waits keeps the test from
	// paying for the slow direction twice.
	//
	// --random-password keeps the new accounts' passwords off the command
	// line entirely: nothing here needs to know them, and the harness
	// echoes failing command lines into the test log.
	harness.Exec(t, dc1.Name, "samba-tool", "user", "create", "alice", "--random-password")
	harness.Exec(t, dc2.Name, "samba-tool", "user", "create", "bob", "--random-password")

	waitForUser(t, dc2.Name, "alice", outboundReplicationTimeout) // dc1 -> dc2
	waitForUser(t, dc1.Name, "bob", inboundReplicationTimeout)    // dc2 -> dc1

	// --- the replication links themselves --------------------------------
	//
	// Each DC must name the other as a partner: "the container is healthy"
	// would also be true of a DC that joined nothing, and a link that only
	// exists one way is the classic half-broken multi-DC domain.
	assertReplicationHealthy(t, dc1.Name, replJoiner)
	assertReplicationHealthy(t, dc2.Name, replPrimary)

	// --- functional-level parity (Phase 2 hand-off) ----------------------
	assertFunctionalLevelParity(t, dc1.Name, dc2.Name)
}

// selfResolvConf writes the resolv.conf a domain controller gets and
// returns its host path, ready to be bind-mounted over /etc/resolv.conf.
//
// 0644 for the same reason harness.Secret uses it: the container's
// capability set is under test and must not need DAC_OVERRIDE to read a
// file the harness mounted.
func selfResolvConf(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte("nameserver 127.0.0.1\n"), 0o644); err != nil { //nolint:gosec // test fixture, see doc comment
		t.Fatalf("writing the DC's resolv.conf: %v", err)
	}
	return path
}

// waitForUser polls until a user account is readable on a DC, which is how
// a test observes that an object replicated to it. Bounded and polled — a
// fixed sleep would either be a flake on a slow runner or dead time on a
// fast one.
func waitForUser(t *testing.T, container, user string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		code, out := harness.ExecErr(t, container, "samba-tool", "user", "show", user)
		if code == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("user %q did not replicate to %s within %s; the directory there still answers:\n%s\n"+
				"--- drs showrepl on %s ---\n%s",
				user, container, within, strings.TrimSpace(out),
				container, showrepl(t, container))
		}
		time.Sleep(3 * time.Second)
	}
}

// showrepl returns `samba-tool drs showrepl` output without asserting on
// it, for use in failure messages.
func showrepl(t *testing.T, container string) string {
	t.Helper()
	_, out := harness.ExecErr(t, container, "samba-tool", "drs", "showrepl")
	return out
}

// consecutiveFailures matches the per-partner failure counter samba prints
// under each neighbour, e.g. "\t\t0 consecutive failure(s).".
var consecutiveFailures = regexp.MustCompile(`(?m)^\s*(\d+) consecutive failure`)

// sectionHeader matches the banners `samba-tool drs showrepl` divides its
// report with, e.g. "==== INBOUND NEIGHBORS ====".
var sectionHeader = regexp.MustCompile(`(?m)^====\s*(.+?)\s*====\s*$`)

// The two sections this test reads. INBOUND is what this DC pulls FROM its
// partners; OUTBOUND is the notify list — the partners it tells about its
// own changes.
const (
	inboundSection  = "INBOUND NEIGHBORS"
	outboundSection = "OUTBOUND NEIGHBORS"
)

// showreplSections splits a showrepl report into its banner-delimited
// sections, keyed by banner text. Reading a counter without knowing which
// section it came from is what makes a failure say "replication is broken"
// instead of "this DC cannot pull from that one".
func showreplSections(out string) map[string]string {
	locs := sectionHeader.FindAllStringSubmatchIndex(out, -1)
	sections := make(map[string]string, len(locs))
	for i, loc := range locs {
		end := len(out)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		sections[strings.ToUpper(out[loc[2]:loc[3]])] = out[loc[1]:end]
	}
	return sections
}

// assertReplicationHealthy asserts that a DC's replication links are not
// merely present but working: it pulls from wantPartner, and no partner in
// either direction has a non-zero failure counter.
//
// Counting the neighbours matters as much as reading the counters: a DC
// with no partners at all reports no failures either, and would sail
// through a check that only looked for the word "failure". That count is
// required of the INBOUND section only — an empty OUTBOUND list is a normal
// transient state, because a DC appears in its partner's notify list only
// once that partner has registered itself there (observed on a freshly
// joined DC), while an empty INBOUND list means this DC can never learn
// anything from the domain.
func assertReplicationHealthy(t *testing.T, container, wantPartner string) {
	t.Helper()
	out := harness.Exec(t, container, "samba-tool", "drs", "showrepl")
	sections := showreplSections(out)
	partner := strings.ToUpper(wantPartner)

	for _, name := range []string{inboundSection, outboundSection} {
		body, ok := sections[name]
		if !ok {
			t.Fatalf("drs showrepl on %s has no %s section:\n%s", container, name, out)
		}
		// The counter — not the presence of the word "failed" — is the
		// verdict: samba keeps printing the last failed attempt of a partner
		// that has since recovered, and a link that recovered is a working
		// link. A counter above zero means it has not.
		for _, m := range consecutiveFailures.FindAllStringSubmatch(body, -1) {
			n, err := strconv.Atoi(m[1])
			if err != nil || n != 0 {
				t.Fatalf("drs showrepl on %s reports %q under %s; "+
					"replication is failing in that direction:\n%s",
					container, strings.TrimSpace(m[0]), name, out)
			}
		}
	}

	inbound := sections[inboundSection]
	// samba prints a DC as "<site>\<NETBIOS NAME>", upper-cased.
	if !strings.Contains(inbound, `\`+partner) {
		t.Fatalf("drs showrepl on %s lists no inbound connection from %s; "+
			"it cannot pull changes made there:\n%s", container, partner, out)
	}
	if len(consecutiveFailures.FindAllString(inbound, -1)) == 0 {
		t.Fatalf("drs showrepl on %s reports no inbound replication partner at all; "+
			"the DC is isolated:\n%s", container, out)
	}
}

// levelLine matches one line of `samba-tool domain level show`, e.g.
// "Domain function level: (Windows) 2016".
var levelLine = regexp.MustCompile(`(?mi)^([^:]*function level[^:]*):\s*(.+)$`)

// domainLevels reads the functional levels a DC reports, keyed by the
// label samba prints them under.
func domainLevels(t *testing.T, container string) map[string]string {
	t.Helper()
	out := harness.Exec(t, container, "samba-tool", "domain", "level", "show")
	levels := map[string]string{}
	for _, m := range levelLine.FindAllStringSubmatch(out, -1) {
		levels[strings.TrimSpace(m[1])] = strings.TrimSpace(m[2])
	}
	if len(levels) == 0 {
		t.Fatalf("samba-tool domain level show on %s reported no functional level:\n%s",
			container, out)
	}
	return levels
}

// assertFunctionalLevelParity resolves the Phase 2 hand-off: a joined DC
// must land at the same functional level as the DC it joined.
//
// Two independent halves, because either one alone can be satisfied by a
// broken domain:
//
//   - both DCs must report the same `samba-tool domain level show` output.
//     Those values live in the directory and replicate, so disagreement
//     means the two are not really one domain.
//   - the `ad dc functional level` each DC advertises for ITSELF must
//     match. That one comes from smb.conf, is NOT replicated, and is what
//     provision mirrors from SAMBA_FUNCTION_LEVEL. A join that did not
//     mirror it leaves dc2 advertising samba's 2008_R2 default inside a
//     2016 domain — which both DCs then report identically as a lowered
//     "lowest function level of a DC", so the first half cannot see it.
func assertFunctionalLevelParity(t *testing.T, primary, joiner string) {
	t.Helper()

	p, j := domainLevels(t, primary), domainLevels(t, joiner)
	for _, key := range sortedLevelKeys(p, j) {
		if p[key] != j[key] {
			t.Fatalf("functional level %q: %s reports %q, %s reports %q; "+
				"the joined DC is not at the same level as the domain it joined",
				key, primary, p[key], joiner, j[key])
		}
	}

	pl, jl := dcFunctionalLevel(t, primary), dcFunctionalLevel(t, joiner)
	if pl != jl {
		t.Fatalf("smb.conf `ad dc functional level`: %s = %q, %s = %q; "+
			"the join did not mirror SAMBA_FUNCTION_LEVEL onto the joined DC, "+
			"so it advertises a lower level than the domain it belongs to",
			primary, pl, joiner, jl)
	}
}

// dcFunctionalLevel reads the level a DC advertises for itself, through
// samba's own configuration parser so that a defaulted value is reported
// exactly as samba sees it.
func dcFunctionalLevel(t *testing.T, container string) string {
	t.Helper()
	out := harness.Exec(t, container, "sh", "-c",
		`testparm -s --parameter-name="ad dc functional level" 2>/dev/null`)
	return strings.TrimSpace(out)
}

// sortedLevelKeys returns the union of two level maps' keys, in a stable
// order so a failure always names the same level first.
func sortedLevelKeys(maps ...map[string]string) []string {
	seen := map[string]bool{}
	var keys []string
	for _, m := range maps {
		for k := range m {
			if !seen[k] {
				seen[k], keys = true, append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}
