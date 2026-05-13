package sleepschedule

import (
	"fmt"
	"log"
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
	// window until enabled is true. HH:MM uses the caller's time.Location().
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

// LocalTimezone reads the device's current timezone fresh on every call.
// This bypasses Go's process-level time.Local cache, which is set once at
// startup — if feral-controld starts before the timezone is configured,
// time.Local stays UTC for the entire process lifetime.
//
// Resolution order, picking the first that succeeds:
//  1. /etc/localtime as a symlink → named zone (gives "Asia/Taipei" in logs).
//  2. /etc/timezone text file → named zone (Debian/Ubuntu style).
//  3. /etc/localtime read as raw TZif data → unnamed "Local" zone with the
//     correct offsets. Works when /etc/localtime is a regular file copy
//     rather than a symlink, and does not require tzdata on disk.
//  4. time.Local (likely stale UTC) as a last resort.
func LocalTimezone() *time.Location {
	if loc, ok := loadZoneFromLocaltimeSymlink(); ok {
		return loc
	}
	if loc, ok := loadZoneFromEtcTimezone(); ok {
		return loc
	}
	if loc, ok := loadZoneFromLocaltimeData(); ok {
		return loc
	}
	log.Printf("[sleepschedule] LocalTimezone: all resolvers failed — falling back to time.Local (%s)", time.Local)
	return time.Local
}

func loadZoneFromLocaltimeSymlink() (*time.Location, bool) {
	target, err := stdsys.Readlink("/etc/localtime")
	if err != nil {
		return nil, false
	}
	const zoneMarker = "zoneinfo/"
	idx := strings.Index(target, zoneMarker)
	if idx < 0 {
		return nil, false
	}
	loc, err := time.LoadLocation(target[idx+len(zoneMarker):])
	if err != nil {
		return nil, false
	}
	return loc, true
}

func loadZoneFromEtcTimezone() (*time.Location, bool) {
	data, err := stdsys.ReadFile("/etc/timezone")
	if err != nil {
		return nil, false
	}
	name := strings.TrimSpace(string(data))
	if name == "" {
		return nil, false
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, false
	}
	return loc, true
}

func loadZoneFromLocaltimeData() (*time.Location, bool) {
	data, err := stdsys.ReadFile("/etc/localtime")
	if err != nil {
		return nil, false
	}
	loc, err := time.LoadLocationFromTZData("Local", data)
	if err != nil {
		return nil, false
	}
	return loc, true
}

func (c ClockTime) OnDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), c.Hour, c.Minute, 0, 0, t.Location())
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

func EffectiveStatus(now time.Time, record *Record) (*Status, bool) {
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

	if isSleepingAt(now, sleepTime, wakeTime) {
		status.CurrentState = StateSleeping
		status.NextTransitionAt = timePtr(nextOccurrence(now, wakeTime))
		return status, changed
	}

	status.NextTransitionAt = timePtr(nextOccurrence(now, sleepTime))
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

func ManualSleep(record *Record, now time.Time) (*Record, error) {
	return applyOverride(record, now, StateSleeping)
}

func ManualWake(record *Record, now time.Time) (*Record, error) {
	return applyOverride(record, now, StateAwake)
}

func applyOverride(record *Record, now time.Time, state State) (*Record, error) {
	record, _ = Normalize(record, now)
	if record == nil {
		record = DefaultRecord()
	}

	updated := *record
	updated.OverrideState = state.Ptr()
	updated.OverrideUntil = nil

	// When the schedule is disabled there is no next automatic boundary, so
	// OverrideUntil stays nil and Normalize will never time out this override.
	// devicectl clears Override* on every setSleepSchedule save so re-enabling
	// (or changing hours) cannot inherit a stale sleepNow/wakeNow from while off.

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
			updated.OverrideUntil = timePtr(nextOccurrence(now, wakeTime))
		case StateAwake:
			updated.OverrideUntil = timePtr(nextOccurrence(now, sleepTime))
		default:
			return nil, fmt.Errorf("unknown override state %q", state)
		}
	}

	return &updated, nil
}

func nextOccurrence(now time.Time, clockTime ClockTime) time.Time {
	candidate := clockTime.OnDay(now)
	if !candidate.After(now) {
		return candidate.Add(24 * time.Hour)
	}
	return candidate
}

func isSleepingAt(now time.Time, sleepTime, wakeTime ClockTime) bool {
	sleepToday := sleepTime.OnDay(now)
	wakeToday := wakeTime.OnDay(now)

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
