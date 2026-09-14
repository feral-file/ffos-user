// Package contentpolicy owns the device's durable audience policy and pure
// playlist admission rules.
package contentpolicy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

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
	mu     sync.Mutex
	path   string
	policy Policy
}

// Fallback keeps safe default admission active when the authoritative file is
// unreadable. Policy RPCs remain unavailable because callers keep the load error.
func Fallback(path string, blockUnratedCurated bool) *Store {
	p := Default()
	p.BlockUnratedCurated = blockUnratedCurated
	return &Store{path: path, policy: p}
}

func Open(path string, blockUnratedCurated bool) (*Store, error) {
	s := &Store{path: path, policy: Default()}
	s.policy.BlockUnratedCurated = blockUnratedCurated
	b, err := os.ReadFile(path) //nolint:gosec // G304: production passes the fixed constant.CONTENT_POLICY_FILE; tests inject their own t.TempDir path.
	if err == nil {
		if err := jsonUnmarshalStrict(b, &s.policy); err != nil {
			return nil, fmt.Errorf("load content policy: %w", err)
		}
		if s.policy.Version != Version {
			return nil, fmt.Errorf("unsupported content policy version %d", s.policy.Version)
		}
		// Operator configuration, not the controller or stale persisted state,
		// owns this gate on every boot.
		s.policy.BlockUnratedCurated = blockUnratedCurated
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read content policy: %w", err)
	}
	return s, nil
}

func (s *Store) Lock()                 { s.mu.Lock() }
func (s *Store) Unlock()               { s.mu.Unlock() }
func (s *Store) CurrentLocked() Policy { return s.policy }

func (s *Store) UpdateLocked(showMature, strictPersonal bool) (Policy, error) {
	next := s.policy
	next.ShowMatureContent = showMature
	next.StrictPersonal = strictPersonal
	if err := persistAtomic(s.path, next); err != nil {
		return Policy{}, err
	}
	s.policy = next
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
