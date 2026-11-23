package relayer

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/feral-file/ffos-user/components/feral-controld/relaywire"
)

func TestDropLogLogsTheFirstDropThenCountsWithinTheInterval(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	now := time.Unix(1_000, 0)
	d := newDropLog(zap.New(core), func() time.Time { return now })

	d.note(dropUnknownSource, zap.String("peer", "a"))
	for range 500 {
		d.note(dropUnknownSource, zap.String("peer", "a"))
	}
	// Another reason is not held back by the first.
	d.note(dropUnfinished, zap.String("peer", "b"))
	if got := logs.Len(); got != 2 {
		t.Fatalf("lines within the interval = %d, want 2", got)
	}
	first := logs.All()[0].ContextMap()
	if first["reason"] != dropUnknownSource || first["count"] != int64(1) || first["peer"] != "a" {
		t.Fatalf("first line = %v", first)
	}

	now = now.Add(dropLogInterval)
	d.note(dropUnknownSource, zap.String("peer", "a"))
	if got := logs.Len(); got != 3 {
		t.Fatalf("lines after the interval = %d, want 3", got)
	}
	if count := logs.All()[2].ContextMap()["count"]; count != int64(501) {
		t.Fatalf("count carried over the interval = %v, want 501", count)
	}
}

func TestDropLogIsSafeUnset(t *testing.T) {
	var d *dropLog
	d.note(dropBadFrame)
}

// A page suspended while it sends leaves a chunked request part way in; the
// device used to forget it without a line.
func TestDeviceSessionReportsARequestLeftUnfinished(t *testing.T) {
	type drop struct{ reason, peer string }
	chunk := func(index int) []byte {
		frame, err := relaywire.Encode(
			relaywire.Header{Version: 2, ID: "frame", To: "player", From: "browser"},
			[]byte(`{"relayChunk":{"id":"transfer","index":`+string(rune('0'+index))+`,"count":3,"data":"eA=="}}`))
		if err != nil {
			t.Fatal(err)
		}
		return frame
	}
	for name, end := range map[string]func(*deviceSession){
		"the sender disconnects": func(s *deviceSession) {
			_, _, _ = s.receive(websocket.TextMessage, []byte(`{"type":"peer_disconnected","connectionId":"browser"}`))
		},
		"the socket closes": func(s *deviceSession) { s.close() },
	} {
		t.Run(name, func(t *testing.T) {
			var drops []drop
			s := newDeviceSession(&fakeShedConn{})
			s.onDrop = func(reason, peer string) { drops = append(drops, drop{reason, peer}) }
			defer s.close()
			if _, _, err := s.receive(websocket.TextMessage, []byte(`{"type":"hello","peers":["browser","idle"]}`)); err != nil {
				t.Fatal(err)
			}
			for index := range 2 {
				if body, _, err := s.receive(websocket.BinaryMessage, chunk(index)); err != nil || body != nil {
					t.Fatalf("chunk %d: body=%v err=%v", index, body, err)
				}
			}
			if len(drops) != 0 {
				t.Fatalf("drops before the sender left = %v", drops)
			}
			end(s)
			if len(drops) != 1 || drops[0] != (drop{dropUnfinished, "browser"}) {
				t.Fatalf("drops = %v, want one unfinished request from browser", drops)
			}
		})
	}
}

func TestDeviceSessionReportsWhatItDiscards(t *testing.T) {
	type drop struct{ reason, peer string }
	var drops []drop
	s := newDeviceSession(&fakeShedConn{})
	s.onDrop = func(reason, peer string) { drops = append(drops, drop{reason, peer}) }
	defer s.close()
	if _, _, err := s.receive(websocket.TextMessage, []byte(`{"type":"hello","peers":["known"]}`)); err != nil {
		t.Fatal(err)
	}
	frame := func(from, payload string) []byte {
		out, err := relaywire.Encode(relaywire.Header{Version: 2, ID: "frame", To: "player", From: from}, []byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	_, _, _ = s.receive(websocket.BinaryMessage, frame("stranger", `{}`))
	_, _, _ = s.receive(websocket.BinaryMessage, frame("known", `{"relayChunk":{"id":"transfer","index":1,"count":2,"data":"eA=="}}`))
	_, _, _ = s.receive(websocket.BinaryMessage, []byte{0})
	// A whole request is dispatched, and one that completes is not a drop.
	if body, _, err := s.receive(websocket.BinaryMessage, frame("known", `{"messageID":"m"}`)); err != nil || body == nil {
		t.Fatalf("whole request: body=%v err=%v", body, err)
	}
	s.close()
	want := []drop{{dropUnknownSource, "stranger"}, {dropBadChunk, "known"}, {dropBadFrame, ""}}
	if len(drops) != len(want) {
		t.Fatalf("drops = %v, want %v", drops, want)
	}
	for i := range want {
		if drops[i] != want[i] {
			t.Fatalf("drops = %v, want %v", drops, want)
		}
	}
}
