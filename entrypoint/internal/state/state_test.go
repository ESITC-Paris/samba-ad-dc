package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/config"
)

// stateDir builds a temp dir; when present is true it also creates the
// private/sam.ldb file that defines "state present".
func stateDir(t *testing.T, present bool) string {
	t.Helper()
	dir := t.TempDir()
	if present {
		if err := os.MkdirAll(filepath.Join(dir, "private"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "private", "sam.ldb"), []byte("ldb"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestObserveEmptyDir(t *testing.T) {
	obs, err := Observe(stateDir(t, false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if obs.Present {
		t.Errorf("Present = true, want false for an empty volume")
	}
	if obs.Marker != nil {
		t.Errorf("Marker = %+v, want nil", obs.Marker)
	}
}

func TestObserveMissingDir(t *testing.T) {
	// An unmounted volume path is simply "no state", not an error.
	obs, err := Observe(filepath.Join(t.TempDir(), "never-created"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if obs.Present {
		t.Errorf("Present = true, want false for a missing directory")
	}
	if obs.Marker != nil {
		t.Errorf("Marker = %+v, want nil", obs.Marker)
	}
}

func TestObservePresentWithoutMarker(t *testing.T) {
	// Foreign / pre-existing volume: state is there, marker is not.
	obs, err := Observe(stateDir(t, true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !obs.Present {
		t.Errorf("Present = false, want true when private/sam.ldb exists")
	}
	if obs.Marker != nil {
		t.Errorf("Marker = %+v, want nil when no marker file exists", obs.Marker)
	}
}

func TestObserveMarkerWithoutState(t *testing.T) {
	// Marker but no sam.ldb: the marker is still reported so the caller can
	// explain the inconsistency instead of silently re-provisioning.
	dir := stateDir(t, false)
	want := Marker{SambaVersion: "4.24.6", InitializedAt: "2026-08-16T10:00:00Z", LastMode: "provision"}
	if err := WriteMarker(dir, want); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	obs, err := Observe(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if obs.Present {
		t.Errorf("Present = true, want false without private/sam.ldb")
	}
	if obs.Marker == nil || *obs.Marker != want {
		t.Errorf("Marker = %+v, want %+v", obs.Marker, want)
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	dir := stateDir(t, true)
	want := Marker{SambaVersion: "4.24.6", InitializedAt: "2026-08-16T10:00:00Z", LastMode: "provision"}
	if err := WriteMarker(dir, want); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}

	obs, err := Observe(dir)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !obs.Present {
		t.Errorf("Present = false, want true")
	}
	if obs.Marker == nil {
		t.Fatal("Marker = nil, want the marker just written")
	}
	if *obs.Marker != want {
		t.Errorf("Marker = %+v, want %+v", *obs.Marker, want)
	}

	// The on-disk shape is contract (documented in the adaptation profile).
	raw, err := os.ReadFile(filepath.Join(dir, MarkerName))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("marker is not valid JSON: %v", err)
	}
	for key, want := range map[string]string{
		"samba_version":  "4.24.6",
		"initialized_at": "2026-08-16T10:00:00Z",
		"last_mode":      "provision",
	} {
		got, ok := fields[key].(string)
		if !ok {
			t.Errorf("marker JSON is missing key %q (got %v)", key, fields)
			continue
		}
		if got != want {
			t.Errorf("marker[%q] = %q, want %q", key, got, want)
		}
	}
}

func TestWriteMarkerPermissions(t *testing.T) {
	dir := stateDir(t, true)
	if err := WriteMarker(dir, Marker{SambaVersion: "4.24.6", InitializedAt: "2026-08-16T10:00:00Z", LastMode: "run"}); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, MarkerName))
	if err != nil {
		t.Fatalf("stat marker: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("marker permissions = %o, want 600", perm)
	}
}

func TestWriteMarkerLeavesNoTempFiles(t *testing.T) {
	dir := stateDir(t, false)
	if err := WriteMarker(dir, Marker{SambaVersion: "4.24.6", InitializedAt: "2026-08-16T10:00:00Z", LastMode: "provision"}); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != MarkerName {
		t.Errorf("directory contains %v, want only %q (temp file not cleaned up)", names, MarkerName)
	}
}

func TestWriteMarkerReplacesAtomically(t *testing.T) {
	dir := stateDir(t, true)
	first := Marker{SambaVersion: "4.24.6", InitializedAt: "2026-08-16T10:00:00Z", LastMode: "provision"}
	second := Marker{SambaVersion: "4.24.10", InitializedAt: "2026-08-17T11:00:00Z", LastMode: "run"}
	if err := WriteMarker(dir, first); err != nil {
		t.Fatalf("WriteMarker(first): %v", err)
	}
	if err := WriteMarker(dir, second); err != nil {
		t.Fatalf("WriteMarker(second): %v", err)
	}
	obs, err := Observe(dir)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.Marker == nil || *obs.Marker != second {
		t.Errorf("Marker = %+v, want %+v", obs.Marker, second)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != MarkerName && e.Name() != "private" {
			t.Errorf("unexpected leftover entry %q after rewrite", e.Name())
		}
	}
}

func TestWriteMarkerFailureLeavesNoPartialFile(t *testing.T) {
	t.Run("target directory does not exist", func(t *testing.T) {
		parent := t.TempDir()
		dir := filepath.Join(parent, "absent")
		err := WriteMarker(dir, Marker{SambaVersion: "4.24.6", InitializedAt: "2026-08-16T10:00:00Z", LastMode: "run"})
		if err == nil {
			t.Fatal("expected an error writing into a missing directory")
		}
		var r *config.Refusal
		if !errors.As(err, &r) {
			t.Fatalf("expected *config.Refusal, got %T: %v", err, err)
		}
		if !strings.Contains(r.Msg, dir) && !strings.Contains(r.Msg, MarkerName) {
			t.Errorf("message %q names neither the directory nor the marker file", r.Msg)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("failed write created something at %q", dir)
		}
	})

	t.Run("failing rename cleans the temp file up", func(t *testing.T) {
		// Make os.Rename fail without relying on permission bits, so the
		// case is exercised as root too (the container runs privileged
		// enough to ignore mode 0500): renaming a regular file over a
		// directory is EISDIR/ENOTDIR for every uid.
		dir := stateDir(t, true)
		if err := os.Mkdir(filepath.Join(dir, MarkerName), 0o700); err != nil {
			t.Fatal(err)
		}

		err := WriteMarker(dir, Marker{SambaVersion: "4.24.6", InitializedAt: "2026-08-16T10:00:00Z", LastMode: "run"})
		if err == nil {
			t.Fatal("expected an error when the marker path is a directory")
		}
		var r *config.Refusal
		if !errors.As(err, &r) {
			t.Fatalf("expected *config.Refusal, got %T: %v", err, err)
		}

		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), MarkerName+".tmp") {
				t.Errorf("failed rename left the temp file %q behind", e.Name())
			}
		}
	})

	t.Run("directory not writable keeps the previous marker intact", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: permission bits do not block writes")
		}
		dir := stateDir(t, true)
		first := Marker{SambaVersion: "4.24.6", InitializedAt: "2026-08-16T10:00:00Z", LastMode: "provision"}
		if err := WriteMarker(dir, first); err != nil {
			t.Fatalf("WriteMarker(first): %v", err)
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		err := WriteMarker(dir, Marker{SambaVersion: "4.24.10", InitializedAt: "2026-08-17T11:00:00Z", LastMode: "run"})
		if err == nil {
			t.Fatal("expected an error writing into a read-only directory")
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}

		obs, obsErr := Observe(dir)
		if obsErr != nil {
			t.Fatalf("Observe: %v", obsErr)
		}
		if obs.Marker == nil || *obs.Marker != first {
			t.Errorf("Marker = %+v, want the untouched original %+v", obs.Marker, first)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() != MarkerName && e.Name() != "private" {
				t.Errorf("failed write left %q behind", e.Name())
			}
		}
	})
}

func TestObserveCorruptMarker(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "truncated json", content: `{"samba_version": "4.24.6"`},
		{name: "not json at all", content: "this volume was initialized by hand\n"},
		{name: "wrong type for field", content: `{"samba_version": 4.24}`},
		{name: "empty file", content: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := stateDir(t, true)
			if err := os.WriteFile(filepath.Join(dir, MarkerName), []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			obs, err := Observe(dir)
			if err == nil {
				t.Fatalf("expected an error for a corrupt marker, got observation %+v", obs)
			}
			var r *config.Refusal
			if !errors.As(err, &r) {
				t.Fatalf("expected *config.Refusal, got %T: %v", err, err)
			}
			if r.Code == 0 {
				t.Errorf("refusal must carry a non-zero exit code")
			}
			if !strings.Contains(r.Msg, MarkerName) {
				t.Errorf("message %q does not name the marker file", r.Msg)
			}
			if strings.Contains(r.Msg, "\n") {
				t.Errorf("message must be a single line, got %q", r.Msg)
			}
		})
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"4.24.6", "4.24.6", 0},
		{"4.24.6", "4.24.10", -1},  // numeric, not lexicographic
		{"4.24.10", "4.24.6", 1},   // numeric, not lexicographic
		{"4.24.10", "4.24.9", 1},   // the plan's worked example
		{"4.24.9", "4.24.10", -1},  //
		{"4.23.99", "4.24.0", -1},  // minor dominates patch
		{"4.24.0", "4.23.99", 1},   //
		{"3.99.99", "4.0.0", -1},   // major dominates minor
		{"4.0.0", "3.99.99", 1},    //
		{"4.24.06", "4.24.6", 0},   // leading zeros are numeric noise
		{" 4.24.6 ", "4.24.6", 0},  // surrounding whitespace tolerated
		{"10.0.0", "9.99.99", 1},   // multi-digit major
		{"0.0.0", "0.0.0", 0},      //
		{"4.24.6", "10.24.6", -1},  //
		{"4.24.6", "4.240.6", -1},  //
		{"4.240.6", "4.24.6", 1},   //
		{"4.24.600", "4.24.6", 1},  //
		{"4.24.6", "4.24.600", -1}, //
	}
	for _, tc := range tests {
		t.Run(tc.a+"_vs_"+tc.b, func(t *testing.T) {
			got, err := CompareVersions(tc.a, tc.b)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
			// Antisymmetry: comparing the other way round must negate.
			back, err := CompareVersions(tc.b, tc.a)
			if err != nil {
				t.Fatalf("unexpected error on reverse comparison: %v", err)
			}
			if back != -tc.want {
				t.Errorf("CompareVersions(%q, %q) = %d, want %d", tc.b, tc.a, back, -tc.want)
			}
		})
	}
}

func TestCompareVersionsMalformed(t *testing.T) {
	malformed := []string{
		"",
		"4.24",
		"4",
		"4.24.6.1",
		"4.24.x",
		"v4.24.6",
		"4.24.6-rc1",
		"4..6",
		"4.-1.6",
		"four.twenty.six",
	}
	for _, bad := range malformed {
		t.Run("bad_a_"+bad, func(t *testing.T) {
			if _, err := CompareVersions(bad, "4.24.6"); err == nil {
				t.Errorf("CompareVersions(%q, \"4.24.6\") = nil error, want an error", bad)
			}
		})
		t.Run("bad_b_"+bad, func(t *testing.T) {
			if _, err := CompareVersions("4.24.6", bad); err == nil {
				t.Errorf("CompareVersions(\"4.24.6\", %q) = nil error, want an error", bad)
			}
		})
	}
}

func TestCompareVersionsErrorNamesTheValue(t *testing.T) {
	_, err := CompareVersions("4.24.x", "4.24.6")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "4.24.x") {
		t.Errorf("error %q does not name the malformed value", err.Error())
	}
}
