// Package modes is the decision engine of the entrypoint: given the
// configuration and one observation of the state volume, it returns the plan
// to execute or the refusal that explains why the container will not start.
//
// Decide is pure. It reads no environment, touches no filesystem and starts
// no process, so the whole transition/refusal matrix of SPEC §6.2 is testable
// as a table. Everything that has an effect lives in the run package.
package modes

import (
	"fmt"
	"strings"

	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/config"
	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/state"
)

// ActionKind is what the entrypoint must do next.
type ActionKind int

// The possible actions. Provision and join initialize the volume and then
// start the daemons; start only starts them; dbcheck-then-start covers both
// the upgrade and the adoption path; maintenance never starts a daemon.
const (
	ActProvision        ActionKind = iota // then start daemons
	ActJoin                               // then start daemons
	ActStart                              // start daemons only
	ActDBCheckThenStart                   // upgrade or adoption path
	ActMaintenance                        // dbcheck/repair, no daemons
)

// String renders an ActionKind for logs and test failures.
func (k ActionKind) String() string {
	switch k {
	case ActProvision:
		return "provision"
	case ActJoin:
		return "join"
	case ActStart:
		return "start"
	case ActDBCheckThenStart:
		return "dbcheck-then-start"
	case ActMaintenance:
		return "maintenance"
	}
	return fmt.Sprintf("ActionKind(%d)", int(k))
}

// Plan is the decision taken for one run. AdoptMarker asks the executor to
// claim a volume that carries no marker (once its database checks out);
// Repair selects dbcheck --fix for maintenance.
type Plan struct {
	Kind        ActionKind
	AdoptMarker bool
	Repair      bool
}

// stateDir is the volume path named in operator-facing messages. The real
// path is passed to the state package by main; naming it here keeps the
// remedy concrete without making Decide depend on it.
const stateDir = "/var/lib/samba"

// Decide maps (configuration, observation, image version) to the plan the
// entrypoint must execute, or to the refusal that stops it. imageVersion is
// the Samba version this image ships, e.g. "4.24.6".
func Decide(cfg *config.Config, obs state.Observation, imageVersion string) (Plan, *config.Refusal) {
	switch cfg.Mode {
	case config.ModeAuto:
		// auto is the only mode that picks its behavior from the volume:
		// an initialized volume is started, an empty one is initialized —
		// joined when join credentials are mounted, provisioned otherwise.
		if obs.Present {
			return startPlan(obs, imageVersion)
		}
		if cfg.JoinPasswordFile != "" {
			return initPlan(cfg, config.ModeJoin)
		}
		return initPlan(cfg, config.ModeProvision)

	case config.ModeProvision, config.ModeJoin:
		if obs.Present {
			return Plan{}, config.Refuse(config.CodeStateExists,
				"SAMBA_MODE=%s would initialize a new domain but the volume already holds samba state (%s/private/sam.ldb exists); set SAMBA_MODE=run to keep and start the existing domain, or delete the state volume first if the existing domain is really meant to be discarded",
				cfg.Mode, stateDir)
		}
		return initPlan(cfg, cfg.Mode)

	case config.ModeRun:
		if !obs.Present {
			return Plan{}, absentRefusal(cfg.Mode)
		}
		return startPlan(obs, imageVersion)

	case config.ModeMaintenance:
		if !obs.Present {
			return Plan{}, absentRefusal(cfg.Mode)
		}
		// Maintenance does not start daemons, but dbcheck --fix from an
		// older Samba against a newer database is exactly the damage the
		// downgrade guard exists to prevent, so the version guard applies
		// here too. An older or absent marker is fine: dbcheck is the
		// operation being asked for, and adoption is left to a start mode.
		if ref := guardNotNewer(obs, imageVersion); ref != nil {
			return Plan{}, ref
		}
		return Plan{Kind: ActMaintenance, Repair: cfg.MaintenanceOp == config.MaintenanceRepair}, nil
	}

	return Plan{}, config.Refuse(config.CodeConfigError,
		"SAMBA_MODE=%q is not a supported mode; set SAMBA_MODE to one of auto, provision, join, run, maintenance",
		string(cfg.Mode))
}

// initPlan validates the configuration an initialization mode needs and
// returns the matching plan. Load validates explicit provision and join
// modes, but auto reaches them only after the volume has been observed, so
// the same checks must exist here — with the same messages, so an operator
// sees one contract whichever mode named the missing variable.
func initPlan(cfg *config.Config, mode config.Mode) (Plan, *config.Refusal) {
	if cfg.Realm == "" {
		return Plan{}, config.Refuse(config.CodeConfigError,
			"SAMBA_REALM is required in %s mode but is not set; set SAMBA_REALM to the Kerberos realm, for example AD.EXAMPLE.COM",
			mode)
	}
	if !strings.Contains(cfg.Realm, ".") {
		return Plan{}, config.Refuse(config.CodeConfigError,
			"SAMBA_REALM=%q is not a dotted DNS domain; set SAMBA_REALM to a fully qualified realm such as AD.EXAMPLE.COM",
			cfg.Realm)
	}

	if mode == config.ModeJoin {
		if cfg.JoinPasswordFile == "" {
			return Plan{}, config.Refuse(config.CodeConfigError,
				"SAMBA_JOIN_PASSWORD_FILE is required in join mode but is not set; mount the join account password as a file and point SAMBA_JOIN_PASSWORD_FILE at it")
		}
		return Plan{Kind: ActJoin}, nil
	}

	if cfg.AdminPasswordFile == "" {
		return Plan{}, config.Refuse(config.CodeConfigError,
			"SAMBA_ADMIN_PASSWORD_FILE is required in provision mode but is not set; mount the initial Administrator password as a file and point SAMBA_ADMIN_PASSWORD_FILE at it")
	}
	return Plan{Kind: ActProvision}, nil
}

// startPlan decides how an initialized volume is brought up: as is when the
// marker matches the image, through dbcheck when the volume is older or was
// never claimed by this image, and not at all when it is newer.
func startPlan(obs state.Observation, imageVersion string) (Plan, *config.Refusal) {
	if obs.Marker == nil {
		// Foreign or pre-existing volume: check the database before
		// trusting it, then claim it by writing the marker.
		if ref := checkImageVersion(imageVersion); ref != nil {
			return Plan{}, ref
		}
		return Plan{Kind: ActDBCheckThenStart, AdoptMarker: true}, nil
	}

	cmp, ref := compare(obs.Marker.SambaVersion, imageVersion)
	if ref != nil {
		return Plan{}, ref
	}
	switch {
	case cmp > 0:
		return Plan{}, downgradeRefusal(obs.Marker.SambaVersion, imageVersion)
	case cmp < 0:
		// Upgrade path: dbcheck first, marker moved forward on success.
		return Plan{Kind: ActDBCheckThenStart}, nil
	default:
		return Plan{Kind: ActStart}, nil
	}
}

// guardNotNewer refuses a volume written by a newer Samba than this image
// provides. A missing marker says nothing about the version, so it passes.
func guardNotNewer(obs state.Observation, imageVersion string) *config.Refusal {
	if obs.Marker == nil {
		return checkImageVersion(imageVersion)
	}
	cmp, ref := compare(obs.Marker.SambaVersion, imageVersion)
	if ref != nil {
		return ref
	}
	if cmp > 0 {
		return downgradeRefusal(obs.Marker.SambaVersion, imageVersion)
	}
	return nil
}

// compare orders the marker version against the image version, turning the
// two possible parse failures into their own refusals: a corrupt marker is an
// operator problem, an unparsable image version is a build bug.
func compare(markerVersion, imageVersion string) (int, *config.Refusal) {
	if ref := checkMarkerVersion(markerVersion); ref != nil {
		return 0, ref
	}
	if ref := checkImageVersion(imageVersion); ref != nil {
		return 0, ref
	}
	cmp, err := state.CompareVersions(markerVersion, imageVersion)
	if err != nil {
		// Unreachable: both operands parsed above. Refuse rather than
		// guess an ordering.
		return 0, config.Refuse(config.CodeConfigError,
			"samba versions %q and %q cannot be compared (%v); this is a bug in the image, please report it",
			markerVersion, imageVersion, err)
	}
	return cmp, nil
}

// checkMarkerVersion refuses a marker whose recorded version is not X.Y.Z.
func checkMarkerVersion(v string) *config.Refusal {
	if _, err := state.CompareVersions(v, v); err != nil {
		return config.Refuse(config.CodeConfigError,
			"marker file %s records samba_version %q, which is not an X.Y.Z version; restore a backup of the volume, or delete %s so the container re-adopts it after a database check",
			state.MarkerName, v, state.MarkerName)
	}
	return nil
}

// checkImageVersion refuses an image whose own version is not X.Y.Z: the
// build injected it, so no operator action can fix it.
func checkImageVersion(v string) *config.Refusal {
	if _, err := state.CompareVersions(v, v); err != nil {
		return config.Refuse(config.CodeConfigError,
			"this image reports its samba version as %q, which is not an X.Y.Z version; this is a bug in the image build, please report it",
			v)
	}
	return nil
}

// downgradeRefusal explains that the volume outranks the image.
func downgradeRefusal(markerVersion, imageVersion string) *config.Refusal {
	return config.Refuse(config.CodeDowngrade,
		"the state volume was written by Samba %s but this image provides Samba %s; deploy an image tag providing Samba %s or newer, or restore a backup of the volume taken on Samba %s",
		markerVersion, imageVersion, markerVersion, imageVersion)
}

// absentRefusal explains that a mode needing state found none.
func absentRefusal(mode config.Mode) *config.Refusal {
	return config.Refuse(config.CodeStateAbsent,
		"SAMBA_MODE=%s needs an initialized domain but the volume holds no samba state (%s/private/sam.ldb is missing); mount the %s volume that holds the domain state, or set SAMBA_MODE=provision or join once to initialize it",
		mode, stateDir, stateDir)
}
