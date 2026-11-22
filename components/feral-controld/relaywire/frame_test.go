package relaywire

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestFrameBinaryRoundTrip(t *testing.T) {
	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(i)
	}
	h := Header{Version: 2, ID: "packet-1", To: "controller-1", From: "player-1"}
	frame, err := Encode(h, payload)
	if err != nil {
		t.Fatal(err)
	}
	got, body, err := Decode(frame)
	if err != nil || got != h || !bytes.Equal(body, payload) {
		t.Fatalf("round trip failed: %v", err)
	}
}

func TestFrameRejectsMalformedInput(t *testing.T) {
	wrap := func(header string) []byte {
		if len(header) > 65535 {
			t.Fatal("test header exceeds uint16 framing")
		}
		b := make([]byte, len(header)+2)
		binary.BigEndian.PutUint16(b, uint16(len(header))) // #nosec G115 -- bounded test header above.
		copy(b[2:], header)
		return b
	}
	cases := [][]byte{nil, {0}, {0, 0}, {0, 20, '{'}, make([]byte, MaxFrameBytes+1), wrap(`{"v":1,"id":"a","to":"b"}`), wrap(`{"v":2,"id":"a/b","to":"b"}`), wrap(`{"v":2,"id":"a","to":"b","from":"../x"}`), wrap("{\xff}"), wrap(strings.Repeat(" ", MaxHeaderBytes+1))}
	for i, frame := range cases {
		if _, _, err := Decode(frame); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestFrameSizeBoundary(t *testing.T) {
	h := Header{Version: 2, ID: "a", To: "b"}
	base, err := Encode(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Encode(h, make([]byte, MaxFrameBytes-len(base))); err != nil {
		t.Fatal(err)
	}
	if _, err := Encode(h, make([]byte, MaxFrameBytes-len(base)+1)); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func TestConnectionURL(t *testing.T) {
	got, err := ConnectionURL("ws://192.168.31.240:8787/", "FF1-SKYZ2E3A")
	if err != nil || got != "ws://192.168.31.240:8787/v2/channels/FF1-SKYZ2E3A/connections" {
		t.Fatalf("%s: %v", got, err)
	}
	for _, endpoint := range []string{"ws://host/api/connection", "ws://user:secret@host", "ws://host?topicID=secret", "ws://host#fragment", "http://host", "ws://host?"} {
		if _, err := ConnectionURL(endpoint, "FF1-test"); err == nil {
			t.Errorf("accepted %s", endpoint)
		}
	}
	for _, id := range []string{"", "../x", strings.Repeat("x", 129)} {
		if _, err := ConnectionURL("wss://host", id); err == nil {
			t.Error("invalid ID accepted")
		}
	}
}
