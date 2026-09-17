package harness

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ClientImageTag is the local tag of the protocol test-client image.
const ClientImageTag = "samba-ad-dc-e2e-client:dev"

// ClientTimeout bounds a single test-client invocation.
const ClientTimeout = 3 * time.Minute

// ClientDNSEnv is a harness directive rather than a container variable:
// when it appears in the env map given to Client/ClientErr, its value (a
// comma-separated list of addresses) becomes the client container's
// `--dns` servers and is NOT exported into the container.
//
// It exists because the client's resolver has to point at the DC for
// Kerberos to work at all — kinit finds the KDC through the realm's SRV
// records — while the Client signature is a locked contract that takes no
// options.
const ClientDNSEnv = "E2E_DNS"

var (
	clientOnce sync.Once
	clientTag  string
	clientErr  error
)

// ClientImage builds the protocol test-client image once per test binary
// and returns its tag. E2E_CLIENT_IMAGE short-circuits the build with a
// prebuilt image (CI builds it as its own step).
//
// The client image is not a published artifact — §5.1's minimality rules
// govern what ships, and nothing here ships. It exists so that Kerberos,
// SMB, LDAP, DNS and NTP are exercised over the network by a separate
// container, the way a real domain member would, instead of by
// shortcuts inside the DC.
func ClientImage(t *testing.T) string {
	t.Helper()
	clientOnce.Do(func() {
		if v := strings.TrimSpace(os.Getenv("E2E_CLIENT_IMAGE")); v != "" {
			clientTag = v
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		dir := repoPath("test", "client")
		out, code, err := dockerCmd(ctx, "build", "-t", ClientImageTag, dir)
		if err != nil || code != 0 {
			clientErr = fmt.Errorf("building the test-client image from %s failed (exit %d): %v\n%s",
				dir, code, err, out)
			return
		}
		clientTag = ClientImageTag
	})
	if clientErr != nil {
		t.Fatalf("%v", clientErr)
	}
	return clientTag
}

// Client runs cmd in a one-off test-client container attached to net and
// returns its combined output, failing the test if it exits non-zero.
//
// Secrets go in through env and are read from stdin inside the container,
// never through argv:
//
//	harness.Client(t, net, map[string]string{"E2E_PW": harness.AdminPassword},
//	    "sh", "-c", `printf %s "$E2E_PW" | kinit --password-file=STDIN `+
//	        "Administrator@"+harness.Realm)
//
// A password in the command line would land in the container's process
// listing, in `docker inspect`, and — because this function echoes the
// command it ran into every failure message — in the test output of any
// failing run. Heimdal's kinit accepts `--password-file=STDIN` precisely
// so it never has to appear there.
func Client(t *testing.T, net string, env map[string]string, cmd ...string) string {
	t.Helper()
	code, out := ClientErr(t, net, env, cmd...)
	if code != 0 {
		t.Fatalf("test-client %s: exit %d\n%s", strings.Join(cmd, " "), code, out)
	}
	return out
}

// ClientErr is Client without the assertion: it returns the exit code and
// the combined output, for tests that assert on a failing client. The
// secret-handling contract in Client's documentation applies here too.
func ClientErr(t *testing.T, net string, env map[string]string, cmd ...string) (int, string) {
	t.Helper()
	img := ClientImage(t)

	// `--rm` covers the normal path, but not the one that matters: if this
	// invocation hits ClientTimeout, the CLI is killed while the container
	// keeps running. Without a name of our own it would then be anonymous
	// and unfindable, so it gets one — and a cleanup that removes it.
	name := UniqueName("e2e-client")
	t.Cleanup(func() { removeOwned("container", name) })

	args := []string{"run", "--rm", "--name", name, "--label", ownerLabelArg, "--network", net}
	for _, srv := range splitList(env[ClientDNSEnv]) {
		args = append(args, "--dns", srv)
	}
	for _, k := range sortedKeys(env) {
		if k == ClientDNSEnv {
			continue
		}
		args = append(args, "-e", k+"="+env[k])
	}
	args = append(args, img)
	args = append(args, cmd...)

	ctx, cancel := context.WithTimeout(context.Background(), ClientTimeout)
	defer cancel()
	out, code, err := dockerCmd(ctx, args...)
	if err != nil {
		t.Fatalf("running the test client (%s): %v\n%s", strings.Join(cmd, " "), err, out)
	}
	return code, out
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// OwnAsRoot gives every named file in hostDir uid 0, gid 0 and the mode asked
// for, by running chown and chmod inside a throwaway container that has the
// directory bind-mounted read-write.
//
// A test process running as an ordinary user cannot create a root-owned file,
// and one of the files here has to be exactly that: samba refuses to start
// its LDAP server unless the TLS private key is mode 0600 AND owned by the
// user samba runs as (root in this image) — measured, and fatal rather than
// advisory. Docker preserves the host uid across a bind mount, so on a Linux
// runner the key would arrive owned by the CI user and samba would refuse it.
//
// Doing it from a container is also what the deployment guide tells an
// operator to do with sudo, so the suite and the documentation ask for the
// same thing.
func OwnAsRoot(t *testing.T, hostDir string, modes map[string]os.FileMode) {
	t.Helper()
	img := ClientImage(t)

	script := "set -e\n"
	for _, name := range sortedFileModes(modes) {
		script += fmt.Sprintf("chown 0:0 /m/%s\nchmod %04o /m/%s\n", name, modes[name].Perm(), name)
	}
	script += "ls -ln /m\n"

	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	out, code, err := dockerCmd(ctx, "run", "--rm", "--label", ownerLabelArg,
		"-v", hostDir+":/m", img, "sh", "-c", script)
	if err != nil || code != 0 {
		t.Fatalf("giving %s root ownership failed (exit %d): %v\n%s", hostDir, code, err, out)
	}
}

// sortedFileModes returns the file names of m in a stable order, so the
// script OwnAsRoot builds is the same on every run.
func sortedFileModes(m map[string]os.FileMode) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
