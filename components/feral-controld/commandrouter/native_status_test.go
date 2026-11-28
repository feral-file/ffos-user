package commandrouter_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/playerlink"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

// Read-only opt-in check; it never changes an artwork or device preference.
func TestNativeStatusDevice(t *testing.T) {
	path := os.Getenv("FF_NATIVE_STATUS_TEST_SOCKET")
	if path == "" {
		t.Skip("native device status socket required")
	}
	client := playerlink.New(path)
	defer func() { _ = client.Close() }()
	ts := setup(t)
	commandrouter.SetNativeControls(ts.handler, client)
	result, err := ts.handler.Process(context.Background(), commands.Command{Type: "checkStatus"})
	require.NoError(t, err)
	reply, ok := result.(map[string]any)["message"].(*status.PlayerStatus)
	require.True(t, ok)
	require.True(t, reply.Ok)
	require.NotNil(t, reply.Runtime)
	require.Equal(t, "responsive", reply.Runtime.Host)
	require.NotNil(t, reply.Runtime.Presented)
	require.NotNil(t, reply.DeviceSettings.ShowingKey)
	require.Equal(t, reply.Runtime.Presented.ID, *reply.DeviceSettings.ShowingKey)
	require.NotNil(t, reply.DeviceSettings.CompositionRevision)
	require.Nil(t, reply.Outcomes)
	require.Nil(t, reply.SignatureStatus)
	t.Logf("native status responsive; occurrence=%s composition=%d generation=%d", *reply.DeviceSettings.ShowingKey, *reply.DeviceSettings.CompositionRevision, reply.Runtime.Presented.Generation)
}
