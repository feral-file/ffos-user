package refresher

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"encoding/json"

	"github.com/feral-file/ffos-user/components/feral-connectd/wrapper"
)

// DynamicQuery represents the structure for dynamic queries
type DynamicQuery struct {
	Endpoint string                 `json:"endpoint"`
	Params   map[string]interface{} `json:"params"`
}

// IndexerToken represents a token from the indexer
type IndexerToken struct {
	ID              string `json:"id"`
	Blockchain      string `json:"blockchain"`
	ContractAddress string `json:"contractAddress"`
	Asset           struct {
		Metadata struct {
			Project struct {
				Latest IndexerArtwork `json:"latest"`
			} `json:"project"`
		} `json:"metadata"`
	}
}

// IndexerArtwork represents artwork information from the indexer
type IndexerArtwork struct {
	Title      string `json:"title,omitempty"`
	PreviewURL string `json:"previewURL"`
}

// DP1Item represents a playlist item for DP1
type DP1Item struct {
	ID         string           `json:"id"`
	Title      *string          `json:"title"`
	Source     *string          `json:"source"`
	Duration   int              `json:"duration"`
	License    *string          `json:"license"`
	Ref        *string          `json:"ref"`
	Override   *json.RawMessage `json:"override"`
	Display    *json.RawMessage `json:"display"`
	Repro      *json.RawMessage `json:"repro"`
	Provenance *json.RawMessage `json:"provenance"`
	Created    string           `json:"created"`
}

type DP1Playlist struct {
	DPVersion      string          `json:"dpVersion"`
	ID             string          `json:"id"`
	Title          string          `json:"title"`
	Slug           string          `json:"slug"`
	Created        string          `json:"created"`
	Defaults       json.RawMessage `json:"defaults"`
	Items          []DP1Item       `json:"items"`
	Signature      string          `json:"signature"`
	DynamicQueries []DynamicQuery  `json:"dynamicQueries,omitempty"`
}

type DP1Provenance struct {
	Contract struct {
		TokenID string `json:"tokenId"`
	} `json:"contract"`
}

// GraphQLResponse represents the response structure from GraphQL queries
type GraphQLResponse struct {
	Data struct {
		Tokens []IndexerToken `json:"tokens"`
	} `json:"data"`
}

//go:generate mockgen -source=playlist_refresher.go -destination=../mocks/playlist_refresher.go -package=mocks -mock_names=PlaylistRefresher=MockPlaylistRefresher

type PlaylistRefresher interface {
	Stop()

	StartWithURL(ctx context.Context, playlistURL string)
	StartWithDynamicQueries(ctx context.Context, dynamicQueries []DynamicQuery)
	FetchPlaylistByURL(ctx context.Context, playlistURL string) (*DP1Playlist, error)
	BuildPlaylistItems(ctx context.Context, playlist *DP1Playlist, dynamicQueries []DynamicQuery) ([]DP1Item, error)
}

type playlistRefresher struct {
	http          wrapper.HTTP
	json          wrapper.JSON
	clock         wrapper.Clock
	logger        *zap.Logger
	queryTicker   *time.Ticker
	queryStopChan chan struct{}
}

func New(
	http wrapper.HTTP,
	json wrapper.JSON,
	clock wrapper.Clock,
	logger *zap.Logger,
) PlaylistRefresher {
	return &playlistRefresher{
		http:          http,
		json:          json,
		clock:         clock,
		logger:        logger,
		queryStopChan: make(chan struct{}),
	}
}

func (p *playlistRefresher) Stop() {
	p.logger.Info("Stopping playlist refresher")

	// Stop dynamic query ticker
	if p.queryTicker != nil {
		p.queryTicker.Stop()
	}

	// Signal stop to query goroutine
	select {
	case <-p.queryStopChan:
		// Already closed
	default:
		close(p.queryStopChan)
	}
}

// StartWithURL starts an interval to fetch playlist object by URL
func (p *playlistRefresher) StartWithURL(ctx context.Context, playlistURL string) {
	p.logger.Info("Starting playlist refresher by URL", zap.String("url", playlistURL))
	p.queryTicker = p.clock.NewTicker(1 * time.Minute)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-p.queryStopChan:
				return
			case <-p.queryTicker.C:
				if playlist, err := p.FetchPlaylistByURL(ctx, playlistURL); err != nil {
					p.logger.Warn("Periodic playlist fetch failed", zap.Error(err))
				} else {
					p.logger.Info("Periodic playlist fetch completed", zap.Any("playlist", playlist))
				}
			}
		}
	}()
}

// StartWithDynamicQueries starts an interval to execute dynamic queries periodically
func (p *playlistRefresher) StartWithDynamicQueries(ctx context.Context, dynamicQueries []DynamicQuery) {
	p.logger.Info("Starting playlist refresher by dynamic query")
	p.queryTicker = p.clock.NewTicker(1 * time.Minute)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-p.queryStopChan:
				return
			case <-p.queryTicker.C:
				if tokens, err := p.executeDynamicQueries(ctx, dynamicQueries); err != nil {
					p.logger.Warn("Periodic dynamic query failed", zap.Error(err))
				} else {
					p.logger.Info("Periodic dynamic query completed", zap.Int("token_count", len(tokens)))
				}
			}
		}
	}()
}

// FetchPlaylistByURL retrieves a playlist JSON from a URL via HTTP GET
func (p *playlistRefresher) FetchPlaylistByURL(ctx context.Context, playlistURL string) (*DP1Playlist, error) {
	p.logger.Info("Fetching playlist by URL", zap.String("url", playlistURL))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, playlistURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch playlist failed: %s", resp.Status)
	}

	bytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var playlist DP1Playlist
	if err := p.json.Unmarshal(bytes, &playlist); err != nil {
		return nil, err
	}

	p.logger.Info("Fetched playlist", zap.Any("playlist", playlist))
	return &playlist, nil
}

// BuildPlaylistItems executes the raw dynamicQueries and returns playlist items (empty slice if none)
func (p *playlistRefresher) BuildPlaylistItems(ctx context.Context, playlist *DP1Playlist, dynamicQueries []DynamicQuery) ([]DP1Item, error) {
	p.logger.Info("Building playlist items", zap.Any("dynamicQueries", dynamicQueries))
	tokens, err := p.executeDynamicQueries(ctx, dynamicQueries)
	if err != nil {
		return nil, err
	}

	items := p.mergeItemsAndTokens(playlist, tokens)
	if items == nil {
		return []DP1Item{}, nil
	}

	p.logger.Info("Built playlist items", zap.Any("items", items))
	return items, nil
}

// mergeItemsAndTokens filters existing playlist items by tokens or converts all tokens to items
func (p *playlistRefresher) mergeItemsAndTokens(playlist *DP1Playlist, tokens []IndexerToken) []DP1Item {
	p.logger.Info("Merging playlist items and tokens")

	// If playlist items are empty, convert all tokens to items
	if len(playlist.Items) == 0 {
		p.logger.Info("No playlist items, converting all tokens to items")
		return p.convertAllTokensToItems(tokens)
	}

	// Otherwise, filter out items not present in token list
	tokenIDs := map[string]struct{}{}
	for _, t := range tokens {
		if t.ID != "" {
			tokenIDs[t.ID] = struct{}{}
		}
	}

	var filteredItems []DP1Item
	for _, item := range playlist.Items {
		if item.ID != "" {
			// use item.provenance.contract.tokenId instead of item.ID
			var provenance DP1Provenance
			if err := p.json.Unmarshal(*item.Provenance, &provenance); err != nil {
				p.logger.Warn("Failed to unmarshal provenance", zap.Error(err))
				continue
			}

			if _, ok := tokenIDs[provenance.Contract.TokenID]; ok {
				filteredItems = append(filteredItems, item)
			}
		}
	}

	p.logger.Info("Filtered playlist items", zap.Any("filteredItems", filteredItems))
	return filteredItems
}

// executeDynamicQueries executes a GraphQL query with offset-based pagination to fetch all tokens
func (p *playlistRefresher) executeDynamicQueries(ctx context.Context, dynamicQueries []DynamicQuery) ([]IndexerToken, error) {
	if len(dynamicQueries) == 0 {
		return nil, fmt.Errorf("no queries provided")
	}

	// For now, only process the first query
	firstQuery := dynamicQueries[0]
	if firstQuery.Endpoint == "" {
		return nil, fmt.Errorf("first query has empty endpoint")
	}

	p.logger.Info("Executing dynamic query", zap.String("endpoint", firstQuery.Endpoint))

	// Execute query with offset-based pagination to fetch all tokens
	var allTokens []IndexerToken
	offset := 0
	size := 100

	for {
		graphqlQuery := p.buildGraphQLQuery(firstQuery.Params, offset)
		tokens, err := p.executeGraphQLQuery(ctx, firstQuery.Endpoint, graphqlQuery)
		if err != nil {
			return nil, fmt.Errorf("failed to execute GraphQL query: %w", err)
		}

		allTokens = append(allTokens, tokens...)

		// If we got fewer tokens than requested, we've reached the end
		if len(tokens) < size {
			break
		}
		offset += size
	}

	p.logger.Info("Dynamic query completed", zap.Int("total_tokens", len(allTokens)))
	return allTokens, nil
}

// executeGraphQLQuery executes a single GraphQL query and returns the results
func (p *playlistRefresher) executeGraphQLQuery(ctx context.Context, endpoint, query string) ([]IndexerToken, error) {
	// Create graphql request body
	requestBody := map[string]interface{}{
		"query": query,
	}

	bodyBytes, err := p.json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	// Create HTTP request with POST method
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	// Execute request using standard HTTP client since wrapper only has Get
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	// Read response
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Parse response
	var graphqlResp GraphQLResponse
	if err := p.json.Unmarshal(respBody, &graphqlResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return graphqlResp.Data.Tokens, nil
}

// buildGraphQLQuery builds a GraphQL query string with offset-based pagination
func (p *playlistRefresher) buildGraphQLQuery(params map[string]interface{}, offset int) string {
	var queryParamsParts []string

	// Add dynamic parameters from params map
	if len(params) > 0 {
		for key, value := range params {
			formattedParam := p.formatGraphQLParam(key, value)
			queryParamsParts = append(queryParamsParts, formattedParam)
		}
	}

	// Always add default parameters
	queryParamsParts = append(queryParamsParts, "burnedIncluded: true")
	queryParamsParts = append(queryParamsParts, "size: 100")
	queryParamsParts = append(queryParamsParts, fmt.Sprintf("offset: %d", offset))

	// Join all parameters
	queryParams := strings.Join(queryParamsParts, "\n\t\t\t")

	query := fmt.Sprintf(`{
		tokens(
			%s
		) {
			id
			blockchain
			contractAddress
			asset {
				metadata {
					project {
						latest {
							title
							previewURL
						}
					}
				}
			}
		}
	}`, queryParams)

	p.logger.Info("Built GraphQL query", zap.String("query", query))
	return query
}

func (p *playlistRefresher) formatGraphQLParam(key string, value interface{}) string {
	if v, ok := value.([]string); ok {
		var items []string
		for _, item := range v {
			items = append(items, fmt.Sprintf(`"%s"`, item))
		}
		return fmt.Sprintf(`%s: [%s]`, key, strings.Join(items, ", "))
	}

	return fmt.Sprintf(`%s: %v`, key, value)
}

func (p *playlistRefresher) convertAllTokensToItems(tokens []IndexerToken) []DP1Item {
	res := make([]DP1Item, 0, len(tokens))
	for _, t := range tokens {
		res = append(res, p.convertTokenToDP1Item(t))
	}
	return res
}

func (p *playlistRefresher) convertTokenToDP1Item(token IndexerToken) DP1Item {
	title := token.Asset.Metadata.Project.Latest.Title
	previewURL := token.Asset.Metadata.Project.Latest.PreviewURL

	provenance := json.RawMessage(fmt.Sprintf(`{
		"type": "onChain",
		"contract": {
			"chain": "%s",
			"tokenId": "%s",
			"address": "%s",
		}
	}`, token.Blockchain, token.ID, token.ContractAddress))

	// Create DP1Item from IndexerToken
	return DP1Item{
		ID:         token.ID,
		Title:      &title,
		Source:     &previewURL,
		Duration:   300,
		Provenance: &provenance,
	}
}
