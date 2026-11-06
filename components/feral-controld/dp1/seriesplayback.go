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
// full casts every catalog edition; first and random both narrow to
// representative — the series item's OWN declared edition (dp1/series.go:
// "its own `source` is one representative edition"), not editions[0]. The
// catalog's ordering has no guarantee of matching what the document itself
// named as representative (feralfile-bot F1, 2026-10-30), so first/random
// must use representative directly rather than slicing the catalog's
// answer. FF1 has no re-roll-per-loop behavior, so random has nothing to
// randomize AT CAST TIME and resolves to the same single edition first
// would (owner decision, 2026-10-30; AC4 of feral-file/ffos-user#425).
// random is still stored and returned as its own value everywhere else —
// only this narrowing step collapses it.
//
// full is itself gated by artworkModel.hasDistinctEditions(): the full/first
// config only has a real choice to make for a `multi_unique` series, whose
// editions are genuinely different artworks. A `single` (1/1) series has
// only ever the one artwork, and a `multi` series' editions are identical
// copies of one artwork, so both narrow to the representative regardless of
// mode — casting N identical copies under `full` shows nothing a viewer
// could tell apart from casting one (owner decision, 2026-11-06). An empty
// or unrecognized artworkModel (the catalog did not state it) is NOT
// narrowed this way — see hasDistinctEditions' doc.
//
// The representative itself still carries the document's series marker
// (contract.seriesId, no tokenId) — it IS the series item, unmodified — so
// emitting it as-is would still satisfy isSeriesItem. clearSeriesMarker
// strips that marker from the copy returned here, never from representative
// itself, so a later re-entry into Process built from this output (a
// setSeriesPlayback re-cast, a recently-played replay) is never re-read as
// an unresolved series (feralfile-bot F4, 2026-10-30).
func NarrowEditions(mode SeriesPlaybackMode, artworkModel SeriesArtworkModel, representative dp1playlist.PlaylistItem, editions []dp1playlist.PlaylistItem) []dp1playlist.PlaylistItem {
	if mode == SeriesPlaybackFull && artworkModel.hasDistinctEditions() {
		return editions
	}
	return []dp1playlist.PlaylistItem{clearSeriesMarker(representative)}
}

// clearSeriesMarker returns a copy of item with its seriesRegistry series
// marker (contract.seriesId) cleared. Provenance and Contract are replaced
// wholesale rather than edited through their shared pointers, so the
// caller's own representative — kept inside ResolvedSeriesGroup for a later
// re-narrow under a different mode — is never mutated by this.
func clearSeriesMarker(item dp1playlist.PlaylistItem) dp1playlist.PlaylistItem {
	if item.Provenance == nil || item.Provenance.Contract == nil || item.Provenance.Contract.SeriesID == nil {
		return item
	}
	provenance := *item.Provenance
	contract := *item.Provenance.Contract
	contract.SeriesID = nil
	provenance.Contract = &contract
	item.Provenance = &provenance
	return item
}

// SeriesGroupKey identifies one series for grouping and caching purposes:
// contract chain + address + seriesId, NOT seriesId alone. A numeric seriesId
// is only unique within one contract — feralfile-bot's F1 evidence on
// ff-player#320 was exactly two different contracts sharing one numeric
// seriesId being collapsed together (AC8 of feral-file/ffos-user#425).
type SeriesGroupKey struct {
	Chain    string
	Address  string
	SeriesID dp1playlist.SeriesID
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

// unresolvedSeriesGroup wraps a series item the walk could not expand — the
// catalog is unreachable, holds nothing for it, or the budget is spent — as
// its own one-edition group, rather than a plain Item segment. A plain Item
// would carry the document's series marker (contract.seriesId) straight
// through Narrow untouched: harmless for a cast built fresh from a real
// document, but not for commandrouter's internal rebuild (recastForMode),
// which feeds Narrow's own output back into Process as a new dp1_call — a
// marker-carrying item there still satisfies isSeriesItem, so it is resolved
// a second time, and because that rebuild's JSON round trip has already
// dropped the document's un-typed seriesPlayback field, the second
// resolution silently defaults to full instead of the original mode
// (feralfile-bot F5, 2026-11-02).
//
// The marker is stripped HERE, unconditionally, rather than left to
// NarrowEditions' per-mode clearSeriesMarker call: this representative IS
// the one and only "edition" regardless of which mode it is later narrowed
// under (there is no catalog data to pick a different edition from), so
// full and first/random show identical content either way — unlike a
// GENUINELY resolved group, where full's whole point is to show editions
// the catalog returned, not the representative. Clearing only for
// first/random (the first attempt at this fix, feralfile-bot F1 pass 1 on
// this fix, 2026-11-02) left full's narrow still marker-carrying, which is
// the branch's own documented DEFAULT mode when neither the document nor
// the device states one — so a plan mixing this fallback with a genuinely
// resolved sibling (tracked because of that sibling) still re-triggered the
// synchronous catalog retry on every recast under the common case
// (feralfile-bot F1 pass 2 on this fix, 2026-11-02).
//
// Resolved is deliberately left false (the zero value): this group has no
// real catalog data behind it, only the one item it was given, and
// CastPlan.HasSeriesGroups must not count a LONE one of these as something
// worth tracking for a later re-cast — see that method's doc.
func unresolvedSeriesGroup(item dp1playlist.PlaylistItem, documentMode *SeriesPlaybackMode) *ResolvedSeriesGroup {
	representative := clearSeriesMarker(item)
	group := &ResolvedSeriesGroup{
		Representative: representative,
		Editions:       []dp1playlist.PlaylistItem{representative},
		DocumentMode:   documentMode,
	}
	// seriesGroupKey reads Contract.SeriesID, which the cleared copy above
	// no longer has — it must run against the original, marker-carrying
	// item to still identify this group correctly.
	if key, ok := seriesGroupKey(item); ok {
		group.Key = key
	}
	return group
}

// ResolvedSeriesGroup is one series item's resolved expansion: its identity,
// the document's own stated mode (nil if absent or unrecognized), the series
// item itself (Representative — what first/random actually cast, per
// NarrowEditions' doc), and the FULL edition set the catalog returned — kept
// un-narrowed so a later device setting change can re-narrow it
// (CastPlan.Narrow) without a second catalog round trip (AC5 of
// feral-file/ffos-user#425).
//
// Resolved is true only when the catalog actually answered for this series —
// false for unresolvedSeriesGroup's fallback, whose "editions" is just the
// one item itself. CastPlan.HasSeriesGroups reads this to decide whether a
// cast is worth caching for a later re-narrow; Narrow and NarrowEditions do
// not care and treat both the same way.
type ResolvedSeriesGroup struct {
	Key            SeriesGroupKey
	DocumentMode   *SeriesPlaybackMode
	Representative dp1playlist.PlaylistItem
	Editions       []dp1playlist.PlaylistItem
	Resolved       bool
	// ArtworkModel is the catalog's own `series.artworkModel` for this
	// group — empty for unresolvedSeriesGroup, which has no catalog answer
	// to read it from. NarrowEditions treats empty the same as an
	// unrecognized value: see SeriesArtworkModel.hasDistinctEditions.
	ArtworkModel SeriesArtworkModel
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
// series groups at all narrows to exactly the original items. maxItems is
// applied by Narrow itself, not while the plan is built: Segments carries
// every item the resolve walk saw, regardless of how many a given mode
// narrows to, so a later re-narrow under a DIFFERENT mode (full, after an
// earlier first) still has every trailing segment to work with (feralfile-
// bot F3, 2026-10-30). The zero value (maxItems 0) narrows unbounded.
type CastPlan struct {
	Segments []CastSegment
	maxItems int
}

// HasSeriesGroups reports whether this plan carries any GENUINELY RESOLVED
// series group — commandrouter's rememberSeriesCast reads this to decide
// whether a cast is worth caching for a later, network-free re-narrow
// (CastPlan.Narrow) when the device setting changes.
//
// An unresolved fallback (unresolvedSeriesGroup: Resolved false) does not
// count, even though it is a Group segment: it carries no real catalog data
// to re-narrow, so caching it buys nothing, and caching it was actively
// harmful (feralfile-bot F1 on this fix, 2026-11-02) — a cast with no OTHER
// resolved group would then be remembered as "tracked", so a later
// setSeriesPlayback call would re-enter recastForMode's rebuild-and-Process
// path instead of no-opping, and for the (default) full mode that rebuild's
// item still carries the series marker, re-triggering a synchronous catalog
// retry inside the command handler — exactly the blocking round trip the
// cache exists to avoid. A plan that mixes a resolved group with an
// unresolved one is unaffected: the resolved group alone makes it worth
// tracking, and the unresolved one's marker is already stripped at
// construction (unresolvedSeriesGroup), so Narrow/NarrowEditions never
// re-trigger a catalog call for it regardless of which mode it narrows
// under.
func (p CastPlan) HasSeriesGroups() bool {
	for _, seg := range p.Segments {
		if seg.Group != nil && seg.Group.Resolved {
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
//
// maxItems is applied HERE, not while the plan was built — see CastPlan's
// doc — so each flatten independently caps at the same result length a
// direct expansion would, whichever mode it narrows to.
func (p CastPlan) Narrow(device *SeriesPlaybackMode) []dp1playlist.PlaylistItem {
	items := make([]dp1playlist.PlaylistItem, 0, len(p.Segments))
	for _, seg := range p.Segments {
		if seg.Item != nil {
			items = append(items, *seg.Item)
		} else {
			mode := ResolveSeriesPlaybackMode(seg.Group.DocumentMode, device)
			items = append(items, NarrowEditions(mode, seg.Group.ArtworkModel, seg.Group.Representative, seg.Group.Editions)...)
		}
		if p.maxItems > 0 && len(items) >= p.maxItems {
			return items[:p.maxItems]
		}
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
// Bounded exactly as expandSeriesItems on the catalog side: the same walk
// budget (seriesExpansionBudget) and the same per-playlist catalog-call cap
// (maxSeriesPerPlaylist). maxItems is NOT applied here — the plan keeps
// every segment the walk saw regardless of how many items the initially
// resolved mode narrows to; it is applied by CastPlan.Narrow instead, once
// per flatten, so a later re-narrow under a wider mode is never missing the
// trailing segments an earlier, narrower flatten had no room for
// (feralfile-bot F3, 2026-10-30; see CastPlan's doc). A series this walk
// cannot expand — the catalog is unreachable, holds nothing for it, or the
// budget is spent — still casts exactly as expandSeriesItems would (its own
// representative, unflattened), but is wrapped as its own one-edition group
// (unresolvedSeriesGroup) rather than a plain item, so the document's mode
// survives into the cache and a later internal rebuild never re-resolves or
// re-fetches it (feralfile-bot F5, 2026-11-02; see unresolvedSeriesGroup's
// doc). deviceMode may be nil (follow the document).
func (d *dp1) resolveSeriesCastPlan(ctx context.Context, playlist Playlist, rawPlaylist json.RawMessage, deviceMode *SeriesPlaybackMode, maxItems int) CastPlan {
	if !hasSeriesItems(playlist.Items) {
		// No series to narrow, so this plan is a straight pass-through — the
		// same guarantee expandSeriesItems's own early return gives ordinary
		// playlists. maxItems is deliberately NOT carried into the plan here:
		// Narrow treats a zero maxItems as unbounded, and feeding it the
		// series-expansion cap would truncate an ordinary playlist longer
		// than that cap, which never expanded anything to bound (feralfile-
		// bot F6, 2026-11-02).
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

	plan := CastPlan{maxItems: maxItems}
	attempts := 0
	for i, item := range playlist.Items {
		var seg CastSegment
		switch {
		case !isSeriesItem(item):
			it := item
			seg = CastSegment{Item: &it}

		case attempts >= maxSeriesPerPlaylist || ctx.Err() != nil:
			d.logger.Warn("series item kept unexpanded; expansion budget spent",
				zap.String("playlist_id", playlist.ID),
				zap.String("item_slug", item.Slug),
				zap.Int("attempts", attempts),
				zap.Error(ctx.Err()))
			var documentMode *SeriesPlaybackMode
			if i < len(documentModes) {
				documentMode = documentModes[i]
			}
			seg = CastSegment{Group: unresolvedSeriesGroup(item, documentMode)}

		default:
			attempts++
			editions, artworkModel, err := d.seriesEditions(ctx, playlist.ID, item)
			if err != nil || len(editions) == 0 {
				if err != nil {
					d.logger.Warn("series item kept unexpanded; its representative still plays",
						zap.String("playlist_id", playlist.ID),
						zap.String("item_slug", item.Slug),
						zap.Error(err))
				}
				var documentMode *SeriesPlaybackMode
				if i < len(documentModes) {
					documentMode = documentModes[i]
				}
				seg = CastSegment{Group: unresolvedSeriesGroup(item, documentMode)}
				break
			}
			inherited := inheritInstanceFields(item, editions)
			group := &ResolvedSeriesGroup{Representative: item, Editions: inherited, Resolved: true, ArtworkModel: artworkModel}
			if key, ok := seriesGroupKey(item); ok {
				group.Key = key
			}
			if i < len(documentModes) {
				group.DocumentMode = documentModes[i]
			}
			seg = CastSegment{Group: group}
			mode := ResolveSeriesPlaybackMode(group.DocumentMode, deviceMode)
			d.logger.Info("resolved a series for cast",
				zap.String("playlist_id", playlist.ID),
				zap.String("item_slug", item.Slug),
				zap.Int("editions", len(inherited)),
				zap.String("mode", string(mode)))
		}

		plan.Segments = append(plan.Segments, seg)
	}
	return plan
}

// ResolveSeriesCastPlan is resolveSeriesCastPlan, exported for the
// commandrouter package: the one call site with the raw document bytes a
// cast needs to read seriesPlayback from (see the DP1 interface doc).
func (d *dp1) ResolveSeriesCastPlan(ctx context.Context, playlist Playlist, rawPlaylist json.RawMessage, deviceMode *SeriesPlaybackMode, maxItems int) CastPlan {
	return d.resolveSeriesCastPlan(ctx, playlist, rawPlaylist, deviceMode, maxItems)
}
