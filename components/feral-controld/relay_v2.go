package main

import (
	"strings"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/config"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/netmetrics"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func relayV2Eligible(cfg *config.RelayV2Config, authority *controlWiring, deviceID string) bool {
	return cfg != nil && relayer.V2Eligible(cfg.Endpoint, deviceID, authority != nil)
}

// The legacy handle shadows the package in initializeApp.
var newRelayV2 = relayer.NewV2

// dialRelayV2 builds the two v2 carriers (spec §10): the device-ID channel
// always, and the keyed channel thumb(devSig) when the authority has an
// identity. Nothing is dialed here; wire starts both.
func (c *controlWiring) dialRelayV2(endpoint, deviceID string, dialer wrapper.WebSocketDialer, randomizer wrapper.Randomizer,
	clock wrapper.Clock, os wrapper.OS, json wrapper.JSON, logger *zap.Logger) {
	carrier := func(channel, name string) relayer.Relayer {
		return newRelayV2(endpoint, channel, dialer, randomizer, clock, os, json, logger.With(zap.String("v2_channel", name)))
	}
	c.v2 = carrier(deviceID, "device")
	if id := c.auth.Store().Identity(); id != nil {
		c.v2Keyed = carrier(id.Pins().Sig, "keyed")
	}
}

// v2Carriers lists the carriers this device dials.
func (c *controlWiring) v2Carriers() []relayer.Relayer {
	var out []relayer.Relayer
	if c == nil {
		return out
	}
	for _, v2 := range []relayer.Relayer{c.v2Keyed, c.v2} {
		if v2 != nil {
			out = append(out, v2)
		}
	}
	return out
}

// "v2 connected" has two meanings now that there are two carriers.
//
// v2AnyConnected is for notifications and status: a notification goes to
// whichever socket its controller is bound on, so either carrier counts.
//
// v2KeyedConnected is for the claim-QR remote-route wait: the phone that
// scans the code enrolls on the keyed channel, so only that carrier proves
// the device is reachable for it. The device-ID channel is public and can be
// held full by anyone who knows the ID; that must not change the answer.
func (c *controlWiring) v2AnyConnected() bool {
	for _, v2 := range c.v2Carriers() {
		if v2.IsConnected() {
			return true
		}
	}
	return false
}

func (c *controlWiring) v2KeyedConnected() bool {
	return c != nil && c.v2Keyed != nil && c.v2Keyed.IsConnected()
}

// v2Wakers returns the redial wake of every carrier (relayer.WakeV2). ok is
// false when a carrier lacks it: a rename broke the seam, and the caller
// reports it rather than waking only some.
func (c *controlWiring) v2Wakers() (wake func(), ok bool) {
	var wakers []func()
	for _, v2 := range c.v2Carriers() {
		w, isWaker := v2.(interface{ WakeV2() })
		if !isWaker {
			return nil, false
		}
		wakers = append(wakers, w.WakeV2)
	}
	return func() {
		for _, w := range wakers {
			w()
		}
	}, true
}

// registerRelayRetirementMetrics wires the scrape-time readers for the
// legacy-relayer retirement gauges (docs/api-design.md, /metrics).
func registerRelayRetirementMetrics(legacy relayer.Relayer, cw *controlWiring, os wrapper.OS) {
	src := netmetrics.DeviceStateSources{
		LegacyTopicPresent: func() bool { return state.ClaimSnapshot().TopicID != "" },
		Claimed:            func() bool { return state.ClaimSnapshot().Claimed },
		ControlState:       cw.controlStateName,
		ResetTargetVersion: func() string {
			data, err := os.ReadFile(constants.RESET_TARGET_VERSION_FILE)
			if v := strings.TrimSpace(string(data)); err == nil && v != "" {
				return v
			}
			return "unknown"
		},
	}
	if r, ok := legacy.(interface{ Retired() bool }); ok {
		src.LegacyRetired = r.Retired
	}
	netmetrics.RegisterDeviceState(src)
}
