package sleepschedule

import (
	"fmt"
	stdsys "os"
	"path/filepath"
	"strings"
	"time"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

type State string

const (
	StateAwake    State = "awake"
	StateSleeping State = "sleeping"

	// DefaultSleepTime and DefaultWakeTime pre-fill the record when
	// sleep-schedule.json is missing or empty; EffectiveStatus ignores the
	// window until enabled is true. HH:MM is anchored to the timezone
	// returned by LoadSystemTimezone, not the Go process-level time.Local.
	DefaultSleepTime = "22:00"
	DefaultWakeTime  = "07:30"
)

type Record struct {
	Enabled       bool       `json:"enabled"`
	SleepTime     string     `json:"sleepTime,omitempty"`
	WakeTime      string     `json:"wakeTime,omitempty"`
	OverrideState *State     `json:"overrideState,omitempty"`
	OverrideUntil *time.Time `json:"overrideUntil,omitempty"`
}

type Status struct {
	Enabled          bool       `json:"enabled"`
	SleepTime        string     `json:"sleepTime,omitempty"`
	WakeTime         string     `json:"wakeTime,omitempty"`
	CurrentState     State      `json:"currentState"`
	OverrideState    *State     `json:"overrideState,omitempty"`
	OverrideUntil    *time.Time `json:"overrideUntil,omitempty"`
	NextTransitionAt *time.Time `json:"nextTransitionAt,omitempty"`
}

type ClockTime struct {
	Hour   int
	Minute int
}

func ParseClockTime(raw string) (ClockTime, error) {
	trimmed := strings.TrimSpace(raw)
	parts := strings.Split(trimmed, ":")
	if len(parts) != 2 {
		return ClockTime{}, fmt.Errorf("invalid time %q: want HH:MM", raw)
	}

	var hour, minute int
	if _, err := fmt.Sscanf(trimmed, "%d:%d", &hour, &minute); err != nil {
		return ClockTime{}, fmt.Errorf("invalid time %q: %w", raw, err)
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return ClockTime{}, fmt.Errorf("invalid time %q: hour must be 0-23 and minute 0-59", raw)
	}

	return ClockTime{Hour: hour, Minute: minute}, nil
}

func (c ClockTime) Format() string {
	return fmt.Sprintf("%02d:%02d", c.Hour, c.Minute)
}

// LoadSystemTimezone returns the device's current local timezone, bypassing
// Go's process-level time.Local cache. time.Local is read once at process
// start; if feral-controld starts before the timezone is configured, it stays
// UTC for the lifetime of that process even after /etc/localtime is updated.
//
// Resolution order:
//  1. /etc/timezone — plain IANA name, present on Debian/Ubuntu.
//  2. /etc/localtime symlink — present on all Linux distros; target path
//     encodes the IANA name as /usr/share/zoneinfo/<name>.
//  3. time.Local — last resort if neither file is readable.
func LoadSystemTimezone(os wrapper.OS) *time.Location {
	// /etc/timezone (Debian/Ubuntu style — plain text IANA name)
	if data, err := os.ReadFile("/etc/timezone"); err == nil {
		if name := strings.TrimSpace(string(data)); name != "" {
			if loc, err := time.LoadLocation(name); err == nil {
				return loc
			}
		}
	}

	// /etc/localtime is a symlink → /usr/share/zoneinfo/<name>.
	// Reading the symlink target is always fresh: it reflects whatever
	// timedatectl / setup wrote, regardless of when this process started.
	if target, err := stdsys.Readlink("/etc/localtime"); err == nil {
		const zonePrefix = "/usr/share/zoneinfo/"
		if strings.HasPrefix(target, zonePrefix) {
			if loc, err := time.LoadLocation(target[len(zonePrefix):]); err == nil {
				return loc
			}
		}
	}

	return time.Local
}

// OnDay anchors the clock time to the calendar date of t, interpreted in loc.
// loc is separate from t so a UTC clock can still express "07:00 in the
// device's local timezone" — both concerns stay explicit at the call site.
func (c ClockTime) OnDay(t time.Time, loc *time.Location) time.Time {
	tLoc := t.In(loc)
	return time.Date(tLoc.Year(), tLoc.Month(), tLoc.Day(), c.Hour, c.Minute, 0, 0, loc)
}

func DefaultRecord() *Record {
	return &Record{
		Enabled:   false,
		SleepTime: DefaultSleepTime,
		WakeTime:  DefaultWakeTime,
	}
}

func Validate(record *Record) error {
	if record == nil {
		return fmt.Errorf("sleep schedule is required")
	}
	if !record.Enabled {
		return nil
	}

	sleepTime, err := ParseClockTime(record.SleepTime)
	if err != nil {
		return err
	}
	wakeTime, err := ParseClockTime(record.WakeTime)
	if err != nil {
		return err
	}
	if sleepTime == wakeTime {
		return fmt.Errorf("sleepTime and wakeTime must be different")
	}
	return nil
}

func Load(os wrapper.OS, json wrapper.JSON) (*Record, error) {
	data, err := os.ReadFile(constants.SLEEP_SCHEDULE_FILE)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultRecord(), nil
		}
		return nil, fmt.Errorf("read sleep schedule: %w", err)
	}
	if len(data) == 0 {
		return DefaultRecord(), nil
	}

	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("parse sleep schedule: %w", err)
	}
	return &record, nil
}

func Save(os wrapper.OS, json wrapper.JSON, record *Record) error {
	if record == nil {
		record = DefaultRecord()
	}
	if err := Validate(record); err != nil {
		return err
	}

	stateDir := filepath.Dir(constants.SLEEP_SCHEDULE_FILE)
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return fmt.Errorf("create sleep schedule dir: %w", err)
	}

	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal sleep schedule: %w", err)
	}

	tmpPath := constants.SLEEP_SCHEDULE_FILE + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write sleep schedule tmp: %w", err)
	}
	if err := os.Rename(tmpPath, constants.SLEEP_SCHEDULE_FILE); err != nil {
		return fmt.Errorf("rename sleep schedule tmp: %w", err)
	}
	return nil
}

// EffectiveStatus derives the current sleep state from record at now.
// loc must be the device's local timezone (from LoadSystemTimezone); it is
// used to anchor HH:MM wall-clock times to concrete instants. Keeping loc
// separate from now allows a UTC clock to coexist with a local schedule.
func EffectiveStatus(now time.Time, record *Record, loc *time.Location) (*Status, bool) {
	record, changed := Normalize(record, now)
	if record == nil {
		record = DefaultRecord()
	}

	status := &Status{
		Enabled:       record.Enabled,
		SleepTime:     record.SleepTime,
		WakeTime:      record.WakeTime,
		OverrideState: record.OverrideState,
		OverrideUntil: record.OverrideUntil,
		CurrentState:  StateAwake,
	}

	if record.OverrideState != nil {
		status.CurrentState = *record.OverrideState
		status.NextTransitionAt = record.OverrideUntil
		return status, changed
	}

	if !record.Enabled || record.SleepTime == "" || record.WakeTime == "" {
		return status, changed
	}

	sleepTime, err := ParseClockTime(record.SleepTime)
	if err != nil {
		return status, changed
	}
	wakeTime, err := ParseClockTime(record.WakeTime)
	if err != nil {
		return status, changed
	}

	if isSleepingAt(now, sleepTime, wakeTime, loc) {
		status.CurrentState = StateSleeping
		status.NextTransitionAt = timePtr(nextOccurrence(now, wakeTime, loc))
		return status, changed
	}

	status.NextTransitionAt = timePtr(nextOccurrence(now, sleepTime, loc))
	return status, changed
}

func Normalize(record *Record, now time.Time) (*Record, bool) {
	if record == nil {
		return DefaultRecord(), false
	}
	if record.OverrideUntil == nil || record.OverrideUntil.After(now) {
		return record, false
	}

	normalized := *record
	normalized.OverrideState = nil
	normalized.OverrideUntil = nil
	return &normalized, true
}

func ManualSleep(record *Record, now time.Time, loc *time.Location) (*Record, error) {
	return applyOverride(record, now, StateSleeping, loc)
}

func ManualWake(record *Record, now time.Time, loc *time.Location) (*Record, error) {
	return applyOverride(record, now, StateAwake, loc)
}

func applyOverride(record *Record, now time.Time, state State, loc *time.Location) (*Record, error) {
	record, _ = Normalize(record, now)
	if record == nil {
		record = DefaultRecord()
	}

	updated := *record
	updated.OverrideState = state.Ptr()
	updated.OverrideUntil = nil

	if record.Enabled {
		sleepTime, err := ParseClockTime(record.SleepTime)
		if err != nil {
			return nil, err
		}
		wakeTime, err := ParseClockTime(record.WakeTime)
		if err != nil {
			return nil, err
		}

		switch state {
		case StateSleeping:
			updated.OverrideUntil = timePtr(nextOccurrence(now, wakeTime, loc))
		case StateAwake:
			updated.OverrideUntil = timePtr(nextOccurrence(now, sleepTime, loc))
		default:
			return nil, fmt.Errorf("unknown override state %q", state)
		}
	}

	return &updated, nil
}

func nextOccurrence(now time.Time, clockTime ClockTime, loc *time.Location) time.Time {
	candidate := clockTime.OnDay(now, loc)
	if !candidate.After(now) {
		return candidate.Add(24 * time.Hour)
	}
	return candidate
}

func isSleepingAt(now time.Time, sleepTime, wakeTime ClockTime, loc *time.Location) bool {
	sleepToday := sleepTime.OnDay(now, loc)
	wakeToday := wakeTime.OnDay(now, loc)

	if sleepToday.Before(wakeToday) {
		return !now.Before(sleepToday) && now.Before(wakeToday)
	}

	return !now.Before(sleepToday) || now.Before(wakeToday)
}

func timePtr(t time.Time) *time.Time {
	return &t
}

func (s State) Ptr() *State {
	return &s
}
