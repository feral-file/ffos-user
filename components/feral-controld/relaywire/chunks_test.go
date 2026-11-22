package relaywire

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestChunksPreservePayloadAndBounds(t *testing.T) {
	body := append([]byte(`{"value":"`), bytes.Repeat([]byte("a"), 100000)...)
	body = append(body, []byte(`"}`)...)
	parts, err := Split("transfer-1", body)
	if err != nil {
		t.Fatal(err)
	}
	var a Assembler
	for i, part := range parts {
		got, err := a.Add(part, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if i == len(parts)-1 && !bytes.Equal(body, got) {
			t.Fatal("payload changed")
		}
		if len(part) > 60000 {
			t.Fatal("oversized frame")
		}
	}
	if _, err := Split("transfer-1", make([]byte, MaxMessageBytes+1)); err == nil {
		t.Fatal("oversized message accepted")
	}
	if _, err := a.Add(parts[1], time.Now()); err == nil {
		t.Fatal("out of order accepted")
	}
	if _, err := a.Add(parts[0], time.Now().Add(-4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Add(parts[1], time.Now()); err == nil {
		t.Fatal("expired accepted")
	}
	invalid, _ := json.Marshal(chunkEnvelope{&Chunk{ID: "x", Count: 129, Data: []byte("x")}})
	if _, err := a.Add(invalid, time.Now()); err == nil {
		t.Fatal("unbounded count accepted")
	}
}
