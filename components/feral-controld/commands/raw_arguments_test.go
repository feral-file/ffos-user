package commands_test

// Command.RawArguments keeps the `request` token verbatim so an inline DP-1
// playlist can be signature-verified on the caller's bytes rather than a
// lossy map round-trip (feral-file/ffos-user#307).

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
)

func TestCommand_UnmarshalJSON_KeepsRawRequestToken(t *testing.T) {
	wire := `{"command":"displayPlaylist","request":{"dp1_call":{"title":"a & b <c>","duration":9007199254740993},"intent":{"action":"now_display"}}}`

	var c commands.Command
	require.NoError(t, json.Unmarshal([]byte(wire), &c))

	assert.Equal(t, commands.CMD_DISPLAY_PLAYLIST, c.Type)
	assert.Contains(t, c.Arguments, "dp1_call")
	raw, ok := c.RawArgument("dp1_call")
	require.True(t, ok)
	assert.Equal(t, `{"title":"a & b <c>","duration":9007199254740993}`, string(raw), "the token is the caller's bytes, byte for byte")

	// The decoded map is the lossy view the token exists to bypass.
	remarshaled, err := json.Marshal(c.Arguments["dp1_call"])
	require.NoError(t, err)
	jsonAmpEscape := string([]byte{'\\', 'u', '0', '0', '2', '6'})
	assert.Contains(t, string(remarshaled), jsonAmpEscape, "encoding/json escapes & on re-marshal (a 6x size inflation)")
	assert.NotContains(t, string(remarshaled), "9007199254740993", "2^53+1 is rounded by the float64 round-trip")
}

func TestCommand_RawArgument_AbsentCases(t *testing.T) {
	var built commands.Command // in-process construction: no wire form
	built.Arguments = map[string]any{"dp1_call": map[string]any{}}
	_, ok := built.RawArgument("dp1_call")
	assert.False(t, ok)

	var c commands.Command
	require.NoError(t, json.Unmarshal([]byte(`{"command":"displayPlaylist","request":{"playlistUrl":"https://x"}}`), &c))
	_, ok = c.RawArgument("dp1_call")
	assert.False(t, ok, "key absent")

	var n commands.Command
	require.NoError(t, json.Unmarshal([]byte(`{"command":"displayPlaylist","request":{"dp1_call":null}}`), &n))
	_, ok = n.RawArgument("dp1_call")
	assert.False(t, ok, "null is absent")

	var none commands.Command
	require.NoError(t, json.Unmarshal([]byte(`{"command":"deviceStatus"}`), &none))
	assert.Nil(t, none.RawArguments)
}

// TestCommand_JSON_RoundTripUnaffected: the retained token never leaks into
// the outbound encoding (the CDP payload is built from Arguments).
func TestCommand_JSON_RoundTripUnaffected(t *testing.T) {
	var c commands.Command
	require.NoError(t, json.Unmarshal([]byte(`{"command":"displayPlaylist","request":{"k":"v"}}`), &c))

	out, err := c.JSON()
	require.NoError(t, err)
	assert.JSONEq(t, `{"command":"displayPlaylist","request":{"k":"v"}}`, string(out))
	assert.False(t, strings.Contains(string(out), "RawArguments"))
}
