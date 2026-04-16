package dp1

import (
	"context"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/google/uuid"
	"go.uber.org/zap"

	ffindexer "github.com/feral-file/ffos-user/components/feral-controld/ff-indexer"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const (
	DEFAULT_DURATION             = 300
	MINIMAL_PLAYLIST_ITEMS_LIMIT = 50
	MAX_PLAYLIST_ITEMS_LIMIT     = 255
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
}

// HasDynamicContent returns true when the playlist requests dynamic item resolution (spec or legacy).
func (p *Playlist) HasDynamicContent() bool {
	if p == nil {
		return false
	}
	return p.DynamicQuery != nil || len(p.DynamicQueries) > 0
}

// PlaylistURLResult is the outcome of fetching a playlist document by URL, optionally with
// If-None-Match revalidation (HTTP 304 Not Modified).
type PlaylistURLResult struct {
	// NotModified is true when the server responded with 304 and the body was not sent.
	NotModified bool
	// ETag is the ETag header from a 200 response (empty if the origin omitted it).
	ETag string
	// Playlist is the fully resolved playlist (including dynamic hydration when applicable).
	// Nil when NotModified is true.
	Playlist *Playlist
}

//go:generate mockgen -source=dp1.go -destination=../mocks/dp1.go -package=mocks -mock_names=DP1=MockDP1
type DP1 interface {
	// ProcessPlaylistURL processes a playlist from an URL.
	// It will fetch the playlist from the URL and process it.
	// If the playlist has dynamic queries, it will hand off to
	// ProcessDynamicPlaylist with the minimal flag.
	ProcessPlaylistURL(ctx context.Context, url string, minimal bool) (*Playlist, error)

	// ProcessPlaylistURLConditional fetches a playlist by URL and supports conditional GET via
	// If-None-Match. On 304 Not Modified, NotModified is true and Playlist is nil.
	ProcessPlaylistURLConditional(ctx context.Context, url string, minimal bool, ifNoneMatch string) (*PlaylistURLResult, error)

	// ProcessDynamicPlaylist processes a dynamic playlist.
	// If the playlist has dynamic queries, it will process them
	// and construct a playlist with the items.
	// Otherwise, it will return the error.
	// If minimal is true, it will only process some first items
	// and return the playlist quickly
	ProcessDynamicPlaylist(ctx context.Context, playlist Playlist, minimal bool) (*Playlist, error)
}

type dp1 struct {
	ffIndexer  ffindexer.FFIndexer
	httpClient wrapper.HTTPClient
	json       wrapper.JSON
	io         wrapper.IO
	logger     *zap.Logger
	// debug mirrors controld --debug: relaxes dp1-go dynamicQuery endpoint policy (http:// and non-public hosts).
	debug bool
}

func New(ffIndexer ffindexer.FFIndexer, httpClient wrapper.HTTPClient, json wrapper.JSON, io wrapper.IO, logger *zap.Logger, debug bool) DP1 {
	return &dp1{
		ffIndexer:  ffIndexer,
		httpClient: httpClient,
		json:       json,
		io:         io,
		logger:     logger,
		debug:      debug,
	}
}

func (d *dp1) ProcessPlaylistURL(ctx context.Context, url string, minimal bool) (*Playlist, error) {
	d.logger.Info("Processing playlist from URL", zap.String("url", url))

	if err := d.validateURL(url); err != nil {
		d.logger.Error("Invalid playlist URL", zap.String("url", url), zap.Error(err))
		return nil, fmt.Errorf("invalid playlist URL: %w", err)
	}

	res, err := d.ProcessPlaylistURLConditional(ctx, url, minimal, "")
	if err != nil {
		return nil, err
	}
	if res.NotModified {
		return nil, fmt.Errorf("unexpected 304 without If-None-Match")
	}
	if res.Playlist == nil {
		return nil, fmt.Errorf("empty playlist after fetch")
	}
	return res.Playlist, nil
}

func (d *dp1) ProcessPlaylistURLConditional(ctx context.Context, url string, minimal bool, ifNoneMatch string) (*PlaylistURLResult, error) {
	d.logger.Info("Processing playlist from URL", zap.String("url", url))

	if err := d.validateURL(url); err != nil {
		d.logger.Error("Invalid playlist URL", zap.String("url", url), zap.Error(err))
		return nil, fmt.Errorf("invalid playlist URL: %w", err)
	}

	notModified, etag, playlist, err := d.fetchPlaylistHTTP(ctx, url, ifNoneMatch)
	if err != nil {
		return nil, err
	}
	if notModified {
		return &PlaylistURLResult{NotModified: true}, nil
	}
	if playlist == nil {
		return nil, fmt.Errorf("internal: fetch returned no playlist without 304")
	}

	var out *Playlist
	if playlist.HasDynamicContent() {
		out, err = d.ProcessDynamicPlaylist(ctx, *playlist, minimal)
		if err != nil {
			return nil, err
		}
	} else {
		out = playlist
	}

	return &PlaylistURLResult{NotModified: false, ETag: etag, Playlist: out}, nil
}

func (d *dp1) ProcessDynamicPlaylist(ctx context.Context, playlist Playlist, minimal bool) (*Playlist, error) {
	d.logger.Info("Processing dynamic playlist", zap.String("playlist_id", playlist.ID))

	if playlist.DynamicQuery != nil {
		if len(playlist.DynamicQueries) > 0 {
			d.logger.Warn("playlist has both dynamicQuery and legacy dynamicQueries; using dynamicQuery only",
				zap.String("playlist_id", playlist.ID))
		}
		return d.processDynamicPlaylistSpec(ctx, playlist, minimal)
	}

	// --- LEGACY: dynamicQueries[] + FFIndexer (remove with LegacyDynamicQuery) ---
	if len(playlist.DynamicQueries) > 0 {
		return d.processDynamicPlaylistLegacy(ctx, playlist, minimal)
	}

	return nil, fmt.Errorf("playlist has no dynamic query configuration")
}

// processDynamicPlaylistSpec resolves items using DP-1 playlists extension dynamicQuery and
// github.com/display-protocol/dp1-go PlaylistItemsFromDynamicQuery. Pagination uses {{limit}} and
// {{offset}} placeholders in dynamicQuery.query, matching MINIMAL/MAX batch sizes used by legacy.
//
// Replacement behavior matches legacy: playlist.Items is replaced by resolved dynamic items only (no merge with static items).
func (d *dp1) processDynamicPlaylistSpec(ctx context.Context, playlist Playlist, minimal bool) (*Playlist, error) {
	if playlist.DynamicQuery == nil {
		return nil, fmt.Errorf("internal: dynamicQuery is nil")
	}

	// Validate dynamic query endpoint URL for security
	if err := d.validateURL(playlist.DynamicQuery.Endpoint); err != nil {
		d.logger.Error("Invalid dynamic query endpoint",
			zap.String("playlist_id", playlist.ID),
			zap.String("endpoint", playlist.DynamicQuery.Endpoint),
			zap.Error(err))
		return nil, fmt.Errorf("invalid dynamic query endpoint: %w", err)
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
			return nil, err
		}
		accumulated = append(accumulated, batch...)

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
func (d *dp1) processDynamicPlaylistLegacy(ctx context.Context, playlist Playlist, minimal bool) (*Playlist, error) {
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

// fetchPlaylistHTTP performs a GET (optionally conditional) and returns the parsed playlist on 200,
// or notModified on 304. Caller must not send If-None-Match on the first fetch (empty string).
func (d *dp1) fetchPlaylistHTTP(ctx context.Context, url, ifNoneMatch string) (notModified bool, etag string, playlist *Playlist, err error) {
	d.logger.Info("Fetching playlist from URL", zap.String("url", url))

	req, err := d.httpClient.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false, "", nil, err
	}
	req = req.WithContext(ctx)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return false, "", nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusNotModified {
		return true, ifNoneMatch, nil, nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, "", nil, fmt.Errorf("fetch playlist failed: %s", resp.Status)
	}

	etag = resp.Header.Get("ETag")

	bytes, err := d.io.ReadAll(resp.Body)
	if err != nil {
		return false, "", nil, err
	}

	var pl Playlist
	if err = d.json.Unmarshal(bytes, &pl); err != nil {
		return false, "", nil, err
	}

	return false, etag, &pl, nil
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

// validateURL validates playlist and dynamic query URLs for security.
// In production mode: requires HTTPS, rejects localhost, private IPs, and empty hosts.
// In debug mode: all validation is bypassed to allow local testing.
func (d *dp1) validateURL(rawURL string) error {
	// Debug mode bypasses all validation for local development and testing
	if d.debug {
		d.logger.Debug("URL validation bypassed in debug mode", zap.String("url", rawURL))
		return nil
	}

	// Parse URL structure
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("malformed URL: %w", err)
	}

	// Require valid scheme
	if parsed.Scheme == "" {
		return fmt.Errorf("malformed URL: missing scheme")
	}

	// Require HTTPS in production to prevent MITM attacks and data interception
	if parsed.Scheme != "https" {
		return fmt.Errorf("insecure scheme %q: HTTPS required in production", parsed.Scheme)
	}

	// Reject empty host
	if parsed.Host == "" {
		return fmt.Errorf("empty host")
	}

	// Extract hostname without port for IP and localhost checks
	hostname := parsed.Hostname()
	if hostname == "" {
		return fmt.Errorf("invalid host")
	}

	// Reject localhost and loopback addresses to prevent SSRF against local services
	if hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1" {
		return fmt.Errorf("localhost access not allowed in production")
	}

	// Reject private IP ranges to prevent SSRF against internal network
	if ip := net.ParseIP(hostname); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			return fmt.Errorf("private IP address not allowed in production")
		}
	}

	return nil
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
