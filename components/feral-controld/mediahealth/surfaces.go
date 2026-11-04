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
}

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
	if rec == nil || rec.State == "" {
		return ItemHealth{State: StateUnknown}
	}
	if rec.State == StateOK {
		return ItemHealth{State: StateOK}
	}
	return ItemHealth{State: rec.State, Category: rec.Category, Reason: rec.Reason, Layer: rec.Layer}
}

// ReportItem is one row of the getMediaHealth reply: the whole playlist on
// screen, one verdict per item, with the detail the app can show on a
// work's page. Never a source URL.
type ReportItem struct {
	Index     int      `json:"index"`
	ID        string   `json:"id,omitempty"`
	Title     string   `json:"title,omitempty"`
	State     State    `json:"state"`
	Category  Category `json:"category,omitempty"`
	Reason    Reason   `json:"reason,omitempty"`
	Layer     Layer    `json:"layer,omitempty"`
	CheckedAt string   `json:"checkedAt,omitempty"`
	Plays     *struct {
		OK   int `json:"ok"`
		Fail int `json:"fail"`
	} `json:"plays,omitempty"`
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
		}
		if rec != nil {
			if !rec.CheckedAt.IsZero() {
				row.CheckedAt = rec.CheckedAt.UTC().Format(time.RFC3339)
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
