package main

import (
	"context"
	"testing"

	"github.com/feral-file/godbus"
	"go.uber.org/zap"
)

// TestMediatorRoutesCDPStuckSignal pins the DBus wiring half of ffos-user#356:
// a payload shaped exactly like feral-controld's EVENT_CDP_STUCK (same member
// string — the two components cannot share the Go constant, see
// DBUS_CONTROLD_EVENT_CDP_STUCK's doc) must reach ChromiumMonitor.SetCDPStuck,
// and nothing else in the dispatch must choke on an unrelated member.
func TestMediatorRoutesCDPStuckSignal(t *testing.T) {
	monitor := NewChromiumMonitor("http://127.0.0.1:0", zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	mediator := NewMediator(nil, nil, nil, nil, nil, monitor, zap.NewNop())

	_, err := mediator.handleDBusSignal(context.Background(), godbus.DBusPayload{
		Member: DBUS_CONTROLD_EVENT_CDP_STUCK,
		Body:   []interface{}{true},
	})
	if err != nil {
		t.Fatalf("handleDBusSignal() error = %v", err)
	}

	monitor.mu.Lock()
	stuck := monitor.cdpStuck
	monitor.mu.Unlock()
	if !stuck {
		t.Fatal("expected SetCDPStuck(true) to be called via the DBus dispatch")
	}

	_, err = mediator.handleDBusSignal(context.Background(), godbus.DBusPayload{
		Member: DBUS_CONTROLD_EVENT_CDP_STUCK,
		Body:   []interface{}{false},
	})
	if err != nil {
		t.Fatalf("handleDBusSignal() error = %v", err)
	}
	monitor.mu.Lock()
	stuck = monitor.cdpStuck
	monitor.mu.Unlock()
	if stuck {
		t.Fatal("expected SetCDPStuck(false) to clear it")
	}
}

// TestMediatorCDPStuckSignalMalformedBodyIsIgnored pins that a malformed
// payload (wrong arity, wrong type) is logged and dropped rather than
// panicking the dispatch goroutine — matching the existing sysmetrics/sysevent
// cases' own validation shape.
func TestMediatorCDPStuckSignalMalformedBodyIsIgnored(t *testing.T) {
	monitor := NewChromiumMonitor("http://127.0.0.1:0", zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	mediator := NewMediator(nil, nil, nil, nil, nil, monitor, zap.NewNop())

	cases := []godbus.DBusPayload{
		{Member: DBUS_CONTROLD_EVENT_CDP_STUCK, Body: nil},
		{Member: DBUS_CONTROLD_EVENT_CDP_STUCK, Body: []interface{}{"not-a-bool"}},
		{Member: DBUS_CONTROLD_EVENT_CDP_STUCK, Body: []interface{}{true, true}},
	}
	for _, payload := range cases {
		if _, err := mediator.handleDBusSignal(context.Background(), payload); err != nil {
			t.Fatalf("handleDBusSignal() error = %v", err)
		}
	}

	monitor.mu.Lock()
	stuck := monitor.cdpStuck
	monitor.mu.Unlock()
	if stuck {
		t.Fatal("a malformed payload must never flip cdpStuck")
	}
}
