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

// requiredDownloadSources returns the Source of every group's Representative,
// unconditionally — regardless of whether that same source also appears
// somewhere in the group's own Editions. These are the sources
// DownloadPlaylist's own queueing bound must never drop: a non-distinct-
// editions group's Narrow(nil) always collapses to the Representative, so a
// Representative the queue excludes leaves that group with nothing playable
// offline even though every catalog edition downloaded successfully
// (feralfile-bot F1, 2026-11-13). Protecting it only when dp1.CastPlan.Full
// appended it as an EXTRA item (source absent from Editions) missed the case
// where the source IS one of the Editions but that edition's own position
// already falls past the queue bound — the representative is just as
// unreplayable there, so the bound has to protect its source either way
// (feralfile-bot F1, follow-up, 2026-11-13).
func requiredDownloadSources(segments []SeriesSegment) map[string]struct{} {
	var required map[string]struct{}
	for _, seg := range segments {
		if seg.Group == nil {
			continue
		}
		if required == nil {
			required = make(map[string]struct{})
		}
		required[seg.Group.Representative.Source] = struct{}{}
	}
	return required
}

// boundQueuedItems applies DownloadPlaylist's maxQueued ceiling the way a
// plain items[:maxQueued] slice cannot: it never drops a source in
// required, trimming ordinary items first instead, and otherwise preserves
// items' original relative order. The result never exceeds maxQueued, even
// when required itself has more entries than the budget allows (an
// unreachable-in-practice case — maxQueued is dp1.MAX_PLAYLIST_ITEMS_LIMIT,
// far larger than any playlist's series-group count — but still bounded
// rather than silently violating the budget's own contract).
//
// The reservation pass below reserves at most ONE slot per distinct
// required source, not one per occurrence: a multi series whose editions
// share a Source with each other (the same artwork reused across several
// catalog entries) used to let every occurrence of that shared source
// consume the budget in the first pass, exhausting maxQueued before a
// later, equally-required source was ever reserved — even though reserving
// one slot per required source would have fit both (feralfile-bot F1,
// 2026-11-13).
func boundQueuedItems(items []dp1playlist.PlaylistItem, maxQueued int, required map[string]struct{}) []dp1playlist.PlaylistItem {
	if len(required) == 0 {
		return items[:maxQueued]
	}
	keep := make([]bool, len(items))
	kept := 0
	reserved := make(map[string]struct{}, len(required))
	for i, item := range items {
		if kept >= maxQueued {
			break
		}
		if _, ok := required[item.Source]; !ok {
			continue
		}
		if _, already := reserved[item.Source]; already {
			continue
		}
		reserved[item.Source] = struct{}{}
		keep[i] = true
		kept++
	}
	for i := range items {
		if kept >= maxQueued {
			break
		}
		if keep[i] {
			continue
		}
		keep[i] = true
		kept++
	}
	result := make([]dp1playlist.PlaylistItem, 0, maxQueued)
	for i, item := range items {
		if keep[i] {
			result = append(result, item)
		}
	}
	return result
}

// seriesSegmentsEnvelope is the versioned on-disk shape for the series-
// segments sidecar, mirroring automaticItemEnvelope's precedent exactly: an
// old binary that does not understand a future version must quarantine
// (refuse to decode) rather than silently misread it, since these segments
// are interpreted directly against whatever Items the main playlist record
// happens to hold for the same playlistID — a silently-wrong read here would
// narrow a playlist's items against a stale or foreign group set, not just
// serve a slightly outdated pointer the way a stale URL index does.
//
// BodyDigest is a hex SHA-256 of the exact playlist body bytes this sidecar
// was saved alongside (Store.SavePlaylistSeriesSegments's playlistRaw
// parameter). The sidecar and the playlist body are two separate files with
// no shared transaction, so a reader has no other way to tell "this sidecar
// still describes the body I just loaded" from "this sidecar belongs to a
// revision that has since been overwritten" — whether that happened because
// a reader landed between the two writes, or because a later save's sidecar
// delete silently failed, leaving this one behind (feralfile-bot F2,
// 2026-11-12). LoadPlaylistSeriesSegments recomputes the digest from the
// caller's own just-loaded body and rejects a mismatch exactly like "no
// sidecar at all" — see that method's doc.
type seriesSegmentsEnvelope struct {
	Version    int             `json:"seriesSegmentsVersion"`
	BodyDigest string          `json:"bodyDigest,omitempty"`
	Segments   []SeriesSegment `json:"segments"`
}

const seriesSegmentsVersion = 1
