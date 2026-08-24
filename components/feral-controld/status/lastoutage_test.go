package status_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// lastOutageTestCollector builds a real deviceStatus collector over the
// standard mock set (same harness as the DisplayURL tests) with the netlog
// lastOutage seam wired.
func lastOutageTestCollector(t *testing.T, src func() *status.LastOutage) status.DeviceStatus {
	t.Helper()
	ctrl := gomock.NewController(t)
	mockOS := mocks.NewMockOS(ctrl)
	mockExec := mocks.NewMockExec(ctrl)
	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockIO := mocks.NewMockIO(ctrl)
	nmcliCmd := mocks.NewMockExecCmd(ctrl)
	pamCmd := mocks.NewMockExecCmd(ctrl)

	expectDeviceStatusOSMocks(t, mockOS)
	expectDeviceStatusExecMocks(t, mockExec, nmcliCmd, pamCmd)

	ds := status.NewDeviceStatus(wrapper.NewJSON(), mockOS, mockExec, mockHTTP, mockIO, nil)
	sink, ok := ds.(interface {
		SetLastOutageSource(func() *status.LastOutage)
	})
	require.True(t, ok, "collector must expose the SetLastOutageSource seam")
	sink.SetLastOutageSource(src)
	return ds
}

// TestGetStatus_AttachesLastOutage pins stage 2b at the point that matters:
// the COLLECTOR attaches the summary, so the pulled getDeviceStatus reply AND
// the poller's pushed device_status feed (both call GetStatus) carry it —
// the backend sees the diagnosis without polling.
func TestGetStatus_AttachesLastOutage(t *testing.T) {
	t.Parallel()

	summary := &status.LastOutage{
		Start:    time.Unix(1700000000, 0).UTC(),
		End:      time.Unix(1700000300, 0).UTC(),
		Class:    "wan-down",
		Count24h: 3,
	}
	ds := lastOutageTestCollector(t, func() *status.LastOutage { return summary })

	resp, err := ds.GetStatus(context.Background())
	require.NoError(t, err)
	require.NotNil(t, resp.LastOutage)
	assert.Equal(t, "wan-down", resp.LastOutage.Class)
	assert.Equal(t, 3, resp.LastOutage.Count24h)

	// Golden payload shape for the wire (additive, exact keys — a rename here
	// is a backend-visible API break).
	raw, err := json.Marshal(resp.LastOutage)
	require.NoError(t, err)
	assert.JSONEq(t,
		`{"start":"2023-11-14T22:13:20Z","end":"2023-11-14T22:18:20Z","class":"wan-down","count24h":3}`,
		string(raw))
}

// TestGetStatus_LastOutageStableBetweenOutages: the summary source returns
// the same value until another outage closes, so consecutive GetStatus
// payloads marshal byte-identically for this field and the poller's MD5
// dedupe keeps suppressing no-change ticks (plan §5).
func TestGetStatus_LastOutageStableBetweenOutages(t *testing.T) {
	t.Parallel()

	summary := &status.LastOutage{
		Start:    time.Unix(1700000000, 0).UTC(),
		End:      time.Unix(1700000300, 0).UTC(),
		Class:    "dns-broken",
		Count24h: 1,
	}
	ds := lastOutageTestCollector(t, func() *status.LastOutage {
		copy := *summary
		return &copy
	})

	first, err := ds.GetStatus(context.Background())
	require.NoError(t, err)

	a, err := json.Marshal(first.LastOutage)
	require.NoError(t, err)
	b, err := json.Marshal(func() *status.LastOutage { c := *summary; return &c }())
	require.NoError(t, err)
	assert.Equal(t, string(a), string(b),
		"unchanged summary must marshal byte-identically so MD5 dedupe suppresses the tick")
}

// TestGetStatus_LastOutageOmittedWhenUnwired: absent seam (or no closed
// outage) omits the additive field entirely.
func TestGetStatus_LastOutageOmittedWhenUnwired(t *testing.T) {
	t.Parallel()

	ds := lastOutageTestCollector(t, func() *status.LastOutage { return nil })
	resp, err := ds.GetStatus(context.Background())
	require.NoError(t, err)
	assert.Nil(t, resp.LastOutage)

	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "lastOutage", "nil summary must omit the key")
}
