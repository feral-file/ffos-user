package offlinecache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	go_os "os"
	"path/filepath"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

var (
	ErrItemNotFound     = errors.New("offline cache: item not found")
	ErrPlaylistNotFound = errors.New("offline cache: playlist not found")
	ErrBlobNotFound     = errors.New("offline cache: blob not found")
	ErrBlobHashMismatch = errors.New("offline cache: blob content does not match its sha256 name")
	// ErrBlobTooLarge is returned by WriteBlob when the source reader
	// produces more than the maxBytes cap passed to it.
	ErrBlobTooLarge = errors.New("offline cache: blob exceeds size limit")
)

// blobFilePerm/recordFilePerm are intentionally not group/world-writable:
// the store lives under the feralfile user's cache dir and nothing else on
// the device needs to modify it.
const (
	dirPerm        go_os.FileMode = 0o755
	blobFilePerm   go_os.FileMode = 0o644
	recordFilePerm go_os.FileMode = 0o644
)

// Store implements the canonical on-disk format from the plan: a shared
// content-addressed blob store plus one JSON record per cached item or
// playlist. There is deliberately no persisted top-level manifest —
// ListItemIDs/ListPlaylistIDs/DiskUsage/GC all derive state by walking the
// directories, so there is nothing that can drift out of sync with the
// files themselves after a crash mid-write (writes below go through a
// temp-file-then-rename so a killed capture process cannot leave a
// half-written record or blob for a later read to trip over).
//
//go:generate mockgen -source=store.go -destination=../mocks/offlinecache_store.go -package=mocks -mock_names=Store=MockOfflineCacheStore
type Store interface {
	// WriteBlob content-addresses data streamed from r under
	// blobs/<sha256>. Hashing happens while copying to a temp file, not
	// after buffering the whole body in memory — captured resources can
	// be gigabyte-scale (see docs/offline-artwork-capture.md's 1.1GB
	// video case), and controld runs on memory-constrained devices
	// alongside a kiosk Chromium already under its own memory pressure.
	// maxBytes caps how many bytes may be read from r before the write
	// aborts with ErrBlobTooLarge and the partial temp file is discarded
	// (<=0 means unlimited); this lets a caller reject one oversized
	// resource mid-stream against a disk budget, rather than discovering
	// the overrun only once the whole body has already landed on disk.
	// Writing content that already exists under its hash (across
	// items/playlists) is a cheap no-op after the redundant temp file is
	// discarded — this is the entire dedup mechanism, there is no
	// separate refcount to maintain.
	WriteBlob(r io.Reader, maxBytes int64) (sha256Hex string, err error)
	// ReadBlob reads a blob and re-verifies its hash before returning it,
	// so on-disk corruption fails loudly instead of silently feeding a
	// mismatched body to replay.
	ReadBlob(sha256Hex string) ([]byte, error)
	// BlobSize stats a blob without reading it into memory; used by the
	// static-server fallback and disk accounting for large assets.
	BlobSize(sha256Hex string) (int64, error)
	// BlobPath returns a blob's absolute path, used by the static-server
	// fallback to stream large assets directly off disk. Callers must
	// treat it as read-only.
	BlobPath(sha256Hex string) string

	SaveItem(rec *ItemRecord) error
	LoadItem(itemID string) (*ItemRecord, error)
	DeleteItem(itemID string) error
	ListItemIDs() ([]string, error)

	SavePlaylist(playlistID string, raw json.RawMessage) error
	LoadPlaylist(playlistID string) (json.RawMessage, error)
	DeletePlaylist(playlistID string) error
	ListPlaylistIDs() ([]string, error)

	// GC sweeps blobs/ and removes any blob not referenced by a resource of
	// a currently saved item. A sweep (rather than a live refcount) is
	// enough because the set of saved items is already the source of
	// truth for what is "live" — keeping a separate refcount in sync would
	// be the redundancy the on-disk format was simplified to avoid.
	GC() (removedBlobs int, freedBytes int64, err error)
	// DiskUsage sums blob file sizes — blobs/ is the only place binary
	// payloads live, so this is the whole of maxDiskBytes accounting.
	DiskUsage() (int64, error)

	RootDir() string
}

type fsStore struct {
	root   string
	os     wrapper.OS
	json   wrapper.JSON
	logger *zap.Logger
}

// NewStore constructs a Store rooted at root (expected:
// /home/feralfile/.cache/offline-artworks/). It performs no I/O itself;
// directories are created lazily on first write.
func NewStore(root string, osWrapper wrapper.OS, jsonWrapper wrapper.JSON, logger *zap.Logger) Store {
	return &fsStore{root: root, os: osWrapper, json: jsonWrapper, logger: logger}
}

func (s *fsStore) RootDir() string { return s.root }

func (s *fsStore) blobsDir() string     { return filepath.Join(s.root, "blobs") }
func (s *fsStore) itemsDir() string     { return filepath.Join(s.root, "items") }
func (s *fsStore) playlistsDir() string { return filepath.Join(s.root, "playlists") }

// safeID defends against a malformed DP-1 id escaping the store root via
// path separators or "..". DP-1 ids are normally opaque UUID/slug strings,
// so this should never trigger outside of a hostile or corrupted playlist.
func safeID(id string) (string, error) {
	if id == "" {
		return "", errors.New("offline cache: empty id")
	}
	clean := filepath.Base(id)
	if clean == "." || clean == ".." || clean != id {
		return "", fmt.Errorf("offline cache: unsafe id %q", id)
	}
	return clean, nil
}

// writeFileAtomic writes data via a temp file + rename so a process killed
// mid-write (headless capture is unsupervised) never leaves a partial file
// for a later reader to trip over.
func (s *fsStore) writeFileAtomic(dir, path string, data []byte, perm go_os.FileMode) error {
	if err := s.os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("offline cache: create dir %s: %w", dir, err)
	}
	tmp := path + ".tmp"
	if err := s.os.WriteFile(tmp, data, perm); err != nil {
		return fmt.Errorf("offline cache: write %s: %w", path, err)
	}
	if err := s.os.Rename(tmp, path); err != nil {
		return fmt.Errorf("offline cache: finalize %s: %w", path, err)
	}
	return nil
}

func (s *fsStore) blobPath(hexSum string) string { return filepath.Join(s.blobsDir(), hexSum) }

func (s *fsStore) BlobPath(hexSum string) string { return s.blobPath(hexSum) }

func (s *fsStore) WriteBlob(r io.Reader, maxBytes int64) (string, error) {
	if err := s.os.MkdirAll(s.blobsDir(), dirPerm); err != nil {
		return "", fmt.Errorf("offline cache: create dir %s: %w", s.blobsDir(), err)
	}

	// The final content-addressed name is only known after r has been
	// fully hashed, so the stream lands in a uniquely-named temp file
	// first (CreateTemp, not writeFileAtomic's fixed ".tmp" suffix,
	// since a fixed name would collide if this were ever called
	// concurrently) and gets renamed into place below.
	tmp, err := s.os.CreateTemp(s.blobsDir(), "incoming-*.tmp")
	if err != nil {
		return "", fmt.Errorf("offline cache: create temp blob: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Chmod(blobFilePerm); err != nil {
		_ = tmp.Close()
		_ = s.os.Remove(tmpPath)
		return "", fmt.Errorf("offline cache: chmod temp blob %s: %w", tmpPath, err)
	}

	// Every return path below must go through this cleanup unless the
	// write is renamed into its final name: GC only recognizes a blob as
	// "live" by its saved-item-referenced hash name (see GC's doc), so a
	// stray un-renamed temp file would otherwise never be reclaimed
	// until the next process restart happens to overwrite it.
	renamed := false
	defer func() {
		if !renamed {
			_ = s.os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	src := r
	if maxBytes > 0 {
		// Read one byte past the cap so a body exactly maxBytes long is
		// never mistaken for an overrun, while still detecting an
		// actual overrun without having to read the entire oversized
		// body first.
		src = io.LimitReader(r, maxBytes+1)
	}
	written, copyErr := io.Copy(tmp, io.TeeReader(src, hasher))
	closeErr := tmp.Close()
	if copyErr != nil {
		return "", fmt.Errorf("offline cache: stream blob to disk: %w", copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("offline cache: finalize temp blob %s: %w", tmpPath, closeErr)
	}
	if maxBytes > 0 && written > maxBytes {
		return "", ErrBlobTooLarge
	}

	hexSum := hex.EncodeToString(hasher.Sum(nil))
	finalPath := s.blobPath(hexSum)
	if _, statErr := s.os.Stat(finalPath); statErr == nil {
		return hexSum, nil // already stored: dedup across items/playlists, discard the redundant temp file via defer
	} else if !s.os.IsNotExist(statErr) {
		return "", fmt.Errorf("offline cache: stat blob %s: %w", hexSum, statErr)
	}

	if err := s.os.Rename(tmpPath, finalPath); err != nil {
		return "", fmt.Errorf("offline cache: finalize blob %s: %w", hexSum, err)
	}
	renamed = true
	return hexSum, nil
}

func (s *fsStore) ReadBlob(hexSum string) ([]byte, error) {
	data, err := s.os.ReadFile(s.blobPath(hexSum))
	if s.os.IsNotExist(err) {
		return nil, ErrBlobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("offline cache: read blob %s: %w", hexSum, err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != hexSum {
		return nil, ErrBlobHashMismatch
	}
	return data, nil
}

func (s *fsStore) BlobSize(hexSum string) (int64, error) {
	info, err := s.os.Stat(s.blobPath(hexSum))
	if s.os.IsNotExist(err) {
		return 0, ErrBlobNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("offline cache: stat blob %s: %w", hexSum, err)
	}
	return info.Size(), nil
}

func (s *fsStore) itemPath(id string) (string, error) {
	clean, err := safeID(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.itemsDir(), clean+".json"), nil
}

func (s *fsStore) SaveItem(rec *ItemRecord) error {
	if rec == nil || rec.ItemID == "" {
		return errors.New("offline cache: item record must have an itemId")
	}
	path, err := s.itemPath(rec.ItemID)
	if err != nil {
		return err
	}
	data, err := s.json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("offline cache: marshal item %s: %w", rec.ItemID, err)
	}
	return s.writeFileAtomic(s.itemsDir(), path, data, recordFilePerm)
}

func (s *fsStore) LoadItem(itemID string) (*ItemRecord, error) {
	path, err := s.itemPath(itemID)
	if err != nil {
		return nil, err
	}
	data, err := s.os.ReadFile(path)
	if s.os.IsNotExist(err) {
		return nil, ErrItemNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("offline cache: read item %s: %w", itemID, err)
	}
	var rec ItemRecord
	if err := s.json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("offline cache: parse item %s: %w", itemID, err)
	}
	return &rec, nil
}

func (s *fsStore) DeleteItem(itemID string) error {
	path, err := s.itemPath(itemID)
	if err != nil {
		return err
	}
	if err := s.os.Remove(path); err != nil && !s.os.IsNotExist(err) {
		return fmt.Errorf("offline cache: delete item %s: %w", itemID, err)
	}
	return nil
}

func (s *fsStore) ListItemIDs() ([]string, error) {
	return s.listJSONIDs(s.itemsDir(), "list items")
}

func (s *fsStore) playlistPath(id string) (string, error) {
	clean, err := safeID(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.playlistsDir(), clean+".json"), nil
}

func (s *fsStore) SavePlaylist(playlistID string, raw json.RawMessage) error {
	path, err := s.playlistPath(playlistID)
	if err != nil {
		return err
	}
	return s.writeFileAtomic(s.playlistsDir(), path, raw, recordFilePerm)
}

func (s *fsStore) LoadPlaylist(playlistID string) (json.RawMessage, error) {
	path, err := s.playlistPath(playlistID)
	if err != nil {
		return nil, err
	}
	data, err := s.os.ReadFile(path)
	if s.os.IsNotExist(err) {
		return nil, ErrPlaylistNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("offline cache: read playlist %s: %w", playlistID, err)
	}
	return json.RawMessage(data), nil
}

func (s *fsStore) DeletePlaylist(playlistID string) error {
	path, err := s.playlistPath(playlistID)
	if err != nil {
		return err
	}
	if err := s.os.Remove(path); err != nil && !s.os.IsNotExist(err) {
		return fmt.Errorf("offline cache: delete playlist %s: %w", playlistID, err)
	}
	return nil
}

func (s *fsStore) ListPlaylistIDs() ([]string, error) {
	return s.listJSONIDs(s.playlistsDir(), "list playlists")
}

func (s *fsStore) listJSONIDs(dir, opDescription string) ([]string, error) {
	entries, err := s.os.ReadDir(dir)
	if s.os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("offline cache: %s: %w", opDescription, err)
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(name, ".json"))
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *fsStore) GC() (int, int64, error) {
	itemIDs, err := s.ListItemIDs()
	if err != nil {
		return 0, 0, err
	}

	keep := make(map[string]bool)
	for _, id := range itemIDs {
		rec, err := s.LoadItem(id)
		if err != nil {
			s.logger.Warn("offline cache GC: skipping unreadable item record",
				zap.String("item_id", id), zap.Error(err))
			continue
		}
		for _, res := range rec.Resources {
			if res.SHA256 != "" {
				keep[res.SHA256] = true
			}
		}
	}

	entries, err := s.os.ReadDir(s.blobsDir())
	if s.os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("offline cache: list blobs for GC: %w", err)
	}

	var removed int
	var freed int64
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") || keep[name] {
			continue
		}
		path := filepath.Join(s.blobsDir(), name)
		size, statErr := s.os.Stat(path)
		if err := s.os.Remove(path); err != nil {
			s.logger.Warn("offline cache GC: failed to remove orphan blob",
				zap.String("sha256", name), zap.Error(err))
			continue
		}
		removed++
		if statErr == nil {
			freed += size.Size()
		}
	}
	return removed, freed, nil
}

func (s *fsStore) DiskUsage() (int64, error) {
	entries, err := s.os.ReadDir(s.blobsDir())
	if s.os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("offline cache: list blobs for disk usage: %w", err)
	}
	var total int64
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		info, err := s.os.Stat(filepath.Join(s.blobsDir(), e.Name()))
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total, nil
}
