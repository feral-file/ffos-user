package mediahealth

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
	"go.uber.org/zap"
)

// Open opens the store at path, checks every page of it, and replaces a
// file it cannot trust with a fresh, empty store. Losing the observations is
// the accepted cost: they are re-learned by playing and probing.
//
// Why this exists: a damaged bbolt file does not always fail with an error.
// Measured by corrupting a store page by page (1,200 cases): it can panic;
// on a page id past the end of the mmap it takes a fault the runtime cannot
// recover from (`fatal error: fault`); a write transaction over a damaged
// freelist loops forever; and tx.Check over a page cycle loops while
// allocating more than 1 GB within seconds. controld opens the store before
// provisioning, the boot OTA gate, SoftAP and the LAN hub start, and runs
// under Restart=always with no start limit, so one torn file would crash or
// hang it on every start and leave the device with no setup, no hub and no
// way to be reached: in-process recovery cannot help with a fault or a loop.
//
// So, in order:
//   - The open runs inside a marker file (path + ".opening"), removed only
//     once the store has passed the check and opened. A marker found at
//     start means the previous open died part-way; the file is deleted
//     before it is touched again. Deleted, not kept: nothing would read a
//     copy (uploadLogs ships only ~/.logs, sshd is off by default) and it
//     would hold a store's worth of disk.
//   - The check (tx.Check, a walk of every page) runs read-only BEFORE the
//     first write transaction. A check that reports damage deletes the
//     file and a fresh store is opened. A check that will not stop
//     (ErrStoreCheckRunaway) deletes the file and the caller exits, since
//     its goroutine cannot be stopped.
//
// The daemon restarts at most once per damaged file, and the hub is up on
// the next start.
//
// The check runs at open only. An unclean shutdown (the FF1's normal power
// loss) leaves no marker and keeps the store: bbolt's copy-on-write pages
// and fsynced meta make a cut write the previous commit, not damage. Damage
// that appears after a clean open is outside this guard.
//
// Not deleted: a store from a newer schema (a rollback; the newer binary
// reads it again after the next update), a lock timeout, and permission or
// missing-path errors. Those return the error and media health runs without
// a store, as before. Any other open error (ENOSPC or EIO included) deletes
// the file: losing re-learnable observations is cheaper than a store that
// may be damaged.
func Open(path string, opts Options) (*Store, error) {
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("media health: create store dir: %w", err)
	}
	marker := path + ".opening"
	if _, err := os.Stat(marker); err == nil {
		discardStore(path, "a previous open of the store did not finish", nil, logger)
	}
	// Best effort: without the marker the open is merely unguarded, which
	// is how it ran before this guard existed.
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		logger.Warn("media health: could not write the store open marker", zap.Error(err))
	}
	s, runaway, err := openChecked(path, opts)
	if runaway {
		// Deleted now, so the restart the caller forces opens a fresh store.
		// The marker is removed below: the file it guards is gone.
		discardStore(path, "the store integrity check would not stop", err, logger)
	} else if err != nil && discardable(err) {
		discardStore(path, "the store failed to open or its integrity check", err, logger)
		s, runaway, err = openChecked(path, opts)
	}
	if rmErr := os.Remove(marker); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		logger.Warn("media health: could not remove the store open marker", zap.Error(rmErr))
	}
	if runaway && !errors.Is(err, ErrStoreCheckRunaway) {
		err = fmt.Errorf("%w: %w", ErrStoreCheckRunaway, err)
	}
	return s, err
}

// ErrStoreCheckRunaway is Open's error when the integrity check of a
// damaged store would not stop. The file has been deleted, but the
// check's goroutine cannot be stopped from outside and keeps allocating
// (more than 1 GB within seconds on a page cycle, measured), so the process
// must exit: the caller logs and exits non-zero, and systemd's restart opens
// a fresh store. One restart per damaged file, as for a crash.
var ErrStoreCheckRunaway = errors.New("media health: store integrity check did not stop on a damaged store")

// checkTimeout bounds the whole page walk; checkGrace bounds how long the
// walk may run on once it has reported damage. A healthy store of any
// realistic size (~300 B per record) checks in milliseconds; a damaged one
// either finishes or loops, and a loop is what both bounds catch.
var (
	checkTimeout = 5 * time.Second
	checkGrace   = 500 * time.Millisecond
)

// openChecked walks every page of an existing store read-only (checkPages),
// then opens it. The check comes first because openStore's first write
// transaction touches the freelist: on a damaged freelist that commit loops
// forever (measured), before any check could run.
func openChecked(path string, opts Options) (s *Store, runaway bool, err error) {
	if runaway, err := checkPages(path); runaway || err != nil {
		return nil, runaway, err
	}
	s, err = openStore(path, opts)
	return s, false, err
}

// checkPages opens path read-only and walks every page (tx.Check). A missing
// file has nothing to check. A page that makes the walk panic or fault kills
// the process; the marker in Open turns that into a deletion on the next
// start. A walk that will not stop is abandoned (runaway): its read
// transaction stays open under the check's goroutine, so the db is neither
// rolled back nor closed, and the caller exits.
func checkPages(path string) (runaway bool, err error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		return false, fmt.Errorf("media health: open store for check: %w", err)
	}
	tx, err := db.Begin(false)
	if err != nil {
		_ = db.Close()
		return false, fmt.Errorf("media health: begin store check: %w", err)
	}
	errs := tx.Check()
	var checkErr error
	deadline := time.NewTimer(checkTimeout)
	defer deadline.Stop()
	var grace <-chan time.Time
	for {
		select {
		case e, open := <-errs:
			if !open {
				_ = tx.Rollback()
				_ = db.Close()
				if checkErr != nil {
					return false, fmt.Errorf("media health: store integrity check: %w", checkErr)
				}
				return false, nil
			}
			if checkErr == nil {
				checkErr = e
				grace = time.After(checkGrace)
			}
		case <-grace:
			return true, fmt.Errorf("%w: %w", ErrStoreCheckRunaway, checkErr)
		case <-deadline.C:
			return true, fmt.Errorf("%w: no result within %s", ErrStoreCheckRunaway, checkTimeout)
		}
	}
}

// discardable reports whether an open error may be about the file's
// contents, which a fresh store fixes, rather than about this binary or the
// path's permissions, which it would not.
func discardable(err error) bool {
	switch {
	case errors.Is(err, errNewerSchema), errors.Is(err, bolterrors.ErrTimeout):
		return false
	case errors.Is(err, fs.ErrPermission), errors.Is(err, fs.ErrNotExist):
		return false
	}
	return true
}

func discardStore(path, why string, cause error, logger *zap.Logger) {
	err := os.Remove(path)
	switch {
	case err == nil:
		logger.Error("media health: damaged store deleted; starting a fresh one",
			zap.String("why", why), zap.Error(cause))
	case errors.Is(err, fs.ErrNotExist):
		// Nothing was written yet: the previous open died before creating it.
	default:
		logger.Error("media health: could not delete the damaged store",
			zap.String("why", why), zap.Error(cause), zap.NamedError("remove_error", err))
	}
}
