package main

import (
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
)

// relayV2Eligible takes the derived endpoint (config.RelayV2Endpoint). Since
// every config with a relayer.endpoint yields one, the authority check here
// is what keeps devices without control state on the legacy relayer only.
func relayV2Eligible(endpoint string, authority *controlWiring, deviceID string) bool {
	return relayer.V2Eligible(endpoint, deviceID, authority != nil)
}

// The legacy handle shadows the package in initializeApp.
var newRelayV2 = relayer.NewV2
