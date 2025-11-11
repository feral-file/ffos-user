package ffindexer

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

var FF_INDEXER_HOSTS = map[string]bool{
	"indexer.feralfile.com": true,
	"indexer.autonomy.io":   true,
}

// V2 GraphQL types matching ff-indexer-v2 schema
type Artist struct {
	DID  string `json:"did"`
	Name string `json:"name"`
}

type Publisher struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type TokenMetadata struct {
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	ImageURL     string     `json:"image_url"`
	AnimationURL string     `json:"animation_url"`
	Artists      []Artist   `json:"artists"`
	Publisher    *Publisher `json:"publisher,omitempty"`
	MimeType     string     `json:"mime_type"`
}

type Owner struct {
	OwnerAddress string `json:"owner_address"`
	Quantity     string `json:"quantity"`
}

type PaginatedOwners struct {
	Items []Owner `json:"items"`
	Total string  `json:"total"`
}

type Token struct {
	ID              string           `json:"id"`
	TokenCID        string           `json:"token_cid"`
	Chain           string           `json:"chain"`
	Standard        string           `json:"standard"`
	ContractAddress string           `json:"contract_address"`
	TokenNumber     string           `json:"token_number"`
	CurrentOwner    string           `json:"current_owner"`
	Burned          bool             `json:"burned"`
	Metadata        *TokenMetadata   `json:"metadata,omitempty"`
	Owners          *PaginatedOwners `json:"owners,omitempty"`
}

// GetPreviewURL returns the preview URL, preferring animation_url over image_url
func (t *Token) GetPreviewURL() string {
	if t.Metadata == nil {
		return ""
	}
	if t.Metadata.AnimationURL != "" {
		return t.Metadata.AnimationURL
	}
	return t.Metadata.ImageURL
}

// GetTitle returns the token title/name
func (t *Token) GetTitle() string {
	if t.Metadata == nil {
		return ""
	}
	return t.Metadata.Name
}

type TokenList struct {
	Items  []Token `json:"items"`
	Offset *string `json:"offset"`
	Total  string  `json:"total"`
}

type GraphQLResponse struct {
	Data struct {
		Tokens *TokenList `json:"tokens,omitempty"`
	} `json:"data,omitempty"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors,omitempty"`
}

type FFIndexer interface {
	QueryTokens(ctx context.Context, endpoint string, params map[string]string) ([]Token, error)
}

//go:generate mockgen -source=ff_indexer.go -destination=../mocks/ff_indexer.go -package=mocks -mock_names=FFIndexer=MockFFIndexer
type ffIndexer struct {
	httpClient wrapper.HTTPClient
	json       wrapper.JSON
	io         wrapper.IO
	logger     *zap.Logger
}

func New(httpClient wrapper.HTTPClient, json wrapper.JSON, io wrapper.IO, logger *zap.Logger) FFIndexer {
	return &ffIndexer{httpClient: httpClient, json: json, io: io, logger: logger}
}

func (i *ffIndexer) QueryTokens(ctx context.Context, endpoint string, params map[string]string) ([]Token, error) {
	i.logger.Info("Querying tokens", zap.String("endpoint", endpoint), zap.Any("params", params))
	// Don't validate endpoint for now
	// if err := validateEndpoint(endpoint); err != nil {
	// 	return nil, err
	// }

	var queryParams []string
	if len(params) > 0 {
		for key, value := range params {
			p := formatGraphQLParam(key, value)
			queryParams = append(queryParams, p)
		}
	}
	qp := strings.Join(queryParams, "\n\t\t\t")

	// Use the ff-indexer-v2 GraphQL schema
	query := fmt.Sprintf(`{
		tokens(%s) {
			items {
				id
				token_cid
				chain
				standard
				contract_address
				token_number
				current_owner
				burned
				metadata {
					name
					description
					image_url
					animation_url
					artists {
						did
						name
					}
					publisher {
						name
						url
					}
					mime_type
				}
				owners {
					items {
						owner_address
						quantity
					}
					total
				}
			}
			offset
			total
		}
	}`, qp)

	resp, err := i.execGraphQLQuery(endpoint, query)
	if err != nil {
		return nil, err
	}

	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("graphql errors: %v", resp.Errors)
	}

	if resp.Data.Tokens == nil {
		return []Token{}, nil
	}

	// Return v2 tokens directly
	return resp.Data.Tokens.Items, nil
}

// validateEndpoint validates that the endpoint is a valid FF Indexer endpoint
// Note: it's not currently used but kept for future reference
// nolint:unused
func validateEndpoint(endpoint string) error {
	url, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	if !FF_INDEXER_HOSTS[url.Host] {
		return fmt.Errorf("invalid endpoint: %s", endpoint)
	}
	return nil
}

// formatGraphQLParam formats a GraphQL parameter
// if the value contains commas, it will be converted to an array
// otherwise, it will be converted to a string with quotes
func formatGraphQLParam(key string, value string) string {
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

// execGraphQLQuery executes a GraphQL query
func (i *ffIndexer) execGraphQLQuery(endpoint string, query string) (*GraphQLResponse, error) {
	i.logger.Info("Executing GraphQL query", zap.String("endpoint", endpoint), zap.String("query", query))
	reqBody := map[string]any{
		"query": query,
	}

	bodyBytes, err := i.json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	resp, err := i.httpClient.Post(endpoint, "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := i.io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var graphqlResp GraphQLResponse
	if err := i.json.Unmarshal(respBody, &graphqlResp); err != nil {
		return nil, err
	}

	return &graphqlResp, nil
}
