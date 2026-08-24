package netmetrics

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// fakeLinkProber returns canned LinkTelemetry verdicts.
type fakeLinkProber struct {
	wired, wifi bool
	err         error
}

func (f fakeLinkProber) LinkTelemetry(context.Context) (bool, bool, error) {
	return f.wired, f.wifi, f.err
}

// fakeExec/fakeCmd are hand-rolled rather than the central mocks package on
// purpose: mocks imports relayer, relayer imports this package for its gauge
// setters, so using mocks here would be an import cycle in tests.
type fakeExec struct {
	t      *testing.T
	output string
	err    error
}

func (f fakeExec) CommandContext(_ context.Context, name string, arg ...string) wrapper.ExecCmd {
	f.t.Helper()
	wantArgv := "nmcli -t -f IN-USE,SIGNAL,CHAN,BSSID device wifi list --rescan no"
	gotArgv := name + " " + strings.Join(arg, " ")
	require.Equal(f.t, wantArgv, gotArgv, "unexpected wifi-list argv")
	return fakeCmd{output: f.output, err: f.err}
}

type fakeCmd struct {
	output string
	err    error
}

func (c fakeCmd) String() string { return "fake nmcli" }
func (c fakeCmd) Run() error     { return c.err }
func (c fakeCmd) Start() error   { return c.err }
func (c fakeCmd) Wait() error    { return c.err }
func (c fakeCmd) Output() ([]byte, error) {
	if c.err != nil {
		return nil, c.err
	}
	return []byte(c.output), nil
}
func (c fakeCmd) CombinedOutput() ([]byte, error) { return c.Output() }
func (c fakeCmd) Pid() int                        { return 0 }

// wifiListExec builds the fake exec whose wifi-list read returns the given
// output (or error), verifying the exact argv tickWifiStation issues.
func wifiListExec(t *testing.T, output string, err error) wrapper.Exec {
	return fakeExec{t: t, output: output, err: err}
}

// TestTick drives one poll over faked probes and asserts the gauge outcomes,
// including the honest encodings: probe failure exports -1 (unknown), and a
// missing/unreadable station exports absence, never 0.
func TestTick(t *testing.T) {
	tests := []struct {
		name         string
		link         fakeLinkProber
		wifiOut      string
		wifiErr      error
		wantEthernet float64
		wantWifi     float64
		wantSignal   *float64
	}{
		{
			name:         "link probe failure exports unknown",
			link:         fakeLinkProber{err: errors.New("nmcli wedged")},
			wifiErr:      errors.New("nmcli wedged"),
			wantEthernet: LinkUnknown,
			wantWifi:     LinkUnknown,
		},
		{
			name:         "wired only, no station",
			link:         fakeLinkProber{wired: true},
			wifiOut:      " :52:11:11\\:22\\:33\\:44\\:55\\:66\n",
			wantEthernet: LinkUp,
			wantWifi:     LinkDown,
		},
		{
			name:         "associated station exports signal and identity",
			link:         fakeLinkProber{wifi: true},
			wifiOut:      " :52:11:11\\:22\\:33\\:44\\:55\\:66\n*:70:36:AA\\:BB\\:CC\\:DD\\:EE\\:FF\n",
			wantEthernet: LinkDown,
			wantWifi:     LinkUp,
			wantSignal:   f64(70),
		},
		{
			name: "wifi list read failure clears the station series",
			// AP mode / wedged NM: the list read failing must not leave a
			// stale station exported from an earlier tick.
			link:         fakeLinkProber{wifi: true},
			wifiErr:      errors.New("EOPNOTSUPP"),
			wantEthernet: LinkDown,
			wantWifi:     LinkUp,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Seed a station so every case also proves it clears (or is
			// replaced) rather than lingering.
			SetWifiStation(99, "FE:ED:FA:CE:BE:EF", "1")

			p := NewPoller(tt.link, wifiListExec(t, tt.wifiOut, tt.wifiErr), nil, zap.NewNop())
			p.tick(context.Background())

			series := gatherFamily(t, "net_link_state")
			require.Len(t, series, 2)
			byType := map[string]float64{}
			for _, m := range series {
				byType[labelValue(t, m, "type")] = m.GetGauge().GetValue()
			}
			assert.Equal(t, tt.wantEthernet, byType["ethernet"])
			assert.Equal(t, tt.wantWifi, byType["wifi"])

			if tt.wantSignal == nil {
				assert.Nil(t, gatherFamily(t, "net_wifi_signal_percent"))
				assert.Nil(t, gatherFamily(t, "net_wifi_link_info"))
			} else {
				assert.Equal(t, *tt.wantSignal, gaugeValue(t, "net_wifi_signal_percent"))
				info := gatherFamily(t, "net_wifi_link_info")
				require.Len(t, info, 1)
				assert.Equal(t, "36", labelValue(t, info[0], "channel"))
			}
		})
	}
}

func f64(v float64) *float64 { return &v }

func TestParseInUseStation(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   wifiStation
		found  bool
	}{
		{
			name:   "in-use row parsed with escaped BSSID",
			output: " :52:1:11\\:22\\:33\\:44\\:55\\:66\n*:70:36:AA\\:BB\\:CC\\:DD\\:EE\\:FF\n",
			want:   wifiStation{signal: 70, channel: "36", bssid: "AA:BB:CC:DD:EE:FF"},
			found:  true,
		},
		{
			name:   "no in-use row",
			output: " :52:1:11\\:22\\:33\\:44\\:55\\:66\n",
			found:  false,
		},
		{
			name:   "empty output",
			output: "",
			found:  false,
		},
		{
			name:   "unparsable signal on in-use row",
			output: "*:--:36:AA\\:BB\\:CC\\:DD\\:EE\\:FF\n",
			found:  false,
		},
		{
			name:   "empty BSSID on in-use row",
			output: "*:70:36:\n",
			found:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := parseInUseStation(tt.output)
			assert.Equal(t, tt.found, found)
			if tt.found {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestSplitTerseFields(t *testing.T) {
	tests := []struct {
		line string
		want []string
	}{
		{"*:70:36:AA\\:BB\\:CC\\:DD\\:EE\\:FF", []string{"*", "70", "36", "AA:BB:CC:DD:EE:FF"}},
		{"a:b\\\\c:d", []string{"a", "b\\c", "d"}},
		{"", []string{""}},
		{"trailing\\", []string{"trailing\\"}},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, splitTerseFields(tt.line), "line %q", tt.line)
	}
}
