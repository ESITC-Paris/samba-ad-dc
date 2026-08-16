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
const (
	dnsUpdateKey     = "dns update command"
	dnsUpdateValue   = "/usr/sbin/samba_dnsupdate --use-samba-tool"
	dnsUpdateCommand = dnsUpdateKey + " = " + dnsUpdateValue
)

// dcFunctionalLevelKey is the smb.conf parameter that sets the functional
// level this DC itself advertises. See dcFunctionalLevel for why provision
// has to set it.
const dcFunctionalLevelKey = "ad dc functional level"

// Default paths and programs.
const (
	defaultSMBConf    = "/etc/samba/smb.conf"
	defaultChronyConf = "/etc/chrony/chrony.conf"
	defaultRoot       = "/"
	// defaultShutdownGrace bounds the orderly stop of each daemon (§6.3:
	// samba then chrony within 10 s).
	defaultShutdownGrace = 10 * time.Second
	// defaultKillGrace bounds the wait after escalating to SIGKILL. It is
	// shared by both daemons, like the grace window itself.
	defaultKillGrace = 2 * time.Second
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
	KillGrace     time.Duration
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
		KillGrace:     defaultKillGrace,
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
	// Before anything shells out, not just before the daemons start:
	// samba-tool domain provision writes into /var/lock/samba (a symlink
	// into the tmpfs /run) while it works, and on a read-only rootfs with a
	// fresh /run that directory does not exist yet. Supervise creates them
	// too — MkdirAll is idempotent — so the Supervise-only entry point
	// keeps working unchanged.
	if ref := e.makeRuntimeDirs(); ref != nil {
		return ref
	}

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
		if ref := e.refreshMarker(stateDir, imageVersion); ref != nil {
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

// writeMarker records that this image now owns the volume, stamping the
// current time as the moment the volume was initialized.
func (e *Executor) writeMarker(stateDir, imageVersion, lastMode string) *config.Refusal {
	return e.putMarker(stateDir, imageVersion, lastMode, e.now())
}

// refreshMarker moves an existing marker forward to this image version after
// a successful database check. initialized_at answers "when was this domain
// created", not "when was it last checked", so the original value is carried
// over; a volume being adopted has no original and is stamped now.
func (e *Executor) refreshMarker(stateDir, imageVersion string) *config.Refusal {
	initializedAt := ""
	obs, err := state.Observe(stateDir)
	if err != nil {
		return asRefusal(err, config.CodeConfigError)
	}
	if obs.Marker != nil {
		initializedAt = strings.TrimSpace(obs.Marker.InitializedAt)
	}
	if initializedAt == "" {
		initializedAt = e.now()
	}
	return e.putMarker(stateDir, imageVersion, string(config.ModeRun), initializedAt)
}

// putMarker writes the marker with an explicit initialized_at.
func (e *Executor) putMarker(stateDir, imageVersion, lastMode, initializedAt string) *config.Refusal {
	m := state.Marker{
		SambaVersion:  imageVersion,
		InitializedAt: initializedAt,
		LastMode:      lastMode,
	}
	if err := state.WriteMarker(stateDir, m); err != nil {
		return asRefusal(err, config.CodeRuntimeFailure)
	}
	return nil
}

// now renders the current time in the marker's format.
func (e *Executor) now() string { return e.Now().UTC().Format(time.RFC3339) }

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
	updated, edit := withDNSUpdateCommand(string(data))
	if edit == confUnchanged {
		return nil
	}
	// smb.conf is the configuration the DC reads on every start: replace it
	// atomically so a failed write cannot leave samba with a truncated file.
	if err := writeFileAtomic(e.SMBConfPath, []byte(updated), 0o644); err != nil {
		return config.Refuse(config.CodeRuntimeFailure,
			"the DNS update command cannot be written into %q (%s); mount /etc/samba read-write",
			e.SMBConfPath, oneLine(err.Error()))
	}
	switch edit {
	case confReplaced:
		e.logf("replaced the %q setting in %s with %q: this image ships no nsupdate, so samba_dnsupdate must use samba-tool",
			dnsUpdateKey, e.SMBConfPath, dnsUpdateValue)
	default:
		e.logf("added %q to %s: this image ships no nsupdate, so samba_dnsupdate must use samba-tool",
			dnsUpdateCommand, e.SMBConfPath)
	}
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

	// The daemons are started under a context that cannot be cancelled:
	// this function owns every signal they receive. Handing them ctx would
	// let its cancellation SIGTERM both of them at once, behind the back of
	// the ordered shutdown below — samba would lose its head start.
	daemonCtx := context.WithoutCancel(ctx)

	var chrony Proc
	var chronyDone chan error
	if cfg.Chrony {
		p, err := e.Runner.Start(daemonCtx, e.Bin.Chronyd, chronyArgs(e.ChronyConf)...)
		if err != nil {
			return config.Refuse(config.CodeRuntimeFailure,
				"chronyd could not be started (%s); set SAMBA_CHRONY=off to run without the MS-SNTP time service, or fix the reported cause",
				oneLine(err.Error()))
		}
		chrony, chronyDone = p, waitChan(p)
	}

	samba, err := e.Runner.Start(daemonCtx, e.Bin.Samba, sambaArgs(cfg.LogLevel)...)
	if err != nil {
		e.stopOne(ctx, "chronyd", chrony, chronyDone)
		return config.Refuse(config.CodeRuntimeFailure,
			"samba could not be started (%s); this is a fault of the image or of the mounted configuration, check the output above",
			oneLine(err.Error()))
	}
	sambaDone := waitChan(samba)

	for {
		select {
		case err := <-sambaDone:
			e.stopOne(ctx, "chronyd", chrony, chronyDone)
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
//
// The two stops share one budget rather than each getting their own: §6.3
// gives the container 10 s to stop samba AND chrony, so per-daemon windows
// would add up to more than the contract allows and the container would be
// SIGKILLed by the runtime mid-shutdown. samba gets whatever it needs of the
// budget first, and chronyd gets the rest.
func (e *Executor) shutdown(ctx context.Context, samba Proc, sambaDone chan error, chrony Proc, chronyDone chan error) *config.Refusal {
	term, kill := e.shutdownBudget(ctx)
	defer term.stop()
	defer kill.stop()
	e.stopProc(term.ctx, kill.ctx, "samba", samba, sambaDone)
	e.stopProc(term.ctx, kill.ctx, "chronyd", chrony, chronyDone)
	return nil
}

// budget is a deadline with its cancel function.
type budget struct {
	ctx  context.Context
	stop context.CancelFunc
}

// shutdownBudget derives the two shared deadlines of one shutdown: the grace
// window for the polite SIGTERM, and a slightly longer one that bounds the
// SIGKILL escalation for both daemons together.
//
// Both deliberately survive a cancelled parent context: when cancellation is
// what triggered the shutdown, an already-expired context would turn the
// orderly stop into an immediate kill.
func (e *Executor) shutdownBudget(ctx context.Context) (budget, budget) {
	base := context.WithoutCancel(ctx)
	termCtx, termCancel := context.WithTimeout(base, e.grace())
	killCtx, killCancel := context.WithTimeout(base, e.grace()+e.killGrace())
	return budget{termCtx, termCancel}, budget{killCtx, killCancel}
}

// stopOne stops a single daemon on its own budget. It is used on the paths
// where only one daemon is left running (samba failed to start, or samba
// exited by itself), so there is nothing to share the budget with.
func (e *Executor) stopOne(ctx context.Context, name string, p Proc, done chan error) {
	term, kill := e.shutdownBudget(ctx)
	defer term.stop()
	defer kill.stop()
	e.stopProc(term.ctx, kill.ctx, name, p, done)
}

// stopProc asks one daemon to stop and waits for it to be reaped, within the
// shared budgets. A daemon that ignores SIGTERM is killed once the grace
// window is spent; one that cannot even be reaped is left to the init
// process (tini) rather than blocking the shutdown of the other.
func (e *Executor) stopProc(termCtx, killCtx context.Context, name string, p Proc, done chan error) {
	if p == nil {
		return
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		e.logf("could not signal %s (%v); waiting for it anyway", name, err)
	}

	select {
	case err := <-done:
		if err != nil {
			e.logf("%s stopped (%v)", name, err)
		} else {
			e.logf("%s stopped", name)
		}
		return
	case <-termCtx.Done():
	}

	e.logf("%s did not stop within the %s shutdown budget: killing it", name, e.grace())
	_ = p.Signal(os.Kill)
	select {
	case <-done:
		e.logf("%s killed", name)
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

// killGrace returns the configured post-SIGKILL reap window, defaulted.
func (e *Executor) killGrace() time.Duration {
	if e.KillGrace <= 0 {
		return defaultKillGrace
	}
	return e.KillGrace
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
	if v, ok := dcFunctionalLevel(cfg.FunctionLevel); ok {
		args = append(args, "--option="+dcFunctionalLevelKey+" = "+v)
	}
	if cfg.DNSForwarder != "" {
		args = append(args, "--option=dns forwarder="+cfg.DNSForwarder)
	}
	args = append(args, "--option="+dnsUpdateCommand)
	return append(args, "--adminpass="+secret)
}

// dcFunctionalLevel maps a requested domain/forest function level onto the
// value the `ad dc functional level` smb.conf parameter must carry, and
// reports whether the parameter has to be set at all.
//
// Since Samba 4.19 that parameter defaults to 2008_R2 and provision refuses
// outright when the requested domain and forest level is higher than it
// ("You want to run SAMBA 4 on a domain and forest function level which
// itself is higher than its actual DC function level"). The default
// SAMBA_FUNCTION_LEVEL of 2016 therefore cannot provision without also
// raising this parameter — which is why it is passed as --option, so
// provision writes it into the generated smb.conf and every later start of
// the container keeps the level the domain was created at.
//
// Levels at or below the default need nothing: the parameter does not
// accept 2000, 2003 or 2008 as values, so setting it for those would turn a
// working provision into a configuration error.
func dcFunctionalLevel(functionLevel string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(functionLevel)) {
	case "2012":
		return "2012", true
	case "2012_R2":
		return "2012_R2", true
	case "2016":
		return "2016", true
	default:
		return "", false
	}
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

// confEdit says what withDNSUpdateCommand did to a file.
type confEdit int

const (
	confUnchanged confEdit = iota
	confAdded
	confReplaced
)

// withDNSUpdateCommand returns conf with the DNS update command set to this
// image's value in the [global] section, and says what it changed. It is pure
// and idempotent: a join that repeats must not append the option twice.
//
// Detection is scoped to [global] and matches the value, not only the key:
// the same key in another section does not configure the DC, and a [global]
// entry pointing at a different command — for instance the samba default,
// which shells out to the nsupdate this image does not ship — is worse than
// no entry at all, so it is replaced rather than trusted.
func withDNSUpdateCommand(conf string) (string, confEdit) {
	lines := strings.Split(conf, "\n")
	entry := "\t" + dnsUpdateCommand

	section := ""
	globalAt := -1
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
			continue
		}
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			section = strings.ToLower(strings.TrimSpace(t[1 : len(t)-1]))
			if section == "global" && globalAt < 0 {
				globalAt = i
			}
			continue
		}
		if section != "global" {
			continue
		}
		k, v, ok := strings.Cut(t, "=")
		if !ok || !strings.EqualFold(normalize(k), dnsUpdateKey) {
			continue
		}
		if normalize(v) == dnsUpdateValue {
			return conf, confUnchanged
		}
		out := make([]string, len(lines))
		copy(out, lines)
		out[i] = entry
		return strings.Join(out, "\n"), confReplaced
	}

	if globalAt >= 0 {
		out := make([]string, 0, len(lines)+1)
		out = append(out, lines[:globalAt+1]...)
		out = append(out, entry)
		out = append(out, lines[globalAt+1:]...)
		return strings.Join(out, "\n"), confAdded
	}

	// No [global] section at all: create one at the end rather than guess
	// where it belongs.
	out := strings.TrimRight(conf, "\n")
	if out != "" {
		out += "\n\n"
	}
	return out + "[global]\n" + entry + "\n", confAdded
}

// normalize collapses the whitespace of one smb.conf key or value so that
// spacing never decides whether a setting is recognized.
func normalize(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// writeFileAtomic replaces path with data through a temp file and a rename,
// so a reader (samba, on its next start) sees either the old file or the new
// one and never a half-written one. No temp file survives a failure.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
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
