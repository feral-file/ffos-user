package main

import (
	"strings"

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
