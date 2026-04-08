package dp1

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	ffindexer "github.com/feral-file/ffos-user/components/feral-controld/ff-indexer"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// testHTTPClient is a minimal wrapper.HTTPClient implementation for testing.
type testHTTPClient struct{}

func (testHTTPClient) NewRequest(method string, url string, body io.Reader) (*http.Request, error) {
	return http.NewRequest(method, url, body)
}

func (testHTTPClient) Do(req *http.Request) (*http.Response, error) {
	return nil, nil
}

func (testHTTPClient) Get(url string) (*http.Response, error) {
	return nil, nil
}

func (testHTTPClient) Post(url string, contentType string, body io.Reader) (*http.Response, error) {
	return nil, nil
}

// testFFIndexer is a minimal ffindexer.FFIndexer implementation for testing.
type testFFIndexer struct{}

func (testFFIndexer) QueryTokens(ctx context.Context, endpoint string, params map[string]string) ([]ffindexer.Token, error) {
	return nil, nil
}

// testJSONDecoder is a minimal wrapper.JSONDecoder implementation for testing.
type testJSONDecoder struct{}

func (testJSONDecoder) Decode(v interface{}) error {
	return nil
}

// testJSONEncoder is a minimal wrapper.JSONEncoder implementation for testing.
type testJSONEncoder struct{}

func (testJSONEncoder) Encode(v interface{}) error {
	return nil
}

// testJSON is a minimal wrapper.JSON implementation for testing.
type testJSON struct{}

func (testJSON) Marshal(v interface{}) ([]byte, error) {
	return nil, nil
}

func (testJSON) Unmarshal(data []byte, v interface{}) error {
	return nil
}

func (testJSON) NewDecoder(r io.Reader) wrapper.JSONDecoder {
	return testJSONDecoder{}
}

func (testJSON) NewEncoder(w io.Writer) wrapper.JSONEncoder {
	return testJSONEncoder{}
}

// testIO is a minimal wrapper.IO implementation for testing.
type testIO struct{}

func (testIO) ReadAll(r io.Reader) ([]byte, error) {
	return nil, nil
}

// TestHTTPClientAsHTTPClient_PreservesTimeout verifies that httpClientAsHTTPClient creates an
// http.Client with the 30s timeout from wrapper.NewHTTPClient, preventing indefinite hangs in
// dynamicQuery paths where caller contexts are typically long-lived.
func TestHTTPClientAsHTTPClient_PreservesTimeout(t *testing.T) {
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))

	d := New(testFFIndexer{}, testHTTPClient{}, testJSON{}, testIO{}, logger, false).(*dp1)

	client := d.httpClientAsHTTPClient()

	// Verify the timeout is set to wrapper.HTTPClientTimeout (30s)
	assert.NotNil(t, client, "http.Client should not be nil")
	assert.Equal(t, wrapper.HTTPClientTimeout, client.Timeout,
		"http.Client.Timeout must match wrapper.HTTPClientTimeout to prevent indefinite hangs")
}
