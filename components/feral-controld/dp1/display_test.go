package dp1_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
)

// A display request reaches the player as the typed Playlist re-encoded, so a display
// field the struct does not model is dropped on the way. `imageRendering`
// (core 1.1.0 §4) was: the player draws a work that sets "pixelated" with
// hard pixel edges, and on dp1-go v0.6.2 the field never reached it.
func TestPlaylist_ImageRenderingSurvivesTheJSONRoundTrip(t *testing.T) {
	doc := `{"dpVersion":"1.1.0","id":"p1","title":"T",` +
		`"defaults":{"display":{"scaling":"fit","imageRendering":"pixelated"}},` +
		`"items":[{"id":"i1","source":"https://cdn.example/a.png","duration":300,` +
		`"display":{"imageRendering":"auto"}}]}`
	var playlist dp1.Playlist
	require.NoError(t, json.Unmarshal([]byte(doc), &playlist))

	encoded, err := json.Marshal(playlist)
	require.NoError(t, err)

	var wire struct {
		Defaults struct {
			Display map[string]json.RawMessage `json:"display"`
		} `json:"defaults"`
		Items []struct {
			Display map[string]json.RawMessage `json:"display"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(encoded, &wire))
	assert.JSONEq(t, `"pixelated"`, string(wire.Defaults.Display["imageRendering"]), "playlist defaults")
	require.Len(t, wire.Items, 1)
	assert.JSONEq(t, `"auto"`, string(wire.Items[0].Display["imageRendering"]), "item display")
}
