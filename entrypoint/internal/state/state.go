// Package state inspects the samba state volume and maintains the image
// version marker written next to it.
//
// Two facts describe a volume: whether samba state is present (the sam.ldb
// database exists) and what the marker says about the version that wrote it.
// The decisions taken from those facts live in the modes package; this
// package only observes and records.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/esitc-paris/samba-ad-dc/entrypoint/internal/config"
)

const (
	// MarkerName is the marker file written at the root of the state dir.
	MarkerName = ".image-state.json"
	// stateFile is the samba database whose existence defines "state present".
	stateFile = "private/sam.ldb"
	// tempPrefix names the temp files used for atomic marker writes.
	tempPrefix = ".image-state.json.tmp"
)

// Marker records which image initialized or last validated the volume. Its
// JSON shape is part of the documented behavior contract.
type Marker struct {
	SambaVersion  string `json:"samba_version"`
	InitializedAt string `json:"initialized_at"`
	LastMode      string `json:"last_mode"`
}

// Observation is what one look at the state directory reveals. Marker is nil
// when no marker file exists (a foreign or pre-existing volume).
type Observation struct {
	Present bool
	Marker  *Marker
}

// Observe inspects the state directory (production: /var/lib/samba). A
// missing directory is reported as "no state", not as an error: it simply
// means the volume has not been initialized yet. A marker file that cannot be
// read or parsed is an error, never a silent nil — mistaking a corrupt marker
// for an absent one would let the caller re-adopt a volume it should refuse.
func Observe(dir string) (Observation, error) {
	obs := Observation{}

	if _, err := os.Stat(filepath.Join(dir, stateFile)); err == nil {
		obs.Present = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Observation{}, &config.Refusal{
			Code: config.CodeConfigError,
			Msg: fmt.Sprintf("state directory %q cannot be inspected (%s); check that the volume is mounted and readable by the container user",
				dir, reason(err)),
		}
	}

	markerPath := filepath.Join(dir, MarkerName)
	data, err := os.ReadFile(markerPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return obs, nil
	case err != nil:
		return Observation{}, &config.Refusal{
			Code: config.CodeConfigError,
			Msg: fmt.Sprintf("marker file %q cannot be read (%s); make %s readable by the container user, or delete it to let the container re-adopt the volume",
				markerPath, reason(err), MarkerName),
		}
	}

	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		return Observation{}, &config.Refusal{
			Code: config.CodeConfigError,
			Msg: fmt.Sprintf("marker file %q is not valid JSON (%s); restore a backup of the volume, or delete %s to let the container re-adopt it after a database check",
				markerPath, oneLine(err.Error()), MarkerName),
		}
	}
	obs.Marker = &m
	return obs, nil
}

// WriteMarker writes m into dir atomically: the JSON goes to a temp file in
// the same directory, is flushed, and is then renamed over the marker. A
// reader therefore sees either the previous marker or the new one, never a
// half-written file. The marker is mode 0600 and no temp file survives a
// failure.
func WriteMarker(dir string, m Marker) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return &config.Refusal{
			Code: config.CodeRuntimeFailure,
			Msg:  fmt.Sprintf("marker for %q cannot be encoded (%s); this is a bug in the image, please report it", dir, oneLine(err.Error())),
		}
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, tempPrefix)
	if err != nil {
		return writeRefusal(dir, err)
	}
	tmpName := tmp.Name()
	// Any failure past this point must not leave the temp file behind.
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return writeRefusal(dir, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return writeRefusal(dir, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return writeRefusal(dir, err)
	}
	if err := tmp.Close(); err != nil {
		return writeRefusal(dir, err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, MarkerName)); err != nil {
		return writeRefusal(dir, err)
	}
	return nil
}

// CompareVersions compares two X.Y.Z version strings numerically and returns
// -1, 0 or 1 for a < b, a == b, a > b. Lexicographic comparison would order
// 4.24.10 before 4.24.6, which is exactly the mistake the version guard must
// not make. Anything that is not three non-negative integers is an error.
func CompareVersions(a, b string) (int, error) {
	av, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	bv, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	for i := range av {
		switch {
		case av[i] < bv[i]:
			return -1, nil
		case av[i] > bv[i]:
			return 1, nil
		}
	}
	return 0, nil
}

// parseVersion splits an X.Y.Z string into its three numeric components.
func parseVersion(v string) ([3]int, error) {
	var out [3]int
	trimmed := strings.TrimSpace(v)
	parts := strings.Split(trimmed, ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("version %q is not in X.Y.Z form", v)
	}
	for i, p := range parts {
		if p == "" || strings.TrimLeft(p, "0123456789") != "" {
			return out, fmt.Errorf("version %q has a non-numeric component %q", v, p)
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, fmt.Errorf("version %q has an unparsable component %q", v, p)
		}
		out[i] = n
	}
	return out, nil
}

// writeRefusal renders a marker write failure as an actionable refusal.
func writeRefusal(dir string, err error) error {
	return &config.Refusal{
		Code: config.CodeRuntimeFailure,
		Msg: fmt.Sprintf("marker file %s cannot be written in %q (%s); mount the state volume read-write and make it writable by the container user",
			MarkerName, dir, reason(err)),
	}
}

// reason renders an os error without repeating the path the caller quotes.
func reason(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return oneLine(err.Error())
}

// oneLine collapses an error string so refusal messages stay single-line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
