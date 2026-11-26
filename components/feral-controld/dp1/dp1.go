package dp1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	goio "io"
	"maps"
	"net/http"
	"strconv"
	"strings"

	"github.com/display-protocol/dp1-go/extension/contentrating"
	"github.com/display-protocol/dp1-go/extension/playlists"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/google/uuid"
	"go.uber.org/zap"

	ffindexer "github.com/feral-file/ffos-user/components/feral-controld/ff-indexer"
	"github.com/feral-file/ffos-user/components/feral-controld/sigverify"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// ValidationPointers extracts the JSON pointers a jsonschema failure names and
// discards everything else about it.
//
// Deliberately an allow-list of one shape — the pointer after "at '" — rather
// than a redaction of known-bad substrings: anything the extractor does not
// recognize is dropped, so a future validator message cannot leak by wording
// this does not anticipate. A pointer is structural (indices and field names
// from the schema), never document content, which matters because dp1-go's
// format assertions print the offending VALUE whole and these errors are
// returned verbatim to senders on both transports.
func ValidationPointers(err error) []string {
	if err == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	// The text before the first "at '" is the validator's preamble, not a
	// location; every later part starts with a pointer. end == 0 is the
	// document root (an empty location), which is a real location (a missing top-level
	// member is reported there), so only end < 0 (no closing quote) is dropped.
	for i, part := range strings.Split(err.Error(), "at '") {
		if i == 0 {
			continue
		}
		end := strings.IndexByte(part, '\'')
		if end < 0 {
			continue
		}
		pointer := part[:end]
		// A pointer is "/a/0/b" or "" (the document root). Anything else is
		// not one, so it is not reported.
		if pointer != "" && !strings.HasPrefix(pointer, "/") {
			continue
		}
		if pointer == "" {
			pointer = "/"
		}
		if !seen[pointer] {
			seen[pointer] = true
			out = append(out, pointer)
		}
	}
	return out
}

// ErrPlaylistInvalid marks a fetched DP-1 document whose content-rating
// extension fields are present but malformed. errors.Is-able so the command
// router can carry it to the transports as the documented playlistInvalid
// classification instead of a generic failure.
var ErrPlaylistInvalid = errors.New("playlistInvalid")

// ErrUnsupportedItemSchema marks a dynamicQuery whose
// responseMapping.itemSchema names a vendor shape this build has no mapper
// for. Refused by name, before any request, rather than letting dp1-go
// validate foreign nodes as DP-1 items and fail with validation noise.
var ErrUnsupportedItemSchema = errors.New("dynamicQuery: unsupported itemSchema")

const (
	// MaxPlaylistBodyBytes bounds a playlist document fetched by URL. It is
	// deliberately LARGER than hub.MAX_REQUEST_BODY_BYTES, which still caps
	// an inline display at 4 MiB: the five Feral File series published between
	// 4.9 and 8.9 MiB are to be displayable by URL and not inline
	// (feral-file/feral-file#3597), and the two paths do not carry the same
	// worst case. The hub admits hub.MAX_INFLIGHT_REQUESTS (64) bodies at the
	// door, before any gate; a URL fetch runs inside the displayPlaylist
	// handler, behind the command gate's `heavy` policy (Weight 4 of a 16
	// budget), so at most 4 are ever in flight. Raising the inline cap would
	// multiply by 64 what raising this one multiplies by 4.
	//
	// It is read before decoding or signature verification, because both
	// scale with the body: the JSON decode into a map costs ~2.6x the body in
	// heap (measured on the 8.49 MiB document), and verification costs one
	// JCS canonicalization of the whole document — once per call, not two per
	// signatures[] entry as it did before the dp1-go version this module pins
	// (see sigverify.MaxSignatures for what that changed). An unbounded remote
	// response is reachable by any LAN caller naming a URL, so the bound stays
	// (feral-file/ffos-user#307).
	//
	// 16 MiB is headroom over the actual HTTP response, not over the
	// compact-JSON byte count the issue measured: feralfile.com serves these
	// documents formatted, and the formatting whitespace alone adds ~33% —
	// crystalline-work-sdk is 8,904,417 bytes compact but 11,854,155 bytes as
	// fetched (2026-11-05). A bound sized against the wrong figure admits
	// nothing; this one is sized against the bytes LimitReader below actually
	// counts. These documents also grow as series mint out — one of the five
	// gained 60 KiB during the investigation — and a bound that has to move
	// with every release is a bound that will be wrong on fielded devices.
	MaxPlaylistBodyBytes = 16 << 20

	DEFAULT_DURATION             = 300
	MINIMAL_PLAYLIST_ITEMS_LIMIT = 50
	MAX_PLAYLIST_ITEMS_LIMIT     = 255
	CAST_PLAYLIST_ITEMS_LIMIT    = MAX_PLAYLIST_ITEMS_LIMIT
	// Namespace UUID for generating deterministic UUIDs from token identifiers
	//nolint:gosec
	TOKEN_NAMESPACE_UUID = "8c95b1c2-4ef7-4ad9-a89a-84e410c1b4b1"

	// hydrationKeyLimit and hydrationKeyOffset must match placeholder names in dynamicQuery.query
	// (e.g. {{limit}}, {{offset}}) for spec-compliant playlists.
	hydrationKeyLimit  = "limit"
	hydrationKeyOffset = "offset"
)

// LegacyDynamicQuery is the pre-DP-1-extension shape: a single indexer endpoint plus flat string params.
//
// --- LEGACY DYNAMIC QUERIES (remove when all playlists use dynamicQuery) ---
// JSON field: "dynamicQueries" (array). Controld maps this to FFIndexer.QueryTokens (Feral GraphQL).
// Delete this type, DynamicQueries on Playlist, processDynamicPlaylistLegacy, and the legacy branch
// in ProcessDynamicPlaylist / ProcessPlaylistURL once migration is complete.
type LegacyDynamicQuery struct {
	Endpoint string            `json:"endpoint"`
	Params   map[string]string `json:"params"`
}

// Playlist is a DP-1 playlist plus optional dynamic resolution config.
type Playlist struct {
	dp1playlist.Playlist
	// LEGACY: see LegacyDynamicQuery. Omit from JSON when using spec dynamicQuery only.
	DynamicQueries []LegacyDynamicQuery `json:"dynamicQueries,omitempty"`
	// Verification is the DP-1 signature verdict computed over the document
	// bytes this playlist was decoded from (feral-file/ffos-user#307). nil
	// means "not verified": verification disabled by config, or a playlist
	// built in-process (tests, scheduler cohorts) rather than decoded from a
	// document. `json:"-"` is load-bearing: the verdict must never ride the
	// dp1_call payload to the player, the offline-cache record, or the
	// player_status re-marshal — it is reported through explicit fields only.
	// Hydration (ProcessDynamicPlaylist*) takes Playlist by value and copies
	// the struct, so a verdict attached at decode time survives it.
	Verification *sigverify.Verdict `json:"-"`
	// Extra holds top-level members the typed Playlist has no field for (a
	// later DP-1 minor's additions, extensions this build does not model), so
	// re-emitting the document does not strip them (DP-1 §12). Top level only:
	// unknown members inside items or nested objects are still dropped, since
	// items are rewritten (hydrated, narrowed, series-expanded) after decode and
	// have no stable identity to splice them back onto.
	Extra map[string]json.RawMessage `json:"-"`
}

// playlistJSON is Playlist without its methods, so UnmarshalJSON can lean on
// encoding/json's struct decoding without recursing into itself.
type playlistJSON Playlist

// UnmarshalJSON decodes the document as the plain struct and maps its items'
// onchfs sources to the device-local form (see onchfs.go).
//
// MarshalJSON re-emits unknown top-level members next to the typed ones (see
// Extra). Historically there was no MarshalJSON. One existed to carry a
// `dynamicQuery.pagination` block that dp1-go's typed DynamicQuery drops,
// for the device-side Raster wallet walk. That walk was removed
// (feral-file/feral-file#3576, section 7 of the 2026-11-23 plan): Raster now
// needs a key that only ff-source-resolver-service holds, so ff-app casts an
// address playlist as a fixed DP-1 list. A `pagination` block in an incoming
// document is now dropped on decode like any other field dp1-go does not
// model, and nothing reads it. Encoding is the plain struct's plus Extra, and MarshalJSON
// does not escape HTML itself, so each encoder's own settings apply: control's SetEscapeHTML(false) encoder writes
// `&` in a source raw where the old hook's inner json.Marshal escaped it.
// Equivalent JSON either way; nothing compares those bytes.
func (p *Playlist) UnmarshalJSON(data []byte) error {
	// dp1-go v0.8.0 added a typed Playlist.Satellites. encoding/json would
	// match any case variant ("Satellites":123) to it and fail the whole
	// decode on a document the schema accepts as an unknown property. The
	// shadow field below is shallower than the promoted one, so it claims
	// every key that folds to "satellites" as raw bytes (never failing), and
	// the real list is then read back by the exact key — the same approach as
	// dp1-go's own decodePlaylist, which controld bypasses by decoding here.
	var decoded struct {
		playlistJSON
		SatellitesRaw json.RawMessage `json:"satellites,omitempty"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*p = Playlist(decoded.playlistJSON)
	p.Satellites = decodeSatellitesExact(data)
	p.Items = mapOnchfsSources(p.Items) // see onchfs.go
	p.Extra = unknownPlaylistMembers(data)
	return nil
}

// MarshalJSON is the typed encoding plus Extra.
func (p Playlist) MarshalJSON() ([]byte, error) {
	return marshalWithExtra(playlistJSON(p), p.Extra)
}

// HasDynamicContent returns true when the playlist requests dynamic item resolution (spec or legacy).
func (p *Playlist) HasDynamicContent() bool {
	if p == nil {
		return false
	}
	return p.DynamicQuery != nil || len(p.DynamicQueries) > 0
}

//go:generate mockgen -source=dp1.go -destination=../mocks/dp1.go -package=mocks -mock_names=DP1=MockDP1
type DP1 interface {
	// ProcessPlaylistURL processes a playlist from an URL.
	// It will fetch the playlist from the URL and process it.
	// If the playlist has dynamic queries, it will hand off to
	// ProcessDynamicPlaylist with the minimal flag.
	ProcessPlaylistURL(ctx context.Context, url string, minimal bool) (*Playlist, error)

	// ProcessPlaylistURLForCast processes a playlist URL for an initial
	// displayPlaylist command. Dynamic playlists hydrate up to
	// CAST_PLAYLIST_ITEMS_LIMIT, and the whole hydrated list is returned.
	// Any seriesRegistry item in the result is already narrowed per
	// deviceMode, the same resolveSeriesCastPlan a static dp1_call display request uses
	// (feral-file/ffos-user#452) — the fetched document's own raw bytes are
	// read for each item's seriesPlayback the same way a static display request's
	// verbatim bytes are. The returned CastPlan is what the caller must pass
	// to rememberSeriesCast so a later setSeriesPlayback re-narrow has it.
	ProcessPlaylistURLForCast(ctx context.Context, url string, deviceMode *SeriesPlaybackMode) (*Playlist, CastPlan, error)

	// FetchPlaylist fetches url and decodes it, returning both the typed
	// playlist and the exact raw bytes it was decoded from (nil on any
	// error) — the same raw bytes ProcessPlaylistURLForCast already threads
	// to resolveSeriesCastPlan for a fetched document's own per-item
	// seriesPlayback. It applies NO series expansion or narrowing of any
	// kind: a dynamic playlist is returned exactly as fetched, with its
	// dynamicQuery still unresolved. For the offline-cache download path
	// (feral-file/ffos-user#455, #453), which needs the raw bytes to call
	// ResolveSeriesCastPlan itself without ProcessPlaylistURLForCast's
	// CAST_PLAYLIST_ITEMS_LIMIT cap or narrowing. Never use this for a live
	// display or a live display resolve; see ProcessPlaylistURLForCast and
	// ProcessPlaylistURL respectively.
	FetchPlaylist(ctx context.Context, url string) (*Playlist, json.RawMessage, error)

	// ProcessDynamicPlaylist processes a dynamic playlist.
	// If the playlist has dynamic queries, it will process them
	// and construct a playlist with the items.
	// Otherwise, it will return the error.
	// If minimal is true, it will only process some first items
	// and return the playlist quickly
	ProcessDynamicPlaylist(ctx context.Context, playlist Playlist, minimal bool) (*Playlist, error)

	// ProcessDynamicPlaylistForCast applies the initial-display hydration
	// policy described on ProcessPlaylistURLForCast, including the same
	// series narrowing and returned CastPlan. A dynamically resolved item has
	// no original document bytes, so its own seriesPlayback always reads as
	// absent — deviceMode and the catalog's artworkModel still apply.
	ProcessDynamicPlaylistForCast(ctx context.Context, playlist Playlist, deviceMode *SeriesPlaybackMode) (*Playlist, CastPlan, error)

	// ExpandSeriesItems replaces every seriesRegistry item in playlist with
	// the editions the catalog returns for it, bounded by maxItems (0 means
	// unbounded), ignoring seriesPlayback, any device override, and the
	// catalog's artworkModel. ProcessPlaylistURL and ProcessDynamicPlaylist
	// (the non-display variants, used by playlist-refresher's resync and the
	// offline-cache paths — feral-file/ffos-user#452's own Out of scope)
	// already apply this internally to whatever they fetch or resolve; this
	// is for a caller that already holds a decoded, static playlist it did
	// NOT get from one of those two — an inline dp1_call payload outside a
	// live display, or a playlist echoed back in player status — and so needs
	// the same expansion applied by hand before treating the playlist as
	// final. Never use this for a live display; see ResolveSeriesCastPlan.
	ExpandSeriesItems(ctx context.Context, playlist Playlist, maxItems int) Playlist

	// ResolveSeriesCastPlan is ExpandSeriesItems's display-aware sibling: it
	// reads each series item's own seriesPlayback from rawPlaylist (the
	// verbatim bytes playlist was decoded from; nil when none exists, e.g. an
	// internally rebuilt re-display), resolves a per-item mode against
	// deviceMode (the device-level override; nil follows the document), and
	// returns a CastPlan that narrows to exactly that mode's editions while
	// keeping every group's full edition set for a later, network-free
	// re-narrow (feral-file/ffos-user#425). maxItems bounds the result the
	// same way it bounds ExpandSeriesItems.
	ResolveSeriesCastPlan(ctx context.Context, playlist Playlist, rawPlaylist json.RawMessage, deviceMode *SeriesPlaybackMode, maxItems int) CastPlan
}

type dp1 struct {
	ffIndexer  ffindexer.FFIndexer
	httpClient wrapper.HTTPClient
	json       wrapper.JSON
	io         wrapper.IO
	logger     *zap.Logger
	// debug mirrors controld --debug: relaxes dp1-go dynamicQuery endpoint policy (http:// and non-public hosts).
	debug bool
	// verify gates signature verification of fetched documents (see
	// Playlist.Verification). Off only via config.SignatureVerificationConfig.
	verify bool
}

func New(ffIndexer ffindexer.FFIndexer, httpClient wrapper.HTTPClient, json wrapper.JSON, io wrapper.IO, logger *zap.Logger, debug, verify bool) DP1 {
	return &dp1{
		ffIndexer:  ffIndexer,
		httpClient: httpClient,
		json:       json,
		io:         io,
		logger:     logger,
		debug:      debug,
		verify:     verify,
	}
}

func (d *dp1) ProcessPlaylistURL(ctx context.Context, url string, minimal bool) (*Playlist, error) {
	d.logger.Info("Processing playlist from URL", zap.String("url", url))

	playlist, _, err := d.fetchPlaylist(url)
	if err != nil {
		return nil, err
	}

	if playlist.HasDynamicContent() {
		return d.ProcessDynamicPlaylist(ctx, playlist, minimal)
	}

	// A curated playlist can still name series rather than editions.
	resolved := d.expandSeriesItems(ctx, playlist, 0)
	return &resolved, nil
}

// ProcessPlaylistURLForCast fetches url and narrows any seriesRegistry item
// it carries the same way a static dp1_call display request already does
// (resolveSeriesCastPlan, gated by seriesPlayback / deviceMode / the
// catalog's artworkModel) — unlike the pre-#452 behavior, which expanded a
// fetched document's series into every edition regardless of mode. The
// returned CastPlan is what the caller must pass to rememberSeriesCast, so
// a later setSeriesPlayback re-narrow has it for THIS display too, not only a
// static one.
func (d *dp1) ProcessPlaylistURLForCast(ctx context.Context, url string, deviceMode *SeriesPlaybackMode) (*Playlist, CastPlan, error) {
	d.logger.Info("Processing playlist from URL for initial display request", zap.String("url", url))

	playlist, rawPlaylist, err := d.fetchPlaylist(url)
	if err != nil {
		return nil, CastPlan{}, err
	}

	if playlist.HasDynamicContent() {
		return d.ProcessDynamicPlaylistForCast(ctx, playlist, deviceMode)
	}

	// Bounded like the dynamic display path: a display request carries at most
	// CAST_PLAYLIST_ITEMS_LIMIT items however many editions a series holds.
	plan := d.resolveSeriesCastPlan(ctx, playlist, rawPlaylist, deviceMode, CAST_PLAYLIST_ITEMS_LIMIT)
	narrowed := playlist
	narrowed.Items = plan.Narrow(deviceMode)
	return &narrowed, plan, nil
}

// FetchPlaylist is ctx-taking only for interface-signature symmetry with
// this package's other exported resolvers; fetchPlaylist itself is not
// context-aware (see its own doc on the shared client's fixed timeout).
func (d *dp1) FetchPlaylist(ctx context.Context, url string) (*Playlist, json.RawMessage, error) {
	playlist, rawPlaylist, err := d.fetchPlaylist(url)
	if err != nil {
		return nil, nil, err
	}
	return &playlist, rawPlaylist, nil
}

func (d *dp1) ProcessDynamicPlaylist(ctx context.Context, playlist Playlist, minimal bool) (*Playlist, error) {
	d.logger.Info("Processing dynamic playlist", zap.String("playlist_id", playlist.ID))

	resolved, err := d.processDynamicPlaylist(ctx, playlist, minimal, 0)
	if err != nil {
		return nil, err
	}

	// A dynamically-resolved item can itself name a series: the core
	// schema's dynamicQuery/legacy answer is not restricted to onChain
	// items, only buildPlaylistItem's own legacy construction happens to
	// always produce one. Expand it the same way the curated (non-dynamic)
	// branch of ProcessPlaylistURL already does, rather than only there.
	expanded := d.expandSeriesItems(ctx, *resolved, 0)
	return &expanded, nil
}

// ProcessDynamicPlaylistForCast resolves playlist's dynamicQuery and narrows
// any seriesRegistry item among the resolved items, the same resolution
// ProcessPlaylistURLForCast uses (feral-file/ffos-user#452). A dynamically
// resolved item is synthesized at runtime — it has no original document
// bytes of its own — so there is no per-item seriesPlayback to read for it
// (rawPlaylist is nil; see readItemSeriesPlaybackModes' own doc on a nil raw
// document). The device-level override and the catalog's artworkModel gate
// still apply.
func (d *dp1) ProcessDynamicPlaylistForCast(ctx context.Context, playlist Playlist, deviceMode *SeriesPlaybackMode) (*Playlist, CastPlan, error) {
	resolved, err := d.processDynamicPlaylist(ctx, playlist, false, CAST_PLAYLIST_ITEMS_LIMIT)
	if err != nil {
		return nil, CastPlan{}, err
	}

	// Bounded by the same display request cap the batch accumulation inside
	// processDynamicPlaylist already respects — narrowing can only shrink
	// the item count, but CastPlan.Narrow re-applies the cap anyway so this
	// stays correct if that ever changes.
	plan := d.resolveSeriesCastPlan(ctx, *resolved, nil, deviceMode, CAST_PLAYLIST_ITEMS_LIMIT)
	narrowed := *resolved
	narrowed.Items = plan.Narrow(deviceMode)

	// The display request carries the whole resolved list (up to
	// CAST_PLAYLIST_ITEMS_LIMIT): for a playlist within that cap and without
	// series items, the same list the playlist refresher resolves a moment
	// later. (The refresher resolves uncapped and expands series without
	// narrowing, so a longer list, or one carrying series under a non-full
	// mode, still changes on its first refresh; those gaps are known and
	// separate.) It used to be trimmed to the first
	// MINIMAL_PLAYLIST_ITEMS_LIMIT items, a remnant of the first-page-only
	// resolve this path once did to put a wall up quickly; by then the whole
	// list had already been fetched, so the trim saved nothing, and the
	// refresher sent the rest about a second later. What it cost: a startItem
	// naming a work past the cut opened the display at item 0, and the player
	// briefly held a list other than the one status was about to report
	// (feral-file/feral-file#3636). Do not reintroduce a cut here that the
	// refresher does not apply to the same playlist.
	//
	// plan keeps the cap it was resolved under, which bounds the list shown
	// here, so a later setSeriesPlayback re-narrow cannot grow past what this
	// display request could show (PR #454 review F1/F2).
	return &narrowed, plan, nil
}

// processDynamicPlaylist resolves the playlist's dynamic query. Every item it
// returns already carries its device-local onchfs source (onchfs.go): DP-1
// hydration and the legacy indexer both exit here.
func (d *dp1) processDynamicPlaylist(ctx context.Context, playlist Playlist, minimal bool, maxItems int) (*Playlist, error) {
	resolved, err := d.resolveDynamicPlaylist(ctx, playlist, minimal, maxItems)
	if err != nil {
		return nil, err
	}
	resolved.Items = mapOnchfsSources(resolved.Items)
	return resolved, nil
}

func (d *dp1) resolveDynamicPlaylist(ctx context.Context, playlist Playlist, minimal bool, maxItems int) (*Playlist, error) {
	if playlist.DynamicQuery != nil {
		if len(playlist.DynamicQueries) > 0 {
			d.logger.Warn("playlist has both dynamicQuery and legacy dynamicQueries; using dynamicQuery only",
				zap.String("playlist_id", playlist.ID))
		}
		return d.processDynamicPlaylistSpec(ctx, playlist, minimal, maxItems)
	}

	if len(playlist.DynamicQueries) > 0 {
		return d.processDynamicPlaylistLegacy(ctx, playlist, minimal, maxItems)
	}

	return nil, fmt.Errorf("playlist has no dynamic query configuration")
}

// processDynamicPlaylistSpec resolves items using DP-1 playlists extension dynamicQuery and
// github.com/display-protocol/dp1-go PlaylistItemsFromDynamicQuery. Pagination uses {{limit}} and
// {{offset}} placeholders in dynamicQuery.query, matching MINIMAL/MAX batch sizes used by legacy.
//
// Replacement behavior matches legacy: playlist.Items is replaced by resolved dynamic items only (no merge with static items).
//
// Invalid items: dp1-go validates every hydrated item against the core
// PlaylistItem schema plus the playlists and content-rating overlays (an
// item's inlineManifest included) and, since v0.9.0, DISCARDS one that fails
// instead of failing the call (playlists extension §4.6.2). The valid items are
// displayed; the discarded count and the first failure's JSON pointers are
// logged, because neither the sender nor the wall is told the list is shorter.
// Two things stay controld's job:
//
//   - dp1-go's default report prints the validator's message, which quotes the
//     offending value whole, so OnInvalidItem is always set and only pointers
//     leave it (see ValidationPointers).
//   - A response with items but not one valid among them is still refused as
//     playlistInvalid: an empty playlist would be answered "ok, deferred" by
//     the display path and leave the sender believing it was accepted.
//
// The resolved items alone no longer say how long a page was, so paging counts
// the discarded ones too; otherwise one bad item on a full page would drop every
// later page. A page with no valid item ends the walk, which bounds a resolver
// that ignores {{offset}} and keeps answering the same broken page.
//
// Pinned by TestDP1_ProcessDynamicPlaylist_SpecDynamicQuery_InlineManifestValidation
// and TestDP1_ProcessDynamicPlaylist_SpecDynamicQuery_DiscardedItemKeepsPaging.
//
// The resolver is chosen by responseMapping.itemSchema. `dp1/*` (and an
// absent schema, for documents published before anything checked it) is the
// dp1-go path above. Any other schema is refused by name before any request.
// That includes `raster/tokensOwned/1.0`, the Raster wallet walk ff-app used
// to send for an address playlist: the device has no Raster key and must not
// carry one, so the walk was removed and the app casts a fixed DP-1 list
// instead (feral-file/feral-file#3576).
func (d *dp1) processDynamicPlaylistSpec(ctx context.Context, playlist Playlist, minimal bool, maxItems int) (*Playlist, error) {
	if playlist.DynamicQuery == nil {
		return nil, fmt.Errorf("internal: dynamicQuery is nil")
	}

	switch schema := playlist.DynamicQuery.ResponseMapping.ItemSchema; {
	case schema == "" || strings.HasPrefix(schema, "dp1/"):
		// dp1-go hydration below.
	default:
		return nil, fmt.Errorf("%w %q", ErrUnsupportedItemSchema, schema)
	}

	client := d.httpClientAsHTTPClient()
	limit := MAX_PLAYLIST_ITEMS_LIMIT
	if minimal {
		limit = MINIMAL_PLAYLIST_ITEMS_LIMIT
	}

	var accumulated []dp1playlist.PlaylistItem
	offset := 0
	discarded := 0
	var firstDiscarded error
	firstDiscardedIndex := 0
	for {
		discardedInPage := 0
		params := dp1playlist.HydrationParams{
			hydrationKeyLimit:  strconv.Itoa(limit),
			hydrationKeyOffset: strconv.Itoa(offset),
		}

		batch, err := dp1playlist.PlaylistItemsFromDynamicQuery(
			ctx,
			playlist.DynamicQuery,
			params,
			client,
			&dp1playlist.DynamicQueryFetchOptions{
				AllowInsecureHTTP: d.debug,
				OnInvalidItem: func(index int, itemErr error) {
					if firstDiscarded == nil {
						firstDiscarded, firstDiscardedIndex = itemErr, offset+index
					}
					discardedInPage++
				},
			})
		if err != nil {
			return nil, err
		}
		discarded += discardedInPage
		accumulated = append(accumulated, batch...)
		if maxItems > 0 && len(accumulated) >= maxItems {
			accumulated = accumulated[:maxItems]
			break
		}

		if len(batch) == 0 || len(batch)+discardedInPage < limit {
			break
		}
		if minimal && len(accumulated) >= MINIMAL_PLAYLIST_ITEMS_LIMIT {
			break
		}
		offset += limit
	}

	if discarded > 0 {
		pointers := ValidationPointers(firstDiscarded)
		d.logger.Warn("Discarded invalid dynamicQuery items",
			zap.String("playlist_id", playlist.ID),
			zap.Int("discarded", discarded),
			zap.Int("valid", len(accumulated)),
			zap.Int("first_index", firstDiscardedIndex),
			zap.Strings("first_pointers", pointers))
		if len(accumulated) == 0 {
			return nil, fmt.Errorf("%w: dynamic item at %s", ErrPlaylistInvalid, strings.Join(pointers, ", "))
		}
	}

	playlist.Items = accumulated
	return &playlist, nil
}

// processDynamicPlaylistLegacy uses FFIndexer.QueryTokens against the Feral indexer GraphQL tokens(...) API.
//
// --- LEGACY REMOVAL ---
// Delete this function when "dynamicQueries" is no longer published. Depends on: LegacyDynamicQuery,
// DynamicQueries field, ffIndexer.QueryTokens loop, buildPlaylistItems from Token.
func (d *dp1) processDynamicPlaylistLegacy(ctx context.Context, playlist Playlist, minimal bool, maxItems int) (*Playlist, error) {
	if len(playlist.DynamicQueries) != 1 {
		return nil, fmt.Errorf("playlist should have exactly 1 dynamic queries, but has %d", len(playlist.DynamicQueries))
	}

	originalQuery := playlist.DynamicQueries[0]
	dynamicQuery := LegacyDynamicQuery{
		Endpoint: originalQuery.Endpoint,
		Params:   make(map[string]string),
	}
	// Copy the original params
	maps.Copy(dynamicQuery.Params, originalQuery.Params)

	var ffTokens []ffindexer.Token
	limit := MAX_PLAYLIST_ITEMS_LIMIT
	if minimal {
		limit = MINIMAL_PLAYLIST_ITEMS_LIMIT
	}
	dynamicQuery.Params["limit"] = fmt.Sprintf("%d", limit)
	offset := 0

	for {
		dynamicQuery.Params["offset"] = fmt.Sprintf("%d", offset)
		tokens, err := d.ffIndexer.QueryTokens(ctx, dynamicQuery.Endpoint, dynamicQuery.Params)
		if err != nil {
			return nil, err
		}
		ffTokens = append(ffTokens, tokens...)
		if maxItems > 0 && len(ffTokens) >= maxItems {
			ffTokens = ffTokens[:maxItems]
			break
		}

		if len(tokens) < limit || (minimal && len(ffTokens) >= MINIMAL_PLAYLIST_ITEMS_LIMIT) {
			break
		}

		offset += limit
	}

	duration := defaultDurationSeconds(playlist)
	newItems := buildPlaylistItems(duration, ffTokens)

	// Always replace the new items to keep the playlist up to date.
	// FIXME: This line will ignore the case that playlist has both curated and dynamic items.
	playlist.Items = newItems

	return &playlist, nil
}

func defaultDurationSeconds(playlist Playlist) float64 {
	duration := float64(DEFAULT_DURATION)
	if playlist.Defaults != nil && playlist.Defaults.Duration != nil && *playlist.Defaults.Duration > 0 {
		duration = *playlist.Defaults.Duration
	}
	return duration
}

// fetchPlaylist also returns the exact bytes the document was decoded from
// (nil on any error), the same way commandrouter/handler.go already keeps
// command.RawArgument("dp1_call") for a static dp1_call display request: a fetched
// document can carry its own per-item seriesPlayback just as an inline one
// can, and resolveSeriesCastPlan needs those raw bytes, not the typed
// struct, to read it (feral-file/ffos-user#452).
func (d *dp1) fetchPlaylist(url string) (Playlist, json.RawMessage, error) {
	d.logger.Info("Fetching playlist from URL", zap.String("url", url))
	resp, err := d.httpClient.Get(url)
	if err != nil {
		return Playlist{}, nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Playlist{}, nil, fmt.Errorf("fetch playlist failed: %s", resp.Status)
	}

	// One byte past the cap is read on purpose: a body of exactly the cap is
	// admitted, and the extra byte is what proves the response was larger.
	bytes, err := d.io.ReadAll(goio.LimitReader(resp.Body, MaxPlaylistBodyBytes+1))
	if err != nil {
		return Playlist{}, nil, err
	}
	if len(bytes) > MaxPlaylistBodyBytes {
		return Playlist{}, nil, fmt.Errorf("fetch playlist failed: body exceeds %d bytes", MaxPlaylistBodyBytes)
	}
	// This ingestion path structurally validates any present content-rating
	// extension fields. The fragment validator needs no core signature, so it
	// applies independently of the document's signature verdict. Ordered
	// after the size cap so an oversized body is
	// refused before it is parsed.
	//
	// What this rejects is a rating of the wrong TYPE. An unrecognized rating
	// STRING is not an error anywhere on this path: DP-1 §3.3
	// (display-protocol/dp1#52) treats a rating this build does not know as
	// unrated, the schema accepts any string, and contentpolicy.Policy.Allows
	// applies the same rule at admission. A document is never refused for
	// carrying a label we do not recognize.
	if err := contentrating.ValidatePlaylistFragment(bytes); err != nil {
		return Playlist{}, nil, fmt.Errorf("%w: content rating extension at %s", ErrPlaylistInvalid, strings.Join(ValidationPointers(err), ", "))
	}

	AuditDocumentAsync(d.logger, "playlistUrl", bytes)

	var playlist Playlist
	err = d.json.Unmarshal(bytes, &playlist)
	if err != nil {
		return Playlist{}, nil, err
	}

	// Verify the bytes as received, before anything reshapes the document:
	// the typed unmarshal above may drop fields the signer covered, and the
	// dynamic hydration every caller runs next rewrites items. Either would
	// turn an honest signature into a payload_hash mismatch.
	if d.verify {
		verdict := sigverify.Verify(bytes)
		playlist.Verification = &verdict
	}

	return playlist, json.RawMessage(bytes), nil
}

func buildPlaylistItems(duration float64, tokens []ffindexer.Token) []dp1playlist.PlaylistItem {
	items := make([]dp1playlist.PlaylistItem, 0, len(tokens))
	for _, token := range tokens {
		items = append(items, buildPlaylistItem(duration, token))
	}
	return items
}

func buildPlaylistItem(duration float64, token ffindexer.Token) dp1playlist.PlaylistItem {
	title := token.GetName()
	previewURL := token.GetPreviewURL()
	chain := normalizeChain(token.Chain)

	itemID := generateTokenUUID(token.ContractAddress, token.Chain, token.TokenNumber)
	dur := duration

	return dp1playlist.PlaylistItem{
		ID:       itemID,
		Title:    title,
		Source:   previewURL,
		Duration: &dur,
		Provenance: &dp1playlist.ProvenanceBlock{
			Type: dp1playlist.ProvenanceOnChain,
			Contract: &dp1playlist.ProvenanceContract{
				Chain:    chain,
				Standard: token.Standard,
				Address:  token.ContractAddress,
				TokenID:  token.TokenNumber,
			},
		},
	}
}

// generateTokenUUID creates a deterministic UUID from contractAddress, blockchain, and tokenNumber.
// This ensures the same token always gets the same UUID.
func generateTokenUUID(contractAddress, blockchain, tokenNumber string) string {
	namespace := uuid.MustParse(TOKEN_NAMESPACE_UUID)
	identifier := fmt.Sprintf("%s:%s:%s", contractAddress, blockchain, tokenNumber)
	return uuid.NewSHA1(namespace, []byte(identifier)).String()
}

func normalizeChain(blockchain string) string {
	b := strings.ToLower(strings.TrimSpace(blockchain))

	// Support CAIP-2 format from ff-indexer-v2
	if strings.HasPrefix(b, "eip155:") {
		return "evm"
	}
	if strings.HasPrefix(b, "tezos:") {
		return "tezos"
	}
	return "other"
}

// httpClientTransport adapts wrapper.HTTPClient to http.RoundTripper for *http.Client used by dp1-go.
type httpClientTransport struct {
	wrapper.HTTPClient
}

func (t httpClientTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.Do(req)
}

// httpClientAsHTTPClient creates an http.Client compatible with dp1-go while preserving the timeout
// configured by wrapper.NewHTTPClient. Without an explicit timeout, slow or stalled dynamicQuery
// endpoints would hang playlist hydration indefinitely (callers typically use long-lived contexts).
func (d *dp1) httpClientAsHTTPClient() *http.Client {
	return &http.Client{
		Transport: httpClientTransport{d.httpClient},
		Timeout:   wrapper.HTTPClientTimeout,
	}
}

// decodeSatellitesExact reads "satellites" by its exact JSON key, leniently: a
// value of the wrong shape decodes as absent rather than failing the playlist.
func decodeSatellitesExact(data []byte) []playlists.Satellite {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil
	}
	raw, ok := top["satellites"]
	if !ok {
		return nil
	}
	var satellites []playlists.Satellite
	if err := json.Unmarshal(raw, &satellites); err != nil {
		return nil
	}
	return satellites
}
