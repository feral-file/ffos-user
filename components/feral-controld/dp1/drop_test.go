package dp1

import (
	"encoding/json"
	"testing"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// One bad edition or indexer item costs itself, not the others.
func TestNonConformingItemsAreDroppedAlone(t *testing.T) {
	d := &dp1{logger: zap.NewNop(), json: wrapper.NewJSON()}

	editions := d.conformingEditions("p", dp1playlist.PlaylistItem{}, []json.RawMessage{
		json.RawMessage(`{"source":"https://example.com/1"}`),
		json.RawMessage(`{"source":"https://example.com/2","duration":"long"}`),
		json.RawMessage(`{"title":"no source"}`),
		json.RawMessage(`{"source":"https://example.com/4","SOURCE":"nope"}`),
	})
	require.Len(t, editions, 2)
	assert.Equal(t, "https://example.com/1", editions[0].Source)
	assert.Equal(t, "https://example.com/4", editions[1].Source)

	items := d.conformingItems("p", []dp1playlist.PlaylistItem{
		{Source: ""}, // a token with no preview URL
		{Source: "https://example.com/ok"},
	})
	require.Len(t, items, 1)
	assert.Equal(t, "https://example.com/ok", items[0].Source)
}
