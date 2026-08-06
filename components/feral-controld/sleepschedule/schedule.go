package sleepschedule

import (
	"fmt"
	stdsys "os"
	"path/filepath"
	"sort"
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
	Enabled   bool   `json:"enabled"`
	SleepTime string `json:"sleepTime,omitempty"`
	WakeTime  string `json:"wakeTime,omitempty"`
	// Days lists the active weekdays as lowercase tokens ("sun".."sat") in
	// canonical Sun..Sat order. nil/absent means every day — the pre-days wire
	// and record shape — so records written by old apps keep today's behavior.
	// The panel sleeps for the whole of any unselected day.
	Days          []string   `json:"days,omitempty"`
	OverrideState *State     `json:"overrideState,omitempty"`
	OverrideUntil *time.Time `json:"overrideUntil,omitempty"`
}

type Status struct {
	Enabled          bool       `json:"enabled"`
	SleepTime        string     `json:"sleepTime,omitempty"`
	WakeTime         string     `json:"wakeTime,omitempty"`
	Days             []string   `json:"days,omitempty"`
	CurrentState     State      `json:"currentState"`
	OverrideState    *State     `json:"overrideState,omitempty"`
	OverrideUntil    *time.Time `json:"overrideUntil,omitempty"`
	NextTransitionAt *time.Time `json:"nextTransitionAt,omitempty"`
}

// dayTokens are the wire/persisted weekday identifiers, indexed by
// time.Weekday (Sunday = 0).
var dayTokens = [7]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// NormalizeDays validates and canonicalizes an active-days list: tokens are
// case/whitespace-insensitive, deduped, and ordered Sun..Sat. nil stays nil
// ("every day"). A list covering all seven days also collapses to nil so a
// fully-selected schedule persists and reports in the legacy every-day shape.
// A non-nil list selecting nothing is an error: "never awake" is not a valid
// schedule (use enabled=false or sleepNow instead).
func NormalizeDays(days []string) ([]string, error) {
	if days == nil {
		return nil, nil
	}

	var selected [7]bool
	for _, raw := range days {
		token := strings.ToLower(strings.TrimSpace(raw))
		found := false
		for i, t := range dayTokens {
			if t == token {
				selected[i] = true
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("invalid day %q: want one of sun,mon,tue,wed,thu,fri,sat", raw)
		}
	}

	normalized := make([]string, 0, 7)
	for i, on := range selected {
		if on {
			normalized = append(normalized, dayTokens[i])
		}
	}
	if len(normalized) == 0 {
		return nil, fmt.Errorf("days must include at least one day")
	}
	if len(normalized) == 7 {
		return nil, nil
	}
	return normalized, nil
}

// dayActive reports whether t's weekday is an active schedule day. nil/empty
// means every day (legacy records, old apps).
func dayActive(days []string, t time.Time) bool {
	if len(days) == 0 {
		return true
	}
	token := dayTokens[t.Weekday()]
	for _, d := range days {
		if d == token {
			return true
		}
	}
	return false
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
	loc, _ := LocalTimezoneResolved()
	return loc
}

// LocalTimezoneResolved is LocalTimezone plus an explicit signal for whether
// any resolver actually succeeded. Callers that ACT on civil time (the sleep
// schedule loop) must check it: the time.Local fallback is usually stale UTC,
// and sleeping/waking the display hours off the user's wall clock is worse
// than deferring until the timezone resolves. Display-only callers (status
// reporting) can ignore the flag.
func LocalTimezoneResolved() (*time.Location, bool) {
	if loc, ok := loadZoneFromLocaltimeSymlink(); ok {
		return loc, true
	}
	if loc, ok := loadZoneFromEtcTimezone(); ok {
		return loc, true
	}
	if loc, ok := loadZoneFromLocaltimeData(); ok {
		return loc, true
	}
	return time.Local, false
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
	if _, err := NormalizeDays(record.Days); err != nil {
		return err
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
		Days:          record.Days,
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

	if scheduleStateAt(now, sleepTime, wakeTime, record.Days) == StateSleeping {
		status.CurrentState = StateSleeping
		status.NextTransitionAt = nextTransitionTo(now, sleepTime, wakeTime, record.Days, StateAwake)
		return status, changed
	}

	status.NextTransitionAt = nextTransitionTo(now, sleepTime, wakeTime, record.Days, StateSleeping)
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

	// OverrideUntil is deliberately day-blind: it expires at the next clock
	// occurrence of the opposite boundary, ignoring Days. A wakeNow on an
	// inactive Saturday therefore lasts until that evening's sleep time — not
	// until Monday — and when it expires the day-aware natural state takes
	// back over (a sleepNow that expires on an inactive day just keeps
	// sleeping). This preserves the pre-days "until tonight's boundary" feel.
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
	loc := now.Location()
	candidate := clockTime.OnDay(now)
	if candidate.After(now) {
		return candidate
	}
	// Advance by one local calendar day, not Add(24*time.Hour). On DST transition
	// days a civil day is not always 86400s long; +24h skews the next wall-clock
	// slot (see sleepschedule tests: Europe/Paris spring 2025).
	noonNext := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, loc).AddDate(0, 0, 1)
	return clockTime.OnDay(noonNext)
}

// scheduleStateAt is the natural (override-free) schedule state at t: an
// unselected day sleeps for its entire civil day; on selected days the
// sleep/wake window rule applies unchanged.
func scheduleStateAt(t time.Time, sleepTime, wakeTime ClockTime, days []string) State {
	if !dayActive(days, t) {
		return StateSleeping
	}
	if isSleepingAt(t, sleepTime, wakeTime) {
		return StateSleeping
	}
	return StateAwake
}

// nextTransitionTo returns the first instant after now when the natural
// schedule state flips into target, or nil when no flip occurs in the scan
// horizon. Rather than composing per-day window arithmetic with the
// active-days rule analytically, it walks the only instants the state can
// change — each day's midnight, sleep time, and wake time — and evaluates
// scheduleStateAt there. Midnight is a boundary because an unselected day
// starts sleeping at 00:00 regardless of the window. Nine occurrences of each
// boundary (~8 days) cover the worst case of a single active day per week.
// Boundary instants come from nextOccurrence so day-advance stays DST-safe.
func nextTransitionTo(now time.Time, sleepTime, wakeTime ClockTime, days []string, target State) *time.Time {
	const horizonDays = 9
	candidates := make([]time.Time, 0, 3*horizonDays)
	for _, ct := range []ClockTime{{}, sleepTime, wakeTime} {
		c := now
		for i := 0; i < horizonDays; i++ {
			c = nextOccurrence(c, ct)
			candidates = append(candidates, c)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Before(candidates[j]) })

	state := scheduleStateAt(now, sleepTime, wakeTime, days)
	for _, candidate := range candidates {
		s := scheduleStateAt(candidate, sleepTime, wakeTime, days)
		if s == state {
			continue
		}
		state = s
		if s == target {
			return timePtr(candidate)
		}
	}
	return nil
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
