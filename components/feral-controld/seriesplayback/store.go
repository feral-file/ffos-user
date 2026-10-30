// Package seriesplayback owns the device-level series-playback override:
// the one setting a controller can set to force every series on this device
// into a single playback mode, regardless of what each document asks for.
//
// This is deliberately its own small store rather than a field on
// contentpolicy.Policy. The two live in the same place for the same reason
// (feral-file/ffos-user#425's AC2: "where the device's content policy
// already lives") — on-disk, atomically, next to the device's other owner
// settings — but they are not the same KIND of setting: content policy is
// continuously enforced admission (every item display re-checks it, and the
// player must acknowledge a change before it is trusted — see
// contentpolicy.Store's ack-before-persist contract) while series playback
// is a one-shot CAST-TIME decision. Once a series is narrowed and sent, the
// player holds an ordinary DP-1 playlist with no notion of a series and
// nothing left to acknowledge. Folding this into Policy would require either
// giving it Policy's CDP round trip for no reason, or special-casing it out
// of Allows/Filter/Project — both worse than a second small file.
package seriesplayback

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
)

const Version = 1

// ErrDurabilityUncertain mirrors contentpolicy.ErrDurabilityUncertain: the
// write's rename committed, but the parent directory's fsync did not, so
// only survival across a power loss before the directory entry reaches disk
// is in doubt.
var ErrDurabilityUncertain = errors.New("series playback directory durability unconfirmed")

// Setting is the device-level override. Mode is nullable and that
// nullability IS the setting (AC2 of feral-file/ffos-user#425): nil means
// "follow the document", distinguishable from an explicit
// dp1.SeriesPlaybackFirst. There is no other field whose presence needs
// distinguishing from its zero value, so — unlike contentpolicy's
// wirePolicy — a single pointer field needs no separate "complete decode"
// shape to tell absent from zero.
type Setting struct {
	Version int                     `json:"version"`
	Mode    *dp1.SeriesPlaybackMode `json:"mode"`
}

func Default() Setting { return Setting{Version: Version} }

// Store serializes setting mutation. One file, one lock, same shape as
// contentpolicy.Store minus the player-acknowledgement machinery that
// setting does not need (see the package doc).
type Store struct {
	mu      sync.Mutex
	path    string
	setting Setting
	// durable is false while the on-disk setting could not be read. A
	// getSeriesPlayback answers unavailable rather than presenting the
	// in-memory default as a saved setting, mirroring
	// contentpolicy.Store.durable.
	durable bool
	// durabilityUnconfirmed mirrors contentpolicy.Store's field of the same
	// name: set when a write's rename committed but the parent-directory
	// fsync did not.
	durabilityUnconfirmed bool
	// snap is a lock-free copy for the one read path that must not take mu:
	// the commandrouter's cast-time resolution, which already holds other
	// locks across a CDP send and must not invert their order against this
	// store's own lock (mirrors contentpolicy.Store.snap).
	snap atomic.Pointer[Setting]
}

// Snapshot returns the current setting without taking the store lock.
func (s *Store) Snapshot() Setting {
	if v := s.snap.Load(); v != nil {
		return *v
	}
	return Default()
}

func (s *Store) publishSnapshot() {
	v := s.setting
	s.snap.Store(&v)
}

// Fallback keeps the store usable (mode nil, i.e. "follow the document") when
// the authoritative file cannot be read, reporting itself non-durable so
// getSeriesPlayback answers unavailable rather than presenting the fallback
// as saved.
func Fallback(path string) *Store {
	s := &Store{path: path, setting: Default()}
	s.publishSnapshot()
	return s
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, setting: Default(), durable: true}
	b, err := os.ReadFile(path) //nolint:gosec // G304: production passes the fixed constant.SERIES_PLAYBACK_FILE; tests inject their own t.TempDir path.
	if err == nil {
		var wire Setting
		if err := jsonUnmarshalStrict(b, &wire); err != nil {
			return nil, fmt.Errorf("load series playback setting: %w", err)
		}
		if wire.Version != Version {
			return nil, fmt.Errorf("unsupported series playback setting version %d", wire.Version)
		}
		if wire.Mode != nil {
			if _, ok := dp1.ParseSeriesPlaybackMode(string(*wire.Mode)); !ok {
				return nil, fmt.Errorf("invalid stored series playback mode %q", *wire.Mode)
			}
		}
		s.setting = wire
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read series playback setting: %w", err)
	}
	s.publishSnapshot()
	return s, nil
}

func (s *Store) Lock()   { s.mu.Lock() }
func (s *Store) Unlock() { s.mu.Unlock() }

func (s *Store) CurrentLocked() Setting { return s.setting }

// DurableLocked mirrors contentpolicy.Store.DurableLocked.
func (s *Store) DurableLocked() bool { return s.durable && !s.durabilityUnconfirmed }

// UpdateLocked persists mode (nil clears the override, i.e. "follow the
// document") and makes it the active setting. Unlike contentpolicy, there is
// no player to acknowledge first: the player never sees this setting, so a
// write here either commits or it does not — no second outcome to reconcile.
func (s *Store) UpdateLocked(mode *dp1.SeriesPlaybackMode) (Setting, error) {
	next := Setting{Version: Version, Mode: mode}
	if s.DurableLocked() && settingsEqual(next, s.setting) {
		return s.setting, nil
	}
	committed, err := persistAtomic(s.path, next)
	if !committed {
		return Setting{}, err
	}
	s.setting = next
	s.durable = true
	s.publishSnapshot()
	if err != nil {
		s.durabilityUnconfirmed = true
		return next, fmt.Errorf("%w: %w", ErrDurabilityUncertain, err)
	}
	s.durabilityUnconfirmed = false
	return next, nil
}

// ConfirmDurableLocked mirrors contentpolicy.Store.ConfirmDurableLocked.
func (s *Store) ConfirmDurableLocked() error {
	if err := syncDir(filepath.Dir(s.path)); err != nil {
		return err
	}
	s.durabilityUnconfirmed = false
	return nil
}

// ResetLocked returns the store to "follow the document" and removes the
// durable file, for a factory reset — the owner's series-playback preference
// must not outlive their claim, same as content policy and the device name.
func (s *Store) ResetLocked() (Setting, error) {
	next := Default()
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Setting{}, fmt.Errorf("remove series playback setting: %w", err)
	}
	s.setting = next
	s.durable = true
	s.publishSnapshot()
	if err := syncDir(filepath.Dir(s.path)); err != nil {
		s.durabilityUnconfirmed = true
		return next, fmt.Errorf("%w: %w", ErrDurabilityUncertain, err)
	}
	s.durabilityUnconfirmed = false
	return next, nil
}

func settingsEqual(a, b Setting) bool {
	if a.Version != b.Version {
		return false
	}
	if a.Mode == nil || b.Mode == nil {
		return a.Mode == b.Mode
	}
	return *a.Mode == *b.Mode
}

func syncDir(path string) error {
	dir, err := os.Open(path) //nolint:gosec // G304: the setting file's own parent directory.
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

// persistAtomic mirrors contentpolicy's persistAtomic exactly (tmp write,
// fsync, rename, directory fsync) — duplicated rather than shared because the
// two stores hold different value types and contentpolicy's helper is
// package-private; both are small and the pattern is pinned by each
// package's own durability tests.
func persistAtomic(path string, v Setting) (committed bool, err error) {
	b, err := json.Marshal(v)
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
	f, err := os.OpenFile(tmp, os.O_RDWR, 0o600) //nolint:gosec // G304: derived from constant.SERIES_PLAYBACK_FILE (t.TempDir in tests).
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
	dir, err := os.Open(filepath.Dir(path)) //nolint:gosec // G304: the setting file's own parent directory.
	if err != nil {
		return true, err
	}
	if err = dir.Sync(); err != nil {
		_ = dir.Close()
		return true, err
	}
	return true, dir.Close()
}

// jsonUnmarshalStrict mirrors contentpolicy's: rejects trailing data, ignores
// unknown fields so a newer build's added field never makes an older one
// discard the owner's saved setting.
func jsonUnmarshalStrict(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
