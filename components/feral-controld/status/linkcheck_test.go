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
		CommandContext(gomock.Any(), "nmcli", "-t", "-f",
			"GENERAL.DEVICE,GENERAL.TYPE,GENERAL.STATE,GENERAL.CONNECTION", "device", "show").
		Return(cmd).
		AnyTimes()
	return exec
}

// dev renders one terse `device show` block in the requested field order, the
// shape linkProbe parses.
func dev(device, typ, state, conn string) string {
	return "GENERAL.DEVICE:" + device + "\nGENERAL.TYPE:" + typ +
		"\nGENERAL.STATE:" + state + "\nGENERAL.CONNECTION:" + conn + "\n"
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
			name: "station association counts",
			output: dev("wlan0", "wifi", "100 (connected)", "HomeWifi") +
				dev("lo", "loopback", "100 (connected (externally))", "lo"),
			want: true,
		},
		{
			name: "ethernet counts",
			output: dev("eth0", "ethernet", "100 (connected)", "Wired connection 1") +
				dev("wlan0", "wifi", "30 (disconnected)", ""),
			want: true,
		},
		{
			name: "own hotspot alone does not count",
			output: dev("wlan0", "wifi", "100 (connected)", "ff1-softap") +
				dev("eth0", "ethernet", "20 (unavailable)", ""),
			want: false,
		},
		{
			name: "own hotspot plus real ethernet still counts",
			output: dev("wlan0", "wifi", "100 (connected)", "ff1-softap") +
				dev("eth0", "ethernet", "100 (connected)", "Wired connection 1"),
			want: true,
		},
		{
			name: "no connected devices",
			output: dev("wlan0", "wifi", "30 (disconnected)", "") +
				dev("eth0", "ethernet", "20 (unavailable)", ""),
			want: false,
		},
		{
			name:   "connecting is not a link",
			output: dev("wlan0", "wifi", "70 (connecting (getting IP configuration))", "HomeWifi"),
			want:   false,
		},
		{
			// The comparison keys on the leading numeric enum, never the
			// parenthetical word — nmcli localizes the word ("verbunden"), and
			// an English text match would read every healthy link on a
			// non-English locale as CONFIRMED absence, the one verdict that
			// authorizes raising the setup AP.
			name:   "localized state text is ignored, enum decides",
			output: dev("eth0", "ethernet", "100 (verbunden)", "Wired connection 1"),
			want:   true,
		},
		{
			name:   "localized non-connected state stays a non-link",
			output: dev("wlan0", "wifi", "30 (getrennt)", ""),
			want:   false,
		},
		{
			// Terse mode backslash-escapes ':' inside values; Cut at the first
			// ':' must keep the whole name intact rather than truncating at
			// the escaped colon and mis-reading the value.
			name:   "colon in connection name stays intact",
			output: dev("wlan0", "wifi", "100 (connected)", `Guest\:Net`),
			want:   true,
		},
		{
			// The malformed block (no parseable numeric state) is skipped; the
			// verdict comes from the block that did survey. No-candidate
			// output is an error instead — see
			// TestExternalLinkUnsurveyedOutputIsError.
			name: "unparseable state is not surveyed",
			output: dev("eth0", "ethernet", "connected", "Wired connection 1") +
				dev("wlan0", "wifi", "30 (disconnected)", ""),
			want: false,
		},
		{
			// NM >= 1.36 renders externally-managed devices with a decorated
			// word, but the enum stays 100 — a healthy wire must never read as
			// CONFIRMED absence.
			name:   "externally-managed connected counts",
			output: dev("eth0", "ethernet", "100 (connected (externally))", "Wired connection 1"),
			want:   true,
		},
		{
			// nmcli emits the requested field order, but nothing may break if
			// it ever did not: blocks are evaluated whole, not positionally.
			name: "field order within a block does not matter",
			output: "GENERAL.DEVICE:eth0\nGENERAL.STATE:100 (connected)\n" +
				"GENERAL.TYPE:ethernet\nGENERAL.CONNECTION:Wired connection 1\n",
			want: true,
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

// TestExternalLinkUnsurveyedOutputIsError pins the parser's failure bias: a
// "no link" verdict counts as CONFIRMED only when the survey saw at least one
// candidate uplink (an ethernet/wifi block with a parseable numeric state).
// Anything less proved nothing, so it must surface as a probe failure
// (ExternalLink's caller defers the AP) rather than as (false, nil) — a
// CONFIRMED absence that would authorize raising the setup AP over a
// possibly-healthy link. HasLink keeps failing closed to false either way.
func TestExternalLinkUnsurveyedOutputIsError(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{name: "garbage output", output: "garbage output"},
		// NetworkManager reports no devices at all. Unreachable on FF1 (the
		// wifi radio is always listed, even unmanaged) but pinned
		// deliberately.
		{name: "empty output", output: ""},
		{
			// Parseable rows that are all non-uplink devices establish
			// nothing about ethernet/wifi state — they must not be mistaken
			// for a survey that confirmed link absence.
			name: "loopback and p2p only",
			output: dev("lo", "loopback", "100 (connected (externally))", "lo") +
				dev("p2p-dev-wlan0", "wifi-p2p", "30 (disconnected)", ""),
		},
		{
			name:   "candidate row with unparseable state",
			output: dev("wlan0", "wifi", "disconnected", ""),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lc := status.NewLinkChecker(probeExec(t, tt.output, nil), zap.NewNop())
			_, err := lc.ExternalLink(context.Background(), "ff1-softap")
			assert.Error(t, err, "an unsurveyed probe must read as unknown, not confirmed absence")
			assert.False(t, lc.HasLink(context.Background()))
		})
	}
}

// TestWiredLink pins the WiredLink verdict semantics from
// docs/network-recovery-ux.md constraint 6: the verdict is computed from
// ethernet rows only, and a valid survey (at least one ethernet/wifi row —
// the same `surveyed` rule as the other probes) with no ethernet row is
// confirmed-no-wire (false, nil).
func TestWiredLink(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name: "activated ethernet is a wire",
			output: dev("eth0", "ethernet", "100 (connected)", "Wired connection 1") +
				dev("wlan0", "wifi", "30 (disconnected)", ""),
			want: true,
		},
		{
			// The assertion that catches the ExternalLink conflation
			// (docs/app-triggered-wifi-setup.md constraint 5): an associated
			// station is a LINK but never a WIRE. A probe reusing the combined
			// verdict would return true here and reject every Wi-Fi target
			// device of the setup flow.
			name:   "wifi-only station is confirmed no-wire",
			output: dev("wlan0", "wifi", "100 (connected)", "HomeWifi"),
			want:   false,
		},
		{
			// A valid survey whose listing simply has no ethernet device at all
			// (Wi-Fi-only hardware) is a confirmed no-wire verdict, not an
			// error — constraint 6's no-ethernet-row case.
			name:   "no ethernet row at all is confirmed no-wire",
			output: dev("wlan0", "wifi", "30 (disconnected)", ""),
			want:   false,
		},
		{
			name: "non-activated ethernet is not a wire",
			output: dev("eth0", "ethernet", "20 (unavailable)", "") +
				dev("wlan0", "wifi", "100 (connected)", "HomeWifi"),
			want: false,
		},
		{
			name: "wire plus station still reads as wired",
			output: dev("eth0", "ethernet", "100 (connected)", "Wired connection 1") +
				dev("wlan0", "wifi", "100 (connected)", "HomeWifi"),
			want: true,
		},
		{
			// The device's own hotspot is a wifi row; it must neither create a
			// wire verdict nor mask an ethernet row.
			name: "own hotspot is not a wire",
			output: dev("wlan0", "wifi", "100 (connected)", "ff1-softap") +
				dev("eth0", "ethernet", "20 (unavailable)", ""),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lc := status.NewLinkChecker(probeExec(t, tt.output, nil), zap.NewNop())
			got, err := lc.WiredLink(context.Background())
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestWiredLinkErrors pins WiredLink's surface-the-error bias: a probe failure
// or an unsurveyed listing (corrupt/empty output — the shared `surveyed` rule)
// must surface as an error, never as a confirmed verdict in either direction.
// The admission caller fails closed on error; the escape-policy window treats
// it as a pause.
func TestWiredLinkErrors(t *testing.T) {
	t.Run("probe failure surfaces", func(t *testing.T) {
		lc := status.NewLinkChecker(probeExec(t, "", errors.New("nmcli timeout")), zap.NewNop())
		_, err := lc.WiredLink(context.Background())
		assert.Error(t, err)
	})
	for _, tt := range []struct {
		name   string
		output string
	}{
		{name: "corrupt output", output: "garbage output"},
		{name: "empty output", output: ""},
		{
			name: "loopback and p2p only",
			output: dev("lo", "loopback", "100 (connected (externally))", "lo") +
				dev("p2p-dev-wlan0", "wifi-p2p", "30 (disconnected)", ""),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lc := status.NewLinkChecker(probeExec(t, tt.output, nil), zap.NewNop())
			_, err := lc.WiredLink(context.Background())
			assert.Error(t, err, "an unsurveyed probe must read as unknown, not confirmed no-wire")
		})
	}
}

// TestHasLinkCountsOwnHotspot pins that HasLink deliberately does NOT exclude
// the setup hotspot: a phone joined to the hotspot is a LAN peer, so mDNS/hub
// discoverability stays keyed on it. Only the provisioning guard needs the
// exclusion, via ExternalLink.
func TestHasLinkCountsOwnHotspot(t *testing.T) {
	exec := probeExec(t, dev("wlan0", "wifi", "100 (connected)", "ff1-softap"), nil)
	lc := status.NewLinkChecker(exec, zap.NewNop())
	assert.True(t, lc.HasLink(context.Background()))
}

// TestLinkTelemetry covers the per-type telemetry verdicts feeding the
// netmetrics gauges: raw (no hotspot exclusion — a radio ACTIVATED as the
// device's own AP reads wifi=true, honest during an outage), and errors
// surface so the caller exports "unknown" rather than a fabricated 0.
func TestLinkTelemetry(t *testing.T) {
	tests := []struct {
		name      string
		output    string
		wantWired bool
		wantWifi  bool
		wantErr   bool
	}{
		{
			name: "ethernet and station both activated",
			output: dev("eth0", "ethernet", "100 (connected)", "Wired connection 1") +
				dev("wlan0", "wifi", "100 (connected)", "HomeWifi"),
			wantWired: true,
			wantWifi:  true,
		},
		{
			name: "own hotspot counts as wifi",
			output: dev("wlan0", "wifi", "100 (connected)", "ff1-softap") +
				dev("eth0", "ethernet", "20 (unavailable)", ""),
			wantWifi: true,
		},
		{
			name: "nothing activated",
			output: dev("eth0", "ethernet", "20 (unavailable)", "") +
				dev("wlan0", "wifi", "30 (disconnected)", ""),
		},
		{
			name:    "unsurveyed output surfaces as error",
			output:  dev("lo", "loopback", "100 (connected (externally))", "lo"),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lc := status.NewLinkChecker(probeExec(t, tt.output, nil), zap.NewNop())
			wired, wifi, err := lc.LinkTelemetry(context.Background())
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.wantWired, wired, "wired")
			assert.Equal(t, tt.wantWifi, wifi, "wifi")
		})
	}
}

// TestLinkTelemetryProbeError: an exec failure must surface, not default.
func TestLinkTelemetryProbeError(t *testing.T) {
	lc := status.NewLinkChecker(probeExec(t, "", errors.New("nmcli exploded")), zap.NewNop())
	_, _, err := lc.LinkTelemetry(context.Background())
	assert.Error(t, err)
}
