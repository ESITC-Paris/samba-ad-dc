package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// envMap turns a map into the getenv function Load consumes.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// refusalOf asserts err is a *Refusal with the expected code and returns it.
func refusalOf(t *testing.T, err error, wantCode int) *Refusal {
	t.Helper()
	if err == nil {
		t.Fatalf("expected refusal with code %d, got nil error", wantCode)
	}
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("expected *Refusal, got %T: %v", err, err)
	}
	if r.Code != wantCode {
		t.Fatalf("expected refusal code %d, got %d (msg: %s)", wantCode, r.Code, r.Msg)
	}
	if strings.TrimSpace(r.Msg) == "" {
		t.Fatalf("refusal message must not be empty")
	}
	if strings.Contains(r.Msg, "\n") {
		t.Fatalf("refusal message must be a single line, got %q", r.Msg)
	}
	if r.Error() != r.Msg {
		t.Fatalf("Error() = %q, want %q", r.Error(), r.Msg)
	}
	return r
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(envMap(map[string]string{}))
	if err != nil {
		t.Fatalf("Load with empty env: unexpected error: %v", err)
	}
	if cfg.Mode != ModeAuto {
		t.Errorf("Mode = %q, want %q", cfg.Mode, ModeAuto)
	}
	if !cfg.Chrony {
		t.Errorf("Chrony = false, want true (default on)")
	}
	if cfg.FunctionLevel != "2016" {
		t.Errorf("FunctionLevel = %q, want %q", cfg.FunctionLevel, "2016")
	}
	if cfg.JoinUsername != "Administrator" {
		t.Errorf("JoinUsername = %q, want %q", cfg.JoinUsername, "Administrator")
	}
	if cfg.DNSBackend != "SAMBA_INTERNAL" {
		t.Errorf("DNSBackend = %q, want %q", cfg.DNSBackend, "SAMBA_INTERNAL")
	}
	if cfg.LogLevel != 1 {
		t.Errorf("LogLevel = %d, want 1", cfg.LogLevel)
	}
	if cfg.MaintenanceOp != "check" {
		t.Errorf("MaintenanceOp = %q, want %q", cfg.MaintenanceOp, "check")
	}
	if cfg.DNSForwarder != "" {
		t.Errorf("DNSForwarder = %q, want empty", cfg.DNSForwarder)
	}
}

func TestLoadRefusals(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		wantCode int
		// wantMsgContains: every fragment must appear in the message
		// (cause + remedy, naming the variable the operator must fix).
		wantMsgContains []string
		// wantMsgOmits: fragments that must NOT appear — secret values
		// refused out of the environment are still secrets.
		wantMsgOmits []string
	}{
		{
			name:            "invalid mode",
			env:             map[string]string{"SAMBA_MODE": "bootstrap"},
			wantCode:        10,
			wantMsgContains: []string{"SAMBA_MODE", "bootstrap", "auto"},
		},
		{
			name:            "plain admin password in environment",
			env:             map[string]string{"SAMBA_ADMIN_PASSWORD": "hunter2"},
			wantCode:        10,
			wantMsgContains: []string{"SAMBA_ADMIN_PASSWORD_FILE"},
			wantMsgOmits:    []string{"hunter2"},
		},
		{
			name:            "plain join password in environment",
			env:             map[string]string{"SAMBA_JOIN_PASSWORD": "hunter2"},
			wantCode:        10,
			wantMsgContains: []string{"SAMBA_JOIN_PASSWORD_FILE"},
			wantMsgOmits:    []string{"hunter2"},
		},
		{
			name: "provision without realm",
			env: map[string]string{
				"SAMBA_MODE":                "provision",
				"SAMBA_ADMIN_PASSWORD_FILE": "/run/secrets/admin",
			},
			wantCode:        10,
			wantMsgContains: []string{"SAMBA_REALM", "provision"},
		},
		{
			name: "provision without admin password file",
			env: map[string]string{
				"SAMBA_MODE":  "provision",
				"SAMBA_REALM": "AD.EXAMPLE.COM",
			},
			wantCode:        10,
			wantMsgContains: []string{"SAMBA_ADMIN_PASSWORD_FILE", "provision"},
		},
		{
			name:            "join without realm",
			env:             map[string]string{"SAMBA_MODE": "join", "SAMBA_JOIN_PASSWORD_FILE": "/run/secrets/join"},
			wantCode:        10,
			wantMsgContains: []string{"SAMBA_REALM", "join"},
		},
		{
			name:            "join without join password file",
			env:             map[string]string{"SAMBA_MODE": "join", "SAMBA_REALM": "AD.EXAMPLE.COM"},
			wantCode:        10,
			wantMsgContains: []string{"SAMBA_JOIN_PASSWORD_FILE", "join"},
		},
		{
			name:            "unsupported dns backend",
			env:             map[string]string{"SAMBA_DNS_BACKEND": "BIND9_DLZ"},
			wantCode:        10,
			wantMsgContains: []string{"SAMBA_DNS_BACKEND", "BIND9_DLZ", "SAMBA_INTERNAL"},
		},
		{
			name:            "non-integer log level",
			env:             map[string]string{"SAMBA_LOG_LEVEL": "verbose"},
			wantCode:        10,
			wantMsgContains: []string{"SAMBA_LOG_LEVEL", "verbose"},
		},
		{
			name:            "negative log level",
			env:             map[string]string{"SAMBA_LOG_LEVEL": "-1"},
			wantCode:        10,
			wantMsgContains: []string{"SAMBA_LOG_LEVEL"},
		},
		{
			name:            "invalid chrony toggle",
			env:             map[string]string{"SAMBA_CHRONY": "yes"},
			wantCode:        10,
			wantMsgContains: []string{"SAMBA_CHRONY", "on", "off"},
		},
		{
			name:            "invalid maintenance op",
			env:             map[string]string{"SAMBA_MODE": "maintenance", "SAMBA_MAINTENANCE_OP": "rebuild"},
			wantCode:        10,
			wantMsgContains: []string{"SAMBA_MAINTENANCE_OP", "check", "repair"},
		},
		{
			name:            "realm without a dot",
			env:             map[string]string{"SAMBA_MODE": "provision", "SAMBA_REALM": "EXAMPLE", "SAMBA_ADMIN_PASSWORD_FILE": "/run/secrets/admin"},
			wantCode:        10,
			wantMsgContains: []string{"SAMBA_REALM", "EXAMPLE"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(envMap(tc.env))
			if cfg != nil {
				t.Errorf("expected nil config on refusal, got %+v", cfg)
			}
			r := refusalOf(t, err, tc.wantCode)
			for _, frag := range tc.wantMsgContains {
				if !strings.Contains(r.Msg, frag) {
					t.Errorf("message %q does not contain %q", r.Msg, frag)
				}
			}
			for _, frag := range tc.wantMsgOmits {
				if strings.Contains(r.Msg, frag) {
					t.Errorf("message %q leaked the secret value %q", r.Msg, frag)
				}
			}
		})
	}
}

func TestLoadValid(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Config
	}{
		{
			name: "domain defaults to first label of realm",
			env: map[string]string{
				"SAMBA_MODE":                "provision",
				"SAMBA_REALM":               "ad.example.com",
				"SAMBA_ADMIN_PASSWORD_FILE": "/run/secrets/admin",
			},
			want: Config{
				Mode: ModeProvision, Realm: "AD.EXAMPLE.COM", Domain: "AD",
				AdminPasswordFile: "/run/secrets/admin", JoinUsername: "Administrator",
				DNSBackend: "SAMBA_INTERNAL", FunctionLevel: "2016",
				LogLevel: 1, Chrony: true, MaintenanceOp: "check",
			},
		},
		{
			name: "explicit domain wins over realm first label",
			env: map[string]string{
				"SAMBA_MODE":                "provision",
				"SAMBA_REALM":               "AD.EXAMPLE.COM",
				"SAMBA_DOMAIN":              "corp",
				"SAMBA_ADMIN_PASSWORD_FILE": "/run/secrets/admin",
				"SAMBA_DNS_FORWARDER":       "10.0.0.53",
				"SAMBA_FUNCTION_LEVEL":      "2012_R2",
				"SAMBA_LOG_LEVEL":           "3",
				"SAMBA_CHRONY":              "off",
			},
			want: Config{
				Mode: ModeProvision, Realm: "AD.EXAMPLE.COM", Domain: "CORP",
				AdminPasswordFile: "/run/secrets/admin", JoinUsername: "Administrator",
				DNSForwarder: "10.0.0.53", DNSBackend: "SAMBA_INTERNAL",
				FunctionLevel: "2012_R2", LogLevel: 3, Chrony: false,
				MaintenanceOp: "check",
			},
		},
		{
			name: "join mode with explicit username",
			env: map[string]string{
				"SAMBA_MODE":               "join",
				"SAMBA_REALM":              "AD.EXAMPLE.COM",
				"SAMBA_JOIN_PASSWORD_FILE": "/run/secrets/join",
				"SAMBA_JOIN_USERNAME":      "joiner",
			},
			want: Config{
				Mode: ModeJoin, Realm: "AD.EXAMPLE.COM", Domain: "AD",
				JoinPasswordFile: "/run/secrets/join", JoinUsername: "joiner",
				DNSBackend: "SAMBA_INTERNAL", FunctionLevel: "2016",
				LogLevel: 1, Chrony: true, MaintenanceOp: "check",
			},
		},
		{
			name: "auto mode does not require realm (decided later from state)",
			env:  map[string]string{"SAMBA_MODE": "auto"},
			want: Config{
				Mode: ModeAuto, JoinUsername: "Administrator",
				DNSBackend: "SAMBA_INTERNAL", FunctionLevel: "2016",
				LogLevel: 1, Chrony: true, MaintenanceOp: "check",
			},
		},
		{
			name: "run mode does not require realm or secrets",
			env:  map[string]string{"SAMBA_MODE": "run", "SAMBA_LOG_LEVEL": "0"},
			want: Config{
				Mode: ModeRun, JoinUsername: "Administrator",
				DNSBackend: "SAMBA_INTERNAL", FunctionLevel: "2016",
				LogLevel: 0, Chrony: true, MaintenanceOp: "check",
			},
		},
		{
			name: "maintenance repair",
			env:  map[string]string{"SAMBA_MODE": "maintenance", "SAMBA_MAINTENANCE_OP": "repair"},
			want: Config{
				Mode: ModeMaintenance, JoinUsername: "Administrator",
				DNSBackend: "SAMBA_INTERNAL", FunctionLevel: "2016",
				LogLevel: 1, Chrony: true, MaintenanceOp: "repair",
			},
		},
		{
			name: "values are trimmed of surrounding whitespace",
			env: map[string]string{
				"SAMBA_MODE":                "  provision ",
				"SAMBA_REALM":               " ad.example.com\n",
				"SAMBA_ADMIN_PASSWORD_FILE": " /run/secrets/admin ",
			},
			want: Config{
				Mode: ModeProvision, Realm: "AD.EXAMPLE.COM", Domain: "AD",
				AdminPasswordFile: "/run/secrets/admin", JoinUsername: "Administrator",
				DNSBackend: "SAMBA_INTERNAL", FunctionLevel: "2016",
				LogLevel: 1, Chrony: true, MaintenanceOp: "check",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(envMap(tc.env))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// DeepEqual rather than ==: Config carries the declarative
			// [global] options as a slice, so it is no longer a
			// comparable type. The assertion is unchanged — the WHOLE
			// value, field for field, so a new field with a wrong
			// default cannot slip past.
			if !reflect.DeepEqual(*cfg, tc.want) {
				t.Errorf("Load() = %+v\nwant %+v", *cfg, tc.want)
			}
		})
	}
}

func TestReadSecret(t *testing.T) {
	dir := t.TempDir()

	newline := filepath.Join(dir, "trailing-newline")
	if err := os.WriteFile(newline, []byte("s3cr3t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spaces := filepath.Join(dir, "surrounding-space")
	if err := os.WriteFile(spaces, []byte("  s3cr3t \r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(dir, "inner-space")
	if err := os.WriteFile(inner, []byte("pass word\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("trailing newline is trimmed", func(t *testing.T) {
		got, err := ReadSecret(newline)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "s3cr3t" {
			t.Errorf("ReadSecret = %q, want %q", got, "s3cr3t")
		}
	})

	t.Run("surrounding whitespace is trimmed", func(t *testing.T) {
		got, err := ReadSecret(spaces)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "s3cr3t" {
			t.Errorf("ReadSecret = %q, want %q", got, "s3cr3t")
		}
	})

	t.Run("inner spaces are preserved", func(t *testing.T) {
		got, err := ReadSecret(inner)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "pass word" {
			t.Errorf("ReadSecret = %q, want %q", got, "pass word")
		}
	})

	t.Run("missing file refuses with code 11", func(t *testing.T) {
		missing := filepath.Join(dir, "does-not-exist")
		got, err := ReadSecret(missing)
		if got != "" {
			t.Errorf("expected empty secret on failure, got %q", got)
		}
		r := refusalOf(t, err, 11)
		if !strings.Contains(r.Msg, missing) {
			t.Errorf("message %q does not name the file %q", r.Msg, missing)
		}
	})

	t.Run("empty file refuses with code 11", func(t *testing.T) {
		got, err := ReadSecret(empty)
		if got != "" {
			t.Errorf("expected empty secret on failure, got %q", got)
		}
		r := refusalOf(t, err, 11)
		if !strings.Contains(r.Msg, empty) {
			t.Errorf("message %q does not name the file %q", r.Msg, empty)
		}
	})

	t.Run("empty path refuses with code 11", func(t *testing.T) {
		if _, err := ReadSecret(""); err == nil {
			t.Fatal("expected refusal for empty path")
		} else {
			refusalOf(t, err, 11)
		}
	})

	t.Run("directory instead of file refuses with code 11", func(t *testing.T) {
		if _, err := ReadSecret(dir); err == nil {
			t.Fatal("expected refusal when path is a directory")
		} else {
			refusalOf(t, err, 11)
		}
	})

	t.Run("secret value never appears in the refusal message", func(t *testing.T) {
		// A refusal on a readable-but-empty file must not echo content.
		_, err := ReadSecret(empty)
		var r *Refusal
		if !errors.As(err, &r) {
			t.Fatalf("expected *Refusal, got %v", err)
		}
		if strings.Contains(r.Msg, "s3cr3t") {
			t.Errorf("refusal message leaked a secret value: %q", r.Msg)
		}
	})
}

// TestExitCodesAreTheirPublishedLiterals pins the numbers themselves, not
// the constant names. The exit-code table in docs/adaptation-profile.md
// (Runtime contract → Exit codes) is the published contract these values are
// frozen by: operators, compose restart policies and the E2E matrix all
// branch on the literals, so renaming a constant is free but renumbering one
// is a breaking change. Asserting `CodeConfigError == CodeConfigError` would
// prove nothing; only the literal does.
func TestExitCodesAreTheirPublishedLiterals(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"configuration error", CodeConfigError, 10},
		{"missing/unreadable secret file", CodeSecretError, 11},
		{"provision/join over existing state", CodeStateExists, 20},
		{"run/maintenance with absent state", CodeStateAbsent, 21},
		{"downgrade refusal", CodeDowngrade, 22},
		{"database consistency check failure", CodeDBCheckFailed, 23},
		{"samba runtime failure", CodeRuntimeFailure, 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("exit code for %s = %d, want %d; docs/adaptation-profile.md publishes %d and exit codes are immutable once released",
					tc.name, tc.got, tc.want, tc.want)
			}
		})
	}
}

func TestRefuse(t *testing.T) {
	r := Refuse(CodeStateAbsent, "no state in %s; mount the %s volume", "/var/lib/samba", "state")
	if r.Code != CodeStateAbsent {
		t.Errorf("Code = %d, want %d", r.Code, CodeStateAbsent)
	}
	if r.Msg != "no state in /var/lib/samba; mount the state volume" {
		t.Errorf("Msg = %q", r.Msg)
	}
	var err error = r
	var got *Refusal
	if !errors.As(err, &got) || got != r {
		t.Errorf("errors.As did not recover the refusal built by Refuse")
	}
}

func TestRefusalIsAnError(t *testing.T) {
	var err error = &Refusal{Code: 10, Msg: "cause; remedy"}
	if err.Error() != "cause; remedy" {
		t.Errorf("Error() = %q", err.Error())
	}
	var r *Refusal
	if !errors.As(err, &r) || r.Code != 10 {
		t.Errorf("errors.As did not recover the refusal")
	}
}

// ---------------------------------------------------------------------------
// SAMBA_GLOBAL_OPTIONS
// ---------------------------------------------------------------------------

// TestLoadGlobalOptions pins the parsing of the declarative [global] block.
// The variable is written as a compose `|` block scalar, so the value really
// does arrive with blank lines, comments and whatever indentation the author
// left behind: each of those is a case here rather than an assumption.
func TestLoadGlobalOptions(t *testing.T) {
	for _, tc := range []struct {
		name         string
		value        string
		want         []GlobalOption
		wantShadowed []string
	}{
		{
			name:  "two entries, in declaration order",
			value: "smb encrypt = required\nlog level = 1 auth:3\n",
			want: []GlobalOption{
				{Key: "smb encrypt", Value: "required"},
				{Key: "log level", Value: "1 auth:3"},
			},
		},
		{
			name:  "blank lines and comments are ignored",
			value: "\n# what this DC encrypts\n\n  smb encrypt = required  \n\t# and nothing else\n\n",
			want:  []GlobalOption{{Key: "smb encrypt", Value: "required"}},
		},
		{
			name:  "keys are normalised to lower case and single spaces",
			value: "Smb   Encrypt = required",
			want:  []GlobalOption{{Key: "smb encrypt", Value: "required"}},
		},
		{
			name:  "a value keeps its words, not its spacing",
			value: "log level =  1   auth:3 ",
			want:  []GlobalOption{{Key: "log level", Value: "1 auth:3"}},
		},
		{
			name:  "a value may contain an equals sign",
			value: "idmap config * : backend = tdb",
			want:  []GlobalOption{{Key: "idmap config * : backend", Value: "tdb"}},
		},
		{
			name:  "a repeated key keeps the last value, and says so",
			value: "smb encrypt = desired\nlog level = 2\nsmb encrypt = required\n",
			want: []GlobalOption{
				{Key: "log level", Value: "2"},
				{Key: "smb encrypt", Value: "required"},
			},
			wantShadowed: []string{"smb encrypt"},
		},
		{
			name:  "an empty variable declares nothing",
			value: "\n  \n# only a comment\n",
			want:  nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(envMap(map[string]string{"SAMBA_GLOBAL_OPTIONS": tc.value}))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(cfg.GlobalOptions, tc.want) {
				t.Errorf("GlobalOptions = %+v, want %+v", cfg.GlobalOptions, tc.want)
			}
			if !reflect.DeepEqual(cfg.GlobalOptionsShadowed, tc.wantShadowed) {
				t.Errorf("GlobalOptionsShadowed = %+v, want %+v", cfg.GlobalOptionsShadowed, tc.wantShadowed)
			}
		})
	}
}

// TestLoadGlobalOptionsRefusals covers the two ways the variable is refused:
// a line that is not a setting at all, and a setting this image owns.
//
// The owned keys are the point of the exercise. Every one of them is already
// derived from something else — another variable, the container's host name,
// or a decision of the image — and a [global] entry quietly overriding it
// would either contradict what the operator asked for elsewhere or break the
// DC outright. The message must therefore name the OWNER, not merely say no:
// an operator who set `realm` in SAMBA_GLOBAL_OPTIONS needs to be told to set
// SAMBA_REALM instead.
func TestLoadGlobalOptionsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name            string
		value           string
		wantMsgContains []string
	}{
		{
			name:            "a line that is not a setting",
			value:           "smb encrypt = required\nthis line has no equals sign\n",
			wantMsgContains: []string{"SAMBA_GLOBAL_OPTIONS", "this line has no equals sign"},
		},
		{
			name:            "an empty key",
			value:           " = required",
			wantMsgContains: []string{"SAMBA_GLOBAL_OPTIONS"},
		},
		{
			name:            "realm is owned by SAMBA_REALM",
			value:           "realm = OTHER.EXAMPLE.COM",
			wantMsgContains: []string{"SAMBA_GLOBAL_OPTIONS", "realm", "SAMBA_REALM"},
		},
		{
			name:            "workgroup is owned by SAMBA_DOMAIN",
			value:           "workgroup = OTHER",
			wantMsgContains: []string{"workgroup", "SAMBA_DOMAIN"},
		},
		{
			name:            "netbios name is owned by the container host name",
			value:           "netbios name = dc99",
			wantMsgContains: []string{"netbios name", "container"},
		},
		{
			name:            "ad dc functional level is owned by SAMBA_FUNCTION_LEVEL",
			value:           "ad dc functional level = 2012",
			wantMsgContains: []string{"ad dc functional level", "SAMBA_FUNCTION_LEVEL"},
		},
		{
			name:            "dns forwarder is owned by SAMBA_DNS_FORWARDER",
			value:           "dns forwarder = 10.0.0.53",
			wantMsgContains: []string{"dns forwarder", "SAMBA_DNS_FORWARDER"},
		},
		{
			name:            "tls certfile is owned by SAMBA_TLS_CERT_FILE",
			value:           "tls certfile = /tls/cert.pem",
			wantMsgContains: []string{"tls certfile", "SAMBA_TLS_CERT_FILE"},
		},
		{
			name:            "tls keyfile is owned by SAMBA_TLS_KEY_FILE",
			value:           "tls keyfile = /tls/key.pem",
			wantMsgContains: []string{"tls keyfile", "SAMBA_TLS_KEY_FILE"},
		},
		{
			name:            "tls cafile is owned by SAMBA_TLS_CA_FILE",
			value:           "tls cafile = /tls/ca.pem",
			wantMsgContains: []string{"tls cafile", "SAMBA_TLS_CA_FILE"},
		},
		{
			name:            "server role is managed by the image",
			value:           "server role = standalone server",
			wantMsgContains: []string{"server role", "image"},
		},
		{
			name:            "dns update command is managed by the image",
			value:           "dns update command = /usr/bin/nsupdate",
			wantMsgContains: []string{"dns update command", "image"},
		},
		{
			name:            "ntp signd socket directory is managed by the image",
			value:           "ntp signd socket directory = /srv/signd",
			wantMsgContains: []string{"ntp signd socket directory", "image"},
		},
		{
			name:            "include is managed by the image",
			value:           "include = /etc/samba/extra.conf",
			wantMsgContains: []string{"include", "image"},
		},
		{
			name:            "an owned key is recognised whatever its spacing and case",
			value:           "DNS    Forwarder = 10.0.0.53",
			wantMsgContains: []string{"dns forwarder", "SAMBA_DNS_FORWARDER"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(envMap(map[string]string{"SAMBA_GLOBAL_OPTIONS": tc.value}))
			if cfg != nil {
				t.Errorf("expected nil config on refusal, got %+v", cfg)
			}
			r := refusalOf(t, err, CodeConfigError)
			for _, frag := range tc.wantMsgContains {
				if !strings.Contains(r.Msg, frag) {
					t.Errorf("message %q does not contain %q", r.Msg, frag)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// SAMBA_TLS_CERT_FILE / SAMBA_TLS_KEY_FILE / SAMBA_TLS_CA_FILE
// ---------------------------------------------------------------------------

// TestLoadTLSMaterial covers the accepting half of the trio: the three paths
// land on the config, and they become the three `tls *` [global] settings
// BEFORE whatever SAMBA_GLOBAL_OPTIONS declares.
//
// The order is not cosmetic. Both lists are applied through the same
// reconciliation, and putting the material first means an operator reading
// their smb.conf finds the certificate the DC serves at the top of what this
// image wrote, next to the identity settings it belongs with.
func TestLoadTLSMaterial(t *testing.T) {
	cfg, err := Load(envMap(map[string]string{
		"SAMBA_TLS_CERT_FILE":  "/run/secrets/tls/cert.pem",
		"SAMBA_TLS_KEY_FILE":   "/run/secrets/tls/key.pem",
		"SAMBA_TLS_CA_FILE":    "/run/secrets/tls/ca.pem",
		"SAMBA_GLOBAL_OPTIONS": "max log size = 4000",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TLSCertFile != "/run/secrets/tls/cert.pem" ||
		cfg.TLSKeyFile != "/run/secrets/tls/key.pem" ||
		cfg.TLSCAFile != "/run/secrets/tls/ca.pem" {
		t.Fatalf("the three paths did not reach the config: %+v", cfg)
	}

	want := []GlobalOption{
		{Key: "tls certfile", Value: "/run/secrets/tls/cert.pem"},
		{Key: "tls keyfile", Value: "/run/secrets/tls/key.pem"},
		{Key: "tls cafile", Value: "/run/secrets/tls/ca.pem"},
		{Key: "max log size", Value: "4000"},
	}
	if got := cfg.EffectiveGlobalOptions(); !reflect.DeepEqual(got, want) {
		t.Errorf("EffectiveGlobalOptions() = %+v, want %+v", got, want)
	}
	// The declarative block itself must not have grown three entries it
	// never declared: the two lists are separate inputs and only the
	// effective view joins them.
	if got := len(cfg.GlobalOptions); got != 1 {
		t.Errorf("GlobalOptions holds %d entries, want the 1 that was declared: %+v", got, cfg.GlobalOptions)
	}
}

// With none of the three set the DC keeps today's behaviour: samba's own
// self-signed material, and not one extra [global] setting.
func TestLoadWithoutTLSMaterial(t *testing.T) {
	cfg, err := Load(envMap(map[string]string{"SAMBA_GLOBAL_OPTIONS": "max log size = 4000"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TLSOptions() != nil {
		t.Errorf("TLSOptions() = %+v with nothing set, want nil", cfg.TLSOptions())
	}
	want := []GlobalOption{{Key: "max log size", Value: "4000"}}
	if got := cfg.EffectiveGlobalOptions(); !reflect.DeepEqual(got, want) {
		t.Errorf("EffectiveGlobalOptions() = %+v, want %+v", got, want)
	}
}

// TestLoadTLSMaterialPartialRefused: all three or none.
//
// A partial trio is refused rather than half-applied because every partial
// combination is a DC that either does not start or serves a certificate the
// operator did not intend — and the operator would have no way to tell which,
// since samba falls back to its own self-signed material without a word.
// The message names the ones that are MISSING: that is what has to be fixed.
func TestLoadTLSMaterialPartialRefused(t *testing.T) {
	const cert, key, ca = "/run/secrets/tls/cert.pem", "/run/secrets/tls/key.pem", "/run/secrets/tls/ca.pem"
	for _, tc := range []struct {
		name            string
		env             map[string]string
		wantMsgContains []string
	}{
		{
			name: "only the certificate",
			env:  map[string]string{"SAMBA_TLS_CERT_FILE": cert},
			wantMsgContains: []string{
				"SAMBA_TLS_CERT_FILE", "SAMBA_TLS_KEY_FILE", "SAMBA_TLS_CA_FILE",
			},
		},
		{
			name:            "only the key",
			env:             map[string]string{"SAMBA_TLS_KEY_FILE": key},
			wantMsgContains: []string{"SAMBA_TLS_CERT_FILE", "SAMBA_TLS_CA_FILE"},
		},
		{
			name:            "only the CA",
			env:             map[string]string{"SAMBA_TLS_CA_FILE": ca},
			wantMsgContains: []string{"SAMBA_TLS_CERT_FILE", "SAMBA_TLS_KEY_FILE"},
		},
		{
			name:            "certificate and key, no CA",
			env:             map[string]string{"SAMBA_TLS_CERT_FILE": cert, "SAMBA_TLS_KEY_FILE": key},
			wantMsgContains: []string{"SAMBA_TLS_CA_FILE"},
		},
		{
			name:            "certificate and CA, no key",
			env:             map[string]string{"SAMBA_TLS_CERT_FILE": cert, "SAMBA_TLS_CA_FILE": ca},
			wantMsgContains: []string{"SAMBA_TLS_KEY_FILE"},
		},
		{
			name:            "key and CA, no certificate",
			env:             map[string]string{"SAMBA_TLS_KEY_FILE": key, "SAMBA_TLS_CA_FILE": ca},
			wantMsgContains: []string{"SAMBA_TLS_CERT_FILE"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(envMap(tc.env))
			if cfg != nil {
				t.Errorf("expected nil config on refusal, got %+v", cfg)
			}
			r := refusalOf(t, err, CodeConfigError)
			for _, frag := range tc.wantMsgContains {
				if !strings.Contains(r.Msg, frag) {
					t.Errorf("message %q does not contain %q", r.Msg, frag)
				}
			}
		})
	}
}

// TestCheckTLSMaterial covers the start-time check on the files themselves.
//
// It is a SECRET-class refusal (exit 11) for the same reason the password
// files are: one of the three is a private key, and the code that reports a
// problem with it must never be tempted to quote what is inside. The message
// names the variable and the path and stops there.
func TestCheckTLSMaterial(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const keyContent = "-----BEGIN PRIVATE KEY-----\nsupersecret\n-----END PRIVATE KEY-----\n"
	cert := write("cert.pem", "-----BEGIN CERTIFICATE-----\n")
	// 0600, because that is the only mode samba accepts on a private key —
	// see TestCheckTLSMaterialKeyPermissions, which is where that rule is
	// pinned. Here it only has to be out of the way.
	key := write("key.pem", keyContent)
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	ca := write("ca.pem", "-----BEGIN CERTIFICATE-----\n")
	empty := write("empty.pem", "")
	absent := filepath.Join(dir, "does-not-exist.pem")

	t.Run("nothing set is nothing to check", func(t *testing.T) {
		if err := CheckTLSMaterial(&Config{}); err != nil {
			t.Fatalf("unexpected refusal with no material declared: %v", err)
		}
	})

	t.Run("three readable files", func(t *testing.T) {
		cfg := &Config{TLSCertFile: cert, TLSKeyFile: key, TLSCAFile: ca}
		if err := CheckTLSMaterial(cfg); err != nil {
			t.Fatalf("unexpected refusal: %v", err)
		}
	})

	for _, tc := range []struct {
		name            string
		cfg             *Config
		wantMsgContains []string
	}{
		{
			name:            "the key file is not there",
			cfg:             &Config{TLSCertFile: cert, TLSKeyFile: absent, TLSCAFile: ca},
			wantMsgContains: []string{"SAMBA_TLS_KEY_FILE", absent},
		},
		{
			name:            "the certificate is not there",
			cfg:             &Config{TLSCertFile: absent, TLSKeyFile: key, TLSCAFile: ca},
			wantMsgContains: []string{"SAMBA_TLS_CERT_FILE", absent},
		},
		{
			name:            "the CA is not there",
			cfg:             &Config{TLSCertFile: cert, TLSKeyFile: key, TLSCAFile: absent},
			wantMsgContains: []string{"SAMBA_TLS_CA_FILE", absent},
		},
		{
			name:            "the path is a directory",
			cfg:             &Config{TLSCertFile: dir, TLSKeyFile: key, TLSCAFile: ca},
			wantMsgContains: []string{"SAMBA_TLS_CERT_FILE", dir},
		},
		{
			name:            "the file is empty",
			cfg:             &Config{TLSCertFile: cert, TLSKeyFile: empty, TLSCAFile: ca},
			wantMsgContains: []string{"SAMBA_TLS_KEY_FILE", empty},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := refusalOf(t, CheckTLSMaterial(tc.cfg), CodeSecretError)
			for _, frag := range tc.wantMsgContains {
				if !strings.Contains(r.Msg, frag) {
					t.Errorf("message %q does not contain %q", r.Msg, frag)
				}
			}
			if strings.Contains(r.Msg, "supersecret") {
				t.Errorf("the refusal quotes the private key: %q", r.Msg)
			}
		})
	}
}

// TestCheckTLSMaterialKeyPermissions pins the one rule that is not this
// image's but samba's, and that was MEASURED rather than assumed (Samba
// 4.24.7 in this image, every mode below tried against a running DC):
//
//	mode 0400  invalid permissions on file '…': has 0400 should be 0600
//	mode 0640  … has 0640 should be 0600
//	mode 0660  … has 0660 should be 0600
//	mode 0600  starts
//
// It is an EXACT comparison, not "no group or other access" — 0400 is refused
// too — and samba does not warn and continue: `ldapsrv_task_init` fails with
// NT_STATUS_CANT_ACCESS_DOMAIN_INFO and the whole server terminates. Checking
// it here is what turns that into one line naming the file and the fix.
//
// Only the private key is checked. The certificate and the CA are public
// material and samba reads them at any mode (both were 0644 on the DC that
// started).
func TestCheckTLSMaterialKeyPermissions(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("-----BEGIN-----\n"), mode); err != nil {
			t.Fatal(err)
		}
		// WriteFile's mode is masked by the umask, so set it explicitly.
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cert := write("cert.pem", 0o644)
	ca := write("ca.pem", 0o644)

	t.Run("0600 is accepted", func(t *testing.T) {
		cfg := &Config{TLSCertFile: cert, TLSKeyFile: write("ok.pem", 0o600), TLSCAFile: ca}
		if err := CheckTLSMaterial(cfg); err != nil {
			t.Fatalf("a 0600 key was refused: %v", err)
		}
	})

	for _, mode := range []os.FileMode{0o400, 0o640, 0o644, 0o660, 0o666} {
		t.Run("mode "+mode.String(), func(t *testing.T) {
			// A fresh name per mode: a 0400 file cannot be rewritten by the
			// next iteration, and the failure would look like the check.
			key := write(fmt.Sprintf("key-%o.pem", mode), mode)
			r := refusalOf(t, CheckTLSMaterial(&Config{TLSCertFile: cert, TLSKeyFile: key, TLSCAFile: ca}), CodeSecretError)
			for _, frag := range []string{EnvTLSKeyFile, key, "0600"} {
				if !strings.Contains(r.Msg, frag) {
					t.Errorf("message %q does not contain %q", r.Msg, frag)
				}
			}
		})
	}

	// The certificate and the CA are public: their mode is samba's business
	// and it does not object, so neither may this.
	t.Run("a world-readable certificate and CA are fine", func(t *testing.T) {
		cfg := &Config{
			TLSCertFile: write("open-cert.pem", 0o666),
			TLSKeyFile:  write("ok2.pem", 0o600),
			TLSCAFile:   write("open-ca.pem", 0o666),
		}
		if err := CheckTLSMaterial(cfg); err != nil {
			t.Fatalf("public material was refused over its mode: %v", err)
		}
	})
}

// TestLoadTLSMaterialRelativePathRefused: the three variables take ABSOLUTE
// paths, and a relative one is exit 10 rather than something this resolves
// quietly against its own working directory.
//
// The two programs disagree about what a relative TLS path means, which is
// what makes it worth a refusal of its own: samba resolves `tls certfile`
// against the private directory on the state volume — its defaults are
// `tls/cert.pem`, `tls/key.pem` and `tls/ca.pem`, measured — while this
// process would resolve it against wherever the entrypoint was started. A
// check that passed on one file while samba opened another would be worse
// than no check.
func TestLoadTLSMaterialRelativePathRefused(t *testing.T) {
	const abs = "/run/secrets/tls/"
	for _, tc := range []struct{ name, cert, key, ca, want string }{
		{"the certificate", "tls/cert.pem", abs + "key.pem", abs + "ca.pem", EnvTLSCertFile},
		{"the key", abs + "cert.pem", "key.pem", abs + "ca.pem", EnvTLSKeyFile},
		{"the CA", abs + "cert.pem", abs + "key.pem", "./ca.pem", EnvTLSCAFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(envMap(map[string]string{
				EnvTLSCertFile: tc.cert,
				EnvTLSKeyFile:  tc.key,
				EnvTLSCAFile:   tc.ca,
			}))
			if cfg != nil {
				t.Errorf("expected nil config on refusal, got %+v", cfg)
			}
			r := refusalOf(t, err, CodeConfigError)
			if !strings.Contains(r.Msg, tc.want) {
				t.Errorf("message %q does not name %q", r.Msg, tc.want)
			}
			if !strings.Contains(r.Msg, "absolute") {
				t.Errorf("message %q does not say the path must be absolute", r.Msg)
			}
		})
	}
}

// TestCheckTLSMaterialKeyOwnership pins the ownership half of the rule, which
// the mode table above cannot reach: a test process cannot create a file
// owned by somebody else. Instead the uid the key is REQUIRED to belong to is
// moved, which exercises exactly the comparison samba makes.
//
// It is the half a bind mount gets wrong by default — docker preserves the
// host file's uid, so a key generated by an ordinary user arrives owned by
// that uid inside a container where samba runs as root, and samba refuses it
// however carefully its mode was set.
func TestCheckTLSMaterialKeyOwnership(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("-----BEGIN-----\n"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg := &Config{
		TLSCertFile: write("cert.pem", 0o644),
		TLSKeyFile:  write("key.pem", 0o600),
		TLSCAFile:   write("ca.pem", 0o644),
	}
	if err := CheckTLSMaterial(cfg); err != nil {
		t.Fatalf("the material this test owns was refused before the uid was moved: %v", err)
	}

	mine := os.Geteuid()
	other := mine + 1
	restore := tlsKeyOwner
	tlsKeyOwner = func() int { return other }
	t.Cleanup(func() { tlsKeyOwner = restore })

	r := refusalOf(t, CheckTLSMaterial(cfg), CodeSecretError)
	for _, frag := range []string{
		EnvTLSKeyFile,
		cfg.TLSKeyFile,
		fmt.Sprintf("owned by uid %d", mine),
		fmt.Sprintf("uid %d in this container", other),
		"chown",
	} {
		if !strings.Contains(r.Msg, frag) {
			t.Errorf("message %q does not contain %q", r.Msg, frag)
		}
	}
	// The certificate and the CA are public: their ownership is samba's
	// business and it does not object, so this must not refuse over them.
	if strings.Contains(r.Msg, cfg.TLSCertFile) || strings.Contains(r.Msg, cfg.TLSCAFile) {
		t.Errorf("the ownership refusal names public material: %q", r.Msg)
	}
}

// TestTLSOptionKeysAreTheOwnedOnes ties the list the reconciler removes to
// the list SAMBA_GLOBAL_OPTIONS refuses. They are the same three keys for the
// same reason — the image owns them — and a future edit that added a fourth
// to one list only would leave a setting nothing takes back out.
func TestTLSOptionKeysAreTheOwnedOnes(t *testing.T) {
	for _, key := range TLSOptionKeys() {
		owner, owned := globalOptionOwners[key]
		if !owned {
			t.Errorf("%q is removed by the reconciler but SAMBA_GLOBAL_OPTIONS does not refuse it", key)
			continue
		}
		if !strings.HasPrefix(owner, "SAMBA_TLS_") {
			t.Errorf("%q is owned by %q, not by one of the SAMBA_TLS_* variables", key, owner)
		}
	}
	if got := len(TLSOptionKeys()); got != 3 {
		t.Errorf("TLSOptionKeys() has %d entries, want the 3 the trio sets", got)
	}
}

// TestCheckTLSMaterialRefusesAFifoWithoutBlocking is why the check stats
// before it opens.
//
// A FIFO at one of the three paths is not a far-fetched mount mistake, and
// opening one for reading BLOCKS until somebody writes to it. A check that
// opened first would hang the boot forever, with no log line and no exit
// code — the worst failure this package could have. So the assertion is not
// only that it refuses, but that it answers at all.
func TestCheckTLSMaterialRefusesAFifoWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "cert.pem")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("this platform cannot create a FIFO: %v", err)
	}
	key := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(key, []byte("-----BEGIN-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, []byte("-----BEGIN-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- CheckTLSMaterial(&Config{TLSCertFile: fifo, TLSKeyFile: key, TLSCAFile: ca})
	}()
	select {
	case err := <-done:
		r := refusalOf(t, err, CodeSecretError)
		for _, frag := range []string{EnvTLSCertFile, fifo} {
			if !strings.Contains(r.Msg, frag) {
				t.Errorf("message %q does not contain %q", r.Msg, frag)
			}
		}
	case <-time.After(5 * time.Second):
		// Deliberately Fatal rather than a hang: `go test` would otherwise
		// report a binary timeout minutes later, pointing at nothing.
		t.Fatal("CheckTLSMaterial blocked on a FIFO: it is opening the file before it stats it")
	}
}
