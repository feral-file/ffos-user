package playerlink

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestPresentationURLPreservesPhoneQRHintWithoutArtworkCredentials(t *testing.T) {
	s := Snapshot{Presented: &Occurrence{ID: "occurrence", Item: json.RawMessage(`{"source":"https://private.example/art?token=secret&step=qr"}`)}}
	if got := s.PresentationURL(); got != "ff-player://artwork/occurrence" || strings.Contains(got, "secret") {
		t.Fatal(got)
	}
	s.Overlay = &Overlay{Owner: "setup", State: "claim_qr"}
	u, err := url.Parse(s.PresentationURL())
	if err != nil || u.Query().Get("step") != "qr" {
		t.Fatalf("old phone claim QR hint: %s, %v", u, err)
	}
	s.Overlay.State = "joining"
	u, err = url.Parse(s.PresentationURL())
	if err != nil || u.Query().Get("step") != "" {
		t.Fatalf("stale QR hint: %s, %v", u, err)
	}
	s.Overlay = nil
	s.Presented = nil
	s.Sleeping = true
	if s.PresentationURL() != "ff-player://sleep" {
		t.Fatal(s.PresentationURL())
	}
	s.Sleeping = false
	if s.PresentationURL() != "ff-player://idle" {
		t.Fatal(s.PresentationURL())
	}
}
