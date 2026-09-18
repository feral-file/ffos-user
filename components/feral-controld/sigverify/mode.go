package sigverify

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// Mode is the owner-chosen policy for what a non-valid verdict does to a
// cast (feral-file/ffos-user#307). It is per device, set from the mobile app,
// persisted in its own record, and read at cast time. Source kind (URL vs
// inline) never changes the outcome: the owner chose consistency over
// tolerance, so the app's own unsigned casts are treated like any other.
//
//	silent: every cast plays; the verdict is logged and reported only.
//	notify: every cast plays; a non-valid verdict is also shown on the wall.
//	strict: a non-valid (or unverifiable) cast is rejected with sigInvalid.
type Mode string

const (
	ModeSilent Mode = "silent"
	ModeNotify Mode = "notify"
	ModeStrict Mode = "strict"
	// DefaultMode applies to a unit whose owner never chose: honest and
	// visible, but never blocking — a fielded frame that upgrades keeps
	// playing everything it played before, and says so.
	DefaultMode = ModeNotify
)

// ErrInvalidMode is returned by ParseMode for anything outside the vocabulary.
var ErrInvalidMode = errors.New("mode must be one of silent, notify, strict")

// ParseMode validates a wire value. Exact, lowercase match only: the wire
// vocabulary is a contract with the app, not free text. The rejected value
// is deliberately NOT echoed in the error: it is caller-sized (a LAN request
// can carry megabytes in `mode`) and the error travels into logs and the
// hub/relayer reply, which must stay bounded.
func ParseMode(raw string) (Mode, error) {
	switch Mode(raw) {
	case ModeSilent, ModeNotify, ModeStrict:
		return Mode(raw), nil
	default:
		return "", ErrInvalidMode
	}
}

// Allows reports whether a document with verdict v may reach the screen
// under m. Only strict ever refuses, and it refuses everything not proven
// valid — including a nil verdict, which means the document was never
// verified (the offline cached copy, or a scheduler cache that outlived its
// verdict): a strict device does not guess. This is THE policy predicate;
// commandrouter's cast path, the refresher's re-push and the scheduler's
// cutover gate all decide through it so they cannot drift.
func (m Mode) Allows(v *Verdict) bool {
	if m != ModeStrict {
		return true
	}
	return v != nil && v.Status == StatusValid
}

// modeRecord is the persisted shape. A struct rather than a bare string so a
// second field (a "since" timestamp, a per-source override) needs no
// migration.
type modeRecord struct {
	Mode Mode `json:"mode"`
}

// LoadMode reads the stored mode. A missing record is the ordinary state of
// a unit nobody configured and yields DefaultMode with no error. A record
// that cannot be read or parsed, or that carries a value outside the
// vocabulary (a downgrade to firmware that predates a future mode), ALSO
// yields DefaultMode — the fallback is the safe, non-blocking policy — and
// returns the error so the caller can log it. An EXISTING empty record is
// in the second group, not the first: the only way one arises is a power
// cut inside SaveMode's rename window, i.e. a lost setting, and a strict
// policy silently degrading to notify with no diagnostic is exactly the
// failure the returned error exists to surface.
func LoadMode(os wrapper.OS, json wrapper.JSON) (Mode, error) {
	data, err := os.ReadFile(constants.SIGNATURE_VERIFICATION_FILE)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultMode, nil
		}
		return DefaultMode, fmt.Errorf("read signature verification mode: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return DefaultMode, errors.New("parse signature verification mode: empty record (lost in a write)")
	}
	var record modeRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return DefaultMode, fmt.Errorf("parse signature verification mode: %w", err)
	}
	mode, err := ParseMode(string(record.Mode))
	if err != nil {
		return DefaultMode, fmt.Errorf("stored signature verification mode: %w", err)
	}
	return mode, nil
}

// SaveMode writes the mode, temp file + rename so a concurrent LoadMode (the
// cast path reads on every displayPlaylist) never sees a torn record. Not
// fsynced, for the same reason as the device name: a power cut in the rename
// window can leave an empty file, which loads as DefaultMode plus an error
// the cast path logs — the lost setting is re-selectable from the app and
// the failure direction is non-blocking, never a wedged device. Callers
// that can race (a setter against factory reset's ClearMode, two setters
// through the one .tmp path) serialize on devicectl's verificationModeMu;
// this function is not itself concurrency-safe.
func SaveMode(os wrapper.OS, json wrapper.JSON, mode Mode) error {
	if _, err := ParseMode(string(mode)); err != nil {
		return err
	}
	stateDir := filepath.Dir(constants.SIGNATURE_VERIFICATION_FILE)
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return fmt.Errorf("create signature verification dir: %w", err)
	}
	data, err := json.Marshal(modeRecord{Mode: mode})
	if err != nil {
		return fmt.Errorf("marshal signature verification mode: %w", err)
	}
	tmpPath := constants.SIGNATURE_VERIFICATION_FILE + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write signature verification mode tmp: %w", err)
	}
	if err := os.Rename(tmpPath, constants.SIGNATURE_VERIFICATION_FILE); err != nil {
		return fmt.Errorf("rename signature verification mode tmp: %w", err)
	}
	return nil
}

// ClearMode removes the stored mode (and a stranded .tmp) so a unit being
// handed on returns to DefaultMode: a previous owner's `strict` must not
// silently block the next owner's casts. Live record first, both paths
// attempted, mirroring devicename.Clear's rationale.
func ClearMode(os wrapper.OS) error {
	var errs []error
	if err := os.Remove(constants.SIGNATURE_VERIFICATION_FILE); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("clear signature verification mode: %w", err))
	}
	if err := os.Remove(constants.SIGNATURE_VERIFICATION_FILE + ".tmp"); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("clear signature verification mode tmp: %w", err))
	}
	return errors.Join(errs...)
}
