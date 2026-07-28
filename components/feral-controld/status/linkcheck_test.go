package status_test

import (
	"context"
	"errors"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

// probeExec wires a MockExec whose nmcli device probe returns the given output
// (or error), matching the exact argv linkProbe issues.
func probeExec(t *testing.T, output string, err error) *mocks.MockExec {
	t.Helper()
	ctrl := gomock.NewController(t)
	cmd := mocks.NewMockExecCmd(ctrl)
	if err != nil {
		cmd.EXPECT().Output().Return(nil, err).AnyTimes()
	} else {
		cmd.EXPECT().Output().Return([]byte(output), nil).AnyTimes()
	}
	exec := mocks.NewMockExec(ctrl)
	exec.EXPECT().
		CommandContext(gomock.Any(), "nmcli", "-t", "-f", "DEVICE,TYPE,STATE,CONNECTION", "device").
		Return(cmd).
		AnyTimes()
	return exec
}

// TestExternalLink covers the hotspot-exclusion probe the provisioning
// AP-trigger guard keys on: the device's own setup hotspot (matched by active
// connection name) must never count as an external link — including the
// window where a failed teardown leaves it connected — while a real ethernet
// link or station association on any device must.
func TestExternalLink(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "station association counts",
			output: "wlan0:wifi:connected:HomeWifi\nlo:loopback:unmanaged:",
			want:   true,
		},
		{
			name:   "ethernet counts",
			output: "eth0:ethernet:connected:Wired connection 1\nwlan0:wifi:disconnected:",
			want:   true,
		},
		{
			name:   "own hotspot alone does not count",
			output: "wlan0:wifi:connected:ff1-softap\neth0:ethernet:unavailable:",
			want:   false,
		},
		{
			name:   "own hotspot plus real ethernet still counts",
			output: "wlan0:wifi:connected:ff1-softap\neth0:ethernet:connected:Wired connection 1",
			want:   true,
		},
		{
			name:   "no connected devices",
			output: "wlan0:wifi:disconnected:\neth0:ethernet:unavailable:",
			want:   false,
		},
		{
			name:   "connecting is not a link",
			output: "wlan0:wifi:connecting (configuring):HomeWifi",
			want:   false,
		},
		{
			// NM >= 1.36 renders externally-managed devices this way; an
			// exact-equality match would read this healthy wire as CONFIRMED
			// absence — the one verdict that authorizes raising the AP.
			name:   "externally-managed connected counts",
			output: "eth0:ethernet:connected (externally):Wired connection 1",
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lc := status.NewLinkChecker(probeExec(t, tt.output, nil), zap.NewNop())
			got, err := lc.ExternalLink(context.Background(), "ff1-softap")
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestExternalLinkSurfacesProbeError pins the failure bias split between the
// two probes: ExternalLink must SURFACE an nmcli failure (its caller treats
// unknown as "defer the AP", because a false 'no link' authorizes a
// destructive raise), while HasLink keeps failing closed to false (its mDNS
// caller must not advertise on a link it cannot confirm).
func TestExternalLinkSurfacesProbeError(t *testing.T) {
	exec := probeExec(t, "", errors.New("nmcli timeout"))
	lc := status.NewLinkChecker(exec, zap.NewNop())

	_, err := lc.ExternalLink(context.Background(), "ff1-softap")
	assert.Error(t, err, "ExternalLink must report the probe failure, not guess")

	assert.False(t, lc.HasLink(context.Background()),
		"HasLink keeps the fail-closed bias for the advertiser")
}

// TestHasLinkCountsOwnHotspot pins that HasLink deliberately does NOT exclude
// the setup hotspot: a phone joined to the hotspot is a LAN peer, so mDNS/hub
// discoverability stays keyed on it. Only the provisioning guard needs the
// exclusion, via ExternalLink.
func TestHasLinkCountsOwnHotspot(t *testing.T) {
	exec := probeExec(t, "wlan0:wifi:connected:ff1-softap", nil)
	lc := status.NewLinkChecker(exec, zap.NewNop())
	assert.True(t, lc.HasLink(context.Background()))
}
