package onchfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
)

// EVM side of onchfs: the FileSystem contract fxhash deployed on Ethereum and
// Base, read with eth_call. Four views are used: readDirectory, readFile,
// inodes and concatenateChunks. Every answer is ABI-decoded with bounds
// checks and verified against the address that named it.

// Contracts is the onchfs file system contract per EVM CAIP-2 chain, as
// deployed by fxhash (onchfs-js 0.1.0 DEFAULT_CONTRACTS). The mainnets must
// match contentpath.OnchfsChains; the testnets are here for tests only.
var Contracts = map[string]string{
	"eip155:1":        "0x9e0f2864c6f125bbf599df6ca6e6c3774c5b2e04",
	"eip155:8453":     "0x2983008f292a43f208bba0275afd7e9b3d39af3b",
	"eip155:11155111": "0x4f555d39e89f6d768f75831d610b3940fa94c6b1",
	"eip155:84532":    "0x3fb48e03291b2490f939c961a1ad088437129f71",
}

// Caller makes one eth_call to a contract at a pinned block. The consumer owns
// the interface so tests can replay recorded answers.
type Caller interface {
	// Call returns the raw ABI-encoded result, ErrNotFound when the contract
	// reverted, or another error for transport failures.
	Call(ctx context.Context, contract string, data []byte, block *big.Int) ([]byte, error)
}

// Reader reads and verifies onchfs trees from one EVM contract at one block.
type Reader struct {
	caller   Caller
	chain    string
	contract string
	block    *big.Int
	limits   Limits
}

// NewReader binds a Caller to the contract for chain (a CAIP-2 id in
// Contracts) at block, with DefaultLimits. A nil block reads the latest
// state; pin one whenever the result is going to be stored or compared.
func NewReader(caller Caller, chain string, block *big.Int) (*Reader, error) {
	contract, ok := Contracts[chain]
	if !ok {
		return nil, fmt.Errorf("onchfs: no contract known for chain %q", chain)
	}
	return &Reader{caller: caller, chain: chain, contract: contract, block: block, limits: DefaultLimits}, nil
}

// ReadTree reads the whole tree under root (64 hex) and verifies every inode.
func (r *Reader) ReadTree(ctx context.Context, root string) (*Tree, error) {
	files, err := readTree(ctx, r, root, r.limits)
	if err != nil {
		return nil, err
	}
	block := "latest"
	if r.block != nil {
		block = r.block.String()
	}
	return &Tree{Root: root, Chain: r.chain, Block: block, Files: files}, nil
}

// readInode asks readDirectory first: the contract reverts for an address
// that is not a directory, and a revert is ErrNotFound. Only then is the
// address read as a file. Both answers are verified before they are returned.
func (r *Reader) readInode(ctx context.Context, cid string) (inode, error) {
	entries, err := r.readDirectory(ctx, cid)
	if err == nil {
		if err := verifyDirectory(cid, entries); err != nil {
			return inode{}, err
		}
		return inode{isDir: true, entries: entries}, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return inode{}, err
	}
	return r.readFile(ctx, cid)
}

// readFile calls readFile(bytes32) for the content and inodes(bytes32) for the
// chunk list and headers, then verifies the three against the address.
func (r *Reader) readFile(ctx context.Context, cid string) (inode, error) {
	out, err := r.call(ctx, "readFile(bytes32)", mustHex(cid))
	if err != nil {
		return inode{}, err
	}
	d := decoder{b: out}
	stored := d.dyn(d.word(0))
	if d.err != nil {
		return inode{}, fmt.Errorf("%w: file %s content is malformed", ErrVerification, cid)
	}
	// dyn returns a window into the whole RPC answer. Keeping that window
	// would keep the whole answer alive, and a node can pad an answer with
	// bytes no length counts, so a tree within MaxTreeBytes could hold far
	// more (#428 review F8). The content is copied out at its exact length;
	// the answer is garbage once this function returns.
	stored = bytes.Clone(stored)
	raw, err := r.call(ctx, "inodes(bytes32)", mustHex(cid))
	if err != nil {
		return inode{}, err
	}
	kind, metadata, chunks, err := decodeInode(raw)
	if err != nil {
		return inode{}, fmt.Errorf("%w: file %s inode is malformed: %w", ErrVerification, cid, err)
	}
	if kind != 1 {
		return inode{}, fmt.Errorf("%w: %s is not a file inode", ErrVerification, cid)
	}
	if err := r.verifyFile(ctx, cid, stored, metadata, chunks); err != nil {
		return inode{}, err
	}
	return inode{stored: stored, metadata: metadata, chunks: len(chunks)}, nil
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
	// Every name is copied out of the answer, and ABI offsets may point many
	// entries at one string, so copying unchecked would let a small answer
	// allocate its length times the entry count before the checksum check
	// refuses it (#428 review F9). An honest encoding stores each name once,
	// so its names together are never longer than the answer itself: more
	// than that is a forgery, refused before the copy that would exceed it.
	entries := make([]dirEntry, 0, n)
	names := 0
	for i := range n {
		so := d.wordAt(o1 + 32 + 32*i)
		name := d.dyn(o1 + 32 + so)
		if names += len(name); names > len(out) {
			return nil, fmt.Errorf("%w: directory %s names more bytes than its answer holds", ErrVerification, cid)
		}
		sum := d.bytes32(o2 + 32 + 32*i)
		entries = append(entries, dirEntry{name: string(name), checksum: sum})
	}
	if d.err != nil {
		return nil, fmt.Errorf("%w: directory %s is malformed: %w", ErrVerification, cid, d.err)
	}
	return entries, nil
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
