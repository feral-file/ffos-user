package dp1

import (
	"context"
	"encoding/json"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"go.uber.org/zap"
)

// SeriesPlaybackMode is a seriesRegistry item's own stated intent, or a
// device's override of it: which of a series' editions a cast actually
// carries. Not part of DP-1 core (display-protocol/dp1#55 does not define
// it) and not a dp1-go field — read from the raw document bytes by
// readItemSeriesPlaybackModes, ahead of a proposal (feral-file/ffos-user#425).
type SeriesPlaybackMode string

const (
	// SeriesPlaybackFirst casts only the series' representative edition.
	SeriesPlaybackFirst SeriesPlaybackMode = "first"
	// SeriesPlaybackFull casts every edition the catalog returned.
	SeriesPlaybackFull SeriesPlaybackMode = "full"
	// SeriesPlaybackRandom is accepted and stored like any other mode, but
	// FF1 has no re-roll-per-loop behavior and narrowEditions resolves it to
	// SeriesPlaybackFirst at cast time — see that function's doc.
	SeriesPlaybackRandom SeriesPlaybackMode = "random"
)

// ParseSeriesPlaybackMode validates s against the three modes above. Used
// both for a document's own per-item field and for a controller's
// setSeriesPlayback request, so an unrecognized string — a future mode this
// build does not know — is rejected the same way in both places rather than
// silently falling through to a default.
func ParseSeriesPlaybackMode(s string) (SeriesPlaybackMode, bool) {
	switch SeriesPlaybackMode(s) {
	case SeriesPlaybackFirst, SeriesPlaybackFull, SeriesPlaybackRandom:
		return SeriesPlaybackMode(s), true
	default:
		return "", false
	}
}

// ResolveSeriesPlaybackMode decides which mode a series item actually casts
// in. device is the owner's device-level setting (nil means "follow the
// document" — the nullable, absent-is-distinguishable-from-first state
// required by feral-file/ffos-user#425); document is the item's own stated
// seriesPlayback, read from the raw bytes by readItemSeriesPlaybackModes.
//
// device wins when set, exactly like the existing device-level duration
// override (dp1playlist.DisplayPrefs): a configured device setting is the
// viewer's explicit choice and takes priority over what the publisher asked
// for. When device is nil, the document's own stated mode is authoritative.
// When NEITHER states anything, the default is SeriesPlaybackFull — the
// behavior every series.go caller already has today (feral-file/ffos-user#421),
// so content published before this field existed keeps casting exactly as it
// does now (owner decision, 2026-10-30).
func ResolveSeriesPlaybackMode(document, device *SeriesPlaybackMode) SeriesPlaybackMode {
	if device != nil {
		return *device
	}
	if document != nil {
		return *document
	}
	return SeriesPlaybackFull
}

// NarrowEditions returns the editions a cast in mode actually carries.
// full casts every edition; first and random both narrow to the
// representative — FF1 has no re-roll-per-loop behavior, so random has
// nothing to randomize AT CAST TIME and resolves to the same single edition
// first would (owner decision, 2026-10-30; AC4 of feral-file/ffos-user#425).
// random is still stored and returned as its own value everywhere else — only
// this narrowing step collapses it.
func NarrowEditions(mode SeriesPlaybackMode, editions []dp1playlist.PlaylistItem) []dp1playlist.PlaylistItem {
	if mode == SeriesPlaybackFull || len(editions) <= 1 {
		return editions
	}
	return editions[:1]
}

// SeriesGroupKey identifies one series for grouping and caching purposes:
// contract chain + address + seriesId, NOT seriesId alone. A numeric seriesId
// is only unique within one contract — feralfile-bot's F1 evidence on
// ff-player#320 was exactly two different contracts sharing one numeric
// seriesId being collapsed together (AC8 of feral-file/ffos-user#425).
type SeriesGroupKey struct {
	Chain    string
	Address  string
	SeriesID int
}

// seriesGroupKey builds item's group key from its provenance contract. ok is
// false when the item carries no seriesId at all (isSeriesItem already
// guarantees this for every item this package expands, but the helper stays
// total so a caller never has to re-check isSeriesItem first).
func seriesGroupKey(item dp1playlist.PlaylistItem) (SeriesGroupKey, bool) {
	if item.Provenance == nil || item.Provenance.Contract == nil {
		return SeriesGroupKey{}, false
	}
	contract := item.Provenance.Contract
	if contract.SeriesID == nil {
		return SeriesGroupKey{}, false
	}
	return SeriesGroupKey{Chain: contract.Chain, Address: contract.Address, SeriesID: *contract.SeriesID}, true
}

// ResolvedSeriesGroup is one series item's resolved expansion: its identity,
// the document's own stated mode (nil if absent or unrecognized), and the
// FULL edition set the catalog returned — kept un-narrowed so a later device
// setting change can re-narrow it (CastPlan.Narrow) without a second catalog
// round trip (AC5 of feral-file/ffos-user#425).
type ResolvedSeriesGroup struct {
	Key          SeriesGroupKey
	DocumentMode *SeriesPlaybackMode
	Editions     []dp1playlist.PlaylistItem
}

// CastSegment is one position in a playlist as resolveSeriesCastPlan walked
// it: either an ordinary item carried through unchanged, or a series group
// kept unflattened. Exactly one of Item/Group is set.
type CastSegment struct {
	Item  *dp1playlist.PlaylistItem
	Group *ResolvedSeriesGroup
}

// CastPlan is what one cast resolved, in playlist order. Narrow rebuilds the
// actual item list to send for a given device setting; a plan with no
// series groups at all narrows to exactly the original items.
type CastPlan struct {
	Segments []CastSegment
}

// HasSeriesGroups reports whether this plan carries any series group —
// callers use it to decide whether a cast is worth remembering for a later
// re-narrow (CastPlan.Narrow) when the device setting changes.
func (p CastPlan) HasSeriesGroups() bool {
	for _, seg := range p.Segments {
		if seg.Group != nil {
			return true
		}
	}
	return false
}

// Narrow flattens the plan into the item list to actually cast under device
// (the current device-level setting; nil means "follow each group's own
// document mode"). Every group resolves independently: two series in the
// same playlist can carry different document modes, and only an EXPLICIT
// device override applies identically to all of them.
func (p CastPlan) Narrow(device *SeriesPlaybackMode) []dp1playlist.PlaylistItem {
	items := make([]dp1playlist.PlaylistItem, 0, len(p.Segments))
	for _, seg := range p.Segments {
		if seg.Item != nil {
			items = append(items, *seg.Item)
			continue
		}
		mode := ResolveSeriesPlaybackMode(seg.Group.DocumentMode, device)
		items = append(items, NarrowEditions(mode, seg.Group.Editions)...)
	}
	return items
}

// rawPlaylistItem is the subset of one playlist item's own JSON this package
// reads independently of the typed dp1playlist.PlaylistItem decode —
// seriesPlayback is not a dp1-go field (see SeriesPlaybackMode's doc), so it
// is read from the raw bytes directly rather than through the typed struct.
type rawPlaylistItem struct {
	SeriesPlayback *string `json:"seriesPlayback"`
}

// readItemSeriesPlaybackModes reads each item's own seriesPlayback from raw
// — the EXACT bytes the playlist was decoded from — and returns one entry
// per item, nil where the field is absent, unreadable, or not one of the
// three recognized modes. The returned slice always has length n (the
// caller's parsed item count): short of that on a parse failure or a raw
// document that named fewer items, nil beyond n's end never happens.
//
// raw may be nil — an internally rebuilt re-cast (setSeriesPlayback's
// re-narrow) has no wire form — in which case every item reads as absent,
// which is correct: there is no document distinct from what controld itself
// just resolved.
//
// Matching is by ARRAY INDEX between raw's `items` and the caller's already-
// parsed playlist.Items, which is only valid when raw is the very bytes that
// parse produced — true at this package's one call site with a real
// document (commandrouter's static dp1_call branch, before any filtering).
func (d *dp1) readItemSeriesPlaybackModes(raw json.RawMessage, n int) []*SeriesPlaybackMode {
	modes := make([]*SeriesPlaybackMode, n)
	if len(raw) == 0 {
		return modes
	}
	var envelope struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := d.json.Unmarshal(raw, &envelope); err != nil {
		return modes
	}
	for i := 0; i < n && i < len(envelope.Items); i++ {
		var item rawPlaylistItem
		if err := d.json.Unmarshal(envelope.Items[i], &item); err != nil || item.SeriesPlayback == nil {
			continue
		}
		if mode, ok := ParseSeriesPlaybackMode(*item.SeriesPlayback); ok {
			modes[i] = &mode
		}
	}
	return modes
}

// resolveSeriesCastPlan is expandSeriesItems's cast-aware sibling: instead of
// flattening every series item into all its editions (always, regardless of
// intent — the #421 behavior this issue corrects), it keeps each series
// group's FULL edition set alongside the document's own stated mode, so the
// caller can narrow it per the resolved playback mode and cache the group for
// a later, network-free re-narrow.
//
// Bounded exactly as expandSeriesItems: the same walk budget
// (seriesExpansionBudget), the same per-playlist catalog-call cap
// (maxSeriesPerPlaylist), and the same maxItems result cap (applied to the
// mode-narrowed count, so a cast this bounds never exceeds maxItems
// regardless of how many editions a full expansion would have produced). A
// series this walk cannot expand — the catalog is unreachable, holds nothing
// for it, or the budget is spent — is kept exactly as expandSeriesItems keeps
// it: as its own representative, unflattened. deviceMode may be nil (follow
// the document).
func (d *dp1) resolveSeriesCastPlan(ctx context.Context, playlist Playlist, rawPlaylist json.RawMessage, deviceMode *SeriesPlaybackMode, maxItems int) CastPlan {
	if !hasSeriesItems(playlist.Items) {
		segments := make([]CastSegment, len(playlist.Items))
		for i := range playlist.Items {
			item := playlist.Items[i]
			segments[i] = CastSegment{Item: &item}
		}
		return CastPlan{Segments: segments}
	}

	ctx, cancel := context.WithTimeout(ctx, seriesExpansionBudget)
	defer cancel()

	documentModes := d.readItemSeriesPlaybackModes(rawPlaylist, len(playlist.Items))

	var plan CastPlan
	castCount := 0
	attempts := 0
	for i, item := range playlist.Items {
		var seg CastSegment
		switch {
		case !isSeriesItem(item):
			it := item
			seg = CastSegment{Item: &it}
			castCount++

		case attempts >= maxSeriesPerPlaylist || ctx.Err() != nil:
			d.logger.Warn("series item kept unexpanded; expansion budget spent",
				zap.String("playlist_id", playlist.ID),
				zap.String("item_slug", item.Slug),
				zap.Int("attempts", attempts),
				zap.Error(ctx.Err()))
			it := item
			seg = CastSegment{Item: &it}
			castCount++

		default:
			attempts++
			editions, err := d.seriesEditions(ctx, playlist.ID, item)
			if err != nil || len(editions) == 0 {
				if err != nil {
					d.logger.Warn("series item kept unexpanded; its representative still plays",
						zap.String("playlist_id", playlist.ID),
						zap.String("item_slug", item.Slug),
						zap.Error(err))
				}
				it := item
				seg = CastSegment{Item: &it}
				castCount++
				break
			}
			inherited := inheritInstanceFields(item, editions)
			group := &ResolvedSeriesGroup{Editions: inherited}
			if key, ok := seriesGroupKey(item); ok {
				group.Key = key
			}
			if i < len(documentModes) {
				group.DocumentMode = documentModes[i]
			}
			seg = CastSegment{Group: group}
			mode := ResolveSeriesPlaybackMode(group.DocumentMode, deviceMode)
			castCount += len(NarrowEditions(mode, inherited))
			d.logger.Info("resolved a series for cast",
				zap.String("playlist_id", playlist.ID),
				zap.String("item_slug", item.Slug),
				zap.Int("editions", len(inherited)),
				zap.String("mode", string(mode)))
		}

		plan.Segments = append(plan.Segments, seg)
		if maxItems > 0 && castCount >= maxItems {
			break
		}
	}
	return plan
}

// ResolveSeriesCastPlan is resolveSeriesCastPlan, exported for the
// commandrouter package: the one call site with the raw document bytes a
// cast needs to read seriesPlayback from (see the DP1 interface doc).
func (d *dp1) ResolveSeriesCastPlan(ctx context.Context, playlist Playlist, rawPlaylist json.RawMessage, deviceMode *SeriesPlaybackMode, maxItems int) CastPlan {
	return d.resolveSeriesCastPlan(ctx, playlist, rawPlaylist, deviceMode, maxItems)
}
