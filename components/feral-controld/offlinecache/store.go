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
	"time"

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
	// ErrItemRecordCorrupt marks a LoadItem failure as DETERMINISTIC —
	// the record's bytes were read fine but do not parse, so retrying can
	// never succeed. GC branches on this to quarantine the record and
	// keep sweeping, versus aborting on a possibly-transient read error;
	// see GC's mark phase for why conflating the two either wedges GC
	// forever or deletes live blobs.
	ErrItemRecordCorrupt = errors.New("offline cache: item record does not parse")
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
// ListItemKeys/ListPlaylistIDs/DiskUsage/GC all derive state by walking the
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
	// Content-addressing IS the entire dedup mechanism — identical bytes
	// from different items/playlists converge on one blob, and there is
	// no separate refcount to maintain (GC's mark-sweep re-derives what
	// is live from the saved records instead).
	//
	// Writing content that already exists under its hash still REPLACES
	// the stored blob rather than short-circuiting on its existence. That
	// is a durability requirement, not a missed optimization: an
	// existence check cannot tell a healthy blob from one left truncated
	// by a power-loss torn write, and skipping the rename in that case
	// discards the freshly-downloaded good bytes forever while ReadBlob's
	// hash verification keeps rejecting the stored ones — the item
	// reports ready and never plays, and no retry or recapture ever
	// repairs it (only clearing the item, which lets GC reclaim the
	// corrupt blob as an orphan, breaks the cycle). Overwriting costs
	// only the rename (the bytes have already been streamed to the temp
	// file either way), is atomic, and leaves concurrent readers on their
	// existing inode. Do not reintroduce the existence shortcut.
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

	// SaveItem persists rec under items/<SourceKey(rec.Item.Source)>.json.
	// The key is derived here, from the record's own source, in the one
	// place a record is ever written — so the filename and the record
	// content can never disagree about identity. rec.Item.Source is the
	// only required field (the DP-1 item id is optional per spec and
	// informational here — see ItemRecord's doc).
	SaveItem(rec *ItemRecord) error
	// LoadItem reads the record for sourceKey (a SourceKey value, NOT a
	// raw source URL — callers at the package boundary hash exactly once
	// and pass the key everywhere inward).
	LoadItem(sourceKey string) (*ItemRecord, error)
	// DeleteItem removes sourceKey's record if it exists, reporting whether
	// there was one to remove. It stays a Remove-if-exists primitive —
	// "already absent" is success, never an error — so removed is the ONLY
	// signal a caller has that this particular call is what made the item
	// stop being cached. Service uses it for exactly that: to distinguish a
	// clear that really settled an item at not_cached (announce it, see
	// ClearItem) from one that found nothing to do, without paying for a
	// LoadItem read+unmarshal of a record it is about to delete anyway.
	//
	// The record is per-SOURCE (see ItemRecord's doc): deleting it removes
	// the cached artifact for EVERY playlist item — in any playlist — whose
	// source hashes to sourceKey. There is deliberately no refcount; a
	// clear issued via one playlist makes the shared source not_cached for
	// all of them.
	DeleteItem(sourceKey string) (removed bool, err error)
	// ListItemKeys lists the sourceKey of every saved item record — a
	// cheap directory-name listing, no record reads. Callers that need a
	// record's original source URL load it (Item.Source).
	ListItemKeys() ([]string, error)

	SavePlaylist(playlistID string, raw json.RawMessage) error
	LoadPlaylist(playlistID string) (json.RawMessage, error)
	DeletePlaylist(playlistID string) error
	ListPlaylistIDs() ([]string, error)

	// SavePlaylistURLIndex records that a displayPlaylist/downloadPlaylist
	// command resolved sourceURL to playlistID, so a later
	// displayPlaylist-by-URL for the same sourceURL can still find and
	// serve this exact cached playlist when live DP-1 resolution fails
	// (e.g. no network) — see commandrouter's displayPlaylist branch.
	// Keyed by sha256(sourceURL), mirroring WriteBlob's content-addressing
	// convention, so an index file's name never embeds an arbitrary
	// externally-controlled URL string as a path component. Call only
	// when playlistID actually came from resolving sourceURL itself —
	// never for an inline dp1_call download, which has no source URL to
	// index by.
	SavePlaylistURLIndex(sourceURL, playlistID string) error
	// LoadPlaylistIDForURL returns the playlistID last recorded for
	// sourceURL, or ErrPlaylistNotFound if none was ever recorded. The
	// returned ID is not guaranteed to still resolve via LoadPlaylist —
	// e.g. it may have since been cleared — callers must handle that
	// LoadPlaylist call failing with ErrPlaylistNotFound too; this index
	// is intentionally not kept in lockstep with DeletePlaylist (see
	// DeletePlaylist's doc) since a stale pointer to a since-deleted
	// playlist already fails closed correctly on the next LoadPlaylist.
	LoadPlaylistIDForURL(sourceURL string) (string, error)

	// GC sweeps blobs/ and removes any blob not referenced by a resource of
	// a currently saved item. A sweep (rather than a live refcount) is
	// enough because the set of saved items is already the source of
	// truth for what is "live" — keeping a separate refcount in sync would
	// be the redundancy the on-disk format was simplified to avoid.
	//
	// GC deliberately never touches blobs/*.tmp (WriteBlob's in-progress
	// temp files): it can run concurrently with an active capture (e.g.
	// triggered by a sibling item's clear), and an in-progress temp file
	// is not yet a "blob" GC's keep-set logic has any way to recognize as
	// live — deleting it out from under a running WriteBlob would corrupt
	// that capture. See SweepIncompleteBlobs for the startup-only sweep
	// that reclaims temp files left by a killed/crashed process instead.
	GC() (removedBlobs int, freedBytes int64, err error)
	// DiskUsage reports every byte this cache has persisted — blobs,
	// item records, playlist bodies, and the playlist URL index — so
	// maxDiskBytes bounds the store's real footprint, not just its
	// binary payloads (see fsStore.DiskUsage's doc for why counting
	// blobs alone made the budget bound the wrong number). Like GC, it
	// deliberately excludes blobs/*.tmp: an in-progress capture's temp
	// file is not yet committed content, so counting it here would make
	// maxDiskBytes accounting flicker based on capture timing rather
	// than actual stored content.
	DiskUsage() (int64, error)
	// PrunePlaylistRecords bounds stored playlist metadata by count —
	// see MaxPlaylistRecords for why a byte budget alone cannot.
	PrunePlaylistRecords(keep int) (int, error)
	// SweepIncompleteBlobs removes every blobs/*.tmp file and is safe to
	// call ONLY when no capture can possibly be in flight (i.e. once at
	// daemon startup, before the offline-cache worker starts processing
	// jobs — see Service.Start). WriteBlob's temp file is cleaned up by
	// its own defer on every normal return path, but a killed process
	// (SIGKILL, power loss) skips that defer entirely, and neither GC nor
	// DiskUsage ever reclaims or counts *.tmp files (by design — see
	// their docs), so a temp file orphaned this way would otherwise sit
	// on disk forever, silently eating into maxDiskBytes headroom that
	// status reports as still available.
	SweepIncompleteBlobs() (removedFiles int, freedBytes int64, err error)

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

// safeID defends against a malformed DP-1 playlist id escaping the store
// root via path separators or "..". Playlist ids are normally opaque
// UUID/slug strings, so this should never trigger outside of a hostile or
// corrupted playlist. Item records no longer need it: their filenames are
// SourceKey hashes, validated by validSourceKey below.
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
//
// The temp file's name is unique per call (CreateTemp, mirroring
// WriteBlob's own pattern below) rather than a fixed path+".tmp" suffix.
// Two concurrent writers targeting the SAME destination path are
// possible here — DownloadPlaylist calls SavePlaylist/
// SavePlaylistURLIndex directly on the caller's own goroutine (not
// serialized through the single-worker capture queue the way per-item
// SaveItem calls are), and the command gate's dedupe only collapses
// byte-identical arguments, so two overlapping downloadPlaylist calls
// for the same playlist id whose raw JSON differs slightly (e.g. a
// refreshed feed payload) are not guaranteed to be serialized upstream.
// A fixed shared temp name would let one writer's in-progress bytes be
// clobbered by the other before either rename ran, or let one rename
// fail after the first already moved the shared temp file out from
// under it. A unique-per-call temp name sidesteps that entirely: POSIX
// rename onto an existing destination is atomic, so two independent
// renames targeting the same path simply race harmlessly — whichever
// runs last wins outright with its own fully-written content, never a
// mix of both, and never a missing destination file in between.
//
// Trade-off accepted deliberately: unlike the old fixed path+".tmp"
// name (which the NEXT SaveItem/SavePlaylist call for that same path
// would simply overwrite), a process killed between CreateTemp and
// Rename now leaves a uniquely-named orphan that nothing ever reclaims
// automatically — records are tiny hand-written JSON, not the
// gigabyte-scale blobs SweepIncompleteBlobs exists for, so this is
// negligible, bounded cruft rather than the unbounded disk-accounting
// leak that motivated that startup sweep for blobs/*.tmp.
func (s *fsStore) writeFileAtomic(dir, path string, data []byte, perm go_os.FileMode) error {
	if err := s.os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("offline cache: create dir %s: %w", dir, err)
	}
	tmp, err := s.os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("offline cache: create temp file for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = s.os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("offline cache: chmod temp file for %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("offline cache: write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("offline cache: finalize temp file for %s: %w", path, err)
	}
	if err := s.os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("offline cache: finalize %s: %w", path, err)
	}
	renamed = true
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
	// first (CreateTemp — the same unique-per-call pattern
	// writeFileAtomic below now also uses, for the same reason: a fixed
	// name would collide if this were ever called concurrently, which
	// it can be — dedup means WriteBlob can be reached from multiple
	// items/playlists referencing identical content) and gets renamed
	// into place below.
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
	// Rename unconditionally, even when finalPath already exists. An
	// earlier version short-circuited on a bare existence check as a dedup
	// optimization, which made a corrupt on-disk blob PERMANENT: after a
	// power-loss torn write (the rename survives, the data doesn't — no
	// fsync here, and ext4 delayed allocation makes that window real), the
	// existence check kept discarding every freshly-downloaded good copy
	// while ReadBlob's hash verification kept rejecting the stored one, so
	// the item reported ready and never played. The content-addressed name
	// means an existing healthy blob is overwritten with identical bytes
	// (harmless — rename is atomic, and any concurrent reader keeps its
	// open inode), while a torn one is repaired by the next capture that
	// references it. The dedup saving was only ever the rename itself: the
	// bytes have already been streamed to the temp file either way.
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

// validSourceKey reports whether key has the exact shape SourceKey
// produces: 64 lowercase hex characters. Every legitimate caller derives
// keys via SourceKey (or reads them back from ListItemKeys, i.e. from
// filenames SourceKey named), so a mismatch is a programming bug — but
// the check also guarantees, independently of caller discipline, that an
// item path can never contain a separator or "..": the item-record
// counterpart of safeID's defense for playlist paths.
func validSourceKey(key string) bool {
	if len(key) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (s *fsStore) itemPath(sourceKey string) (string, error) {
	if !validSourceKey(sourceKey) {
		return "", fmt.Errorf("offline cache: invalid source key %q (want 64 lowercase hex chars — pass SourceKey(source), never a raw source URL)", sourceKey)
	}
	return filepath.Join(s.itemsDir(), sourceKey+".json"), nil
}

func (s *fsStore) SaveItem(rec *ItemRecord) error {
	if rec == nil || rec.Item.Source == "" {
		return errors.New("offline cache: item record must have an item source")
	}
	key := SourceKey(rec.Item.Source)
	path, err := s.itemPath(key)
	if err != nil {
		return err
	}
	data, err := s.json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("offline cache: marshal item %s: %w", key, err)
	}
	return s.writeFileAtomic(s.itemsDir(), path, data, recordFilePerm)
}

func (s *fsStore) LoadItem(sourceKey string) (*ItemRecord, error) {
	path, err := s.itemPath(sourceKey)
	if err != nil {
		return nil, err
	}
	data, err := s.os.ReadFile(path)
	if s.os.IsNotExist(err) {
		return nil, ErrItemNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("offline cache: read item %s: %w", sourceKey, err)
	}
	var rec ItemRecord
	if err := s.json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("%w: item %s: %w", ErrItemRecordCorrupt, sourceKey, err)
	}
	// Filename and content must agree on identity, the same way ReadBlob
	// re-verifies a blob against its content-addressed name. SaveItem
	// derives the filename from rec.Item.Source, so a mismatch means the
	// bytes on disk are not the record this key's readers (replay scope,
	// status, eviction) believe they are — tampering, a torn/misplaced
	// write, or a future bug — and serving them would key replay's
	// resource set or status off the WRONG source. Deterministic, so it
	// takes the corrupt (quarantine) path, not the transient one.
	if got := SourceKey(rec.Item.Source); got != sourceKey {
		return nil, fmt.Errorf("%w: item %s: record's own source hashes to %s (filename/content identity mismatch)",
			ErrItemRecordCorrupt, sourceKey, got)
	}
	return &rec, nil
}

func (s *fsStore) DeleteItem(sourceKey string) (bool, error) {
	path, err := s.itemPath(sourceKey)
	if err != nil {
		return false, err
	}
	if err := s.os.Remove(path); err != nil {
		if s.os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("offline cache: delete item %s: %w", sourceKey, err)
	}
	return true, nil
}

func (s *fsStore) ListItemKeys() ([]string, error) {
	names, err := s.listJSONIDs(s.itemsDir(), "list items")
	if err != nil {
		return nil, err
	}
	// Filter to valid source keys: a .json file under any other name is
	// unreachable through LoadItem's key validation, so surfacing it here
	// would only feed every reader (status, Start's rebuild, eviction) a
	// name it can never load. GC — the one caller that must see such
	// strays to quarantine them — walks the raw directory entries itself.
	keys := names[:0]
	for _, name := range names {
		if validSourceKey(name) {
			keys = append(keys, name)
		}
	}
	return keys, nil
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

// playlistsByURLDir is a subdirectory of playlistsDir, not a sibling —
// listJSONIDs/ListPlaylistIDs only match "*.json" entries directly inside
// playlistsDir, so a nested directory here is invisible to that walk and
// can never be mistaken for a playlist ID.
func (s *fsStore) playlistsByURLDir() string { return filepath.Join(s.playlistsDir(), "by-url") }

func (s *fsStore) playlistURLIndexPath(sourceURL string) string {
	sum := sha256.Sum256([]byte(sourceURL))
	return filepath.Join(s.playlistsByURLDir(), hex.EncodeToString(sum[:])+".json")
}

type playlistURLIndexRecord struct {
	PlaylistID string `json:"playlistId"`
}

func (s *fsStore) SavePlaylistURLIndex(sourceURL, playlistID string) error {
	if sourceURL == "" || playlistID == "" {
		return errors.New("offline cache: playlist URL index requires both a source URL and a playlist id")
	}
	data, err := s.json.Marshal(playlistURLIndexRecord{PlaylistID: playlistID})
	if err != nil {
		return fmt.Errorf("offline cache: marshal playlist URL index: %w", err)
	}
	path := s.playlistURLIndexPath(sourceURL)
	return s.writeFileAtomic(s.playlistsByURLDir(), path, data, recordFilePerm)
}

func (s *fsStore) LoadPlaylistIDForURL(sourceURL string) (string, error) {
	if sourceURL == "" {
		return "", ErrPlaylistNotFound
	}
	data, err := s.os.ReadFile(s.playlistURLIndexPath(sourceURL))
	if s.os.IsNotExist(err) {
		return "", ErrPlaylistNotFound
	}
	if err != nil {
		return "", fmt.Errorf("offline cache: read playlist URL index: %w", err)
	}
	var rec playlistURLIndexRecord
	if err := s.json.Unmarshal(data, &rec); err != nil {
		return "", fmt.Errorf("offline cache: parse playlist URL index: %w", err)
	}
	return rec.PlaylistID, nil
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

// quarantineItemRecordSuffix is appended to an unparsable item record's
// filename by GC's quarantine path. The resulting name no longer ends in
// ".json", so ListItemKeys — the single listing seam every reader
// (itemStatus, Start's rebuild, eviction's victim scan, GC itself) goes
// through — stops seeing the record entirely, while the bytes stay on
// disk for post-incident inspection. Quarantined files are counted by
// DiskUsage (they live in the items dir) but are a few KB of JSON, not
// blob-scale data.
const quarantineItemRecordSuffix = ".corrupt"

// quarantineItemRecord renames a record GC cannot use out of
// ListItemKeys' view, preserving its bytes for forensics. name is the raw
// directory-entry name minus ".json" and this joins it directly rather
// than going through itemPath (safe — ReadDir entry names never contain
// path separators), because one reachable shape is a record that is both
// unparsable AND at a name key validation would reject. See GC's mark
// phase for why these are quarantined rather than aborting the sweep —
// and note that the bulk shape, a valid-JSON record simply sitting at a
// non-source-key filename, is DELETED there instead: quarantining a whole
// pre-source-keying store would strand those bytes inside maxDiskBytes
// forever.
func (s *fsStore) quarantineItemRecord(name string) error {
	path := filepath.Join(s.itemsDir(), name+".json")
	if err := s.os.Rename(path, path+quarantineItemRecordSuffix); err != nil {
		return fmt.Errorf("offline cache: quarantine item record %s: %w", name, err)
	}
	return nil
}

// loadItemByName reads an item record by its raw directory-entry name
// (minus ".json"), bypassing LoadItem's source-key validation. GC-only:
// the mark phase iterates real directory entries and must distinguish
// "reads fine but unusable" from "transiently unreadable" even for files
// whose names are not valid source keys — LoadItem's validation error
// would conflate those into one undifferentiated failure.
func (s *fsStore) loadItemByName(name string) (*ItemRecord, error) {
	path := filepath.Join(s.itemsDir(), name+".json")
	data, err := s.os.ReadFile(path)
	if s.os.IsNotExist(err) {
		return nil, ErrItemNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("offline cache: read item %s: %w", name, err)
	}
	var rec ItemRecord
	if err := s.json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("%w: item %s: %w", ErrItemRecordCorrupt, name, err)
	}
	return &rec, nil
}

func (s *fsStore) GC() (int, int64, error) {
	// Raw directory names, NOT ListItemKeys: that seam filters out
	// invalid-key strays so ordinary readers never see them, but GC is
	// exactly the one caller that must see them to quarantine them (see
	// the invalid-name branch below).
	itemNames, err := s.listJSONIDs(s.itemsDir(), "list items")
	if err != nil {
		return 0, 0, err
	}

	keep := make(map[string]bool)
	for _, id := range itemNames {
		rec, err := s.loadItemByName(id)
		// A record whose filename is not a valid source key is
		// unreachable by construction — LoadItem validates keys, so no
		// reader (status, replay scope, eviction) can load it however
		// well its bytes parse — and it is DELETED rather than
		// quarantined. Quarantine exists to preserve evidence of a
		// genuine anomaly, and this shape is not one: it is the expected,
		// bulk-scale state of a cache written before source keying, which
		// Service.Start's GC pass retires wholesale on the first boot
		// after the upgrade. Renaming a whole store's worth of records to
		// *.corrupt would leave that bulk permanently inside the
		// maxDiskBytes budget — DiskUsage counts them, DeleteItem only
		// targets <key>.json, and eviction only walks ListItemKeys, so
		// nothing could ever reclaim them. Their bytes are a stale record
		// of a format this daemon no longer reads; the blobs they
		// referenced are reclaimed by this same sweep.
		if err == nil && !validSourceKey(id) {
			if rmErr := s.os.Remove(filepath.Join(s.itemsDir(), id+".json")); rmErr != nil {
				return 0, 0, fmt.Errorf("offline cache: GC aborted, could not remove unreachable item record %s: %w", id, rmErr)
			}
			s.logger.Info("offline cache GC: removed item record whose filename is not a source key",
				zap.String("name", id))
			continue
		}
		// A record at a VALID key that does not match its own source is a
		// different matter: bytes were written under an identity they do
		// not carry (tampering, a torn/misplaced write, or a bug), which
		// is exactly the anomaly quarantine preserves evidence of. It is
		// equally unreachable — LoadItem rejects the mismatch, see its
		// identity check — so it takes the corrupt-record path below:
		// quarantined for forensics, its exclusive blobs reclaimed.
		// Rare by construction, so the "a few KB of JSON" bound the
		// quarantine comment relies on genuinely holds here.
		if err == nil && SourceKey(rec.Item.Source) != id {
			err = fmt.Errorf("%w: item %s: record's own source hashes to %s (filename/content identity mismatch)",
				ErrItemRecordCorrupt, id, SourceKey(rec.Item.Source))
		}
		switch {
		case errors.Is(err, ErrItemRecordCorrupt):
			// Deterministic: the bytes read fine but will never parse, so
			// aborting here would wedge GC on every future pass — and GC
			// is the ONLY thing that frees blob bytes (DeleteItem removes
			// just the record JSON), so a permanent abort silently
			// disables the disk budget while eviction keeps deleting
			// healthy records for zero reclaimed bytes. Nothing else can
			// remove the record either: eviction's victim scan skips
			// records it cannot read. Quarantining it (rename to a suffix
			// ListItemKeys ignores, preserving the bytes for forensics)
			// and continuing is safe because no reader can use the record
			// anyway — itemStatus reports it not_cached and Start's
			// rebuild skips it — so blobs only it referenced are already
			// unreachable, and blobs it shared stay in the keep-set via
			// the readable records that also reference them.
			if qErr := s.quarantineItemRecord(id); qErr != nil {
				return 0, 0, fmt.Errorf("offline cache: GC aborted, could not quarantine corrupt item record %s: %w", id, qErr)
			}
			s.logger.Warn("offline cache GC: quarantined unparsable item record",
				zap.String("source_key", id), zap.Error(err))
			continue
		case err != nil:
			// Possibly transient (EIO/EMFILE): abort the whole sweep
			// rather than skipping the record. It still references blobs,
			// and skipping would silently narrow the keep-set — the sweep
			// below would then delete those blobs as "orphans" while the
			// record recovers on a later read, an unrecoverable loss of
			// good data from a recoverable error. Deleting bytes based on
			// an error must fail safe in the direction that preserves
			// data; an aborted pass costs only disk until a retry
			// succeeds (or, if the error proves permanent for this
			// record's file specifically, until the device's real problem
			// — a failing disk — is addressed).
			return 0, 0, fmt.Errorf("offline cache: GC aborted, item record %s unreadable (would orphan its blobs): %w", id, err)
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

// DiskUsage reports every byte this cache has persisted, not just its
// blobs.
//
// Blobs dominate, but they were once the whole of it — which made
// maxDiskBytes a bound on the wrong number. Item records and playlist
// bodies are cache data too: DownloadPlaylist persists a playlist's raw
// JSON (and its URL-index entry) BEFORE any item is queued, and does so
// even when nothing in it is cacheable at all, so a device asked to
// download a series of distinct all-streaming playlists accumulated
// metadata that no eviction pass could see and no budget accounted for
// (feral-file/ffos-user#229 review finding). Counting all three
// directories is what makes the configured ceiling mean what
// OptionsFromConfig's DefaultMaxDiskBytes doc says it means.
//
// See MaxPlaylistRecords for the separate bound that keeps that
// metadata evictable rather than merely visible.
func (s *fsStore) DiskUsage() (int64, error) {
	total, err := s.dirSize(s.blobsDir())
	if err != nil {
		return 0, fmt.Errorf("offline cache: measure blobs for disk usage: %w", err)
	}
	items, err := s.dirSize(s.itemsDir())
	if err != nil {
		return 0, fmt.Errorf("offline cache: measure item records for disk usage: %w", err)
	}
	playlists, err := s.dirSize(s.playlistsDir())
	if err != nil {
		return 0, fmt.Errorf("offline cache: measure playlist records for disk usage: %w", err)
	}
	byURL, err := s.dirSize(s.playlistsByURLDir())
	if err != nil {
		return 0, fmt.Errorf("offline cache: measure playlist URL index for disk usage: %w", err)
	}
	return total + items + playlists + byURL, nil
}

// dirSize sums the regular files directly inside dir, skipping
// subdirectories (playlistsDir contains by-url, which callers measure
// separately) and the .tmp files writeFileAtomic/WriteBlob leave
// mid-write. A missing directory is 0, not an error: the store creates
// each lazily on first write.
func (s *fsStore) dirSize(dir string) (int64, error) {
	entries, err := s.os.ReadDir(dir)
	if s.os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		info, err := s.os.Stat(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total, nil
}

// MaxPlaylistRecords bounds how many playlist bodies the store keeps.
//
// Playlist records are not reachable by enforceDiskLimit's eviction,
// which walks ITEMS (by CapturedAt) and then GCs the blobs they
// released — a playlist body is neither, so it would otherwise persist
// until an explicit ClearPlaylist that may never come. Counting them in
// DiskUsage alone would make the overage visible while leaving nothing
// able to act on it, so they are bounded by count at the point they are
// written instead.
//
// Sized so that a device's realistic working set of playlists is never
// pruned, while an unbounded series of one-off downloads cannot grow
// without limit. Pruning the oldest costs only the
// displayPlaylist-by-URL offline fallback for playlists that far back —
// see CachedPlaylistForURL, which already fails closed when a record is
// absent.
const MaxPlaylistRecords = 256

// PrunePlaylistRecords deletes the oldest playlist bodies beyond
// MaxPlaylistRecords, plus any by-url index entry left pointing at one.
// Returns how many bodies were removed. Best-effort: a failure to delete
// any single record is logged and skipped rather than failing the caller,
// since this runs after the write it is bounding has already succeeded.
func (s *fsStore) PrunePlaylistRecords(keep int) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	entries, err := s.os.ReadDir(s.playlistsDir())
	if s.os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("offline cache: list playlists for pruning: %w", err)
	}

	type record struct {
		id      string
		modTime time.Time
	}
	records := make([]record, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		info, err := s.os.Stat(filepath.Join(s.playlistsDir(), name))
		if err != nil {
			continue
		}
		records = append(records, record{id: strings.TrimSuffix(name, ".json"), modTime: info.ModTime()})
	}
	if len(records) <= keep {
		return 0, nil
	}

	// Newest first, so everything past the keep window is the oldest.
	sort.Slice(records, func(i, j int) bool { return records[i].modTime.After(records[j].modTime) })

	removed := make(map[string]bool, len(records)-keep)
	for _, rec := range records[keep:] {
		if err := s.DeletePlaylist(rec.id); err != nil {
			s.logger.Warn("offline cache: failed to prune old playlist record",
				zap.String("playlist_id", rec.id), zap.Error(err))
			continue
		}
		removed[rec.id] = true
	}
	if len(removed) == 0 {
		return 0, nil
	}
	s.pruneURLIndexEntriesFor(removed)
	return len(removed), nil
}

// pruneURLIndexEntriesFor drops by-url entries pointing at a playlist
// that no longer exists. Without this the index grows on its own: a
// stale entry still fails closed at lookup time (LoadPlaylistIDForURL's
// doc), but it is unbounded metadata all the same.
func (s *fsStore) pruneURLIndexEntriesFor(removedPlaylistIDs map[string]bool) {
	entries, err := s.os.ReadDir(s.playlistsByURLDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(s.playlistsByURLDir(), name)
		data, err := s.os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec playlistURLIndexRecord
		if err := s.json.Unmarshal(data, &rec); err != nil {
			continue
		}
		if !removedPlaylistIDs[rec.PlaylistID] {
			continue
		}
		if err := s.os.Remove(path); err != nil && !s.os.IsNotExist(err) {
			s.logger.Warn("offline cache: failed to prune stale playlist URL index entry", zap.Error(err))
		}
	}
}

func (s *fsStore) SweepIncompleteBlobs() (int, int64, error) {
	entries, err := s.os.ReadDir(s.blobsDir())
	if s.os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("offline cache: list blobs for incomplete-blob sweep: %w", err)
	}
	var removed int
	var freed int64
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".tmp") {
			continue
		}
		path := filepath.Join(s.blobsDir(), name)
		size, statErr := s.os.Stat(path)
		if err := s.os.Remove(path); err != nil {
			s.logger.Warn("offline cache: failed to remove stale incomplete blob",
				zap.String("file", name), zap.Error(err))
			continue
		}
		removed++
		if statErr == nil {
			freed += size.Size()
		}
	}
	return removed, freed, nil
}
