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

// ReadTree reads the whole tree under root (64 hex) and verifies every inode.
func (r *Reader) ReadTree(ctx context.Context, root string) (*Tree, error) {
	root = strings.ToLower(root)
	if !isHex64(root) {
		return nil, fmt.Errorf("onchfs: root is not 64 hex characters: %q", root)
	}
	t := &Tree{Root: root, Block: r.block, Files: map[string]*File{}}
	if err := r.walk(ctx, root, "", t); err != nil {
		return nil, err
	}
	return t, nil
}

func (r *Reader) walk(ctx context.Context, cid, rel string, t *Tree) error {
	dir, err := r.readDirectory(ctx, cid)
	if err == nil {
		if err := verifyDirectory(cid, dir); err != nil {
			return err
		}
		for _, e := range dir {
			if e.name == "" || e.name == "." || e.name == ".." || strings.ContainsAny(e.name, "/\\") || strings.HasPrefix(e.name, ".") {
				return fmt.Errorf("%w: directory %s has an unsafe entry name %q", ErrVerification, cid, e.name)
			}
			child := e.name
			if rel != "" {
				child = rel + "/" + e.name
			}
			if err := r.walk(ctx, e.checksum, child, t); err != nil {
				return err
			}
		}
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	f, err := r.readFile(ctx, cid)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: %s at %q is neither a directory nor a file", ErrNotFound, cid, rel)
		}
		return err
	}
	f.Path = rel
	t.Files[rel] = f
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
	n := d.word(o1 / 32)
	m := d.word(o2 / 32)
	if d.err != nil || n != m || n > 100000 {
		return nil, fmt.Errorf("%w: directory %s is malformed", ErrVerification, cid)
	}
	entries := make([]dirEntry, 0, n)
	for i := uint64(0); i < n; i++ {
		so := d.word((o1+32)/32 + i)
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
	if len(stored) >= 2 && stored[0] == 0x1f && stored[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(stored))
		if err == nil {
			plain, rerr := io.ReadAll(io.LimitReader(zr, 1<<30))
			if rerr == nil {
				f.Plain, f.Gzip = plain, true
			}
		}
	}
	return f, nil
}

// verifyFile checks that the address commits to this content and metadata
// (onchfs writers hash either the chunk checksums or the whole content), and
// that the content is exactly the chunks. Writers choose their own chunk size
// (fxhash used 24000), so the first chunk is fetched alone once to learn it.
func (r *Reader) verifyFile(ctx context.Context, cid string, stored, metadata []byte, chunks []string) error {
	chunkBytes := make([]byte, 0, 32*len(chunks))
	for _, c := range chunks {
		chunkBytes = append(chunkBytes, mustHex(c)...)
	}
	mh := keccak(metadata)
	byChunks := keccak(append(append([]byte{1}, keccak(chunkBytes)...), mh...))
	byContent := keccak(append(append([]byte{1}, keccak(stored)...), mh...))
	if hexString(byChunks) != cid && hexString(byContent) != cid {
		return fmt.Errorf("%w: file %s does not match its chunk list and headers", ErrVerification, cid)
	}
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
	size := len(first)
	if size == 0 || (len(stored)+size-1)/size != len(chunks) {
		return fmt.Errorf("%w: file %s chunk size %d does not fit %d chunks", ErrVerification, cid, size, len(chunks))
	}
	for i, c := range chunks {
		lo, hi := i*size, min((i+1)*size, len(stored))
		if hexString(keccak(stored[lo:hi])) != c {
			return fmt.Errorf("%w: file %s chunk %d does not match", ErrVerification, cid, i)
		}
	}
	return nil
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
	mo, co := d.word(fo/32), d.word(fo/32+1)
	metadata = d.dyn(fo + mo)
	n := d.word((fo + co) / 32)
	if d.err != nil || n > 1<<20 {
		return 0, nil, nil, fmt.Errorf("malformed inode: %w", d.err)
	}
	chunks = make([]string, 0, n)
	for i := uint64(0); i < n; i++ {
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
type decoder struct {
	b   []byte
	err error
}

func (d *decoder) word(i uint64) uint64 {
	if d.err != nil {
		return 0
	}
	off := 32 * i
	if off+32 > uint64(len(d.b)) {
		d.err = fmt.Errorf("word %d past end", i)
		return 0
	}
	w := d.b[off : off+32]
	for _, x := range w[:24] {
		if x != 0 {
			d.err = fmt.Errorf("word %d does not fit 64 bits", i)
			return 0
		}
	}
	return binary.BigEndian.Uint64(w[24:])
}

// dyn reads a dynamic bytes value whose length word sits at byte offset off.
func (d *decoder) dyn(off uint64) []byte {
	if d.err != nil {
		return nil
	}
	if off%32 != 0 {
		d.err = fmt.Errorf("offset %d is not word aligned", off)
		return nil
	}
	n := d.word(off / 32)
	if d.err != nil {
		return nil
	}
	start := off + 32
	if start+n > uint64(len(d.b)) {
		d.err = fmt.Errorf("bytes at %d run past end", off)
		return nil
	}
	return d.b[start : start+n]
}

func (d *decoder) bytes32(off uint64) string {
	if d.err != nil {
		return ""
	}
	if off+32 > uint64(len(d.b)) {
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
