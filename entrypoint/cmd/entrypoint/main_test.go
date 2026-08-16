package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/config"
	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/modes"
	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/run"
)

// fakeRunner stands in for the process seam. Every test in this file stops
// before a real program would be executed, so a call to it is a test failure
// rather than an expected event.
type fakeRunner struct{ t *testing.T }

func (f fakeRunner) Run(_ context.Context, name string, args ...string) error {
	f.t.Fatalf("unexpected Run(%q, %v)", name, args)
	return nil
}

func (f fakeRunner) Start(_ context.Context, name string, args ...string) (run.Proc, error) {
	f.t.Fatalf("unexpected Start(%q, %v)", name, args)
	return nil, nil
}

// testApp wires an app against a temporary state directory and an
// environment the test controls entirely.
func testApp(t *testing.T, env map[string]string) (*app, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	dir := t.TempDir()
	return &app{
		getenv:       func(k string) string { return env[k] },
		stdout:       &stdout,
		stderr:       &stderr,
		stateDir:     dir,
		smbConfPath:  filepath.Join(dir, "smb.conf"),
		imageVersion: "4.24.6",
		newRunner:    func(_, _ io.Writer) run.Runner { return fakeRunner{t: t} },
	}, &stdout, &stderr
}

func TestUnknownArgumentIsAConfigurationError(t *testing.T) {
	a, _, stderr := testApp(t, nil)

	if code := a.run([]string{"start-the-thing"}); code != config.CodeConfigError {
		t.Fatalf("exit code = %d, want %d", code, config.CodeConfigError)
	}
	if got := stderr.String(); !strings.HasPrefix(got, "ERROR: ") ||
		!strings.Contains(got, healthcheckCommand) {
		t.Fatalf("stderr = %q, want an ERROR line naming %q", got, healthcheckCommand)
	}
}

func TestVersionFlagReportsTheInjectedVersion(t *testing.T) {
	a, stdout, _ := testApp(t, nil)

	if code := a.run([]string{versionFlag}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := stdout.String(); !strings.Contains(got, "4.24.6") {
		t.Fatalf("stdout = %q, want it to name the injected version", got)
	}
}

func TestHealthcheckIsUnhealthyWhenTheDCHasNotBeenInitialized(t *testing.T) {
	a, _, stderr := testApp(t, nil)

	// Docker reads any non-zero code as unhealthy; the refusal codes must
	// never leak onto this path, so the exact value matters.
	if code := a.run([]string{healthcheckCommand}); code != exitUnhealthy {
		t.Fatalf("exit code = %d, want %d", code, exitUnhealthy)
	}
	if got := stderr.String(); !strings.Contains(got, "ERROR: ") || !strings.Contains(got, "smb.conf") {
		t.Fatalf("stderr = %q, want an ERROR line naming the configuration file", got)
	}
}

func TestRunModeWithoutStateExitsWithTheAbsentStateCode(t *testing.T) {
	a, _, stderr := testApp(t, map[string]string{"SAMBA_MODE": "run"})

	if code := a.run(nil); code != config.CodeStateAbsent {
		t.Fatalf("exit code = %d, want %d", code, config.CodeStateAbsent)
	}
	if got := stderr.String(); !strings.Contains(got, "SAMBA_MODE=provision") {
		t.Fatalf("stderr = %q, want the remedy naming an initialization mode", got)
	}
}

func TestProvisionOverExistingStateExitsWithTheStateExistsCode(t *testing.T) {
	a, _, stderr := testApp(t, map[string]string{
		"SAMBA_MODE":                "provision",
		"SAMBA_REALM":               "AD.EXAMPLE.TEST",
		"SAMBA_ADMIN_PASSWORD_FILE": "/secrets/adminpass",
	})
	writeState(t, a.stateDir)

	if code := a.run(nil); code != config.CodeStateExists {
		t.Fatalf("exit code = %d, want %d", code, config.CodeStateExists)
	}
	if got := stderr.String(); !strings.Contains(got, "SAMBA_MODE=run") {
		t.Fatalf("stderr = %q, want the remedy naming run mode", got)
	}
}

func TestPlainPasswordInTheEnvironmentIsRefusedBeforeAnythingElse(t *testing.T) {
	a, _, stderr := testApp(t, map[string]string{"SAMBA_ADMIN_PASSWORD": "hunter2"})

	if code := a.run(nil); code != config.CodeConfigError {
		t.Fatalf("exit code = %d, want %d", code, config.CodeConfigError)
	}
	got := stderr.String()
	if !strings.Contains(got, "SAMBA_ADMIN_PASSWORD_FILE") {
		t.Fatalf("stderr = %q, want the remedy naming the _FILE variant", got)
	}
	if strings.Contains(got, "hunter2") {
		t.Fatalf("stderr leaked the password: %q", got)
	}
}

func TestChronyStateDirIsCreatedOnlyForPlansThatStartTheDaemon(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chrony bool
		plan   modes.Plan
		want   bool
	}{
		{"start plan with chrony on", true, modes.Plan{Kind: modes.ActStart}, true},
		{"provision plan with chrony on", true, modes.Plan{Kind: modes.ActProvision}, true},
		{"join plan with chrony on", true, modes.Plan{Kind: modes.ActJoin}, true},
		{"dbcheck-then-start plan with chrony on", true, modes.Plan{Kind: modes.ActDBCheckThenStart}, true},
		{"start plan with chrony off", false, modes.Plan{Kind: modes.ActStart}, false},
		{"maintenance never starts a daemon", true, modes.Plan{Kind: modes.ActMaintenance}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _ := testApp(t, nil)
			cfg := &config.Config{Chrony: tc.chrony}

			if ref := a.ensureChronyStateDir(cfg, tc.plan); ref != nil {
				t.Fatalf("unexpected refusal: %v", ref)
			}
			_, err := os.Stat(filepath.Join(a.stateDir, chronyDirName))
			if got := err == nil; got != tc.want {
				t.Fatalf("directory exists = %v, want %v (stat error %v)", got, tc.want, err)
			}
		})
	}
}

// writeState makes dir look like an initialized samba volume.
func writeState(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "private"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "private", "sam.ldb"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write sam.ldb: %v", err)
	}
}
