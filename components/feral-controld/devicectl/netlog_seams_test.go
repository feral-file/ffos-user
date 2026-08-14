package devicectl_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
)

// (The lastOutage attach test lives in the status package beside GetStatus —
// the collector is the shared point both the pulled reply and the poller's
// pushed device_status feed flow through; see
// status.TestGetStatus_AttachesLastOutage.)

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
