package offlinecache

import (
	dp1playlist "github.com/display-protocol/dp1-go/playlist"
)

// SeriesSegment and SeriesGroup mirror dp1.CastSegment/dp1.ResolvedSeriesGroup
// field-for-field, duplicated here rather than imported: this package has
// zero dependency on the local dp1 package today (a deliberate service
// boundary — offlinecache only ever knows dp1playlist, the upstream DP-1
// type), and commandrouter — the one package that already imports both — is
// where the 1:1 conversion between the two shapes belongs (feral-file/
// ffos-user#455, #453).
//
// DocumentMode/ArtworkModel are plain strings rather than dp1's own string-
// alias types, for the same import-boundary reason. An absent DocumentMode
// (nil) means the same thing it does in dp1.ResolvedSeriesGroup: the
// document stated nothing, so narrowing at read time falls through to the
// device override, then the SeriesPlaybackFull default.
type SeriesSegment struct {
	// Item is set for an ordinary (non-series) playlist position; exactly
	// one of Item/Group is non-nil, mirroring dp1.CastSegment.
	Item  *dp1playlist.PlaylistItem `json:"item,omitempty"`
	Group *SeriesGroup              `json:"group,omitempty"`
}

// SeriesGroupKey identifies one series for grouping — contract chain +
// address + seriesId, never seriesId alone (a numeric seriesId is only
// unique within one contract; see dp1.SeriesGroupKey's doc).
type SeriesGroupKey struct {
	Chain    string `json:"chain,omitempty"`
	Address  string `json:"address,omitempty"`
	SeriesID string `json:"seriesId,omitempty"`
}

// SeriesGroup is one series item's resolved expansion at the time it was
// cached: its identity, the document's own stated mode, the series item
// itself (Representative), and the FULL edition set the catalog returned —
// kept un-narrowed so a later read (a device-setting change, or a catalog-
// unreachable offline fallback) can re-narrow it with no network round
// trip, exactly like dp1.CastPlan.Narrow already does for a live cast. See
// dp1.ResolvedSeriesGroup's doc for the full field-by-field rationale —
// this is the same data, kept in step as it evolves.
type SeriesGroup struct {
	Key            SeriesGroupKey             `json:"key"`
	DocumentMode   *string                    `json:"documentMode,omitempty"`
	Representative dp1playlist.PlaylistItem   `json:"representative"`
	Editions       []dp1playlist.PlaylistItem `json:"editions"`
	Resolved       bool                       `json:"resolved"`
	ArtworkModel   string                     `json:"artworkModel,omitempty"`
}

// seriesSegmentsEnvelope is the versioned on-disk shape for the series-
// segments sidecar, mirroring automaticItemEnvelope's precedent exactly: an
// old binary that does not understand a future version must quarantine
// (refuse to decode) rather than silently misread it, since these segments
// are interpreted directly against whatever Items the main playlist record
// happens to hold for the same playlistID — a silently-wrong read here would
// narrow a playlist's items against a stale or foreign group set, not just
// serve a slightly outdated pointer the way a stale URL index does.
type seriesSegmentsEnvelope struct {
	Version  int             `json:"seriesSegmentsVersion"`
	Segments []SeriesSegment `json:"segments"`
}

const seriesSegmentsVersion = 1
