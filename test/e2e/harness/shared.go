package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// This file adds what a *shared fixture* needs and nothing more.
//
// Every other resource in this package hangs its teardown on the
// `t.Cleanup` of the test that created it, which is exactly right for a
// resource one test owns. A fixture shared by a whole package cannot work
// that way: it would be destroyed at the end of whichever test happened to
// touch it first, and every later test would then race a dying container.
// So a shared resource registers its teardown here instead, and the
// package's TestMain drains the registry after `m.Run()` — the only moment
// at which no test can still be using it.

var (
	atExitMu sync.Mutex
	atExit   []func()
)

// AtExit registers a teardown function for a resource whose lifetime is the
// whole package run rather than one test. Cleanup runs them.
func AtExit(f func()) {
	atExitMu.Lock()
	defer atExitMu.Unlock()
	atExit = append(atExit, f)
}

// Cleanup runs every function AtExit registered, most recent first, and
// empties the registry. TestMain calls it after m.Run() returns; calling it
// twice is harmless. It never panics on a failing teardown — the individual
// teardowns are best-effort docker commands.
func Cleanup() {
	atExitMu.Lock()
	fns := atExit
	atExit = nil
	atExitMu.Unlock()

	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
}

// Shared makes StartDC treat the container (and the volumes it creates for
// it) as a package-lifetime resource: teardown goes to AtExit instead of
// t.Cleanup, so the container survives the test that started it.
//
// The trade-off is deliberate and small: a shared container's logs are not
// dumped automatically when a test fails, because the test that would do
// the dumping has long finished by the time the teardown runs. Tests using
// a shared DC print harness.Logs themselves on the paths where the logs
// matter.
func Shared() Opt { return func(s *spec) { s.shared = true } }

// SharedNetwork is Network for a package-lifetime network: same network,
// same addressing, teardown registered with AtExit rather than t.Cleanup.
func SharedNetwork(t *testing.T) string {
	t.Helper()
	return newNetwork(t, AtExit)
}

// CopyFrom copies a file out of a container with `docker cp` and returns
// its bytes. It is how a test gets hold of material the DC generated for
// itself — the TLS CA and certificate LDAPS is served with — without
// reading it through a shell, where the combined stdout/stderr of an exec
// could corrupt binary or PEM content.
func CopyFrom(t *testing.T, container, containerPath string) []byte {
	t.Helper()

	dst := filepath.Join(t.TempDir(), filepath.Base(containerPath))
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	out, code, err := dockerCmd(ctx, "cp", container+":"+containerPath, dst)
	if err != nil || code != 0 {
		t.Fatalf("docker cp %s:%s: exit %d: %v\n%s",
			container, containerPath, code, err, strings.TrimSpace(out))
	}
	data, err := os.ReadFile(dst) //nolint:gosec // path built from t.TempDir
	if err != nil {
		t.Fatalf("reading the copy of %s:%s: %v", container, containerPath, err)
	}
	return data
}
