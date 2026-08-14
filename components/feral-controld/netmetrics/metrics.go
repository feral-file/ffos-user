// Package netmetrics owns the stage-0 network-observability gauges served on
// the hub's /metrics route (docs/wan-outage-observability.md). The gauges are
// CACHE-ONLY by construction: a scrape reads whatever the last real probe or
// relayer event wrote — it never triggers nmcli or network work itself (the
// §4.7 snapshot rule extended to metrics). The values ride vmagent's offline
// disk spool, so they are the one record of an outage that survives it and
// reaches the backend on reconnect.
//
// The package deliberately has no dependency on relayer, status, or the
// provisioning machine: producers push values through the package-level
// setters (the same precedent as status's playback metrics), and the Poller
// consumes narrow, consumer-owned interfaces.
package netmetrics

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Link-state gauge values. Unknown is exported as -1 rather than omitting the
// sample or guessing 0: a probe failure mid-outage is exactly when the
// timeline matters most, and "we could not tell" must stay distinguishable
// from "link down" (the plan's no-fabricated-zero rule).
const (
	LinkDown    = 0.0
	LinkUp      = 1.0
	LinkUnknown = -1.0
)

var (
	registry = prometheus.NewRegistry()

	// netLinkState is per link type ("ethernet", "wifi"), value
	// LinkUp/LinkDown/LinkUnknown. GaugeVec so nothing is exported until the
	// first real probe result lands (a plain gauge would fabricate 0 samples
	// the moment the registry is scraped).
	netLinkState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "net_link_state",
		Help: "Local link state per type: 1 ACTIVATED, 0 not, -1 unknown (probe failed). Absent until the first probe. Wifi counts the device's own setup AP.",
	}, []string{"type"})

	// netWifiSignalPercent is NetworkManager's 0-100 signal quality for the
	// in-use BSS (nmcli SIGNAL — NM's percent scale, not raw dBm). Zero-label
	// vec so the series can be dropped (Reset) whenever the device is not
	// associated as a station: an absent sample means "not associated or
	// unknown", never a stale or fabricated strength.
	netWifiSignalPercent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "net_wifi_signal_percent",
		Help: "Wi-Fi signal quality (0-100, NetworkManager scale) of the associated BSS. Absent when not associated as a station.",
	}, []string{})

	// netWifiLinkInfo is an info-style gauge (constant 1) carrying the
	// associated BSS identity: bssid_hash (SALTED truncated SHA-256, so fleet
	// metrics never carry a raw or brute-force-recoverable BSSID — see
	// bssidSalt) and channel. Label cardinality is bounded by
	// the handful of BSSes one device actually associates with; stale label
	// sets are Reset away on every update so at most one series is live.
	netWifiLinkInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "net_wifi_link_info",
		Help: "Identity of the associated BSS: bssid_hash (salted truncated SHA-256, per-device salt) and channel. Constant 1; absent when not associated.",
	}, []string{"bssid_hash", "channel"})

	// relayerConnected is a plain gauge on purpose: 0 at process start is the
	// truth (the relayer is not connected yet), so eager export is correct
	// here, unlike the probe-backed gauges above.
	relayerConnected = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "relayer_connected",
		Help: "1 while the relayer websocket is connected, 0 otherwise.",
	})

	// relayerLastCloseCode is absent until the first websocket CloseError is
	// observed — "never saw a close code" and "close code 0" must not alias.
	// Correlating this with net_internet_reachable is the stage-0 payoff: a
	// middlebox killing the WSS while the probe stays green becomes visible.
	// Note the target failure mode (a blackholed WAN) tears the socket down
	// via i/o timeout with NO close frame, so in exactly that scenario this
	// gauge stays absent by construction; relayer_disconnects_total and the
	// netlog ladder class carry the signal there. Do not "fix" the absence.
	relayerLastCloseCode = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "relayer_last_close_code",
		Help: "Websocket close code of the most recent relayer close frame. Absent until one is observed.",
	}, []string{})

	// relayerDisconnectsTotal counts connection teardowns. The 60s scrape
	// cadence misses brief flaps entirely; the counter is what still moves.
	relayerDisconnectsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "relayer_disconnects_total",
		Help: "Total relayer websocket connection teardowns since process start.",
	})
)

func init() {
	registry.MustRegister(netLinkState, netWifiSignalPercent, netWifiLinkInfo,
		relayerConnected, relayerLastCloseCode, relayerDisconnectsTotal)
}

// Gatherer exposes the package registry for the hub's /metrics handler.
func Gatherer() prometheus.Gatherer {
	return registry
}

// SetLinkState records one link-probe outcome per type; pass LinkUnknown for
// both when the probe itself failed.
func SetLinkState(ethernet, wifi float64) {
	netLinkState.WithLabelValues("ethernet").Set(ethernet)
	netLinkState.WithLabelValues("wifi").Set(wifi)
}

// SetWifiStation records the associated BSS the last telemetry read saw.
func SetWifiStation(signalPercent float64, bssid string, channel string) {
	netWifiSignalPercent.WithLabelValues().Set(signalPercent)
	// Reset before Set so a BSS or channel change replaces the info series
	// instead of accumulating one series per BSS ever seen.
	netWifiLinkInfo.Reset()
	netWifiLinkInfo.WithLabelValues(hashBSSID(bssid), channel).Set(1)
}

// ClearWifiStation drops the station series: not associated (or not knowable)
// is exported as absence, never as a stale value.
func ClearWifiStation() {
	netWifiSignalPercent.Reset()
	netWifiLinkInfo.Reset()
}

// SetRelayerConnected records the relayer websocket connection state.
func SetRelayerConnected(connected bool) {
	v := 0.0
	if connected {
		v = 1.0
	}
	relayerConnected.Set(v)
}

// RecordRelayerDisconnect counts one connection teardown.
func RecordRelayerDisconnect() {
	relayerDisconnectsTotal.Inc()
}

// RecordRelayerCloseCode records the close code from a websocket close frame.
// Callers only invoke it when a real code was observed (see
// relayerLastCloseCode for why absence is meaningful).
func RecordRelayerCloseCode(code int) {
	relayerLastCloseCode.WithLabelValues().Set(float64(code))
}

// bssidSalt is a per-device secret mixed into the BSSID digest. An UNSALTED
// truncated hash of a 48-bit MAC is reversible by brute force (a known OUI
// leaves ~2^24 candidates), and a recovered BSSID is geolocatable through
// public wardriving databases — so without the salt the metric would carry
// venue-location data. /etc/machine-id gives a stable per-device value
// (series identity survives restarts); when unreadable, a random per-process
// salt preserves the privacy property at the cost of series identity across
// restarts — the right side of that trade.
var bssidSalt = sync.OnceValue(func() []byte {
	if id, err := os.ReadFile("/etc/machine-id"); err == nil && len(bytes.TrimSpace(id)) > 0 {
		return bytes.TrimSpace(id)
	}
	// Random per-process fallback: preserves the privacy property at the
	// cost of series identity across restarts. crypto/rand.Read is
	// guaranteed not to fail (Go >= 1.24: it crashes the program
	// irrecoverably rather than returning an error), so there is no state
	// in which a digest could be produced from a public salt.
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	return salt
})

// hashBSSID replaces a raw BSSID with a short salted digest: fleet metrics
// must not carry venue MAC addresses (see bssidSalt for why unsalted hashing
// would), but "did the BSS change across the outage" must stay answerable.
// 8 hex chars keep collision odds negligible at per-device cardinality.
func hashBSSID(bssid string) string {
	h := sha256.New()
	h.Write(bssidSalt())
	h.Write([]byte(bssid))
	return hex.EncodeToString(h.Sum(nil))[:8]
}
