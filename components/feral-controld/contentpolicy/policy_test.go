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

// A truncated or half-written state file must not silently reset the owner's
// saved policy and then be reported active. Decoding into an already-defaulted
// policy accepted all of these, because the defaults filled in whatever the
// file omitted and the version check passed on the default's own version.
func TestOpenRejectsAnIncompletePolicyFile(t *testing.T) {
	for name, body := range map[string]string{
		"null":            `null`,
		"empty object":    `{}`,
		"version only":    `{"version":1}`,
		"missing a field": `{"version":1,"showMatureContent":true,"strictPersonal":false}`,
		"wrong version":   `{"version":2,"showMatureContent":false,"strictPersonal":false,"blockUnratedCurated":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "policy.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path, false); err == nil {
				t.Fatal("an incomplete policy file must be a load failure, not a silent reset")
			}
		})
	}
}

func TestOpenAcceptsACompletePolicyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	body := `{"version":1,"showMatureContent":true,"strictPersonal":true,"blockUnratedCurated":false}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	s.Lock()
	got, durable := s.CurrentLocked(), s.DurableLocked()
	s.Unlock()
	if !got.ShowMatureContent || !got.StrictPersonal || !durable {
		t.Fatalf("got %+v durable=%v", got, durable)
	}
}

// Unknown fields must be IGNORED, like every other state-file reader in this
// component. A newer build that adds a field must not make an older one reject
// the file and discard the owner's saved policy on a rollback. Completeness of
// the known v1 fields is still required.
func TestOpenIgnoresUnknownFieldsButStillRequiresTheKnownOnes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	body := `{"version":1,"showMatureContent":true,"strictPersonal":false,"blockUnratedCurated":false,"somethingNewer":{"a":1}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, false)
	if err != nil {
		t.Fatalf("an additive field must not discard the saved policy: %v", err)
	}
	s.Lock()
	got := s.CurrentLocked()
	s.Unlock()
	if !got.ShowMatureContent {
		t.Fatalf("saved policy lost: %+v", got)
	}

	// Trailing data is still rejected: that is a truncated or doubled write,
	// not an additive field.
	twice := filepath.Join(t.TempDir(), "twice.json")
	if err := os.WriteFile(twice, []byte(body+body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(twice, false); err == nil {
		t.Fatal("trailing JSON must still be a load failure")
	}
}

// Once the rename lands, the file holds the new values and a restart would load
// them — so memory must agree even if the parent-directory fsync afterwards
// fails. Reporting that as a failed update is the inconsistency: the caller
// would be told it did not apply, and a reboot would apply it.
func TestUpdateLockedCommitsWhenOnlyDirectoryDurabilityIsUncertain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	s, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}

	s.Lock()
	got, err := s.UpdateLocked(true, true)
	s.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !got.ShowMatureContent || !got.StrictPersonal {
		t.Fatalf("got %+v", got)
	}

	// Whatever persistAtomic reports after the rename, the file and memory must
	// describe the same policy — that is the invariant the split return exists
	// for, and it is what a restart depends on.
	reloaded, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.Lock()
	stored := reloaded.CurrentLocked()
	reloaded.Unlock()
	s.Lock()
	inMemory := s.CurrentLocked()
	s.Unlock()
	if stored != inMemory {
		t.Fatalf("file %+v and memory %+v disagree", stored, inMemory)
	}
}

func TestErrDurabilityUncertainIsDistinctFromAFailedWrite(t *testing.T) {
	// A write that never commits must NOT read as merely uncertain durability:
	// the handler treats the latter as success.
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
	s := Fallback(path, false)
	s.Lock()
	_, err := s.UpdateLocked(true, false)
	active := s.CurrentLocked()
	s.Unlock()
	if err == nil {
		t.Fatal("an uncommitted write must report an error")
	}
	if errors.Is(err, ErrDurabilityUncertain) {
		t.Fatalf("an uncommitted write must not be classified as uncertain durability: %v", err)
	}
	if active.ShowMatureContent {
		t.Fatal("an uncommitted write must not change the active policy")
	}
}

// The owner's audience setting falls with the claim on a factory reset: a reset
// that rolls back must not hand a resold frame the previous owner's admission
// rules. The operator gate survives, because it comes from device
// configuration, not from the owner being removed.
func TestResetLockedClearsTheOwnerSettingAndKeepsTheOperatorGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	s, err := Open(path, true) // operator gate on
	if err != nil {
		t.Fatal(err)
	}
	s.Lock()
	if _, err := s.UpdateLocked(true, true); err != nil {
		s.Unlock()
		t.Fatal(err)
	}
	s.Unlock()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("setup did not persist: %v", err)
	}

	s.Lock()
	after, err := s.ResetLocked()
	snapshot := s.Snapshot()
	s.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if after.ShowMatureContent || after.StrictPersonal {
		t.Fatalf("owner setting survived the reset: %+v", after)
	}
	if !after.BlockUnratedCurated {
		t.Fatal("the operator gate is device configuration and must survive")
	}
	if snapshot != after {
		t.Fatalf("the lock-free snapshot still reads %+v", snapshot)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the durable file must be gone so the next boot starts clean: %v", err)
	}

	// Reset on a device that never wrote a policy is not an error.
	s.Lock()
	_, err = s.ResetLocked()
	s.Unlock()
	if err != nil {
		t.Fatalf("reset with no file present: %v", err)
	}
}

// A present but unrecognized rating fails closed. Public ingress rejects such a
// document outright, but a playlist read back from player status never went
// through that validator, so an item retained from a pre-feature cast can carry
// any string — and admitting it merely because it is not the exact word
// "mature" would let a malformed label outlive a restrictive policy.
func TestAllowsFailsClosedOnAnUnrecognizedRating(t *testing.T) {
	bogus := contentrating.Rating("not-a-rating")
	item := dp1playlist.PlaylistItem{Source: "https://a", ContentRating: &bogus}

	for name, p := range map[string]Policy{
		"defaults":             Default(),
		"mature allowed":       {Version: Version, ShowMatureContent: true},
		"personal relaxed":     {Version: Version},
		"audit gate on":        {Version: Version, BlockUnratedCurated: true},
		"everything permitted": {Version: Version, ShowMatureContent: true, StrictPersonal: false},
	} {
		for _, origin := range []Context{ContextCurated, ContextPersonal} {
			if p.Allows(item, origin) {
				t.Fatalf("%s/%s admitted a malformed rating", name, origin)
			}
		}
	}

	// The known values are unaffected.
	general, mature := contentrating.RatingGeneral, contentrating.RatingMature
	if !Default().Allows(dp1playlist.PlaylistItem{Source: "https://a", ContentRating: &general}, ContextCurated) {
		t.Fatal("a general item must still be admitted")
	}
	if !(Policy{Version: Version, ShowMatureContent: true}).Allows(
		dp1playlist.PlaylistItem{Source: "https://a", ContentRating: &mature}, ContextCurated) {
		t.Fatal("an explicitly allowed mature item must still be admitted")
	}
	if !Default().Allows(dp1playlist.PlaylistItem{Source: "https://a"}, ContextCurated) {
		t.Fatal("an unrated item must still be admitted while the audit gate is off")
	}
}

// An unconfirmed directory fsync must keep EVERY read reporting unavailable,
// not just the write that hit it: otherwise the next getContentPolicy tells the
// owner the setting is saved while a power loss can still revert it.
func TestDurabilityStaysUnconfirmedUntilItIsConfirmed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	s, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}

	s.Lock()
	if _, err := s.UpdateLocked(true, false); err != nil {
		s.Unlock()
		t.Fatal(err)
	}
	durable := s.DurableLocked()
	s.Unlock()
	if !durable {
		t.Fatal("a fully confirmed write must read durable")
	}

	// Force the unconfirmed state the handler sees after a committed rename
	// whose directory fsync failed.
	s.Lock()
	s.durabilityUnconfirmed = true
	durable = s.DurableLocked()
	s.Unlock()
	if durable {
		t.Fatal("an unconfirmed write must not read durable on a LATER call")
	}

	s.Lock()
	confirmErr := s.ConfirmDurableLocked()
	durable = s.DurableLocked()
	s.Unlock()
	if confirmErr != nil {
		t.Fatal(confirmErr)
	}
	if !durable {
		t.Fatal("a successful confirmation must restore durable reads")
	}
}

// The reset's UNLINK has to be durable for the same reason the reset exists: a
// power loss before the directory entry is flushed can bring the previous
// owner's file back on a rolled-back reset.
func TestResetLockedReportsUnconfirmedDeletion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	s, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	s.Lock()
	if _, err := s.UpdateLocked(true, false); err != nil {
		s.Unlock()
		t.Fatal(err)
	}
	s.Unlock()

	// A normal reset confirms, so the store reads durable afterwards.
	s.Lock()
	_, err = s.ResetLocked()
	durable := s.DurableLocked()
	s.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !durable {
		t.Fatal("a confirmed reset must read durable")
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the file must be gone: %v", statErr)
	}
}

// While a previous write's directory entry is unconfirmed the file is not yet
// authoritative, so a repeated IDENTICAL set must not take the unchanged-write
// shortcut: that rewrite and its fsync are the retry. Taking the shortcut would
// report the setting as saved while a power loss could still revert it.
func TestUpdateLockedRetriesWhileDurabilityIsUnconfirmed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	s, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	s.Lock()
	if _, err := s.UpdateLocked(true, false); err != nil {
		s.Unlock()
		t.Fatal(err)
	}
	s.Unlock()

	// Enter the state a committed rename with a failed directory fsync leaves.
	s.Lock()
	s.durabilityUnconfirmed = true
	s.Unlock()

	// Removing the file makes the retry observable: the shortcut would leave it
	// missing, while a real re-persist puts it back.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	s.Lock()
	got, err := s.UpdateLocked(true, false) // identical values
	durable := s.DurableLocked()
	s.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !got.ShowMatureContent {
		t.Fatalf("got %+v", got)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("an identical set while unconfirmed must re-persist: %v", statErr)
	}
	if !durable {
		t.Fatal("a successful re-persist must clear the unconfirmed state")
	}
}
