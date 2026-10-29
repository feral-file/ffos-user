package main

import (
	"github.com/feral-file/ffos-user/components/feral-controld/config"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
)

func relayV2Eligible(cfg *config.RelayV2Config, authority *controlWiring, deviceID string) bool {
	return cfg != nil && relayer.V2Eligible(cfg.Endpoint, deviceID, authority != nil)
}

// The legacy handle shadows the package in initializeApp.
var newRelayV2 = relayer.NewV2
