package netmetrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Relay-retirement gauges (docs/api-design.md /metrics; Gate B of
// docs/ff1-control-v1.md §15.4). They answer the
// questions the legacy-relayer retirement gates ask of the fleet: is the
// device on v2, does it still depend on the legacy relayer, and what would a
// factory reset boot. relay_v2_connected is event-driven (the v2 socket's own
// lifecycle); the rest are read at scrape time from in-memory state or one
// local file, never from the network.

var (
	relayV2State struct {
		sync.RWMutex
		connected bool
	}

	relayV2Connected = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "relay_v2_connected",
		Help: "1 while the v2 device-ID relay websocket is connected, 0 otherwise.",
	})

	registerDeviceStateOnce sync.Once
)

func init() {
	registry.MustRegister(relayV2Connected)
}

// SetRelayV2Connected records the v2 relay socket state.
func SetRelayV2Connected(connected bool) {
	relayV2State.Lock()
	defer relayV2State.Unlock()
	relayV2State.connected = connected
	v := 0.0
	if connected {
		v = 1.0
	}
	relayV2Connected.Set(v)
}

// RelayV2Connected reports the cached v2 socket state for the LAN status.
func RelayV2Connected() bool {
	relayV2State.RLock()
	defer relayV2State.RUnlock()
	return relayV2State.connected
}

// DeviceStateSources are the scrape-time readers for the retirement gauges.
// Each must be cheap and local. A nil reader leaves its gauge unregistered.
type DeviceStateSources struct {
	LegacyRetired      func() bool
	LegacyTopicPresent func() bool
	Claimed            func() bool
	// ControlState is "legacy", "enrolled", "enforced", "locked", or "off"
	// when the control protocol is not running.
	ControlState func() string
	// ResetTargetVersion is the version a factory reset would boot, or
	// "unknown" when it cannot be read (counted as below G3 by the gates).
	ResetTargetVersion func() string
}

// RegisterDeviceState registers the retirement gauges once, at wiring time.
func RegisterDeviceState(src DeviceStateSources) {
	registerDeviceStateOnce.Do(func() {
		for _, g := range []struct {
			name, help string
			fn         func() bool
		}{
			{"relayer_legacy_retired", "1 while the legacy relayer is latched retired or not configured.", src.LegacyRetired},
			{"legacy_topic_present", "1 when a legacy relayer topic is stored.", src.LegacyTopicPresent},
			{"device_claimed", "1 when the device is claimed.", src.Claimed},
		} {
			if g.fn == nil {
				continue
			}
			fn := g.fn
			registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: g.name, Help: g.help}, func() float64 {
				if fn() {
					return 1
				}
				return 0
			}))
		}
		if src.ControlState != nil {
			registry.MustRegister(&labelInfo{
				desc: prometheus.NewDesc("control_state", "1 for the current control-protocol state.", []string{"state"}, nil),
				read: src.ControlState,
			})
		}
		if src.ResetTargetVersion != nil {
			registry.MustRegister(&labelInfo{
				desc: prometheus.NewDesc("reset_target_version_info", "1, labeled with the version a factory reset would boot.", []string{"version"}, nil),
				read: src.ResetTargetVersion,
			})
		}
	})
}

// labelInfo exports one constant-1 sample whose label is read at scrape time,
// so a changed value replaces the old series instead of leaving it behind.
type labelInfo struct {
	desc *prometheus.Desc
	read func() string
}

func (l *labelInfo) Describe(ch chan<- *prometheus.Desc) { ch <- l.desc }

func (l *labelInfo) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(l.desc, prometheus.GaugeValue, 1, l.read())
}
