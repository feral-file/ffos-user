package refresher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-connectd/relayer"
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

type PlayerStatus struct {
	CastCommand *relayer.RelayerCmd `json:"castCommand,omitempty"`

	PlaylistURL *string      `json:"playlistURL,omitempty"`
	Playlist    *DP1Playlist `json:"playlist,omitempty"`
	Index       *int         `json:"index,omitempty"`
	IsPaused    *bool        `json:"isPaused,omitempty"`
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
		RefreshInterval: 5 * time.Minute,
		RequestTimeout:  20 * time.Second,
		PageSize:        100,
		InitialPageSize: 5,
		MaxRetries:      3,
		RetryBackoff:    1 * time.Second,
	}
}

type Refresher interface {
	Start(ctx context.Context, playerStatus func(ctx context.Context) (map[string]interface{}, error))
	Stop()

	StartPollingWithPlaylistURL(ctx context.Context, playlistURL string, withInitialSync bool)
	StartPollingWithDynamicQueries(ctx context.Context, dynamicQueries []DynamicQuery, playlist DP1Playlist, withInitialSync bool)

	FetchPlaylistByURL(ctx context.Context, playlistURL string) (*DP1Playlist, error)
	BuildInitialPlaylistItems(ctx context.Context, playlist DP1Playlist, dynamicQueries []DynamicQuery) ([]DP1Item, error)

	SetOnPlaylistUpdated(callback func(ctx context.Context, playlist DP1Playlist))
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

	// onPlaylistUpdated is called after each successful URL refetch
	onPlaylistUpdated func(ctx context.Context, playlist DP1Playlist)
}

// SetOnPlaylistUpdated registers a callback invoked after each successful URL-based refetch
func (p *refresher) SetOnPlaylistUpdated(callback func(ctx context.Context, playlist DP1Playlist)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onPlaylistUpdated = callback
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
		queryStopChan: nil,
	}
}

func (p *refresher) Start(ctx context.Context, statusProvider func(ctx context.Context) (map[string]interface{}, error)) {
	p.logger.Info("Starting Refresher")

	status, err := statusProvider(ctx)
	if err != nil {
		p.logger.Warn("Failed to fetch player status; will retry every 2m until success", zap.Error(err))
		retryTicker := p.clock.NewTicker(2 * time.Minute)
		defer retryTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				p.logger.Info("Start cancelled during status fetch retry")
				return
			case <-retryTicker.C:
				status, err = statusProvider(ctx)
				if err != nil {
					p.logger.Warn("Retry fetch player status failed", zap.Error(err))
					continue
				}
			}
			break
		}
	}

	p.logger.Info("Fetched player status", zap.Any("status", status))

	// convert status to PlayerStatus struct
	raw, err := p.json.Marshal(status)
	if err != nil {
		p.logger.Warn("Failed to marshal player status", zap.Error(err))
		return
	}
	var playerStatus PlayerStatus
	if err := p.json.Unmarshal(raw, &playerStatus); err != nil {
		p.logger.Warn("Failed to unmarshal player status", zap.Error(err))
		return
	}

	if !playerStatus.CastCommand.DisplayPlaylistCmd() {
		p.logger.Warn("Player command is not displayPlaylist; skipping", zap.Any("command", playerStatus.CastCommand))
		return
	}

	if playerStatus.PlaylistURL != nil && *playerStatus.PlaylistURL != "" {
		p.logger.Info("Auto: starting URL refresher", zap.String("url", *playerStatus.PlaylistURL))
		p.StartPollingWithPlaylistURL(ctx, *playerStatus.PlaylistURL, true)
		return
	}

	// Otherwise fall back to embedded playlist object with dynamicQueries
	if playerStatus.Playlist == nil {
		p.logger.Debug("No playlist URL or embedded playlist present in status; skipping")
		return
	}

	if len(playerStatus.Playlist.DynamicQueries) > 0 {
		p.logger.Info("Auto: starting dynamic queries refresher", zap.Int("query_count", len(playerStatus.Playlist.DynamicQueries)))
		p.StartPollingWithDynamicQueries(ctx, playerStatus.Playlist.DynamicQueries, *playerStatus.Playlist, true)
		return
	}

	p.logger.Info("Auto: no playlistURL or dynamicQueries; nothing to start")
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

// StartPollingWithPlaylistURL starts an interval to fetch playlist object by URL.
func (p *refresher) StartPollingWithPlaylistURL(ctx context.Context, playlistURL string, withInitialSync bool) {
	p.logger.Info("Starting playlist refresher by URL", zap.String("url", playlistURL))

	rebuildPlaylistFn := func(ctx context.Context) (*DP1Playlist, error) {
		playlist, err := p.FetchPlaylistByURL(ctx, playlistURL)
		if err != nil {
			return nil, err
		}

		if len(playlist.DynamicQueries) > 0 {
			playlistItems, err := p.buildPlaylistItems(ctx, *playlist, playlist.DynamicQueries, -1)
			if err != nil {
				return nil, err
			}
			playlist.Items = playlistItems
		}

		return playlist, nil
	}

	p.startRefresher(ctx, withInitialSync, rebuildPlaylistFn)
}

// StartPollingWithDynamicQueries starts an interval to execute dynamic queries periodically.
func (p *refresher) StartPollingWithDynamicQueries(ctx context.Context, dynamicQueries []DynamicQuery, playlist DP1Playlist, withInitialSync bool) {
	p.logger.Info("Starting playlist refresher by dynamic query", zap.Any("dynamicQueries", dynamicQueries))

	rebuildPlaylistFn := func(ctx context.Context) (*DP1Playlist, error) {
		playlistItems, err := p.buildPlaylistItems(ctx, playlist, dynamicQueries, -1)
		if err != nil {
			return nil, err
		}
		playlist.Items = playlistItems
		return &playlist, nil
	}

	p.startRefresher(ctx, withInitialSync, rebuildPlaylistFn)
}

// startRefresher is a shared helper that manages ticker, stopChan, and periodic execution.
// It ensures that after each rebuildFn, the playlist will be notified if no error occurs.
func (p *refresher) startRefresher(
	ctx context.Context,
	withInitialSync bool,
	rebuildPlaylistFn func(ctx context.Context) (*DP1Playlist, error),
) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Reset any existing ticker/goroutine
	if p.queryTicker != nil {
		p.queryTicker.Stop()
	}
	if p.queryStopChan != nil {
		close(p.queryStopChan)
	}
	p.queryStopChan = make(chan struct{})
	p.queryTicker = p.clock.NewTicker(p.config.RefreshInterval)

	refreshPlaylist := func(ctx context.Context) {
		playlist, err := rebuildPlaylistFn(ctx)
		if err != nil {
			p.logger.Warn("Rebuild playlist failed", zap.Error(err))
			return
		}
		if playlist == nil {
			p.logger.Warn("Rebuild playlist returned nil")
			return
		}
		p.notifyPlaylistUpdated(ctx, *playlist)
	}

	// Initial run if requested
	if withInitialSync {
		go refreshPlaylist(ctx)
	}

	// Periodic goroutine
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
				p.logger.Info("Refresher goroutine stopped due to context cancellation")
				return
			case <-p.queryStopChan:
				p.logger.Info("Refresher goroutine stopped due to stop signal")
				return
			case <-p.queryTicker.C:
				refreshPlaylist(ctx)
			}
		}
	}()
}

func (p *refresher) notifyPlaylistUpdated(ctx context.Context, playlist DP1Playlist) {
	p.mu.RLock()
	cb := p.onPlaylistUpdated
	p.mu.RUnlock()
	if cb != nil {
		cb(ctx, playlist)
	}
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

func (p *refresher) BuildInitialPlaylistItems(ctx context.Context, playlist DP1Playlist, dynamicQueries []DynamicQuery) ([]DP1Item, error) {
	return p.buildPlaylistItems(ctx, playlist, dynamicQueries, p.config.InitialPageSize)
}

// BuildPlaylistItems executes the raw dynamicQueries and returns playlist items (empty slice if none)
func (p *refresher) buildPlaylistItems(ctx context.Context, playlist DP1Playlist, dynamicQueries []DynamicQuery, limit int) ([]DP1Item, error) {
	if limit <= 0 {
		p.logger.Info("Building playlist items", zap.Any("dynamicQueries", dynamicQueries))
	} else {
		p.logger.Info("Building playlist items with limit", zap.Int("limit", limit))
	}

	tokens, err := p.executeDynamicQueries(ctx, dynamicQueries, limit)
	if err != nil {
		p.logger.Error("Failed to execute dynamic queries", zap.Error(err))
		tokens = []IndexerToken{}
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
func (p *refresher) mergeItemsAndTokens(playlist DP1Playlist, tokens []IndexerToken) []DP1Item {
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
		if item.ID == "" || item.Provenance == nil {
			continue
		}
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

		resp, err := p.http.Post(endpoint, "application/json", bytes.NewReader(bodyBytes))
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

	return fmt.Sprintf(`%s: "%s"`, key, value)
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

	contract := map[string]interface{}{
		"chain":    chain,
		"standard": token.ContractType,
		"address":  token.ContractAddress,
		"tokenId":  token.ID,
	}
	provenanceObj := map[string]interface{}{
		"type":     "onChain",
		"contract": contract,
	}
	provenanceBytes, _ := json.Marshal(provenanceObj)
	provenanceRaw := json.RawMessage(provenanceBytes)

	return DP1Item{
		ID:         token.ID,
		Title:      &title,
		Source:     previewURL,
		Duration:   30,
		License:    LicenseOpen,
		Provenance: &provenanceRaw,
	}
}

func normalizeProvenanceChain(blockchain string) ProvenanceChain {
	b := strings.ToLower(strings.TrimSpace(blockchain))
	switch b {
	case "ethereum", "evm":
		return ProvenanceChainEthereum
	case "tezos":
		return ProvenanceChainTezos
	case "bitmark":
		return ProvenanceChainBitmark
	default:
		return ProvenanceChainOther
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
