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
	kind string // "run" (waited), "start" (daemon) or "output" (read back)
	name string
	args []string
	ctx  context.Context
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
	mu         sync.Mutex
	sigs       []os.Signal
	done       chan error
	once       sync.Once
	onSignal   func() // observation point for shutdown ordering
	ignoreTerm bool   // only SIGKILL ends it
	ignoreAll  bool   // nothing ends it: the unreapable daemon
}

// exitingProc returns a process that has already exited with err.
func exitingProc(err error) *fakeProc {
	p := &fakeProc{done: make(chan error, 1)}
	p.done <- err
	return p
}

// liveProc returns a process that runs until it is signaled.
func liveProc() *fakeProc { return &fakeProc{done: make(chan error, 1)} }

// stubbornProc ignores SIGTERM the way a wedged samba does, and dies only
// when it is killed.
func stubbornProc() *fakeProc { return &fakeProc{done: make(chan error, 1), ignoreTerm: true} }

// unreapableProc never exits, whatever it is sent: the D-state process the
// shutdown must give up on instead of hanging behind.
func unreapableProc(t *testing.T) *fakeProc {
	p := &fakeProc{done: make(chan error, 1), ignoreAll: true}
	// Release the reaping goroutine when the test ends.
	t.Cleanup(p.exit)
	return p
}

func (p *fakeProc) Signal(sig os.Signal) error {
	p.mu.Lock()
	p.sigs = append(p.sigs, sig)
	hook := p.onSignal
	ignoreAll, ignoreTerm := p.ignoreAll, p.ignoreTerm
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	switch {
	case ignoreAll:
	case ignoreTerm && sig != os.Kill:
	default:
		p.exit()
	}
	return nil
}

// exit ends the process once.
func (p *fakeProc) exit() { p.once.Do(func() { p.done <- nil }) }

func (p *fakeProc) Wait() error { return <-p.done }

func (p *fakeProc) signals() []os.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]os.Signal(nil), p.sigs...)
}

// fakeRunner records every shell-out and returns scripted results.
type fakeRunner struct {
	mu        sync.Mutex
	calls     []call
	runErr    map[string]error  // keyed by key()
	startErr  map[string]error  // keyed by binary name
	procs     map[string]Proc   // keyed by binary name
	output    map[string]string // stdout, keyed by binary name
	outputErr map[string]error  // keyed by binary name
	hook      func(c call)      // observation point, runs before the result
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		runErr:   map[string]error{},
		startErr: map[string]error{},
		procs:    map[string]Proc{},
		// What testparm answers on a provisioned DC. Scripted by default
		// so that every supervision test runs the ordinary path; the
		// chrony-configuration tests override it.
		output: map[string]string{
			"testparm":       provisionedSigndDir + "\n",
			"testparm check": validTestparmDump,
		},
		outputErr: map[string]error{},
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
	f.record(call{kind: "run", name: name, args: args, ctx: ctx})
	return f.runErr[key(name, args)]
}

func (f *fakeRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	f.record(call{kind: "output", name: name, args: args, ctx: ctx})
	if k := outputKey(name, args); f.hasOutput(k) {
		return f.output[k], f.outputErr[k]
	}
	return f.output[name], f.outputErr[name]
}

// hasOutput reports whether a call-specific answer is scripted for k.
func (f *fakeRunner) hasOutput(k string) bool {
	_, out := f.output[k]
	_, err := f.outputErr[k]
	return out || err
}

// outputKey distinguishes the two testparm invocations, which ask samba two
// different questions and must be allowed two different answers: reading ONE
// parameter back (chrony's signing socket) and checking the WHOLE file (the
// SAMBA_GLOBAL_OPTIONS gate). Scripts keyed by the bare binary name still
// answer both, so the tests written before the second call existed are
// unaffected.
func outputKey(name string, args []string) string {
	for _, a := range args {
		if strings.HasPrefix(a, "--parameter-name=") {
			return name + " parameter"
		}
	}
	return name + " check"
}

func (f *fakeRunner) Start(ctx context.Context, name string, args ...string) (Proc, error) {
	f.record(call{kind: "start", name: name, args: args, ctx: ctx})
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

// startContexts returns the context each daemon was started with.
func (f *fakeRunner) startContexts() []context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []context.Context
	for _, c := range f.calls {
		if c.kind == "start" {
			out = append(out, c.ctx)
		}
	}
	return out
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

// provisionedSigndDir is samba's compile-time default for the MS-SNTP
// signing socket — the value the baked-in template carries — and
// customSigndDir stands for an smb.conf that sets `ntp signd socket
// directory` to something else. The gap between the two is the whole reason
// chrony.conf is generated rather than baked.
const (
	provisionedSigndDir = "/var/lib/samba/ntp_signd"
	customSigndDir      = "/srv/samba/ntp_signd"
)

// chronyTemplateContent is a stand-in for the file the image bakes at
// /etc/chrony/chrony.conf: the directive that gets rewritten, plus the
// absolute drift and pid paths that must survive the rewrite untouched.
const chronyTemplateContent = `# baked template
ntpsigndsocket ` + provisionedSigndDir + `
user root
driftfile /var/lib/samba/chrony/drift
pidfile /run/chrony/chronyd.pid
local stratum 10
`

// validTestparmDump is what `testparm -s -l --debug-stdout` prints for a file
// samba accepts: the configuration echoed back, and nothing before it. The
// gate reads the lines BEFORE the dump, so "nothing before it" is the part
// that matters — see testparmDiagnostics.
const validTestparmDump = "# Global parameters\n[global]\n\trealm = AD.EXAMPLE.COM\n"

// deprecatedParameterWarning is what samba prints for a parameter it still
// accepts but no longer likes — measured in the image under test:
//
//	lpcfg_do_global_parameter: WARNING: The "syslog only" option is deprecated
//
// testparm exits 0 and loads the file. A gate that read "WARNING" as a
// verdict would refuse to boot a domain controller whose configuration samba
// is perfectly happy with.
const deprecatedParameterWarning = `lpcfg_do_global_parameter: WARNING: The "syslog only" option is deprecated`

// testSecret is the sentinel that must never reach a log or an error.
const testSecret = "hunter2"

// newTestExecutor wires an executor with fake binaries, a temp filesystem
// root, a fixed clock and a captured log.
func newTestExecutor(t *testing.T, r Runner) (*Executor, *syncBuffer) {
	t.Helper()
	logBuf := &syncBuffer{}
	e := New(r)
	e.Root = t.TempDir()
	e.Log = logBuf
	e.Now = func() time.Time { return time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC) }
	e.SMBConfPath = filepath.Join(t.TempDir(), "smb.conf")
	// The chrony template is read and the effective configuration is
	// written, so both are rooted in the test's own filesystem: a unit test
	// must neither depend on the image's /etc nor write to the developer's
	// /run. The generated path sits under Root/run/chrony, the directory
	// makeRuntimeDirs creates.
	e.ChronyTemplate = filepath.Join(t.TempDir(), "chrony.conf")
	if err := os.WriteFile(e.ChronyTemplate, []byte(chronyTemplateContent), 0o644); err != nil {
		t.Fatal(err)
	}
	e.ChronyConf = filepath.Join(e.Root, "run", "chrony", "chrony.conf")
	// Never touch the real signal machinery from a unit test.
	e.Notify = func(c chan<- os.Signal, _ ...os.Signal) {}
	e.Stop = func(c chan<- os.Signal) {}
	return e, logBuf
}

// syncBuffer is a log sink that can be read while the supervisor is writing
// to it from its own goroutine. A plain bytes.Buffer is not safe for that,
// and the supervision tests do exactly that.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
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

	want := []string{"run:samba-tool", "output:testparm", "start:chronyd", "start:samba"}
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

	// The whole command line, in order, on its redacted rendering: an
	// option landing in the wrong place is a different command.
	args := r.argsOf(t, "run", "samba-tool")
	wantArgs := []string{
		"domain", "provision",
		"--server-role=dc",
		"--use-rfc2307",
		"--dns-backend=SAMBA_INTERNAL",
		"--realm=AD.EXAMPLE.COM",
		"--domain=AD",
		"--function-level=2016",
		"--option=ad dc functional level = 2016",
		"--option=dns update command = /usr/sbin/samba_dnsupdate --use-samba-tool",
		"--adminpass=<redacted>",
	}
	if got := redactArgs(args); !equalStrings(got, wantArgs) {
		t.Errorf("provision args =\n%v\nwant\n%v", got, wantArgs)
	}
	if !hasArg(args, "--adminpass="+testSecret) {
		t.Errorf("the real password did not reach samba-tool: %v", redactArgs(args))
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
	wantArgs := []string{
		"domain", "provision",
		"--server-role=dc",
		"--use-rfc2307",
		"--dns-backend=SAMBA_INTERNAL",
		"--realm=AD.EXAMPLE.COM",
		"--domain=AD",
		"--function-level=2016",
		"--option=ad dc functional level = 2016",
		"--option=dns forwarder=10.0.0.53",
		"--option=dns update command = /usr/sbin/samba_dnsupdate --use-samba-tool",
		"--adminpass=<redacted>",
	}
	if got := redactArgs(r.argsOf(t, "run", "samba-tool")); !equalStrings(got, wantArgs) {
		t.Errorf("provision args =\n%v\nwant\n%v", got, wantArgs)
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

// TestExecuteRefusalNeverEchoesTheSecret covers the last line of defence: a
// Runner that is not the ExecRunner — a future implementation, or samba-tool
// itself quoting the failing command back at us — can hand this package an
// error that still holds the password. It must not reach the refusal an
// operator sees in the container log.
func TestExecuteRefusalNeverEchoesTheSecret(t *testing.T) {
	tests := []struct {
		name string
		flag string
		plan modes.Plan
		cfg  func(*testing.T) *config.Config
	}{
		{name: "provision", flag: "--adminpass", plan: modes.Plan{Kind: modes.ActProvision}, cfg: provisionConfig},
		{name: "join", flag: "--password", plan: modes.Plan{Kind: modes.ActJoin}, cfg: joinConfig},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newFakeRunner()
			r.runErr["samba-tool domain"] = errors.New(
				"samba-tool domain " + tc.name + " " + tc.flag + "=" + testSecret + " failed: ERROR(ldb)")
			e, logBuf := newTestExecutor(t, r)

			ref := e.Execute(context.Background(), tc.cfg(t), tc.plan, t.TempDir(), testImageVersion)
			if ref == nil {
				t.Fatal("expected a refusal")
			}
			if strings.Contains(ref.Msg, testSecret) {
				t.Errorf("the refusal leaked the password: %s", ref.Msg)
			}
			if !strings.Contains(ref.Msg, "<redacted>") {
				t.Errorf("the refusal does not show the redacted flag: %s", ref.Msg)
			}
			if strings.Contains(logBuf.String(), testSecret) {
				t.Errorf("the log leaked the password:\n%s", logBuf.String())
			}
		})
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

	want := []string{"run:samba-tool", "output:testparm", "start:chronyd", "start:samba"}
	if got := r.names(); !equalStrings(got, want) {
		t.Fatalf("call order = %v, want %v", got, want)
	}

	args := r.argsOf(t, "run", "samba-tool")
	wantArgs := []string{
		"domain", "join",
		"AD.EXAMPLE.COM", "DC",
		"-Ujoiner",
		"--dns-backend=SAMBA_INTERNAL",
		"--password=<redacted>",
	}
	if got := redactArgs(args); !equalStrings(got, wantArgs) {
		t.Errorf("join args =\n%v\nwant\n%v", got, wantArgs)
	}
	if !hasArg(args, "--password="+testSecret) {
		t.Errorf("the real password did not reach samba-tool: %v", redactArgs(args))
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
	want := []string{"output:testparm", "start:chronyd", "start:samba"}
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
	want := []string{"run:samba-tool", "output:testparm", "start:chronyd", "start:samba"}
	if got := r.names(); !equalStrings(got, want) {
		t.Fatalf("call order = %v, want %v", got, want)
	}
	args := r.argsOf(t, "run", "samba-tool")
	if !equalStrings(args, []string{"dbcheck"}) {
		t.Errorf("dbcheck args = %v, want [dbcheck] (no --fix on the upgrade path)", args)
	}
	m := readMarker(t, dir)
	if m == nil || m.SambaVersion != testImageVersion {
		t.Fatalf("marker = %+v, want the image version %q after a successful check", m, testImageVersion)
	}
	// initialized_at answers "when was this domain created", not "when was
	// it last checked": an upgrade must not rewrite the domain's birthday.
	if m.InitializedAt != "2026-01-01T00:00:00Z" {
		t.Errorf("InitializedAt = %q, want the original %q preserved across the upgrade", m.InitializedAt, "2026-01-01T00:00:00Z")
	}
	if m.LastMode != "run" {
		t.Errorf("LastMode = %q, want run", m.LastMode)
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
	// Adoption has no earlier marker to preserve, so it stamps the clock.
	if m.InitializedAt != "2026-08-16T10:00:00Z" {
		t.Errorf("InitializedAt = %q, want the injected clock value for an adopted volume", m.InitializedAt)
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
	// -f names the GENERATED configuration, never the baked-in template:
	// the template's signing-socket directory is only a default, and a
	// chronyd pointed at the template would ignore the rewrite entirely.
	if got := r.argsOf(t, "start", "chronyd"); !equalStrings(got, []string{"-d", "-x", "-f", e.ChronyConf}) {
		t.Errorf("chronyd args = %v, want -f %s (the generated configuration)", got, e.ChronyConf)
	}
	if e.ChronyConf == e.ChronyTemplate {
		t.Fatal("the test executor points the generated configuration at the template")
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
	waitFor(t, func() bool { return len(r.names()) == 3 })
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

// supervising starts Supervise in the background with a captured signal
// channel and returns the channel, the result channel and the runner.
func supervising(t *testing.T, ctx context.Context, e *Executor, r *fakeRunner, cfg *config.Config) (chan<- os.Signal, chan *config.Refusal) {
	t.Helper()
	var sigCh chan<- os.Signal
	ready := make(chan struct{})
	e.Notify = func(c chan<- os.Signal, _ ...os.Signal) { sigCh = c; close(ready) }
	e.Stop = func(chan<- os.Signal) {}

	done := make(chan *config.Refusal, 1)
	go func() { done <- e.Supervise(ctx, cfg) }()
	<-ready
	waitFor(t, func() bool { return len(r.names()) == 3 })
	return sigCh, done
}

// awaitClean asserts Supervise returned a clean exit within the timeout.
func awaitClean(t *testing.T, done <-chan *config.Refusal, timeout time.Duration) {
	t.Helper()
	select {
	case ref := <-done:
		if ref != nil {
			t.Fatalf("a clean shutdown must exit 0, got refusal %d: %s", ref.Code, ref.Msg)
		}
	case <-time.After(timeout):
		t.Fatalf("Supervise did not return within %s", timeout)
	}
}

// TestSuperviseCancellationStopsSambaBeforeChrony pins the ordering on the
// context path. Handing the daemons the cancellable context would let
// os/exec signal both of them the moment it is cancelled — concurrently, and
// behind this function's back — so the test also asserts that the context
// the daemons were started with is not the one being cancelled.
func TestSuperviseCancellationStopsSambaBeforeChrony(t *testing.T) {
	r := newFakeRunner()
	chrony, samba := liveProc(), liveProc()
	r.procs["chronyd"], r.procs["samba"] = chrony, samba

	var mu sync.Mutex
	var order []string
	chrony.onSignal = func() { mu.Lock(); order = append(order, "chronyd"); mu.Unlock() }
	samba.onSignal = func() { mu.Lock(); order = append(order, "samba"); mu.Unlock() }

	e, _ := newTestExecutor(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	_, done := supervising(t, ctx, e, r, runConfig(config.ModeRun))

	cancel()
	awaitClean(t, done, 5*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if !equalStrings(order, []string{"samba", "chronyd"}) {
		t.Errorf("shutdown order = %v, want samba stopped before chronyd", order)
	}
	for i, daemonCtx := range r.startContexts() {
		if daemonCtx.Err() != nil {
			t.Errorf("daemon %d was started with a cancellable context (%v); os/exec would signal it on its own", i, daemonCtx.Err())
		}
	}
}

// TestSuperviseKillsADaemonThatIgnoresSIGTERM exercises the safety valve: a
// wedged daemon is killed once the grace window is spent, and must not
// strand the other daemon.
func TestSuperviseKillsADaemonThatIgnoresSIGTERM(t *testing.T) {
	r := newFakeRunner()
	chrony, samba := liveProc(), stubbornProc()
	r.procs["chronyd"], r.procs["samba"] = chrony, samba

	e, logBuf := newTestExecutor(t, r)
	e.ShutdownGrace = 40 * time.Millisecond
	e.KillGrace = 40 * time.Millisecond

	sigCh, done := supervising(t, context.Background(), e, r, runConfig(config.ModeRun))
	sigCh <- syscall.SIGTERM
	awaitClean(t, done, 5*time.Second)

	if got := samba.signals(); len(got) != 2 || got[0] != syscall.SIGTERM || got[1] != os.Kill {
		t.Errorf("samba signals = %v, want SIGTERM then SIGKILL", got)
	}
	if len(chrony.signals()) == 0 {
		t.Error("chronyd was never stopped: a wedged samba stranded it")
	}
	if !strings.Contains(logBuf.String(), "killing it") {
		t.Errorf("the escalation was not announced:\n%s", logBuf.String())
	}
}

// TestSuperviseGivesUpOnAnUnreapableDaemon covers the last branch: a process
// that cannot be reaped at all is left to the init process rather than
// blocking the container's shutdown for ever.
func TestSuperviseGivesUpOnAnUnreapableDaemon(t *testing.T) {
	r := newFakeRunner()
	chrony, samba := liveProc(), unreapableProc(t)
	r.procs["chronyd"], r.procs["samba"] = chrony, samba

	e, logBuf := newTestExecutor(t, r)
	e.ShutdownGrace = 40 * time.Millisecond
	e.KillGrace = 40 * time.Millisecond

	sigCh, done := supervising(t, context.Background(), e, r, runConfig(config.ModeRun))
	sigCh <- syscall.SIGTERM
	awaitClean(t, done, 5*time.Second)

	if !strings.Contains(logBuf.String(), "could not be reaped") {
		t.Errorf("giving up was not announced:\n%s", logBuf.String())
	}
	if len(chrony.signals()) == 0 {
		t.Error("chronyd was never stopped: an unreapable samba stranded it")
	}
}

// TestSuperviseShutdownSharesOneBudget pins the §6.3 ceiling: the 10 s is
// the total an orderly stop may take — both daemons, SIGTERM phase and
// SIGKILL escalation included. Two daemons that never respond must still be
// given up on inside that one window, because past it the container runtime
// kills the container itself.
func TestSuperviseShutdownSharesOneBudget(t *testing.T) {
	r := newFakeRunner()
	chrony, samba := unreapableProc(t), unreapableProc(t)
	r.procs["chronyd"], r.procs["samba"] = chrony, samba

	e, logBuf := newTestExecutor(t, r)
	e.ShutdownGrace = 300 * time.Millisecond
	e.KillGrace = 100 * time.Millisecond

	sigCh, done := supervising(t, context.Background(), e, r, runConfig(config.ModeRun))
	start := time.Now()
	sigCh <- syscall.SIGTERM
	awaitClean(t, done, 5*time.Second)
	elapsed := time.Since(start)

	// The whole shutdown fits in ShutdownGrace. Anything that adds the kill
	// window on top (400ms), or gives each daemon its own (600ms+), busts
	// the contract. The slack is for scheduling, not for another phase.
	const slack = 80 * time.Millisecond
	if elapsed > e.ShutdownGrace+slack {
		t.Errorf("shutdown took %s: the %s budget is the total, escalation included", elapsed, e.ShutdownGrace)
	}
	// It must still have waited: the SIGTERM phase is the budget minus the
	// reap window held back for SIGKILL.
	if want := e.ShutdownGrace - e.KillGrace; elapsed < want {
		t.Errorf("shutdown took %s, less than the %s SIGTERM phase: samba was not given its window", elapsed, want)
	}
	// Both daemons were escalated on, and chronyd's message says how much
	// of the shared budget was actually left for it.
	for _, want := range []string{"samba did not stop", "chronyd did not stop", "could not be reaped"} {
		if !strings.Contains(logBuf.String(), want) {
			t.Errorf("log does not contain %q:\n%s", want, logBuf.String())
		}
	}
	if !strings.Contains(logBuf.String(), "shared") {
		t.Errorf("the log does not say the budget was shared:\n%s", logBuf.String())
	}
}

// TestSuperviseShutdownFitsTheGraceWhenTheKillWindowExceedsTheGrace checks
// the same ceiling with a kill window larger than the whole grace: the phases
// must still add up to no more than the total.
func TestSuperviseShutdownFitsTheGraceWhenTheKillWindowExceedsTheGrace(t *testing.T) {
	r := newFakeRunner()
	chrony, samba := unreapableProc(t), unreapableProc(t)
	r.procs["chronyd"], r.procs["samba"] = chrony, samba

	e, _ := newTestExecutor(t, r)
	e.ShutdownGrace = 200 * time.Millisecond
	e.KillGrace = 400 * time.Millisecond // absurd: larger than the total

	sigCh, done := supervising(t, context.Background(), e, r, runConfig(config.ModeRun))
	start := time.Now()
	sigCh <- syscall.SIGTERM
	awaitClean(t, done, 5*time.Second)

	if elapsed := time.Since(start); elapsed > e.ShutdownGrace+80*time.Millisecond {
		t.Errorf("shutdown took %s: a kill window wider than the budget must not extend it", elapsed)
	}
}

// TestSuperviseSurvivesChronydExit records a deliberate decision: chrony
// serves time, it is not the directory, so losing it degrades the DC without
// taking it down.
func TestSuperviseSurvivesChronydExit(t *testing.T) {
	r := newFakeRunner()
	chrony := exitingProc(errors.New("chronyd: exit status 1"))
	samba := liveProc()
	r.procs["chronyd"], r.procs["samba"] = chrony, samba

	e, logBuf := newTestExecutor(t, r)
	sigCh, done := supervising(t, context.Background(), e, r, runConfig(config.ModeRun))

	waitFor(t, func() bool { return strings.Contains(logBuf.String(), "chronyd exited") })
	select {
	case ref := <-done:
		t.Fatalf("Supervise returned (%v) when only chronyd died; the DC must keep serving", ref)
	case <-time.After(50 * time.Millisecond):
	}

	sigCh <- syscall.SIGTERM
	awaitClean(t, done, 5*time.Second)

	if got := samba.signals(); len(got) == 0 || got[0] != syscall.SIGTERM {
		t.Errorf("samba signals = %v, want SIGTERM", got)
	}
	if len(chrony.signals()) != 0 {
		t.Errorf("a dead chronyd was signaled again: %v", chrony.signals())
	}
	if !strings.Contains(logBuf.String(), "signed NTP is no longer served") {
		t.Errorf("the degradation was not announced:\n%s", logBuf.String())
	}
}

// TestSuperviseSignalWithRealProcesses exercises the one path fakes cannot
// cover: real fork/exec, real signal delivery and real reaping.
func TestSuperviseSignalWithRealProcesses(t *testing.T) {
	dir := t.TempDir()
	orderLog := filepath.Join(dir, "order.log")
	// Each stand-in announces itself only once its TERM trap is installed,
	// so the test waits for a fact instead of guessing a duration: a signal
	// delivered before the trap exists would kill the shell outright and
	// the ordering assertion below would test nothing.
	readyFile := func(name string) string { return filepath.Join(dir, name+".ready") }
	sleeper := func(name string) string {
		path := filepath.Join(dir, name+".sh")
		script := "#!/bin/sh\n" +
			"trap 'echo " + name + " >> " + orderLog + "; kill $pid 2>/dev/null; exit 0' TERM\n" +
			"sleep 30 & pid=$!\n" +
			": > " + readyFile(name) + "\n" +
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

	// A real stand-in for testparm as well: this is the only test that
	// forks real processes, so it is also the one place where the chrony
	// configuration is generated from a value that came back over a real
	// pipe rather than out of a scripted fake.
	testparm := filepath.Join(dir, "testparm.sh")
	if err := os.WriteFile(testparm, []byte("#!/bin/sh\necho "+customSigndDir+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{}, 4)
	base := NewExecRunner(out, out)
	r := &countingRunner{Runner: base, started: started}

	e := New(r)
	e.Root = t.TempDir()
	e.Log = out
	e.Bin = Binaries{
		SambaTool: "/bin/true", Samba: sleeper("samba"), Chronyd: sleeper("chronyd"),
		Smbclient: "/bin/true", Testparm: testparm,
	}
	e.ChronyTemplate = filepath.Join(dir, "chrony.conf")
	if err := os.WriteFile(e.ChronyTemplate, []byte(chronyTemplateContent), 0o644); err != nil {
		t.Fatal(err)
	}
	e.ChronyConf = filepath.Join(e.Root, "run", "chrony", "chrony.conf")
	var sigCh chan<- os.Signal
	ready := make(chan struct{})
	e.Notify = func(c chan<- os.Signal, _ ...os.Signal) { sigCh = c; close(ready) }
	e.Stop = func(chan<- os.Signal) {}

	done := make(chan *config.Refusal, 1)
	go func() { done <- e.Supervise(context.Background(), runConfig(config.ModeRun)) }()

	<-ready
	<-started // chronyd
	<-started // samba
	waitFor(t, func() bool {
		for _, name := range []string{"samba", "chronyd"} {
			if _, err := os.Stat(readyFile(name)); err != nil {
				return false
			}
		}
		return true
	})
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

	// The configuration the real chronyd was handed: generated, and
	// carrying the directory the real testparm stand-in reported.
	generated, err := os.ReadFile(e.ChronyConf)
	if err != nil {
		t.Fatalf("the generated chrony configuration is missing: %v", err)
	}
	if want := ntpSigndDirective + " " + customSigndDir; !strings.Contains(string(generated), want) {
		t.Errorf("generated chrony configuration does not carry %q:\n%s", want, generated)
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
// chrony configuration generation
// ---------------------------------------------------------------------------

// What this covers: an smb.conf that sets `ntp signd socket directory` to
// anything but samba's default. /etc/samba is the operator's volume, and a
// baked chrony.conf would keep naming the default — a socket samba never
// creates on that DC — leaving signed NTP dead and silent, because chrony
// opens the socket lazily, only when an authenticated request arrives.
// Asking samba's own parser is what makes the two files agree.
func TestChronyConfigFollowsTheDCsSigndSocketDirectory(t *testing.T) {
	r := newFakeRunner()
	r.output["testparm"] = customSigndDir + "\n"
	e, logBuf := newTestExecutor(t, r)
	if ref := e.makeRuntimeDirs(); ref != nil {
		t.Fatalf("makeRuntimeDirs: %s", ref.Msg)
	}

	path, ref := e.chronyConfig(context.Background())
	if ref != nil {
		t.Fatalf("chronyConfig: %s", ref.Msg)
	}
	if path != e.ChronyConf {
		t.Errorf("generated path = %q, want %q", path, e.ChronyConf)
	}

	// The value came from samba's own parser, over the documented flags.
	// `-l` is pinned in this expectation because it is a deliberate choice
	// rather than a default — a defensive one, for the reason testparmArgs
	// gives — and a silent drop would leave the answer at the mercy of
	// checks that have nothing to do with reading one parameter.
	if got := r.argsOf(t, "output", "testparm"); !equalStrings(got,
		[]string{"-s", "-l", "--parameter-name=ntp signd socket directory", e.SMBConfPath}) {
		t.Errorf("testparm args = %v", got)
	}

	got := readFile(t, path)
	if want := ntpSigndDirective + " " + customSigndDir; !strings.Contains(got, want) {
		t.Errorf("generated configuration does not carry %q:\n%s", want, got)
	}
	if strings.Contains(got, provisionedSigndDir) {
		t.Errorf("the template's default directory survived the rewrite:\n%s", got)
	}
	// Everything else is copied verbatim — the absolute drift and pid
	// paths in particular, which the generated file's new location must
	// not disturb.
	for _, line := range []string{
		"user root",
		"driftfile /var/lib/samba/chrony/drift",
		"pidfile /run/chrony/chronyd.pid",
		"local stratum 10",
	} {
		if !strings.Contains(got, line) {
			t.Errorf("generated configuration lost %q:\n%s", line, got)
		}
	}
	// The template itself is never written to: /etc is read-only (B.2).
	if tmpl := readFile(t, e.ChronyTemplate); tmpl != chronyTemplateContent {
		t.Errorf("the baked template was modified:\n%s", tmpl)
	}
	if !strings.Contains(logBuf.String(), customSigndDir) {
		t.Errorf("the rewrite was not announced to the operator:\n%s", logBuf.String())
	}
}

// The fallback. testparm is the only source for this value, so what
// happens when it does not answer decides whether an unreadable
// configuration costs the DC its time service or merely its certainty.
func TestChronyConfigFallsBackToTheTemplateValue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		err    error
	}{
		{name: "testparm fails", err: errors.New("exit status 1")},
		{name: "empty answer", output: "\n"},
		{name: "not a path", output: "Loaded services file OK.\n"},
		{name: "several lines", output: "/var/lib/samba/ntp_signd\nWARNING\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFakeRunner()
			r.output["testparm"] = tc.output
			r.outputErr["testparm"] = tc.err
			e, logBuf := newTestExecutor(t, r)
			if ref := e.makeRuntimeDirs(); ref != nil {
				t.Fatalf("makeRuntimeDirs: %s", ref.Msg)
			}

			path, ref := e.chronyConfig(context.Background())
			if ref != nil {
				t.Fatalf("a testparm that does not answer must not stop the time service, got refusal %d: %s", ref.Code, ref.Msg)
			}
			if got := readFile(t, path); got != chronyTemplateContent {
				t.Errorf("the template was not copied verbatim:\n%s", got)
			}
			if !strings.Contains(logBuf.String(), "baked into the image") {
				t.Errorf("the fallback was not announced to the operator:\n%s", logBuf.String())
			}
		})
	}
}

func TestChronyConfigRefusesWhenTheTemplateIsMissing(t *testing.T) {
	r := newFakeRunner()
	e, _ := newTestExecutor(t, r)
	e.ChronyTemplate = filepath.Join(t.TempDir(), "absent.conf")

	_, ref := e.chronyConfig(context.Background())
	if ref == nil || ref.Code != config.CodeRuntimeFailure {
		t.Fatalf("refusal = %v, want code %d", ref, config.CodeRuntimeFailure)
	}
	if !strings.Contains(ref.Msg, "SAMBA_CHRONY=off") {
		t.Errorf("the refusal names no way forward: %s", ref.Msg)
	}
}

// withNTPSigndSocket is the pure half: it must rewrite the directive and
// nothing else, and generating the same file twice must produce the same
// bytes.
func TestWithNTPSigndSocket(t *testing.T) {
	tests := []struct {
		name        string
		conf        string
		dir         string
		want        string
		wantChanged bool
	}{
		{
			name:        "rewrites the directive in place",
			conf:        "# c\nntpsigndsocket /var/lib/samba/ntp_signd\nuser root\n",
			dir:         customSigndDir,
			want:        "# c\nntpsigndsocket " + customSigndDir + "\nuser root\n",
			wantChanged: true,
		},
		{
			name:        "idempotent when it already agrees",
			conf:        "ntpsigndsocket " + customSigndDir + "\nuser root\n",
			dir:         customSigndDir,
			want:        "ntpsigndsocket " + customSigndDir + "\nuser root\n",
			wantChanged: false,
		},
		{
			name:        "appends when the directive is absent",
			conf:        "user root\n",
			dir:         customSigndDir,
			want:        "user root\nntpsigndsocket " + customSigndDir + "\n",
			wantChanged: true,
		},
		{
			name:        "a commented directive is not the directive",
			conf:        "# ntpsigndsocket /nowhere\n",
			dir:         customSigndDir,
			want:        "# ntpsigndsocket /nowhere\nntpsigndsocket " + customSigndDir + "\n",
			wantChanged: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := withNTPSigndSocket(tc.conf, tc.dir)
			if got != tc.want {
				t.Errorf("result =\n%q\nwant\n%q", got, tc.want)
			}
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			// Idempotence: a second pass over the result changes nothing.
			if again, changed := withNTPSigndSocket(got, tc.dir); changed || again != got {
				t.Errorf("second pass changed the file (changed=%v):\n%q", changed, again)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// smb.conf editing (pure)
// ---------------------------------------------------------------------------

// withGlobalSetting is one editor used for every setting a join has to force
// in; the three TestWithGlobalSetting* tests exercise it per setting. This
// one carries the general cases — sections, spacing, comments, replacement —
// and the other two the specifics of their own parameter.
func TestWithGlobalSettingDNSUpdateCommand(t *testing.T) {
	const line = "\tdns update command = /usr/sbin/samba_dnsupdate --use-samba-tool"
	tests := []struct {
		name     string
		in       string
		want     string
		wantEdit confEdit
	}{
		{
			name:     "inserted at the top of an existing global section",
			in:       "[global]\n\trealm = AD.EXAMPLE.COM\n\n[netlogon]\n\tpath = /var/lib/samba/sysvol\n",
			want:     "[global]\n" + line + "\n\trealm = AD.EXAMPLE.COM\n\n[netlogon]\n\tpath = /var/lib/samba/sysvol\n",
			wantEdit: confAdded,
		},
		{
			name:     "already present is left byte-for-byte alone",
			in:       "[global]\n\trealm = AD.EXAMPLE.COM\n\tdns update command = /usr/sbin/samba_dnsupdate --use-samba-tool\n",
			want:     "[global]\n\trealm = AD.EXAMPLE.COM\n\tdns update command = /usr/sbin/samba_dnsupdate --use-samba-tool\n",
			wantEdit: confUnchanged,
		},
		{
			name:     "recognized despite odd spacing and case",
			in:       "[global]\n   DNS Update Command   =   /usr/sbin/samba_dnsupdate --use-samba-tool\n",
			want:     "[global]\n   DNS Update Command   =   /usr/sbin/samba_dnsupdate --use-samba-tool\n",
			wantEdit: confUnchanged,
		},
		{
			name:     "global section header is matched case-insensitively",
			in:       "[Global]\n\trealm = AD.EXAMPLE.COM\n",
			want:     "[Global]\n" + line + "\n\trealm = AD.EXAMPLE.COM\n",
			wantEdit: confAdded,
		},
		{
			name:     "a file without a global section gets one",
			in:       "[netlogon]\n\tpath = /var/lib/samba/sysvol\n",
			want:     "[netlogon]\n\tpath = /var/lib/samba/sysvol\n\n[global]\n" + line + "\n",
			wantEdit: confAdded,
		},
		{
			name:     "an empty file gets a global section",
			in:       "",
			want:     "[global]\n" + line + "\n",
			wantEdit: confAdded,
		},
		{
			name:     "a commented-out option does not count as present",
			in:       "[global]\n\t# dns update command = /usr/sbin/samba_dnsupdate\n",
			want:     "[global]\n" + line + "\n\t# dns update command = /usr/sbin/samba_dnsupdate\n",
			wantEdit: confAdded,
		},
		{
			// The samba default calls nsupdate, which this image does not
			// ship: a wrong value is worse than a missing one.
			name:     "a different value in global is replaced",
			in:       "[global]\n\trealm = AD.EXAMPLE.COM\n\tdns update command = /usr/sbin/samba_dnsupdate\n",
			want:     "[global]\n\trealm = AD.EXAMPLE.COM\n" + line + "\n",
			wantEdit: confReplaced,
		},
		{
			name:     "a value pointing somewhere else entirely is replaced",
			in:       "[global]\n\tdns update command = /usr/local/bin/my-updater --flag\n",
			want:     "[global]\n" + line + "\n",
			wantEdit: confReplaced,
		},
		{
			// Only [global] configures the DC: the same key in another
			// section must not be mistaken for the setting.
			name:     "the key in another section does not count",
			in:       "[global]\n\trealm = AD.EXAMPLE.COM\n\n[custom]\n\tdns update command = /usr/sbin/samba_dnsupdate --use-samba-tool\n",
			want:     "[global]\n" + line + "\n\trealm = AD.EXAMPLE.COM\n\n[custom]\n\tdns update command = /usr/sbin/samba_dnsupdate --use-samba-tool\n",
			wantEdit: confAdded,
		},
		{
			name:     "the setting is found further down the global section",
			in:       "[global]\n\trealm = AD.EXAMPLE.COM\n\tworkgroup = AD\n\tdns update command = /usr/sbin/samba_dnsupdate --use-samba-tool\n\n[netlogon]\n",
			want:     "[global]\n\trealm = AD.EXAMPLE.COM\n\tworkgroup = AD\n\tdns update command = /usr/sbin/samba_dnsupdate --use-samba-tool\n\n[netlogon]\n",
			wantEdit: confUnchanged,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, edit := withGlobalSetting(tc.in, dnsUpdateKey, dnsUpdateValue)
			if got != tc.want {
				t.Errorf("withGlobalSetting()\n got %q\nwant %q", got, tc.want)
			}
			if edit != tc.wantEdit {
				t.Errorf("edit = %v, want %v", edit, tc.wantEdit)
			}
			// Applying it twice must be a no-op: joins repeat on restart.
			again, editAgain := withGlobalSetting(got, dnsUpdateKey, dnsUpdateValue)
			if again != got || editAgain != confUnchanged {
				t.Errorf("second application changed the file (idempotence broken)")
			}
		})
	}
}

// The `ad dc functional level` entry is the second thing a join has to force
// into the generated smb.conf, and the one samba refuses to start without
// when it is below the domain's level. It goes through the same editor, so
// what is tested here is that editor applied to THAT setting: a missing
// entry is added, this image's value is left alone, and a value samba would
// refuse to boot with is replaced rather than kept.
func TestWithGlobalSettingFunctionalLevel(t *testing.T) {
	const line = "\tad dc functional level = 2016"
	tests := []struct {
		name     string
		in       string
		want     string
		wantEdit confEdit
	}{
		{
			name:     "a join-generated file without the setting gets it",
			in:       "[global]\n\trealm = AD.EXAMPLE.COM\n\tworkgroup = AD\n",
			want:     "[global]\n" + line + "\n\trealm = AD.EXAMPLE.COM\n\tworkgroup = AD\n",
			wantEdit: confAdded,
		},
		{
			name:     "already at this image's level is left byte-for-byte alone",
			in:       "[global]\n\tad dc functional level = 2016\n",
			want:     "[global]\n\tad dc functional level = 2016\n",
			wantEdit: confUnchanged,
		},
		{
			// samba's default since 4.19: a DC carrying it cannot start in a
			// domain provisioned at 2016.
			name:     "the samba default below the domain level is replaced",
			in:       "[global]\n\trealm = AD.EXAMPLE.COM\n\tad dc functional level = 2008_R2\n",
			want:     "[global]\n\trealm = AD.EXAMPLE.COM\n" + line + "\n",
			wantEdit: confReplaced,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, edit := withGlobalSetting(tc.in, dcFunctionalLevelKey, "2016")
			if got != tc.want {
				t.Errorf("withGlobalSetting()\n got %q\nwant %q", got, tc.want)
			}
			if edit != tc.wantEdit {
				t.Errorf("edit = %v, want %v", edit, tc.wantEdit)
			}
			again, editAgain := withGlobalSetting(got, dcFunctionalLevelKey, "2016")
			if again != got || editAgain != confUnchanged {
				t.Errorf("second application changed the file (idempotence broken)")
			}
		})
	}
}

// joinedConfSettings decides WHICH settings the post-join edit forces in.
// Both conditional ones are conditional in exactly the way provisionArgs is:
// the functional level has no value for levels at or below samba's 2008_R2
// default (setting it there would turn a working join into a configuration
// error), and the forwarder is only written when the operator asked for one.
func TestJoinedConfSettings(t *testing.T) {
	tests := []struct {
		name          string
		functionLevel string
		forwarder     string
		wantLevel     string // "" means the level must not be set at all
		wantForwarder string // "" means the forwarder must not be set at all
	}{
		{"2016", "2016", "", "2016", ""},
		{"2012_R2", "2012_R2", "", "2012_R2", ""},
		{"2012", "2012", "", "2012", ""},
		{"2008_R2 needs no level option", "2008_R2", "", "", ""},
		{"2003 needs no level option", "2003", "", "", ""},
		{"forwarder set", "2016", "10.0.0.53", "2016", "10.0.0.53"},
		// A DC that resolves through itself is unusable without one, so the
		// forwarder has to survive to the joined DC's smb.conf whatever else
		// is configured.
		{"forwarder with no level option", "2008_R2", "10.0.0.53", "", "10.0.0.53"},
		{"blank forwarder is not written", "2016", "   ", "2016", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := joinConfig(t)
			cfg.FunctionLevel = tc.functionLevel
			cfg.DNSForwarder = tc.forwarder

			var gotLevel, gotForwarder string
			var gotDNS bool
			for _, s := range joinedConfSettings(cfg) {
				switch s.key {
				case dcFunctionalLevelKey:
					gotLevel = s.value
				case dnsForwarderKey:
					gotForwarder = s.value
				case dnsUpdateKey:
					gotDNS = s.value == dnsUpdateValue
				}
				if s.why == "" {
					t.Errorf("setting %q carries no reason to log", s.key)
				}
			}
			if !gotDNS {
				t.Errorf("the DNS update command is not among the settings a join forces in")
			}
			if gotLevel != tc.wantLevel {
				t.Errorf("%q = %q, want %q", dcFunctionalLevelKey, gotLevel, tc.wantLevel)
			}
			if gotForwarder != tc.wantForwarder {
				t.Errorf("%q = %q, want %q", dnsForwarderKey, gotForwarder, tc.wantForwarder)
			}
		})
	}
}

// The forwarder goes through the same editor as everything else, so what is
// tested here is that editor applied to THAT setting: added when absent, left
// alone when already this DC's upstream, and replaced when it points
// somewhere else — a wrong forwarder is worse than none, because the DC then
// waits on an upstream that will never answer.
func TestWithGlobalSettingDNSForwarder(t *testing.T) {
	const line = "\tdns forwarder = 10.0.0.53"
	tests := []struct {
		name     string
		in       string
		want     string
		wantEdit confEdit
	}{
		{
			name:     "a join-generated file without the setting gets it",
			in:       "[global]\n\trealm = AD.EXAMPLE.COM\n",
			want:     "[global]\n" + line + "\n\trealm = AD.EXAMPLE.COM\n",
			wantEdit: confAdded,
		},
		{
			name:     "the same upstream is left byte-for-byte alone",
			in:       "[global]\n\tdns forwarder = 10.0.0.53\n",
			want:     "[global]\n\tdns forwarder = 10.0.0.53\n",
			wantEdit: confUnchanged,
		},
		{
			name:     "a different upstream is replaced",
			in:       "[global]\n\tdns forwarder = 192.0.2.1\n",
			want:     "[global]\n" + line + "\n",
			wantEdit: confReplaced,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, edit := withGlobalSetting(tc.in, dnsForwarderKey, "10.0.0.53")
			if got != tc.want {
				t.Errorf("withGlobalSetting()\n got %q\nwant %q", got, tc.want)
			}
			if edit != tc.wantEdit {
				t.Errorf("edit = %v, want %v", edit, tc.wantEdit)
			}
			again, editAgain := withGlobalSetting(got, dnsForwarderKey, "10.0.0.53")
			if again != got || editAgain != confUnchanged {
				t.Errorf("second application changed the file (idempotence broken)")
			}
		})
	}
}

func TestEnsureJoinedConfWritesAtomically(t *testing.T) {
	e, logBuf := newTestExecutor(t, newFakeRunner())
	dir := filepath.Dir(e.SMBConfPath)
	if err := os.WriteFile(e.SMBConfPath, []byte("[global]\n\trealm = AD.EXAMPLE.COM\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := joinConfig(t)
	cfg.DNSForwarder = "10.0.0.53"
	if ref := e.ensureJoinedConf(cfg); ref != nil {
		t.Fatalf("unexpected refusal: %s", ref.Msg)
	}
	data, err := os.ReadFile(e.SMBConfPath)
	if err != nil {
		t.Fatal(err)
	}
	// Every setting lands in ONE rewrite: a joined DC that got the DNS
	// command but not the functional level does not start at all, and one
	// that got both but not the forwarder cannot replicate.
	for _, want := range []string{
		dnsUpdateCommand,
		dcFunctionalLevelKey + " = 2016",
		dnsForwarderKey + " = 10.0.0.53",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("smb.conf does not carry %q:\n%s", want, data)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(e.SMBConfPath) {
			t.Errorf("the rewrite left %q behind; it must be temp file + rename", entry.Name())
		}
	}
	info, err := os.Stat(e.SMBConfPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("smb.conf permissions = %o, want 644 (samba must read it)", perm)
	}
	if !strings.Contains(logBuf.String(), "added") {
		t.Errorf("the edit was not announced:\n%s", logBuf.String())
	}
}

func TestEnsureJoinedConfReplacesAWrongValue(t *testing.T) {
	e, logBuf := newTestExecutor(t, newFakeRunner())
	if err := os.WriteFile(e.SMBConfPath, []byte("[global]\n\tdns update command = /usr/sbin/samba_dnsupdate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ref := e.ensureJoinedConf(joinConfig(t)); ref != nil {
		t.Fatalf("unexpected refusal: %s", ref.Msg)
	}
	data, err := os.ReadFile(e.SMBConfPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), dnsUpdateKey) != 1 {
		t.Errorf("the wrong value was not replaced but duplicated:\n%s", data)
	}
	if !strings.Contains(string(data), dnsUpdateValue) {
		t.Errorf("smb.conf does not carry this image's command:\n%s", data)
	}
	if !strings.Contains(logBuf.String(), "replaced") {
		t.Errorf("the replacement was not announced:\n%s", logBuf.String())
	}
}

func TestEnsureJoinedConfMissingFileRefuses(t *testing.T) {
	e, _ := newTestExecutor(t, newFakeRunner())
	e.SMBConfPath = filepath.Join(t.TempDir(), "absent", "smb.conf")
	ref := e.ensureJoinedConf(joinConfig(t))
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

// readFile returns the contents of path, failing the test if it cannot.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

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

// The `ad dc functional level` parameter defaults to 2008_R2 since Samba
// 4.19 and provision refuses any domain/forest level above it, so the
// requested level has to be mirrored onto that parameter — but only for the
// values the parameter actually accepts. Getting this wrong is not a
// cosmetic difference: either provision refuses (level too high) or it
// refuses on an invalid parameter value (level too low).
func TestProvisionMirrorsTheFunctionLevelOntoTheDCParameter(t *testing.T) {
	for _, tc := range []struct {
		functionLevel string
		wantOption    string // empty means the option must not be passed
	}{
		{"2016", "--option=ad dc functional level = 2016"},
		{"2012_R2", "--option=ad dc functional level = 2012_R2"},
		{"2012", "--option=ad dc functional level = 2012"},
		{"2008_R2", ""},
		{"2008", ""},
		{"2003", ""},
		{"2000", ""},
	} {
		t.Run(tc.functionLevel, func(t *testing.T) {
			cfg := provisionConfig(t)
			cfg.FunctionLevel = tc.functionLevel
			args := redactArgs(provisionArgs(cfg, testSecret))

			if !hasArg(args, "--function-level="+tc.functionLevel) {
				t.Fatalf("args %v do not carry the requested function level", args)
			}
			got := ""
			for _, a := range args {
				if strings.HasPrefix(a, "--option="+dcFunctionalLevelKey) {
					got = a
				}
			}
			if got != tc.wantOption {
				t.Errorf("dc functional level option = %q, want %q (args %v)", got, tc.wantOption, args)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// SAMBA_GLOBAL_OPTIONS
// ---------------------------------------------------------------------------

// globalOptionsConfig is a start-mode config declaring two harmless [global]
// settings.
func globalOptionsConfig(mode config.Mode, opts ...config.GlobalOption) *config.Config {
	cfg := runConfig(mode)
	if len(opts) == 0 {
		opts = []config.GlobalOption{
			{Key: "smb encrypt", Value: "required"},
			{Key: "log level", Value: "1 auth:3"},
		}
	}
	cfg.GlobalOptions = opts
	return cfg
}

// writeSMBConf writes a minimal provisioned-looking smb.conf and returns its
// bytes, so a test can assert the file came back to exactly them.
func writeSMBConf(t *testing.T, path string) []byte {
	t.Helper()
	data := []byte("[global]\n\trealm = AD.EXAMPLE.COM\n\tworkgroup = AD\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return data
}

// countCalls returns how many recorded calls are a `kind` call to `name`.
func (f *fakeRunner) countCalls(kind, name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.kind == kind && c.name == name {
			n++
		}
	}
	return n
}

func TestProvisionPassesTheDeclaredGlobalOptions(t *testing.T) {
	cfg := provisionConfig(t)
	cfg.GlobalOptions = []config.GlobalOption{
		{Key: "smb encrypt", Value: "required"},
		{Key: "log level", Value: "1 auth:3"},
	}
	args := redactArgs(provisionArgs(cfg, testSecret))

	// The form matters: samba-tool takes `--option=key = value`, and the
	// declaration order is preserved so a block an operator can read top to
	// bottom is the order samba sees.
	want := []string{"--option=smb encrypt = required", "--option=log level = 1 auth:3"}
	var got []string
	for _, a := range args {
		if strings.HasPrefix(a, "--option=smb encrypt") || strings.HasPrefix(a, "--option=log level") {
			got = append(got, a)
		}
	}
	if !equalStrings(got, want) {
		t.Errorf("declared options reached samba-tool as %v, want %v (full args %v)", got, want, args)
	}
	if args[len(args)-1] != "--adminpass="+redactedValue {
		t.Errorf("the options were appended after the password: %v", args)
	}
}

func TestEnsureGlobalOptionsAddsThemAndValidatesTheResult(t *testing.T) {
	r := newFakeRunner()
	e, logBuf := newTestExecutor(t, r)
	writeSMBConf(t, e.SMBConfPath)

	if ref := e.ensureGlobalOptions(context.Background(), globalOptionsConfig(config.ModeRun)); ref != nil {
		t.Fatalf("unexpected refusal %d: %s", ref.Code, ref.Msg)
	}

	conf := readFile(t, e.SMBConfPath)
	for _, want := range []string{"smb encrypt = required", "log level = 1 auth:3"} {
		if !strings.Contains(conf, want) {
			t.Errorf("smb.conf does not carry %q:\n%s", want, conf)
		}
	}
	// One log line per change, naming the variable, the setting and the
	// file: an operator reading their configuration back must be able to
	// find out from the container log who wrote that line.
	for _, want := range []string{
		`SAMBA_GLOBAL_OPTIONS: added "smb encrypt" = "required" in ` + e.SMBConfPath,
		`SAMBA_GLOBAL_OPTIONS: added "log level" = "1 auth:3" in ` + e.SMBConfPath,
	} {
		if !strings.Contains(logBuf.String(), want) {
			t.Errorf("the log does not carry %q:\n%s", want, logBuf.String())
		}
	}
	// The rewrite is gated by samba's own parser, reading the same file
	// samba will read.
	args := r.argsOf(t, "output", "testparm")
	if !equalStrings(args, []string{"-s", "-l", "--debug-stdout", e.SMBConfPath}) {
		t.Errorf("testparm was called with %v, want the whole-file check on %s", args, e.SMBConfPath)
	}
}

func TestEnsureGlobalOptionsIsIdempotent(t *testing.T) {
	r := newFakeRunner()
	e, logBuf := newTestExecutor(t, r)
	writeSMBConf(t, e.SMBConfPath)
	cfg := globalOptionsConfig(config.ModeRun)

	if ref := e.ensureGlobalOptions(context.Background(), cfg); ref != nil {
		t.Fatalf("first call: unexpected refusal: %s", ref.Msg)
	}
	first := readFile(t, e.SMBConfPath)
	calls := r.countCalls("output", "testparm")

	logBuf.reset()
	if ref := e.ensureGlobalOptions(context.Background(), cfg); ref != nil {
		t.Fatalf("second call: unexpected refusal: %s", ref.Msg)
	}

	// Nothing changed, so nothing was written, nothing was validated and
	// nothing was said. A start that rewrites smb.conf every time would
	// churn the file samba reads and drown the log in noise.
	if again := readFile(t, e.SMBConfPath); again != first {
		t.Errorf("the second call rewrote smb.conf:\nwas\n%s\nnow\n%s", first, again)
	}
	if got := r.countCalls("output", "testparm"); got != calls {
		t.Errorf("testparm ran %d time(s) on a start that changed nothing (%d before)", got-calls, calls)
	}
	if strings.Contains(logBuf.String(), "SAMBA_GLOBAL_OPTIONS") {
		t.Errorf("the second call announced a change it did not make:\n%s", logBuf.String())
	}
}

func TestEnsureGlobalOptionsReplacesAChangedValue(t *testing.T) {
	e, logBuf := newTestExecutor(t, newFakeRunner())
	if err := os.WriteFile(e.SMBConfPath, []byte("[global]\n\tsmb encrypt = desired\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := globalOptionsConfig(config.ModeRun, config.GlobalOption{Key: "smb encrypt", Value: "required"})
	if ref := e.ensureGlobalOptions(context.Background(), cfg); ref != nil {
		t.Fatalf("unexpected refusal: %s", ref.Msg)
	}

	conf := readFile(t, e.SMBConfPath)
	if strings.Count(conf, "smb encrypt") != 1 {
		t.Errorf("the old value was duplicated instead of replaced:\n%s", conf)
	}
	if !strings.Contains(conf, "smb encrypt = required") {
		t.Errorf("smb.conf does not carry the new value:\n%s", conf)
	}
	want := `SAMBA_GLOBAL_OPTIONS: replaced "smb encrypt" = "required" in ` + e.SMBConfPath
	if !strings.Contains(logBuf.String(), want) {
		t.Errorf("the log does not carry %q:\n%s", want, logBuf.String())
	}
}

// TestEnsureGlobalOptionsRestoresTheFileWhenTestparmRefusesIt is the reason
// the gate exists at all. An option samba cannot parse is not a degraded DC,
// it is a DC that will not start — and the configuration file lives on a
// volume, so a bad rewrite would outlive the container that made it and break
// every later start, including the one an operator makes after removing the
// variable.
func TestEnsureGlobalOptionsRestoresTheFileWhenTestparmRefusesIt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		output    string
		outputErr error
		wantQuote string
	}{
		{
			// An unknown parameter: testparm reports it and still exits 0,
			// so the exit code alone would let it through.
			name:      "unknown parameter, zero exit",
			output:    "Unknown parameter encountered: \"this is not a parameter\"\nIgnoring unknown parameter \"this is not a parameter\"\n# Global parameters\n[global]\n",
			wantQuote: `Unknown parameter encountered: "this is not a parameter"`,
		},
		{
			// An invalid value for a real parameter: testparm exits 1 and
			// says why on the line before.
			name:      "invalid value, non-zero exit",
			output:    "WARNING: Ignoring invalid value 'bogus' for parameter 'smb encrypt'\n",
			outputErr: errors.New("testparm -s -l --debug-stdout /etc/samba/smb.conf: exit status 1"),
			wantQuote: "WARNING: Ignoring invalid value 'bogus' for parameter 'smb encrypt'",
		},
		{
			// Nothing to quote: the refusal still has to name a cause.
			name:      "no diagnostic at all, non-zero exit",
			outputErr: errors.New("testparm: exit status 1"),
			wantQuote: "exit status 1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFakeRunner()
			r.output["testparm check"] = tc.output
			r.outputErr["testparm check"] = tc.outputErr
			e, logBuf := newTestExecutor(t, r)
			original := writeSMBConf(t, e.SMBConfPath)

			ref := e.ensureGlobalOptions(context.Background(), globalOptionsConfig(config.ModeRun))
			if ref == nil {
				t.Fatal("expected a refusal when testparm rejects the result")
			}
			if ref.Code != config.CodeConfigError {
				t.Errorf("refusal code = %d, want %d: a rejected option is a configuration error",
					ref.Code, config.CodeConfigError)
			}
			if !strings.Contains(ref.Msg, "SAMBA_GLOBAL_OPTIONS") {
				t.Errorf("message %q does not name the variable to fix", ref.Msg)
			}
			if !strings.Contains(ref.Msg, tc.wantQuote) {
				t.Errorf("message %q does not quote testparm's own words %q", ref.Msg, tc.wantQuote)
			}
			if strings.Contains(ref.Msg, "\n") {
				t.Errorf("message must be a single line, got %q", ref.Msg)
			}
			// The bytes, not merely "a file exists": the volume must hold
			// exactly what it held before this boot touched it.
			if got := readFile(t, e.SMBConfPath); got != string(original) {
				t.Errorf("smb.conf was not restored:\nwant\n%s\ngot\n%s", original, got)
			}
			// And the log must not claim a change that was rolled back: an
			// operator reading it for what is in force would be misled by an
			// "added" line describing bytes no longer on the volume.
			if strings.Contains(logBuf.String(), "added") || strings.Contains(logBuf.String(), "replaced") {
				t.Errorf("the refused boot announced an edit it rolled back:\n%s", logBuf.String())
			}
		})
	}
}

// TestEnsureGlobalOptionsDoesNotRefuseADeprecationWarning is the other half of
// the gate, and the more dangerous one to get wrong: samba prints a WARNING
// for a parameter it still accepts, and treating that as a verdict would
// refuse to boot a domain controller over a configuration samba loads without
// complaint. The warning belongs in the log, not in an exit code.
func TestEnsureGlobalOptionsDoesNotRefuseADeprecationWarning(t *testing.T) {
	r := newFakeRunner()
	r.output["testparm check"] = deprecatedParameterWarning + "\n" + validTestparmDump
	e, logBuf := newTestExecutor(t, r)
	writeSMBConf(t, e.SMBConfPath)

	cfg := globalOptionsConfig(config.ModeRun, config.GlobalOption{Key: "syslog only", Value: "no"})
	if ref := e.ensureGlobalOptions(context.Background(), cfg); ref != nil {
		t.Fatalf("a deprecation warning refused the boot: %d: %s", ref.Code, ref.Msg)
	}
	if !strings.Contains(readFile(t, e.SMBConfPath), "syslog only = no") {
		t.Errorf("the setting was rolled back:\n%s", readFile(t, e.SMBConfPath))
	}
	// Logged, both of them: what samba said, and what was written.
	if !strings.Contains(logBuf.String(), "is deprecated") {
		t.Errorf("testparm's warning never reached the log:\n%s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `added "syslog only" = "no"`) {
		t.Errorf("the edit was not announced:\n%s", logBuf.String())
	}
}

// A file samba accepts produces no diagnostics at all, so the gate must add
// nothing to the log beyond the edit itself.
func TestEnsureGlobalOptionsSaysNothingExtraWhenTestparmIsSilent(t *testing.T) {
	e, logBuf := newTestExecutor(t, newFakeRunner())
	writeSMBConf(t, e.SMBConfPath)

	if ref := e.ensureGlobalOptions(context.Background(), globalOptionsConfig(config.ModeRun)); ref != nil {
		t.Fatalf("unexpected refusal: %s", ref.Msg)
	}
	if strings.Contains(logBuf.String(), "testparm says") {
		t.Errorf("the gate invented a warning out of a clean run:\n%s", logBuf.String())
	}
}

func TestEnsureGlobalOptionsAnnouncesAShadowedKey(t *testing.T) {
	e, logBuf := newTestExecutor(t, newFakeRunner())
	writeSMBConf(t, e.SMBConfPath)

	cfg := globalOptionsConfig(config.ModeRun, config.GlobalOption{Key: "smb encrypt", Value: "required"})
	cfg.GlobalOptionsShadowed = []string{"smb encrypt"}
	if ref := e.ensureGlobalOptions(context.Background(), cfg); ref != nil {
		t.Fatalf("unexpected refusal: %s", ref.Msg)
	}
	if !strings.Contains(logBuf.String(), "smb encrypt") || !strings.Contains(logBuf.String(), "more than once") {
		t.Errorf("a key set twice was applied without a word about it:\n%s", logBuf.String())
	}
}

func TestEnsureGlobalOptionsWithNothingDeclaredTouchesNothing(t *testing.T) {
	r := newFakeRunner()
	e, logBuf := newTestExecutor(t, r)
	// No smb.conf at all: a container with no declared options must not
	// even read the file, let alone refuse over it.
	e.SMBConfPath = filepath.Join(t.TempDir(), "absent", "smb.conf")

	if ref := e.ensureGlobalOptions(context.Background(), runConfig(config.ModeRun)); ref != nil {
		t.Fatalf("unexpected refusal: %s", ref.Msg)
	}
	if n := r.countCalls("output", "testparm"); n != 0 {
		t.Errorf("testparm ran %d time(s) with nothing declared", n)
	}
	if logBuf.String() != "" {
		t.Errorf("the log is not silent:\n%s", logBuf.String())
	}
}

// TestExecuteStartAppliesGlobalOptionsBeforeTheDaemons pins the wiring: the
// reconciliation is part of every start, and it happens before samba reads
// the file — not after, where samba would already be serving the old one.
func TestExecuteStartAppliesGlobalOptionsBeforeTheDaemons(t *testing.T) {
	r := newFakeRunner()
	r.procs["chronyd"] = liveProc()
	r.procs["samba"] = exitingProc(nil)
	e, _ := newTestExecutor(t, r)
	writeSMBConf(t, e.SMBConfPath)

	confAtFirstStart := ""
	seenStart := false
	r.hook = func(c call) {
		if c.kind == "start" && !seenStart {
			seenStart = true
			data, _ := os.ReadFile(e.SMBConfPath)
			confAtFirstStart = string(data)
		}
	}

	dir := stateDirWith(t, &state.Marker{SambaVersion: testImageVersion, LastMode: "provision"})
	if ref := e.Execute(context.Background(), globalOptionsConfig(config.ModeRun), modes.Plan{Kind: modes.ActStart}, dir, testImageVersion); ref != nil {
		t.Fatalf("Execute: unexpected refusal %d: %s", ref.Code, ref.Msg)
	}
	if !strings.Contains(confAtFirstStart, "smb encrypt = required") {
		t.Errorf("the daemons started on an smb.conf without the declared options:\n%s", confAtFirstStart)
	}
}

func TestExecuteStartRefusesWhenTestparmRejectsTheOptions(t *testing.T) {
	r := newFakeRunner()
	r.output["testparm check"] = "Unknown parameter encountered: \"this is not a parameter\"\n"
	e, _ := newTestExecutor(t, r)
	original := writeSMBConf(t, e.SMBConfPath)

	dir := stateDirWith(t, &state.Marker{SambaVersion: testImageVersion, LastMode: "provision"})
	ref := e.Execute(context.Background(), globalOptionsConfig(config.ModeRun), modes.Plan{Kind: modes.ActStart}, dir, testImageVersion)
	if ref == nil {
		t.Fatal("expected a refusal")
	}
	if ref.Code != config.CodeConfigError {
		t.Errorf("refusal code = %d, want %d", ref.Code, config.CodeConfigError)
	}
	// Nothing was started: a DC must not serve a configuration its own
	// parser rejected.
	if n := r.countCalls("start", "samba") + r.countCalls("start", "chronyd"); n != 0 {
		t.Errorf("%d daemon(s) were started after the refusal (%v)", n, r.names())
	}
	if got := readFile(t, e.SMBConfPath); got != string(original) {
		t.Errorf("smb.conf was not restored:\nwant\n%s\ngot\n%s", original, got)
	}
}

// Maintenance runs a database check and starts nothing; it must not touch the
// configuration either. An operator reaching for maintenance mode is
// debugging a DC that will not run, and a mode that edited smb.conf on the
// way past would change the thing they are trying to diagnose.
func TestExecuteMaintenanceAppliesNoGlobalOptions(t *testing.T) {
	r := newFakeRunner()
	e, _ := newTestExecutor(t, r)
	original := writeSMBConf(t, e.SMBConfPath)

	dir := stateDirWith(t, &state.Marker{SambaVersion: testImageVersion, LastMode: "provision"})
	cfg := globalOptionsConfig(config.ModeMaintenance)
	if ref := e.Execute(context.Background(), cfg, modes.Plan{Kind: modes.ActMaintenance}, dir, testImageVersion); ref != nil {
		t.Fatalf("Execute: unexpected refusal: %s", ref.Msg)
	}
	if got := readFile(t, e.SMBConfPath); got != string(original) {
		t.Errorf("maintenance mode rewrote smb.conf:\nwas\n%s\nnow\n%s", original, got)
	}
}

// reset empties the captured log so a test can assert on what ONE step said.
func (b *syncBuffer) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// ---------------------------------------------------------------------------
// SAMBA_TLS_CERT_FILE / SAMBA_TLS_KEY_FILE / SAMBA_TLS_CA_FILE
// ---------------------------------------------------------------------------

// tlsMaterialFiles writes three stand-in PEM files and returns their paths in
// certificate, key, CA order. The content is irrelevant: nothing in the
// entrypoint parses this material — samba does — so what is under test is
// that the paths reach smb.conf and that a path which is not there stops the
// boot.
func tlsMaterialFiles(t *testing.T) (cert, key, ca string) {
	t.Helper()
	dir := t.TempDir()
	for name, f := range map[string]struct {
		content string
		mode    os.FileMode
	}{
		"cert.pem": {"-----BEGIN CERTIFICATE-----\n", 0o644},
		// The private key at 0600: the only mode samba accepts (the rule
		// itself is pinned by config.TestCheckTLSMaterialKeyPermissions).
		"key.pem": {"-----BEGIN PRIVATE KEY-----\n", 0o600},
		"ca.pem":  {"-----BEGIN CERTIFICATE-----\n", 0o644},
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(f.content), f.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, f.mode); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), filepath.Join(dir, "ca.pem")
}

// withTLSMaterial points cfg at three readable files and returns their paths.
func withTLSMaterial(t *testing.T, cfg *config.Config) (cert, key, ca string) {
	t.Helper()
	cert, key, ca = tlsMaterialFiles(t)
	cfg.TLSCertFile, cfg.TLSKeyFile, cfg.TLSCAFile = cert, key, ca
	return cert, key, ca
}

// The material has to reach provision on the command line, because the
// smb.conf provision generates is the one the DC's very first start reads:
// a DC that served samba's self-signed certificate for its first boot and the
// operator's from the second would be a DC whose identity changed under a
// client that had already pinned it.
func TestProvisionPassesTheTLSMaterialBeforeTheDeclaredOptions(t *testing.T) {
	cfg := provisionConfig(t)
	cert, key, ca := withTLSMaterial(t, cfg)
	cfg.GlobalOptions = []config.GlobalOption{{Key: "max log size", Value: "4000"}}

	args := redactArgs(provisionArgs(cfg, testSecret))
	var got []string
	for _, a := range args {
		if strings.HasPrefix(a, "--option=tls ") || strings.HasPrefix(a, "--option=max log size") {
			got = append(got, a)
		}
	}
	want := []string{
		"--option=tls certfile = " + cert,
		"--option=tls keyfile = " + key,
		"--option=tls cafile = " + ca,
		"--option=max log size = 4000",
	}
	if !equalStrings(got, want) {
		t.Errorf("provision got %v, want %v (full args %v)", got, want, args)
	}
}

// Every start reconciles the material into smb.conf, the same way the
// declarative block is reconciled — and each line is announced naming the
// variable that asked for it, not the variable next to it.
func TestEnsureGlobalOptionsAppliesTheTLSMaterial(t *testing.T) {
	e, logBuf := newTestExecutor(t, newFakeRunner())
	writeSMBConf(t, e.SMBConfPath)

	cfg := runConfig(config.ModeRun)
	cert, key, ca := withTLSMaterial(t, cfg)

	if ref := e.ensureGlobalOptions(context.Background(), cfg); ref != nil {
		t.Fatalf("unexpected refusal %d: %s", ref.Code, ref.Msg)
	}
	conf := readFile(t, e.SMBConfPath)
	for _, want := range []string{
		"tls certfile = " + cert,
		"tls keyfile = " + key,
		"tls cafile = " + ca,
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("smb.conf does not carry %q:\n%s", want, conf)
		}
	}
	for _, want := range []string{
		`SAMBA_TLS_CERT_FILE: added "tls certfile" = "` + cert + `" in ` + e.SMBConfPath,
		`SAMBA_TLS_KEY_FILE: added "tls keyfile" = "` + key + `" in ` + e.SMBConfPath,
		`SAMBA_TLS_CA_FILE: added "tls cafile" = "` + ca + `" in ` + e.SMBConfPath,
	} {
		if !strings.Contains(logBuf.String(), want) {
			t.Errorf("the log does not carry %q:\n%s", want, logBuf.String())
		}
	}
	// A second start with the same material must change nothing and say
	// nothing: `tls enabled` is already yes on an AD DC, and a DC that
	// rewrote its own configuration on every boot would make the log useless
	// exactly when it matters.
	logBuf.reset()
	if ref := e.ensureGlobalOptions(context.Background(), cfg); ref != nil {
		t.Fatalf("second pass: unexpected refusal: %s", ref.Msg)
	}
	if logBuf.String() != "" {
		t.Errorf("the second start re-applied the material:\n%s", logBuf.String())
	}
}

// A path that is not there stops the boot BEFORE samba-tool runs. A provision
// that went ahead would leave a volume claimed by a half-configured DC, and
// the operator would have to delete it to retry — for a typo in a mount path.
func TestExecuteRefusesUnreadableTLSMaterial(t *testing.T) {
	r := newFakeRunner()
	e, _ := newTestExecutor(t, r)
	cfg := provisionConfig(t)
	cert, _, ca := withTLSMaterial(t, cfg)
	missing := filepath.Join(t.TempDir(), "never-mounted", "key.pem")
	cfg.TLSKeyFile = missing

	ref := e.Execute(context.Background(), cfg, modes.Plan{Kind: modes.ActProvision}, t.TempDir(), testImageVersion)
	if ref == nil {
		t.Fatal("expected a refusal for a TLS key file that is not there")
	}
	if ref.Code != config.CodeSecretError {
		t.Errorf("refusal code = %d, want %d (secret material)", ref.Code, config.CodeSecretError)
	}
	for _, frag := range []string{"SAMBA_TLS_KEY_FILE", missing} {
		if !strings.Contains(ref.Msg, frag) {
			t.Errorf("message %q does not name %q", ref.Msg, frag)
		}
	}
	// The two paths that ARE readable must not be quoted: the message has to
	// point at the one thing to fix.
	for _, other := range []string{cert, ca} {
		if strings.Contains(ref.Msg, other) {
			t.Errorf("message %q also names the readable %q", ref.Msg, other)
		}
	}
	if n := r.countCalls("run", "samba-tool"); n != 0 {
		t.Errorf("samba-tool ran %d time(s) despite the refusal (%v)", n, r.names())
	}
}

// Maintenance starts no listener, so material it will never use must not stop
// an operator from diagnosing a DC that does not run — the same reason
// maintenance applies no [global] settings at all.
func TestExecuteMaintenanceIgnoresTLSMaterial(t *testing.T) {
	r := newFakeRunner()
	e, _ := newTestExecutor(t, r)
	original := writeSMBConf(t, e.SMBConfPath)

	cfg := runConfig(config.ModeMaintenance)
	withTLSMaterial(t, cfg)
	cfg.TLSKeyFile = filepath.Join(t.TempDir(), "never-mounted", "key.pem")

	dir := stateDirWith(t, &state.Marker{SambaVersion: testImageVersion, LastMode: "provision"})
	if ref := e.Execute(context.Background(), cfg, modes.Plan{Kind: modes.ActMaintenance}, dir, testImageVersion); ref != nil {
		t.Fatalf("maintenance refused over TLS material it does not use: %s", ref.Msg)
	}
	if got := readFile(t, e.SMBConfPath); got != string(original) {
		t.Errorf("maintenance mode rewrote smb.conf:\nwas\n%s\nnow\n%s", original, got)
	}
}
