package onchfs

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"runtime"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2/hpack"
)

// fakeFS is an in-memory onchfs contract: it derives addresses the way
// FileSystem.sol does and answers the four view functions the Reader calls
// with ABI encodings of the same shape. It lets a test build trees the
// recorded fixture does not contain (uneven chunks, dot-prefixed names,
// undeclared gzip) without a node.
type fakeFS struct {
	dirs   map[string][]dirEntry
	files  map[string]fakeFile
	chunks map[string][]byte
	// override, when set for a calldata hex string, replaces the answer.
	override map[string][]byte
}

type fakeFile struct {
	metadata []byte
	chunks   []string
}

func newFakeFS() *fakeFS {
	return &fakeFS{dirs: map[string][]dirEntry{}, files: map[string]fakeFile{}, chunks: map[string][]byte{}, override: map[string][]byte{}}
}

// addFile writes content cut at the given chunk lengths (which must sum to
// len(content)) and returns its address.
func (f *fakeFS) addFile(t *testing.T, content []byte, headers map[string]string, cuts ...int) string {
	t.Helper()
	if len(cuts) == 0 {
		cuts = []int{len(content)}
	}
	var sums []string
	packed := []byte{}
	off := 0
	for _, n := range cuts {
		piece := content[off : off+n]
		off += n
		sum := hexString(keccak(piece))
		f.chunks[sum] = piece
		sums = append(sums, sum)
		packed = append(packed, keccak(piece)...)
	}
	require.Equal(t, len(content), off, "cuts must cover the content")
	meta := encodeHeaders(headers)
	cid := hexString(keccak(append(append([]byte{1}, keccak(packed)...), keccak(meta)...)))
	f.files[cid] = fakeFile{metadata: meta, chunks: sums}
	return cid
}

// addDir writes a directory of name -> child address and returns its address.
func (f *fakeFS) addDir(entries map[string]string) string {
	var es []dirEntry
	for name, sum := range entries {
		es = append(es, dirEntry{name: name, checksum: sum})
	}
	sort.Slice(es, func(i, j int) bool { return es[i].name > es[j].name })
	acc := []byte{0}
	for _, e := range es {
		acc = append(acc, mustHex(e.checksum)...)
		acc = append(acc, keccak([]byte(e.name))...)
	}
	cid := hexString(keccak(acc))
	f.dirs[cid] = es
	return cid
}

func (f *fakeFS) Call(_ context.Context, _ string, data []byte, _ *big.Int) ([]byte, error) {
	if out, ok := f.override[hex.EncodeToString(data)]; ok {
		return out, nil
	}
	sel, arg := hex.EncodeToString(data[:4]), data[4:]
	switch sel {
	case selector("readDirectory(bytes32)"):
		es, ok := f.dirs[hexString(arg)]
		if !ok {
			return nil, ErrNotFound
		}
		return encodeDirectory(es), nil
	case selector("readFile(bytes32)"):
		file, ok := f.files[hexString(arg)]
		if !ok {
			return nil, ErrNotFound
		}
		var content []byte
		for _, c := range file.chunks {
			content = append(content, f.chunks[c]...)
		}
		return encodeBytesResult(content), nil
	case selector("inodes(bytes32)"):
		file, ok := f.files[hexString(arg)]
		if !ok {
			return nil, ErrNotFound
		}
		return encodeFileInode(file), nil
	case selector("concatenateChunks(bytes32[])"):
		n := binary.BigEndian.Uint64(arg[56:64])
		var out []byte
		for i := range n {
			out = append(out, f.chunks[hexString(arg[64+32*i:96+32*i])]...)
		}
		return encodeBytesResult(out), nil
	}
	return nil, errors.New("fakeFS: unknown selector " + sel)
}

func selector(sig string) string { return hex.EncodeToString(keccak([]byte(sig))[:4]) }

func encodeHeaders(h map[string]string) []byte {
	var buf bytes.Buffer
	enc := hpack.NewEncoder(&buf)
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		_ = enc.WriteField(hpack.HeaderField{Name: k, Value: h[k]})
	}
	return buf.Bytes()
}

func word(v uint64) []byte { return uint256(v) }

func padded(b []byte) []byte {
	out := append([]byte(nil), b...)
	for len(out)%32 != 0 {
		out = append(out, 0)
	}
	return out
}

// encodeBytesResult is the ABI encoding of a single `bytes` return value.
func encodeBytesResult(b []byte) []byte {
	return append(append(word(32), word(uint64(len(b)))...), padded(b)...)
}

// encodeDirectory is the ABI encoding of (string[] names, bytes32[] checksums).
func encodeDirectory(es []dirEntry) []byte {
	names := word(uint64(len(es)))
	var heads, tails []byte
	for _, e := range es {
		heads = append(heads, word(uint64(32*len(es)+len(tails)))...)
		tails = append(tails, append(word(uint64(len(e.name))), padded([]byte(e.name))...)...)
	}
	names = append(append(names, heads...), tails...)
	sums := word(uint64(len(es)))
	for _, e := range es {
		sums = append(sums, mustHex(e.checksum)...)
	}
	out := append(word(64), word(uint64(64+len(names)))...)
	return append(append(out, names...), sums...)
}

// encodeFileInode is the ABI encoding of the inodes(bytes32) getter for a
// file: (uint8 kind, File{bytes metadata, bytes32[] chunks}, Directory{}).
func encodeFileInode(file fakeFile) []byte {
	meta := append(word(uint64(len(file.metadata))), padded(file.metadata)...)
	chunks := word(uint64(len(file.chunks)))
	for _, c := range file.chunks {
		chunks = append(chunks, mustHex(c)...)
	}
	fileTuple := append(append(word(64), word(uint64(64+len(meta)))...), meta...)
	fileTuple = append(fileTuple, chunks...)
	dirTuple := append(append(word(64), word(96)...), append(word(0), word(0)...)...)
	out := append(word(1), word(96)...)
	out = append(out, word(uint64(96+len(fileTuple)))...)
	return append(append(out, fileTuple...), dirTuple...)
}

func readFake(t *testing.T, f *fakeFS, root string) (*Tree, error) {
	t.Helper()
	r, err := NewReader(f, "eip155:1", big.NewInt(1))
	require.NoError(t, err)
	return r.ReadTree(context.Background(), root)
}

// F1 (#428 review): an untrusted node answers with lengths or offsets that
// wrap uint64 arithmetic. Each must be a verification error, never a panic.
func TestReadTreeRefusesHostileABILengths(t *testing.T) {
	root := hexString(keccak([]byte("root")))
	rootArg := mustHex(root)
	calldata := func(sig string) string { return selector(sig) + hex.EncodeToString(rootArg) }
	maxWord := word(math.MaxUint64)

	cases := map[string]struct {
		call   string
		answer []byte
	}{
		// the reviewer's case: readDirectory reverts, readFile says offset 32, length 2^64-1
		"readFile length near 2^64":  {"readFile(bytes32)", append(word(32), maxWord...)},
		"readFile offset near 2^64":  {"readFile(bytes32)", append(maxWord, word(0)...)},
		"readFile misaligned offset": {"readFile(bytes32)", append(word(33), word(0)...)},
		"readFile empty answer":      {"readFile(bytes32)", nil},
		"directory offset near 2^64": {"readDirectory(bytes32)", append(maxWord, word(64)...)},
		"directory count near 2^64":  {"readDirectory(bytes32)", append(append(word(64), word(96)...), append(maxWord, word(0)...)...)},
		"directory name offset wraps": {"readDirectory(bytes32)", append(append(append(word(64), word(128)...), word(1)...),
			append(word(math.MaxUint64-63), word(1)...)...)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeFS()
			f.override[calldata(tc.call)] = tc.answer
			var err error
			require.NotPanics(t, func() { _, err = readFake(t, f, root) })
			require.Error(t, err)
			if tc.call == "readFile(bytes32)" || tc.answer != nil {
				assert.True(t, errors.Is(err, ErrVerification), err)
			}
		})
	}
}

func TestDecoderRefusesWordsLargerThanTheResult(t *testing.T) {
	d := decoder{b: append(word(32), word(65)...)}
	assert.Equal(t, uint64(32), d.word(0))
	require.NoError(t, d.err)
	d.word(1)
	assert.Error(t, d.err, "65 is past the 64-byte result")

	d = decoder{b: word(0)}
	d.bytes32(math.MaxUint64 - 16)
	assert.Error(t, d.err)
}

// F2 (#428 review): the contract lets every chunk have its own length.
func TestReadTreeAcceptsUnevenChunks(t *testing.T) {
	cases := map[string]struct {
		content string
		cuts    []int
	}{
		"short first chunk":   {"abc", []int{1, 2}},
		"short last chunk":    {"abc", []int{2, 1}},
		"three uneven chunks": {"0123456789", []int{3, 5, 2}},
		"equal chunks":        {"abcdef", []int{2, 2, 2}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeFS()
			cid := f.addFile(t, []byte(tc.content), map[string]string{"content-type": "text/plain"}, tc.cuts...)
			tree, err := readFake(t, f, cid)
			require.NoError(t, err)
			assert.Equal(t, tc.content, string(tree.Files[""].Plain))
			assert.Equal(t, len(tc.cuts), tree.Files[""].Chunks)
		})
	}
}

func TestReadTreeRefusesContentThatIsNotItsChunks(t *testing.T) {
	f := newFakeFS()
	cid := f.addFile(t, []byte("0123456789"), nil, 3, 5, 2)
	// the node serves different content for the file; each chunk is still honest
	f.override[selector("readFile(bytes32)")+cid] = encodeBytesResult([]byte("0123456788"))
	_, err := readFake(t, f, cid)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrVerification), err)

	// an honest file whose middle chunk the node lies about
	f = newFakeFS()
	cid = f.addFile(t, []byte("0123456789"), nil, 3, 5, 2)
	mid := f.files[cid].chunks[1]
	f.override[selector("concatenateChunks(bytes32[])")+hex.EncodeToString(append(append(word(32), word(1)...), mustHex(mid)...))] = encodeBytesResult([]byte("34568"))
	_, err = readFake(t, f, cid)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrVerification), err)
}

// F3 (#428 review): dot-prefixed names are valid; only names that escape or
// alias a path are refused.
func TestReadTreeEntryNames(t *testing.T) {
	f := newFakeFS()
	asset := f.addFile(t, []byte("x"), nil)
	index := f.addFile(t, []byte("<html>"), map[string]string{"content-type": "text/html"})
	root := f.addDir(map[string]string{".asset.js": asset, "index.html": index})
	tree, err := readFake(t, f, root)
	require.NoError(t, err)
	assert.Contains(t, tree.Files, ".asset.js")
	assert.Contains(t, tree.Files, "index.html")

	for _, bad := range []string{"..", ".", "a/b", `a\b`, ""} {
		f := newFakeFS()
		child := f.addFile(t, []byte("x"), nil)
		_, err := readFake(t, f, f.addDir(map[string]string{bad: child}))
		require.Error(t, err, bad)
		assert.True(t, errors.Is(err, ErrVerification), bad)
	}
}

// F4 (#428 review): gzip is decided by the verified content-encoding header,
// not by the body's magic bytes.
func TestReadTreeGzipFollowsContentEncoding(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("hello"))
	require.NoError(t, zw.Close())

	cases := map[string]struct {
		headers   map[string]string
		body      []byte
		wantPlain []byte
		wantGzip  bool
	}{
		"gzip archive without content-encoding stays an archive": {
			map[string]string{"content-type": "application/gzip"}, gz.Bytes(), gz.Bytes(), false},
		"declared gzip is decompressed": {
			map[string]string{"content-type": "text/plain", "content-encoding": "gzip"}, gz.Bytes(), []byte("hello"), true},
		"declared gzip that does not decompress is left as stored": {
			map[string]string{"content-encoding": "gzip"}, []byte("not gzip"), []byte("not gzip"), false},
		"plain body": {
			map[string]string{"content-type": "text/plain"}, []byte("hello"), []byte("hello"), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeFS()
			cid := f.addFile(t, tc.body, tc.headers)
			tree, err := readFake(t, f, cid)
			require.NoError(t, err)
			file := tree.Files[""]
			assert.Equal(t, tc.wantPlain, file.Plain)
			assert.Equal(t, tc.body, file.Stored)
			assert.Equal(t, tc.wantGzip, file.Gzip)
		})
	}
}

// F5 (#428 review): on the EVM contracts a file address commits to the chunk
// checksums, so a node that answers readFile with those checksums packed
// together must not pass as the file.
func TestReadTreeRefusesChunkChecksumsServedAsContent(t *testing.T) {
	for name, cuts := range map[string][]int{"one chunk": {10}, "three chunks": {3, 5, 2}} {
		t.Run(name, func(t *testing.T) {
			f := newFakeFS()
			cid := f.addFile(t, []byte("0123456789"), map[string]string{"content-type": "text/plain"}, cuts...)
			var packed []byte
			for _, c := range f.files[cid].chunks {
				packed = append(packed, mustHex(c)...)
			}
			f.override[selector("readFile(bytes32)")+cid] = encodeBytesResult(packed)
			_, err := readFake(t, f, cid)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrVerification), err)
		})
	}
}

// F5 against production bytes: the recorded index.html of the Casey Reas
// tree, its readFile answer replaced by its own chunk list.
func TestReadTreeRefusesChunkChecksumsServedAsContentRecorded(t *testing.T) {
	fx := loadFixture(t)
	inode := fx.Files["index.html"].Inode
	readFileKey := "0x" + selector("readFile(bytes32)") + inode
	inodes := fx.Calls["0x"+selector("inodes(bytes32)")+inode]
	require.NotNil(t, inodes)
	raw, err := hex.DecodeString((*inodes)[2:])
	require.NoError(t, err)
	_, _, chunks, err := decodeInode(raw)
	require.NoError(t, err)
	var packed []byte
	for _, c := range chunks {
		packed = append(packed, mustHex(c)...)
	}
	forged := 0
	r := newReader(t, fx, func(calldata string, out []byte) []byte {
		if calldata == readFileKey {
			forged++
			return encodeBytesResult(packed)
		}
		return out
	})
	_, err = r.ReadTree(context.Background(), fx.Root)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrVerification), err)
	assert.Equal(t, 1, forged)
}

// F6 (#428 review): output past the cap is an error, never a truncated file.
func TestGunzipRefusesOutputPastTheCap(t *testing.T) {
	gz := func(b []byte) []byte {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(b)
		require.NoError(t, zw.Close())
		return buf.Bytes()
	}
	exact := bytes.Repeat([]byte{0}, 100)
	plain, ok, err := gunzip(gz(exact), 100)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, exact, plain)

	_, ok, err = gunzip(gz(append(exact, '!')), 100)
	assert.False(t, ok)
	assert.True(t, errors.Is(err, ErrTooLarge), err)

	corrupt := gz([]byte("hello"))
	corrupt[len(corrupt)-5] ^= 0xff // CRC trailer
	_, ok, err = gunzip(corrupt, 100)
	require.NoError(t, err)
	assert.False(t, ok, "a stream whose trailer fails is not decompressed")
}

func TestReadTreeRefusesGzipBomb(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(make([]byte, MaxPlainFileBytes))
	_, _ = zw.Write([]byte("!"))
	require.NoError(t, zw.Close())
	body := buf.Bytes()
	cuts := []int{}
	for off := 0; off < len(body); off += 24000 {
		cuts = append(cuts, min(24000, len(body)-off))
	}
	f := newFakeFS()
	cid := f.addFile(t, body, map[string]string{"content-encoding": "gzip"}, cuts...)
	_, err := readFake(t, f, cid)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTooLarge), err)
}

// countingFS counts the calls a read makes per selector.
type countingFS struct {
	*fakeFS
	mu    sync.Mutex
	calls map[string]int
}

func (c *countingFS) Call(ctx context.Context, contract string, data []byte, block *big.Int) ([]byte, error) {
	c.mu.Lock()
	c.calls[hex.EncodeToString(data[:4])]++
	c.mu.Unlock()
	return c.fakeFS.Call(ctx, contract, data, block)
}

func gzipped(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// F7 (#428 review): one inode under many names is read once and kept once.
func TestReadTreeReadsAnAliasedInodeOnce(t *testing.T) {
	fs := &countingFS{fakeFS: newFakeFS(), calls: map[string]int{}}
	payload := fs.addFile(t, gzipped(t, make([]byte, 4<<20)), map[string]string{"content-encoding": "gzip"})
	names := map[string]string{}
	for i := range 32 {
		names[fmt.Sprintf("copy-%02d.bin", i)] = payload
	}
	root := fs.addDir(names)
	r, err := NewReader(fs, "eip155:1", big.NewInt(1))
	require.NoError(t, err)
	tree, err := r.ReadTree(context.Background(), root)
	require.NoError(t, err)
	require.Len(t, tree.Files, 32)
	first := tree.Files["copy-00.bin"]
	for p, f := range tree.Files {
		assert.Equal(t, p, f.Path)
		assert.Same(t, &first.Plain[0], &f.Plain[0], "%s shares the one decompressed buffer", p)
	}
	assert.Equal(t, 1, fs.calls[selector("readFile(bytes32)")])
	assert.Equal(t, 1, fs.calls[selector("inodes(bytes32)")])
}

func TestReadTreeBoundsWhatATreeKeeps(t *testing.T) {
	fs := newFakeFS()
	names := map[string]string{}
	for i := range 5 { // five distinct 16 MiB files: 80 MiB in all
		body := make([]byte, 16<<20)
		body[0] = byte(i)
		names[fmt.Sprintf("part-%d.bin", i)] = fs.addFile(t, gzipped(t, body), map[string]string{"content-encoding": "gzip"})
	}
	_, err := readFake(t, fs, fs.addDir(names))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTooLarge), err)
}

func TestReadTreeBoundsPathsThroughAliasedDirectories(t *testing.T) {
	fs := &countingFS{fakeFS: newFakeFS(), calls: map[string]int{}}
	next := fs.addFile(t, []byte("x"), nil)
	for range 3 { // three levels of 100 names each: a million paths
		names := map[string]string{}
		for i := range 100 {
			names[fmt.Sprintf("n%03d", i)] = next
		}
		next = fs.addDir(names)
	}
	r, err := NewReader(fs, "eip155:1", big.NewInt(1))
	require.NoError(t, err)
	_, err = r.ReadTree(context.Background(), next)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTooLarge), err)
	assert.LessOrEqual(t, fs.calls[selector("readDirectory(bytes32)")], 4, "each directory is read once")
}

// F8 (#428 review): a node that pads its readFile answer with bytes no
// length counts must not make the tree hold them.
func TestReadTreeKeepsOnlyTheContentOfAnAnswer(t *testing.T) {
	fs := newFakeFS()
	cid := fs.addFile(t, []byte("hello"), map[string]string{"content-type": "text/plain"})
	padded := append(encodeBytesResult([]byte("hello")), make([]byte, 1<<20)...)
	fs.override[selector("readFile(bytes32)")+cid] = padded
	tree, err := readFake(t, fs, cid)
	require.NoError(t, err)
	stored := tree.Files[""].Stored
	assert.Equal(t, "hello", string(stored))
	assert.Less(t, cap(stored), 1024, "the content owns its own small buffer, not a window into the 1 MiB answer")
}

// F9 (#428 review): directory entries whose name offsets all point at one
// long string must be refused before their copies add up, not after.
func TestReadDirectoryRefusesAliasedNames(t *testing.T) {
	root := hexString(keccak([]byte("root")))
	const entries, nameLen = 4096, 32768
	name := bytes.Repeat([]byte("n"), nameLen)
	// string[]: count, one offset per entry (all to the same string), the string
	names := word(entries)
	for range entries {
		names = append(names, word(32*entries)...)
	}
	names = append(append(names, word(nameLen)...), padded(name)...)
	sums := word(entries)
	for range entries {
		sums = append(sums, make([]byte, 32)...)
	}
	answer := append(append(word(64), word(uint64(64+len(names)))...), names...)
	answer = append(answer, sums...)

	f := newFakeFS()
	f.override[selector("readDirectory(bytes32)")+root] = answer
	r, err := NewReader(f, "eip155:1", big.NewInt(1))
	require.NoError(t, err)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err = r.readDirectory(context.Background(), root)
	runtime.ReadMemStats(&after)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrVerification), err)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(16<<20), "allocation stays near the answer's size, not entries x name")
}
