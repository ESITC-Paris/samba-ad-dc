// Package e2e is the black-box end-to-end suite for the samba-ad-dc
// image (SPEC §8.2, Annex B.5). Every test function name in this package
// is a stable traceability ID: they are referenced from
// docs/traceability.md and MUST NOT be renamed.
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
	code := m.Run()
	harness.Cleanup()
	os.Exit(code)
}
