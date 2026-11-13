package main

import (
	"testing"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// main.go wires the mediator's online edge to the v2 carrier by type
// assertion; a rename would silently bring back minutes of v2 downtime per
// network change (feral-file#3551 G3 bench F2).
func TestRelayV2CarrierIsWakeable(t *testing.T) {
	v2 := newRelayV2("wss://relay.test", "FF1-SKYZ2E3A", wrapper.NewWebSocketDialer(websocket.DefaultDialer),
		wrapper.NewRandomizer(), wrapper.NewClock(), wrapper.NewOS(), wrapper.NewJSON(), zap.NewNop())
	if _, ok := v2.(interface{ WakeV2() }); !ok {
		t.Fatal("the v2 carrier no longer implements WakeV2")
	}
}
