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
// has to set it, and joinedConfSettings for why a join has to as well.
const dcFunctionalLevelKey = "ad dc functional level"

// dnsForwarderKey is the smb.conf parameter naming the upstream this DC's
// internal DNS sends the names it does not serve to. See joinedConfSettings
// for why a DC without one is not merely less useful but slow enough to
// break Kerberos.
const dnsForwarderKey = "dns forwarder"

// ntpSigndDirKey is the smb.conf parameter naming the directory samba
// creates the MS-SNTP signing socket in, and ntpSigndDirective is the
// chrony.conf directive that has to name the SAME directory or the signing
// hand-off silently does nothing. Nothing in samba or in chrony reconciles
// the two: see chronyConfig for who does.
const (
	ntpSigndDirKey    = "ntp signd socket directory"
	ntpSigndDirective = "ntpsigndsocket"
)

// Default paths and programs.
const (
	defaultSMBConf = "/etc/samba/smb.conf"
	// defaultChronyTemplate is the chrony configuration baked into the
	// image. It is a TEMPLATE, not the file chronyd reads: everything in
	// it is final except the ntpsigndsocket directive, which is rewritten
	// per boot from the DC's own smb.conf.
	defaultChronyTemplate = "/etc/chrony/chrony.conf"
	// defaultChronyConf is the effective configuration, generated from
	// that template before chronyd starts. It lives under /run because the
	// root filesystem is read-only (B.2) and /run is the tmpfs the
	// entrypoint already creates.
	defaultChronyConf = "/run/chrony/chrony.conf"
	defaultRoot       = "/"
	// defaultShutdownGrace is the TOTAL an orderly stop may take: samba and
	// chrony together, SIGTERM phase and SIGKILL escalation included (§6.3:
	// samba then chrony within 10 s). Nothing in a shutdown is allowed to
	// push past it — after it the container runtime kills the container
	// itself, and a samba killed mid-write is the outcome the ordered
	// shutdown exists to avoid.
	defaultShutdownGrace = 10 * time.Second
	// defaultKillGrace is the slice of that total held back to reap what
	// SIGKILL leaves behind. It is subtracted from the grace window, not
	// added to it.
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
	Testparm  string
}

// DefaultBinaries resolves the programs through PATH, as the image installs
// them.
func DefaultBinaries() Binaries {
	return Binaries{
		SambaTool: "samba-tool",
		Samba:     "samba",
		Chronyd:   "chronyd",
		Smbclient: "smbclient",
		Testparm:  "testparm",
	}
}

// Executor carries everything Execute and Supervise need that is not part of
// the operator-visible configuration: the shell-out seam, the program names,
// the filesystem root, the clock and the signal plumbing. Production uses the
// defaults from New; tests replace the parts they need to observe.
type Executor struct {
	Runner         Runner
	Bin            Binaries
	Root           string // filesystem root under which runtime dirs are made
	SMBConfPath    string
	ChronyTemplate string // baked-in template, read
	ChronyConf     string // generated effective config, written, and what chronyd reads
	Now            func() time.Time
	Log            io.Writer
	Notify         func(c chan<- os.Signal, sig ...os.Signal)
	Stop           func(c chan<- os.Signal)
	ShutdownGrace  time.Duration
	KillGrace      time.Duration
}

// New returns an Executor wired for the container.
func New(r Runner) *Executor {
	return &Executor{
		Runner:         r,
		Bin:            DefaultBinaries(),
		Root:           defaultRoot,
		SMBConfPath:    defaultSMBConf,
		ChronyTemplate: defaultChronyTemplate,
		ChronyConf:     defaultChronyConf,
		Now:            time.Now,
		Log:            os.Stdout,
		Notify:         signal.Notify,
		Stop:           signal.Stop,
		ShutdownGrace:  defaultShutdownGrace,
		KillGrace:      defaultKillGrace,
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
// The marker is written after initialization and never before: only a
// completed initialization may claim the volume, so no later start can mistake
// a half-provisioned tree for one this image owns (§6.2). That is a guarantee
// about the marker, not about the tree: samba-tool creates private/sam.ldb
// early, so a failed initialization can still leave partial state behind — the
// next boot then sees state without a marker and refuses (20) or adopts it and
// fails the database check (23), which is why the failure messages below say
// to delete the volume rather than promising a free retry.
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

	// The operator's own LDAPS material is checked before anything shells
	// out, because provision names those paths on samba-tool's command line
	// and every start writes them into smb.conf. A path that is not there has
	// to stop the boot with one clear line here; left to samba it would not
	// stop anything, since a file it cannot use sends it back to its own
	// self-signed material — a container that comes up healthy serving a
	// certificate nobody vouched for.
	//
	// Maintenance is excluded for the same reason it applies no [global]
	// settings at all (see ensureGlobalOptions): it starts no listener, and
	// material it will never use must not stand between an operator and the
	// database check they reached for because their DC will not run.
	if plan.Kind != modes.ActMaintenance {
		if err := config.CheckTLSMaterial(cfg); err != nil {
			return asRefusal(err, config.CodeSecretError)
		}
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
		if ref := e.ensureJoinedConf(cfg); ref != nil {
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

	// Every path that is about to start the daemons passes here, and only
	// those: the declared [global] settings are CONFIGURATION, not state
	// (§6.2). State is the directory database, which a restart never
	// touches; configuration is reconciled on every start, so changing the
	// variable and recreating the container is all an operator has to do.
	// Provision has already passed them to samba-tool with --option, so on
	// that path the reconcile normally finds them in place and does
	// nothing. It runs there anyway: nothing should depend on samba-tool
	// having written every one of them into the file it generated.
	if ref := e.ensureGlobalOptions(ctx, cfg); ref != nil {
		return ref
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
			"samba-tool domain provision failed (%s) and no version marker was written; inspect the logs; if the volume now holds partial state, delete/recreate it before retrying",
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
			"samba-tool domain join failed (%s) and no version marker was written; inspect the logs and check that the realm resolves and that %s may join a DC; if the volume now holds partial state, delete/recreate it before retrying",
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

// smbConfSetting is one [global] entry the entrypoint forces into the
// smb.conf a join generated, together with the reason it does so — the
// reason is logged, because an operator reading their configuration file
// back must find out from the container log who changed it and why.
type smbConfSetting struct{ key, value, why string }

// joinedConfSettings lists what the smb.conf written by `samba-tool domain
// join` is missing. provision passes every one of these with --option;
// samba-tool domain join has no equivalent passthrough — it renders its own
// configuration file from a template — so the generated file is edited once,
// right after the join. Without this, three variables an operator sets would
// silently do nothing on a joined DC.
//
// The functional level is not cosmetic: samba's dsdb_check_and_update_fl
// REFUSES to start a DC whose smb.conf level is below the domain's ("Refusing
// to start as smb.conf 'ad dc functional level' maps to 4, which is less than
// the domain functional level of 7"). Since Samba 4.19 the parameter defaults
// to 2008_R2, so a DC joining a domain provisioned at this image's default
// level of 2016 joins successfully and then cannot boot — which is exactly
// what an E2E two-DC run observed. Mirroring SAMBA_FUNCTION_LEVEL here is the
// join-side half of what provisionArgs already does.
//
// Nor is the DNS forwarder: samba's internal DNS with no upstream takes
// SECONDS to fail a query it is not authoritative for, rather than answering
// immediately (measured at 4-8 s against this image). A domain controller
// that resolves through itself — which a multi-DC domain requires, because a
// replication partner is addressed by a `_msdcs` CNAME only the directory's
// own DNS can answer — then pays that stall on every Kerberos bind, and the
// sealed DRSUAPI bind that carries replication times out before it finishes.
// SAMBA_DNS_FORWARDER is therefore load-bearing for a joined DC, and it is
// exactly the DC an operator would have had no way to set it on.
func joinedConfSettings(cfg *config.Config) []smbConfSetting {
	settings := []smbConfSetting{{
		key:   dnsUpdateKey,
		value: dnsUpdateValue,
		why:   "this image ships no nsupdate, so samba_dnsupdate must use samba-tool",
	}}
	if v, ok := dcFunctionalLevel(cfg.FunctionLevel); ok {
		settings = append(settings, smbConfSetting{
			key:   dcFunctionalLevelKey,
			value: v,
			why:   "samba refuses to start a domain controller whose own functional level is below the domain's",
		})
	}
	if f := strings.TrimSpace(cfg.DNSForwarder); f != "" {
		settings = append(settings, smbConfSetting{
			key:   dnsForwarderKey,
			value: f,
			why:   "without an upstream, this DC's DNS stalls for seconds on every name it does not serve, which times out Kerberos and replication",
		})
	}
	return settings
}

// ensureJoinedConf makes the smb.conf generated by a join carry every
// setting joinedConfSettings names, in one atomic rewrite.
func (e *Executor) ensureJoinedConf(cfg *config.Config) *config.Refusal {
	data, err := os.ReadFile(e.SMBConfPath)
	if err != nil {
		return config.Refuse(config.CodeRuntimeFailure,
			"the configuration file %q that samba-tool domain join should have written cannot be read (%s); check that /etc/samba is writable and inspect the join output above",
			e.SMBConfPath, oneLine(err.Error()))
	}

	conf := string(data)
	var announcements []string
	for _, s := range joinedConfSettings(cfg) {
		updated, edit := withGlobalSetting(conf, s.key, s.value)
		if edit == confUnchanged {
			continue
		}
		conf = updated
		verb := "added"
		if edit == confReplaced {
			verb = "replaced"
		}
		announcements = append(announcements,
			fmt.Sprintf("%s %q = %q in %s: %s", verb, s.key, s.value, e.SMBConfPath, s.why))
	}
	if len(announcements) == 0 {
		return nil
	}

	// smb.conf is the configuration the DC reads on every start: replace it
	// atomically so a failed write cannot leave samba with a truncated file.
	if err := writeFileAtomic(e.SMBConfPath, []byte(conf), 0o644); err != nil {
		return config.Refuse(config.CodeRuntimeFailure,
			"the settings a joined domain controller needs cannot be written into %q (%s); mount /etc/samba read-write",
			e.SMBConfPath, oneLine(err.Error()))
	}
	for _, a := range announcements {
		e.logf("%s", a)
	}
	return nil
}

// ensureGlobalOptions reconciles the declared [global] settings — the
// operator's own LDAPS material from the SAMBA_TLS_* variables first, then
// the SAMBA_GLOBAL_OPTIONS block — into the smb.conf on the configuration
// volume, and refuses the boot when samba's own parser rejects the result.
//
// It runs on every start rather than only on the boot that initialized the
// volume, because the variable is what an operator edits: a setting that only
// took effect on a freshly provisioned domain would be a setting nobody can
// change on a DC that already exists — which is every DC past its first day.
//
// It ADDS and REPLACES; it does not remove. Dropping an entry from the
// variable leaves the line it wrote in smb.conf, because nothing here can
// tell a line this code wrote last boot from one the operator wrote by hand,
// and deleting somebody else's configuration on a guess is worse than leaving
// a stale setting behind. The documented remedy is to give the setting the
// value you want — samba's default, spelled out — or to edit the file on the
// volume. (Runtime contract, Declarative configuration; deployment guide
// §3.5.)
//
// Maintenance mode is deliberately not among its callers (see Execute): an
// operator reaching for it is diagnosing a DC that will not run, and a mode
// that edited the configuration on the way past would change the very thing
// they are looking at.
func (e *Executor) ensureGlobalOptions(ctx context.Context, cfg *config.Config) *config.Refusal {
	options := cfg.EffectiveGlobalOptions()
	if len(options) == 0 {
		return nil
	}
	for _, key := range cfg.GlobalOptionsShadowed {
		e.logf("SAMBA_GLOBAL_OPTIONS: %q is set more than once; the last occurrence wins", key)
	}

	original, err := os.ReadFile(e.SMBConfPath)
	if err != nil {
		return config.Refuse(config.CodeRuntimeFailure,
			"the configuration file %q that the declared [global] settings must be applied to cannot be read (%s); mount /etc/samba read-write, or unset SAMBA_GLOBAL_OPTIONS and the SAMBA_TLS_* variables",
			e.SMBConfPath, oneLine(err.Error()))
	}

	conf := string(original)
	var announcements, sources []string
	for _, o := range options {
		updated, edit := withGlobalSetting(conf, o.Key, o.Value)
		if edit == confUnchanged {
			continue
		}
		conf = updated
		verb := "added"
		if edit == confReplaced {
			verb = "replaced"
		}
		// Each line names the variable that asked for THIS setting, not the
		// variable next to it: an operator reading their log has to be sent
		// to the thing they can edit.
		source := config.OptionSource(o.Key)
		announcements = append(announcements,
			fmt.Sprintf("%s: %s %q = %q in %s", source, verb, o.Key, o.Value, e.SMBConfPath))
		if !containsString(sources, source) {
			sources = append(sources, source)
		}
	}
	// Nothing to do is the steady state, and it says nothing: an operator
	// who changed nothing must see no configuration noise at all, and the
	// file samba reads must not be rewritten on every single boot.
	if len(announcements) == 0 {
		return nil
	}

	if err := writeFileAtomic(e.SMBConfPath, []byte(conf), 0o644); err != nil {
		return config.Refuse(config.CodeRuntimeFailure,
			"the [global] settings %s declares cannot be written into %q (%s); mount /etc/samba read-write",
			strings.Join(sources, " and "), e.SMBConfPath, oneLine(err.Error()))
	}
	// The gate first, the announcements after it: the log must record what
	// the domain controller is actually running with. A boot that announced
	// "replaced max log size" and then put the previous file back would leave
	// an operator reading their log for a setting that is not in force.
	if ref := e.checkSMBConf(ctx, original, strings.Join(sources, " and ")); ref != nil {
		return ref
	}
	for _, a := range announcements {
		e.logf("%s", a)
	}
	return nil
}

// containsString reports whether values holds want.
func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// checkSMBConf asks testparm whether the file that was just rewritten is a
// configuration samba can load, and puts the previous one back when it is
// not.
//
// The restore is the point. smb.conf lives on a volume, so a rewrite that
// samba cannot parse would outlive the container that made it and break every
// later start — including the one an operator makes right after removing the
// offending variable. Refusing with the file already back to what it held
// means the remedy is exactly "fix the variable and start again".
//
// What counts as a rejection is MEASURED, not guessed, because both mistakes
// are expensive: missing a real error lets a silently-ignored setting through,
// and reading a harmless remark as an error refuses to boot a domain
// controller whose configuration is fine. Every line below was produced by
// `testparm -s -l --debug-stdout` against samba 4.24.7 in this image:
//
//	Unknown parameter encountered: "this is not a parameter"      exit 0
//	Ignoring unknown parameter "this is not a parameter"          exit 0
//	WARNING: Ignoring invalid value 'bogus' for parameter 'smb encrypt'   exit 1
//	set_variable_helper(notanumber): value is not a valid size specifier! exit 1
//	lpcfg_do_global_parameter: WARNING: The "syslog only" option is deprecated   exit 0
//
// The last one is why a keyword like "WARNING" cannot be the test: a
// deprecated-but-accepted parameter — `syslog only`, `lanman auth`, `domain
// logons` and a dozen others in 4.24 — prints it, exits 0, and is a
// configuration samba loads happily. It is logged and the boot continues.
//
// So the verdict is: a non-zero exit, or the unknown-parameter shape, which
// is the one real error that exits 0 (and the exact case an operator's typo
// produces). Everything else testparm says is passed on to the log.
//
// The diagnostics are DEBUG output, which samba writes to stderr by default —
// where Output deliberately does not capture it (see Runner.Output) — so
// `--debug-stdout` moves them onto the stream this can read. testparm's own
// banner stays on stderr and still reaches the container log.
func (e *Executor) checkSMBConf(ctx context.Context, original []byte, sources string) *config.Refusal {
	out, err := e.Runner.Output(ctx, e.Bin.Testparm, testparmCheckArgs(e.SMBConfPath)...)
	diagnostics := testparmDiagnostics(out)

	problem := ""
	for _, d := range diagnostics {
		if testparmRejects(d) {
			problem = d
			break
		}
	}
	if err == nil && problem == "" {
		// Accepted. Whatever testparm still had to say — a deprecation
		// warning, most often — is the operator's business and reaches
		// their log, but it is not a reason to refuse their DC.
		for _, d := range diagnostics {
			e.logf("%s: testparm says: %s", sources, d)
		}
		return nil
	}
	if problem == "" {
		problem = testparmCause(diagnostics, err)
	}

	if rerr := writeFileAtomic(e.SMBConfPath, original, 0o644); rerr != nil {
		return config.Refuse(config.CodeConfigError,
			"testparm rejects the [global] settings %s declares (%s) and %q could not be put back (%s); fix or remove the offending entry in %s — the configuration volume still holds the rejected settings",
			sources, problem, e.SMBConfPath, oneLine(rerr.Error()), sources)
	}
	return config.Refuse(config.CodeConfigError,
		"testparm rejects the [global] settings %s declares (%s); %s has been put back to what it held before this start, so fix or remove the offending entry in %s and start the container again",
		sources, problem, e.SMBConfPath, sources)
}

// testparmDiagnostics returns everything testparm printed BEFORE the dump of
// the configuration, one line each, whitespace collapsed.
//
// The cut at the dump is what makes this safe to read: with `-s` everything
// from `# Global parameters` on is the configuration being echoed back, so a
// parameter whose VALUE happens to read like an error message cannot be
// mistaken for one.
func testparmDiagnostics(out string) []string {
	var diagnostics []string
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "#") || strings.HasPrefix(t, "[") {
			break
		}
		diagnostics = append(diagnostics, oneLine(t))
	}
	return diagnostics
}

// testparmRejects reports whether one diagnostic line means the configuration
// is unusable EVEN THOUGH testparm exited 0. There is exactly one such shape
// (see checkSMBConf for the measurements): a parameter samba does not know,
// which it announces and then ignores — leaving the operator with a setting
// that silently does nothing, the failure this whole variable exists to
// prevent.
func testparmRejects(line string) bool {
	return strings.Contains(line, "Unknown parameter encountered") ||
		strings.Contains(line, "Ignoring unknown parameter")
}

// testparmCause picks the line to quote when testparm exited non-zero.
//
// Deprecation warnings are skipped rather than quoted: they are printed
// first, before the line that actually failed the run (measured on a file
// carrying both), so quoting the first diagnostic would name the wrong
// parameter in the refusal an operator has to act on.
func testparmCause(diagnostics []string, err error) string {
	for _, d := range diagnostics {
		if !strings.Contains(d, "is deprecated") {
			return d
		}
	}
	if err != nil {
		return oneLine(err.Error())
	}
	if len(diagnostics) > 0 {
		return diagnostics[0]
	}
	return "no diagnostic"
}

// chronyConfig generates the configuration chronyd will actually read and
// returns its path.
//
// The file baked into the image is a template, and this is why. chrony has
// to be pointed at the directory samba creates the MS-SNTP signing socket
// in, and that directory is not the image's to decide: `ntp signd socket
// directory` is an ordinary smb.conf parameter, and /etc/samba is a volume
// an operator owns. Nothing in samba or in chrony reconciles the two files,
// so the day someone sets that parameter a chrony.conf carrying the
// compile-time default names a socket samba never creates — and says
// nothing about it, because chrony opens that socket lazily, only when a
// request carrying an authenticator arrives. A time service whose signing
// fails in silence is the one failure mode this must not have.
//
// So the value is not assumed, it is ASKED: `testparm` is samba's own
// parser reading samba's own configuration, which makes the generated file
// agree with the running DC by construction rather than by coincidence.
//
// To be clear about what this does NOT fix, because the restore path was
// suspected and measured: `samba-tool domain backup restore` relocates
// `state directory`, `cache directory`, `lock directory` and sysvol under
// /var/lib/samba/state, but it leaves this parameter unset, and samba's
// compile-time default for it does not track `state directory`. A restored
// DC keeps the signing socket exactly where a provisioned one does.
//
// Everything else in the template is copied verbatim, the absolute
// driftfile and pidfile paths included: they are unaffected by where the
// generated file sits.
func (e *Executor) chronyConfig(ctx context.Context) (string, *config.Refusal) {
	tmplPath, outPath := e.chronyTemplate(), e.chronyConfPath()

	data, err := os.ReadFile(tmplPath)
	if err != nil {
		return "", config.Refuse(config.CodeRuntimeFailure,
			"the chrony configuration template %q cannot be read (%s); it is part of the image, so this is an image or mount fault — set SAMBA_CHRONY=off to run without the MS-SNTP time service",
			tmplPath, oneLine(err.Error()))
	}

	conf := string(data)
	if dir := e.ntpSigndDir(ctx); dir != "" {
		updated, changed := withNTPSigndSocket(conf, dir)
		if changed {
			e.logf("chrony will use the signing socket directory this DC's smb.conf declares: %s %s", ntpSigndDirective, dir)
		}
		conf = updated
	}

	if err := writeFileAtomic(outPath, []byte(conf), 0o644); err != nil {
		return "", config.Refuse(config.CodeRuntimeFailure,
			"the chrony configuration %q cannot be written (%s); /run must be writable (mount it as tmpfs when the root filesystem is read-only), or set SAMBA_CHRONY=off",
			outPath, oneLine(err.Error()))
	}
	return outPath, nil
}

// ntpSigndDir asks samba where it puts the MS-SNTP signing socket, and
// returns "" when the answer cannot be trusted.
//
// "" is not a failure of the boot: it means the generated configuration
// keeps the template's own value, which is samba's compile-time default and
// therefore right on every DC that has not been told otherwise — the
// overwhelming majority, and the best guess available when there is nothing
// better. Losing the time service over a testparm that did not answer would
// be a far worse trade than serving unsigned time — and the fallback is
// logged, so an operator debugging signed NTP is not left guessing which
// value is in force.
func (e *Executor) ntpSigndDir(ctx context.Context) string {
	out, err := e.Runner.Output(ctx, e.Bin.Testparm, testparmArgs(e.SMBConfPath, ntpSigndDirKey)...)
	if err != nil {
		e.logf("%q could not be read from %s (%s); chrony keeps the signing socket directory baked into the image",
			ntpSigndDirKey, e.SMBConfPath, oneLine(err.Error()))
		return ""
	}
	// testparm prints the value on stdout and its banner on stderr, but a
	// future release printing one extra line must not turn into a chrony
	// configuration pointing at a banner: only a lone absolute path is
	// taken, anything else falls back.
	dir := strings.TrimSpace(out)
	if dir == "" || strings.ContainsAny(dir, "\n\r") || !strings.HasPrefix(dir, "/") {
		e.logf("%s reported no usable %q (%q); chrony keeps the signing socket directory baked into the image",
			e.Bin.Testparm, ntpSigndDirKey, oneLine(dir))
		return ""
	}
	return dir
}

// chronyTemplate is the baked-in template path, defaulted.
func (e *Executor) chronyTemplate() string {
	if e.ChronyTemplate == "" {
		return defaultChronyTemplate
	}
	return e.ChronyTemplate
}

// chronyConfPath is the generated configuration's path, defaulted.
func (e *Executor) chronyConfPath() string {
	if e.ChronyConf == "" {
		return defaultChronyConf
	}
	return e.ChronyConf
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
		conf, ref := e.chronyConfig(ctx)
		if ref != nil {
			return ref
		}
		p, err := e.Runner.Start(daemonCtx, e.Bin.Chronyd, chronyArgs(conf)...)
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
// budget first, and chronyd gets the rest — which may be none of it, in
// which case chronyd is killed at once rather than the shutdown overrunning.
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

// shutdownBudget derives the two shared deadlines of one shutdown. The kill
// deadline is the hard total — the whole grace window, for both daemons and
// both phases together — and the term deadline sits earlier by the reap
// window, so escalating to SIGKILL still leaves time to collect the corpse
// inside the same total. Nothing here may outlive e.grace().
//
// Both deliberately survive a cancelled parent context: when cancellation is
// what triggered the shutdown, an already-expired context would turn the
// orderly stop into an immediate kill.
func (e *Executor) shutdownBudget(ctx context.Context) (budget, budget) {
	base := context.WithoutCancel(ctx)
	termCtx, termCancel := context.WithTimeout(base, e.termWindow())
	killCtx, killCancel := context.WithTimeout(base, e.grace())
	return budget{termCtx, termCancel}, budget{killCtx, killCancel}
}

// termWindow is how long the polite SIGTERM phase may last: the total budget
// minus the reap window held back for the escalation. A configured grace no
// larger than the kill window splits the total in half instead, so the
// SIGTERM phase always ends strictly before the total does.
func (e *Executor) termWindow() time.Duration {
	if w := e.grace() - e.killGrace(); w > 0 {
		return w
	}
	return e.grace() / 2
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
// shared budgets. A daemon that ignores SIGTERM is killed once the SIGTERM
// phase is spent; one that cannot even be reaped is left to the init process
// (tini) rather than blocking the shutdown of the other.
//
// The window this daemon actually gets is whatever the previous one left, so
// the messages report it: "chronyd was killed immediately" is a fact about
// samba having eaten the budget, not about chronyd misbehaving.
func (e *Executor) stopProc(termCtx, killCtx context.Context, name string, p Proc, done chan error) {
	if p == nil {
		return
	}
	window := until(termCtx)
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

	e.logf("%s did not stop in the %s it had of the shared %s shutdown budget: killing it",
		name, round(window), e.grace())
	_ = p.Signal(os.Kill)
	select {
	case <-done:
		e.logf("%s killed", name)
	case <-killCtx.Done():
		e.logf("%s could not be reaped before the shared %s shutdown budget ran out; leaving it to the init process",
			name, e.grace())
	}
}

// until is how much of a budget is left, never negative.
func until(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	if left := time.Until(deadline); left > 0 {
		return left
	}
	return 0
}

// round renders a window for an operator, without spurious precision.
func round(d time.Duration) time.Duration { return d.Round(time.Millisecond) }

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
		args = append(args, "--option="+dnsForwarderKey+"="+cfg.DNSForwarder)
	}
	args = append(args, "--option="+dnsUpdateCommand)
	// The operator's own TLS material and declarative block, in that order
	// and last, so that they are applied over the image's own options rather
	// than under them. Nothing here can collide with those: config.Load
	// refuses every key this image owns before the value ever reaches
	// provision. The TLS files are passed HERE, and not only reconciled
	// afterwards, because the smb.conf provision generates is what the DC's
	// very first start reads: a DC that served samba's self-signed
	// certificate for one boot and the operator's from the next would change
	// identity under a client that had already seen it.
	for _, o := range cfg.EffectiveGlobalOptions() {
		args = append(args, "--option="+o.Key+" = "+o.Value)
	}
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
//
// conf is the GENERATED configuration (chronyConfig), never the baked-in
// template: the template's signing-socket directory is only a default.
func chronyArgs(conf string) []string {
	return []string{"-d", "-x", "-f", conf}
}

// testparmArgs builds the command line that reads one smb.conf parameter
// back through samba's own parser.
//
//   - `-s` suppresses the "Press enter" prompt, which is what makes a bare
//     testparm hang in a container.
//   - `-l` skips the GLOBAL LOGIC CHECKS. This is DEFENSIVE, not a fix for
//     a failure anybody here has seen: no boot has been observed where
//     those checks cost this command its answer. The reason to pass it
//     anyway is that the checks verify that the directories smb.conf names
//     already exist, and this command runs before the daemons do — so a
//     directory samba creates at startup is a plausible way for testparm to
//     print the requested value on stdout and still exit non-zero, which
//     Output reports as an error and ntpSigndDir then treats as no answer.
//     Reading one parameter is not validating a configuration: those checks
//     are somebody else's job, and whether they pass must not decide
//     whether this one gets its value.
//   - the configuration file is named explicitly rather than left to the
//     compiled-in default, so the entrypoint and samba can never read two
//     different files.
func testparmArgs(smbConf, parameter string) []string {
	return []string{"-s", "-l", "--parameter-name=" + parameter, smbConf}
}

// testparmCheckArgs builds the command line that asks samba to PARSE the
// whole file instead of reading one parameter out of it: same `-s -l` and the
// same explicit file, no `--parameter-name`.
//
// A parse verdict is all it is, and deliberately so: `-l` skips the global
// logic checks — which verify that the directories smb.conf names already
// exist, and this runs before the daemons create them — so a green answer
// means "samba can load this file", not "this configuration is correct".
// Validating the deployment is somebody else's job; what this gate owes the
// operator is that the file their variable produced is one samba can read.
//
// `--debug-stdout` is added for this call only, and it is load-bearing rather
// than cosmetic: it is what puts the diagnostics where checkSMBConf can read
// them. The parameter-reading call above must NOT have it — there the same
// redirection would mix debug lines into the value being read.
func testparmCheckArgs(smbConf string) []string {
	return []string{"-s", "-l", "--debug-stdout", smbConf}
}

// withNTPSigndSocket returns conf with the ntpsigndsocket directive naming
// dir, and says whether that changed anything. It is pure and idempotent:
// generating the same configuration twice produces the same bytes.
//
// Only the directive line is touched. A directive that is missing entirely
// is appended rather than assumed to be commented out somewhere — a chrony
// configuration without it cannot sign anything, so adding it is the only
// answer that leaves signed NTP working.
func withNTPSigndSocket(conf, dir string) (string, bool) {
	entry := ntpSigndDirective + " " + dir
	lines := strings.Split(conf, "\n")
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != ntpSigndDirective {
			continue
		}
		if line == entry {
			return conf, false
		}
		out := make([]string, len(lines))
		copy(out, lines)
		out[i] = entry
		return strings.Join(out, "\n"), true
	}

	out := strings.TrimRight(conf, "\n")
	if out != "" {
		out += "\n"
	}
	return out + entry + "\n", true
}

// confEdit says what withGlobalSetting did to a file.
type confEdit int

const (
	confUnchanged confEdit = iota
	confAdded
	confReplaced
)

// withGlobalSetting returns conf with key set to value in the [global]
// section, and says what it changed. It is pure and idempotent: a join that
// repeats must not append the same option twice.
//
// Detection is scoped to [global] and matches the value, not only the key:
// the same key in another section does not configure the DC, and a [global]
// entry carrying a different value — the samba default DNS update command,
// which shells out to the nsupdate this image does not ship, or a functional
// level below the domain's, which stops samba from starting at all — is worse
// than no entry, so it is replaced rather than trusted.
func withGlobalSetting(conf, key, value string) (string, confEdit) {
	lines := strings.Split(conf, "\n")
	entry := "\t" + key + " = " + value

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
		if !ok || !strings.EqualFold(normalize(k), key) {
			continue
		}
		if normalize(v) == value {
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
