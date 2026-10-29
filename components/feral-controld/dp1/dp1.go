package dp1

import (
	"bytes"
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
// returned verbatim to casters on both transports.
func ValidationPointers(err error) []string {
	if err == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(err.Error(), "at '") {
		end := strings.IndexByte(part, '\'')
		if end <= 0 {
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
	// MaxPlaylistBodyBytes bounds a playlist document fetched by URL, matching
	// hub.MAX_REQUEST_BODY_BYTES for inline casts so both ingress paths admit
	// the same worst-case document. It is read before decoding or signature
	// verification: every signatures[] entry costs two JCS canonicalizations
	// of the whole body, so an unbounded remote response — reachable by any
	// LAN caller naming a URL — would otherwise buy CPU and memory the inline
	// cap deliberately denies (feral-file/ffos-user#307).
	MaxPlaylistBodyBytes = 4 << 20

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

// DynamicQueryPagination is the `dynamicQuery.pagination` block: cursor
// paging for a resolver that cannot page by `{{limit}}`/`{{offset}}`
// placeholders. The next page sends the value read at CursorPath as GraphQL
// variable Variable, and the walk continues while the boolean at HasNextPath
// is true. Today only the Raster wallet walk (raster.go) reads it.
type DynamicQueryPagination struct {
	Variable    string `json:"variable"`
	CursorPath  string `json:"cursorPath"`
	HasNextPath string `json:"hasNextPath"`
}

// Playlist is a DP-1 playlist plus optional dynamic resolution config.
type Playlist struct {
	dp1playlist.Playlist
	// LEGACY: see LegacyDynamicQuery. Omit from JSON when using spec dynamicQuery only.
	DynamicQueries []LegacyDynamicQuery `json:"dynamicQueries,omitempty"`
	// DynamicQueryPagination is `dynamicQuery.pagination`, held beside
	// DynamicQuery because dp1-go's typed playlists.DynamicQuery has no such
	// field and a plain decode would silently drop it. It is carried by the
	// MarshalJSON/UnmarshalJSON hooks below, NOT by a struct tag: it must
	// survive the round trip through the player (the dp1_call payload comes
	// back in player_status, and the refresher re-resolves the dynamic source
	// from that copy), so it is written back INSIDE the `dynamicQuery` object
	// on marshal, where it came from, never as a sibling key the document's
	// signer did not cover. nil when the document carries none.
	DynamicQueryPagination *DynamicQueryPagination `json:"-"`
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
}

// playlistJSON is Playlist without its methods, so the hooks below can lean on
// encoding/json's struct decoding without recursing into themselves.
type playlistJSON Playlist

// UnmarshalJSON decodes the document as the plain struct, then peeks
// `dynamicQuery.pagination` out of the same bytes (see
// DynamicQueryPagination on Playlist for why it cannot be a struct field).
// A `pagination` that is present but not an object is left nil rather than
// failing the decode: a document that is not a Raster walk is not refused for
// carrying one, and a Raster walk without a usable block is refused later by
// name, with a clearer error than a decode failure would give.
func (p *Playlist) UnmarshalJSON(data []byte) error {
	var decoded playlistJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var peek struct {
		DynamicQuery *struct {
			Pagination json.RawMessage `json:"pagination"`
		} `json:"dynamicQuery"`
	}
	// The plain decode above already typed dynamicQuery as an object (or
	// failed), so this second pass can only fail on `pagination` itself.
	if err := json.Unmarshal(data, &peek); err == nil && peek.DynamicQuery != nil {
		var pagination DynamicQueryPagination
		if json.Unmarshal(peek.DynamicQuery.Pagination, &pagination) == nil &&
			bytes.HasPrefix(bytes.TrimSpace(peek.DynamicQuery.Pagination), []byte("{")) {
			decoded.DynamicQueryPagination = &pagination
		}
	}
	*p = Playlist(decoded)
	return nil
}

// MarshalJSON is the plain encoding, plus `pagination` written back inside
// the `dynamicQuery` object when the playlist carries one. A playlist without
// pagination encodes exactly as the plain struct does, byte for byte, so
// nothing that is not a Raster walk changes shape on the wire.
func (p Playlist) MarshalJSON() ([]byte, error) {
	out, err := json.Marshal(playlistJSON(p))
	if err != nil || p.DynamicQueryPagination == nil || p.DynamicQuery == nil {
		return out, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		return nil, err
	}
	var dq map[string]json.RawMessage
	if err := json.Unmarshal(top["dynamicQuery"], &dq); err != nil {
		return nil, err
	}
	pagination, err := json.Marshal(p.DynamicQueryPagination)
	if err != nil {
		return nil, err
	}
	dq["pagination"] = pagination
	if top["dynamicQuery"], err = json.Marshal(dq); err != nil {
		return nil, err
	}
	return json.Marshal(top)
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
	// displayPlaylist command. Dynamic playlists hydrate only to a bounded
	// scheduling cap; if no item-level displayAt is discovered, the returned
	// playlist is trimmed back to the legacy minimal size before sending to the
	// player.
	ProcessPlaylistURLForCast(ctx context.Context, url string) (*Playlist, error)

	// ProcessDynamicPlaylist processes a dynamic playlist.
	// If the playlist has dynamic queries, it will process them
	// and construct a playlist with the items.
	// Otherwise, it will return the error.
	// If minimal is true, it will only process some first items
	// and return the playlist quickly
	ProcessDynamicPlaylist(ctx context.Context, playlist Playlist, minimal bool) (*Playlist, error)

	// ProcessDynamicPlaylistForCast applies the initial-cast bounded hydration
	// policy described on ProcessPlaylistURLForCast.
	ProcessDynamicPlaylistForCast(ctx context.Context, playlist Playlist) (*Playlist, error)

	// ExpandSeriesItems replaces every seriesRegistry item in playlist with
	// the editions the catalog returns for it, bounded by maxItems (0 means
	// unbounded). ProcessPlaylistURL(ForCast) and ProcessDynamicPlaylist(ForCast)
	// already apply this internally to whatever they fetch or resolve; this
	// is for a caller that already holds a decoded, static playlist it did
	// NOT get from one of those four — an inline dp1_call payload, or a
	// playlist echoed back in player status — and so needs the same
	// expansion applied by hand before treating the playlist as final.
	ExpandSeriesItems(ctx context.Context, playlist Playlist, maxItems int) Playlist
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
	// rasterAPIKey is the device's own Raster read key (config `raster.apiKey`),
	// sent as x-api-key on every Raster wallet-walk request. It is never
	// logged and never read from a document; empty means unauthenticated.
	rasterAPIKey string
	// rasterHTTP carries the key, so it MUST NOT follow redirects: the host
	// allowlist checks only the URL the document names, and a 3xx from that
	// host would otherwise carry x-api-key wherever Location points (Go
	// strips Authorization and Cookie across hosts, not custom headers).
	// Build it with NewRasterHTTPClient; fetchRasterPage additionally refuses
	// a response whose request moved. Separate from httpClient because the
	// rest of the daemon relies on redirects being followed.
	rasterHTTP wrapper.HTTPClient
}

func New(ffIndexer ffindexer.FFIndexer, httpClient, rasterHTTP wrapper.HTTPClient, json wrapper.JSON, io wrapper.IO, logger *zap.Logger, debug, verify bool, rasterAPIKey string) DP1 {
	return &dp1{
		ffIndexer:    ffIndexer,
		httpClient:   httpClient,
		rasterHTTP:   rasterHTTP,
		json:         json,
		io:           io,
		logger:       logger,
		debug:        debug,
		verify:       verify,
		rasterAPIKey: rasterAPIKey,
	}
}

func (d *dp1) ProcessPlaylistURL(ctx context.Context, url string, minimal bool) (*Playlist, error) {
	d.logger.Info("Processing playlist from URL", zap.String("url", url))

	playlist, err := d.fetchPlaylist(url)
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

func (d *dp1) ProcessPlaylistURLForCast(ctx context.Context, url string) (*Playlist, error) {
	d.logger.Info("Processing playlist from URL for initial cast", zap.String("url", url))

	playlist, err := d.fetchPlaylist(url)
	if err != nil {
		return nil, err
	}

	if playlist.HasDynamicContent() {
		return d.ProcessDynamicPlaylistForCast(ctx, playlist)
	}

	// Bounded like the dynamic cast path: a cast carries at most
	// CAST_PLAYLIST_ITEMS_LIMIT items however many editions a series holds.
	resolved := d.expandSeriesItems(ctx, playlist, CAST_PLAYLIST_ITEMS_LIMIT)
	return &resolved, nil
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

func (d *dp1) ProcessDynamicPlaylistForCast(ctx context.Context, playlist Playlist) (*Playlist, error) {
	resolved, err := d.processDynamicPlaylist(ctx, playlist, false, CAST_PLAYLIST_ITEMS_LIMIT)
	if err != nil {
		return nil, err
	}

	// Same series expansion as above, bounded by the same cast cap the
	// batch accumulation inside processDynamicPlaylist already respects —
	// expanding can only grow the item count further, so the cap is
	// re-applied here rather than assumed to still hold.
	expanded := d.expandSeriesItems(ctx, *resolved, CAST_PLAYLIST_ITEMS_LIMIT)

	if hasDisplayAtItems(expanded.Items) || len(expanded.Items) <= MINIMAL_PLAYLIST_ITEMS_LIMIT {
		return &expanded, nil
	}
	expanded.Items = expanded.Items[:MINIMAL_PLAYLIST_ITEMS_LIMIT]
	return &expanded, nil
}

func (d *dp1) processDynamicPlaylist(ctx context.Context, playlist Playlist, minimal bool, maxItems int) (*Playlist, error) {
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

// dp1ItemValidationMarker is the prefix dp1-go puts on a hydrated item that
// fails its schema validation, content-rating overlay included.
//
// Matched as a STRING deliberately. dp1-go's validation sentinel
// (dp1go.ErrValidation) lives in the module's ROOT package, and that package
// imports its signing path: adding it as a direct import pulls go-ethereum,
// gnark-crypto and ~30 related packages into this daemon and grows the binary
// by ~1.4 MB (measured: 28.4 MB -> 29.8 MB), on a device where nothing else
// needs any of it. That is a poor trade for classifying an error.
//
// The fragility is bounded and fails in the safe direction: if dp1-go's wording
// changes, a malformed item falls back to the generic error this code returned
// before, never to a wrong classification. TestDP1_ProcessDynamicPlaylist_
// RejectsWrongTypedResolvedRatings pins the behavior.
const dp1ItemValidationMarker = "invalid playlist item"

// classifyHydrationError marks a resolver's malformed CONTENT as
// playlistInvalid so the transports answer the documented classification;
// transport and query failures stay generic.
func classifyHydrationError(err error) error {
	if err == nil || !strings.Contains(err.Error(), dp1ItemValidationMarker) {
		return err
	}
	return fmt.Errorf("%w: dynamic item at %s", ErrPlaylistInvalid, strings.Join(ValidationPointers(err), ", "))
}

func hasDisplayAtItems(items []dp1playlist.PlaylistItem) bool {
	for _, item := range items {
		if item.DisplayAt != nil {
			return true
		}
	}
	return false
}

// processDynamicPlaylistSpec resolves items using DP-1 playlists extension dynamicQuery and
// github.com/display-protocol/dp1-go PlaylistItemsFromDynamicQuery. Pagination uses {{limit}} and
// {{offset}} placeholders in dynamicQuery.query, matching MINIMAL/MAX batch sizes used by legacy.
//
// Replacement behavior matches legacy: playlist.Items is replaced by resolved dynamic items only (no merge with static items).
//
// ACCEPTED FAIL-CLOSED: dp1-go validates every hydrated item against the core
// PlaylistItem schema plus the playlists-extension overlay, and its hydration
// loop RETURNS on the first item that fails rather than skipping it — so one
// malformed item fails the whole cast, and the error surfaces to the caller
// (commandrouter returns it; the dp1_call branch has no cached fallback, only
// the playlistUrl branch does). This is deliberate, but note it widened at
// dp1-go v0.6.0: the single-item overlay now $refs the ref-manifest schema, so
// an item's inlineManifest (§3.6) is checked too, where before it was an
// undescribed key that passed unexamined. A resolver that starts emitting a
// slightly non-conforming manifest therefore takes the whole playlist down,
// not just its own item.
//
// Kept rather than softened, for two reasons. Skipping bad items would put a
// silently shorter playlist on the wall — the controller asked for a playlist
// and would be told it succeeded, with no signal that items went missing,
// which is the worse failure for something whose whole job is showing a
// specific set of works. And dp1-go owns the fetch/validate/decode loop, so
// per-item recovery would mean reimplementing hydration here to get back a
// behavior the spec does not ask for.
//
// Pinned by TestDP1_ProcessDynamicPlaylist_SpecDynamicQuery_InlineManifestValidation,
// which asserts both directions so a future change cannot quietly flip either.
//
// The resolver is chosen by responseMapping.itemSchema. `dp1/*` (and an
// absent schema, for documents published before anything checked it) is the
// dp1-go path above. `raster/tokensOwned/1.0` is the Raster wallet walk in
// raster.go, which pages by cursor and maps vendor nodes to items itself; it
// is the one place that departs from the fail-closed rule, per node, and
// says so on processRasterWalk. Any other schema is refused by name.
func (d *dp1) processDynamicPlaylistSpec(ctx context.Context, playlist Playlist, minimal bool, maxItems int) (*Playlist, error) {
	if playlist.DynamicQuery == nil {
		return nil, fmt.Errorf("internal: dynamicQuery is nil")
	}

	switch schema := playlist.DynamicQuery.ResponseMapping.ItemSchema; {
	case schema == "" || strings.HasPrefix(schema, "dp1/"):
		// dp1-go hydration below.
	case schema == RasterTokensOwnedItemSchema:
		return d.processRasterWalk(ctx, playlist, minimal, maxItems)
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
	for {
		params := dp1playlist.HydrationParams{
			hydrationKeyLimit:  strconv.Itoa(limit),
			hydrationKeyOffset: strconv.Itoa(offset),
		}

		batch, err := dp1playlist.PlaylistItemsFromDynamicQuery(
			ctx,
			playlist.DynamicQuery,
			params,
			client,
			&dp1playlist.DynamicQueryFetchOptions{AllowInsecureHTTP: d.debug})
		if err != nil {
			return nil, classifyHydrationError(err)
		}
		accumulated = append(accumulated, batch...)
		if maxItems > 0 && len(accumulated) >= maxItems {
			accumulated = accumulated[:maxItems]
			break
		}

		if len(batch) < limit {
			break
		}
		if minimal && len(accumulated) >= MINIMAL_PLAYLIST_ITEMS_LIMIT {
			break
		}
		offset += limit
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

func (d *dp1) fetchPlaylist(url string) (Playlist, error) {
	d.logger.Info("Fetching playlist from URL", zap.String("url", url))
	resp, err := d.httpClient.Get(url)
	if err != nil {
		return Playlist{}, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Playlist{}, fmt.Errorf("fetch playlist failed: %s", resp.Status)
	}

	// One byte past the cap is read on purpose: a body of exactly the cap is
	// admitted, and the extra byte is what proves the response was larger.
	bytes, err := d.io.ReadAll(goio.LimitReader(resp.Body, MaxPlaylistBodyBytes+1))
	if err != nil {
		return Playlist{}, err
	}
	if len(bytes) > MaxPlaylistBodyBytes {
		return Playlist{}, fmt.Errorf("fetch playlist failed: body exceeds %d bytes", MaxPlaylistBodyBytes)
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
		return Playlist{}, fmt.Errorf("%w: content rating extension at %s", ErrPlaylistInvalid, strings.Join(ValidationPointers(err), ", "))
	}

	var playlist Playlist
	err = d.json.Unmarshal(bytes, &playlist)
	if err != nil {
		return Playlist{}, err
	}

	// Verify the bytes as received, before anything reshapes the document:
	// the typed unmarshal above may drop fields the signer covered, and the
	// dynamic hydration every caller runs next rewrites items. Either would
	// turn an honest signature into a payload_hash mismatch.
	if d.verify {
		verdict := sigverify.Verify(bytes)
		playlist.Verification = &verdict
	}

	return playlist, nil
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
		License:  "open",
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
