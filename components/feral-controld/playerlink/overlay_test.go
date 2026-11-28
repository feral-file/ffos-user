package playerlink

import (
	"context"
	"testing"
)

func TestCPPSetupOverlay(t *testing.T) {
	client := New(startCPPHost(t))
	defer func() { _ = client.Close() }()
	ctx := context.Background()
	if err := client.SetOverlay(ctx, "setup", "softap_qr", map[string]any{"ssid": "FF1-test", "password": "secret;test", "portal_url": "http://10.42.0.1"}); err != nil {
		t.Fatal(err)
	}
	var state Snapshot
	if err := client.Call(ctx, "state.get", nil, &state, nil); err != nil {
		t.Fatal(err)
	}
	if state.Overlay == nil || state.Overlay.State != "softap_qr" {
		t.Fatalf("missing native overlay: %+v", state.Overlay)
	}
	if err := client.SetOverlay(ctx, "setup", "ready", nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(ctx, "state.get", nil, &state, nil); err != nil {
		t.Fatal(err)
	}
	if state.Overlay != nil {
		t.Fatalf("ready retained overlay: %+v", state.Overlay)
	}
	if err := client.SetOverlay(ctx, "setup", "unknown", nil); err == nil {
		t.Fatal("unknown state silently accepted")
	}
}
