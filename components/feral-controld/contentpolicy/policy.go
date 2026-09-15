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

	"github.com/display-protocol/dp1-go/extension/contentrating"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"
)

const Version = 1

type Context string

const (
	ContextCurated  Context = "curated"
	ContextPersonal Context = "personal"
)

var ErrContentBlocked = errors.New("contentBlocked")

// ErrDurabilityUncertain marks a write whose rename COMMITTED but whose parent
// directory could not be fsynced afterwards. The new values are in the file and
// in memory; only their survival across a power loss before the directory entry
// reaches disk is in doubt. Callers treat it as success and log it, because the
// alternative — reporting failure — is the inconsistency, not the fix.
var ErrDurabilityUncertain = errors.New("content policy directory durability unconfirmed")

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
	// A present but UNRECOGNIZED rating fails closed, and is checked before any
	// allowance below. Public ingress rejects such a document outright
	// (playlistInvalid), but a playlist read back from player status never went
	// through that validator — an item retained from a pre-feature cast can
	// carry any string. Admitting it because it merely is not the exact word
	// "mature" would let a malformed label outlive a restrictive policy
	// indefinitely, which is the opposite of "a malformed label is never
	// silently treated as unrated".
	//
	// This is stricter than the player's mirror, deliberately and safely: the
	// daemon filters before sending, so a withheld item never reaches the
	// player at all. Divergence only matters in the other direction — the
	// daemon admitting what the player withholds.
	if item.ContentRating != nil && !isKnownRating(*item.ContentRating) {
		return false
	}
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

// isKnownRating reports whether r is a content-rating value this build
// understands. Kept next to Allows rather than inlined so the fail-closed rule
// has one definition.
func isKnownRating(r contentrating.Rating) bool {
	return r == contentrating.RatingGeneral || r == contentrating.RatingMature
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
	// durabilityUnconfirmed is set when a write's rename COMMITTED but its
	// parent-directory fsync did not. The values are in place and in memory,
	// but a power loss could still restore the previous file — so the RPCs must
	// not report the policy as saved until a confirmation succeeds. Cleared by
	// ConfirmDurableLocked.
	durabilityUnconfirmed bool
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
// been written to) the durable file AND that file's directory entry is
// confirmed. An unconfirmed write reads as not durable, so every RPC that
// reports the saved setting answers contentPolicyUnavailable until it is
// confirmed — not just the write that first hit the problem.
func (s *Store) DurableLocked() bool { return s.durable && !s.durabilityUnconfirmed }

// ResetLocked returns the store to factory defaults and removes the durable
// file, for a factory reset. The operator-owned audit gate is preserved: it
// comes from device configuration, not from the owner being removed.
//
// The file is deleted rather than rewritten with defaults, so the next boot
// takes the same path as a device that has never had a policy written.
func (s *Store) ResetLocked() (Policy, error) {
	next := Default()
	next.BlockUnratedCurated = s.policy.BlockUnratedCurated
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Policy{}, fmt.Errorf("remove content policy: %w", err)
	}
	s.policy = next
	s.durable = true
	s.publishSnapshot()
	// The UNLINK has to be durable too, for the reason the reset exists: a
	// power loss before the directory entry is flushed can bring the previous
	// owner's file back, and a rolled-back reset would then enforce their
	// audience policy on a resold device. An unconfirmed deletion keeps the
	// store reporting unavailable rather than claiming the default is saved.
	if err := syncDir(filepath.Dir(s.path)); err != nil {
		s.durabilityUnconfirmed = true
		return next, fmt.Errorf("%w: %w", ErrDurabilityUncertain, err)
	}
	s.durabilityUnconfirmed = false
	return next, nil
}

// ConfirmDurableLocked re-attempts the parent-directory fsync that makes an
// already-renamed policy file survive a power loss. It exists so a caller that
// saw ErrDurabilityUncertain can retry before deciding what to report: the
// content is in place either way, and this is the only thing still unconfirmed.
func (s *Store) ConfirmDurableLocked() error {
	if err := syncDir(filepath.Dir(s.path)); err != nil {
		return err
	}
	s.durabilityUnconfirmed = false
	return nil
}

// syncDir fsyncs a directory so a create or unlink within it is durable.
func syncDir(path string) error {
	dir, err := os.Open(path) //nolint:gosec // G304: the policy file's own parent directory.
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

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
	// the file is already the source of truth, which is DurableLocked rather
	// than the raw durable flag: while a previous write's directory entry is
	// unconfirmed the file is not yet authoritative, so an identical set has to
	// fall through and re-persist — that rewrite and its fsync ARE the retry,
	// and taking the shortcut instead would report the setting as saved while a
	// power loss could still revert it.
	if s.DurableLocked() && next == s.policy {
		return s.policy, nil
	}
	committed, err := persistAtomic(s.path, next)
	if !committed {
		return Policy{}, err
	}
	// The rename succeeded, so the file already holds these values and a
	// restart would load them. Memory must agree, or the daemon would keep
	// admitting by the old policy while the file says otherwise — and the
	// caller would be told an update failed that a reboot then applies.
	s.policy = next
	s.durable = true
	s.publishSnapshot()
	if err != nil {
		s.durabilityUnconfirmed = true
		return next, fmt.Errorf("%w: %w", ErrDurabilityUncertain, err)
	}
	s.durabilityUnconfirmed = false
	return next, nil
}

func (s *Store) FilterLocked(in *dp1playlist.Playlist, origin Context) (*dp1playlist.Playlist, error) {
	return s.policy.Filter(in, origin)
}

func (s *Store) ProjectLocked(in *dp1playlist.Playlist, origin Context) (*dp1playlist.Playlist, bool, error) {
	return s.policy.Project(in, origin)
}

// persistAtomic writes p to path. committed reports whether the rename that
// makes the new content visible has happened: once it has, the file holds p
// regardless of what the parent-directory fsync afterwards does, and the caller
// must not treat the update as not applied.
func persistAtomic(path string, p Policy) (committed bool, err error) {
	b, err := jsonMarshal(p)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return false, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return false, err
	}
	// Reopened only to fsync the bytes just written; the path is this store's
	// own fixed policy file plus a ".tmp" suffix, never a caller-supplied name.
	f, err := os.OpenFile(tmp, os.O_RDWR, 0o600) //nolint:gosec // G304: derived from constant.CONTENT_POLICY_FILE (t.TempDir in tests).
	if err != nil {
		return false, err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return false, err
	}
	if err = f.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, err
	}
	// Past this point the new content IS what the path resolves to. Everything
	// below only makes that visible entry durable across a power loss, so its
	// failures are reported WITH committed=true.
	dir, err := os.Open(filepath.Dir(path)) //nolint:gosec // G304: the policy file's own parent directory.
	if err != nil {
		return true, err
	}
	if err = dir.Sync(); err != nil {
		_ = dir.Close()
		return true, err
	}
	return true, dir.Close()
}
