// Package onchfs reads works from the onchfs on-chain file system and proves
// every byte against its address.
//
// onchfs is the content-addressed store fxhash wrote generative works into on
// Ethereum, Base and Tezos: a directory is a list of names and child
// addresses, a file is content plus HPACK-encoded headers, and every address
// is a keccak over exactly that. fxhash closed on 2026-08-17 and the gateway
// every player used for onchfs:// URIs, onchfs.fxhash2.xyz, no longer resolves
// (ffos-user#427). The works are intact in chain state; this package reads
// them from the contract itself through public nodes and verifies every inode
// it receives, so a node only has to be reachable, never trusted.
//
// Layers, bottom up:
//   - evm.go and tezos.go each read one inode from one chain and verify it
//     (Reader over eth_call; TezosReader over run_script_view). The two chains
//     derive a directory address the same way but a file address differently.
//   - readTree (this file) walks a tree over either of them, concurrently and
//     within Limits, and builds the Files a consumer serves.
//   - rpc.go is the wire: one endpoint per caller, no redirects.
//   - resolver.go picks the chain (an onchfs URI usually names none), checks
//     each node is on the chain it claims, pins a block, and fails over
//     between nodes.
package onchfs

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	enchex "encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"golang.org/x/crypto/sha3"
	"golang.org/x/net/http2/hpack"
)

// ErrNotFound is returned when the chain holds no inode at an address.
var ErrNotFound = errors.New("onchfs: not found")

// ErrVerification is wrapped by every mismatch between bytes received and the
// address that names them. A caller that sees it has a node lying, a node on
// another chain, or a bug; none of them should reach a screen.
var ErrVerification = errors.New("onchfs: verification failed")

// ErrTooLarge is returned when a tree exceeds its Limits. It is a property of
// the work, not of the node, so asking another node does not help.
var ErrTooLarge = errors.New("onchfs: tree exceeds limits")

// File is one verified file of a tree.
type File struct {
	Path        string // relative to the tree root, forward slashes, no leading slash
	Inode       string // 64 hex: the file's onchfs address
	Stored      []byte // as written on chain (possibly gzip)
	Plain       []byte // Stored, decompressed when the headers declare gzip
	Gzip        bool
	Chunks      int
	ContentType string            // from the HPACK headers, "" if the writer recorded none
	Headers     map[string]string // every HPACK header the writer recorded, lower-cased names
}

// Tree is a verified snapshot of an onchfs directory (or of a single file at
// the root, keyed "").
type Tree struct {
	Root  string // 64 hex
	Chain string // CAIP-2 id of the chain it was read from
	Block string // the pinned block: a decimal number on EVM, a level on Tezos
	Files map[string]*File
}

// Limits bound one tree read. A wall plays generative code, typically a few
// files and a library or two (the Casey Reas tree is five files, 1 MB plain);
// the caps leave a wide margin above that while keeping one hostile or
// enormous tree from filling the device's memory or disk.
type Limits struct {
	MaxFileBytes    int // per file, stored and plain
	MaxTreeBytes    int // kept by one read: each distinct inode's stored bytes, plus its plaintext when gzip
	MaxPaths        int // paths visited by one read, directories included
	MaxDirEntries   int
	MaxDepth        int
	ConcurrentReads int // inode reads in flight for one tree
}

// The default caps, exported for tests and for consumers that report them.
const (
	MaxPlainFileBytes = 64 << 20
	MaxTreeBytes      = 64 << 20
	MaxTreePaths      = 8192
)

// DefaultLimits are the limits a device reads with.
var DefaultLimits = Limits{
	MaxFileBytes:    MaxPlainFileBytes,
	MaxTreeBytes:    MaxTreeBytes,
	MaxPaths:        MaxTreePaths,
	MaxDirEntries:   4096,
	MaxDepth:        16,
	ConcurrentReads: 4,
}

// inode is one verified inode: a directory listing, or a file's stored bytes
// and metadata.
type inode struct {
	isDir    bool
	entries  []dirEntry
	stored   []byte
	metadata []byte
	chunks   int
}

type dirEntry struct {
	name     string
	checksum string
}

// inodeSource reads one inode at one pinned block and verifies it against
// cid before returning it. ErrNotFound means the chain holds no inode there.
type inodeSource interface {
	readInode(ctx context.Context, cid string) (inode, error)
}

// readTree reads and verifies the whole tree under root.
//
// Children of a directory are walked concurrently, at most
// Limits.ConcurrentReads inode reads at a time; the semaphore is held only
// around a read, never while waiting for children, so a deep tree cannot
// deadlock it. The first error cancels the rest of the walk.
//
// onchfs lets a directory give one inode many names (fxhash/onchfs
// doc/docs/concepts/file-objects/directory.md), so a small tree can name the
// same bytes, or the same directory, any number of times. Inodes are
// therefore read once per readTree, by address, and a second walker that
// reaches an inode already being read waits for that read: an alias costs a
// map entry, not a second read or a second buffer (#428 review F7). Only
// distinct inodes count against MaxTreeBytes. An aliased directory is still
// walked under each of its names, which MaxPaths bounds: without it, a chain
// of directories that each name the next one many times multiplies paths
// exponentially while every inode stays small.
func readTree(ctx context.Context, src inodeSource, root string, l Limits) (map[string]*File, error) {
	root = strings.ToLower(root)
	if !isHex64(root) {
		return nil, fmt.Errorf("onchfs: root is not 64 hex characters: %q", root)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := &walker{
		src: src, limits: l, sem: make(chan struct{}, max(l.ConcurrentReads, 1)), cancel: cancel,
		nodes: map[string]*node{}, files: map[string]*File{},
	}
	w.walk(ctx, root, "", 0)
	if w.err != nil {
		return nil, w.err
	}
	return w.files, nil
}

type walker struct {
	src    inodeSource
	limits Limits
	sem    chan struct{}
	cancel context.CancelFunc

	mu    sync.Mutex
	nodes map[string]*node // by inode address; guarded by mu
	files map[string]*File // by path; guarded by mu
	kept  int              // bytes held by distinct inodes; guarded by mu
	// inflateMu makes decompression serial, so the budget it is charged
	// against is exact (buildFile).
	inflateMu sync.Mutex
	paths     int   // guarded by mu
	err       error // first error; guarded by mu
}

// node is one inode's read, shared by every path that names it. done closes
// when in/file/err are final.
type node struct {
	done chan struct{}
	in   inode
	file *File // set for a file inode
	err  error
}

func (w *walker) fail(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err == nil {
		w.err = err
		w.cancel()
	}
}

// load returns cid's verified inode, reading it only if no other path has.
func (w *walker) load(ctx context.Context, cid, rel string) (*node, error) {
	w.mu.Lock()
	n, seen := w.nodes[cid]
	if !seen {
		n = &node{done: make(chan struct{})}
		w.nodes[cid] = n
	}
	w.mu.Unlock()
	if seen {
		select {
		case <-n.done:
			return n, n.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	defer close(n.done)
	select {
	case w.sem <- struct{}{}:
	case <-ctx.Done():
		n.err = ctx.Err()
		return nil, n.err
	}
	n.in, n.err = w.src.readInode(ctx, cid)
	<-w.sem
	if n.err != nil {
		if errors.Is(n.err, ErrNotFound) {
			n.err = fmt.Errorf("%w: no inode %s at %q", ErrNotFound, cid, rel)
		}
		return nil, n.err
	}
	if n.in.isDir {
		return n, nil
	}
	n.file, n.err = w.buildFile(ctx, cid, rel, n.in)
	// Kept once, in n.file. The raw metadata is dropped too: on EVM it is a
	// window into the whole RPC answer, and the cache holds every node until
	// the walk ends, so keeping it would keep every answer alive outside
	// MaxTreeBytes (PR #461 review F2). n.file carries its decoded headers.
	n.in.stored, n.in.metadata = nil, nil
	if n.err != nil {
		return nil, n.err
	}
	return n, nil
}

// buildFile charges a verified file against the tree budget and builds it.
//
// The budget is charged before the bytes are made, not after (PR #461
// review F10): the stored bytes as soon as they are read, and a declared
// gzip body is decompressed only up to what remains of the budget, one file
// at a time (inflateMu). Charging after expansion let every concurrent
// decompression reach MaxFileBytes before the first was counted, so a tree
// of many gzip files held hundreds of MiB before it was refused. Serial
// expansion costs little: inode reads, the slow part, stay concurrent.
func (w *walker) buildFile(ctx context.Context, cid, rel string, in inode) (*File, error) {
	if len(in.stored) > w.limits.MaxFileBytes {
		return nil, fmt.Errorf("%w: %q is %d bytes", ErrTooLarge, rel, len(in.stored))
	}
	if err := w.charge(len(in.stored)); err != nil {
		return nil, err
	}
	w.inflateMu.Lock()
	defer w.inflateMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w.mu.Lock()
	remaining := w.limits.MaxTreeBytes - w.kept
	w.mu.Unlock()
	f, err := newFile(cid, rel, in, w.limits.MaxFileBytes, min(w.limits.MaxFileBytes, remaining))
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			return nil, fmt.Errorf("%w: tree keeps more than %d bytes", ErrTooLarge, w.limits.MaxTreeBytes)
		}
		return nil, err
	}
	if f.Gzip {
		if err := w.charge(len(f.Plain)); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// charge adds n bytes to what the tree keeps, failing past MaxTreeBytes.
func (w *walker) charge(n int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.kept += n
	if w.kept > w.limits.MaxTreeBytes {
		return fmt.Errorf("%w: tree keeps more than %d bytes", ErrTooLarge, w.limits.MaxTreeBytes)
	}
	return nil
}

func (w *walker) walk(ctx context.Context, cid, rel string, depth int) {
	if depth > w.limits.MaxDepth {
		w.fail(fmt.Errorf("%w: %q is deeper than %d directories", ErrTooLarge, rel, w.limits.MaxDepth))
		return
	}
	w.mu.Lock()
	w.paths++
	tooMany := w.paths > w.limits.MaxPaths
	w.mu.Unlock()
	if tooMany {
		w.fail(fmt.Errorf("%w: tree has more than %d paths", ErrTooLarge, w.limits.MaxPaths))
		return
	}
	n, err := w.load(ctx, cid, rel)
	if err != nil {
		w.fail(err)
		return
	}
	if n.file != nil {
		f := *n.file
		f.Path = rel
		w.mu.Lock()
		w.files[rel] = &f
		w.mu.Unlock()
		return
	}
	if len(n.in.entries) > w.limits.MaxDirEntries {
		w.fail(fmt.Errorf("%w: directory %q has %d entries", ErrTooLarge, rel, len(n.in.entries)))
		return
	}
	var wg sync.WaitGroup
	for _, e := range n.in.entries {
		// Only names that would escape or alias a path are unsafe. Dot-prefixed
		// names (".asset.js") are valid onchfs entries and a work may load them,
		// so refusing them would refuse the whole tree.
		if e.name == "" || e.name == "." || e.name == ".." || strings.ContainsAny(e.name, "/\\") {
			w.fail(fmt.Errorf("%w: directory %s has an unsafe entry name %q", ErrVerification, cid, e.name))
			return
		}
		child := e.name
		if rel != "" {
			child = rel + "/" + e.name
		}
		wg.Go(func() { w.walk(ctx, e.checksum, child, depth+1) })
	}
	wg.Wait()
}

// newFile builds a File from a verified inode.
//
// It decompresses only what the writer declared as gzip content-encoding.
// The headers are part of the verified address; the body's first two bytes
// are not a declaration: a .gz asset (content-type application/gzip, no
// content-encoding) must reach the work as the archive it is. A body that
// declares gzip but does not decompress keeps Plain == Stored and Gzip false,
// so a consumer never serves it as something it is not.
//
// maxBytes bounds the stored bytes, maxPlain the decompressed ones.
func newFile(cid, rel string, in inode, maxBytes, maxPlain int) (*File, error) {
	if len(in.stored) > maxBytes {
		return nil, fmt.Errorf("%w: %q is %d bytes", ErrTooLarge, rel, len(in.stored))
	}
	f := &File{Path: rel, Inode: cid, Stored: in.stored, Plain: in.stored, Chunks: in.chunks, Headers: decodeHeaders(in.metadata)}
	f.ContentType = f.Headers["content-type"]
	if !strings.EqualFold(strings.TrimSpace(f.Headers["content-encoding"]), "gzip") {
		return f, nil
	}
	plain, ok, err := gunzip(in.stored, max(maxPlain, 0))
	if err != nil {
		return nil, fmt.Errorf("%q: %w", rel, err)
	}
	if ok {
		f.Plain, f.Gzip = plain, true
	}
	return f, nil
}

// gunzip decompresses stored. ok is false when stored is not a valid gzip
// stream (its trailer checksum included). Output past maxBytes is
// ErrTooLarge: io.LimitReader ends at the limit without an error, so reading
// only up to the limit would return a silently truncated file as if it were
// whole (#428 review F6). Reading one byte more tells the two apart.
func gunzip(stored []byte, maxBytes int) (plain []byte, ok bool, err error) {
	zr, err := gzip.NewReader(bytes.NewReader(stored))
	if err != nil {
		return nil, false, nil
	}
	plain, err = io.ReadAll(io.LimitReader(zr, int64(maxBytes)+1))
	if len(plain) > maxBytes {
		return nil, false, fmt.Errorf("%w: decompresses past %d bytes", ErrTooLarge, maxBytes)
	}
	if err != nil {
		return nil, false, nil
	}
	return plain, true, nil
}

// verifyDirectory recomputes keccak(0x00 || for each entry in descending name
// order: checksum || keccak(name)), which is how onchfs writers derive a
// directory's address.
func verifyDirectory(cid string, entries []dirEntry) error {
	sorted := append([]dirEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].name > sorted[j].name })
	acc := []byte{0}
	for _, e := range sorted {
		acc = append(acc, mustHex(e.checksum)...)
		acc = append(acc, keccak([]byte(e.name))...)
	}
	if hexString(keccak(acc)) != cid {
		return fmt.Errorf("%w: directory %s does not match its entries", ErrVerification, cid)
	}
	return nil
}

// decodeHeaders decodes the HPACK header block onchfs stores as file metadata.
// Names come back lower-cased; a block that does not decode yields no headers,
// which is a missing content type, not a failure: the bytes are still proven.
func decodeHeaders(metadata []byte) map[string]string {
	out := map[string]string{}
	dec := hpack.NewDecoder(4096, func(f hpack.HeaderField) { out[strings.ToLower(f.Name)] = f.Value })
	if _, err := dec.Write(metadata); err != nil {
		return map[string]string{}
	}
	if err := dec.Close(); err != nil {
		return map[string]string{}
	}
	return out
}

func keccak(b []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	_, _ = h.Write(b)
	return h.Sum(nil)
}

func uint256(v uint64) []byte {
	out := make([]byte, 32)
	binary.BigEndian.PutUint64(out[24:], v)
	return out
}

const hexDigits = "0123456789abcdef"

func hexString(b []byte) string {
	return enchex.EncodeToString(b)
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune(hexDigits, c) {
			return false
		}
	}
	return true
}

// mustHex decodes a hex string this package produced or validated itself.
func mustHex(s string) []byte {
	b, err := enchex.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}
