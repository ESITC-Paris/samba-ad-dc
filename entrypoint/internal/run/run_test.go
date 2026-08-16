package run

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/config"
	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/modes"
	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/state"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// call is one shell-out recorded by the fake runner.
type call struct {
	kind string // "run" (waited) or "start" (daemon)
	name string
	args []string
}

// key names a call the way the fakes are scripted: binary plus its first
// argument, so "samba-tool domain ..." and "samba-tool dbcheck" are distinct.
func key(name string, args []string) string {
	if len(args) == 0 {
		return name
	}
	return name + " " + args[0]
}

// fakeProc is a controllable stand-in for a started daemon.
type fakeProc struct {
	mu       sync.Mutex
	sigs     []os.Signal
	done     chan error
	once     sync.Once
	onSignal func() // observation point for shutdown ordering
}

// exitingProc returns a process that has already exited with err.
func exitingProc(err error) *fakeProc {
	p := &fakeProc{done: make(chan error, 1)}
	p.done <- err
	return p
}

// liveProc returns a process that runs until it is signaled.
func liveProc() *fakeProc { return &fakeProc{done: make(chan error, 1)} }

func (p *fakeProc) Signal(sig os.Signal) error {
	p.mu.Lock()
	p.sigs = append(p.sigs, sig)
	hook := p.onSignal
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	p.once.Do(func() { p.done <- nil })
	return nil
}

func (p *fakeProc) Wait() error { return <-p.done }

func (p *fakeProc) signals() []os.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]os.Signal(nil), p.sigs...)
}

// fakeRunner records every shell-out and returns scripted results.
type fakeRunner struct {
	mu       sync.Mutex
	calls    []call
	runErr   map[string]error // keyed by key()
	startErr map[string]error // keyed by binary name
	procs    map[string]Proc  // keyed by binary name
	hook     func(c call)     // observation point, runs before the result
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		runErr:   map[string]error{},
		startErr: map[string]error{},
		procs:    map[string]Proc{},
	}
}

func (f *fakeRunner) record(c call) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if f.hook != nil {
		f.hook(c)
	}
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) error {
	f.record(call{kind: "run", name: name, args: args})
	return f.runErr[key(name, args)]
}

func (f *fakeRunner) Start(ctx context.Context, name string, args ...string) (Proc, error) {
	f.record(call{kind: "start", name: name, args: args})
	if err := f.startErr[name]; err != nil {
		return nil, err
	}
	p, ok := f.procs[name]
	if !ok {
		p = exitingProc(nil)
		f.procs[name] = p
	}
	return p, nil
}

// names renders the recorded calls as "kind:binary" for order assertions.
func (f *fakeRunner) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.kind+":"+c.name)
	}
	return out
}

// argsOf returns the arguments of the first call to the named binary.
func (f *fakeRunner) argsOf(t *testing.T, kind, name string) []string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.kind == kind && c.name == name {
			return c.args
		}
	}
	t.Fatalf("no %s call to %q recorded (calls: %v)", kind, name, f.calls)
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

const testImageVersion = "4.24.6"

// testSecret is the sentinel that must never reach a log or an error.
const testSecret = "hunter2"

// newTestExecutor wires an executor with fake binaries, a temp filesystem
// root, a fixed clock and a captured log.
func newTestExecutor(t *testing.T, r Runner) (*Executor, *bytes.Buffer) {
	t.Helper()
	logBuf := &bytes.Buffer{}
	e := New(r)
	e.Root = t.TempDir()
	e.Log = logBuf
	e.Now = func() time.Time { return time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC) }
	e.SMBConfPath = filepath.Join(t.TempDir(), "smb.conf")
	// Never touch the real signal machinery from a unit test.
	e.Notify = func(c chan<- os.Signal, _ ...os.Signal) {}
	e.Stop = func(c chan<- os.Signal) {}
	return e, logBuf
}

// secretFile writes a password file and returns its path.
func secretFile(t *testing.T, secret string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// stateDirWith creates a state directory, optionally holding a marker.
func stateDirWith(t *testing.T, marker *state.Marker) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "private", "sam.ldb"), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if marker != nil {
		if err := state.WriteMarker(dir, *marker); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// readMarker returns the marker in dir, or nil when there is none.
func readMarker(t *testing.T, dir string) *state.Marker {
	t.Helper()
	obs, err := state.Observe(dir)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return obs.Marker
}

// hasArg reports whether args contains exactly want.
func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// provisionConfig is a config that reaches the provision path.
func provisionConfig(t *testing.T) *config.Config {
	return &config.Config{
		Mode: config.ModeProvision, Realm: "AD.EXAMPLE.COM", Domain: "AD",
		AdminPasswordFile: secretFile(t, testSecret), JoinUsername: "Administrator",
		DNSBackend: config.DNSBackendInternal, FunctionLevel: "2016",
		LogLevel: 1, Chrony: true, MaintenanceOp: config.MaintenanceCheck,
	}
}

// joinConfig is a config that reaches the join path.
func joinConfig(t *testing.T) *config.Config {
	return &config.Config{
		Mode: config.ModeJoin, Realm: "AD.EXAMPLE.COM", Domain: "AD",
		JoinPasswordFile: secretFile(t, testSecret), JoinUsername: "joiner",
		DNSBackend: config.DNSBackendInternal, FunctionLevel: "2016",
		LogLevel: 1, Chrony: true, MaintenanceOp: config.MaintenanceCheck,
	}
}

// runConfig is a config for a start-only or maintenance run.
func runConfig(mode config.Mode) *config.Config {
	return &config.Config{
		Mode: mode, Realm: "AD.EXAMPLE.COM", Domain: "AD",
		JoinUsername: "Administrator", DNSBackend: config.DNSBackendInternal,
		FunctionLevel: "2016", LogLevel: 3, Chrony: true,
		MaintenanceOp: config.MaintenanceCheck,
	}
}

// ---------------------------------------------------------------------------
// Execute: initialization paths
// ---------------------------------------------------------------------------

func TestExecuteProvisionInitializesThenStartsDaemons(t *testing.T) {
	r := newFakeRunner()
	r.procs["chronyd"] = liveProc()
	r.procs["samba"] = exitingProc(nil)

	e, logBuf := newTestExecutor(t, r)
	dir := t.TempDir()

	// Record whether the marker already existed when the daemons started:
	// initialization must be recorded before anything is served.
	markerAtFirstStart := false
	seenStart := false
	r.hook = func(c call) {
		if c.kind == "start" && !seenStart {
			seenStart = true
			_, err := os.Stat(filepath.Join(dir, state.MarkerName))
			markerAtFirstStart = err == nil
		}
	}

	if ref := e.Execute(context.Background(), provisionConfig(t), modes.Plan{Kind: modes.ActProvision}, dir, testImageVersion); ref != nil {
		t.Fatalf("Execute: unexpected refusal %d: %s", ref.Code, ref.Msg)
	}

	want := []string{"run:samba-tool", "start:chronyd", "start:samba"}
	if got := r.names(); !equalStrings(got, want) {
		t.Fatalf("call order = %v, want %v", got, want)
	}
	if !markerAtFirstStart {
		t.Error("daemons started before the marker was written")
	}

	m := readMarker(t, dir)
	if m == nil {
		t.Fatal("no marker written after a successful provision")
	}
	if m.SambaVersion != testImageVersion || m.LastMode != "provision" {
		t.Errorf("marker = %+v, want version %q and last_mode provision", m, testImageVersion)
	}
	if m.InitializedAt != "2026-08-16T10:00:00Z" {
		t.Errorf("InitializedAt = %q, want the injected clock value", m.InitializedAt)
	}

	args := r.argsOf(t, "run", "samba-tool")
	for _, want := range []string{
		"domain", "provision", "--server-role=dc", "--use-rfc2307",
		"--dns-backend=SAMBA_INTERNAL", "--realm=AD.EXAMPLE.COM",
		"--domain=AD", "--function-level=2016",
		"--option=dns update command = /usr/sbin/samba_dnsupdate --use-samba-tool",
		"--adminpass=" + testSecret,
	} {
		if !hasArg(args, want) {
			t.Errorf("provision args %v missing %q", args, want)
		}
	}
	if hasArg(args, "--option=dns forwarder=") {
		t.Errorf("empty forwarder must not be passed: %v", args)
	}
	// The executor narrates the step; the runner renders the command line
	// (redacted, see TestExecRunnerLogsRedactedCommands). Neither may echo
	// the password.
	if strings.Contains(logBuf.String(), testSecret) {
		t.Errorf("the log leaked the admin password:\n%s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "provision") {
		t.Errorf("the log does not narrate the provision step:\n%s", logBuf.String())
	}
}

func TestExecuteProvisionPassesDNSForwarder(t *testing.T) {
	r := newFakeRunner()
	e, _ := newTestExecutor(t, r)
	cfg := provisionConfig(t)
	cfg.DNSForwarder = "10.0.0.53"

	if ref := e.Execute(context.Background(), cfg, modes.Plan{Kind: modes.ActProvision}, t.TempDir(), testImageVersion); ref != nil {
		t.Fatalf("unexpected refusal: %s", ref.Msg)
	}
	args := r.argsOf(t, "run", "samba-tool")
	if !hasArg(args, "--option=dns forwarder=10.0.0.53") {
		t.Errorf("provision args %v missing the forwarder option", args)
	}
}

func TestExecuteFailedProvisionWritesNoMarker(t *testing.T) {
	r := newFakeRunner()
	r.runErr["samba-tool domain"] = errors.New("provision failed: domain already exists")

	e, _ := newTestExecutor(t, r)
	dir := t.TempDir()

	ref := e.Execute(context.Background(), provisionConfig(t), modes.Plan{Kind: modes.ActProvision}, dir, testImageVersion)
	if ref == nil {
		t.Fatal("expected a refusal when provision fails")
	}
	if ref.Code != config.CodeRuntimeFailure {
		t.Errorf("code = %d, want %d", ref.Code, config.CodeRuntimeFailure)
	}
	if _, err := os.Stat(filepath.Join(dir, state.MarkerName)); !os.IsNotExist(err) {
		t.Error("a failed provision wrote a marker; the next start would skip initialization")
	}
	for _, n := range r.names() {
		if strings.HasPrefix(n, "start:") {
			t.Errorf("daemons started after a failed provision: %v", r.names())
		}
	}
}

func TestExecuteJoinInitializesThenStartsDaemons(t *testing.T) {
	r := newFakeRunner()
	r.procs["chronyd"] = liveProc()
	r.procs["samba"] = exitingProc(nil)

	e, logBuf := newTestExecutor(t, r)
	dir := t.TempDir()
	// samba-tool domain join writes this file; the fake does not, so seed it.
	if err := os.WriteFile(e.SMBConfPath, []byte("[global]\n\trealm = AD.EXAMPLE.COM\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if ref := e.Execute(context.Background(), joinConfig(t), modes.Plan{Kind: modes.ActJoin}, dir, testImageVersion); ref != nil {
		t.Fatalf("Execute: unexpected refusal %d: %s", ref.Code, ref.Msg)
	}

	want := []string{"run:samba-tool", "start:chronyd", "start:samba"}
	if got := r.names(); !equalStrings(got, want) {
		t.Fatalf("call order = %v, want %v", got, want)
	}

	args := r.argsOf(t, "run", "samba-tool")
	for _, want := range []string{
		"domain", "join", "AD.EXAMPLE.COM", "DC", "-Ujoiner",
		"--dns-backend=SAMBA_INTERNAL", "--password=" + testSecret,
	} {
		if !hasArg(args, want) {
			t.Errorf("join args %v missing %q", args, want)
		}
	}

	conf, err := os.ReadFile(e.SMBConfPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(conf), dnsUpdateCommand) {
		t.Errorf("join did not add the dns update command to smb.conf:\n%s", conf)
	}

	m := readMarker(t, dir)
	if m == nil || m.LastMode != "join" || m.SambaVersion != testImageVersion {
		t.Errorf("marker = %+v, want version %q and last_mode join", m, testImageVersion)
	}
	if strings.Contains(logBuf.String(), testSecret) {
		t.Errorf("the command log leaked the join password:\n%s", logBuf.String())
	}
}

func TestExecuteFailedJoinWritesNoMarker(t *testing.T) {
	r := newFakeRunner()
	r.runErr["samba-tool domain"] = errors.New("join failed: no writable DC found")
	e, _ := newTestExecutor(t, r)
	dir := t.TempDir()

	ref := e.Execute(context.Background(), joinConfig(t), modes.Plan{Kind: modes.ActJoin}, dir, testImageVersion)
	if ref == nil || ref.Code != config.CodeRuntimeFailure {
		t.Fatalf("refusal = %v, want code %d", ref, config.CodeRuntimeFailure)
	}
	if _, err := os.Stat(filepath.Join(dir, state.MarkerName)); !os.IsNotExist(err) {
		t.Error("a failed join wrote a marker")
	}
}

func TestExecuteMissingSecretFileRefusesWithCode11(t *testing.T) {
	r := newFakeRunner()
	e, _ := newTestExecutor(t, r)
	cfg := provisionConfig(t)
	cfg.AdminPasswordFile = filepath.Join(t.TempDir(), "absent")

	ref := e.Execute(context.Background(), cfg, modes.Plan{Kind: modes.ActProvision}, t.TempDir(), testImageVersion)
	if ref == nil || ref.Code != config.CodeSecretError {
		t.Fatalf("refusal = %v, want code %d", ref, config.CodeSecretError)
	}
	if len(r.names()) != 0 {
		t.Errorf("samba-tool ran without a password: %v", r.names())
	}
}

// ---------------------------------------------------------------------------
// Execute: start paths
// ---------------------------------------------------------------------------

func TestExecuteStartRunsNoSambaTool(t *testing.T) {
	r := newFakeRunner()
	r.procs["chronyd"] = liveProc()
	r.procs["samba"] = exitingProc(nil)
	e, _ := newTestExecutor(t, r)
	marker := state.Marker{SambaVersion: testImageVersion, InitializedAt: "2026-01-01T00:00:00Z", LastMode: "provision"}
	dir := stateDirWith(t, &marker)

	if ref := e.Execute(context.Background(), runConfig(config.ModeRun), modes.Plan{Kind: modes.ActStart}, dir, testImageVersion); ref != nil {
		t.Fatalf("unexpected refusal: %s", ref.Msg)
	}
	want := []string{"start:chronyd", "start:samba"}
	if got := r.names(); !equalStrings(got, want) {
		t.Fatalf("call order = %v, want %v (a restart must not touch state)", got, want)
	}
	if m := readMarker(t, dir); m == nil || *m != marker {
		t.Errorf("marker = %+v, want it untouched %+v", m, marker)
	}
}

func TestExecuteDBCheckThenStartUpgradesMarker(t *testing.T) {
	r := newFakeRunner()
	r.procs["chronyd"] = liveProc()
	r.procs["samba"] = exitingProc(nil)
	e, _ := newTestExecutor(t, r)
	dir := stateDirWith(t, &state.Marker{SambaVersion: "4.24.5", InitializedAt: "2026-01-01T00:00:00Z", LastMode: "provision"})

	if ref := e.Execute(context.Background(), runConfig(config.ModeRun), modes.Plan{Kind: modes.ActDBCheckThenStart}, dir, testImageVersion); ref != nil {
		t.Fatalf("unexpected refusal: %s", ref.Msg)
	}
	want := []string{"run:samba-tool", "start:chronyd", "start:samba"}
	if got := r.names(); !equalStrings(got, want) {
		t.Fatalf("call order = %v, want %v", got, want)
	}
	args := r.argsOf(t, "run", "samba-tool")
	if !equalStrings(args, []string{"dbcheck"}) {
		t.Errorf("dbcheck args = %v, want [dbcheck] (no --fix on the upgrade path)", args)
	}
	m := readMarker(t, dir)
	if m == nil || m.SambaVersion != testImageVersion {
		t.Errorf("marker = %+v, want the image version %q after a successful check", m, testImageVersion)
	}
}

func TestExecuteDBCheckThenStartAdoptsForeignVolume(t *testing.T) {
	r := newFakeRunner()
	r.procs["chronyd"] = liveProc()
	r.procs["samba"] = exitingProc(nil)
	e, logBuf := newTestExecutor(t, r)
	dir := stateDirWith(t, nil)

	plan := modes.Plan{Kind: modes.ActDBCheckThenStart, AdoptMarker: true}
	if ref := e.Execute(context.Background(), runConfig(config.ModeRun), plan, dir, testImageVersion); ref != nil {
		t.Fatalf("unexpected refusal: %s", ref.Msg)
	}
	m := readMarker(t, dir)
	if m == nil || m.SambaVersion != testImageVersion {
		t.Fatalf("marker = %+v, want an adopted marker carrying %q", m, testImageVersion)
	}
	if !strings.Contains(strings.ToLower(logBuf.String()), "adopt") {
		t.Errorf("adoption was not announced in the log:\n%s", logBuf.String())
	}
}

func TestExecuteDBCheckFailureRefusesAndStartsNothing(t *testing.T) {
	r := newFakeRunner()
	r.runErr["samba-tool dbcheck"] = errors.New("dbcheck: 3 errors found")
	e, _ := newTestExecutor(t, r)
	marker := state.Marker{SambaVersion: "4.24.5", InitializedAt: "2026-01-01T00:00:00Z", LastMode: "provision"}
	dir := stateDirWith(t, &marker)

	ref := e.Execute(context.Background(), runConfig(config.ModeRun), modes.Plan{Kind: modes.ActDBCheckThenStart}, dir, testImageVersion)
	if ref == nil || ref.Code != config.CodeDBCheckFailed {
		t.Fatalf("refusal = %v, want code %d", ref, config.CodeDBCheckFailed)
	}
	if got := r.names(); !equalStrings(got, []string{"run:samba-tool"}) {
		t.Errorf("calls = %v, want only the dbcheck (no daemon may start)", got)
	}
	if m := readMarker(t, dir); m == nil || *m != marker {
		t.Errorf("marker = %+v, want it left at %+v after a failed check", m, marker)
	}
}

// ---------------------------------------------------------------------------
// Execute: maintenance
// ---------------------------------------------------------------------------

func TestExecuteMaintenanceNeverStartsDaemons(t *testing.T) {
	tests := []struct {
		name     string
		repair   bool
		wantArgs []string
	}{
		{name: "check", repair: false, wantArgs: []string{"dbcheck"}},
		{name: "repair", repair: true, wantArgs: []string{"dbcheck", "--fix", "--yes"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newFakeRunner()
			e, _ := newTestExecutor(t, r)
			marker := state.Marker{SambaVersion: testImageVersion, InitializedAt: "2026-01-01T00:00:00Z", LastMode: "provision"}
			dir := stateDirWith(t, &marker)

			plan := modes.Plan{Kind: modes.ActMaintenance, Repair: tc.repair}
			if ref := e.Execute(context.Background(), runConfig(config.ModeMaintenance), plan, dir, testImageVersion); ref != nil {
				t.Fatalf("unexpected refusal: %s", ref.Msg)
			}
			if got := r.names(); !equalStrings(got, []string{"run:samba-tool"}) {
				t.Fatalf("calls = %v, want only the dbcheck", got)
			}
			if got := r.argsOf(t, "run", "samba-tool"); !equalStrings(got, tc.wantArgs) {
				t.Errorf("args = %v, want %v", got, tc.wantArgs)
			}
			if m := readMarker(t, dir); m == nil || *m != marker {
				t.Errorf("maintenance modified the marker: %+v", m)
			}
		})
	}
}

func TestExecuteMaintenanceFailureRefusesWith23(t *testing.T) {
	r := newFakeRunner()
	r.runErr["samba-tool dbcheck"] = errors.New("dbcheck: 3 errors found")
	e, _ := newTestExecutor(t, r)
	dir := stateDirWith(t, nil)

	ref := e.Execute(context.Background(), runConfig(config.ModeMaintenance), modes.Plan{Kind: modes.ActMaintenance}, dir, testImageVersion)
	if ref == nil || ref.Code != config.CodeDBCheckFailed {
		t.Fatalf("refusal = %v, want code %d", ref, config.CodeDBCheckFailed)
	}
	if !strings.Contains(ref.Msg, "dbcheck") {
		t.Errorf("message %q does not name the failing check", ref.Msg)
	}
}

// TestExecuteUndecidedPlanRefuses covers modes.ActNone, the zero value of
// ActionKind: a Plan that was never decided — a zero Plan reaching Execute
// through a missed error check — must stop the container, not silently take
// whichever action happens to be first.
func TestExecuteUndecidedPlanRefuses(t *testing.T) {
	r := newFakeRunner()
	e, _ := newTestExecutor(t, r)
	ref := e.Execute(context.Background(), runConfig(config.ModeRun), modes.Plan{Kind: modes.ActNone}, t.TempDir(), testImageVersion)
	if ref == nil {
		t.Fatal("an undecided plan must refuse, not start daemons")
	}
	if ref.Code != config.CodeConfigError {
		t.Errorf("code = %d, want %d", ref.Code, config.CodeConfigError)
	}
	if !strings.Contains(ref.Msg, "internal") {
		t.Errorf("message %q does not mark this as an internal error", ref.Msg)
	}
	if len(r.names()) != 0 {
		t.Errorf("calls = %v, want none", r.names())
	}
}

// TestExecuteUnknownPlanRefuses covers every other ActionKind Execute does
// not name. Falling through to a default action would be the worst possible
// failure mode, so the switch must refuse instead.
func TestExecuteUnknownPlanRefuses(t *testing.T) {
	r := newFakeRunner()
	e, _ := newTestExecutor(t, r)
	ref := e.Execute(context.Background(), runConfig(config.ModeRun), modes.Plan{Kind: modes.ActionKind(99)}, t.TempDir(), testImageVersion)
	if ref == nil {
		t.Fatal("an unknown action must refuse, not start daemons")
	}
	if ref.Code != config.CodeConfigError {
		t.Errorf("code = %d, want %d", ref.Code, config.CodeConfigError)
	}
	if !strings.Contains(ref.Msg, "internal") {
		t.Errorf("message %q does not mark this as an internal error", ref.Msg)
	}
	if len(r.names()) != 0 {
		t.Errorf("calls = %v, want none", r.names())
	}
}

// ---------------------------------------------------------------------------
// Supervise
// ---------------------------------------------------------------------------

func TestSuperviseStartsDaemonsWithTheContractCommands(t *testing.T) {
	r := newFakeRunner()
	r.procs["chronyd"] = liveProc()
	r.procs["samba"] = exitingProc(nil)
	e, _ := newTestExecutor(t, r)
	cfg := runConfig(config.ModeRun) // LogLevel 3

	if ref := e.Supervise(context.Background(), cfg); ref != nil {
		t.Fatalf("unexpected refusal: %s", ref.Msg)
	}
	if got := r.argsOf(t, "start", "chronyd"); !equalStrings(got, []string{"-d", "-x", "-f", "/etc/chrony/chrony.conf"}) {
		t.Errorf("chronyd args = %v", got)
	}
	if got := r.argsOf(t, "start", "samba"); !equalStrings(got, []string{"--foreground", "--no-process-group", "--debug-stdout", "-d", "3"}) {
		t.Errorf("samba args = %v", got)
	}
}

func TestSuperviseWithoutChrony(t *testing.T) {
	r := newFakeRunner()
	r.procs["samba"] = exitingProc(nil)
	e, _ := newTestExecutor(t, r)
	cfg := runConfig(config.ModeRun)
	cfg.Chrony = false

	if ref := e.Supervise(context.Background(), cfg); ref != nil {
		t.Fatalf("unexpected refusal: %s", ref.Msg)
	}
	if got := r.names(); !equalStrings(got, []string{"start:samba"}) {
		t.Errorf("calls = %v, want samba only when SAMBA_CHRONY=off", got)
	}
}

func TestSuperviseCreatesRuntimeDirectories(t *testing.T) {
	r := newFakeRunner()
	r.procs["samba"] = exitingProc(nil)
	e, _ := newTestExecutor(t, r)

	if ref := e.Supervise(context.Background(), runConfig(config.ModeRun)); ref != nil {
		t.Fatalf("unexpected refusal: %s", ref.Msg)
	}
	for _, dir := range []string{"run/samba", "run/lock/samba", "run/chrony"} {
		info, err := os.Stat(filepath.Join(e.Root, dir))
		if err != nil {
			t.Errorf("runtime directory %q was not created: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%q is not a directory", dir)
		}
	}
}

func TestSuperviseSambaFailureRefusesWith30(t *testing.T) {
	r := newFakeRunner()
	chrony := liveProc()
	r.procs["chronyd"] = chrony
	r.procs["samba"] = exitingProc(errors.New("exit status 1"))
	e, _ := newTestExecutor(t, r)

	ref := e.Supervise(context.Background(), runConfig(config.ModeRun))
	if ref == nil || ref.Code != config.CodeRuntimeFailure {
		t.Fatalf("refusal = %v, want code %d", ref, config.CodeRuntimeFailure)
	}
	if len(chrony.signals()) == 0 {
		t.Error("chronyd was left running after samba failed")
	}
}

func TestSuperviseSambaStartFailureRefusesWith30(t *testing.T) {
	r := newFakeRunner()
	chrony := liveProc()
	r.procs["chronyd"] = chrony
	r.startErr["samba"] = errors.New("exec: \"samba\": executable file not found in $PATH")
	e, _ := newTestExecutor(t, r)

	ref := e.Supervise(context.Background(), runConfig(config.ModeRun))
	if ref == nil || ref.Code != config.CodeRuntimeFailure {
		t.Fatalf("refusal = %v, want code %d", ref, config.CodeRuntimeFailure)
	}
	if len(chrony.signals()) == 0 {
		t.Error("chronyd was left running after samba failed to start")
	}
}

func TestSuperviseSignalStopsSambaBeforeChrony(t *testing.T) {
	r := newFakeRunner()
	chrony := liveProc()
	samba := liveProc()
	r.procs["chronyd"] = chrony
	r.procs["samba"] = samba

	e, _ := newTestExecutor(t, r)
	var sigCh chan<- os.Signal
	ready := make(chan struct{})
	e.Notify = func(c chan<- os.Signal, _ ...os.Signal) { sigCh = c; close(ready) }

	// Order is recorded by the fake processes themselves.
	var mu sync.Mutex
	var order []string
	r.hook = func(c call) {}
	chrony.onSignal = func() { mu.Lock(); order = append(order, "chronyd"); mu.Unlock() }
	samba.onSignal = func() { mu.Lock(); order = append(order, "samba"); mu.Unlock() }

	done := make(chan *config.Refusal, 1)
	go func() { done <- e.Supervise(context.Background(), runConfig(config.ModeRun)) }()

	<-ready
	// Wait until both daemons are up before asking for a shutdown.
	waitFor(t, func() bool { return len(r.names()) == 2 })
	sigCh <- syscall.SIGTERM

	select {
	case ref := <-done:
		if ref != nil {
			t.Fatalf("a clean shutdown must exit 0, got refusal %d: %s", ref.Code, ref.Msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Supervise did not return after SIGTERM")
	}

	mu.Lock()
	defer mu.Unlock()
	if !equalStrings(order, []string{"samba", "chronyd"}) {
		t.Errorf("shutdown order = %v, want samba stopped before chronyd", order)
	}
	if sigs := samba.signals(); len(sigs) == 0 || sigs[0] != syscall.SIGTERM {
		t.Errorf("samba signals = %v, want SIGTERM first", sigs)
	}
}

// TestSuperviseSignalWithRealProcesses exercises the one path fakes cannot
// cover: real fork/exec, real signal delivery and real reaping.
func TestSuperviseSignalWithRealProcesses(t *testing.T) {
	dir := t.TempDir()
	orderLog := filepath.Join(dir, "order.log")
	sleeper := func(name string) string {
		path := filepath.Join(dir, name+".sh")
		script := "#!/bin/sh\n" +
			"trap 'echo " + name + " >> " + orderLog + "; kill $pid 2>/dev/null; exit 0' TERM\n" +
			"sleep 30 & pid=$!\n" +
			"wait $pid\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// Real files, not buffers: os/exec would otherwise wire pipes whose
	// copying goroutines outlive the children.
	out, err := os.Create(filepath.Join(dir, "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	started := make(chan struct{}, 4)
	base := NewExecRunner(out, out)
	r := &countingRunner{Runner: base, started: started}

	e := New(r)
	e.Root = t.TempDir()
	e.Log = out
	e.Bin = Binaries{SambaTool: "/bin/true", Samba: sleeper("samba"), Chronyd: sleeper("chronyd"), Smbclient: "/bin/true"}
	var sigCh chan<- os.Signal
	ready := make(chan struct{})
	e.Notify = func(c chan<- os.Signal, _ ...os.Signal) { sigCh = c; close(ready) }
	e.Stop = func(chan<- os.Signal) {}

	done := make(chan *config.Refusal, 1)
	go func() { done <- e.Supervise(context.Background(), runConfig(config.ModeRun)) }()

	<-ready
	<-started // chronyd
	<-started // samba
	// Give the shells a moment to install their TERM traps.
	time.Sleep(300 * time.Millisecond)
	sigCh <- syscall.SIGTERM

	select {
	case ref := <-done:
		if ref != nil {
			t.Fatalf("clean shutdown returned refusal %d: %s", ref.Code, ref.Msg)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Supervise did not return after SIGTERM with real processes")
	}

	data, err := os.ReadFile(orderLog)
	if err != nil {
		t.Fatalf("no daemon recorded a TERM: %v", err)
	}
	got := strings.Fields(string(data))
	if !equalStrings(got, []string{"samba", "chronyd"}) {
		t.Errorf("shutdown order = %v, want [samba chronyd]", got)
	}
}

// countingRunner announces each Start on a channel.
type countingRunner struct {
	Runner
	started chan struct{}
}

func (c *countingRunner) Start(ctx context.Context, name string, args ...string) (Proc, error) {
	p, err := c.Runner.Start(ctx, name, args...)
	c.started <- struct{}{}
	return p, err
}

// ---------------------------------------------------------------------------
// smb.conf editing (pure)
// ---------------------------------------------------------------------------

func TestWithDNSUpdateCommand(t *testing.T) {
	const line = "\tdns update command = /usr/sbin/samba_dnsupdate --use-samba-tool"
	tests := []struct {
		name        string
		in          string
		want        string
		wantChanged bool
	}{
		{
			name:        "inserted at the top of an existing global section",
			in:          "[global]\n\trealm = AD.EXAMPLE.COM\n\n[netlogon]\n\tpath = /var/lib/samba/sysvol\n",
			want:        "[global]\n" + line + "\n\trealm = AD.EXAMPLE.COM\n\n[netlogon]\n\tpath = /var/lib/samba/sysvol\n",
			wantChanged: true,
		},
		{
			name:        "already present is left byte-for-byte alone",
			in:          "[global]\n\trealm = AD.EXAMPLE.COM\n\tdns update command = /usr/sbin/samba_dnsupdate --use-samba-tool\n",
			want:        "[global]\n\trealm = AD.EXAMPLE.COM\n\tdns update command = /usr/sbin/samba_dnsupdate --use-samba-tool\n",
			wantChanged: false,
		},
		{
			name:        "recognized despite odd spacing and case",
			in:          "[global]\n   DNS Update Command   =   /usr/sbin/samba_dnsupdate --use-samba-tool\n",
			want:        "[global]\n   DNS Update Command   =   /usr/sbin/samba_dnsupdate --use-samba-tool\n",
			wantChanged: false,
		},
		{
			name:        "global section header is matched case-insensitively",
			in:          "[Global]\n\trealm = AD.EXAMPLE.COM\n",
			want:        "[Global]\n" + line + "\n\trealm = AD.EXAMPLE.COM\n",
			wantChanged: true,
		},
		{
			name:        "a file without a global section gets one",
			in:          "[netlogon]\n\tpath = /var/lib/samba/sysvol\n",
			want:        "[netlogon]\n\tpath = /var/lib/samba/sysvol\n\n[global]\n" + line + "\n",
			wantChanged: true,
		},
		{
			name:        "an empty file gets a global section",
			in:          "",
			want:        "[global]\n" + line + "\n",
			wantChanged: true,
		},
		{
			name:        "a commented-out option does not count as present",
			in:          "[global]\n\t# dns update command = /usr/sbin/samba_dnsupdate\n",
			want:        "[global]\n" + line + "\n\t# dns update command = /usr/sbin/samba_dnsupdate\n",
			wantChanged: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := withDNSUpdateCommand(tc.in)
			if got != tc.want {
				t.Errorf("withDNSUpdateCommand()\n got %q\nwant %q", got, tc.want)
			}
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			// Applying it twice must be a no-op: joins repeat on restart.
			again, changedAgain := withDNSUpdateCommand(got)
			if again != got || changedAgain {
				t.Errorf("second application changed the file (idempotence broken)")
			}
		})
	}
}

func TestEnsureDNSUpdateCommandMissingFileRefuses(t *testing.T) {
	e, _ := newTestExecutor(t, newFakeRunner())
	e.SMBConfPath = filepath.Join(t.TempDir(), "absent", "smb.conf")
	ref := e.ensureDNSUpdateCommand()
	if ref == nil {
		t.Fatal("expected a refusal when the generated smb.conf is missing")
	}
	if !strings.Contains(ref.Msg, e.SMBConfPath) {
		t.Errorf("message %q does not name the file", ref.Msg)
	}
}

// ---------------------------------------------------------------------------
// runner: redaction and real execution
// ---------------------------------------------------------------------------

func TestRedactArgs(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "adminpass with an equals sign",
			in:   []string{"domain", "provision", "--adminpass=" + testSecret},
			want: []string{"domain", "provision", "--adminpass=<redacted>"},
		},
		{
			name: "password with an equals sign",
			in:   []string{"domain", "join", "--password=" + testSecret},
			want: []string{"domain", "join", "--password=<redacted>"},
		},
		{
			name: "separate value argument",
			in:   []string{"--adminpass", testSecret, "--realm=AD.EXAMPLE.COM"},
			want: []string{"--adminpass", "<redacted>", "--realm=AD.EXAMPLE.COM"},
		},
		{
			name: "trailing flag without a value",
			in:   []string{"--password"},
			want: []string{"--password"},
		},
		{
			name: "unrelated arguments are untouched",
			in:   []string{"--option=dns forwarder=10.0.0.53", "-Ujoiner"},
			want: []string{"--option=dns forwarder=10.0.0.53", "-Ujoiner"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactArgs(tc.in)
			if !equalStrings(got, tc.want) {
				t.Errorf("redactArgs(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for _, a := range got {
				if strings.Contains(a, testSecret) {
					t.Errorf("redaction left the secret in %q", a)
				}
			}
			// The input must not be mutated: the caller still needs it.
			for _, a := range tc.in {
				_ = a
			}
		})
	}
}

func TestRedactArgsDoesNotMutateInput(t *testing.T) {
	in := []string{"--adminpass=" + testSecret}
	redactArgs(in)
	if in[0] != "--adminpass="+testSecret {
		t.Errorf("redactArgs mutated its input to %q; the command would lose its password", in[0])
	}
}

func TestExecRunnerLogsRedactedCommands(t *testing.T) {
	script := filepath.Join(t.TempDir(), "ok.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	logBuf := &bytes.Buffer{}
	r := NewExecRunner(logBuf, logBuf)

	if err := r.Run(context.Background(), script, "domain", "provision", "--adminpass="+testSecret); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := logBuf.String()
	if strings.Contains(out, testSecret) {
		t.Errorf("the runner logged the secret:\n%s", out)
	}
	if !strings.Contains(out, "--adminpass=<redacted>") {
		t.Errorf("the runner did not log the redacted command:\n%s", out)
	}
	if !strings.Contains(out, "domain provision") {
		t.Errorf("the runner did not log the command:\n%s", out)
	}
}

func TestExecRunnerFailureErrorIsRedacted(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fail.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	logBuf := &bytes.Buffer{}
	r := NewExecRunner(logBuf, logBuf)

	err := r.Run(context.Background(), script, "--password="+testSecret)
	if err == nil {
		t.Fatal("expected an error from a command exiting 3")
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Errorf("the error leaked the secret: %v", err)
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Errorf("the error does not show the redacted command: %v", err)
	}
}

func TestExecRunnerStartAndWait(t *testing.T) {
	logBuf := &bytes.Buffer{}
	r := NewExecRunner(logBuf, logBuf)
	p, err := r.Start(context.Background(), "/bin/sh", "-c", "exit 0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Wait(); err != nil {
		t.Errorf("Wait: %v", err)
	}
}

func TestExecRunnerStartMissingBinary(t *testing.T) {
	logBuf := &bytes.Buffer{}
	r := NewExecRunner(logBuf, logBuf)
	if _, err := r.Start(context.Background(), filepath.Join(t.TempDir(), "nope"), "-d"); err == nil {
		t.Fatal("expected an error starting a missing binary")
	}
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// equalStrings compares two string slices element by element.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition never held")
}
