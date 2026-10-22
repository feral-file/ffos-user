package relayer

import "testing"

func TestV2Eligibility(t *testing.T) {
	for _, tc := range []struct {
		endpoint, id    string
		authority, want bool
	}{
		{"", "FF1-SKYZ2E3A", true, false},
		{"wss://relay.test", "FF1-SKYZ2E3A", false, false},
		{"wss://relay.test", "FF1", true, false},
		{"wss://relay.test", "FF1-UNKNOWN", true, false},
		{"wss://relay.test", "FF1-SKYZ2E3A", true, true},
	} {
		if got := V2Eligible(tc.endpoint, tc.id, tc.authority); got != tc.want {
			t.Errorf("%+v: %v", tc, got)
		}
	}
}

func TestPeerDisconnectHookRetiresGeneration(t *testing.T) {
	s := newDeviceSession(nil)
	s.peers["one"] = &relayPeer{}
	s.peers["two"] = &relayPeer{}
	one := Peer{relaySource{s, "one"}}
	two := Peer{relaySource{s, "two"}}
	gone := map[Peer]bool{}
	s.onDisconnect = func(p Peer) {
		gone[p] = true
		if p.Live() {
			t.Error("callback before retirement")
		}
	}
	_, _, err := s.receive(1, []byte(`{"type":"peer_disconnected","connectionId":"one"}`))
	if err != nil || one.Live() || !two.Live() || !gone[one] {
		t.Fatal("peer retirement failed", err)
	}
	s.close()
	if two.Live() || !gone[two] {
		t.Fatal("generation did not clear all peers")
	}
}
