// Package run turns a plan from the modes package into effects: it
// initializes the domain with samba-tool, records the version marker, and
// supervises the daemons for the life of the container.
//
// Every external program goes through the Runner seam (§6.7), so the whole
// package is exercised by unit tests with recorded fakes; only the signal
// path uses real processes, because signal delivery and reaping are exactly
// what a fake cannot prove.
package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/config"
	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/modes"
	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/state"
)

// dnsUpdateCommand is the smb.conf option that makes samba_dnsupdate use
// samba-tool instead of nsupdate. The image deliberately ships no
// bind9-dnsutils (Phase 1 ruling), so without this option every dynamic DNS
// update inside the DC fails. It is passed to provision with --option and
// written into the smb.conf that a join generates.
const dnsUpdateCommand = "dns update command = /usr/sbin/samba_dnsupdate --use-samba-tool"

// Default paths and programs.
const (
	defaultSMBConf    = "/etc/samba/smb.conf"
	defaultChronyConf = "/etc/chrony/chrony.conf"
	defaultRoot       = "/"
	// defaultShutdownGrace bounds the orderly stop of each daemon (§6.3:
	// samba then chrony within 10 s).
	defaultShutdownGrace = 10 * time.Second
	// killGrace bounds the wait after escalating to SIGKILL.
	killGrace = 2 * time.Second
)

// runtimeDirs are created before the daemons start. They live on tmpfs at
// runtime (the root filesystem is read-only, B.2), so they are empty on every
// boot and neither samba nor chronyd creates them itself.
var runtimeDirs = []string{"run/samba", "run/lock/samba", "run/chrony"}

// Binaries names the external programs. They are fields rather than
// constants so tests can substitute stand-ins that really fork and really
// receive signals.
type Binaries struct {
	SambaTool string
	Samba     string
	Chronyd   string
	Smbclient string
}

// DefaultBinaries resolves the programs through PATH, as the image installs
// them.
func DefaultBinaries() Binaries {
	return Binaries{
		SambaTool: "samba-tool",
		Samba:     "samba",
		Chronyd:   "chronyd",
		Smbclient: "smbclient",
	}
}

// Executor carries everything Execute and Supervise need that is not part of
// the operator-visible configuration: the shell-out seam, the program names,
// the filesystem root, the clock and the signal plumbing. Production uses the
// defaults from New; tests replace the parts they need to observe.
type Executor struct {
	Runner        Runner
	Bin           Binaries
	Root          string // filesystem root under which runtime dirs are made
	SMBConfPath   string
	ChronyConf    string
	Now           func() time.Time
	Log           io.Writer
	Notify        func(c chan<- os.Signal, sig ...os.Signal)
	Stop          func(c chan<- os.Signal)
	ShutdownGrace time.Duration
}

// New returns an Executor wired for the container.
func New(r Runner) *Executor {
	return &Executor{
		Runner:        r,
		Bin:           DefaultBinaries(),
		Root:          defaultRoot,
		SMBConfPath:   defaultSMBConf,
		ChronyConf:    defaultChronyConf,
		Now:           time.Now,
		Log:           os.Stdout,
		Notify:        signal.Notify,
		Stop:          signal.Stop,
		ShutdownGrace: defaultShutdownGrace,
	}
}

// Execute performs plan with the default executor.
func Execute(ctx context.Context, r Runner, cfg *config.Config, plan modes.Plan, stateDir, imageVersion string) *config.Refusal {
	return New(r).Execute(ctx, cfg, plan, stateDir, imageVersion)
}

// Supervise runs the daemons with the default executor.
func Supervise(ctx context.Context, r Runner, cfg *config.Config) *config.Refusal {
	return New(r).Supervise(ctx, cfg)
}

// Execute performs the plan: initialize the volume if the plan says so,
// record the marker only after that succeeded, and hand over to Supervise.
// Maintenance is the one plan that never starts a daemon.
//
// The marker is written after initialization and never before: a provision
// that fails halfway must leave the volume looking uninitialized, so the next
// start retries instead of starting a broken domain (§6.2).
func (e *Executor) Execute(ctx context.Context, cfg *config.Config, plan modes.Plan, stateDir, imageVersion string) *config.Refusal {
	switch plan.Kind {
	case modes.ActProvision:
		if ref := e.provision(ctx, cfg); ref != nil {
			return ref
		}
		if ref := e.writeMarker(stateDir, imageVersion, string(config.ModeProvision)); ref != nil {
			return ref
		}

	case modes.ActJoin:
		if ref := e.join(ctx, cfg); ref != nil {
			return ref
		}
		if ref := e.ensureDNSUpdateCommand(); ref != nil {
			return ref
		}
		if ref := e.writeMarker(stateDir, imageVersion, string(config.ModeJoin)); ref != nil {
			return ref
		}

	case modes.ActStart:
		// A restart touches nothing: the volume and its marker already
		// agree with this image.

	case modes.ActDBCheckThenStart:
		if plan.AdoptMarker {
			e.logf("the volume holds samba state but no %s marker: checking the database before adopting it", state.MarkerName)
		} else {
			e.logf("the volume was written by an older samba: checking the database before starting %s", imageVersion)
		}
		if ref := e.dbcheck(ctx, false); ref != nil {
			return ref
		}
		if ref := e.writeMarker(stateDir, imageVersion, string(config.ModeRun)); ref != nil {
			return ref
		}
		if plan.AdoptMarker {
			e.logf("volume adopted: marker written for samba %s", imageVersion)
		} else {
			e.logf("marker moved forward to samba %s", imageVersion)
		}

	case modes.ActMaintenance:
		if ref := e.dbcheck(ctx, plan.Repair); ref != nil {
			return ref
		}
		e.logf("database check completed with no errors; maintenance mode does not start the domain controller")
		return nil

	default:
		// Includes modes.ActNone, the zero value meaning "no action
		// decided". Falling through to a default action here would be the
		// worst possible failure mode, so refuse loudly instead.
		return config.Refuse(config.CodeConfigError,
			"internal error: the entrypoint was handed the action %s, which it cannot execute; this is a bug in the image, please report it with the container log",
			plan.Kind)
	}

	return e.Supervise(ctx, cfg)
}

// provision creates a new domain. The admin password is read from its mounted
// file and handed to samba-tool on the command line.
//
// samba-tool domain provision has no environment or stdin channel for the
// initial Administrator password: --adminpass is the only non-interactive
// way in. The exposure is one argv, visible only inside this container's PID
// namespace, during the seconds before any daemon exists (nothing else runs
// in the container at provision time) and to a process able to read /proc of
// a process it already shares a namespace with. Everything this package
// prints — logs and errors alike — is redacted (see redactArgs), so the
// password never reaches a log file, a CI artifact or a crash report.
func (e *Executor) provision(ctx context.Context, cfg *config.Config) *config.Refusal {
	secret, err := config.ReadSecret(cfg.AdminPasswordFile)
	if err != nil {
		return asRefusal(err, config.CodeSecretError)
	}
	e.logf("provisioning a new domain %s in realm %s (functional level %s)", cfg.Domain, cfg.Realm, cfg.FunctionLevel)
	if err := e.Runner.Run(ctx, e.Bin.SambaTool, provisionArgs(cfg, secret)...); err != nil {
		return config.Refuse(config.CodeRuntimeFailure,
			"samba-tool domain provision failed (%s); read the samba-tool output above, fix the cause and start the container again — no state was recorded, so provisioning will be retried",
			scrub(oneLine(err.Error()), secret))
	}
	e.logf("domain %s provisioned", cfg.Domain)
	return nil
}

// join adds this container as an additional DC to an existing domain. The
// same argv note as provision applies to --password.
func (e *Executor) join(ctx context.Context, cfg *config.Config) *config.Refusal {
	secret, err := config.ReadSecret(cfg.JoinPasswordFile)
	if err != nil {
		return asRefusal(err, config.CodeSecretError)
	}
	e.logf("joining realm %s as a domain controller with account %s", cfg.Realm, cfg.JoinUsername)
	if err := e.Runner.Run(ctx, e.Bin.SambaTool, joinArgs(cfg, secret)...); err != nil {
		return config.Refuse(config.CodeRuntimeFailure,
			"samba-tool domain join failed (%s); check that the realm resolves and that %s may join a DC, then start the container again — no state was recorded, so the join will be retried",
			scrub(oneLine(err.Error()), secret), cfg.JoinUsername)
	}
	e.logf("joined realm %s", cfg.Realm)
	return nil
}

// dbcheck runs the database consistency check, optionally repairing.
func (e *Executor) dbcheck(ctx context.Context, repair bool) *config.Refusal {
	args := []string{"dbcheck"}
	if repair {
		args = append(args, "--fix", "--yes")
	}
	if repair {
		e.logf("running samba-tool dbcheck --fix")
	} else {
		e.logf("running samba-tool dbcheck")
	}
	if err := e.Runner.Run(ctx, e.Bin.SambaTool, args...); err != nil {
		remedy := "run the container once with SAMBA_MODE=maintenance and SAMBA_MAINTENANCE_OP=repair, or restore a backup of the volume"
		if repair {
			remedy = "restore a backup of the volume: the errors reported above could not be repaired automatically"
		}
		return config.Refuse(config.CodeDBCheckFailed,
			"samba-tool dbcheck reported errors in the directory database (%s); %s",
			oneLine(err.Error()), remedy)
	}
	return nil
}

// writeMarker records that this image now owns the volume.
func (e *Executor) writeMarker(stateDir, imageVersion, lastMode string) *config.Refusal {
	m := state.Marker{
		SambaVersion:  imageVersion,
		InitializedAt: e.Now().UTC().Format(time.RFC3339),
		LastMode:      lastMode,
	}
	if err := state.WriteMarker(stateDir, m); err != nil {
		return asRefusal(err, config.CodeRuntimeFailure)
	}
	return nil
}

// ensureDNSUpdateCommand makes sure the smb.conf generated by a join carries
// the samba-tool DNS update command. provision receives it as --option;
// samba-tool domain join has no equivalent passthrough, so the generated file
// is edited once, right after the join.
func (e *Executor) ensureDNSUpdateCommand() *config.Refusal {
	data, err := os.ReadFile(e.SMBConfPath)
	if err != nil {
		return config.Refuse(config.CodeRuntimeFailure,
			"the configuration file %q that samba-tool domain join should have written cannot be read (%s); check that /etc/samba is writable and inspect the join output above",
			e.SMBConfPath, oneLine(err.Error()))
	}
	updated, changed := withDNSUpdateCommand(string(data))
	if !changed {
		return nil
	}
	if err := os.WriteFile(e.SMBConfPath, []byte(updated), 0o644); err != nil {
		return config.Refuse(config.CodeRuntimeFailure,
			"the DNS update command cannot be written into %q (%s); mount /etc/samba read-write",
			e.SMBConfPath, oneLine(err.Error()))
	}
	e.logf("added %q to %s: this image ships no nsupdate, so samba_dnsupdate must use samba-tool", dnsUpdateCommand, e.SMBConfPath)
	return nil
}

// Supervise starts chronyd (when enabled) and then samba in the foreground,
// and stays until one of them ends or the container is asked to stop. A
// signal stops samba first and chronyd second: the directory should leave the
// network before the time service it depends on.
func (e *Executor) Supervise(ctx context.Context, cfg *config.Config) *config.Refusal {
	if ref := e.makeRuntimeDirs(); ref != nil {
		return ref
	}

	sigs := make(chan os.Signal, 2)
	e.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer e.Stop(sigs)

	var chrony Proc
	var chronyDone chan error
	if cfg.Chrony {
		p, err := e.Runner.Start(ctx, e.Bin.Chronyd, chronyArgs(e.ChronyConf)...)
		if err != nil {
			return config.Refuse(config.CodeRuntimeFailure,
				"chronyd could not be started (%s); set SAMBA_CHRONY=off to run without the MS-SNTP time service, or fix the reported cause",
				oneLine(err.Error()))
		}
		chrony, chronyDone = p, waitChan(p)
	}

	samba, err := e.Runner.Start(ctx, e.Bin.Samba, sambaArgs(cfg.LogLevel)...)
	if err != nil {
		e.stopProc(ctx, "chronyd", chrony, chronyDone)
		return config.Refuse(config.CodeRuntimeFailure,
			"samba could not be started (%s); this is a fault of the image or of the mounted configuration, check the output above",
			oneLine(err.Error()))
	}
	sambaDone := waitChan(samba)

	for {
		select {
		case err := <-sambaDone:
			e.stopProc(ctx, "chronyd", chrony, chronyDone)
			if err != nil {
				return config.Refuse(config.CodeRuntimeFailure,
					"samba exited unexpectedly (%s); read the samba log above — the container is restarted by its restart policy, the state volume is untouched",
					oneLine(err.Error()))
			}
			e.logf("samba exited cleanly")
			return nil

		case err := <-chronyDone:
			// chrony serves time, it is not the directory: losing it
			// degrades MS-SNTP signing but must not take the DC down.
			chrony, chronyDone = nil, nil
			e.logf("chronyd exited (%v); the domain controller keeps running but signed NTP is no longer served", err)

		case sig := <-sigs:
			e.logf("received %s: stopping samba, then chronyd", sig)
			return e.shutdown(ctx, samba, sambaDone, chrony, chronyDone)

		case <-ctx.Done():
			e.logf("shutdown requested: stopping samba, then chronyd")
			return e.shutdown(ctx, samba, sambaDone, chrony, chronyDone)
		}
	}
}

// shutdown stops the daemons in order and reports a clean exit: a container
// asked to stop has not failed.
func (e *Executor) shutdown(ctx context.Context, samba Proc, sambaDone chan error, chrony Proc, chronyDone chan error) *config.Refusal {
	e.stopProc(ctx, "samba", samba, sambaDone)
	e.stopProc(ctx, "chronyd", chrony, chronyDone)
	return nil
}

// stopProc asks one daemon to stop and waits for it to be reaped. The grace
// window deliberately survives a cancelled parent context: when cancellation
// is what triggered the shutdown, an already-expired context would turn the
// orderly stop into an immediate kill.
func (e *Executor) stopProc(ctx context.Context, name string, p Proc, done chan error) {
	if p == nil {
		return
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		e.logf("could not signal %s (%v); waiting for it anyway", name, err)
	}

	graceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.grace())
	defer cancel()
	select {
	case err := <-done:
		if err != nil {
			e.logf("%s stopped (%v)", name, err)
		} else {
			e.logf("%s stopped", name)
		}
		return
	case <-graceCtx.Done():
	}

	e.logf("%s did not stop within %s: killing it", name, e.grace())
	_ = p.Signal(os.Kill)
	killCtx, cancelKill := context.WithTimeout(context.WithoutCancel(ctx), killGrace)
	defer cancelKill()
	select {
	case <-done:
	case <-killCtx.Done():
		e.logf("%s could not be reaped; leaving it to the init process", name)
	}
}

// makeRuntimeDirs creates the directories the daemons expect under /run.
func (e *Executor) makeRuntimeDirs() *config.Refusal {
	root := e.Root
	if root == "" {
		root = defaultRoot
	}
	for _, d := range runtimeDirs {
		path := filepath.Join(root, filepath.FromSlash(d))
		if err := os.MkdirAll(path, 0o755); err != nil {
			return config.Refuse(config.CodeRuntimeFailure,
				"runtime directory %q cannot be created (%s); /run must be writable (mount it as tmpfs when the root filesystem is read-only)",
				path, oneLine(err.Error()))
		}
	}
	return nil
}

// grace returns the configured shutdown grace, defaulted.
func (e *Executor) grace() time.Duration {
	if e.ShutdownGrace <= 0 {
		return defaultShutdownGrace
	}
	return e.ShutdownGrace
}

// logf writes one operator-facing line to the container log (§6.4).
func (e *Executor) logf(format string, args ...any) {
	if e.Log == nil {
		return
	}
	fmt.Fprintf(e.Log, "entrypoint: "+format+"\n", args...)
}

// waitChan reaps p in the background and delivers the result once.
func waitChan(p Proc) chan error {
	ch := make(chan error, 1)
	go func() { ch <- p.Wait() }()
	return ch
}

// provisionArgs builds the samba-tool domain provision command line.
func provisionArgs(cfg *config.Config, secret string) []string {
	args := []string{
		"domain", "provision",
		"--server-role=dc",
		"--use-rfc2307",
		"--dns-backend=" + cfg.DNSBackend,
		"--realm=" + cfg.Realm,
		"--domain=" + cfg.Domain,
		"--function-level=" + cfg.FunctionLevel,
	}
	if cfg.DNSForwarder != "" {
		args = append(args, "--option=dns forwarder="+cfg.DNSForwarder)
	}
	args = append(args, "--option="+dnsUpdateCommand)
	return append(args, "--adminpass="+secret)
}

// joinArgs builds the samba-tool domain join command line.
func joinArgs(cfg *config.Config, secret string) []string {
	return []string{
		"domain", "join",
		cfg.Realm, "DC",
		"-U" + cfg.JoinUsername,
		"--dns-backend=" + cfg.DNSBackend,
		"--password=" + secret,
	}
}

// sambaArgs builds the samba command line: foreground, no process group of
// its own (so the entrypoint stays in control of shutdown) and logging to
// stdout (§6.4).
func sambaArgs(logLevel int) []string {
	return []string{"--foreground", "--no-process-group", "--debug-stdout", "-d", strconv.Itoa(logLevel)}
}

// chronyArgs builds the chronyd command line: foreground with logging to
// stderr (-d) and never stepping the clock (-x), because the container shares
// the host's clock and must not try to discipline it (B.3).
func chronyArgs(conf string) []string {
	return []string{"-d", "-x", "-f", conf}
}

// withDNSUpdateCommand returns conf with the DNS update command present in
// its [global] section, and reports whether anything changed. It is pure and
// idempotent: a join that repeats must not append the option twice.
func withDNSUpdateCommand(conf string) (string, bool) {
	if hasDNSUpdateCommand(conf) {
		return conf, false
	}
	entry := "\t" + dnsUpdateCommand

	lines := strings.Split(conf, "\n")
	for i, l := range lines {
		if strings.EqualFold(strings.TrimSpace(l), "[global]") {
			out := make([]string, 0, len(lines)+1)
			out = append(out, lines[:i+1]...)
			out = append(out, entry)
			out = append(out, lines[i+1:]...)
			return strings.Join(out, "\n"), true
		}
	}

	// No [global] section at all: create one at the end rather than guess
	// where it belongs.
	out := strings.TrimRight(conf, "\n")
	if out != "" {
		out += "\n\n"
	}
	return out + "[global]\n" + entry + "\n", true
}

// hasDNSUpdateCommand reports whether conf already sets the option. Comments
// do not count: a commented-out line is exactly the case where the option
// must be added.
func hasDNSUpdateCommand(conf string) bool {
	for _, l := range strings.Split(conf, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
			continue
		}
		k, _, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.Join(strings.Fields(k), " "), "dns update command") {
			return true
		}
	}
	return false
}

// asRefusal recovers the *config.Refusal an inner package returned, or wraps
// a plain error with the given code so a caller always gets an exit code.
func asRefusal(err error, code int) *config.Refusal {
	var ref *config.Refusal
	if errors.As(err, &ref) {
		return ref
	}
	return config.Refuse(code, "%s", oneLine(err.Error()))
}

// scrub removes a secret from a message built from an external error. The
// Runner already redacts what it renders; this is the belt to that braces.
func scrub(msg, secret string) string {
	if secret == "" {
		return msg
	}
	return strings.ReplaceAll(msg, secret, redactedValue)
}

// oneLine collapses an error string so refusal messages stay single-line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
