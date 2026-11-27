package dp1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A member that differs only in case from a modeled one is unknown to DP-1:
// it must not override what the schema checked, at any depth.
func TestPlaylistDecodeMatchesMemberNamesExactly(t *testing.T) {
	doc := `{"dpVersion":"1.1.0","title":"t","TITLE":"other","ID":"nope",
	  "defaults":{"duration":5,"Duration":9},
	  "items":[{"source":"https://example.com/a","SOURCE":"https://evil.example/b",
	    "display":{"scaling":"fit","Scaling":"fill"},
	    "provenance":{"type":"onChain","Type":"offChainURI"}}]}`

	var p Playlist
	require.NoError(t, json.Unmarshal([]byte(doc), &p))

	assert.Equal(t, "t", p.Title)
	assert.Empty(t, p.ID)
	assert.InDelta(t, 5, *p.Defaults.Duration, 0)
	require.Len(t, p.Items, 1)
	assert.Equal(t, "https://example.com/a", p.Items[0].Source)
	assert.Equal(t, "fit", p.Items[0].Display.Scaling)
	assert.Equal(t, "onChain", string(p.Items[0].Provenance.Type))
	assert.Contains(t, p.Extra, "TITLE")
	assert.Contains(t, p.Extra, "ID")
}
