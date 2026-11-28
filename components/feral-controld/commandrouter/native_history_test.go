package commandrouter_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/playerlink"
)

type historyPlayer struct {
	value any
	err   error
	call  func(string, any)
}

func (p *historyPlayer) Snapshot(context.Context) (playerlink.Snapshot, error) {
	return playerlink.Snapshot{}, errors.New("history must use an atomic read")
}
func (p *historyPlayer) Call(_ context.Context, method string, params, result any, expected *playerlink.Preconditions) error {
	if p.call != nil {
		p.call(method, params)
	}
	if expected != nil {
		return errors.New("unexpected mutation fence")
	}
	if p.err != nil {
		return p.err
	}
	data, err := json.Marshal(p.value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, result)
}

func setHistoryRecord(t *testing.T, h commandrouter.Handler, id string, item map[string]any, contentContext string) {
	t.Helper()
	commandrouter.SetNativeControls(h, &historyPlayer{
		value: map[string]any{"recordId": id, "playedAtMs": 17, "item": item, "context": contentContext, "defaults": map[string]any{"duration": 30}},
		call: func(method string, params any) {
			require.Equal(t, "history.resolve", method)
			require.Equal(t, map[string]any{"recordId": id}, params)
		},
	})
}

func TestNativeHistoryProjectsPrivateRecords(t *testing.T) {
	ts := setup(t)
	const source = "https://cdn.test/private?token=secret"
	record := func(id string) map[string]any {
		return map[string]any{"recordId": id, "playedAtMs": 17, "context": "personal", "defaults": map[string]any{"duration": 30}, "item": map[string]any{
			"id": "work", "source": source, "title": "fallback", "metadata": map[string]any{"title": "wrong"},
			"inlineManifest": map[string]any{"metadata": map[string]any{
				"title": strings.Repeat("é", 600), "artists": []any{nil, 4, map[string]any{"name": "Dmitri"}, map[string]any{"name": "Artist"}},
				"thumbnails": map[string]any{"small": map[string]any{"uri": "https://thumb.test/a?token=private"}},
			}},
		}}
	}
	commandrouter.SetNativeControls(ts.handler, &historyPlayer{value: map[string]any{
		"records": []any{record("incoming"), record("outgoing")}, "activeOccurrence": "outgoing", "incomplete": true,
	}, call: func(method string, params any) {
		require.Equal(t, "history.list", method)
		require.Equal(t, map[string]any{}, params)
	}})
	result, err := ts.handler.Process(ts.ctx, commands.Command{Type: commands.CMD_GET_RECENTLY_PLAYED})
	require.NoError(t, err)
	message := result.(map[string]any)["message"].(map[string]any)
	require.Equal(t, true, message["activeOccurrenceKnown"])
	require.Equal(t, true, message["incomplete"])
	rows := message["records"].([]any)
	require.Len(t, rows, 2)
	first, second := rows[0].(map[string]any), rows[1].(map[string]any)
	require.Equal(t, false, first["isActive"])
	require.Equal(t, true, second["isActive"])
	require.Equal(t, "Dmitri, Artist", first["artist"])
	require.Equal(t, "https://thumb.test/a?<redacted>", first["thumbnailUrl"])
	require.Len(t, first["title"], 512)
	require.Equal(t, float64(17), first["playedAtMs"])
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	for _, private := range []string{source, "token=", "defaults", "inlineManifest", "context"} {
		require.NotContains(t, string(encoded), private)
	}
}

func TestNativeHistoryEmptyAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		value  any
		err    error
		status string
	}{
		{"empty", map[string]any{"records": []any{}, "incomplete": false, "activeOccurrence": nil}, nil, "empty"},
		{"missing fields", map[string]any{}, nil, "error"},
		{"missing active occurrence", map[string]any{"records": []any{}, "incomplete": false}, nil, "error"},
		{"invalid active occurrence", map[string]any{"records": []any{}, "incomplete": false, "activeOccurrence": 42}, nil, "error"},
		{"missing item", map[string]any{"records": []any{map[string]any{"recordId": "a", "playedAtMs": 17}}, "incomplete": false}, nil, "error"},
		{"transport", nil, errors.New("private source token=secret"), "error"},
		{"unsupported", nil, &playerlink.RemoteError{Code: "unknown_method", Message: "History unsupported"}, "unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := setup(t)
			commandrouter.SetNativeControls(ts.handler, &historyPlayer{value: tc.value, err: tc.err})
			result, err := ts.handler.Process(ts.ctx, commands.Command{Type: commands.CMD_GET_RECENTLY_PLAYED})
			require.NoError(t, err)
			message := result.(map[string]any)["message"].(map[string]any)
			require.Equal(t, tc.status, message["status"])
			require.Equal(t, tc.status == "empty", message["ok"])
			encoded, _ := json.Marshal(result)
			require.NotContains(t, string(encoded), "secret")
		})
	}
}

func TestNativeHistoryResolveRejectsMismatchedRecord(t *testing.T) {
	ts := setup(t)
	commandrouter.SetNativeControls(ts.handler, &historyPlayer{value: map[string]any{"recordId": "other", "playedAtMs": 17, "item": map[string]any{"source": "https://private"}}})
	result, err := ts.handler.Process(ts.ctx, commands.Command{Type: commands.CMD_PLAY_RECENTLY_PLAYED, Arguments: map[string]any{"recordId": "wanted"}})
	require.NoError(t, err)
	require.Equal(t, false, result.(map[string]any)["message"].(map[string]any)["ok"])
}

func TestNativeHistoryRestartDoesNotMarkRetainedRecordActive(t *testing.T) {
	ts := setup(t)
	commandrouter.SetNativeControls(ts.handler, &historyPlayer{value: map[string]any{
		"records": []any{map[string]any{"recordId": "previous-instance", "playedAtMs": 17,
			"item": map[string]any{"source": "https://private", "title": "Fallback", "metadata": map[string]any{"artists": "malformed"}}}},
		"activeOccurrence": nil, "incomplete": false,
	}})
	result, err := ts.handler.Process(ts.ctx, commands.Command{Type: commands.CMD_GET_RECENTLY_PLAYED})
	require.NoError(t, err)
	message := result.(map[string]any)["message"].(map[string]any)
	require.Equal(t, false, message["activeOccurrenceKnown"])
	row := message["records"].([]any)[0].(map[string]any)
	require.Equal(t, false, row["isActive"])
	require.Equal(t, "Fallback", row["title"])
	require.NotContains(t, row, "artist")
}

func TestNativeHistoryUnrecordedPresentationRemainsUnknown(t *testing.T) {
	ts := setup(t)
	commandrouter.SetNativeControls(ts.handler, &historyPlayer{value: map[string]any{"records": []any{}, "incomplete": true, "activeOccurrence": "write-failed"}})
	result, err := ts.handler.Process(ts.ctx, commands.Command{Type: commands.CMD_GET_RECENTLY_PLAYED})
	require.NoError(t, err)
	message := result.(map[string]any)["message"].(map[string]any)
	require.Equal(t, false, message["activeOccurrenceKnown"])
	require.Equal(t, true, message["incomplete"])
	require.Equal(t, "empty", message["status"])
}

// Read-only: querying history must not change the user's current presentation.
func TestNativeHistoryDevice(t *testing.T) {
	path := os.Getenv("FF_NATIVE_STATUS_TEST_SOCKET")
	if path == "" {
		t.Skip("native device socket required")
	}
	client := playerlink.New(path)
	defer func() { _ = client.Close() }()
	ts := setup(t)
	commandrouter.SetNativeControls(ts.handler, client)
	result, err := ts.handler.Process(ts.ctx, commands.Command{Type: commands.CMD_GET_RECENTLY_PLAYED})
	require.NoError(t, err)
	message := result.(map[string]any)["message"].(map[string]any)
	require.Equal(t, true, message["ok"])
	require.Equal(t, true, message["activeOccurrenceKnown"])
	rows := message["records"].([]any)
	require.NotEmpty(t, rows)
	for _, row := range rows {
		require.NotContains(t, row.(map[string]any), "item")
		require.NotContains(t, row.(map[string]any), "source")
	}
	var record playerlink.HistoryRecord
	id := rows[0].(map[string]any)["recordId"]
	require.NoError(t, client.Call(context.Background(), "history.resolve", map[string]any{"recordId": id}, &record, nil))
	require.Equal(t, id, record.RecordID)
	require.NotEmpty(t, record.Item["source"])
	require.NotNil(t, record.PlayedAtMS)
	t.Logf("native history: %d bounded records, incomplete=%v", len(rows), message["incomplete"])
}
