// Command entrypoint is PID 1's payload in the samba-ad-dc image: it reads
// the SAMBA_* environment, looks at the state volume, decides what this boot
// must do, does it, and then supervises the daemons until the container is
// asked to stop.
//
// It is deliberately thin. Every decision lives in internal/modes, every
// effect in internal/run, every probe in internal/health; this file only
// wires them together and turns a typed refusal into the process exit code
// that the operator, docker and CI all read (SPEC §6.5).
//
// Two entry paths exist, distinguished by argv:
//
//	entrypoint              the container's ENTRYPOINT: start the DC
//	entrypoint healthcheck  the image's HEALTHCHECK: probe the running DC
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/config"
	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/health"
	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/modes"
	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/run"
	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/state"
)

// sambaVersion is the Samba version this image ships. The build injects it
// with -ldflags "-X main.sambaVersion=<X.Y.Z>" so the binary and the image
// can never disagree about which Samba wrote the state volume. The default
// is deliberately not a plausible version: a binary built without the flag
// must be refused by the version guard, not silently trusted.
var sambaVersion = "unset"

const (
	// stateDir is the persistent volume samba initializes and the marker
	// lives in (adaptation profile B.2).
	stateDir = "/var/lib/samba"
	// smbConfPath is where samba-tool writes the generated configuration;
	// the health check reads the realm from it.
	smbConfPath = "/etc/samba/smb.conf"
	// chronyDirName is the subdirectory of the state volume that holds
	// chronyd's drift file (see /etc/chrony/chrony.conf). It sits on the
	// volume rather than on tmpfs so the clock estimate survives a restart,
	// and it is created here because /var/lib/samba is a volume: nothing
	// baked into the image at that path would survive being mounted over.
	chronyDirName = "chrony"

	// healthcheckCommand is the single subcommand this binary accepts.
	healthcheckCommand = "healthcheck"
	// versionFlag prints the injected Samba version; the image build uses
	// it to prove the -ldflags injection worked.
	versionFlag = "--version"

	// healthcheckTimeout bounds the whole probe run. It matches the
	// HEALTHCHECK --timeout in the Dockerfile: a probe that outlives the
	// timeout would be killed by docker anyway, and being killed produces a
	// worse message than timing out here does.
	healthcheckTimeout = 10 * time.Second

	// exitUnhealthy is what a failed HEALTHCHECK returns. Docker reads 0 as
	// healthy and anything else as unhealthy, so the refusal exit codes
	// (which mean something entirely different) never appear on this path.
	exitUnhealthy = 1
)

func main() {
	os.Exit(defaultApp().run(os.Args[1:]))
}

// app carries what main needs from its environment. The paths and the
// runner factory are fields rather than constants so the wiring itself can
// be unit-tested against a temporary directory instead of a real container.
type app struct {
	getenv       func(string) string
	stdout       io.Writer
	stderr       io.Writer
	stateDir     string
	smbConfPath  string
	imageVersion string
	newRunner    func(stdout, stderr io.Writer) run.Runner
}

// defaultApp returns the wiring used inside the container.
func defaultApp() *app {
	return &app{
		getenv:       os.Getenv,
		stdout:       os.Stdout,
		stderr:       os.Stderr,
		stateDir:     stateDir,
		smbConfPath:  smbConfPath,
		imageVersion: sambaVersion,
		newRunner:    run.NewExecRunner,
	}
}

// run dispatches on argv and returns the process exit code.
func (a *app) run(args []string) int {
	switch {
	case len(args) == 0:
		return a.start()
	case len(args) == 1 && args[0] == healthcheckCommand:
		return a.healthcheck()
	case len(args) == 1 && args[0] == versionFlag:
		fmt.Fprintf(a.stdout, "samba-ad-dc entrypoint, samba %s\n", a.imageVersion)
		return 0
	}

	// An unknown argument is a configuration error like any other, and it
	// gets the same treatment: say what was wrong and what to do instead
	// (§6.5). Silently ignoring it would start a domain controller the
	// operator did not ask for.
	return a.fail(config.Refuse(config.CodeConfigError,
		"the entrypoint was called with the argument(s) %q, which it does not understand; run the image with no arguments to start the domain controller, or with the single argument %q to probe a running one",
		args, healthcheckCommand))
}

// start is the container's normal life: load, observe, decide, execute.
//
// The signal context is installed before anything else so that a stop
// arriving during a long provision reaches samba-tool too. Supervise
// installs its own handler on top; both paths lead to the same orderly
// shutdown, and the grace window there survives this context's cancellation
// on purpose.
func (a *app) start() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	cfg, err := config.Load(a.getenv)
	if err != nil {
		return a.fail(err)
	}

	obs, err := state.Observe(a.stateDir)
	if err != nil {
		return a.fail(err)
	}

	plan, ref := modes.Decide(cfg, obs, a.imageVersion)
	if ref != nil {
		return a.fail(ref)
	}

	if ref := a.ensureChronyStateDir(cfg, plan); ref != nil {
		return a.fail(ref)
	}

	runner := a.newRunner(a.stdout, a.stderr)
	if ref := run.Execute(ctx, runner, cfg, plan, a.stateDir, a.imageVersion); ref != nil {
		return a.fail(ref)
	}
	return 0
}

// healthcheck answers the image's HEALTHCHECK: it exits 0 only when DNS,
// LDAP and SMB all answered on the loopback address.
func (a *app) healthcheck() int {
	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()

	if err := health.Check(ctx, a.newRunner(a.stdout, a.stderr), a.smbConfPath); err != nil {
		fmt.Fprintf(a.stderr, "ERROR: %s\n", err)
		return exitUnhealthy
	}
	fmt.Fprintln(a.stdout, "healthy: DNS, LDAP and SMB all answered on the loopback address")
	return 0
}

// ensureChronyStateDir creates the directory chronyd keeps its drift file
// in, but only for the plans that will actually start it. /var/lib/samba is
// a volume, so this cannot be baked into the image: whatever the image holds
// at that path disappears the moment the volume is mounted over it.
func (a *app) ensureChronyStateDir(cfg *config.Config, plan modes.Plan) *config.Refusal {
	if !cfg.Chrony || plan.Kind == modes.ActMaintenance || plan.Kind == modes.ActNone {
		return nil
	}
	dir := filepath.Join(a.stateDir, chronyDirName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return config.Refuse(config.CodeRuntimeFailure,
			"the chrony state directory %q cannot be created (%v); mount the %s volume read-write, or set SAMBA_CHRONY=off to run without the MS-SNTP time service",
			dir, err, a.stateDir)
	}
	return nil
}

// fail prints one actionable line to stderr and returns the exit code the
// refusal carries. A plain error that is not a refusal can only come from a
// package that failed to wrap one; it is reported as a runtime failure
// rather than silently downgraded to success.
func (a *app) fail(err error) int {
	var ref *config.Refusal
	if errors.As(err, &ref) {
		fmt.Fprintf(a.stderr, "ERROR: %s\n", ref.Msg)
		return ref.Code
	}
	fmt.Fprintf(a.stderr, "ERROR: %s\n", err)
	return config.CodeRuntimeFailure
}
