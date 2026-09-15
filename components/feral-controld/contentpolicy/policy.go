// Package contentpolicy owns the device's durable audience policy and pure
// playlist admission rules.
package contentpolicy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
)

const Version = 1

type Context string

const (
	ContextCurated  Context = "curated"
	ContextPersonal Context = "personal"
)

var ErrContentBlocked = errors.New("contentBlocked")

// Policy is the complete device/player mirror contract. BlockUnratedCurated
// is an operator audit gate and must not be populated from a controller request.
type Policy struct {
	Version             int  `json:"version"`
	ShowMatureContent   bool `json:"showMatureContent"`
	StrictPersonal      bool `json:"strictPersonal"`
	BlockUnratedCurated bool `json:"blockUnratedCurated"`
}

func Default() Policy { return Policy{Version: Version} }

// NormalizeRequestContext is NormalizeContext for PUBLIC ingress, where the
// difference between an absent field and an explicitly empty one is real: the
// documented contract is that omitting contentContext means curated, while its
// only valid present values are "curated" and "personal". Treating an explicit
// "" or null as omitted would silently widen a malformed controller request
// into the default audience instead of telling the caller it is malformed.
//
// Internal callers (restored scheduler state, the refresher, replay) keep using
// NormalizeContext: they carry a value, not a request.
func NormalizeRequestContext(v any, present bool) (Context, error) {
	if !present {
		return ContextCurated, nil
	}
	if v == nil || v == "" {
		return "", fmt.Errorf("contentContext must be %q or %q when present", ContextCurated, ContextPersonal)
	}
	return NormalizeContext(v)
}

func NormalizeContext(v any) (Context, error) {
	if v == nil || v == "" {
		return ContextCurated, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("contentContext must be a string")
	}
	switch Context(s) {
	case ContextCurated, ContextPersonal:
		return Context(s), nil
	default:
		return "", fmt.Errorf("invalid contentContext %q", s)
	}
}

func (p Policy) Allows(item dp1playlist.PlaylistItem, origin Context) bool {
	if p.ShowMatureContent || (origin == ContextPersonal && !p.StrictPersonal) {
		return true
	}
	if item.ContentRating != nil && *item.ContentRating == "mature" {
		return false
	}
	// An unrated item is withheld only when the operator gate is on and the item
	// came from a curated source; unrated personal content stays admissible.
	if item.ContentRating == nil && origin == ContextCurated && p.BlockUnratedCurated {
		return false
	}
	return true
}

// Filter creates an internal playback projection. When items are removed its
// signatures are cleared: the source document's signatures do not describe
// the projection, even though rating fields themselves remain signed in the source.
func (p Policy) Filter(in *dp1playlist.Playlist, origin Context) (*dp1playlist.Playlist, error) {
	out, blocked, err := p.Project(in, origin)
	if err != nil {
		return nil, err
	}
	if blocked {
		return nil, ErrContentBlocked
	}
	return out, nil
}

// Project also returns an empty projection. Refresh uses that shape to retire
// content newly labeled blocked; independent casts use Filter and do not send it.
func (p Policy) Project(in *dp1playlist.Playlist, origin Context) (*dp1playlist.Playlist, bool, error) {
	if in == nil {
		return nil, false, fmt.Errorf("nil playlist")
	}
	out := *in
	out.Items = make([]dp1playlist.PlaylistItem, 0, len(in.Items))
	for _, item := range in.Items {
		if p.Allows(item, origin) {
			out.Items = append(out.Items, item)
		}
	}
	if len(out.Items) == 0 {
		out.Signatures = nil
		out.Signature = ""
		return &out, true, nil
	}
	if len(out.Items) != len(in.Items) {
		out.Signatures = nil
		out.Signature = ""
	}
	return &out, false, nil
}

// Store serializes policy mutation and playlist admission. Holding this lock
// across the player's acknowledgement makes the accepted policy and casts one
// order instead of two independently-racing views.
type Store struct {
	mu   sync.Mutex
	path string
	// policy is the acknowledged admission policy: the one casts, refreshes and
	// scheduler cutovers are filtered against, and the only thing this file ever
	// holds. A value the player has not acknowledged is never written and never
	// enforced, so there is no second state to keep, to persist, or to restore
	// after a restart.
	policy Policy
	// durable is false while the on-disk policy could not be read. Admission
	// keeps running on safe defaults, but the RPCs must not claim the saved
	// user setting is in force, because it is unknown. A successful
	// UpdateLocked writes the file and repairs the state.
	durable bool
	// snap is a lock-free copy of policy for readers that MUST NOT take mu.
	// Load-bearing, not an optimization: the command handler holds mu for the
	// whole displayPlaylist call, including the scheduler's WithPlayerPush, so
	// a scheduler-owned push that took mu to read the policy would invert the
	// lock order (mu -> pushMu here, pushMu -> mu there) and deadlock. Written
	// only under mu, so it never goes backwards.
	snap atomic.Pointer[Policy]
}

// Snapshot returns the current policy WITHOUT taking the store lock, for
// readers on the scheduler's push path. See the snap field for why that path
// must not take mu.
func (s *Store) Snapshot() Policy {
	if p := s.snap.Load(); p != nil {
		return *p
	}
	return Default()
}

func (s *Store) publishSnapshot() {
	p := s.policy
	s.snap.Store(&p)
}

// Fallback keeps safe default admission active when the authoritative file is
// unreadable. The store reports itself non-durable so getContentPolicy answers
// contentPolicyUnavailable rather than presenting defaults as the saved setting.
func Fallback(path string, blockUnratedCurated bool) *Store {
	p := Default()
	p.BlockUnratedCurated = blockUnratedCurated
	s := &Store{path: path, policy: p}
	s.publishSnapshot()
	return s
}

// DurableLocked reports whether the policy in memory came from (or has since
// been written to) the durable file.
func (s *Store) DurableLocked() bool { return s.durable }

// CandidateLocked builds the policy a set request is asking for WITHOUT
// changing or persisting anything. The caller sends this to the player and only
// commits it once the current generation acknowledges exactly these values.
func (s *Store) CandidateLocked(showMature, strictPersonal bool) Policy {
	next := s.policy
	next.ShowMatureContent = showMature
	next.StrictPersonal = strictPersonal
	return next
}

// wirePolicy is the on-disk shape decoded into ZERO values, with every field a
// pointer so "absent" is distinguishable from "false".
//
// Decoding straight into an already-defaulted Policy accepted `null`, `{}` and
// `{"version":1}` as valid: the defaults supplied whatever the file omitted,
// and the version check passed because the default already carried version 1.
// A truncated or half-written state file would then silently reset the owner's
// saved policy AND be reported active. A complete object is now required, and
// anything short of one is a load failure, which surfaces as
// contentPolicyUnavailable.
type wirePolicy struct {
	Version             *int  `json:"version"`
	ShowMatureContent   *bool `json:"showMatureContent"`
	StrictPersonal      *bool `json:"strictPersonal"`
	BlockUnratedCurated *bool `json:"blockUnratedCurated"`
}

// ParseComplete decodes a v1 policy object and requires EVERY field to be
// present. Exported for the player-acknowledgement check, which must not accept
// a partial reply: decoding into a plain Policy, `{"version":1}` alone comes
// back as the all-false default and compares equal to it, so a player that
// echoed nothing would look like it had acknowledged the default policy.
func ParseComplete(b []byte) (Policy, error) {
	var wire wirePolicy
	if err := jsonUnmarshalStrict(b, &wire); err != nil {
		return Policy{}, err
	}
	return wire.policy()
}

func (w wirePolicy) policy() (Policy, error) {
	switch {
	case w.Version == nil:
		return Policy{}, fmt.Errorf("content policy is missing version")
	case w.ShowMatureContent == nil:
		return Policy{}, fmt.Errorf("content policy is missing showMatureContent")
	case w.StrictPersonal == nil:
		return Policy{}, fmt.Errorf("content policy is missing strictPersonal")
	case w.BlockUnratedCurated == nil:
		return Policy{}, fmt.Errorf("content policy is missing blockUnratedCurated")
	}
	if *w.Version != Version {
		return Policy{}, fmt.Errorf("unsupported content policy version %d", *w.Version)
	}
	return Policy{
		Version:             *w.Version,
		ShowMatureContent:   *w.ShowMatureContent,
		StrictPersonal:      *w.StrictPersonal,
		BlockUnratedCurated: *w.BlockUnratedCurated,
	}, nil
}

func Open(path string, blockUnratedCurated bool) (*Store, error) {
	s := &Store{path: path, policy: Default(), durable: true}
	s.policy.BlockUnratedCurated = blockUnratedCurated
	b, err := os.ReadFile(path) //nolint:gosec // G304: production passes the fixed constant.CONTENT_POLICY_FILE; tests inject their own t.TempDir path.
	if err == nil {
		var wire wirePolicy
		if err := jsonUnmarshalStrict(b, &wire); err != nil {
			return nil, fmt.Errorf("load content policy: %w", err)
		}
		stored, err := wire.policy()
		if err != nil {
			return nil, fmt.Errorf("load content policy: %w", err)
		}
		s.policy = stored
		// Operator configuration, not the controller or stale persisted state,
		// owns this gate on every boot.
		s.policy.BlockUnratedCurated = blockUnratedCurated
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read content policy: %w", err)
	}
	s.publishSnapshot()
	return s, nil
}

func (s *Store) Lock()   { s.mu.Lock() }
func (s *Store) Unlock() { s.mu.Unlock() }

// TryLock reports whether the store lock was free, acquiring it if so. It
// exists so a caller can ASSERT lock state without blocking — specifically the
// lock-order test that pins "content policy before the kiosk playback lock" on
// the cast path. Do not use it to skip work: a policy read that silently
// no-ops when the lock is busy would answer from a policy nobody applied.
func (s *Store) TryLock() bool { return s.mu.TryLock() }

func (s *Store) CurrentLocked() Policy { return s.policy }

// UpdateLocked commits a policy the player has ALREADY acknowledged: it writes
// the file and makes the values the active admission policy.
//
// Called after the acknowledgement, never before. Persisting first and then
// discovering the player refused would leave a caller told the update failed
// with a device that had nevertheless changed its admission behavior — and a
// restart would then promote that refused policy, since the file is the only
// thing a restart can restore. Writing only acknowledged values keeps those two
// impossible without a second persisted state to reconcile.
//
// If the write fails after the player accepted, the daemon keeps the old policy
// and reports the failure; the next reconnect sync re-pushes the stored policy
// and puts the player back in step.
func (s *Store) UpdateLocked(showMature, strictPersonal bool) (Policy, error) {
	next := s.CandidateLocked(showMature, strictPersonal)
	// A repeated identical set must not cost a flash write. Skipped only when
	// the file is already the source of truth: on a non-durable store the same
	// values still have to be written, because that write is what repairs it.
	if s.durable && next == s.policy {
		return s.policy, nil
	}
	if err := persistAtomic(s.path, next); err != nil {
		return Policy{}, err
	}
	s.policy = next
	s.durable = true
	s.publishSnapshot()
	return next, nil
}

func (s *Store) FilterLocked(in *dp1playlist.Playlist, origin Context) (*dp1playlist.Playlist, error) {
	return s.policy.Filter(in, origin)
}

func (s *Store) ProjectLocked(in *dp1playlist.Playlist, origin Context) (*dp1playlist.Playlist, bool, error) {
	return s.policy.Project(in, origin)
}

func persistAtomic(path string, p Policy) error {
	b, err := jsonMarshal(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	// Reopened only to fsync the bytes just written; the path is this store's
	// own fixed policy file plus a ".tmp" suffix, never a caller-supplied name.
	f, err := os.OpenFile(tmp, os.O_RDWR, 0o600) //nolint:gosec // G304: derived from constant.CONTENT_POLICY_FILE (t.TempDir in tests).
	if err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err = dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}
