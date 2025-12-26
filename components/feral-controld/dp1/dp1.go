package dp1

import (
	"context"
	"fmt"
	"strings"

	dp1playlist "github.com/display-protocol/dp1-validator/playlist"
	"github.com/google/uuid"
	"go.uber.org/zap"

	ffindexer "github.com/feral-file/ffos-user/components/feral-controld/ff-indexer"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const (
	DEFAULT_DURATION             = 300
	MINIMAL_PLAYLIST_ITEMS_LIMIT = 25
	MAX_PLAYLIST_ITEMS_LIMIT     = 100
	// Namespace UUID for generating deterministic UUIDs from token identifiers
	//nolint:gosec
	TOKEN_NAMESPACE_UUID = "8c95b1c2-4ef7-4ad9-a89a-84e410c1b4b1"
)

type DynamicQuery struct {
	Endpoint string            `json:"endpoint"`
	Params   map[string]string `json:"params"`
}

type Playlist struct {
	dp1playlist.Playlist
	DynamicQueries []DynamicQuery `json:"dynamicQueries"`
}

//go:generate mockgen -source=dp1.go -destination=../mocks/dp1.go -package=mocks -mock_names=DP1=MockDP1
type DP1 interface {
	// ProcessPlaylistURL processes a playlist from an URL.
	// It will fetch the playlist from the URL and process it.
	// If the playlist has dynamic queries, it will hand off to
	// ProcessDynamicPlaylist with the minimal flag.
	ProcessPlaylistURL(ctx context.Context, url string, minimal bool) (*Playlist, error)

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
}

func New(ffIndexer ffindexer.FFIndexer, httpClient wrapper.HTTPClient, json wrapper.JSON, io wrapper.IO, logger *zap.Logger) DP1 {
	return &dp1{
		ffIndexer:  ffIndexer,
		httpClient: httpClient,
		json:       json,
		io:         io,
		logger:     logger,
	}
}

func (d *dp1) ProcessPlaylistURL(ctx context.Context, url string, minimal bool) (*Playlist, error) {
	d.logger.Info("Processing playlist from URL", zap.String("url", url))

	// Fetch playlist from URL
	playlist, err := d.fetchPlaylist(url)
	if err != nil {
		return nil, err
	}

	if len(playlist.DynamicQueries) > 0 {
		return d.ProcessDynamicPlaylist(ctx, playlist, minimal)
	}

	return &playlist, nil
}

func (d *dp1) ProcessDynamicPlaylist(ctx context.Context, playlist Playlist, minimal bool) (*Playlist, error) {
	d.logger.Info("Processing dynamic playlist", zap.String("playlist_id", playlist.ID))
	if len(playlist.DynamicQueries) != 1 {
		return nil, fmt.Errorf("playlist should have exactly 1 dynamic queries, but has %d", len(playlist.DynamicQueries))
	}

	// Process dynamic query by executing the GraphQL query
	// Create a copy to avoid modifying the original playlist
	originalQuery := playlist.DynamicQueries[0]
	dynamicQuery := DynamicQuery{
		Endpoint: originalQuery.Endpoint,
		Params:   make(map[string]string),
	}
	// Copy the original params
	for k, v := range originalQuery.Params {
		dynamicQuery.Params[k] = v
	}

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

	// Build playlist items
	duration := DEFAULT_DURATION
	if playlist.Defaults != nil && playlist.Defaults.Duration > 0 {
		duration = playlist.Defaults.Duration
	}

	// Build new items from tokens
	newItems := buildPlaylistItems(duration, ffTokens)

	originalItems := playlist.Items
	originalItemIDs := make(map[string]bool)
	for _, item := range originalItems {
		originalItemIDs[item.ID] = true
	}

	// Filter newItems to only include items that don't exist in originalItems
	filteredNewItems := make([]dp1playlist.PlaylistItem, 0, len(newItems))
	for _, newItem := range newItems {
		if !originalItemIDs[newItem.ID] {
			filteredNewItems = append(filteredNewItems, newItem)
		}
	}

	// Merge original items with new items
	playlist.Items = append(originalItems, filteredNewItems...)

	return &playlist, nil
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

	bytes, err := d.io.ReadAll(resp.Body)
	if err != nil {
		return Playlist{}, err
	}

	var playlist Playlist
	err = d.json.Unmarshal(bytes, &playlist)
	if err != nil {
		return Playlist{}, err
	}

	return playlist, nil
}

func buildPlaylistItems(duration int, tokens []ffindexer.Token) []dp1playlist.PlaylistItem {
	items := make([]dp1playlist.PlaylistItem, 0, len(tokens))
	for _, token := range tokens {
		items = append(items, buildPlaylistItem(duration, token))
	}
	return items
}
func buildPlaylistItem(duration int, token ffindexer.Token) dp1playlist.PlaylistItem {
	title := token.GetTitle()
	previewURL := token.GetPreviewURL()
	chain := normalizeChain(token.Chain)

	// Generate deterministic UUID from contractAddress, blockchain, and tokenNumber
	itemID := generateTokenUUID(token.ContractAddress, token.Chain, token.TokenNumber)

	return dp1playlist.PlaylistItem{
		ID:       itemID,
		Title:    &title,
		Source:   previewURL,
		Duration: duration,
		License:  "open",
		Provenance: &dp1playlist.Provenance{
			Type: "onChain",
			Contract: &dp1playlist.Contract{
				Chain:    chain,
				Standard: &token.Standard,
				Address:  &token.ContractAddress,
				TokenID:  &token.TokenNumber,
			},
		},
	}
}

// generateTokenUUID creates a deterministic UUID from contractAddress, blockchain, and tokenNumber.
// This ensures the same token always gets the same UUID.
func generateTokenUUID(contractAddress, blockchain, tokenNumber string) string {
	namespace := uuid.MustParse(TOKEN_NAMESPACE_UUID)
	// Combine the three fields with a delimiter to create a unique identifier
	identifier := fmt.Sprintf("%s:%s:%s", blockchain, contractAddress, tokenNumber)
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
