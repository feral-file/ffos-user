package refresher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-connectd/wrapper"
)

type GraphQLResponse struct {
	Data struct {
		Tokens []IndexerToken `json:"tokens,omitempty"`
	} `json:"data,omitempty"`
}

type IndexerToken struct {
	ID              string `json:"id,omitempty"`
	Blockchain      string `json:"blockchain,omitempty"`
	ContractType    string `json:"contractType,omitempty"`
	ContractAddress string `json:"contractAddress,omitempty"`
	Asset           struct {
		Metadata struct {
			Project struct {
				Latest IndexerArtwork `json:"latest,omitempty"`
			} `json:"project,omitempty"`
		} `json:"metadata,omitempty"`
	}
}

type IndexerArtwork struct {
	Title      string `json:"title,omitempty"`
	PreviewURL string `json:"previewURL,omitempty"`
}

type DP1Playlist struct {
	DPVersion      string          `json:"dpVersion,omitempty"`
	ID             string          `json:"id,omitempty"`
	Title          string          `json:"title,omitempty"`
	Slug           string          `json:"slug,omitempty"`
	Created        string          `json:"created,omitempty"`
	Defaults       json.RawMessage `json:"defaults,omitempty"`
	Items          []DP1Item       `json:"items,omitempty"`
	Signature      string          `json:"signature,omitempty"`
	DynamicQueries []DynamicQuery  `json:"dynamicQueries,omitempty"`
}

type DynamicQuery struct {
	Endpoint string            `json:"endpoint"`
	Params   map[string]string `json:"params"`
}

type DP1Item struct {
	ID         string           `json:"id,omitempty"`
	Title      *string          `json:"title,omitempty"`
	Source     string           `json:"source,omitempty"`
	Duration   int              `json:"duration"`
	License    LicenseType      `json:"license,omitempty"`
	Ref        *string          `json:"ref,omitempty"`
	Override   *json.RawMessage `json:"override,omitempty"`
	Display    *json.RawMessage `json:"display,omitempty"`
	Repro      *json.RawMessage `json:"repro,omitempty"`
	Provenance *json.RawMessage `json:"provenance,omitempty"`
	Created    string           `json:"created,omitempty"`
}

type LicenseType string

const (
	LicenseOpen         LicenseType = "open"
	LicenseToken        LicenseType = "token"
	LicenseSubscription LicenseType = "subscription"
)

type DP1Provenance struct {
	Type     ProvenanceType `json:"type,omitempty"`
	Contract struct {
		Chain    ProvenanceChain `json:"chain,omitempty"`
		Standard *string         `json:"standard,omitempty"`
		Address  *string         `json:"address,omitempty"`
		SeriesID *string         `json:"seriesId,omitempty"`
		TokenID  *string         `json:"tokenId,omitempty"`
		URI      *string         `json:"uri,omitempty"`
		MetaHash *string         `json:"metaHash,omitempty"`
	} `json:"contract,omitempty"`
	Dependencies *json.RawMessage `json:"dependencies,omitempty"`
}

type ProvenanceType string

const (
	ProvenanceOnChain            ProvenanceType = "onChain"
	ProvenanceSeriesRegistration ProvenanceType = "seriesRegistration"
	ProvenanceOffChainURI        ProvenanceType = "offChainURI"
)

type ProvenanceChain string

const (
	ProvenanceChainEthereum ProvenanceChain = "evm"
	ProvenanceChainTezos    ProvenanceChain = "tezos"
	ProvenanceChainBitmark  ProvenanceChain = "bitmark"
	ProvenanceChainOther    ProvenanceChain = "other"
)

func normalizeProvenanceChain(blockchain string) ProvenanceChain {
	if blockchain == "ethereum" {
		return ProvenanceChainEthereum
	}

	casted := ProvenanceChain(blockchain)
	switch casted {
	case ProvenanceChainEthereum, ProvenanceChainTezos, ProvenanceChainBitmark:
		return casted
	default:
		return ProvenanceChainOther
	}
}

type Config struct {
	RefreshInterval time.Duration `json:"refreshInterval"`
	RequestTimeout  time.Duration `json:"requestTimeout"`
	PageSize        int           `json:"pageSize"`
	InitialPageSize int           `json:"initialPageSize"`
	MaxRetries      int           `json:"maxRetries"`
	RetryBackoff    time.Duration `json:"retryBackoff"`
}

func DefaultConfig() *Config {
	return &Config{
		RefreshInterval: 30 * time.Minute,
		RequestTimeout:  20 * time.Second,
		PageSize:        100,
		InitialPageSize: 5,
		MaxRetries:      3,
		RetryBackoff:    1 * time.Second,
	}
}

type Refresher interface {
	Stop()

	StartWithURL(ctx context.Context, playlistURL string)
	StartWithDynamicQueries(ctx context.Context, dynamicQueries []DynamicQuery)
	FetchPlaylistByURL(ctx context.Context, playlistURL string) (*DP1Playlist, error)
	BuildInitialPlaylistItems(ctx context.Context, playlist *DP1Playlist, dynamicQueries []DynamicQuery) ([]DP1Item, error)
}

type refresher struct {
	mu            sync.RWMutex
	config        *Config
	http          wrapper.HTTP
	json          wrapper.JSON
	clock         wrapper.Clock
	logger        *zap.Logger
	queryTicker   *time.Ticker
	queryStopChan chan struct{}
}

func New(
	config *Config,
	http wrapper.HTTP,
	json wrapper.JSON,
	clock wrapper.Clock,
	logger *zap.Logger,
) Refresher {
	if config == nil {
		config = DefaultConfig()
	}

	return &refresher{
		config:        config,
		http:          http,
		json:          json,
		clock:         clock,
		logger:        logger,
		queryStopChan: make(chan struct{}),
	}
}

func (p *refresher) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.logger.Info("Stopping playlist refresher")

	// Stop dynamic query ticker
	if p.queryTicker != nil {
		p.queryTicker.Stop()
		p.queryTicker = nil
	}

	// Signal stop to query goroutine
	if p.queryStopChan != nil {
		close(p.queryStopChan)
		p.queryStopChan = nil
	}
}

// StartWithURL starts an interval to fetch playlist object by URL
func (p *refresher) StartWithURL(ctx context.Context, playlistURL string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.logger.Info("Starting playlist refresher by URL", zap.String("url", playlistURL))

	// Create new channels if they don't exist
	if p.queryStopChan == nil {
		p.queryStopChan = make(chan struct{})
	}

	p.queryTicker = p.clock.NewTicker(p.config.RefreshInterval)
	go func() {
		defer func() {
			p.mu.Lock()
			if p.queryTicker != nil {
				p.queryTicker.Stop()
			}
			p.mu.Unlock()
		}()

		for {
			select {
			case <-ctx.Done():
				p.logger.Info("StartWithURL goroutine stopped due to context cancellation")
				return
			case <-p.queryStopChan:
				p.logger.Info("StartWithURL goroutine stopped due to stop signal")
				return
			case <-p.queryTicker.C:
				playlist, err := p.FetchPlaylistByURL(ctx, playlistURL)
				if err != nil {
					p.logger.Warn("Periodic playlist fetch failed", zap.Error(err))
					continue
				}

				p.logger.Info("Periodic playlist fetch completed", zap.Any("playlist", playlist))
			}
		}
	}()
}

// StartWithDynamicQueries starts an interval to execute dynamic queries periodically
func (p *refresher) StartWithDynamicQueries(ctx context.Context, dynamicQueries []DynamicQuery) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.logger.Info("Starting playlist refresher by dynamic query")

	// Create new channels if they don't exist
	if p.queryStopChan == nil {
		p.queryStopChan = make(chan struct{})
	}

	p.queryTicker = p.clock.NewTicker(p.config.RefreshInterval)
	go func() {
		defer func() {
			p.mu.Lock()
			if p.queryTicker != nil {
				p.queryTicker.Stop()
			}
			p.mu.Unlock()
		}()

		for {
			select {
			case <-ctx.Done():
				p.logger.Info("StartWithDynamicQueries goroutine stopped due to context cancellation")
				return
			case <-p.queryStopChan:
				p.logger.Info("StartWithDynamicQueries goroutine stopped due to stop signal")
				return
			case <-p.queryTicker.C:
				tokens, err := p.executeDynamicQueries(ctx, dynamicQueries, -1)
				if err != nil {
					p.logger.Warn("Periodic dynamic query failed", zap.Error(err))
					continue
				}

				p.logger.Info("Periodic dynamic query completed", zap.Int("token_count", len(tokens)))
			}
		}
	}()
}

// FetchPlaylistByURL retrieves a playlist JSON from a URL via HTTP GET
func (p *refresher) FetchPlaylistByURL(ctx context.Context, playlistURL string) (*DP1Playlist, error) {
	p.logger.Info("Fetching playlist by URL", zap.String("url", playlistURL))

	var playlist DP1Playlist
	err := p.executeWithRetry(ctx, func() error {
		resp, err := p.http.Get(playlistURL)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("fetch playlist failed: %s", resp.Status)
		}

		bytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		if err := p.json.Unmarshal(bytes, &playlist); err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	p.logger.Info("Fetched playlist", zap.Any("playlist", playlist))
	return &playlist, nil
}

func (p *refresher) BuildInitialPlaylistItems(ctx context.Context, playlist *DP1Playlist, dynamicQueries []DynamicQuery) ([]DP1Item, error) {
	return p.buildPlaylistItems(ctx, playlist, dynamicQueries, p.config.InitialPageSize)
}

// BuildPlaylistItems executes the raw dynamicQueries and returns playlist items (empty slice if none)
func (p *refresher) buildPlaylistItems(ctx context.Context, playlist *DP1Playlist, dynamicQueries []DynamicQuery, limit int) ([]DP1Item, error) {
	if limit <= 0 {
		p.logger.Info("Building playlist items", zap.Any("dynamicQueries", dynamicQueries))
	} else {
		p.logger.Info("Building playlist items with limit", zap.Int("limit", limit))
	}

	tokens, err := p.executeDynamicQueries(ctx, dynamicQueries, limit)
	if err != nil {
		return nil, err
	}

	items := p.mergeItemsAndTokens(playlist, tokens)
	if items == nil {
		return []DP1Item{}, nil
	}

	if limit <= 0 {
		p.logger.Info("Built playlist items", zap.Any("items", items))
	} else {
		p.logger.Info("Built limited playlist items", zap.Int("count", len(items)))
	}
	return items, nil
}

// mergeItemsAndTokens filters existing playlist items by tokens or converts all tokens to items
func (p *refresher) mergeItemsAndTokens(playlist *DP1Playlist, tokens []IndexerToken) []DP1Item {
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
			var provenance DP1Provenance
			if err := p.json.Unmarshal(*item.Provenance, &provenance); err != nil {
				p.logger.Warn("Failed to unmarshal provenance", zap.Error(err))
				continue
			}

			if provenance.Contract.TokenID != nil {
				if _, ok := tokenIDs[*provenance.Contract.TokenID]; ok {
					filteredItems = append(filteredItems, item)
				}
			}
		}
	}

	p.logger.Info("Filtered playlist items", zap.Any("filteredItems", filteredItems))
	return filteredItems
}

// executeDynamicQueries executes a GraphQL query with offset-based pagination to fetch tokens
func (p *refresher) executeDynamicQueries(ctx context.Context, dynamicQueries []DynamicQuery, limit int) ([]IndexerToken, error) {
	if len(dynamicQueries) == 0 {
		return nil, fmt.Errorf("no queries provided")
	}

	// For now, only process the first query
	firstQuery := dynamicQueries[0]
	if firstQuery.Endpoint == "" {
		return nil, fmt.Errorf("first query has empty endpoint")
	}

	p.logger.Info("Executing dynamic query", zap.String("endpoint", firstQuery.Endpoint))

	fetchTokens := func(offset, size int) ([]IndexerToken, error) {
		query := p.buildGraphQLQuery(firstQuery.Params, offset, size)
		tokens, err := p.executeGraphQLQuery(ctx, firstQuery.Endpoint, query)
		if err != nil {
			return nil, fmt.Errorf("failed to execute GraphQL query: %w", err)
		}

		return tokens, nil
	}

	// If limit is specified, fetch only one page
	if limit > 0 {
		if limit > p.config.PageSize {
			limit = p.config.PageSize
		}

		return fetchTokens(0, limit)
	}

	// Execute query with offset-based pagination to fetch all tokens
	var allTokens []IndexerToken
	offset := 0
	size := p.config.PageSize

	for {
		tokens, err := fetchTokens(offset, size)
		if err != nil {
			return nil, err
		}

		allTokens = append(allTokens, tokens...)

		if len(tokens) < size {
			break
		}

		offset += size
	}

	p.logger.Info("Dynamic query completed", zap.Int("total_tokens", len(allTokens)))
	return allTokens, nil
}

// executeGraphQLQuery executes a single GraphQL query and returns the results
func (p *refresher) executeGraphQLQuery(ctx context.Context, endpoint, query string) ([]IndexerToken, error) {
	var tokens []IndexerToken

	err := p.executeWithRetry(ctx, func() error {
		// Create graphql request body
		requestBody := map[string]interface{}{
			"query": query,
		}

		bodyBytes, err := p.json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("failed to marshal request body: %w", err)
		}

		resp, err := p.http.Post(endpoint, "application/json", strings.NewReader(string(bodyBytes)))
		if err != nil {
			return fmt.Errorf("failed to execute request: %w", err)
		}
		defer resp.Body.Close()

		// Read response
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read response: %w", err)
		}

		// Parse response
		var graphqlResp GraphQLResponse
		if err := p.json.Unmarshal(respBody, &graphqlResp); err != nil {
			return fmt.Errorf("failed to unmarshal response: %w", err)
		}

		tokens = graphqlResp.Data.Tokens
		return nil
	})

	if err != nil {
		return nil, err
	}

	return tokens, nil
}

// buildGraphQLQuery builds a GraphQL query string with offset-based pagination
func (p *refresher) buildGraphQLQuery(params map[string]string, offset int, pageSize int) string {
	if pageSize <= 0 {
		pageSize = p.config.PageSize
	}

	var queryParamsParts []string

	// Add dynamic parameters from params map
	if len(params) > 0 {
		for key, value := range params {
			formattedParam := p.formatGraphQLParam(key, value)
			queryParamsParts = append(queryParamsParts, formattedParam)
		}
	}

	// Always add default parameters
	queryParamsParts = append(queryParamsParts, fmt.Sprintf("size: %d", pageSize))
	queryParamsParts = append(queryParamsParts, fmt.Sprintf("offset: %d", offset))

	// Join all parameters
	queryParams := strings.Join(queryParamsParts, "\n\t\t\t")

	query := fmt.Sprintf(`{
		tokens(
			%s
		) {
			id
			blockchain
			contractType
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

func (p *refresher) formatGraphQLParam(key string, value string) string {
	// Check if the value contains commas (comma-separated values that should be converted to array)
	if strings.Contains(value, ",") {
		// Split by comma and format as GraphQL array
		items := strings.Split(value, ",")
		var quotedItems []string
		for _, item := range items {
			trimmed := strings.TrimSpace(item)
			quotedItems = append(quotedItems, fmt.Sprintf(`"%s"`, trimmed))
		}
		return fmt.Sprintf(`%s: [%s]`, key, strings.Join(quotedItems, ", "))
	}

	return fmt.Sprintf(`%s: %s`, key, value)
}

func (p *refresher) convertAllTokensToItems(tokens []IndexerToken) []DP1Item {
	res := make([]DP1Item, 0, len(tokens))
	for _, t := range tokens {
		res = append(res, p.convertTokenToDP1Item(t))
	}
	return res
}

func (p *refresher) convertTokenToDP1Item(token IndexerToken) DP1Item {
	title := token.Asset.Metadata.Project.Latest.Title
	previewURL := token.Asset.Metadata.Project.Latest.PreviewURL
	chain := normalizeProvenanceChain(token.Blockchain)
	provenance := json.RawMessage(fmt.Sprintf(`{
		"type": "onChain",
		"contract": {
			"chain": "%s",
			"standard": "%s",
			"address": "%s",
			"tokenId": "%s",
		}
	}`, chain, token.ContractType, token.ContractAddress, token.ID))

	// Create DP1Item from IndexerToken
	return DP1Item{
		ID:         token.ID,
		Title:      &title,
		Source:     previewURL,
		Duration:   300,
		License:    LicenseOpen,
		Provenance: &provenance,
	}
}

// executeWithRetry executes a function with retry logic and exponential backoff
func (p *refresher) executeWithRetry(ctx context.Context, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt < p.config.MaxRetries; attempt++ {
		if err := fn(); err != nil {
			lastErr = err
			if attempt == p.config.MaxRetries-1 {
				return fmt.Errorf("failed after %d attempts: %w", p.config.MaxRetries, err)
			}

			// Exponential backoff
			backoff := time.Duration(attempt+1) * p.config.RetryBackoff
			p.logger.Info("Retrying after error",
				zap.Error(err),
				zap.Int("attempt", attempt+1),
				zap.Duration("backoff", backoff))

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
				continue
			}
		}
		return nil
	}
	return lastErr
}
