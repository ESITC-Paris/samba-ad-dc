// Package e2e is the black-box end-to-end suite for the samba-ad-dc
// image (SPEC §8.2, Annex B.5). Every test function name in this package
// is a stable traceability ID: they are referenced from
// docs/traceability.md and MUST NOT be renamed.
//
// # Running it
//
//	cd test/e2e && go test ./... -v -count=1 -timeout 45m
//
// The `-timeout` is REQUIRED, not decoration. go test defaults to 10
// minutes for the whole binary, and this suite provisions domains, joins a
// second domain controller and waits for replication to converge: the
// multi-DC test alone budgets up to ~19 minutes of worst case. Worse than
// being slow, blowing the binary timeout is *destructive*: go test panics
// the process, which skips every t.Cleanup and every AtExit teardown and
// leaves labeled containers, volumes and networks behind. (harness.Sweep,
// which TestMain runs, cleans those up on the NEXT run — but a run that
// leaks is still a run whose teardown never proved anything.)
//
// TestJoinReplicationBothWays refuses to start when the deadline it is
// given is plainly too short, rather than failing 10 minutes later with a
// timeout that looks like a product defect.
package e2e

import (
	"fmt"
	"os"
	"testing"

	"github.com/esitc-paris/samba-ad-dc/test/e2e/harness"
)

// TestMain refuses to run the suite unless the two things it cannot
// create itself are present: a working docker CLI and the image under
// test. Failing here, once, beats every test failing with an obscure
// docker error.
//
// It creates no fixture: the shared provisioned DC is built lazily by
// provisionedDC(), so a run that selects only tests which do not need it
// never pays for it. TestMain only tears down what such a package-lifetime
// fixture left behind, at the one moment no test can still be using it.
func TestMain(m *testing.M) {
	if err := harness.Preflight(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e preflight failed: %v\n", err)
		os.Exit(1)
	}
	// Anything still labeled belongs to a run that died without teardown —
	// a killed `go test`, a cancelled CI job, a binary timeout. It is
	// reported rather than swept in silence, because "the suite destroyed
	// something" must always be visible in the log.
	for _, s := range harness.Sweep() {
		fmt.Fprintf(os.Stderr, "e2e preflight: swept leftover %s from a previous run\n", s)
	}
	code := m.Run()
	harness.Cleanup()
	os.Exit(code)
}
