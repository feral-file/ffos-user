package refresher

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
)

// The retire signal is an empty list, and still says which DP-1 it is.
func TestRetireSignalDocumentIsVersionedAndEmpty(t *testing.T) {
	raw, err := json.Marshal(retireSignalDocument())
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	assert.Equal(t, dp1.EmittedDPVersion, doc["dpVersion"])
	assert.NotEmpty(t, doc["title"])
	assert.Equal(t, []any{}, doc["items"])
}
