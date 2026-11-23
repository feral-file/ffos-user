package relayer

import (
	"errors"
	"strconv"
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
			[]byte(`{"relayChunk":{"id":"transfer","index":`+strconv.Itoa(index)+`,"count":3,"data":"eA=="}}`))
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

func TestDeviceSessionReportsATransferItsSenderReplaced(t *testing.T) {
	type drop struct{ reason, peer string }
	var drops []drop
	s := newDeviceSession(&fakeShedConn{})
	s.onDrop = func(reason, peer string) { drops = append(drops, drop{reason, peer}) }
	if _, _, err := s.receive(websocket.TextMessage, []byte(`{"type":"hello","peers":["browser"]}`)); err != nil {
		t.Fatal(err)
	}
	chunk := func(transfer string, index int) []byte {
		frame, err := relaywire.Encode(
			relaywire.Header{Version: 2, ID: "frame", To: "player", From: "browser"},
			[]byte(`{"relayChunk":{"id":"`+transfer+`","index":`+strconv.Itoa(index)+`,"count":2,"data":"eyJhIjoxfQ=="}}`))
		if err != nil {
			t.Fatal(err)
		}
		return frame
	}
	// A is begun and never finished; B begins over it and completes.
	_, _, _ = s.receive(websocket.BinaryMessage, chunk("transfer-a", 0))
	if len(drops) != 0 {
		t.Fatalf("drops after one chunk = %v", drops)
	}
	_, _, _ = s.receive(websocket.BinaryMessage, chunk("transfer-b", 0))
	if len(drops) != 1 || drops[0] != (drop{dropUnfinished, "browser"}) {
		t.Fatalf("drops after the replacing chunk = %v", drops)
	}
	body, _, err := s.receive(websocket.BinaryMessage, chunk("transfer-b", 1))
	if err != nil || body == nil {
		t.Fatalf("completed transfer: body=%v err=%v", body, err)
	}
	// A transfer that completed leaves nothing to report when its sender goes.
	s.close()
	if len(drops) != 1 {
		t.Fatalf("drops after a completed transfer and close = %v", drops)
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

// failingAckConn refuses every write, as a socket does once its far end is gone.
type failingAckConn struct{ fakeShedConn }

func (c *failingAckConn) WriteMessage(int, []byte) error { return errors.New("write: broken pipe") }

func TestDeviceSessionReportsARequestWhoseReceiptCouldNotBeWritten(t *testing.T) {
	type drop struct{ reason, peer string }
	var drops []drop
	s := newDeviceSession(&failingAckConn{})
	s.onDrop = func(reason, peer string) { drops = append(drops, drop{reason, peer}) }
	defer s.close()
	if _, _, err := s.receive(websocket.TextMessage, []byte(`{"type":"hello","peers":["browser"]}`)); err != nil {
		t.Fatal(err)
	}
	frame, err := relaywire.Encode(relaywire.Header{Version: 2, ID: "frame", To: "player", From: "browser"}, []byte(`{"messageID":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	if body, _, err := s.receive(websocket.BinaryMessage, frame); err == nil || body != nil {
		t.Fatalf("request dispatched without its receipt: body=%v err=%v", body, err)
	}
	if len(drops) != 1 || drops[0] != (drop{dropReceiptFailed, "browser"}) {
		t.Fatalf("drops = %v", drops)
	}
}

// A frame whose receipt was being written while the socket closed must not
// land in an assembly nobody will read again.
func TestDeviceSessionReportsAFrameThatArrivesAsTheSessionCloses(t *testing.T) {
	type drop struct{ reason, peer string }
	var drops []drop
	s := newDeviceSession(&fakeShedConn{})
	s.onDrop = func(reason, peer string) { drops = append(drops, drop{reason, peer}) }
	if _, _, err := s.receive(websocket.TextMessage, []byte(`{"type":"hello","peers":["browser"]}`)); err != nil {
		t.Fatal(err)
	}
	s.close()
	frame, err := relaywire.Encode(relaywire.Header{Version: 2, ID: "frame", To: "player", From: "browser"},
		[]byte(`{"relayChunk":{"id":"transfer","index":0,"count":2,"data":"eA=="}}`))
	if err != nil {
		t.Fatal(err)
	}
	if body, _, err := s.receive(websocket.BinaryMessage, frame); err == nil || body != nil {
		t.Fatalf("frame accepted after close: body=%v err=%v", body, err)
	}
	if len(drops) != 1 || drops[0] != (drop{dropSessionClosed, "browser"}) {
		t.Fatalf("drops = %v", drops)
	}
}
