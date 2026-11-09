package hub

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/netmetrics"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
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

// A healthy LAN must not hide a dropped relay socket on either status route.
func TestStatusRelayerLifecycle(t *testing.T) {
	h := &hub{logger: zap.NewNop(), json: wrapper.NewJSON(), statusProvider: fixedStatusProvider{info: StatusInfo{Connectivity: "connected", Internet: true}}}
	netmetrics.SetRelayerConnected(false)
	defer netmetrics.SetRelayerConnected(false)
	for _, route := range []string{"/api/status", "/api/v2/status"} {
		var last string
		for _, connected := range []bool{false, true, false} {
			netmetrics.SetRelayerConnected(connected)
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", route, nil)
			if route == "/api/status" {
				h.handleStatus(w, req)
			} else {
				h.handleStatusV2(w, req)
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["relayer_connected"] != connected || body["connectivity"] != "connected" || body["internet"] != true {
				t.Fatalf("unexpected status: %s", w.Body.String())
			}
			stamp, _ := body["relayer_last_connected_at"].(string)
			if connected {
				if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
					t.Fatal(err)
				}
				last = stamp
			} else if last != "" && stamp != last {
				t.Fatalf("disconnect lost last-connected timestamp: %s", w.Body.String())
			}
		}
	}
}
