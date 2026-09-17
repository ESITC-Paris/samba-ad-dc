package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
