package dp1

import (
	"context"
	"fmt"
	"strings"

	dp1playlist "github.com/display-protocol/dp1-validator/playlist"
	"go.uber.org/zap"

	ffindexer "github.com/feral-file/ffos-user/components/feral-controld/ff-indexer"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const (
	DEFAULT_DURATION             = 300
	MINIMAL_PLAYLIST_ITEMS_LIMIT = 25
	MAX_PLAYLIST_ITEMS_LIMIT     = 100
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
	ffIndexer ffindexer.FFIndexer
	http      wrapper.HTTP
	json      wrapper.JSON
	io        wrapper.IO
	logger    *zap.Logger
}

func New(ffIndexer ffindexer.FFIndexer, http wrapper.HTTP, json wrapper.JSON, io wrapper.IO, logger *zap.Logger) DP1 {
	return &dp1{
		ffIndexer: ffIndexer,
		http:      http,
		json:      json,
		io:        io,
		logger:    logger,
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
	d.logger.Info("Processing dynamic playlist", zap.Any("playlist", playlist))
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
	size := MAX_PLAYLIST_ITEMS_LIMIT
	if minimal {
		size = MINIMAL_PLAYLIST_ITEMS_LIMIT
	}
	dynamicQuery.Params["size"] = fmt.Sprintf("%d", size)
	offset := 0

	for {
		dynamicQuery.Params["offset"] = fmt.Sprintf("%d", offset)
		tokens, err := d.ffIndexer.QueryTokens(ctx, dynamicQuery.Endpoint, dynamicQuery.Params)
		if err != nil {
			return nil, err
		}

		// Filter tokens with balance > 0 so it only includes the tokens actually owned by the owner
		for _, token := range tokens {
			if token.Balance > 0 {
				ffTokens = append(ffTokens, token)
			}
		}

		if len(tokens) < size || (minimal && len(ffTokens) >= MINIMAL_PLAYLIST_ITEMS_LIMIT) {
			break
		}

		offset += size
	}

	// Build playlist items
	duration := DEFAULT_DURATION
	if playlist.Defaults != nil {
		duration = playlist.Defaults.Duration
	}

	// Merge original items with new items
	originalItems := playlist.Items
	newItems := buildPlaylistItems(duration, ffTokens)
	playlist.Items = append(originalItems, newItems...)

	return &playlist, nil
}

func (d *dp1) fetchPlaylist(url string) (Playlist, error) {
	d.logger.Info("Fetching playlist from URL", zap.String("url", url))
	resp, err := d.http.Get(url)
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
	title := token.Asset.Metadata.Project.Latest.Title
	previewURL := token.Asset.Metadata.Project.Latest.PreviewURL
	chain := normalizeChain(token.Blockchain)

	return dp1playlist.PlaylistItem{
		ID:       token.ID,
		Title:    &title,
		Source:   previewURL,
		Duration: duration,
		License:  "open",
		Provenance: &dp1playlist.Provenance{
			Type: "onChain",
			Contract: &dp1playlist.Contract{
				Chain:    chain,
				Standard: &token.ContractType,
				Address:  &token.ContractAddress,
				TokenID:  &token.ID,
			},
		},
	}
}

func normalizeChain(blockchain string) string {
	b := strings.ToLower(strings.TrimSpace(blockchain))
	switch b {
	case "ethereum":
		return "evm"
	case "tezos":
		return "tezos"
	case "bitmark":
		return "bitmark"
	default:
		return "other"
	}
}
