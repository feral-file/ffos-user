package playlistschedule_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// A persisted inline document is re-judged before it can be played again.
func TestRestorePersistedChecksTheInlineDocument(t *testing.T) {
	for name, tt := range map[string]struct {
		doc      string
		restored bool
	}{
		"conforming":       {`{"dpVersion":"1.1.0","title":"t","items":[{"source":"https://example.com/a"}]}`, true},
		"ff-app exemption": {`{"dpVersion":"1.0.0","title":"t","id":"favorite","items":[{"source":"https://example.com/a"}]}`, true},
		"no title":         {`{"dpVersion":"1.1.0","items":[{"source":"https://example.com/a"}]}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			var doc dp1.Playlist
			require.NoError(t, json.Unmarshal([]byte(tt.doc), &doc))
			store := &memoryStore{saved: playlistschedule.Source{DynamicPlaylist: &doc}}

			s := playlistschedule.NewWithStore(context.Background(), nil, wrapper.NewClock(), nil, store, zap.NewNop())
			defer s.Stop()

			assert.Equal(t, tt.restored, s.RestoredPending())
		})
	}
}
