// Package devicename persists the owner's name for this Art Computer.
//
// The name is a label, never an identity. The serial (the hostname) stays the
// device's identity on the network and the key every other system uses; this
// record only changes what a controller displays. Keeping the two apart is why
// a rename cannot strand a paired app, a support ledger row, or a registry
// entry.
package devicename

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// MaxLength is the longest name accepted, in runes.
//
// The name lands in single-line slots beside other chrome on every controller
// that displays it, and it is also the mDNS service-instance label, which has
// its own practical ceiling. 32 leaves room for "Living Room" or "Sam's
// Studio" without either constraint biting.
const MaxLength = 32

// Record is the persisted device-name state.
//
// A struct rather than a bare string file: the name is the first thing an
// owner sets about a unit and it is unlikely to be the last, so the record
// starts in a shape that can carry a second field without a migration.
type Record struct {
	Name string `json:"name"`
}

// controlCharacters matches C0/C1 controls and DEL. These become a space:
// dropping them outright would join words that a newline or tab separated.
var controlCharacters = regexp.MustCompile(`[\x00-\x1f\x7f-\x9f]`)

// formattingCharacters matches characters that are invisible but change how
// the text around them renders: zero-width spaces and joiners, the bidi marks
// (LRM/RLM/ALM), bidi overrides and isolates, the line and paragraph
// separators, and the BOM. These are removed rather than spaced, because they
// occupy no width — turning one into a space would alter a name the owner can
// see.
//
// The plain marks matter as much as the overrides: U+200E, U+200F and U+061C
// set direction for the text that follows without any of the paired-scope
// machinery, so leaving them in would let a caller reorder a label with a
// single invisible character while the override class was carefully blocked.
//
// They are stripped rather than rejected. The name arrives over a command
// surface the hub serves unauthenticated on the LAN today, so a device on the
// network can set it; a name able to carry bidi overrides would let one unit's
// mDNS record display as another's.
//
// This split must stay identical to the app's `stripUnsafeDisplayCharacters`.
// The app sanitizes to drive its character counter and preview; if the two
// disagree, the owner sees one name in the field and the device stores
// another.
var formattingCharacters = regexp.MustCompile(
	`[\x{061c}\x{200b}-\x{200f}\x{202a}-\x{202e}\x{2066}-\x{2069}\x{2028}\x{2029}\x{feff}]`,
)

var repeatedWhitespace = regexp.MustCompile(`\s+`)

// Sanitize normalizes a caller-supplied name for storage.
//
// Truncation is hard rather than elliptical: this string is read back into the
// field the owner edits next, so a decorative ellipsis would become part of
// the name on the following save. An empty result is a legitimate value
// meaning "no name" — callers fall back to the serial — so this returns a
// cleaned string rather than an error for input that cleans away to nothing.
func Sanitize(raw string) string {
	cleaned := controlCharacters.ReplaceAllString(raw, " ")
	cleaned = formattingCharacters.ReplaceAllString(cleaned, "")
	cleaned = strings.TrimSpace(repeatedWhitespace.ReplaceAllString(cleaned, " "))

	runes := []rune(cleaned)
	if len(runes) <= MaxLength {
		return cleaned
	}
	return strings.TrimRight(string(runes[:MaxLength]), " ")
}

// Load reads the stored name. A missing or empty file is not an error: it is
// the ordinary state of a unit nobody has named, and it yields an empty record
// so callers take their serial fallback.
func Load(os wrapper.OS, json wrapper.JSON) (*Record, error) {
	data, err := os.ReadFile(constants.DEVICE_NAME_FILE)
	if err != nil {
		if os.IsNotExist(err) {
			return &Record{}, nil
		}
		return nil, fmt.Errorf("read device name: %w", err)
	}
	if len(data) == 0 {
		return &Record{}, nil
	}

	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("parse device name: %w", err)
	}
	record.Name = Sanitize(record.Name)
	return &record, nil
}

// Save writes the name, sanitizing first so nothing reaches disk that Load
// would have to clean on the way back out.
//
// Written to a temp file and renamed: the mDNS advertiser reads this file when
// it re-registers, and a torn write would either fail to parse or advertise a
// half-written name.
func Save(os wrapper.OS, json wrapper.JSON, record *Record) error {
	if record == nil {
		record = &Record{}
	}
	record.Name = Sanitize(record.Name)

	stateDir := filepath.Dir(constants.DEVICE_NAME_FILE)
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return fmt.Errorf("create device name dir: %w", err)
	}

	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal device name: %w", err)
	}

	tmpPath := constants.DEVICE_NAME_FILE + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write device name tmp: %w", err)
	}
	if err := os.Rename(tmpPath, constants.DEVICE_NAME_FILE); err != nil {
		return fmt.Errorf("rename device name tmp: %w", err)
	}
	return nil
}

// Clear removes the stored name. Used by factory reset: the unit is being
// handed on, and arriving in its new home still called "Sam's Studio" would
// leak the previous owner's vocabulary.
func Clear(os wrapper.OS) error {
	if err := os.Remove(constants.DEVICE_NAME_FILE); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear device name: %w", err)
	}
	return nil
}
