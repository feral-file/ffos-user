package dp1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"go.uber.org/zap"
)

// A seriesRegistry item is a series, not an edition: core 1.1.0 §6's
// whole-series form, `contract.seriesId` with no `tokenId` (Sean,
// 2026-10-27). Its own `source` is one representative edition, so a player
// that knows nothing about series still shows the work.
//
// The editions come from the art catalog, which is the component that reads
// the series registry: `contract.seriesId` names an entry in the
// SeriesRegistry contract (feral-file/ff-evm-contracts), the contract holds
// that series' `tokenDataURI`, and the catalog resolves the two into items
// (Sang, 2026-10-28). This daemon asks the catalog rather than the chain
// index for three reasons:
//
//   - The index was keyed on the item's slug, as its `release_vendor_slug`.
//     A slug is editorial, not an identity: a series the index did not carry
//     under that slug had no editions at all, and nothing in DP-1 says a
//     slug means anything to a chain index.
//   - The catalog answers with DP-1 items that have already inherited from
//     the series item — duration, license, display, the manifest's artists —
//     so there is nothing to rebuild here. The index answered with tokens,
//     which had to be reshaped.
//   - One revision id per series comes back with them, so a client can tell
//     when the set moved without re-reading it.
//
// Every series is expanded, whatever playback mode the item asks for. The
// player narrows a full expansion back to the representative when the
// viewer's device setting says `first`, and narrowing needs no network —
// so the override is free in both directions, exactly as the device-level
// duration override is.
const (
	// The catalog this daemon reads editions from. A series item carries no
	// endpoint of its own (unlike a dynamicQuery, where it arrives inside
	// the signed document), so the default is named here and still passes
	// validateCatalogEndpoint.
	catalogBaseURL = "https://catalog.feralfile.io"

	// Editions fetched per request, and the ceiling on one series. A series
	// of 9,048 editions exists in the catalog; expanding it whole would
	// swamp a playlist whose other items are single works, so it is capped
	// at the same ceiling a dynamic playlist already respects.
	//
	// This is deliberately BELOW what the catalog would serve — its own page
	// maximum is 500 (SERIES_EDITIONS_PAGE_MAX in art-catalog-service's
	// read.ts) — because the binding limit here is the playlist's, not the
	// route's: a single series must not be able to fill more of a playlist
	// than a whole dynamic answer may. Raising this to 500 would let one
	// series carry nearly twice MAX_PLAYLIST_ITEMS_LIMIT.
	seriesPageLimit = MAX_PLAYLIST_ITEMS_LIMIT

	// The most series items one playlist is allowed to spend a catalog call
	// on. Derived rather than chosen: a conforming playlist holds at most
	// MAX_PLAYLIST_ITEMS_LIMIT items and every expansion contributes at least
	// one, so beyond this point no further call can change a conforming
	// result — while an unbounded loop costs one sequential fetch, at the
	// shared client's 30s timeout, for every series item a caller cares to
	// put in the document.
	maxSeriesPerPlaylist = MAX_PLAYLIST_ITEMS_LIMIT
)

// CATALOG_HOSTS is the allowlist validateCatalogEndpoint checks the base URL
// against, for the same reason FF_INDEXER_HOSTS exists: the answer decides
// what the screen plays, so the set of servers that may give it is closed.
// Both names front the same service and the same /api/v1 shape — production
// and the Development deployment the app also points at — so the one request
// buildCatalogEditionsURL builds is valid against either.
var CATALOG_HOSTS = map[string]bool{
	"catalog.feralfile.io":                                true,
	"art-catalog-service.bitmark-development.workers.dev": true,
}

// catalogEditions is the catalog's answer, of which only these two fields
// are read: `items` is the editions and `total` says how many exist, which
// is how a truncated expansion knows it was truncated.
//
// The answer's `series` block (the identity the catalog resolved
// `contract.seriesId` to) is deliberately NOT read. Stamping it onto each
// edition would let ff-player group the run — and its grouping default is
// what makes that harmful; see the note on seriesEditions.
type catalogEditions struct {
	Total int                        `json:"total"`
	Items []dp1playlist.PlaylistItem `json:"items"`
}

// expandSeriesItems replaces every seriesRegistry item in playlist.Items
// with the editions the catalog returns for it.
//
// An item the catalog cannot answer for is left exactly as it is: a series
// that fails to expand still plays its representative edition, which is the
// one outcome a screen must never lose. maxItems bounds the whole playlist,
// 0 meaning unbounded; the catalog calls the walk will make are bounded
// separately by maxSeriesPerPlaylist, which 0 does not lift.
func (d *dp1) expandSeriesItems(ctx context.Context, playlist Playlist, maxItems int) Playlist {
	if !hasSeriesItems(playlist.Items) {
		return playlist
	}

	expanded := make([]dp1playlist.PlaylistItem, 0, len(playlist.Items))
	attempts := 0
	for _, item := range playlist.Items {
		switch {
		case !isSeriesItem(item):
			expanded = append(expanded, item)

		// Bounded independently of maxItems, which bounds the RESULT and is
		// 0 — unbounded — on four of this method's six call sites, among them
		// CMD_DOWNLOAD_PLAYLIST's, which a LAN caller reaches with no pairing
		// under the shipped control.enabled=false default. Each series item
		// costs one sequential catalog round trip, so without a budget of its
		// own a document carrying N series items buys N of them, at the
		// shared HTTP client's 30s timeout each (run-reviewer round 2, F1).
		//
		// The context is checked on the same branch so a caller whose
		// deadline has passed stops paying for a result nobody is waiting
		// for. An already-issued request still runs to its own timeout: the
		// shared client takes no context, and threading one through it
		// reaches well past this expansion.
		//
		// Either way a series keeps its representative, which is exactly what
		// an unreachable catalog leaves behind — no new degraded shape.
		case attempts >= maxSeriesPerPlaylist || ctx.Err() != nil:
			d.logger.Warn("series item kept unexpanded; expansion budget spent",
				zap.String("playlist_id", playlist.ID),
				zap.String("item_slug", item.Slug),
				zap.Int("attempts", attempts),
				zap.Error(ctx.Err()))
			expanded = append(expanded, item)

		default:
			attempts++
			if editions, err := d.seriesEditions(ctx, playlist.ID, item); err == nil && len(editions) > 0 {
				d.logger.Info("expanded a series into its editions",
					zap.String("playlist_id", playlist.ID),
					zap.String("item_slug", item.Slug),
					zap.Int("editions", len(editions)))
				expanded = append(expanded, inheritInstanceFields(item, editions)...)
			} else {
				if err != nil {
					d.logger.Warn("series item kept unexpanded; its representative still plays",
						zap.String("playlist_id", playlist.ID),
						zap.String("item_slug", item.Slug),
						zap.Error(err))
				}
				expanded = append(expanded, item)
			}
		}

		// Checked after every append, not only a successful expansion's:
		// a plain item or a kept-unexpanded representative can cross
		// maxItems just as a batch of editions can, and the cast path
		// that passes a non-zero cap here must never hand the player
		// more than it asked for regardless of which branch grew the
		// slice (see CAST_PLAYLIST_ITEMS_LIMIT at the call site).
		if maxItems > 0 && len(expanded) >= maxItems {
			expanded = expanded[:maxItems]
			break
		}
	}

	playlist.Items = expanded
	return playlist
}

// ExpandSeriesItems is expandSeriesItems, exported for a caller outside this
// package that already holds a decoded, static playlist it did not get from
// ProcessPlaylistURL/ProcessPlaylistURLForCast/ProcessDynamicPlaylist(ForCast)
// — see the DP1 interface doc for why those four need no separate call.
func (d *dp1) ExpandSeriesItems(ctx context.Context, playlist Playlist, maxItems int) Playlist {
	return d.expandSeriesItems(ctx, playlist, maxItems)
}

// inheritInstanceFields copies the series item's playlist-instance fields onto
// the editions that replace it.
//
// The catalog merges each edition with its OWN copy of the series item, which
// is not necessarily the item in the document being played: a dp1_call payload,
// or a cast of a playlist edited after the catalog ingested it, can carry a
// schedule or a rating the catalog's copy never had. Everything else an edition
// needs is already merged catalog-side (duration, license, display, repro, the
// manifest's artists), so only the fields that belong to THIS playlist instance
// are propagated here.
//
// Three consumers read these fields after expansion, which is why losing them
// is not cosmetic:
//
//   - the cast preflight's scheduled-playlist scan (commandrouter/handler.go):
//     a series whose editions carry no displayAt is read as unscheduled, so the
//     dead-link probe runs against a premiere whose sources are legitimately
//     not up yet and REJECTS the cast outright.
//   - the displayAt scheduler (playlistschedule): no displayAt on any item is
//     the "nothing scheduled" branch, which clears the Daily timer and plays a
//     scheduled series immediately.
//   - the content policy filter (contentpolicy.FilterLocked): a `mature` series
//     expanding into unrated editions stops tripping the mature refusal, which
//     is the PERMISSIVE direction — the one failure here that must not be left
//     to a follow-up.
//
// The series item's own stated value wins over whatever the catalog supplied:
// the item in hand is the playlist instance being played. A field the series
// item does not state is left exactly as the edition had it. Values are copied
// per edition rather than shared, so a later edit that mutates one edition's
// schedule cannot reach its siblings.
//
// These three are the item in hand's outright because an edition cannot
// meaningfully state them: a schedule and a content rating are curation, made
// about an item's place in a playlist, not about the work. They still differ
// from each other in which way they fail. A rating is only ever re-applied,
// never cleared, because clearing is the PERMISSIVE direction — a `mature`
// series whose editions came back unrated has to stay mature even if the
// rating reached the catalog and not this copy. A schedule has no such safe
// direction and is reset, for the reason given at the assignment below.
//
// The playback settings the catalog also inherited are a different case again,
// and are handled by inheritPlaybackSettings.
func inheritInstanceFields(item dp1playlist.PlaylistItem, editions []dp1playlist.PlaylistItem) []dp1playlist.PlaylistItem {
	for i := range editions {
		// Cleared before being re-applied, unlike the two rating fields
		// below. The catalog answers from a snapshot of the series item taken
		// at its last ingest, so a schedule a curator has since removed can
		// still come back on every edition — and `displayAt` has no safe
		// direction to fail in: a stale timestamp both makes the cast
		// preflight treat an unscheduled playlist as scheduled (skipping the
		// dead-link probe) and arms a Daily timer that delays a cast the
		// document in hand no longer delays. So the item in hand decides
		// whether there is a schedule at all, not merely which one.
		editions[i].DisplayAt = nil
		if item.DisplayAt != nil {
			at := *item.DisplayAt
			editions[i].DisplayAt = &at
		}
		if item.ContentRating != nil {
			rating := *item.ContentRating
			editions[i].ContentRating = &rating
		}
		if item.ContentReasons != nil {
			reasons := make([]string, len(*item.ContentReasons))
			copy(reasons, *item.ContentReasons)
			editions[i].ContentReasons = &reasons
		}
	}
	inheritPlaybackSettings(item, editions)
	return editions
}

// inheritPlaybackSettings re-bases the playback settings the catalog inherited
// from ITS copy of the series item onto the copy actually being played.
//
// The catalog builds an edition by starting from its stored series item and
// letting the edition override it field by field — only `id`, `source` and
// `provenance` are always the edition's own (`editionItem` in
// art-catalog-service's series-editions.ts). So an expanded edition's
// `duration`, `license`, `display` and `override` are the STORED series item's
// values unless that edition stated its own, and the stored copy is not
// necessarily the item in hand: an inline dp1_call payload, or a playlist
// edited after the catalog ingested it, carries its own. Before this, a cast
// setting duration 99 on the series item silently played the catalog's 5
// (feralfile-bot F3, 2026-10-29).
//
// Only settings that are statements of the playlist instance are re-based.
// Left to the edition deliberately: `repro` (a seed and a frame hash describe
// that edition, not the series), `title`/`slug` (an edition is "#42"), and
// `ref`/`refHash`/`inlineManifest` (the work's manifest, where the catalog's
// own one-level metadata merge already keeps per-edition credits).
//
// Unlike the curation fields above, each is re-based per edition rather than
// across the whole answer, so the item in hand wins over the catalog's stale
// copy but never over a value an edition stated for itself — see rebaseField.
func inheritPlaybackSettings(item dp1playlist.PlaylistItem, editions []dp1playlist.PlaylistItem) {
	if item.Duration != nil {
		rebaseField(editions,
			func(e dp1playlist.PlaylistItem) any { return e.Duration },
			func(e *dp1playlist.PlaylistItem) {
				duration := *item.Duration
				e.Duration = &duration
			})
	}
	if item.License != "" {
		rebaseField(editions,
			func(e dp1playlist.PlaylistItem) any { return e.License },
			func(e *dp1playlist.PlaylistItem) { e.License = item.License })
	}
	if len(item.Override) > 0 {
		rebaseField(editions,
			func(e dp1playlist.PlaylistItem) any { return e.Override },
			func(e *dp1playlist.PlaylistItem) {
				override := make(json.RawMessage, len(item.Override))
				copy(override, item.Override)
				e.Override = override
			})
	}
	if item.Display != nil {
		rebaseField(editions,
			func(e dp1playlist.PlaylistItem) any { return e.Display },
			func(e *dp1playlist.PlaylistItem) {
				// A failed copy leaves the edition's own display rather than
				// blanking it: losing the re-base is a stale preference,
				// losing the field is an item with no render preferences.
				if display := copyDisplayPrefs(item.Display); display != nil {
					e.Display = display
				}
			})
	}
}

// rebaseField applies set to every edition that carries the field's inherited
// value, and leaves the rest alone.
//
// The inherited value is the one most editions share. That is what separates a
// value the catalog supplied from one an edition stated, without the daemon
// ever seeing the catalog's own copy of the series item: every edition of one
// series is merged against a SINGLE such copy, so the field comes back
// identical on every edition that did not state it, and an edition holding
// anything else stated it for itself.
//
// Taking the most common value per field, rather than requiring the whole
// answer to agree, is what makes a `multi_unique` series work: one edition of
// twenty stating its own duration must not strand the other nineteen on the
// catalog's stale one, which is the same defect this re-basing exists to fix.
func rebaseField(
	editions []dp1playlist.PlaylistItem,
	get func(dp1playlist.PlaylistItem) any,
	set func(*dp1playlist.PlaylistItem),
) {
	inherited := pluralityIndex(editions, get)
	if inherited < 0 {
		return
	}
	// Read before the loop mutates anything. Every comparison below is against
	// this original value, and an edition is only ever written after it has
	// been compared, so a rewritten edition cannot change a later verdict.
	value := get(editions[inherited])
	for i := range editions {
		if reflect.DeepEqual(get(editions[i]), value) {
			set(&editions[i])
		}
	}
}

// pluralityIndex returns the index of an edition holding the most common value
// for the field get reads, or -1 when no single value is most common.
//
// Pointers are followed, so two editions whose durations hold the same number
// count as the same value even though the pointers differ. A one-edition answer
// returns that edition: with no sibling to compare against, the document in
// hand is still the more current of the two copies. A true tie returns -1 and
// nothing is re-based — with the set split evenly there is no evidence which
// half the catalog supplied, and leaving both as they arrived is the reading
// that invents nothing.
//
// Editions that all state the same value are indistinguishable from
// inheritance and are re-based. Accepted: a value the whole set shares is the
// series' as far as anything here can tell, and the item in hand is the better
// authority for it.
//
// Cost: O(n²) reflect.DeepEqual comparisons per field, n bounded by
// seriesPageLimit, so at most ~65k narrow comparisons per field for the
// largest page the catalog will serve into one series. Kept as a plain pass
// rather than an O(n) hash of each value, because the bound is already far
// below the sequential-fetch latency the same expansion accepts — one 30s
// catalog round trip per series item — and a hashing pass would trade tested
// comparison semantics for a key function that has to agree with
// reflect.DeepEqual on pointers, maps and raw JSON. Pinned at the page ceiling
// by TestDP1_RebasingHoldsAcrossAFullEditionPage (run-reviewer round 3, F2).
func pluralityIndex(editions []dp1playlist.PlaylistItem, get func(dp1playlist.PlaylistItem) any) int {
	best, bestCount, tied := -1, 0, false
	for i := range editions {
		count := 0
		for j := range editions {
			if reflect.DeepEqual(get(editions[j]), get(editions[i])) {
				count++
			}
		}
		switch {
		case count > bestCount:
			best, bestCount, tied = i, count, false
		case count == bestCount && !reflect.DeepEqual(get(editions[i]), get(editions[best])):
			tied = true
		}
	}
	if tied {
		return -1
	}
	return best
}

// copyDisplayPrefs deep-copies display preferences, returning nil when they
// cannot be copied.
//
// Through JSON rather than by assignment because DisplayPrefs is a struct of
// pointers and maps: a shallow copy would leave every edition sharing the same
// Interaction, Autoplay and UserOverrides, which is exactly the aliasing the
// per-edition copy rule above exists to prevent.
func copyDisplayPrefs(prefs *dp1playlist.DisplayPrefs) *dp1playlist.DisplayPrefs {
	encoded, err := json.Marshal(prefs)
	if err != nil {
		return nil
	}
	var copied dp1playlist.DisplayPrefs
	if err := json.Unmarshal(encoded, &copied); err != nil {
		return nil
	}
	return &copied
}

// seriesEditions asks the catalog for one series' editions.
//
// UNRESOLVED, and deliberately left alone by the review round of 2026-10-29
// (feral-file/ffos-user#421): what a `first` device override does with an
// expanded series.
//
// The editions this returns may already carry a shared
// `provenance.contract.seriesId`. The daemon does not add it — it reads only
// `total` and `items` from the answer and never copies the series item's own
// id onto them — but the catalog passes each edition's OWN provenance through
// wholesale (`EDITION_OWN_FIELDS` in art-catalog-service's series-editions.ts),
// so an edition whose on-chain provenance names its series arrives labeled and
// nothing here strips it.
//
// That matters because ff-player#320 groups a run by exactly that key and then
// resolves the run's mode from the DEVICE override alone — expansion has
// already discarded the item's own intent — and that override is null ("auto")
// on any device a controller has not configured, which `coerceSeriesPlayback`
// reads as `first`. So a labeled run can collapse to a single edition on a
// default device, which is not what this branch set out to deliver.
//
// Three directions were considered and none is taken here: stamping the id on
// (tried and reverted — it creates the collapse where editions arrive
// unlabeled), stripping it off (rejected for now — it removes a true
// provenance field to steer another component's default), and carrying the
// publisher's `seriesPlayback` across the boundary (impossible today — no
// dp1-go release has the field, absent in v0.6.2 and v0.7.1 alike). The
// likeliest real fix is in the player's own default for an expanded run.
// Pinned by TestDP1_AnEditionsOwnSeriesIDSurvivesExpansion.
func (d *dp1) seriesEditions(ctx context.Context, playlistID string, item dp1playlist.PlaylistItem) ([]dp1playlist.PlaylistItem, error) {
	if playlistID == "" || item.ID == "" {
		return nil, fmt.Errorf("series item has no playlist id and item id to resolve editions by")
	}

	endpoint, err := buildCatalogEditionsURL(catalogBaseURL, playlistID, item.ID, seriesPageLimit)
	if err != nil {
		return nil, err
	}

	resp, err := d.httpClient.Get(endpoint)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch series editions failed: %s", resp.Status)
	}

	body, err := d.io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var answer catalogEditions
	if err := d.json.Unmarshal(body, &answer); err != nil {
		return nil, err
	}

	// The catalog has already merged each edition with its series item, so
	// its items go straight into the playlist. Only the cap is applied here:
	// one request asks for seriesPageLimit, and a series larger than that is
	// played in part rather than not at all.
	editions := answer.Items
	if len(editions) > seriesPageLimit {
		editions = editions[:seriesPageLimit]
	}
	if answer.Total > len(editions) {
		d.logger.Info("series expanded in part",
			zap.String("playlist_id", playlistID),
			zap.String("item_slug", item.Slug),
			zap.Int("editions", len(editions)),
			zap.Int("total", answer.Total))
	}
	return editions, nil
}

// buildCatalogEditionsURL is the catalog's editions route for one series
// item, validated before it is called.
func buildCatalogEditionsURL(base, playlistID, itemID string, limit int) (string, error) {
	if err := validateCatalogEndpoint(base); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/api/v1/playlists/%s/items/%s/editions?limit=%d",
		base, url.PathEscape(playlistID), url.PathEscape(itemID), limit), nil
}

// validateCatalogEndpoint validates that base is a catalog this daemon may
// read — see CATALOG_HOSTS for why the host set is closed. Matched on Host
// (so any port is rejected as a different host), never on a suffix: a suffix
// match would accept "catalog.feralfile.io.attacker.example". The scheme is
// checked as well, because the answer decides what the device displays and
// cleartext would let anyone on the path rewrite it.
func validateCatalogEndpoint(base string) error {
	parsed, err := url.Parse(base)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("invalid catalog scheme %q (https required): %s", parsed.Scheme, base)
	}
	if !CATALOG_HOSTS[parsed.Host] {
		return fmt.Errorf("invalid catalog endpoint: %s", base)
	}
	return nil
}

// isSeriesItem reports whether item names a series rather than one edition.
//
// `seriesRegistry` provenance has TWO forms in core 1.1.0 §6 and only one of
// them is a series: `contract.seriesId` with no `tokenId` is the whole series,
// while the same type WITH a `tokenId` names one specific edition of it.
// Reading the type alone treated that second form as a series, so a playlist
// of explicitly-named editions spent one catalog round trip per item — each
// answered `item-not-a-series` — and serialized that many network waits in
// front of playback for nothing (feralfile-bot F2, 2026-10-29).
//
// This is deliberately the same predicate the catalog resolves a series by
// (`isSeriesItem` in art-catalog-service's series-editions.ts). The two must
// agree: asking for the editions of something the catalog does not consider a
// series can only ever fail. That is also why a missing `seriesId` is not a
// series here — there is nothing for the catalog to resolve — even though the
// route is addressed by the playlist and item ids rather than by that id.
func isSeriesItem(item dp1playlist.PlaylistItem) bool {
	if item.Provenance == nil || item.Provenance.Type != dp1playlist.ProvenanceSeriesRegistry {
		return false
	}
	contract := item.Provenance.Contract
	return contract != nil && contract.SeriesID != nil && contract.TokenID == ""
}

func hasSeriesItems(items []dp1playlist.PlaylistItem) bool {
	for _, item := range items {
		if isSeriesItem(item) {
			return true
		}
	}
	return false
}
