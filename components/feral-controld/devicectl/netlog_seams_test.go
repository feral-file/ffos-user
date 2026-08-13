package devicectl_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

// TestExecutor_DeviceStatus_AttachesLastOutage: the additive stage-2b field
// rides getDeviceStatus when the netlog seam is wired, and MD5-dedupe safety
// holds trivially (the summary only changes when an outage closes).
func TestExecutor_DeviceStatus_AttachesLastOutage(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	summary := &status.LastOutage{
		Start:    time.Unix(1700000000, 0),
		End:      time.Unix(1700000300, 0),
		Class:    "wan-down",
		Count24h: 3,
	}
	sink, ok := ts.executor.(interface {
		SetLastOutage(func() *status.LastOutage)
	})
	require.True(t, ok, "executor must expose the SetLastOutage seam")
	sink.SetLastOutage(func() *status.LastOutage { return summary })

	cmd := commands.Command{Type: commands.CMD_DEVICE_STATUS, Arguments: map[string]interface{}{}}
	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(`{}`), nil)
	ts.mockDeviceStatus.EXPECT().
		GetStatus(ts.ctx).
		Return(&status.DeviceStatusResponse{}, nil)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	require.NoError(t, err)
	resp, ok := result.(*status.DeviceStatusResponse)
	require.True(t, ok)
	require.NotNil(t, resp.LastOutage)
	assert.Equal(t, "wan-down", resp.LastOutage.Class)
	assert.Equal(t, 3, resp.LastOutage.Count24h)
}

// TestExecutor_RunNetworkDiagnostics_Unwired: same reject-never-pretend
// posture as startWifiSetup — a wiring that predates the seam must error.
func TestExecutor_RunNetworkDiagnostics_Unwired(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{Type: commands.CMD_RUN_NETWORK_DIAGNOSTICS, Arguments: map[string]interface{}{}}
	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(`{}`), nil)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "unavailable")
}

// TestExecutor_RunNetworkDiagnostics_Wired: the reply is the ladder result,
// synchronous, and the seam receives a deadline-bounded ctx (the hub's 30s
// write deadline must not be blown by a wedged rung).
func TestExecutor_RunNetworkDiagnostics_Wired(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	fakeResult := map[string]any{"class": "captive-portal"}
	var gotDeadline bool
	sink, ok := ts.executor.(interface {
		SetNetworkDiagnostics(func(context.Context) (any, error))
	})
	require.True(t, ok, "executor must expose the SetNetworkDiagnostics seam")
	sink.SetNetworkDiagnostics(func(ctx context.Context) (any, error) {
		_, gotDeadline = ctx.Deadline()
		return fakeResult, nil
	})

	cmd := commands.Command{Type: commands.CMD_RUN_NETWORK_DIAGNOSTICS, Arguments: map[string]interface{}{}}
	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(`{}`), nil)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	require.NoError(t, err)
	assert.Equal(t, fakeResult, result)
	assert.True(t, gotDeadline, "the ladder run must be deadline-bounded")
}

func TestExecutor_RunNetworkDiagnostics_LadderError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	sink := ts.executor.(interface {
		SetNetworkDiagnostics(func(context.Context) (any, error))
	})
	sink.SetNetworkDiagnostics(func(context.Context) (any, error) {
		return nil, errors.New("ladder wedged")
	})

	cmd := commands.Command{Type: commands.CMD_RUN_NETWORK_DIAGNOSTICS, Arguments: map[string]interface{}{}}
	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(`{}`), nil)

	_, err := ts.executor.Execute(ts.ctx, cmd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ladder wedged")
}
