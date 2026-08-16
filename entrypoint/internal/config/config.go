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

// Config is the validated configuration of one entrypoint run. It is a
// comparable value type: no pointers, no slices.
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
