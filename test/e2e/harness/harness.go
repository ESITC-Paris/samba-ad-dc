// Package harness drives the samba-ad-dc image through the docker CLI.
//
// The CLI — not the Docker SDK — is the interface used deliberately: it is
// the stable, always-present surface on every CI runner, and what the
// operator documentation tells people to type. Every DC container this
// package starts — the running DCs and the short-lived `samba-tool`
// one-offs that go through the same startDC — runs the *constrained
// profile* of the adaptation profile (SPEC Annex B.2): read-only rootfs,
// tmpfs for the writable runtime paths, `--cap-drop ALL` plus the
// minimal capability set, and `--security-opt no-new-privileges:true`.
// Nothing here ever uses `--privileged` (§5.2), and the profile is
// therefore proven on every single E2E run rather than asserted in
// prose.
//
// The one container deliberately OUTSIDE that profile is the test client
// (client.go), which runs unconstrained. It is not the product: it plays
// a domain member talking to the DC from the network, and constraining
// it would prove something about the throwaway client image instead of
// about this image.
package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The domain under test. Realm and domain are throwaway names in the
// reserved-by-convention `.test` TLD (RFC 6761) so nothing here can
// collide with, or leak into, a real directory.
const (
	// Realm is the Kerberos realm / AD DNS domain the suite provisions.
	Realm = "AD.E2E.TEST"
	// Domain is the NetBIOS domain name.
	Domain = "E2E"
	// AdminPassword is a THROWAWAY test literal, deliberately in the
	// repository: it only ever protects a container that is destroyed at
	// the end of the test that created it. It never reaches an
	// environment variable of a DC container — the harness writes it to a
	// file and mounts it, because the image accepts secrets through
	// `*_FILE` variables only (§6.1).
	AdminPassword = "E2ePassw0rd!"
)

// DefaultImage is the image under test unless E2E_IMAGE overrides it.
const DefaultImage = "samba-ad-dc:dev"

// OwnerLabel marks every container, volume and network this package
// creates, and OwnerLabelValue is the value it carries.
//
// It exists so the harness can never destroy something it did not create.
// The suite works with fixed, meaningful container names (`dc1`,
// `nominal-dc1`), and a leftover from an interrupted run has to be
// force-removed before the name can be reused — but `dc1` is also a name
// a developer might have given their own container on the same machine.
// Every removal path therefore inspects this label first and refuses to
// touch anything that does not carry it.
const (
	OwnerLabel      = "e2e.harness"
	OwnerLabelValue = "1"
)

// ownerLabelArg is the `--label` argument every create path passes.
const ownerLabelArg = OwnerLabel + "=" + OwnerLabelValue

// Timeouts. Generous on purpose: a first-boot provision on a cold volume
// legitimately takes minutes, and CI runners are slower than a laptop.
const (
	// HealthTimeout is the default budget for reaching `healthy`. The
	// image's healthcheck has a 180 s start period, so anything shorter
	// than that would only ever measure the start period.
	HealthTimeout = 5 * time.Minute
	// HealthTransitionTimeout is the budget for a container that should
	// become healthy on an already-initialized volume — a restart, an
	// upgrade, a restored backup — where no provision has to happen first.
	//
	// It MUST stay above the image's healthcheck `--start-period` (180 s)
	// and is 4 minutes for that reason. Docker never reports `unhealthy`
	// while a container is inside its start period: it keeps saying
	// `starting`. A budget at or below 180 s can therefore never observe
	// a health verdict at all — it can only ever expire mid-start-period
	// and report "did not become healthy in time", which measures the
	// start period rather than the container. Shorten this and the tests
	// that use it stop testing anything.
	HealthTransitionTimeout = 4 * time.Minute
	// ExitTimeout is how long RunDCExpectExit waits for a container that
	// is expected to terminate on its own.
	ExitTimeout = 5 * time.Minute
	// ExecTimeout bounds a single `docker exec`.
	ExecTimeout = 2 * time.Minute
	// PullTimeout is how long EnsureImage may spend fetching a reference
	// image from a registry. It is exported because a test that calls
	// EnsureImage must count it into the deadline it demands of the test
	// binary: a pull that runs for its whole budget is time the test then
	// no longer has for the domain controller it was about to start.
	PullTimeout = 10 * time.Minute
	// BuildTimeout is how long Build may spend on a `docker build`. It is
	// exported for the same reason PullTimeout is: a test that builds an
	// image has to count that build into the deadline it demands of the
	// test binary — and that demand is what keeps the suite from being
	// killed mid-run by `go test`'s own timeout.
	//
	// Three minutes is already ~30x what the one thing the suite builds
	// costs: the derived image of the B.5 reuse row is a single COPY and
	// a single ENV on top of a base that is already in the local store,
	// with a build context of two small files. What varies is not the
	// layers but the daemon — tarring the context up, and on a loaded
	// runner waiting for a builder at all — so the margin is over that,
	// not over the work. It is deliberately NOT as generous as
	// PullTimeout: nothing here goes to a registry, and a budget large
	// enough to hide a hung daemon would be spent by the test that has to
	// declare it up front.
	BuildTimeout = 3 * time.Minute
	// dockerTimeout bounds the short bookkeeping commands (inspect, rm,
	// volume create) that should answer immediately or not at all.
	dockerTimeout = 60 * time.Second
)

// DefaultCaps is THE capability set under test — the single place the
// whole suite reads it from, and the set adaptation-profile B.2
// documents. It is no longer a hypothesis: every entry below was
// MEASURED by test/capbisect/bisect.sh, which re-runs the smoke subset
// once per capability with that capability taken away. Reports:
// test/capbisect/results-amd64.txt and results-arm64.txt, both from the
// CI bisection of 2026-09-16 and both ending "=> AGREE" against this
// list; results-arm64-2026-08-16.txt is the local run that first
// established it, kept because it is the only one that also measured
// DAC_OVERRIDE.
//
// Five of the six are required because the image visibly breaks without
// them — the failure each removal produces is quoted in B.2. The sixth,
// NET_BIND_SERVICE, is the one this suite CANNOT measure: docker sets
// net.ipv4.ip_unprivileged_port_start=0 in the network namespace it
// creates, so no port is privileged inside a container on a bridge and
// the suite passes without the capability. It is kept because the
// bisection's separate port probe shows a bind of :389 in this image
// being denied without it once the floor is back at the kernel default
// — which is the floor a container gets under the host networking SPEC
// B.3 supports. Dropping it here would make the suite green and the
// documented deployment broken.
//
// DAC_OVERRIDE was in the pre-bisection hypothesis and is NOT here: the
// suite passes without it, because everything in the container runs as
// uid 0 over paths the entrypoint has already chowned to itself, so
// there is no discretionary check left to override.
//
// E2E_CAPS overrides this list; the two are the only inputs to what a DC
// container gets.
var DefaultCaps = []string{
	"SYS_ADMIN",
	"NET_BIND_SERVICE",
	"CHOWN",
	"FOWNER",
	"SETUID",
	"SETGID",
}

// DefaultTmpfs is the writable-path set mounted as tmpfs on top of the
// read-only rootfs, matching adaptation-profile B.2. `/var/lib/samba` and
// `/etc/samba` are persistent volumes instead and are handled separately.
//
// `/var/cache/samba` is here because winbindd opens its netsamlogon cache
// there on every boot and a read-only rootfs turns that into three lines
// of error on every single start (evidence: task-1 report). It holds a
// pure cache — nothing there needs to survive a restart — so a tmpfs is
// the correct answer rather than a volume.
var DefaultTmpfs = []string{"/run", "/tmp", "/var/cache/samba"}

// DefaultSecurityOpt is the `--security-opt` set every DC container gets.
//
// `no-new-privileges:true` is in every deployment example the guides
// publish (deployment-guide §1.8, README quickstart), and
// docs/traceability.md states that the suite runs under it. It is here so
// that statement is true: tested is documented, and a setting only the
// documentation carries is a setting nobody has ever run.
//
// Unlike DefaultCaps it is NOT a measured minimum — nothing in the image
// escalates privilege at exec time, so the suite would be just as green
// without it. What the flag buys is the guarantee that it stays costless:
// if a future change to the image ever needed a setuid helper, the suite
// goes red here instead of an operator's production DC going red on the
// profile the guides told them to use.
var DefaultSecurityOpt = []string{"no-new-privileges:true"}

// Image returns the DC image under test.
func Image() string {
	if v := strings.TrimSpace(os.Getenv("E2E_IMAGE")); v != "" {
		return v
	}
	return DefaultImage
}

// Caps returns the capability set every DC container is started with.
//
// E2E_CAPS overrides it with a comma-separated list; an E2E_CAPS that is
// set but empty means *no* capabilities beyond the dropped-all baseline,
// which is what the bisection driver needs to express.
func Caps() []string {
	v, ok := os.LookupEnv("E2E_CAPS")
	if !ok {
		return append([]string(nil), DefaultCaps...)
	}
	return parseCaps(v)
}

// RestoreCaps returns the capability set for the ONE-OFF container that
// runs `samba-tool domain backup restore`, and whether E2E_RESTORE_CAPS
// named one at all. Unset (ok == false) means "no override": the restore
// container gets exactly what everything else gets.
//
// It exists because of a measured, branch-specific fact and covers
// nothing else. On Samba 4.22.11 the restore's sysvol NT-ACL step fails
// under the B.2 capability set with
//
//	py_smbd_mkdir: mkdirat error=13 (Permission denied)
//
// and `samba-tool domain backup restore` exits 255; the same restore
// succeeds with DAC_OVERRIDE added, and needs no such thing on 4.23.12 or
// 4.24.7 (measured locally, arm64, 2026-09-16). The ruling that follows
// from that is deliberately narrow: the capability set a running DC is
// tested under does NOT change on any branch, because nothing showed that
// it must — what changes is the one short-lived container that performs a
// restore, on the one branch that needs it. So this knob reaches the
// restore one-off and nothing else: the backup one-off, the listing
// one-off and every DC (including the restored one) keep Caps().
//
// Same parsing as E2E_CAPS, and the same meaning for a set-but-empty
// value: no capabilities at all beyond the dropped-all baseline.
func RestoreCaps() ([]string, bool) {
	v, ok := os.LookupEnv("E2E_RESTORE_CAPS")
	if !ok {
		return nil, false
	}
	return parseCaps(v), true
}

// parseCaps turns a comma-separated capability list into the normalized
// form docker is given. Empty entries are dropped, so a trailing comma or
// a stray space cannot turn into a `--cap-add ` with nothing after it.
func parseCaps(v string) []string {
	var caps []string
	for _, c := range strings.Split(v, ",") {
		if c = strings.ToUpper(strings.TrimSpace(c)); c != "" {
			caps = append(caps, c)
		}
	}
	return caps
}

// FQDN returns the fully qualified name a container called name answers
// to inside the test network.
func FQDN(name string) string {
	return name + "." + strings.ToLower(Realm)
}

// BaseDN returns the realm as a directory naming context — the form
// samba's own tooling reports, e.g. "DC=ad,DC=e2e,DC=test".
func BaseDN() string {
	labels := strings.Split(strings.ToLower(Realm), ".")
	for i, l := range labels {
		labels[i] = "DC=" + l
	}
	return strings.Join(labels, ",")
}

// Preflight checks the two things the suite cannot create for itself.
// TestMain calls it once; a failure here beats every test failing with an
// obscure docker error.
func Preflight() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("the docker CLI is not on PATH; install docker and retry")
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()

	if out, code, err := dockerCmd(ctx, "version", "--format", "{{.Server.Version}}"); err != nil || code != 0 {
		return fmt.Errorf("the docker daemon is not reachable: %s", firstLine(out))
	}
	img := Image()
	if _, code, err := dockerCmd(ctx, "image", "inspect", img); err != nil || code != 0 {
		// The remedy has to name the image actually looked for, or an
		// E2E_IMAGE run sends the reader off building the wrong thing.
		if img != DefaultImage {
			return fmt.Errorf("image %s not found (E2E_IMAGE points at it); "+
				"build or pull that image, or unset E2E_IMAGE to test the default %s",
				img, DefaultImage)
		}
		return fmt.Errorf("image %s not found; build the image first: docker build -t %s .",
			img, DefaultImage)
	}
	return nil
}

// Sweep removes every container, volume, network and image still carrying
// this harness's ownership label, and returns one line per object it
// destroyed.
//
// It exists for the abnormal exit: a `go test` killed with SIGKILL, a CI job
// cancelled mid-run, a laptop that slept through a provision. None of those
// run t.Cleanup or AtExit, so they leave a container holding a fixed name
// and volumes holding a stale domain — and the next run then either fails on
// the name or, worse, silently adopts the old state.
//
// It is safe by construction: selection is by LABEL, so an object this
// harness did not create cannot be selected, no matter what it is called.
// That is the same guarantee removeOwned gives, moved to the front of the
// run and applied to objects whose names this process no longer knows.
//
// TestMain calls it before the first test. Callers should log what it
// returns: silently destroying state is exactly the behaviour the ownership
// label exists to prevent.
func Sweep() []string {
	var swept []string
	// Containers first: a volume or network still attached to a running
	// container cannot be removed, and `rm -f` on the container releases both.
	// Images last for the same reason one step further out: an image a
	// container still references cannot be removed either, and by this point
	// every container the harness owns is gone.
	for _, kind := range []string{"container", "volume", "network", "image"} {
		for _, name := range listOwned(kind) {
			// Re-check ownership rather than trusting the listing: removeOwned
			// is the single place that decides what may be destroyed.
			if _, owned := ownership(kind, name); !owned {
				continue
			}
			removeOwned(kind, name)
			if exists, _ := ownership(kind, name); !exists {
				swept = append(swept, kind+" "+name)
			}
		}
	}
	return swept
}

// listOwned returns the names of docker objects of that kind carrying the
// ownership label. A docker error yields no names: a sweep that cannot see
// is a sweep that does nothing.
func listOwned(kind string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()

	args := []string{kind, "ls", "--filter", "label=" + ownerLabelArg, "--format", "{{.Name}}"}
	switch kind {
	case "container":
		// Only `container ls` needs -a: a stopped leftover is precisely the
		// case this exists for. It also reports {{.Names}}, not {{.Name}}.
		args = []string{"container", "ls", "-a",
			"--filter", "label=" + ownerLabelArg, "--format", "{{.Names}}"}
	case "image":
		// Identified by ID rather than by `repository:tag`, because a
		// harness-built tag that a later build moved elsewhere leaves the
		// old image behind as `<none>:<none>` — a name `docker rmi` cannot
		// act on, so a listing by tag would report leftovers it can never
		// clear. The ID always names exactly one image.
		args = []string{"image", "ls",
			"--filter", "label=" + ownerLabelArg, "--format", "{{.ID}}"}
	}
	out, code, err := dockerCmd(ctx, args...)
	if err != nil || code != 0 {
		return nil
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if n := strings.TrimSpace(line); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// ---------------------------------------------------------------------
// docker plumbing
// ---------------------------------------------------------------------

// dockerCmd runs the docker CLI and returns its combined output and exit
// code. A non-nil error means docker could not be run (or the context
// expired) — a container that merely exited non-zero is reported through
// the code, not through the error.
func dockerCmd(ctx context.Context, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := buf.String()
	switch {
	case err == nil:
		return out, 0, nil
	case ctx.Err() != nil:
		return out, -1, fmt.Errorf("docker %s: %w", strings.Join(args, " "), ctx.Err())
	default:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out, ee.ExitCode(), nil
		}
		return out, -1, fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
}

// mustDocker runs a docker command that has no business failing.
func mustDocker(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	out, code, err := dockerCmd(ctx, args...)
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	if code != 0 {
		t.Fatalf("docker %s: exit %d\n%s", strings.Join(args, " "), code, out)
	}
	return out
}

// quietDocker runs a best-effort cleanup command; failures are ignored on
// purpose (the object may already be gone).
func quietDocker(args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	_, _, _ = dockerCmd(ctx, args...)
}

var nameSeq atomic.Uint64

// UniqueName builds a docker object name that cannot collide with another
// run, another test binary, or a leftover: prefix, this process's pid, and
// a counter.
//
// Use it for anything whose name does not have to be predictable. Names
// that a test *asserts* on — a DC's name is also its DNS name on the test
// network, so `dc1` becomes `dc1.ad.e2e.test` — are the exception, and
// those must be unique per test *by construction*: `docker run --name` is
// one flat namespace, so two tests sharing a fixed name would fight over
// the same container (see the `fixtureName` note in nominal_test.go).
func UniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, os.Getpid(), nameSeq.Add(1))
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

// ---------------------------------------------------------------------
// ownership: the harness only ever destroys what it created
// ---------------------------------------------------------------------

// ownership reports whether a docker object of that kind and name exists,
// and whether it carries this harness's ownership label. kind is one of
// "container", "volume", "network", "image".
func ownership(kind, name string) (exists, owned bool) {
	if name == "" {
		return false, false
	}
	// Where the label lives differs per object: a container and an image
	// keep it under .Config, a volume and a network at the top level.
	format := `{{index .Labels "` + OwnerLabel + `"}}`
	if kind == "container" || kind == "image" {
		format = `{{index .Config.Labels "` + OwnerLabel + `"}}`
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	out, code, err := dockerCmd(ctx, kind, "inspect", "-f", format, name)
	if err != nil || code != 0 {
		return false, false // no such object
	}
	return true, strings.TrimSpace(out) == OwnerLabelValue
}

// requireOwnedOrAbsent fails the test when an object of that name already
// exists and was not created by this harness. It is the guard in front of
// every force-removal by a fixed name.
func requireOwnedOrAbsent(t *testing.T, kind, name string) {
	t.Helper()
	if exists, owned := ownership(kind, name); exists && !owned {
		t.Fatalf("%s %q exists and is not harness-owned (it carries no %s=%s label); "+
			"the suite refuses to destroy it — remove or rename it yourself, "+
			"or run the suite where that name is free",
			kind, name, OwnerLabel, OwnerLabelValue)
	}
}

// removeOwned force-removes a docker object, but only if this harness
// created it. Best-effort and silent: it runs on teardown paths, including
// package-lifetime ones that have no *testing.T to report to.
func removeOwned(kind, name string) {
	if exists, owned := ownership(kind, name); !exists || !owned {
		return
	}
	switch kind {
	case "container":
		quietDocker("rm", "-f", name)
	case "volume":
		quietDocker("volume", "rm", "-f", name)
	case "network":
		quietDocker("network", "rm", name)
	case "image":
		// -f because a harness-built image may carry more than one tag (a
		// rebuilt tag leaves the previous image behind), and `docker rmi`
		// refuses an image referenced by several repositories without it.
		// It cannot reach anything unlabeled: ownership() above is the gate.
		quietDocker("rmi", "-f", name)
	}
}

// ---------------------------------------------------------------------
// network, secrets
// ---------------------------------------------------------------------

// Network creates a user-defined bridge network with its own /24 and
// returns its name; removal is registered with t.Cleanup.
//
// This topology is CI-only and does not contradict B.3. What B.3 forbids
// is NAT between domain members and the DC — a user-defined bridge gives
// the containers direct L2/L3 reachability to each other with no address
// translation, which is exactly the property the DC needs. Production
// documentation still mandates macvlan/ipvlan or host networking, because
// there the members live outside the docker host.
func Network(t *testing.T) string {
	t.Helper()
	return newNetwork(t, t.Cleanup)
}

// newNetwork is Network with the teardown registration left to the caller,
// so that SharedNetwork can hand it AtExit instead of t.Cleanup.
func newNetwork(t *testing.T, registerCleanup func(func())) string {
	t.Helper()
	name := UniqueName("e2e-net")
	// Pick a /24 from 10.199.0.0/16, which is outside docker's default
	// address pools (172.17.0.0/12 and 192.168.0.0/16): asking for a
	// subnet the daemon also hands out automatically would make this
	// collide with whatever unrelated compose project is running. Start at
	// a random offset and walk, so two suites on one machine do not fight
	// over the same first candidate.
	start := rand.Intn(200) //nolint:gosec // test fixture, not cryptography
	var last string
	for i := 0; i < 32; i++ {
		subnet := fmt.Sprintf("10.199.%d.0/24", (start+i)%200)
		ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
		out, code, err := dockerCmd(ctx, "network", "create",
			"--driver", "bridge", "--label", ownerLabelArg, "--subnet", subnet, name)
		cancel()
		if err == nil && code == 0 {
			registerCleanup(func() { removeOwned("network", name) })
			return name
		}
		last = out
	}
	t.Fatalf("could not create a test network after 32 subnet attempts: %s", strings.TrimSpace(last))
	return ""
}

// Secret writes value to a file in the test's temporary directory and
// returns its host path, ready to be bind-mounted into a container.
//
// The file is world-readable on purpose: it is bind-mounted into a
// container whose capability set is under test, and a 0600 file owned by
// the host user would make an unrelated capability (DAC_OVERRIDE) a
// prerequisite for reading it — which would corrupt the bisection.
func Secret(t *testing.T, value string) (hostPath string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil { //nolint:gosec // throwaway test secret, see doc comment
		t.Fatalf("writing secret file: %v", err)
	}
	return path
}

// ---------------------------------------------------------------------
// DC containers
// ---------------------------------------------------------------------

// DC describes a started DC container.
type DC struct {
	Name        string // container name (and its short DNS name on the network)
	IP          string // its address on the test network
	Realm       string // the realm it serves
	FQDN        string // <name>.<realm in lower case>
	Network     string // the network it is attached to
	Mode        string // the SAMBA_MODE it was started in
	StateVolume string // volume backing /var/lib/samba
	ConfVolume  string // volume backing /etc/samba
}

// Opt customizes a DC container before it is started.
type Opt func(*spec)

type spec struct {
	hostname   string
	aliases    []string
	env        map[string]string
	binds      []string
	stateVol   string
	confVol    string
	ownVolumes bool
	caps       []string
	tmpfs      []string
	dns        []string
	ip         string
	shared     bool
	runArgs    []string
	entrypoint string
	cmd        []string
	image      string
}

// WithVolumes reuses existing state and configuration volumes instead of
// creating fresh ones — the way an operator restarts a DC. The harness
// does not remove volumes it did not create.
func WithVolumes(state, conf string) Opt {
	return func(s *spec) {
		s.stateVol, s.confVol, s.ownVolumes = state, conf, false
	}
}

// WithBind adds a host bind mount.
func WithBind(hostPath, containerPath string, readOnly bool) Opt {
	return func(s *spec) {
		m := hostPath + ":" + containerPath
		if readOnly {
			m += ":ro"
		}
		s.binds = append(s.binds, m)
	}
}

// WithSecret mounts hostPath read-only at /run/secrets/<name> and points
// envVar at it — the `*_FILE` convention the image mandates (§6.1).
func WithSecret(envVar, name, hostPath string) Opt {
	return func(s *spec) {
		containerPath := "/run/secrets/" + name
		s.binds = append(s.binds, hostPath+":"+containerPath+":ro")
		s.env[envVar] = containerPath
	}
}

// AdminSecret mounts the throwaway Administrator password and points
// SAMBA_ADMIN_PASSWORD_FILE at it. Tests that must *not* get a secret
// (the negative matrix) simply do not pass this option.
func AdminSecret(t *testing.T) Opt {
	t.Helper()
	return WithSecret("SAMBA_ADMIN_PASSWORD_FILE", "admin-password", Secret(t, AdminPassword))
}

// JoinSecret mounts the throwaway join-account password and points
// SAMBA_JOIN_PASSWORD_FILE at it.
func JoinSecret(t *testing.T) Opt {
	t.Helper()
	return WithSecret("SAMBA_JOIN_PASSWORD_FILE", "join-password", Secret(t, AdminPassword))
}

// WithDNS points the container's resolver at the given addresses.
func WithDNS(ips ...string) Opt {
	return func(s *spec) { s.dns = append(s.dns, ips...) }
}

// WithAliases adds extra DNS names the container answers to on the
// network (it always answers to its name and its FQDN).
func WithAliases(aliases ...string) Opt {
	return func(s *spec) { s.aliases = append(s.aliases, aliases...) }
}

// WithIP pins the container's address on the network.
func WithIP(ip string) Opt { return func(s *spec) { s.ip = ip } }

// WithCaps replaces the capability set for this container only.
func WithCaps(caps ...string) Opt {
	return func(s *spec) { s.caps = append([]string(nil), caps...) }
}

// WithTmpfs replaces the tmpfs set for this container only.
func WithTmpfs(paths ...string) Opt {
	return func(s *spec) { s.tmpfs = append([]string(nil), paths...) }
}

// WithRunArgs appends raw `docker run` flags, for the rare case the
// options above do not cover.
func WithRunArgs(args ...string) Opt {
	return func(s *spec) { s.runArgs = append(s.runArgs, args...) }
}

// WithEntrypoint overrides the image entrypoint and its argv — used by
// the operational matrix to drive samba-tool directly against the
// volumes without starting a DC.
func WithEntrypoint(entrypoint string, argv ...string) Opt {
	return func(s *spec) {
		s.entrypoint = entrypoint
		s.cmd = append([]string(nil), argv...)
	}
}

// WithImage runs this container from a DIFFERENT image than the one under
// test, in the same constrained profile.
//
// It exists for exactly two rows of the B.5 matrix, and for nothing else —
// a test that silently ran against another image would report a verdict
// about something the build never produced:
//
//   - the upgrade row, which has to provision a volume with the LAST
//     PUBLISHED image and then start the candidate on it;
//   - the reuse row, whose subject is an image built FROM the one under
//     test, so that "a derived image inherits the runtime contract" is
//     measured on a real derived image rather than asserted in prose.
func WithImage(ref string) Opt {
	return func(s *spec) { s.image = strings.TrimSpace(ref) }
}

// Volume creates an empty, harness-owned docker volume and returns its
// name; removal is registered with t.Cleanup.
//
// StartDC creates the state and configuration volumes a DC needs on its
// own, so this is only for the volumes that are not a DC's own state: the
// one an offline backup writes its tarball into, and the empty pair a
// restore is poured into before any container has ever run on them.
func Volume(t *testing.T) string {
	t.Helper()
	name := UniqueName("e2e-vol")
	mustDocker(t, "volume", "create", "--label", ownerLabelArg, name)
	t.Cleanup(func() { removeOwned("volume", name) })
	return name
}

// EnsureImage makes ref available locally, pulling it once if it is not.
//
// Preflight does this for the image under test, which the suite must never
// build or fetch for itself — a suite that pulled its own subject could
// report green about an image the build never produced. A reference image
// named by the operator through E2E_UPGRADE_FROM is the opposite case: it
// is an input, it is expected to come from a registry, and failing the
// upgrade test with "no such image" would say nothing useful.
func EnsureImage(t *testing.T, ref string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	_, code, err := dockerCmd(ctx, "image", "inspect", ref)
	cancel()
	if err == nil && code == 0 {
		return
	}
	pullCtx, pullCancel := context.WithTimeout(context.Background(), PullTimeout)
	defer pullCancel()
	out, code, err := dockerCmd(pullCtx, "pull", ref)
	if err != nil || code != 0 {
		t.Fatalf("image %s is not present locally and cannot be pulled (exit %d): %v\n%s",
			ref, code, err, strings.TrimSpace(out))
	}
}

// Build builds contextDir into a harness-owned image tagged ref, and
// registers its removal with t.Cleanup.
//
// It is the one place the suite is allowed to produce an image, and it is
// NOT a way to build the subject: Preflight refuses to run at all unless
// the image under test already exists, because a suite that built its own
// subject could report green about something the release build never
// produced. What this is for is an image whose whole point is to be
// derived from that subject (the B.5 reuse row).
//
// The ownership label goes on with `--label`, so the image is subject to
// the same rule as every container, volume and network here: the harness
// removes what it created and refuses to touch anything else. That matters
// more for an image than for the rest, because an image is the one docker
// object a developer is likely to have under a name of their own — hence
// the requireOwnedOrAbsent guard before the build, which refuses to
// overwrite a tag this harness did not make.
//
// Removal is unconditional, including when the test fails. An image is not
// evidence: this one's entire content is the Dockerfile the caller just
// wrote a few lines above, and the diagnosis lives in the container logs
// the failing test already dumps.
func Build(t *testing.T, ref, contextDir string) {
	t.Helper()
	requireOwnedOrAbsent(t, "image", ref)
	// Registered BEFORE the build, so an interrupted or partially
	// successful build cannot leave a tagged image behind. removeOwned is
	// a no-op for an image that was never created.
	t.Cleanup(func() { removeOwned("image", ref) })

	ctx, cancel := context.WithTimeout(context.Background(), BuildTimeout)
	defer cancel()
	out, code, err := dockerCmd(ctx, "build", "--label", ownerLabelArg, "-t", ref, contextDir)
	if err != nil || code != 0 {
		t.Fatalf("docker build -t %s %s: exit %d: %v\n%s",
			ref, contextDir, code, err, strings.TrimSpace(out))
	}
}

// Inspect renders a docker Go template over an object and returns the
// result trimmed. kind is one of "container", "volume", "network",
// "image".
//
// It is how a test asserts on what the IMAGE declares — the entrypoint,
// the healthcheck, the volumes, the environment, the OCI labels — rather
// than on what happens to work at runtime. Those are the parts of the
// runtime contract that have no observable behaviour to test: an image
// that lost its HEALTHCHECK still starts and still serves the domain, and
// only an inspect notices.
func Inspect(t *testing.T, kind, name, format string) string {
	t.Helper()
	return strings.TrimSpace(mustDocker(t, kind, "inspect", "-f", format, name))
}

// StartDC starts the image under test in the constrained profile and
// returns once the container is running — not once it is healthy; use
// WaitHealthy for that. Container and (harness-created) volumes are
// removed by t.Cleanup, and the container's logs are dumped into the test
// log if the test failed.
func StartDC(t *testing.T, net, name, mode string, env map[string]string, opts ...Opt) *DC {
	t.Helper()
	dc, _ := startDC(t, net, name, mode, env, true, opts...)
	return dc
}

// RunDCExpectExit starts the image in the SAME constrained profile, waits
// for it to terminate on its own, and returns its exit code together with
// its combined logs. It is how the negative matrix asserts the runtime
// contract's exit codes.
func RunDCExpectExit(t *testing.T, net, name, mode string, env map[string]string, opts ...Opt) (int, string) {
	t.Helper()
	dc, _ := startDC(t, net, name, mode, env, false, opts...)

	ctx, cancel := context.WithTimeout(context.Background(), ExitTimeout)
	defer cancel()
	out, code, err := dockerCmd(ctx, "wait", dc.Name)
	logs := Logs(t, dc.Name)
	if err != nil || code != 0 {
		t.Fatalf("container %s did not exit within %s (%v): %s\n--- logs ---\n%s",
			dc.Name, ExitTimeout, err, strings.TrimSpace(out), logs)
	}
	// `docker wait` prints the exit status it waited for, so there is no
	// need to ask again with an inspect — and no window in which something
	// could remove the container between the two calls.
	exit, convErr := strconv.Atoi(lastLine(out))
	if convErr != nil {
		t.Fatalf("docker wait %s printed %q instead of an exit code\n--- logs ---\n%s",
			dc.Name, strings.TrimSpace(out), logs)
	}
	return exit, logs
}

// startDC is the shared body of StartDC and RunDCExpectExit. mustLive says
// which of the two called it: true when the container is expected to come up
// and keep running, false when it is expected to terminate on its own. Only
// the name guard below reads it.
func startDC(t *testing.T, net, name, mode string, env map[string]string, mustLive bool, opts ...Opt) (*DC, *spec) {
	t.Helper()

	s := &spec{
		hostname:   name,
		env:        map[string]string{},
		caps:       Caps(),
		tmpfs:      append([]string(nil), DefaultTmpfs...),
		ownVolumes: true,
	}
	for k, v := range env {
		s.env[k] = v
	}
	if mode != "" {
		s.env["SAMBA_MODE"] = mode
	}
	for _, opt := range opts {
		opt(s)
	}
	requireNetBIOSName(t, name, mode, s, mustLive)

	// A leftover container of the same name from an interrupted run must
	// not turn every later run red — but only a leftover of *ours* may be
	// destroyed; a developer's own `dc1` is left strictly alone.
	requireOwnedOrAbsent(t, "container", name)
	removeOwned("container", name)

	if s.ownVolumes {
		s.stateVol = UniqueName(name + "-state")
		s.confVol = UniqueName(name + "-conf")
	}
	stateVol, confVol, ownVolumes := s.stateVol, s.confVol, s.ownVolumes

	teardown := func() {
		removeOwned("container", name)
		if ownVolumes {
			removeOwned("volume", stateVol)
			removeOwned("volume", confVol)
		}
	}
	// Registered BEFORE the volumes exist: if the second `volume create`
	// fails, the first one is already covered by this teardown instead of
	// being orphaned. removeOwned is a no-op for a volume that was never
	// created.
	if s.shared {
		// A package-lifetime container outlives the test that started it,
		// so its teardown cannot log through that test's t (see Shared).
		AtExit(teardown)
	} else {
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("--- docker logs %s ---\n%s", name, Logs(t, name))
			}
			teardown()
		})
	}

	if ownVolumes {
		mustDocker(t, "volume", "create", "--label", ownerLabelArg, stateVol)
		mustDocker(t, "volume", "create", "--label", ownerLabelArg, confVol)
	}

	args := []string{"run", "-d", "--name", name, "--label", ownerLabelArg,
		"--network", net, "--hostname", s.hostname}
	for _, a := range append([]string{FQDN(name)}, s.aliases...) {
		args = append(args, "--network-alias", a)
	}
	if s.ip != "" {
		args = append(args, "--ip", s.ip)
	}
	// The constrained profile (B.2 / §5.2), applied to every DC container
	// the suite ever starts.
	args = append(args, "--read-only")
	for _, p := range s.tmpfs {
		args = append(args, "--tmpfs", p)
	}
	args = append(args, "--cap-drop", "ALL")
	for _, c := range s.caps {
		args = append(args, "--cap-add", c)
	}
	for _, o := range DefaultSecurityOpt {
		args = append(args, "--security-opt", o)
	}
	args = append(args,
		"-v", s.stateVol+":/var/lib/samba",
		"-v", s.confVol+":/etc/samba")
	for _, b := range s.binds {
		args = append(args, "-v", b)
	}
	for _, d := range s.dns {
		args = append(args, "--dns", d)
	}
	for _, k := range sortedKeys(s.env) {
		args = append(args, "-e", k+"="+s.env[k])
	}
	args = append(args, s.runArgs...)
	if s.entrypoint != "" {
		args = append(args, "--entrypoint", s.entrypoint)
	}
	image := s.image
	if image == "" {
		image = Image()
	}
	args = append(args, image)
	args = append(args, s.cmd...)

	mustDocker(t, args...)

	dc := &DC{
		Name:        name,
		Realm:       Realm,
		FQDN:        FQDN(name),
		Network:     net,
		Mode:        mode,
		StateVolume: s.stateVol,
		ConfVolume:  s.confVol,
	}
	// The address and the state are read in ONE inspect on purpose. A
	// container that has already finished has no address any more, and the
	// one-off containers of the operational matrix — `samba-tool domain
	// backup offline`, a marker edit through python3 — routinely exit before
	// this line runs. Treating that as "no address on the network" would
	// report a networking fault for a command that simply succeeded quickly.
	//
	// The relaxation is keyed on mustLive, not on the entrypoint override: a
	// container handed to RunDCExpectExit is asserted to terminate, and it
	// may well have done so already — the negative matrix's refusals exit in
	// under a second, and so does `python3 -c`. Demanding an address of a
	// container that has correctly finished reports a networking fault for a
	// success.
	//
	// A container StartDC started is the opposite case: it must be running,
	// its address is what the tests talk to, and a missing one is a fault
	// worth naming here and now rather than letting WaitHealthy turn a
	// precise diagnosis into a timeout.
	out := strings.TrimSpace(mustDocker(t, "inspect", "-f",
		fmt.Sprintf("{{with index .NetworkSettings.Networks %q}}{{.IPAddress}}{{end}}|{{.State.Status}}", net), name))
	ip, status, _ := strings.Cut(out, "|")
	dc.IP = strings.TrimSpace(ip)
	if dc.IP == "" && mustLive {
		t.Fatalf("container %s has no address on network %s (state: %s)",
			name, net, strings.TrimSpace(status))
	}
	return dc, s
}

// NetBIOSNameLimit is the longest a container name may be when that
// container is allowed to INITIALIZE a domain.
//
// A DC container's name is its host name, and samba derives the domain
// controller's NetBIOS name from it. NetBIOS names are capped at 15
// characters, so samba truncates — and a name that is 16 characters with a
// hyphen at position 16 truncates to something ending in `-`, which is not a
// legal DNS label. The provision then registers a host record the image's own
// health probe cannot resolve, and the container sits at `starting` until it
// times out with a DNS error that points at samba's DNS server rather than at
// the name that caused it.
//
// Evidence: Task 5 hit exactly this with the container name
// `negative-state-dc1` (18 characters). The failure looked like a product
// defect and was not one.
const NetBIOSNameLimit = 15

// requireNetBIOSName refuses, before anything is created, a container name
// that samba would have to truncate.
//
// It is deliberately narrow, and each of the three exemptions is load-bearing
// rather than a convenience:
//
//   - Only modes that may WRITE a new domain into the volume are checked —
//     provision, join, and the auto mode an empty SAMBA_MODE selects —
//     because that is the moment the name is baked into the directory as
//     `netbios name`. A run- or maintenance-mode container reads that name
//     back out of the smb.conf on the configuration volume and never
//     consults its own host name, which is why the restart and upgrade rows
//     can legitimately give the replacement container a longer, more
//     descriptive name.
//   - A container whose entrypoint is overridden is exempt because the
//     entrypoint never runs at all: a one-off `samba-tool`, `sh` or
//     `python3` initializes nothing.
//   - Only StartDC is checked, never RunDCExpectExit (mustLive). A container
//     handed to RunDCExpectExit is asserted to TERMINATE — it is the
//     negative matrix, where provision mode is chosen precisely so the
//     entrypoint can refuse it, and the name never reaches samba. Guarding
//     there would fail correct tests (`refuse-provision`, `refuse-env-admin`)
//     for a truncation that can never happen, while adding nothing: a
//     container that did provision successfully would not exit, and the
//     exit-code assertion would catch it.
func requireNetBIOSName(t *testing.T, name, mode string, s *spec, mustLive bool) {
	t.Helper()
	if !mustLive || s.entrypoint != "" || len(name) <= NetBIOSNameLimit {
		return
	}
	switch mode {
	case "", "auto", "provision", "join":
		t.Fatalf("container name %q is %d characters, over the %d-character NetBIOS limit, "+
			"and SAMBA_MODE=%s may initialize a domain with it: samba would truncate the "+
			"name to %q and register a host record the health probe cannot resolve "+
			"(a truncation ending in `-` is not a legal DNS label). Give the container a "+
			"name of at most %d characters.",
			name, len(name), NetBIOSNameLimit, modeOrAuto(mode),
			name[:NetBIOSNameLimit], NetBIOSNameLimit)
	}
}

// modeOrAuto renders an empty SAMBA_MODE as the mode it actually selects.
func modeOrAuto(mode string) string {
	if mode == "" {
		return "auto (unset)"
	}
	return mode
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------
// observing a container
// ---------------------------------------------------------------------

// WaitHealthy blocks until docker reports the container healthy, failing
// the test — with logs and the health-probe output — if it exits, stays
// unhealthy, or runs out of time.
func WaitHealthy(t *testing.T, name string, within time.Duration) {
	t.Helper()
	if within <= 0 {
		within = HealthTimeout
	}
	deadline := time.Now().Add(within)
	const format = "{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}"

	for {
		ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
		out, code, err := dockerCmd(ctx, "inspect", "-f", format, name)
		cancel()
		if err != nil || code != 0 {
			t.Fatalf("inspecting %s: %v\n%s", name, err, strings.TrimSpace(out))
		}
		state, health, _ := strings.Cut(strings.TrimSpace(out), "|")

		switch {
		// Death first: a container that exited has no healthcheck status
		// worth reporting, and diagnosing it as "no healthcheck" would send
		// the reader after the image instead of after the logs.
		case state != "running" && state != "created" && state != "restarting":
			t.Fatalf("container %s is %s (exit %d) instead of becoming healthy\n--- logs ---\n%s",
				name, state, exitCode(t, name), Logs(t, name))
		case health == "healthy":
			return
		case health == "none":
			t.Fatalf("container %s has no healthcheck; the image under test must define one", name)
		// Docker only ever says `unhealthy` once the start period is over
		// and the probe has failed `retries` times in a row — inside the
		// start period it keeps saying `starting`. So this is already a
		// settled verdict, and waiting out the remaining deadline would
		// only delay the same failure.
		case health == "unhealthy":
			t.Fatalf("container %s went unhealthy (docker's verdict after the start period; "+
				"waiting longer would not change it)\n--- health probe ---\n%s\n--- logs ---\n%s",
				name, healthLog(name), Logs(t, name))
		case time.Now().After(deadline):
			t.Fatalf("container %s did not become healthy within %s (last health: %s)\n"+
				"--- health probe ---\n%s\n--- logs ---\n%s",
				name, within, health, healthLog(name), Logs(t, name))
		}
		time.Sleep(2 * time.Second)
	}
}

// Stop stops a container the way an orchestrator does and returns its
// exit code. The runtime contract says a container asked to stop exits 0
// (§6.3), so this is what the operational matrix asserts on.
func Stop(t *testing.T, name string, timeout time.Duration) int {
	t.Helper()
	secs := int(timeout.Round(time.Second) / time.Second)
	if secs <= 0 {
		secs = 15
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout+dockerTimeout)
	defer cancel()
	if out, code, err := dockerCmd(ctx, "stop", "-t", strconv.Itoa(secs), name); err != nil || code != 0 {
		t.Fatalf("stopping %s: %v (exit %d)\n%s", name, err, code, strings.TrimSpace(out))
	}
	return exitCode(t, name)
}

// Exec runs a command inside a running container and returns its combined
// output, failing the test if it exits non-zero.
func Exec(t *testing.T, name string, cmd ...string) string {
	t.Helper()
	code, out := ExecErr(t, name, cmd...)
	if code != 0 {
		t.Fatalf("docker exec %s %s: exit %d\n%s", name, strings.Join(cmd, " "), code, out)
	}
	return out
}

// ExecErr is Exec without the assertion: it returns the exit code and the
// combined output so a test can assert on a failure.
func ExecErr(t *testing.T, name string, cmd ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), ExecTimeout)
	defer cancel()
	out, code, err := dockerCmd(ctx, append([]string{"exec", name}, cmd...)...)
	if err != nil {
		t.Fatalf("docker exec %s %s: %v\n%s", name, strings.Join(cmd, " "), err, out)
	}
	return code, out
}

// Logs returns a container's combined stdout and stderr so far. It never
// fails the test: it is used on the failure path, where losing the logs
// to a secondary error would be the worst possible outcome.
func Logs(t *testing.T, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	out, _, err := dockerCmd(ctx, "logs", name)
	if err != nil {
		return fmt.Sprintf("(logs unavailable: %v)", err)
	}
	return out
}

// exitCode reads a stopped container's exit status.
func exitCode(t *testing.T, name string) int {
	t.Helper()
	out := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.State.ExitCode}}", name))
	code, err := strconv.Atoi(out)
	if err != nil {
		t.Fatalf("unparsable exit code %q for container %s", out, name)
	}
	return code
}

// healthLog returns the recorded output of the last health probes.
func healthLog(name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	out, _, err := dockerCmd(ctx, "inspect", "-f",
		"{{range .State.Health.Log}}[exit {{.ExitCode}}] {{.Output}}{{end}}", name)
	if err != nil {
		return fmt.Sprintf("(health log unavailable: %v)", err)
	}
	return strings.TrimSpace(out)
}

// repoPath resolves a path relative to the repository root, independent
// of the directory `go test` was invoked from.
func repoPath(parts ...string) string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return filepath.Join(parts...)
	}
	// this file is <root>/test/e2e/harness/harness.go
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	return filepath.Join(append([]string{root}, parts...)...)
}
