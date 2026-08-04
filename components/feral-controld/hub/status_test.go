package hub

import (
	"testing"

	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

// TestDeviceStatusContractMatchesHubV2 pins the cross-package constant pair:
// the relayer/cast getDeviceStatus `contract` field and the hub's
// /api/v2/status contract are ONE firmware gate seen from two transports
// (docs/app-triggered-wifi-setup.md §4.2/§4.3) — the app treats either
// reading "2" as v2-capable, so the constants must never drift apart.
func TestDeviceStatusContractMatchesHubV2(t *testing.T) {
	if status.DeviceStatusContract != StatusContractV2 {
		t.Fatalf("status.DeviceStatusContract %q != hub.StatusContractV2 %q",
			status.DeviceStatusContract, StatusContractV2)
	}
}
