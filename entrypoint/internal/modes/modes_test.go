package modes

import (
	"reflect"
	"strings"
	"testing"

	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/config"
	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/state"
)

// imageVer is the version this image pretends to ship in the matrix below.
const imageVer = "4.24.6"

// cfgFor builds a config the way Load would leave it for mode, then applies
// the case-specific mutations. Decide is pure, so tests never touch the
// environment.
func cfgFor(mode config.Mode, mut ...func(*config.Config)) *config.Config {
	cfg := &config.Config{
		Mode:          mode,
		Realm:         "AD.EXAMPLE.COM",
		Domain:        "AD",
		JoinUsername:  "Administrator",
		DNSBackend:    config.DNSBackendInternal,
		FunctionLevel: "2016",
		LogLevel:      1,
		Chrony:        true,
		MaintenanceOp: config.MaintenanceCheck,
	}
	switch mode {
	case config.ModeProvision:
		cfg.AdminPasswordFile = "/secrets/adminpass"
	case config.ModeJoin:
		cfg.JoinPasswordFile = "/secrets/joinpass"
	}
	for _, m := range mut {
		m(cfg)
	}
	return cfg
}

// absent is a volume with no samba state at all.
func absent() state.Observation { return state.Observation{} }

// presentWith is an initialized volume whose marker records version v.
func presentWith(v string) state.Observation {
	return state.Observation{
		Present: true,
		Marker: &state.Marker{
			SambaVersion:  v,
			InitializedAt: "2026-08-16T10:00:00Z",
			LastMode:      "provision",
		},
	}
}

// presentNoMarker is a foreign / pre-existing volume: state, no marker.
func presentNoMarker() state.Observation { return state.Observation{Present: true} }

type cell struct {
	name         string
	cfg          *config.Config
	obs          state.Observation
	imageVersion string

	// Exactly one of the two expectations is used: wantCode == 0 means the
	// cell must produce wantPlan, otherwise it must produce that refusal.
	wantPlan     Plan
	wantCode     int
	wantContains []string // remedy keywords the refusal message must carry
}

func matrix() []cell {
	adminFile := func(p string) func(*config.Config) {
		return func(c *config.Config) { c.AdminPasswordFile = p }
	}
	joinFile := func(p string) func(*config.Config) {
		return func(c *config.Config) { c.JoinPasswordFile = p }
	}
	realm := func(r string) func(*config.Config) {
		return func(c *config.Config) { c.Realm = r }
	}

	return []cell{
		// ---- auto ------------------------------------------------------
		{
			name:     "auto/absent/no join creds -> provision",
			cfg:      cfgFor(config.ModeAuto, adminFile("/secrets/adminpass")),
			obs:      absent(),
			wantPlan: Plan{Kind: ActProvision},
		},
		{
			name:     "auto/absent/join creds -> join",
			cfg:      cfgFor(config.ModeAuto, joinFile("/secrets/joinpass")),
			obs:      absent(),
			wantPlan: Plan{Kind: ActJoin},
		},
		{
			// Both secrets mounted: joining an existing domain is the
			// non-destructive reading, so join wins.
			name:     "auto/absent/both admin and join creds -> join",
			cfg:      cfgFor(config.ModeAuto, adminFile("/secrets/adminpass"), joinFile("/secrets/joinpass")),
			obs:      absent(),
			wantPlan: Plan{Kind: ActJoin},
		},
		{
			name:     "auto/present/marker == image -> start",
			cfg:      cfgFor(config.ModeAuto),
			obs:      presentWith(imageVer),
			wantPlan: Plan{Kind: ActStart},
		},
		{
			name:     "auto/present/marker < image -> dbcheck then start",
			cfg:      cfgFor(config.ModeAuto),
			obs:      presentWith("4.24.5"),
			wantPlan: Plan{Kind: ActDBCheckThenStart},
		},
		{
			name:         "auto/present/marker > image -> refusal 22",
			cfg:          cfgFor(config.ModeAuto),
			obs:          presentWith("4.25.0"),
			wantCode:     config.CodeDowngrade,
			wantContains: []string{"4.25.0", imageVer, "restore"},
		},
		{
			name:     "auto/present/no marker -> dbcheck then start with adoption",
			cfg:      cfgFor(config.ModeAuto),
			obs:      presentNoMarker(),
			wantPlan: Plan{Kind: ActDBCheckThenStart, AdoptMarker: true},
		},
		{
			name:         "auto/present/malformed marker version -> refusal 10",
			cfg:          cfgFor(config.ModeAuto),
			obs:          presentWith("4.24"),
			wantCode:     config.CodeConfigError,
			wantContains: []string{state.MarkerName, "delete"},
		},
		{
			name:         "auto resolving to provision without realm -> refusal 10",
			cfg:          cfgFor(config.ModeAuto, adminFile("/secrets/adminpass"), realm("")),
			obs:          absent(),
			wantCode:     config.CodeConfigError,
			wantContains: []string{"SAMBA_REALM", "provision"},
		},
		{
			name:         "auto resolving to provision without admin password file -> refusal 10",
			cfg:          cfgFor(config.ModeAuto),
			obs:          absent(),
			wantCode:     config.CodeConfigError,
			wantContains: []string{"SAMBA_ADMIN_PASSWORD_FILE", "mount"},
		},
		{
			name:         "auto resolving to provision with undotted realm -> refusal 10",
			cfg:          cfgFor(config.ModeAuto, adminFile("/secrets/adminpass"), realm("EXAMPLE")),
			obs:          absent(),
			wantCode:     config.CodeConfigError,
			wantContains: []string{"SAMBA_REALM", "AD.EXAMPLE.COM"},
		},
		{
			name:         "auto resolving to join without realm -> refusal 10",
			cfg:          cfgFor(config.ModeAuto, joinFile("/secrets/joinpass"), realm("")),
			obs:          absent(),
			wantCode:     config.CodeConfigError,
			wantContains: []string{"SAMBA_REALM", "join"},
		},
		{
			name:         "auto resolving to join with undotted realm -> refusal 10",
			cfg:          cfgFor(config.ModeAuto, joinFile("/secrets/joinpass"), realm("EXAMPLE")),
			obs:          absent(),
			wantCode:     config.CodeConfigError,
			wantContains: []string{"SAMBA_REALM", "AD.EXAMPLE.COM"},
		},

		// ---- provision -------------------------------------------------
		{
			name:     "provision/absent -> provision",
			cfg:      cfgFor(config.ModeProvision),
			obs:      absent(),
			wantPlan: Plan{Kind: ActProvision},
		},
		{
			name:         "provision/present -> refusal 20",
			cfg:          cfgFor(config.ModeProvision),
			obs:          presentWith(imageVer),
			wantCode:     config.CodeStateExists,
			wantContains: []string{"SAMBA_MODE=run", "delete"},
		},
		{
			// An explicit mode is never overridden by the presence of the
			// other mode's credentials.
			name:     "provision/absent with join creds also present -> provision",
			cfg:      cfgFor(config.ModeProvision, joinFile("/secrets/joinpass")),
			obs:      absent(),
			wantPlan: Plan{Kind: ActProvision},
		},
		{
			name:         "provision/absent without admin password file -> refusal 10",
			cfg:          cfgFor(config.ModeProvision, adminFile("")),
			obs:          absent(),
			wantCode:     config.CodeConfigError,
			wantContains: []string{"SAMBA_ADMIN_PASSWORD_FILE", "mount"},
		},

		// ---- join ------------------------------------------------------
		{
			name:     "join/absent -> join",
			cfg:      cfgFor(config.ModeJoin),
			obs:      absent(),
			wantPlan: Plan{Kind: ActJoin},
		},
		{
			name:         "join/present -> refusal 20",
			cfg:          cfgFor(config.ModeJoin),
			obs:          presentWith(imageVer),
			wantCode:     config.CodeStateExists,
			wantContains: []string{"SAMBA_MODE=run", "delete"},
		},
		{
			name:         "join/absent without join password file -> refusal 10",
			cfg:          cfgFor(config.ModeJoin, joinFile("")),
			obs:          absent(),
			wantCode:     config.CodeConfigError,
			wantContains: []string{"SAMBA_JOIN_PASSWORD_FILE", "mount"},
		},

		// ---- run -------------------------------------------------------
		{
			name:         "run/absent -> refusal 21",
			cfg:          cfgFor(config.ModeRun),
			obs:          absent(),
			wantCode:     config.CodeStateAbsent,
			wantContains: []string{"/var/lib/samba", "mount", "provision"},
		},
		{
			name:     "run/present/marker == image -> start",
			cfg:      cfgFor(config.ModeRun),
			obs:      presentWith(imageVer),
			wantPlan: Plan{Kind: ActStart},
		},
		{
			name:     "run/present/marker < image -> dbcheck then start",
			cfg:      cfgFor(config.ModeRun),
			obs:      presentWith("4.23.9"),
			wantPlan: Plan{Kind: ActDBCheckThenStart},
		},
		{
			name: "run/present/marker < image numerically not lexicographically",
			cfg:  cfgFor(config.ModeRun),
			obs:  presentWith("4.24.6"),
			// 4.24.6 < 4.24.10: a lexicographic compare would refuse here.
			imageVersion: "4.24.10",
			wantPlan:     Plan{Kind: ActDBCheckThenStart},
		},
		{
			name:         "run/present/marker > image -> refusal 22",
			cfg:          cfgFor(config.ModeRun),
			obs:          presentWith("4.24.10"),
			wantCode:     config.CodeDowngrade,
			wantContains: []string{"4.24.10", imageVer, "restore"},
		},
		{
			name:     "run/present/no marker -> dbcheck then start with adoption",
			cfg:      cfgFor(config.ModeRun),
			obs:      presentNoMarker(),
			wantPlan: Plan{Kind: ActDBCheckThenStart, AdoptMarker: true},
		},
		{
			name:         "run/present/malformed marker version -> refusal 10",
			cfg:          cfgFor(config.ModeRun),
			obs:          presentWith("four.twenty.four"),
			wantCode:     config.CodeConfigError,
			wantContains: []string{state.MarkerName, "delete"},
		},

		{
			// An empty marker file ({}) parses but records no version.
			name:         "run/present/empty marker version -> refusal 10",
			cfg:          cfgFor(config.ModeRun),
			obs:          presentWith(""),
			wantCode:     config.CodeConfigError,
			wantContains: []string{state.MarkerName, "samba_version", "delete"},
		},

		// ---- maintenance -----------------------------------------------
		{
			name:         "maintenance/absent -> refusal 21",
			cfg:          cfgFor(config.ModeMaintenance),
			obs:          absent(),
			wantCode:     config.CodeStateAbsent,
			wantContains: []string{"/var/lib/samba", "mount"},
		},
		{
			name:     "maintenance/present/check -> maintenance without repair",
			cfg:      cfgFor(config.ModeMaintenance),
			obs:      presentWith(imageVer),
			wantPlan: Plan{Kind: ActMaintenance},
		},
		{
			name: "maintenance/present/repair -> maintenance with repair",
			cfg: cfgFor(config.ModeMaintenance, func(c *config.Config) {
				c.MaintenanceOp = config.MaintenanceRepair
			}),
			obs:      presentWith(imageVer),
			wantPlan: Plan{Kind: ActMaintenance, Repair: true},
		},
		{
			name:     "maintenance/present/marker < image -> maintenance",
			cfg:      cfgFor(config.ModeMaintenance),
			obs:      presentWith("4.23.1"),
			wantPlan: Plan{Kind: ActMaintenance},
		},
		{
			name:     "maintenance/present/no marker -> maintenance without adoption",
			cfg:      cfgFor(config.ModeMaintenance),
			obs:      presentNoMarker(),
			wantPlan: Plan{Kind: ActMaintenance},
		},
		{
			name:         "maintenance/present/marker > image -> refusal 22",
			cfg:          cfgFor(config.ModeMaintenance),
			obs:          presentWith("5.0.0"),
			wantCode:     config.CodeDowngrade,
			wantContains: []string{"5.0.0", imageVer, "restore"},
		},
		{
			// The version guard applies to maintenance too: dbcheck --fix
			// must not run against a database of unknown vintage.
			name:         "maintenance/present/malformed marker version -> refusal 10",
			cfg:          cfgFor(config.ModeMaintenance),
			obs:          presentWith("4.24"),
			wantCode:     config.CodeConfigError,
			wantContains: []string{state.MarkerName, "samba_version", "delete"},
		},
		{
			name:         "maintenance/present/no marker with malformed image version -> refusal 10",
			cfg:          cfgFor(config.ModeMaintenance),
			obs:          presentNoMarker(),
			imageVersion: "not-a-version",
			wantCode:     config.CodeConfigError,
			wantContains: []string{"not-a-version", "image", "bug"},
		},

		// ---- degenerate inputs ------------------------------------------
		{
			name:         "malformed image version -> refusal 10",
			cfg:          cfgFor(config.ModeRun),
			obs:          presentWith("4.24.6"),
			imageVersion: "unknown",
			wantCode:     config.CodeConfigError,
			wantContains: []string{"unknown", "image"},
		},
		{
			name:         "unsupported mode -> refusal 10",
			cfg:          cfgFor(config.Mode("dance")),
			obs:          presentWith(imageVer),
			wantCode:     config.CodeConfigError,
			wantContains: []string{"SAMBA_MODE", "provision"},
		},
	}
}

func TestDecideMatrix(t *testing.T) {
	for _, c := range matrix() {
		t.Run(c.name, func(t *testing.T) {
			img := c.imageVersion
			if img == "" {
				img = imageVer
			}

			plan, ref := Decide(c.cfg, c.obs, img)

			if c.wantCode == 0 {
				if ref != nil {
					t.Fatalf("Decide refused with code %d (%s), want plan %+v", ref.Code, ref.Msg, c.wantPlan)
				}
				if plan != c.wantPlan {
					t.Fatalf("Decide = %+v, want %+v", plan, c.wantPlan)
				}
				return
			}

			if ref == nil {
				t.Fatalf("Decide = %+v, want refusal with code %d", plan, c.wantCode)
			}
			if ref.Code != c.wantCode {
				t.Fatalf("refusal code = %d, want %d (msg: %s)", ref.Code, c.wantCode, ref.Msg)
			}
			// Invariant: a refusal decides nothing. The zero Plan must
			// never name an action — least of all provision.
			if plan.Kind != ActNone {
				t.Errorf("plan kind = %s alongside a refusal, want %s", plan.Kind, ActNone)
			}
			if plan != (Plan{}) {
				t.Errorf("plan = %+v alongside a refusal, want the zero Plan", plan)
			}
			if strings.Contains(ref.Msg, "\n") {
				t.Errorf("refusal message must be a single line, got %q", ref.Msg)
			}
			if ref.Error() != ref.Msg {
				t.Errorf("Error() = %q, want %q", ref.Error(), ref.Msg)
			}
			if !strings.Contains(ref.Msg, ";") {
				t.Errorf("refusal message must state cause and remedy, got %q", ref.Msg)
			}
			for _, want := range c.wantContains {
				if !strings.Contains(ref.Msg, want) {
					t.Errorf("refusal message %q does not mention %q", ref.Msg, want)
				}
			}
		})
	}
}

func TestZeroPlanDecidesNothing(t *testing.T) {
	// The zero value of ActionKind must be "no decision", so a Plan that
	// escaped a refusal path can never be executed as provision.
	if (Plan{}).Kind != ActNone {
		t.Errorf("Plan{}.Kind = %s, want %s", (Plan{}).Kind, ActNone)
	}
	if ActNone == ActProvision {
		t.Errorf("ActNone must not share a value with ActProvision")
	}
}

func TestActionKindString(t *testing.T) {
	// The names appear in logs and in the §6.7 evidence, so they are part
	// of the observable behavior.
	want := map[ActionKind]string{
		ActNone:             "none",
		ActProvision:        "provision",
		ActJoin:             "join",
		ActStart:            "start",
		ActDBCheckThenStart: "dbcheck-then-start",
		ActMaintenance:      "maintenance",
	}
	for kind, name := range want {
		if got := kind.String(); got != name {
			t.Errorf("ActionKind(%d).String() = %q, want %q", int(kind), got, name)
		}
	}
	if got := ActionKind(42).String(); !strings.Contains(got, "42") {
		t.Errorf("unknown ActionKind renders as %q, want it to name the value", got)
	}
}

func TestDecideIsPure(t *testing.T) {
	// Every cell of the matrix, decided twice: same answer both times, and
	// neither the config nor the observation is left modified.
	for _, c := range matrix() {
		t.Run(c.name, func(t *testing.T) {
			img := c.imageVersion
			if img == "" {
				img = imageVer
			}
			before := *c.cfg
			var markerBefore state.Marker
			if c.obs.Marker != nil {
				markerBefore = *c.obs.Marker
			}

			first, ref1 := Decide(c.cfg, c.obs, img)
			second, ref2 := Decide(c.cfg, c.obs, img)

			if first != second {
				t.Errorf("Decide is not deterministic: %+v then %+v", first, second)
			}
			switch {
			case (ref1 == nil) != (ref2 == nil):
				t.Errorf("Decide refused inconsistently: %v then %v", ref1, ref2)
			case ref1 != nil && *ref1 != *ref2:
				t.Errorf("refusal differs between runs: %+v then %+v", *ref1, *ref2)
			}
			// DeepEqual rather than ==: Config carries the declarative
			// [global] options as a slice, so it is no longer a
			// comparable type. What is asserted is unchanged — Decide
			// must leave the whole value it was handed untouched.
			if !reflect.DeepEqual(*c.cfg, before) {
				t.Errorf("Decide mutated the config: %+v, want %+v", *c.cfg, before)
			}
			if c.obs.Marker != nil && *c.obs.Marker != markerBefore {
				t.Errorf("Decide mutated the marker: %+v, want %+v", *c.obs.Marker, markerBefore)
			}
		})
	}
}

func TestDecideDoesNotMutateObservation(t *testing.T) {
	obs := presentWith("4.23.0")
	marker := *obs.Marker

	if _, ref := Decide(cfgFor(config.ModeRun), obs, imageVer); ref != nil {
		t.Fatalf("unexpected refusal: %v", ref)
	}
	if *obs.Marker != marker {
		t.Errorf("Decide mutated the marker: %+v, want %+v", *obs.Marker, marker)
	}
}
