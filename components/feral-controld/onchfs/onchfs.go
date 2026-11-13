// Package onchfs reads files from the onchfs on-chain file system and proves
// them against their addresses.
//
// onchfs is the content-addressed store fxhash wrote generative works into on
// Ethereum and Base: a directory is a sorted list of names and child
// checksums, a file is a list of chunk checksums plus HPACK-encoded headers,
// and every address is a keccak over exactly that content. fxhash closed on
// 2026-08-17 and the gateway every player used for onchfs:// URIs,
// onchfs.fxhash2.xyz, no longer resolves (ffos-user#427). The works are intact
// in chain state; this package reads them from the contract itself through
// any JSON-RPC node and verifies every inode it receives, so the node only has
// to be reachable, never trusted.
//
// Four view functions are used: readDirectory, readFile, inodes and
// concatenateChunks. Reads are made at one pinned block so a tree is a
// consistent snapshot, and a Tree records that block. Bodies that arrive
// gzip-compressed (the content-encoding onchfs writers use) are decompressed
// into Plain; Stored keeps the bytes as written on chain. The HPACK headers
// give the content type when the writer recorded one.
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
	"math/big"
	"path"
	"sort"
	"strings"

	"golang.org/x/crypto/sha3"
	"golang.org/x/net/http2/hpack"
)

// Contracts is the onchfs file system contract per CAIP-2 chain, as deployed
// by fxhash (onchfs-js 0.1.0 DEFAULT_CONTRACTS).
var Contracts = map[string]string{
	"eip155:1":        "0x9e0f2864c6f125bbf599df6ca6e6c3774c5b2e04",
	"eip155:8453":     "0x2983008f292a43f208bba0275afd7e9b3d39af3b",
	"eip155:11155111": "0x4f555d39e89f6d768f75831d610b3940fa94c6b1",
	"eip155:84532":    "0x3fb48e03291b2490f939c961a1ad088437129f71",
}

// ErrNotFound is returned when the contract reverts for a checksum: no inode of
// that kind exists at that address.
var ErrNotFound = errors.New("onchfs: not found")

// ErrVerification is wrapped by every mismatch between bytes received and the
// address that names them. A caller that sees it has an RPC node lying, a
// chain reorganization across an unpinned read, or a bug; none of them should
// reach a screen.
var ErrVerification = errors.New("onchfs: verification failed")

// ErrTooLarge is returned when a file exceeds MaxPlainFileBytes. It is a
// property of the work, not of the node, so asking another node does not help.
var ErrTooLarge = errors.New("onchfs: file too large")

// Caller makes one eth_call to a contract at a pinned block. The consumer owns
// the interface so tests can replay recorded answers and production can use
// whichever RPC client the daemon already has.
type Caller interface {
	// Call returns the raw ABI-encoded result, ErrNotFound when the contract
	// reverted, or another error for transport failures.
	Call(ctx context.Context, contract string, data []byte, block *big.Int) ([]byte, error)
}

// File is one verified file of a tree.
type File struct {
	Path        string // relative to the tree root, forward slashes, no leading slash
	Inode       string // 64 hex: the file's onchfs checksum
	Stored      []byte // as written on chain (possibly gzip)
	Plain       []byte // Stored, decompressed if it was gzip
	Gzip        bool
	Chunks      int
	ContentType string            // from the HPACK headers, "" if the writer recorded none
	Headers     map[string]string // every HPACK header the writer recorded, lower-cased names
}

// Tree is a verified snapshot of an onchfs directory (or a single file at the
// root, keyed "").
type Tree struct {
	Root  string // 64 hex
	Block *big.Int
	Files map[string]*File
}

// Reader reads and verifies onchfs trees from one contract at one block.
type Reader struct {
	caller   Caller
	contract string
	block    *big.Int
}

// NewReader binds a Caller to the contract for chain (a CAIP-2 id in
// Contracts) at block. A nil block reads the latest state; pin one whenever
// the result is going to be stored or compared.
func NewReader(caller Caller, chain string, block *big.Int) (*Reader, error) {
	contract, ok := Contracts[chain]
	if !ok {
		return nil, fmt.Errorf("onchfs: no contract known for chain %q", chain)
	}
	return &Reader{caller: caller, contract: contract, block: block}, nil
}

// ParseURI splits an onchfs URI into its 64-hex root cid, the path inside the
// tree (without a leading slash, "" for the root) and the raw query (without
// "?"), which fxhash works read their seed from. The authority form
// (onchfs://<cid>.<chain>/...) is not accepted: the chain is the Reader's.
func ParseURI(uri string) (cid, inner, query string, err error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(uri), "onchfs://")
	if !ok {
		return "", "", "", fmt.Errorf("onchfs: not an onchfs:// URI: %q", uri)
	}
	rest, _, _ = strings.Cut(rest, "#")
	rest, query, _ = strings.Cut(rest, "?")
	cid, inner, _ = strings.Cut(rest, "/")
	cid = strings.ToLower(cid)
	if !isHex64(cid) {
		return "", "", "", fmt.Errorf("onchfs: cid is not 64 hex characters: %q", cid)
	}
	inner = strings.Trim(inner, "/")
	if inner != "" && (path.Clean("/"+inner) != "/"+inner || strings.Contains(inner, "\\")) {
		return "", "", "", fmt.Errorf("onchfs: path is not normalized: %q", inner)
	}
	return cid, inner, query, nil
}

// MaxTreeBytes caps the bytes one tree read keeps: every distinct file's
// stored bytes, plus its plaintext when that differs. MaxTreePaths caps the
// paths it visits, directories included.
//
// onchfs lets a directory give one inode many names (fxhash/onchfs
// doc/docs/concepts/file-objects/directory.md), so a small tree can name the
// same bytes, or the same directory, any number of times. Reads are therefore
// cached by address within a read: an alias costs a map entry, not a second
// read or a second buffer, and only distinct inodes count against
// MaxTreeBytes (#428 review F7). An aliased directory is still walked under
// each of its names, which is what MaxTreePaths bounds: without it, a chain of
// directories that each name the next one many times multiplies paths
// exponentially while every inode stays small.
const (
	MaxTreeBytes = 64 << 20
	MaxTreePaths = 8192
)

// ReadTree reads the whole tree under root (64 hex) and verifies every inode.
func (r *Reader) ReadTree(ctx context.Context, root string) (*Tree, error) {
	root = strings.ToLower(root)
	if !isHex64(root) {
		return nil, fmt.Errorf("onchfs: root is not 64 hex characters: %q", root)
	}
	t := &Tree{Root: root, Block: r.block, Files: map[string]*File{}}
	w := &walk{r: r, t: t, dirs: map[string][]dirEntry{}, files: map[string]*File{}}
	if err := w.visit(ctx, root, ""); err != nil {
		return nil, err
	}
	return t, nil
}

// walk is one ReadTree: the inodes it has verified, by address, and what it
// has kept and visited so far.
type walk struct {
	r     *Reader
	t     *Tree
	dirs  map[string][]dirEntry
	files map[string]*File
	bytes int
	paths int
}

func (w *walk) visit(ctx context.Context, cid, rel string) error {
	w.paths++
	if w.paths > MaxTreePaths {
		return fmt.Errorf("%w: tree has more than %d paths", ErrTooLarge, MaxTreePaths)
	}
	if f, ok := w.files[cid]; ok {
		alias := *f
		alias.Path = rel
		w.t.Files[rel] = &alias
		return nil
	}
	dir, isDir := w.dirs[cid]
	if !isDir {
		var err error
		dir, err = w.r.readDirectory(ctx, cid)
		switch {
		case err == nil:
			if err := verifyDirectory(cid, dir); err != nil {
				return err
			}
			w.dirs[cid] = dir
			isDir = true
		case !errors.Is(err, ErrNotFound):
			return err
		}
	}
	if isDir {
		for _, e := range dir {
			// Only names that would escape or alias a path are unsafe. Dot-prefixed
			// names (".asset.js") are valid onchfs entries and a work may load them,
			// so refusing them would refuse the whole tree.
			if e.name == "" || e.name == "." || e.name == ".." || strings.ContainsAny(e.name, "/\\") {
				return fmt.Errorf("%w: directory %s has an unsafe entry name %q", ErrVerification, cid, e.name)
			}
			child := e.name
			if rel != "" {
				child = rel + "/" + e.name
			}
			if err := w.visit(ctx, e.checksum, child); err != nil {
				return err
			}
		}
		return nil
	}
	f, err := w.r.readFile(ctx, cid)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: %s at %q is neither a directory nor a file", ErrNotFound, cid, rel)
		}
		return err
	}
	w.bytes += len(f.Stored)
	if f.Gzip {
		w.bytes += len(f.Plain)
	}
	if w.bytes > MaxTreeBytes {
		return fmt.Errorf("%w: tree keeps more than %d bytes", ErrTooLarge, MaxTreeBytes)
	}
	w.files[cid] = f
	f.Path = rel
	w.t.Files[rel] = f
	return nil
}

type dirEntry struct {
	name     string
	checksum string
}

// readDirectory calls readDirectory(bytes32) -> (string[] names, bytes32[] checksums).
func (r *Reader) readDirectory(ctx context.Context, cid string) ([]dirEntry, error) {
	out, err := r.call(ctx, "readDirectory(bytes32)", mustHex(cid))
	if err != nil {
		return nil, err
	}
	d := decoder{b: out}
	o1, o2 := d.word(0), d.word(1)
	n := d.wordAt(o1)
	m := d.wordAt(o2)
	if d.err != nil || n != m || n > 100000 {
		return nil, fmt.Errorf("%w: directory %s is malformed", ErrVerification, cid)
	}
	entries := make([]dirEntry, 0, n)
	for i := range n {
		so := d.wordAt(o1 + 32 + 32*i)
		name := d.dyn(o1 + 32 + so)
		sum := d.bytes32(o2 + 32 + 32*i)
		entries = append(entries, dirEntry{name: string(name), checksum: sum})
	}
	if d.err != nil {
		return nil, fmt.Errorf("%w: directory %s is malformed: %w", ErrVerification, cid, d.err)
	}
	return entries, nil
}

// readFile calls readFile(bytes32) for the content and inodes(bytes32) for the
// chunk list and headers, then verifies the three against the address.
func (r *Reader) readFile(ctx context.Context, cid string) (*File, error) {
	out, err := r.call(ctx, "readFile(bytes32)", mustHex(cid))
	if err != nil {
		return nil, err
	}
	d := decoder{b: out}
	stored := d.dyn(d.word(0))
	if d.err != nil {
		return nil, fmt.Errorf("%w: file %s content is malformed", ErrVerification, cid)
	}
	inode, err := r.call(ctx, "inodes(bytes32)", mustHex(cid))
	if err != nil {
		return nil, err
	}
	kind, metadata, chunks, err := decodeInode(inode)
	if err != nil {
		return nil, fmt.Errorf("%w: file %s inode is malformed: %w", ErrVerification, cid, err)
	}
	if kind != 1 {
		return nil, fmt.Errorf("%w: %s is not a file inode", ErrVerification, cid)
	}
	if err := r.verifyFile(ctx, cid, stored, metadata, chunks); err != nil {
		return nil, err
	}
	f := &File{Inode: cid, Stored: stored, Plain: stored, Chunks: len(chunks), Headers: decodeHeaders(metadata)}
	f.ContentType = f.Headers["content-type"]
	// Decompress only what the writer declared as gzip content-encoding. The
	// headers are part of the verified address, the bytes' first two bytes are
	// not a declaration: a .gz asset (content-type application/gzip, no
	// content-encoding) must reach the work as the archive it is. A body that
	// declares gzip but does not decompress keeps Plain == Stored and Gzip
	// false, so a consumer never serves it as something it is not.
	if strings.EqualFold(strings.TrimSpace(f.Headers["content-encoding"]), "gzip") {
		plain, ok, err := gunzip(stored, MaxPlainFileBytes)
		if err != nil {
			return nil, fmt.Errorf("file %s: %w", cid, err)
		}
		if ok {
			f.Plain, f.Gzip = plain, true
		}
	}
	return f, nil
}

// MaxPlainFileBytes caps one file's decompressed size. A wall plays
// generative code (the Casey Reas tree is 1 MB plain in all); the cap keeps a
// gzip bomb from exhausting the device's memory.
const MaxPlainFileBytes = 64 << 20

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

// verifyFile proves stored against the address cid.
//
// The EVM contracts derive a file address as keccak(0x01 ‖
// keccak(chunk checksums) ‖ keccak(metadata)) (FileSystem.sol createFile), so
// the address proves the chunk list, not the content: the content must then be
// shown to be exactly those chunks in order, always.
//
// Hazard: do not also accept keccak(0x01 ‖ keccak(content) ‖ keccak(metadata))
// "in case a writer hashed the content". On these contracts that formula is
// satisfied by a node that answers readFile with the packed chunk checksums
// themselves (keccak(content) is then keccak(chunk checksums)), so it would
// pass forged bytes under a valid address (#428 review F5). A chain whose
// contract does hash content needs its own reader, chosen by chain.
func (r *Reader) verifyFile(ctx context.Context, cid string, stored, metadata []byte, chunks []string) error {
	chunkBytes := make([]byte, 0, 32*len(chunks))
	for _, c := range chunks {
		chunkBytes = append(chunkBytes, mustHex(c)...)
	}
	if hexString(keccak(append(append([]byte{1}, keccak(chunkBytes)...), keccak(metadata)...))) != cid {
		return fmt.Errorf("%w: file %s does not match its chunk list and headers", ErrVerification, cid)
	}
	return r.verifyChunks(ctx, cid, stored, chunks)
}

// verifyChunks proves stored is the concatenation of chunks, in order.
//
// The contract lets a writer give every chunk its own length (createFile and
// concatenateChunks impose none), so a chunk's length cannot be inferred from
// its neighbors. The common case is cheap: writers like fxhash cut content
// into equal chunks (24000 bytes), so the first chunk is fetched once to learn
// that size and every slice of stored is hashed against its checksum. If any
// slice disagrees the layout is not uniform, and every chunk is fetched on its
// own and hashed by its actual bytes. Either path proves the same thing: when
// each piece hashes to its checksum in order and together they are exactly
// stored, stored is the file.
func (r *Reader) verifyChunks(ctx context.Context, cid string, stored []byte, chunks []string) error {
	switch len(chunks) {
	case 0:
		if len(stored) != 0 {
			return fmt.Errorf("%w: file %s has content but no chunks", ErrVerification, cid)
		}
		return nil
	case 1:
		if hexString(keccak(stored)) != chunks[0] {
			return fmt.Errorf("%w: file %s content does not match its chunk", ErrVerification, cid)
		}
		return nil
	}
	first, err := r.concatenateChunks(ctx, chunks[:1])
	if err != nil {
		return err
	}
	if uniformChunksMatch(stored, chunks, len(first)) {
		return nil
	}
	off := 0
	for i, c := range chunks {
		piece := first
		if i > 0 {
			if piece, err = r.concatenateChunks(ctx, chunks[i:i+1]); err != nil {
				return err
			}
		}
		if hexString(keccak(piece)) != c {
			return fmt.Errorf("%w: file %s chunk %d does not match its checksum", ErrVerification, cid, i)
		}
		if len(piece) > len(stored)-off || !bytes.Equal(stored[off:off+len(piece)], piece) {
			return fmt.Errorf("%w: file %s content differs from chunk %d", ErrVerification, cid, i)
		}
		off += len(piece)
	}
	if off != len(stored) {
		return fmt.Errorf("%w: file %s content runs past its chunks", ErrVerification, cid)
	}
	return nil
}

// uniformChunksMatch reports whether stored, cut into size-byte slices (the
// last one shorter), hashes slice by slice to chunks.
func uniformChunksMatch(stored []byte, chunks []string, size int) bool {
	if size == 0 || (len(stored)+size-1)/size != len(chunks) {
		return false
	}
	for i, c := range chunks {
		lo, hi := i*size, min((i+1)*size, len(stored))
		if hexString(keccak(stored[lo:hi])) != c {
			return false
		}
	}
	return true
}

// concatenateChunks calls concatenateChunks(bytes32[]) -> bytes.
func (r *Reader) concatenateChunks(ctx context.Context, chunks []string) ([]byte, error) {
	arg := make([]byte, 0, 64+32*len(chunks))
	arg = append(arg, uint256(32)...)
	arg = append(arg, uint256(uint64(len(chunks)))...)
	for _, c := range chunks {
		arg = append(arg, mustHex(c)...)
	}
	out, err := r.call(ctx, "concatenateChunks(bytes32[])", arg)
	if err != nil {
		return nil, err
	}
	d := decoder{b: out}
	b := d.dyn(d.word(0))
	if d.err != nil {
		return nil, fmt.Errorf("%w: chunk bytes are malformed", ErrVerification)
	}
	return b, nil
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

// decodeInode decodes inodes(bytes32) -> (uint8 kind, File{bytes metadata,
// bytes32[] chunkChecksums}, Directory{...}); only the file half is needed.
func decodeInode(b []byte) (kind uint64, metadata []byte, chunks []string, err error) {
	d := decoder{b: b}
	kind = d.word(0)
	fo := d.word(1)
	mo, co := d.wordAt(fo), d.wordAt(fo+32)
	metadata = d.dyn(fo + mo)
	n := d.wordAt(fo + co)
	if d.err != nil {
		return 0, nil, nil, fmt.Errorf("malformed inode: %w", d.err)
	}
	if n > 1<<20 {
		return 0, nil, nil, fmt.Errorf("inode lists %d chunks", n)
	}
	chunks = make([]string, 0, n)
	for i := range n {
		chunks = append(chunks, d.bytes32(fo+co+32+32*i))
	}
	if d.err != nil {
		return 0, nil, nil, d.err
	}
	return kind, metadata, chunks, nil
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

func (r *Reader) call(ctx context.Context, sig string, arg []byte) ([]byte, error) {
	data := append(keccak([]byte(sig))[:4], arg...)
	return r.caller.Call(ctx, r.contract, data, r.block)
}

// decoder reads ABI words with bounds checks; the first error sticks.
//
// Every value this package reads from a result is an offset, a length, an
// element count or a one-byte enum, and each is bounded by the result it came
// from: nothing can point or reach past the end of the buffer. word refuses
// any value larger than the buffer, so the sums callers build from a few of
// them (offset + 32 + 32*i) stay far below 2^64 and cannot wrap round a
// bounds check. The checks below are written as subtractions from the buffer
// length for the same reason. An RPC node is untrusted: a hostile length must
// be an ErrVerification, never a slice panic.
type decoder struct {
	b   []byte
	err error
}

// word returns the i-th 32-byte word as a value no larger than the buffer.
func (d *decoder) word(i uint64) uint64 {
	if d.err != nil {
		return 0
	}
	if i >= uint64(len(d.b))/32 {
		d.err = fmt.Errorf("word %d past end", i)
		return 0
	}
	w := d.b[32*i : 32*i+32]
	for _, x := range w[:24] {
		if x != 0 {
			d.err = fmt.Errorf("word %d does not fit 64 bits", i)
			return 0
		}
	}
	v := binary.BigEndian.Uint64(w[24:])
	if v > uint64(len(d.b)) {
		d.err = fmt.Errorf("word %d (%d) exceeds the %d-byte result", i, v, len(d.b))
		return 0
	}
	return v
}

// wordAt returns the word at byte offset off, which must be word aligned.
func (d *decoder) wordAt(off uint64) uint64 {
	if d.err != nil {
		return 0
	}
	if off%32 != 0 {
		d.err = fmt.Errorf("offset %d is not word aligned", off)
		return 0
	}
	return d.word(off / 32)
}

// dyn reads a dynamic bytes value whose length word sits at byte offset off.
func (d *decoder) dyn(off uint64) []byte {
	n := d.wordAt(off)
	if d.err != nil {
		return nil
	}
	start := off + 32 // wordAt proved off+32 <= len(d.b)
	if n > uint64(len(d.b))-start {
		d.err = fmt.Errorf("bytes at %d run past end", off)
		return nil
	}
	return d.b[start : start+n]
}

func (d *decoder) bytes32(off uint64) string {
	if d.err != nil {
		return ""
	}
	if off > uint64(len(d.b)) || uint64(len(d.b))-off < 32 {
		d.err = fmt.Errorf("bytes32 at %d past end", off)
		return ""
	}
	return hexString(d.b[off : off+32])
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
