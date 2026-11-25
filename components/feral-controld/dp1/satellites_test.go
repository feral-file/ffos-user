package dp1_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
)

// Both inline displayPlaylist payloads and URL-fetched documents are decoded
// through dp1.Playlist.UnmarshalJSON, so this pins the shared seam. dp1-go
// v0.8.0 added a typed Satellites field; encoding/json would otherwise fold
// a case-variant key into it and fail the whole playlist.
func TestPlaylistUnmarshalSatellites(t *testing.T) {
	const base = `"dpVersion":"1.1.0","id":"11111111-1111-4111-8111-111111111111","title":"t","items":[]`
	const sat = `[{"playlist":"https://example.invalid/p.json","kind":"burn"}]`

	cases := []struct {
		name      string
		doc       string
		wantCount int
	}{
		{"case-variant unknown member of the wrong type is ignored", `{` + base + `,"Satellites":123}`, 0},
		{"exact key survives a trailing case-variant", `{` + base + `,"satellites":` + sat + `,"Satellites":[]}`, 1},
		{"exact key survives a leading case-variant", `{` + base + `,"Satellites":[],"satellites":` + sat + `}`, 1},
		{"wrong-typed exact key decodes as absent", `{` + base + `,"satellites":123}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p dp1.Playlist
			require.NoError(t, json.Unmarshal([]byte(tc.doc), &p))
			assert.Equal(t, "t", p.Title)
			assert.Len(t, p.Satellites, tc.wantCount)
		})
	}
}
