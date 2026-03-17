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

const FF_INDEXER_HOST = "indexer-v2.feralfile.com"

type Display struct {
	ImageURL     *string `json:"image_url,omitempty"`
	AnimationURL *string `json:"animation_url,omitempty"`
	Name         *string `json:"name,omitempty"`
}
type Token struct {
	Chain           string   `json:"chain,omitempty"`
	Standard        string   `json:"standard,omitempty"`
	ContractAddress string   `json:"contract_address,omitempty"`
	TokenNumber     string   `json:"token_number,omitempty"`
	Display         *Display `json:"display,omitempty"`
}

// GetPreviewURL returns the preview URL, preferring animation_url over image_url
func (t *Token) GetPreviewURL() string {
	if t.Display == nil {
		return ""
	}
	if t.Display.AnimationURL != nil && *t.Display.AnimationURL != "" {
		return *t.Display.AnimationURL
	}
	if t.Display.ImageURL != nil && *t.Display.ImageURL != "" {
		return *t.Display.ImageURL
	}
	return ""
}

// GetName returns the token name
func (d *Token) GetName() string {
	if d.Display == nil {
		return ""
	}
	if d.Display.Name != nil {
		return *d.Display.Name
	}
	return ""
}

type TokenList struct {
	Items  []Token `json:"items"`
	Offset *string `json:"offset"`
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
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}

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
				chain
				standard
				contract_address
				token_number
				display {
					image_url
					animation_url
					name
				}
			}
			offset
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
func validateEndpoint(endpoint string) error {
	url, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	if url.Host != FF_INDEXER_HOST {
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
