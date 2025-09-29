package ffindexer

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const (
	FF_INDEXER_HOST = "indexer.feralfile.com"
)

type ProjectMetadata struct {
	Title      string `json:"title,omitempty"`
	PreviewURL string `json:"previewURL,omitempty"`
}

type Token struct {
	ID              string `json:"id,omitempty"`
	Blockchain      string `json:"blockchain,omitempty"`
	ContractType    string `json:"contractType,omitempty"`
	ContractAddress string `json:"contractAddress,omitempty"`
	Asset           struct {
		Metadata struct {
			Project struct {
				Latest ProjectMetadata `json:"latest,omitempty"`
			} `json:"project,omitempty"`
		} `json:"metadata,omitempty"`
	}
}

type GraphQLResponse struct {
	Data struct {
		Tokens []Token `json:"tokens,omitempty"`
	} `json:"data,omitempty"`
}

type FFIndexer interface {
	QueryTokens(ctx context.Context, endpoint string, params map[string]string) ([]Token, error)
}

//go:generate mockgen -source=ff_indexer.go -destination=../mocks/ff_indexer.go -package=mocks -mock_names=FFIndexer=MockFFIndexer
type ffIndexer struct {
	http wrapper.HTTP
	json wrapper.JSON
	io   wrapper.IO
}

func New(http wrapper.HTTP, json wrapper.JSON, io wrapper.IO) FFIndexer {
	return &ffIndexer{http: http, json: json, io: io}
}

func (i *ffIndexer) QueryTokens(ctx context.Context, endpoint string, params map[string]string) ([]Token, error) {
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
	}`, qp)

	resp, err := i.execGraphQLQuery(endpoint, query)
	if err != nil {
		return nil, err
	}

	return resp.Data.Tokens, nil
}

// validateEndpoint validates that the endpoint is a valid FF Indexer endpoint
func validateEndpoint(endpoint string) error {
	url, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	if url.Host != FF_INDEXER_HOST {
		return fmt.Errorf("invalid endpoint: %s, expected %s", endpoint, FF_INDEXER_HOST)
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
	reqBody := map[string]any{
		"query": query,
	}

	bodyBytes, err := i.json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	resp, err := i.http.Post(endpoint, "application/json", bytes.NewReader(bodyBytes))
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
