// Package config loads and validates the entrypoint's environment
// configuration. It is the single place that knows the SAMBA_* variable
// contract, and it is the home of Refusal, the typed error every other
// package returns when it refuses to act.
//
// Secrets are never read from the environment: only *_FILE variables are
// accepted, and their contents are read on demand through ReadSecret.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// Exit codes (contract, immutable once released — SPEC §6.5).
const (
	// CodeConfigError is returned for any configuration problem.
	CodeConfigError = 10
	// CodeSecretError is returned when a secret file is missing or unreadable.
	CodeSecretError = 11
	// CodeStateExists is returned when provision or join is asked to run
	// over an already initialized volume.
	CodeStateExists = 20
	// CodeStateAbsent is returned when run or maintenance finds no state.
	CodeStateAbsent = 21
	// CodeDowngrade is returned when the volume was written by a newer
	// Samba than the image provides.
	CodeDowngrade = 22
	// CodeDBCheckFailed is returned when the database consistency check fails.
	CodeDBCheckFailed = 23
	// CodeRuntimeFailure is returned for samba/runtime failures.
	CodeRuntimeFailure = 30
)

// Refusal is the typed error carried out of every package that declines to
// act. Code becomes the process exit code; Msg is a single actionable line
// (cause + remedy) printed to stderr. It must never contain a secret value.
type Refusal struct {
	Code int
	Msg  string
}

// Error implements the error interface.
func (r *Refusal) Error() string { return r.Msg }

// Refuse builds a *Refusal from a format string. It is the single
// constructor every package uses, so refusals are always built the same way
// and callers never assemble a Refusal literal by hand.
func Refuse(code int, format string, args ...any) *Refusal {
	return &Refusal{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Mode is the operating mode selected by SAMBA_MODE.
type Mode string

// The supported modes (B.4).
const (
	ModeAuto        Mode = "auto"
	ModeProvision   Mode = "provision"
	ModeJoin        Mode = "join"
	ModeRun         Mode = "run"
	ModeMaintenance Mode = "maintenance"
)

// validModes lists the modes in the order shown to the operator.
var validModes = []Mode{ModeAuto, ModeProvision, ModeJoin, ModeRun, ModeMaintenance}

// Maintenance operations (SAMBA_MAINTENANCE_OP).
const (
	MaintenanceCheck  = "check"
	MaintenanceRepair = "repair"
)

// DNSBackendInternal is the only DNS backend supported in v1.
const DNSBackendInternal = "SAMBA_INTERNAL"

// GlobalOption is one `key = value` entry an operator declared through
// SAMBA_GLOBAL_OPTIONS. Key is normalized (lower case, single spaces) so that
// spacing and case can never decide whether a setting is recognized, matched
// against the owned list, or found again in an existing smb.conf.
type GlobalOption struct {
	Key   string
	Value string
}

// Config is the validated configuration of one entrypoint run.
//
// Every scalar field is a plain value, so most of it can still be compared
// field by field; the two SAMBA_GLOBAL_OPTIONS fields are lists, because a
// declarative block is inherently a sequence and flattening it back into one
// string here would only move the parsing to a place with no way to refuse.
type Config struct {
	Mode   Mode
	Realm  string
	Domain string

	AdminPasswordFile string
	JoinPasswordFile  string
	JoinUsername      string

	DNSForwarder  string
	DNSBackend    string
	FunctionLevel string

	LogLevel      int
	Chrony        bool
	MaintenanceOp string

	// TLSCertFile, TLSKeyFile and TLSCAFile are the paths, inside the
	// container, of the LDAPS material the operator supplied through the
	// SAMBA_TLS_* variables. All three are set or none of them is (Load
	// refuses a partial trio), so TLSCertFile alone answers "did the
	// operator bring their own certificate?".
	TLSCertFile string
	TLSKeyFile  string
	TLSCAFile   string

	// GlobalOptions are the [global] settings declared through
	// SAMBA_GLOBAL_OPTIONS, in declaration order.
	GlobalOptions []GlobalOption
	// GlobalOptionsShadowed lists the keys that appeared more than once,
	// whose earlier occurrences were dropped. It exists so the boot can say
	// so out loud: silently keeping one of two contradictory lines is
	// exactly the kind of thing an operator must not have to discover by
	// reading the resulting smb.conf.
	GlobalOptionsShadowed []string
}

// Load reads and validates the configuration from the environment. getenv is
// injected so tests never touch the real process environment. Every failure
// is a *Refusal with CodeConfigError.
func Load(getenv func(string) string) (*Config, error) {
	get := func(key string) string { return strings.TrimSpace(getenv(key)) }

	// Secrets are file-only (§6.1): refuse before anything else so the
	// operator gets the security-relevant message first.
	for _, plain := range []struct{ env, file string }{
		{"SAMBA_ADMIN_PASSWORD", "SAMBA_ADMIN_PASSWORD_FILE"},
		{"SAMBA_JOIN_PASSWORD", "SAMBA_JOIN_PASSWORD_FILE"},
	} {
		if get(plain.env) != "" {
			return nil, Refuse(CodeConfigError,
				"%s is set in the environment and passwords are never accepted that way; unset %s and pass the password via %s pointing at a mounted secret file",
				plain.env, plain.env, plain.file)
		}
	}

	cfg := &Config{
		Mode:              ModeAuto,
		Realm:             get("SAMBA_REALM"),
		Domain:            get("SAMBA_DOMAIN"),
		AdminPasswordFile: get("SAMBA_ADMIN_PASSWORD_FILE"),
		JoinPasswordFile:  get("SAMBA_JOIN_PASSWORD_FILE"),
		JoinUsername:      "Administrator",
		DNSForwarder:      get("SAMBA_DNS_FORWARDER"),
		DNSBackend:        DNSBackendInternal,
		FunctionLevel:     "2016",
		LogLevel:          1,
		Chrony:            true,
		MaintenanceOp:     MaintenanceCheck,
	}

	if v := get("SAMBA_MODE"); v != "" {
		mode := Mode(strings.ToLower(v))
		if !isValidMode(mode) {
			return nil, Refuse(CodeConfigError,
				"SAMBA_MODE=%q is not a supported mode; set SAMBA_MODE to one of %s",
				v, modeList())
		}
		cfg.Mode = mode
	}

	if v := get("SAMBA_JOIN_USERNAME"); v != "" {
		cfg.JoinUsername = v
	}
	if v := get("SAMBA_FUNCTION_LEVEL"); v != "" {
		cfg.FunctionLevel = v
	}

	if v := get("SAMBA_DNS_BACKEND"); v != "" {
		if !strings.EqualFold(v, DNSBackendInternal) {
			return nil, Refuse(CodeConfigError,
				"SAMBA_DNS_BACKEND=%q is not supported by this image; set SAMBA_DNS_BACKEND=%s or leave it unset",
				v, DNSBackendInternal)
		}
		cfg.DNSBackend = DNSBackendInternal
	}

	if v := get("SAMBA_LOG_LEVEL"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, Refuse(CodeConfigError,
				"SAMBA_LOG_LEVEL=%q is not an integer; set SAMBA_LOG_LEVEL to a samba debug level such as 0, 1 or 3",
				v)
		}
		if n < 0 {
			return nil, Refuse(CodeConfigError,
				"SAMBA_LOG_LEVEL=%q is negative; set SAMBA_LOG_LEVEL to a samba debug level such as 0, 1 or 3",
				v)
		}
		cfg.LogLevel = n
	}

	if v := get("SAMBA_CHRONY"); v != "" {
		switch strings.ToLower(v) {
		case "on":
			cfg.Chrony = true
		case "off":
			cfg.Chrony = false
		default:
			return nil, Refuse(CodeConfigError,
				"SAMBA_CHRONY=%q is not a valid toggle; set SAMBA_CHRONY to on or off",
				v)
		}
	}

	if v := get("SAMBA_MAINTENANCE_OP"); v != "" {
		op := strings.ToLower(v)
		if op != MaintenanceCheck && op != MaintenanceRepair {
			return nil, Refuse(CodeConfigError,
				"SAMBA_MAINTENANCE_OP=%q is not a valid operation; set SAMBA_MAINTENANCE_OP to check or repair",
				v)
		}
		cfg.MaintenanceOp = op
	}

	opts, shadowed, err := parseGlobalOptions(get(EnvGlobalOptions))
	if err != nil {
		return nil, err
	}
	cfg.GlobalOptions, cfg.GlobalOptionsShadowed = opts, shadowed

	cfg.TLSCertFile = get(EnvTLSCertFile)
	cfg.TLSKeyFile = get(EnvTLSKeyFile)
	cfg.TLSCAFile = get(EnvTLSCAFile)
	if err := checkTLSTrio(cfg); err != nil {
		return nil, err
	}

	// Realm and secrets are required only by the modes that initialize
	// state. auto decides between provision and join once the volume has
	// been observed, so it is validated in the modes engine, not here.
	if cfg.Mode == ModeProvision || cfg.Mode == ModeJoin {
		if cfg.Realm == "" {
			return nil, Refuse(CodeConfigError,
				"SAMBA_REALM is required in %s mode but is not set; set SAMBA_REALM to the Kerberos realm, for example AD.EXAMPLE.COM",
				cfg.Mode)
		}
		if !strings.Contains(cfg.Realm, ".") {
			return nil, Refuse(CodeConfigError,
				"SAMBA_REALM=%q is not a dotted DNS domain; set SAMBA_REALM to a fully qualified realm such as AD.EXAMPLE.COM",
				cfg.Realm)
		}
	}
	if cfg.Mode == ModeProvision && cfg.AdminPasswordFile == "" {
		return nil, Refuse(CodeConfigError,
			"SAMBA_ADMIN_PASSWORD_FILE is required in provision mode but is not set; mount the initial Administrator password as a file and point SAMBA_ADMIN_PASSWORD_FILE at it")
	}
	if cfg.Mode == ModeJoin && cfg.JoinPasswordFile == "" {
		return nil, Refuse(CodeConfigError,
			"SAMBA_JOIN_PASSWORD_FILE is required in join mode but is not set; mount the join account password as a file and point SAMBA_JOIN_PASSWORD_FILE at it")
	}

	// Samba upper-cases realms and NetBIOS names; normalize once here so
	// every downstream package sees the canonical form.
	cfg.Realm = strings.ToUpper(cfg.Realm)
	if cfg.Domain == "" {
		cfg.Domain = firstLabel(cfg.Realm)
	}
	cfg.Domain = strings.ToUpper(cfg.Domain)

	return cfg, nil
}

// The variables that carry the operator's own LDAPS material, and the
// [global] parameter each of them sets. They are named once here and used
// both by the owned-key table (which refuses those parameters in
// SAMBA_GLOBAL_OPTIONS) and by the loader below, so the two can never drift
// apart and name different things to the same operator.
const (
	EnvGlobalOptions = "SAMBA_GLOBAL_OPTIONS"

	EnvTLSCertFile = "SAMBA_TLS_CERT_FILE"
	EnvTLSKeyFile  = "SAMBA_TLS_KEY_FILE"
	EnvTLSCAFile   = "SAMBA_TLS_CA_FILE"
	// EnvTLSGroup names the three at once, for the one message that is
	// about their ABSENCE and can therefore name none of them individually.
	EnvTLSGroup = "SAMBA_TLS_*_FILE"

	tlsCertFileKey = "tls certfile"
	tlsKeyFileKey  = "tls keyfile"
	tlsCAFileKey   = "tls cafile"
)

// The two owners that are not an environment variable. They are sentinels,
// not prose to be printed as-is: globalOptionRefusal turns each into its own
// sentence, because "set the image instead" would be nonsense.
const (
	ownerImage    = "\x00image"
	ownerHostname = "\x00hostname"
)

// globalOptionOwners lists the [global] parameters SAMBA_GLOBAL_OPTIONS may
// NOT set, each mapped to what owns it.
//
// Every one of them is already derived from somewhere else, and an entry
// quietly overriding it would either contradict what the operator asked for
// through the variable that owns it, or break the DC outright:
//
//   - realm, workgroup and netbios name ARE the domain controller's identity.
//     The directory on the state volume was created with them; changing them
//     in smb.conf does not rename a DC, it makes the running server disagree
//     with its own database.
//   - ad dc functional level below the domain's stops samba from starting at
//     all (see run.joinedConfSettings), which is why SAMBA_FUNCTION_LEVEL is
//     mirrored onto it rather than left to chance.
//   - dns forwarder decides whether this DC's DNS answers or stalls for
//     seconds; SAMBA_DNS_FORWARDER exists precisely to set it.
//   - dns update command must name samba_dnsupdate --use-samba-tool, because
//     the image deliberately ships no nsupdate (B.6).
//   - server role is what makes this an AD DC rather than a file server.
//   - ntp signd socket directory is read back out of smb.conf to generate
//     chrony's configuration; the entrypoint follows it, so an operator
//     setting it here would be configuring two files through one.
//   - include would let an arbitrary file contradict every line above, from
//     a path this image cannot validate.
//   - the tls * files belong to the SAMBA_TLS_* variables that own them,
//     which check that the material is there and readable before naming it
//     in smb.conf. Setting one of the three here would also let an operator
//     set one WITHOUT the other two, which is the half-configured DC the
//     all-or-none rule below exists to prevent.
var globalOptionOwners = map[string]string{
	"realm":                      "SAMBA_REALM",
	"workgroup":                  "SAMBA_DOMAIN",
	"netbios name":               ownerHostname,
	"ad dc functional level":     "SAMBA_FUNCTION_LEVEL",
	"dns forwarder":              "SAMBA_DNS_FORWARDER",
	tlsCertFileKey:               EnvTLSCertFile,
	tlsKeyFileKey:                EnvTLSKeyFile,
	tlsCAFileKey:                 EnvTLSCAFile,
	"server role":                ownerImage,
	"dns update command":         ownerImage,
	"ntp signd socket directory": ownerImage,
	"include":                    ownerImage,
}

// canonicalGlobalOptionOwners is globalOptionOwners keyed the way samba
// compares parameter names, so that a lookup cannot be evaded by spelling.
// It is derived from the table above rather than written out a second time:
// two hand-maintained lists of owned keys would eventually disagree, and the
// one that disagreed would be the one that lets a key through.
var canonicalGlobalOptionOwners = func() map[string]string {
	owners := make(map[string]string, len(globalOptionOwners))
	for key, owner := range globalOptionOwners {
		owners[CanonicalKey(key)] = owner
	}
	return owners
}()

// ownerOfGlobalOption reports what owns key, if anything, comparing it the
// way samba's own parser does.
func ownerOfGlobalOption(key string) (string, bool) {
	owner, owned := canonicalGlobalOptionOwners[CanonicalKey(key)]
	return owner, owned
}

// globalOptionKeyPattern is the charset one declared parameter name may use,
// applied to the lower-cased, space-collapsed form.
//
// It exists for one specific escape, and it is a CRITICAL boundary rather
// than tidiness. The settings are written into smb.conf as `\tkey = value`
// lines under [global], and samba's ini parser treats any line whose first
// non-blank character is `[` as the start of a new SECTION, discarding
// whatever follows the closing `]`. So a key of `[myshare] path` does not
// declare a parameter at all: it ends [global] and opens a share.
//
// Measured in this image (samba 4.24.7), with a file holding `[global]`,
// `workgroup = EXAMPLE` and a tab-indented `[myshare] path = /tmp`,
// `testparm -s -l --debug-stdout` printed
//
//	WARNING: No path in service myshare - making it unavailable!
//	NOTE: Service myshare is flagged unavailable.
//
// and exited 0, with a `[myshare]` section in its dump and the `path = /tmp`
// gone. Exit 0 and no unknown-parameter line is exactly what checkSMBConf
// treats as ACCEPTED — so the boot would have continued, and the share would
// have persisted on the configuration volume. The only place to stop it is
// here, before anything is written.
//
// The charset is what real smb.conf parameter names use: letters, digits,
// spaces, and the punctuation of `idmap config * : backend`, which is a
// genuine parameter name and must keep working. `#` and `;` are excluded
// with the brackets — they start a comment when they open a line, and
// nothing legitimate needs them in a name.
//
// VALUES are deliberately not restricted this way. A value is written after
// the key, so it cannot be the first non-blank character of its line and
// cannot open a section; measured in the same image, `log file =
// /var/log/[x]/l#z;q` round-tripped through `testparm -s` unchanged.
var globalOptionKeyPattern = regexp.MustCompile(`^[a-z0-9 :*._-]+$`)

// CanonicalKey renders one smb.conf parameter name the way samba COMPARES
// them: case-folded with every space removed.
//
// Samba matches parameter names with strwicmp, which ignores whitespace as
// well as case, and this was measured rather than taken on faith. In this
// image (samba 4.24.7), a `[global]` holding `maxlogsize = 4000`, `MAX  LOG
// size = 4000`, `TLSCertFile = /x/cert.pem` and `ServerRole = standalone
// server` was echoed back by `testparm -s -l --debug-stdout` as `max log size
// = 4000`, `tls certfile = /x/cert.pem` and `server role = standalone
// server`, exit 0.
//
// Everything that decides whether two names are THE SAME setting therefore
// has to compare this form: the owned-key table above (or `tlscertfile` would
// bypass SAMBA_TLS_CERT_FILE and the all-or-none TLS rule), the
// last-occurrence-wins rule below, and run.withGlobalSetting /
// run.withoutGlobalSetting when they recognise a line already in smb.conf.
// What is WRITTEN and what is printed to the operator stays the spelling they
// used: samba understands it, and a message quoting something they never
// typed is a message they cannot search their configuration for.
func CanonicalKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), ""))
}

// parseGlobalOptions turns the SAMBA_GLOBAL_OPTIONS block into the settings
// the entrypoint will reconcile into smb.conf, and refuses everything it
// cannot make sense of.
//
// The value arrives as a compose `|` block scalar, so it really does carry
// blank lines, comments and whatever indentation the author left behind:
// those are skipped rather than treated as settings. Anything else that is
// not a `key = value` pair is refused rather than ignored — a typo that
// silently does nothing is the failure mode this whole variable exists to
// avoid, since an option that does not reach smb.conf is invisible until the
// day it was supposed to matter.
//
// A key repeated in the block keeps its LAST value and moves to that last
// position. Refusing the repetition would be defensible too, but a generated
// or concatenated block legitimately ends up with one, and "the last line
// wins" is what every configuration file this resembles already does. The
// shadowed keys are returned so the boot can announce them.
func parseGlobalOptions(value string) ([]GlobalOption, []string, error) {
	var opts []GlobalOption
	var shadowed []string

	for _, line := range strings.Split(value, "\n") {
		entry := strings.TrimSpace(line)
		if entry == "" || strings.HasPrefix(entry, "#") || strings.HasPrefix(entry, ";") {
			continue
		}
		rawKey, rawValue, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, nil, Refuse(CodeConfigError,
				"SAMBA_GLOBAL_OPTIONS contains the line %q, which is not a `key = value` smb.conf setting; write one setting per line, for example `max log size = 10000`, or start the line with # to comment it out",
				entry)
		}
		key := strings.ToLower(normalizeSpace(rawKey))
		if key == "" {
			return nil, nil, Refuse(CodeConfigError,
				"SAMBA_GLOBAL_OPTIONS contains the line %q, which sets no parameter name; write one `key = value` smb.conf setting per line, for example `max log size = 10000`",
				entry)
		}
		if !globalOptionKeyPattern.MatchString(key) {
			return nil, nil, Refuse(CodeConfigError,
				"SAMBA_GLOBAL_OPTIONS contains the line %q, whose parameter name %q is not one samba could read as a [global] setting; an smb.conf parameter name is made of letters, digits, spaces and the characters : * . _ - (as in `idmap config * : backend`), and a name carrying a bracket would open a new section instead of setting a parameter; write one `key = value` setting per line, for example `max log size = 10000`",
				entry, key)
		}
		if owner, owned := ownerOfGlobalOption(key); owned {
			return nil, nil, globalOptionRefusal(key, owner)
		}
		value := normalizeSpace(rawValue)
		// Defensive: the block was split on "\n" and normalizeSpace drops
		// every other whitespace character, so no value can carry a line
		// break by the time it gets here. A second line in a value would be a
		// second smb.conf directive nobody declared, which is the same escape
		// the key charset above exists to close, so it is asserted rather
		// than assumed.
		if strings.ContainsAny(value, "\n\r") {
			return nil, nil, Refuse(CodeConfigError,
				"SAMBA_GLOBAL_OPTIONS contains the line %q, whose value carries a line break; write one `key = value` setting per line",
				entry)
		}

		opt := GlobalOption{Key: key, Value: value}
		canonical := CanonicalKey(key)
		if i := indexGlobalOption(opts, canonical); i >= 0 {
			opts = append(opts[:i], opts[i+1:]...)
			if !containsKey(shadowed, canonical) {
				shadowed = append(shadowed, key)
			}
		}
		opts = append(opts, opt)
	}
	return opts, shadowed, nil
}

// globalOptionRefusal says no to one owned key, naming what owns it and what
// the operator should set instead. Naming the owner is the whole point: an
// operator who put `realm` in SAMBA_GLOBAL_OPTIONS needs to be sent to
// SAMBA_REALM, not merely told that they may not have this one.
func globalOptionRefusal(key, owner string) error {
	switch owner {
	case ownerImage:
		return Refuse(CodeConfigError,
			"SAMBA_GLOBAL_OPTIONS sets %q, which this image manages itself and must keep consistent with the rest of the container; remove that line from SAMBA_GLOBAL_OPTIONS",
			key)
	case ownerHostname:
		return Refuse(CodeConfigError,
			"SAMBA_GLOBAL_OPTIONS sets %q, which this image takes from the container hostname and which names the domain controller its own directory knows; remove that line and set the container hostname instead",
			key)
	default:
		return Refuse(CodeConfigError,
			"SAMBA_GLOBAL_OPTIONS sets %q, which is owned by %s; remove that line and set %s instead",
			key, owner, owner)
	}
}

// checkTLSTrio enforces the all-or-none rule on the operator's own LDAPS
// material.
//
// Refusing is cheaper than finding out what a partial trio does. `tls
// enabled` is already yes on an AD DC (measured with testparm against Samba
// 4.24.7 in this image), so LDAPS is served either way; what changes is that
// the settings left unset keep samba's own defaults — `tls/cert.pem`,
// `tls/key.pem`, `tls/ca.pem` under the private directory on the state
// volume, also measured — and the DC would then assemble its chain from two
// different places, or regenerate the missing half itself. Which of those
// happens has NOT been measured here, and that is precisely the problem: the
// operator cannot tell either, from a container that came up healthy.
//
// The message names the variables that are MISSING, because those are what
// has to be set.
func checkTLSTrio(cfg *Config) error {
	var set, missing []string
	for _, v := range []struct{ env, path string }{
		{EnvTLSCertFile, cfg.TLSCertFile},
		{EnvTLSKeyFile, cfg.TLSKeyFile},
		{EnvTLSCAFile, cfg.TLSCAFile},
	} {
		if v.path == "" {
			missing = append(missing, v.env)
		} else {
			set = append(set, v.env)
		}
	}
	if len(set) > 0 && len(missing) > 0 {
		return Refuse(CodeConfigError,
			"%s %s set but %s %s not; serving LDAPS with your own material takes the certificate, its private key and the CA that issued it, so set all three or none — with none of them set this domain controller serves the self-signed certificate samba generates for itself",
			joinAnd(set), isAre(set), joinAnd(missing), isAre(missing))
	}
	// A relative path would be read by two different programs from two
	// different directories. This process resolves it against its own working
	// directory; samba resolves `tls certfile` against the PRIVATE directory
	// on the state volume — that is what its defaults `tls/cert.pem`,
	// `tls/key.pem` and `tls/ca.pem` are relative to (measured). So the check
	// below could pass on a file samba never opens, which is the one outcome
	// checking at all is meant to rule out.
	for _, v := range []struct{ env, path string }{
		{EnvTLSCertFile, cfg.TLSCertFile},
		{EnvTLSKeyFile, cfg.TLSKeyFile},
		{EnvTLSCAFile, cfg.TLSCAFile},
	} {
		if v.path != "" && !filepath.IsAbs(v.path) {
			return Refuse(CodeConfigError,
				"%s is %q, which is not an absolute path; samba resolves a relative TLS path against the private directory on the state volume while this entrypoint would resolve it against its own working directory, so set %s to the absolute path the file has inside the container, for example /run/secrets/tls/cert.pem",
				v.env, v.path, v.env)
		}
	}
	return nil
}

// TLSOptionKeys are the three [global] parameters the SAMBA_TLS_* variables
// own. They are needed even when the variables are UNSET: those keys belong
// to the image, so unsetting the variables has to take them back OUT of
// smb.conf rather than leave a DC pointing at a mount that is gone.
func TLSOptionKeys() []string {
	return []string{tlsCertFileKey, tlsKeyFileKey, tlsCAFileKey}
}

// TLSOptions renders the operator's own LDAPS material as the three [global]
// settings that point samba at it, or nil when none was supplied.
//
// `tls enabled` is deliberately NOT among them: it already defaults to yes on
// an AD DC, so setting it would add a line that changes nothing and invite the
// reading that LDAPS is off without it.
func (c *Config) TLSOptions() []GlobalOption {
	if c.TLSCertFile == "" {
		return nil
	}
	return []GlobalOption{
		{Key: tlsCertFileKey, Value: c.TLSCertFile},
		{Key: tlsKeyFileKey, Value: c.TLSKeyFile},
		{Key: tlsCAFileKey, Value: c.TLSCAFile},
	}
}

// EffectiveGlobalOptions is everything the entrypoint reconciles into
// [global]: the TLS material first, then the declarative block, in
// declaration order.
//
// The material goes first so that an operator reading their smb.conf finds
// the certificate their DC serves at the top of what this image wrote. The
// order cannot change the outcome — nothing in the declarative block may set
// a `tls *` file (globalOptionOwners refuses them) — so it is chosen for the
// reader.
func (c *Config) EffectiveGlobalOptions() []GlobalOption {
	return append(c.TLSOptions(), c.GlobalOptions...)
}

// OptionSource names the environment variable an applied [global] setting
// came from, so that a log line about it can send the operator to the right
// place. It is exhaustive rather than a guess: the three `tls *` files can
// only come from the SAMBA_TLS_* variables, because globalOptionOwners
// refuses them everywhere else.
func OptionSource(key string) string {
	switch key {
	case tlsCertFileKey:
		return EnvTLSCertFile
	case tlsKeyFileKey:
		return EnvTLSKeyFile
	case tlsCAFileKey:
		return EnvTLSCAFile
	default:
		return EnvGlobalOptions
	}
}

// CheckTLSMaterial verifies, at start, that each file the trio names is there
// and can be opened. It reads nothing: one of the three is a private key, and
// code that never holds the content cannot leak it into a message, a log or a
// crash report. The refusal is CodeSecretError for the same reason.
//
// Checking at start rather than trusting samba is what turns a misconfigured
// mount into one clear line. Samba would not say much: with a file it cannot
// use it goes back to its own self-signed material, which is a working DC
// serving the wrong certificate — visible only to whoever next verifies the
// chain, long after the container came up healthy.
func CheckTLSMaterial(c *Config) error {
	for _, v := range []struct {
		env, path  string
		privateKey bool
	}{
		{env: EnvTLSCertFile, path: c.TLSCertFile},
		{env: EnvTLSKeyFile, path: c.TLSKeyFile, privateKey: true},
		{env: EnvTLSCAFile, path: c.TLSCAFile},
	} {
		if v.path == "" {
			continue
		}
		info, err := checkReadableFile(v.env, v.path)
		if err != nil {
			return err
		}
		if v.privateKey {
			if err := checkPrivateKeyPermissions(v.env, v.path, info); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkReadableFile is CheckTLSMaterial's per-file half: it opens the file
// (which is what proves it is readable — os.Stat only proves it exists) and
// closes it again without reading a byte.
func checkReadableFile(variable, path string) (os.FileInfo, error) {
	// Stat BEFORE open, and refuse anything that is not a regular file.
	// Opening first would be a way to hang the boot forever: a FIFO at the
	// path blocks in open(2) until somebody writes to it, and a container
	// stuck there never starts and never says why.
	info, err := os.Stat(path)
	if err != nil {
		return nil, Refuse(CodeSecretError,
			"%s names %q, which cannot be read (%s); bind-mount the file into the container read-only at that exact path and make it readable by the container user",
			variable, path, errReason(err))
	}
	if info.IsDir() {
		return nil, Refuse(CodeSecretError,
			"%s names %q, which is a directory; point %s at the PEM file itself, not at the directory the material is mounted in",
			variable, path, variable)
	}
	if !info.Mode().IsRegular() {
		return nil, Refuse(CodeSecretError,
			"%s names %q, which is not a regular file (%s); point %s at the PEM file itself",
			variable, path, info.Mode().Type(), variable)
	}
	if info.Size() == 0 {
		return nil, Refuse(CodeSecretError,
			"%s names %q, which is empty; write the PEM material into the file before starting the container",
			variable, path)
	}

	// Opening is what proves the file is READABLE — a stat succeeds on a
	// file whose mode denies it. Nothing is read from the handle.
	f, err := os.Open(path) //nolint:gosec // the path is the operator's own, and nothing is read from it
	if err != nil {
		return nil, Refuse(CodeSecretError,
			"%s names %q, which cannot be read (%s); bind-mount the file into the container read-only at that exact path and make it readable by the container user",
			variable, path, errReason(err))
	}
	_ = f.Close()
	return info, nil
}

// tlsKeyMode is the ONLY mode samba accepts on a TLS private key. It is not
// this image's rule and it is not "no group or other access": samba compares
// the low nine bits for equality and refuses 0400 exactly as it refuses 0644.
//
// Measured against Samba 4.24.7 in this image, by starting a DC on a key at
// each mode:
//
//	0400  invalid permissions on file '…': has 0400 should be 0600
//	0640  … has 0640 should be 0600
//	0660  … has 0660 should be 0600
//	0600  starts
//
// And it is fatal, not advisory. The message samba prints cites
// CVE-2013-4476, `ldapsrv_task_init` then fails with
// NT_STATUS_CANT_ACCESS_DOMAIN_INFO, and the whole server terminates — so a
// DC whose key is mounted 0644 does not come up degraded, it does not come up
// at all. Which is why this is worth checking here: the container log would
// otherwise carry that failure twenty lines deep in samba's own output, and
// the operator would be looking for a networking fault.
const tlsKeyMode = os.FileMode(0o600)

// checkPrivateKeyPermissions enforces tlsKeyMode, and the ownership samba
// checks alongside it: the key must belong to the user samba runs as, which
// is the user this process runs as.
//
// Ownership is the half a bind mount gets wrong by default. Docker preserves
// the host file's uid, so a key generated by an ordinary user and mounted in
// is owned by that user's uid inside the container — where samba runs as
// root — and samba refuses it however carefully its mode was set.
func checkPrivateKeyPermissions(variable, path string, info os.FileInfo) error {
	want := tlsKeyOwner()
	if mode := info.Mode().Perm(); mode != tlsKeyMode {
		return Refuse(CodeSecretError,
			"%s names %q, whose permissions are %04o; samba refuses to start its LDAP server unless the TLS private key is exactly mode %04o (it cites CVE-2013-4476 and terminates), so run chmod %04o on the file before mounting it",
			variable, path, mode, tlsKeyMode, tlsKeyMode)
	}
	if uid, ok := fileOwner(info); ok && uid != want {
		return Refuse(CodeSecretError,
			"%s names %q, which is owned by uid %d; samba refuses to start its LDAP server unless the TLS private key belongs to the user it runs as (uid %d in this container), so run chown %d:%d on the file on the host before mounting it",
			variable, path, uid, want, want, os.Getegid())
	}
	return nil
}

// tlsKeyOwner reports the uid the TLS private key must belong to. It is a
// variable so a unit test can pin the ownership refusal without needing a
// file it is not allowed to create, and it is the EFFECTIVE uid because that
// is what samba compares against (file_check_permissions is called with
// geteuid()).
var tlsKeyOwner = os.Geteuid

// fileOwner returns the uid owning info, and whether the platform told us.
// The stat structure is not part of the io/fs contract, so a type that does
// not carry one leaves ownership unchecked rather than refused: on such a
// platform this code cannot know, and refusing on ignorance would be worse
// than letting samba have the last word.
func fileOwner(info os.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// joinAnd renders a list of variable names for a sentence: "A", "A and B",
// "A, B and C".
func joinAnd(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// isAre picks the verb that agrees with a list rendered by joinAnd.
func isAre(names []string) string {
	if len(names) == 1 {
		return "is"
	}
	return "are"
}

// indexGlobalOption returns the position of key in opts, or -1.
func indexGlobalOption(opts []GlobalOption, canonical string) int {
	for i, o := range opts {
		if CanonicalKey(o.Key) == canonical {
			return i
		}
	}
	return -1
}

// containsKey reports whether keys already holds a name samba would consider
// the same one as canonical.
func containsKey(keys []string, canonical string) bool {
	for _, k := range keys {
		if CanonicalKey(k) == canonical {
			return true
		}
	}
	return false
}

// normalizeSpace collapses the whitespace of one smb.conf key or value so
// that what is WRITTEN is one tidy line whatever the block scalar carried. It
// is the same normalization run.normalize applies to a value read back out of
// smb.conf, and the two must stay identical or an option would be rewritten
// on every single start.
//
// It is not what decides whether two parameter NAMES are the same one: samba
// ignores whitespace entirely there, which is CanonicalKey's job.
func normalizeSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ReadSecret reads the secret stored in the file at path and returns it with
// surrounding whitespace removed. A missing, unreadable or empty file is a
// *Refusal with CodeSecretError. The secret value is never included in the
// message.
func ReadSecret(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", Refuse(CodeSecretError,
			"no secret file path was given; set the matching SAMBA_*_PASSWORD_FILE variable to a mounted secret file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", Refuse(CodeSecretError,
			"secret file %q cannot be read (%s); mount the file into the container and make it readable by the container user",
			path, errReason(err))
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return "", Refuse(CodeSecretError,
			"secret file %q is empty; write the password into the file before starting the container",
			path)
	}
	return secret, nil
}

// errReason renders an os error without repeating the path (already quoted by
// the caller) and without any file content. Unwrapping goes through errors.As
// so a wrapped *os.PathError is still recognized.
func errReason(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// isValidMode reports whether m is one of the supported modes.
func isValidMode(m Mode) bool {
	for _, v := range validModes {
		if m == v {
			return true
		}
	}
	return false
}

// modeList renders the supported modes for an operator-facing message.
func modeList() string {
	names := make([]string, 0, len(validModes))
	for _, m := range validModes {
		names = append(names, string(m))
	}
	return strings.Join(names, ", ")
}

// firstLabel returns the part of a dotted name before the first dot.
func firstLabel(realm string) string {
	if i := strings.Index(realm, "."); i >= 0 {
		return realm[:i]
	}
	return realm
}
