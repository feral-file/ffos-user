package playerlink

import "encoding/json"

// HistoryRecord is retained by the host only after presentation. Item and
// Defaults are private replay material; external clients get a label projection.
type HistoryRecord struct {
	RecordID   string         `json:"recordId"`
	PlayedAtMS *int64         `json:"playedAtMs"`
	Item       map[string]any `json:"item"`
	Defaults   map[string]any `json:"defaults"`
	Context    string         `json:"context"`
}

// History reads records and the presented occurrence in one host operation.
// A null ActiveOccurrence means no active artwork, including after restart.
type History struct {
	Records          []HistoryRecord `json:"records"`
	Incomplete       *bool           `json:"incomplete"`
	ActiveOccurrence json.RawMessage `json:"activeOccurrence"` // Required, nullable string; missing is not "none".
}
