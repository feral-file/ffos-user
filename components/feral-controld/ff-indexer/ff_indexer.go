package ffindexer

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

var FF_INDEXER_HOSTS = map[string]bool{
	"indexer.feralfile.com": true,
	"indexer.autonomy.io":   true,
}

// Legacy types for backward compatibility with DP1
type ProjectMetadata struct {
	Title      string `json:"title,omitempty"`
	PreviewURL string `json:"previewURL,omitempty"`
}

type Token struct {
	ID              string `json:"id,omitempty"`
	Blockchain      string `json:"blockchain,omitempty"`
	ContractType    string `json:"contractType,omitempty"`
	ContractAddress string `json:"contractAddress,omitempty"`
	Balance         int    `json:"balance"`
	Asset           struct {
		Metadata struct {
			Project struct {
				Latest ProjectMetadata `json:"latest,omitempty"`
			} `json:"project,omitempty"`
		} `json:"metadata,omitempty"`
	}
}

// New GraphQL types matching ff-indexer-v2 schema
type gqlArtist struct {
	DID  string `json:"did"`
	Name string `json:"name"`
}

type gqlTokenMetadata struct {
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	ImageURL     string       `json:"image_url"`
	AnimationURL string       `json:"animation_url"`
	Artists      []gqlArtist  `json:"artists"`
	MimeType     string       `json:"mime_type"`
}

type gqlOwner struct {
	OwnerAddress string `json:"owner_address"`
	Quantity     string `json:"quantity"`
}

type gqlPaginatedOwners struct {
	Items []gqlOwner `json:"items"`
	Total uint64     `json:"total"`
}

type gqlToken struct {
	ID              uint64              `json:"id"`
	TokenCID        string              `json:"token_cid"`
	Chain           string              `json:"chain"`
	Standard        string              `json:"standard"`
	ContractAddress string              `json:"contract_address"`
	TokenNumber     string              `json:"token_number"`
	CurrentOwner    string              `json:"current_owner"`
	Burned          bool                `json:"burned"`
	Metadata        *gqlTokenMetadata   `json:"metadata,omitempty"`
	Owners          *gqlPaginatedOwners `json:"owners,omitempty"`
}

type gqlTokenList struct {
	Items  []gqlToken `json:"items"`
	Offset uint64     `json:"offset"`
	Total  uint64     `json:"total"`
}

type GraphQLResponse struct {
	Data struct {
		Tokens *gqlTokenList `json:"tokens,omitempty"`
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

	// Map old param names to new schema
	mappedParams := mapParams(params)

	var queryParams []string
	if len(mappedParams) > 0 {
		for key, value := range mappedParams {
			p := formatGraphQLParam(key, value)
			queryParams = append(queryParams, p)
		}
	}
	qp := strings.Join(queryParams, "\n\t\t\t")

	// Use the new ff-indexer-v2 GraphQL schema
	query := fmt.Sprintf(`{
		tokens(
			%s
			expand: ["metadata", "owners"]
		) {
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

	// Map new schema to legacy Token structure for backward compatibility
	return mapToLegacyTokens(resp.Data.Tokens.Items, params), nil
}

// validateEndpoint validates that the endpoint is a valid FF Indexer endpoint
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

// mapParams maps old parameter names to new schema parameter names
func mapParams(params map[string]string) map[string]string {
	mapped := make(map[string]string)
	for key, value := range params {
		switch key {
		case "size":
			// Map old "size" to new "limit"
			mapped["limit"] = value
		case "blockchains":
			// Map old "blockchains" to new "chain"
			mapped["chain"] = value
		default:
			// Keep other params as-is
			mapped[key] = value
		}
	}
	return mapped
}

// mapToLegacyTokens maps new gqlToken format to legacy Token format for backward compatibility
func mapToLegacyTokens(gqlTokens []gqlToken, params map[string]string) []Token {
	tokens := make([]Token, 0, len(gqlTokens))
	
	// Get the owner address if filtering by owner
	ownerFilter := params["owner"]
	
	for _, gt := range gqlTokens {
		token := Token{
			ID:              gt.TokenCID, // Use token_cid as the primary ID
			Blockchain:      gt.Chain,
			ContractType:    gt.Standard,
			ContractAddress: gt.ContractAddress,
			Balance:         calculateBalance(gt, ownerFilter),
		}
		
		// Map metadata fields
		if gt.Metadata != nil {
			token.Asset.Metadata.Project.Latest.Title = gt.Metadata.Name
			
			// Prefer animation_url over image_url for preview
			if gt.Metadata.AnimationURL != "" {
				token.Asset.Metadata.Project.Latest.PreviewURL = gt.Metadata.AnimationURL
			} else {
				token.Asset.Metadata.Project.Latest.PreviewURL = gt.Metadata.ImageURL
			}
		}
		
		tokens = append(tokens, token)
	}
	
	return tokens
}

// calculateBalance determines the balance for a token based on owner filter or owners list
func calculateBalance(token gqlToken, ownerFilter string) int {
	// If burned, balance is 0
	if token.Burned {
		return 0
	}
	
	// If we have owners data, calculate balance
	if token.Owners != nil && len(token.Owners.Items) > 0 {
		// If filtering by specific owner, find that owner's quantity
		if ownerFilter != "" {
			for _, owner := range token.Owners.Items {
				if strings.EqualFold(owner.OwnerAddress, ownerFilter) {
					quantity, err := strconv.Atoi(owner.Quantity)
					if err != nil {
						return 0
					}
					return quantity
				}
			}
			return 0
		}
		
		// Otherwise sum all quantities
		totalBalance := 0
		for _, owner := range token.Owners.Items {
			quantity, err := strconv.Atoi(owner.Quantity)
			if err != nil {
				continue
			}
			totalBalance += quantity
		}
		return totalBalance
	}
	
	// If current_owner is set and matches the filter, assume balance of 1
	if ownerFilter != "" && token.CurrentOwner != "" && strings.EqualFold(token.CurrentOwner, ownerFilter) {
		return 1
	}
	
	// If current_owner is set (not burned), assume at least 1
	if token.CurrentOwner != "" {
		return 1
	}
	
	return 0
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
