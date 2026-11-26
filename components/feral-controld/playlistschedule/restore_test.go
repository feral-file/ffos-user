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

const (
	specQuery = `"dynamicQuery":{"profile":"https-json-v1","endpoint":"https://example.com/q",` +
		`"responseMapping":{"itemsPath":"items","itemSchema":"dp1/1.1"}}`
	legacyQueries = `"dynamicQueries":[{"endpoint":"https://example.com/graphql","params":{}}]`
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
		// A dynamic source is saved without its items; records on devices
		// already hold `null`.
		"dynamicQuery, items dropped":      {`{"dpVersion":"1.1.0","title":"t","items":null,` + specQuery + `}`, true},
		"dynamicQueries, items dropped":    {`{"dpVersion":"1.1.0","title":"t","items":null,` + legacyQueries + `}`, true},
		"dynamic, items dropped, no title": {`{"dpVersion":"1.1.0","items":null,` + specQuery + `}`, false},
		"no query, items dropped":          {`{"dpVersion":"1.1.0","title":"t","items":null}`, false},
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

// What the scheduler itself saves for a scheduled dynamic cast must restore.
func TestScheduledDynamicSourceSurvivesRestart(t *testing.T) {
	for name, query := range map[string]string{"dynamicQuery": specQuery, "dynamicQueries": legacyQueries} {
		t.Run(name, func(t *testing.T) {
			var doc dp1.Playlist
			require.NoError(t, json.Unmarshal([]byte(`{"dpVersion":"1.1.0","title":"t","items":[`+
				`{"source":"https://example.com/a","displayAt":"2999-01-01T00:00:00Z"}],`+query+`}`), &doc))
			store := &memoryStore{}

			first := playlistschedule.NewWithStore(context.Background(), nil, wrapper.NewClock(), nil, store, zap.NewNop())
			first.PrepareWithSource(&doc, playlistschedule.Source{DynamicPlaylist: &doc})
			first.Commit()
			first.Stop()
			require.NotNil(t, store.saved.DynamicPlaylist)

			second := playlistschedule.NewWithStore(context.Background(), nil, wrapper.NewClock(), nil, store, zap.NewNop())
			defer second.Stop()

			assert.True(t, second.RestoredPending())
		})
	}
}
