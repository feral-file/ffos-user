package relayer

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// fakeShedConn is a minimal WebSocketConn that records WriteMessage payloads.
// A hand-rolled fake avoids importing the generated mocks package, which would
// create an import cycle in this in-package (white-box) test.
type fakeShedConn struct {
	writes [][]byte
}

func (f *fakeShedConn) WriteJSON(interface{}) error { return nil }
func (f *fakeShedConn) ReadMessage() (int, []byte, error) {
	return 0, nil, nil
}
func (f *fakeShedConn) WriteMessage(_ int, data []byte) error {
	f.writes = append(f.writes, data)
	return nil
}
func (f *fakeShedConn) WriteControl(int, []byte, time.Time) error { return nil }
func (f *fakeShedConn) SetPongHandler(func(string) error)         {}
func (f *fakeShedConn) SetReadDeadline(time.Time) error           { return nil }
func (f *fakeShedConn) Close() error                              { return nil }

// A shed command must receive a legible rate_limited RPC response mirroring the
// command router's shape, so callers see a rejection instead of a silent
// timeout (feral-file/ffos-user#208).
func TestSendShedResponse_RepliesToCommand(t *testing.T) {
	conn := &fakeShedConn{}
	r := &relayer{
		json:   wrapper.NewJSON(),
		conn:   conn,
		logger: zaptest.NewLogger(t),
	}

	command := "displayPlaylist"
	r.sendShedResponse(context.Background(), Payload{
		MessageID: "msg-1",
		Message:   Message{Command: &command},
	})

	require.Len(t, conn.writes, 1, "exactly one shed response should be written")

	var resp Response
	require.NoError(t, json.Unmarshal(conn.writes[0], &resp))
	assert.Equal(t, "RPC", resp.Type)
	assert.Equal(t, "msg-1", resp.MessageID)

	body, ok := resp.Message.(map[string]interface{})
	require.True(t, ok, "shed response body must be a structured map")
	assert.Equal(t, "rate_limited", body["error"])
	assert.Equal(t, "displayPlaylist", body["command"])
}

// Control-plane (system) and response-less messages have no caller awaiting a
// reply by messageID, so they must not trigger a shed response.
func TestSendShedResponse_SkipsSystemAndEmpty(t *testing.T) {
	conn := &fakeShedConn{}
	r := &relayer{
		json:   wrapper.NewJSON(),
		conn:   conn,
		logger: zaptest.NewLogger(t),
	}

	r.sendShedResponse(context.Background(), Payload{MessageID: MESSAGE_ID_SYSTEM})
	r.sendShedResponse(context.Background(), Payload{MessageID: ""})

	assert.Empty(t, conn.writes, "no response should be sent for system/empty message IDs")
}
