package mediahealth

import (
	"time"
)

// Hint is the per-item entry controld attaches to a displayPlaylist push
// under the request's `mediaHealth` field (manifest contract mediaHealth
// v1): a sparse map keyed by item source, carrying ONLY conclusively
// broken items, so a 1,000-item wall with three dead links sends three
// entries. The player skips a hinted item in its timeline without mounting
// it; the item stays in its list and is reported on status.
type Hint struct {
	State    State    `json:"state"`
	Category Category `json:"category,omitempty"`
	Reason   Reason   `json:"reason,omitempty"`
}

// Hints returns the hint map for sources at now: conclusive broken records
// only. Sources that are not in the store, ok, suspect, or broken past
// their recheck window produce no entry.
func (s *Store) Hints(sources []string, now time.Time) (map[string]Hint, error) {
	records, err := s.GetMany(sources)
	if err != nil {
		return nil, err
	}
	hints := make(map[string]Hint)
	for src, rec := range records {
		if rec.Conclusive(now) {
			hints[src] = Hint{State: StateBroken, Category: rec.Category, Reason: rec.Reason}
		}
	}
	return hints, nil
}

// ItemHealth is the per-item annotation on player_status (`mediaHealth`,
// index-aligned with `items`) — deliberately tiny: state, and for a
// non-ok state the category, reason and layer. No timestamps, no URLs, no
// counters; those live on getMediaHealth. Strings are stable so the
// notification dedupe hash moves only on a real change.
type ItemHealth struct {
	State    State    `json:"state"`
	Category Category `json:"category,omitempty"`
	Reason   Reason   `json:"reason,omitempty"`
	Layer    Layer    `json:"layer,omitempty"`
	// Skipped is whether the PLAYER is resolving past this item right now,
	// from its own report (PlayerStatus.SkippedIndices) — the hint it
	// actually holds, not a recomputation at poll time. State is the last
	// observation; the two differ (an expired verdict, a replayable
	// source, a player without the contract, a push not yet received),
	// so the app's "Skip" badge must come from here, never from State.
	// Always serialized.
	Skipped bool `json:"skipped"`
	// Unskipped says why a broken item is NOT being skipped; present only
	// when State is broken and Skipped is false.
	Unskipped Unskipped `json:"unskipped,omitempty"`
	// Issue is the latest real attempt's problem (see Issue), present only
	// while State is NOT broken: a broken item shows its Category instead.
	// It complements State — an `ok` item with `rate_limited` plays fine
	// while the host throttles the device's checks.
	Issue Issue `json:"issue,omitempty"`
}

// Unskipped is the reason a broken item is not skipped by the player.
type Unskipped string

const (
	// UnskippedExpired: the verdict is past its recheck window, so it is
	// not conclusive and was not hinted.
	UnskippedExpired Unskipped = "expired"
	// UnskippedReplayable: the offline cache can serve the source, so the
	// hint deliberately left it out.
	UnskippedReplayable Unskipped = "replayable"
	// UnskippedPlayer: the installed player does not declare the hint
	// contract; nothing is hinted.
	UnskippedPlayer Unskipped = "player"
	// UnskippedPending: hinted now, but the player is not skipping it —
	// the push carrying the hint has not happened yet, or the player
	// chose to play it (repeat-one, or an all-broken map).
	UnskippedPending Unskipped = "pending"
)

// ItemHealths returns one ItemHealth per source, in order. A source with no
// record, or a record without a verdict, is "unknown". A broken record is
// reported broken whether or not it is still conclusive: it is the last
// thing this device observed, which is what the app's label means.
func (s *Store) ItemHealths(sources []string) ([]ItemHealth, error) {
	records, err := s.GetMany(sources)
	if err != nil {
		return nil, err
	}
	out := make([]ItemHealth, len(sources))
	for i, src := range sources {
		out[i] = itemHealthOf(records[src])
	}
	return out, nil
}

func itemHealthOf(rec *Record) ItemHealth {
	if rec == nil {
		return ItemHealth{State: StateUnknown}
	}
	switch rec.State {
	case "":
		return ItemHealth{State: StateUnknown, Issue: rec.Issue}
	case StateOK:
		return ItemHealth{State: StateOK, Issue: rec.Issue}
	case StateBroken:
		return ItemHealth{State: rec.State, Category: rec.Category, Reason: rec.Reason, Layer: rec.Layer}
	}
	return ItemHealth{State: rec.State, Category: rec.Category, Reason: rec.Reason, Layer: rec.Layer, Issue: rec.Issue}
}

// ReportItem is one row of the getMediaHealth reply: the whole playlist on
// screen, one verdict per item, with the detail the app can show on a
// work's page. Never a source URL.
type ReportItem struct {
	Index     int       `json:"index"`
	ID        string    `json:"id,omitempty"`
	Title     string    `json:"title,omitempty"`
	State     State     `json:"state"`
	Category  Category  `json:"category,omitempty"`
	Reason    Reason    `json:"reason,omitempty"`
	Layer     Layer     `json:"layer,omitempty"`
	Skipped   bool      `json:"skipped"`
	Unskipped Unskipped `json:"unskipped,omitempty"`
	CheckedAt string    `json:"checkedAt,omitempty"`
	// Issue / IssueSince: see ItemHealth.Issue; IssueSince is when the
	// current run of trouble began (RFC 3339).
	Issue      Issue  `json:"issue,omitempty"`
	IssueSince string `json:"issueSince,omitempty"`
	// NextCheckAt is when the device looks at this source again — the
	// verdict's recheck or a transient hold, whichever is later (RFC 3339).
	NextCheckAt string `json:"nextCheckAt,omitempty"`
	// FailedPlays counts the failed plays in the current failure run (any
	// ready but an error page ends it); the source concludes broken at 2.
	// Omitted at 0.
	FailedPlays int `json:"failedPlays,omitempty"`
	// ServedBy is the HOST of the fallback gateway the device loads this
	// work through, when the source's own gateway could not serve it. A
	// host, never a URL. Omitted when the source's own gateway serves it.
	ServedBy string `json:"servedBy,omitempty"`
	Plays    *struct {
		OK   int `json:"ok"`
		Fail int `json:"fail"`
	} `json:"plays,omitempty"`
}

// NeedsAttention reports whether a row is worth showing on its own: a
// verdict (broken or suspect), an issue, or a player skip. The
// `attention` filter of getMediaHealth; rows the lookahead has not reached
// yet read `unknown` with no issue and are not.
func (r ReportItem) NeedsAttention() bool {
	return r.State == StateBroken || r.State == StateSuspect || r.Issue != "" || r.Skipped
}

// ReportInput is what the caller knows about each item on screen.
type ReportInput struct {
	ID     string
	Title  string
	Source string
}

// Report builds the getMediaHealth rows for items, in order.
func (s *Store) Report(items []ReportInput) ([]ReportItem, error) {
	sources := make([]string, len(items))
	for i, it := range items {
		sources[i] = it.Source
	}
	records, err := s.GetMany(sources)
	if err != nil {
		return nil, err
	}
	out := make([]ReportItem, len(items))
	for i, it := range items {
		rec := records[it.Source]
		health := itemHealthOf(rec)
		row := ReportItem{
			Index:    i,
			ID:       it.ID,
			Title:    it.Title,
			State:    health.State,
			Category: health.Category,
			Reason:   health.Reason,
			Layer:    health.Layer,
			Issue:    health.Issue,
		}
		if rec != nil {
			if !rec.CheckedAt.IsZero() {
				row.CheckedAt = rec.CheckedAt.UTC().Format(time.RFC3339)
			}
			if health.Issue != "" && rec.IssueSince != nil {
				row.IssueSince = rec.IssueSince.UTC().Format(time.RFC3339)
			}
			if due := rec.dueAt(); !due.IsZero() {
				row.NextCheckAt = due.UTC().Format(time.RFC3339)
			}
			row.FailedPlays = rec.Plays.ConsecutiveFail
			if rec.ServedBy != "" {
				row.ServedBy = hostOf(rec.ServedBy)
			}
			if rec.Plays.OK > 0 || rec.Plays.Fail > 0 {
				row.Plays = &struct {
					OK   int `json:"ok"`
					Fail int `json:"fail"`
				}{OK: rec.Plays.OK, Fail: rec.Plays.Fail}
			}
		}
		out[i] = row
	}
	return out, nil
}
