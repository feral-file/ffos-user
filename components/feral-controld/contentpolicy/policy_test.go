package contentpolicy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/display-protocol/dp1-go/extension/contentrating"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"
)

func TestPolicyMatrix(t *testing.T) {
	general, mature := contentrating.RatingGeneral, contentrating.RatingMature
	items := map[string]dp1playlist.PlaylistItem{
		"general": {Source: "https://a", ContentRating: &general},
		"mature":  {Source: "https://a", ContentRating: &mature},
		"unrated": {Source: "https://a"},
	}
	tests := []struct {
		name string
		p    Policy
		c    Context
		item string
		want bool
	}{
		{"show all", Policy{ShowMatureContent: true}, ContextCurated, "mature", true},
		{"personal relaxed", Policy{}, ContextPersonal, "mature", true},
		{"personal strict mature", Policy{StrictPersonal: true}, ContextPersonal, "mature", false},
		{"curated mature", Policy{}, ContextCurated, "mature", false},
		{"curated unrated before audit", Policy{}, ContextCurated, "unrated", true},
		{"curated unrated after audit", Policy{BlockUnratedCurated: true}, ContextCurated, "unrated", false},
		{"general", Policy{StrictPersonal: true, BlockUnratedCurated: true}, ContextCurated, "general", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.Allows(items[tt.item], tt.c); got != tt.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestFilterDropsProjectionSignaturesAndBlocksEmpty(t *testing.T) {
	mature := contentrating.RatingMature
	p := &dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{{Source: "https://a"}, {Source: "https://b", ContentRating: &mature}}, Signature: "signed", Signatures: []dp1playlist.Signature{{Sig: "x"}}}
	out, err := Default().Filter(p, ContextCurated)
	if err != nil || len(out.Items) != 1 || out.Signature != "" || out.Signatures != nil {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	_, err = Default().Filter(&dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{{Source: "https://b", ContentRating: &mature}}}, ContextCurated)
	if !errors.Is(err, ErrContentBlocked) {
		t.Fatalf("got %v", err)
	}
}

func TestStorePersistsAtomicallyAndOperatorOwnsAuditGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	s, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	s.Lock()
	got, err := s.UpdateLocked(true, true)
	s.Unlock()
	if err != nil || !got.BlockUnratedCurated {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tmp remains: %v", err)
	}
	reloaded, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.Lock()
	got = reloaded.CurrentLocked()
	reloaded.Unlock()
	if !got.ShowMatureContent || !got.StrictPersonal || got.BlockUnratedCurated {
		t.Fatalf("got %+v", got)
	}
}

func TestFallbackStoreIsNotDurableUntilAWriteRepairsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	s := Fallback(path, false)
	s.Lock()
	durable := s.DurableLocked()
	s.Unlock()
	if durable {
		// A store built from an unreadable file must not claim the values it is
		// admitting on are the owner's saved setting.
		t.Fatal("fallback store reports durable")
	}

	s.Lock()
	// Identical-to-default values: the skip-unchanged shortcut must NOT apply
	// here, because this write is what repairs the store.
	_, err := s.UpdateLocked(false, false)
	durable = s.DurableLocked()
	s.Unlock()
	if err != nil || !durable {
		t.Fatalf("err=%v durable=%v", err, durable)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("repairing write did not reach disk: %v", err)
	}
}

func TestUpdateLockedSkipsTheFlashWriteWhenNothingChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	s, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	s.Lock()
	if _, err := s.UpdateLocked(true, true); err != nil {
		s.Unlock()
		t.Fatal(err)
	}
	s.Unlock()

	// Removing the file makes a second durable write observable: if the
	// unchanged set still wrote, the file comes back.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	s.Lock()
	got, err := s.UpdateLocked(true, true)
	s.Unlock()
	if err != nil || !got.ShowMatureContent || !got.StrictPersonal {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an unchanged set still rewrote the policy file: %v", err)
	}

	// A real change must still be written.
	s.Lock()
	_, err = s.UpdateLocked(false, true)
	s.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("changed set did not reach disk: %v", err)
	}
}
