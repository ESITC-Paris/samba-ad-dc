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
	"strconv"
	"strings"
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

	opts, shadowed, err := parseGlobalOptions(get("SAMBA_GLOBAL_OPTIONS"))
	if err != nil {
		return nil, err
	}
	cfg.GlobalOptions, cfg.GlobalOptionsShadowed = opts, shadowed

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
//   - the tls * files are reserved for the SAMBA_TLS_* variables that own
//     them, which mount and check the material before naming it. They are
//     refused from the moment SAMBA_GLOBAL_OPTIONS exists rather than from
//     the moment those variables do, so that no deployment can come to
//     depend on setting them by hand first.
var globalOptionOwners = map[string]string{
	"realm":                      "SAMBA_REALM",
	"workgroup":                  "SAMBA_DOMAIN",
	"netbios name":               ownerHostname,
	"ad dc functional level":     "SAMBA_FUNCTION_LEVEL",
	"dns forwarder":              "SAMBA_DNS_FORWARDER",
	"tls certfile":               "SAMBA_TLS_CERT_FILE",
	"tls keyfile":                "SAMBA_TLS_KEY_FILE",
	"tls cafile":                 "SAMBA_TLS_CA_FILE",
	"server role":                ownerImage,
	"dns update command":         ownerImage,
	"ntp signd socket directory": ownerImage,
	"include":                    ownerImage,
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
		if owner, owned := globalOptionOwners[key]; owned {
			return nil, nil, globalOptionRefusal(key, owner)
		}

		opt := GlobalOption{Key: key, Value: normalizeSpace(rawValue)}
		if i := indexGlobalOption(opts, key); i >= 0 {
			opts = append(opts[:i], opts[i+1:]...)
			if !containsString(shadowed, key) {
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

// indexGlobalOption returns the position of key in opts, or -1.
func indexGlobalOption(opts []GlobalOption, key string) int {
	for i, o := range opts {
		if o.Key == key {
			return i
		}
	}
	return -1
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

// normalizeSpace collapses the whitespace of one smb.conf key or value, so
// that spacing never decides whether two settings are the same one. It is the
// same normalization run.withGlobalSetting applies when it reads an existing
// smb.conf back, and the two must stay identical or an option would be
// rewritten on every single start.
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
