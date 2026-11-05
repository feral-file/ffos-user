package onchfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixture is a recording of every eth_call the verified read of one tree made
// against the Ethereum contract at one block (dp1-capsule tools/onchfs-read.py,
// 2026-10-31): calldata -> result. Replaying it makes the test offline and
// pins the real contract's answers, so a decoding or verification regression
// shows against production bytes.
type fixture struct {
	Chain    string             `json:"chain"`
	Contract string             `json:"contract"`
	Block    uint64             `json:"block"`
	Root     string             `json:"root"`
	Calls    map[string]*string `json:"calls"`
	Files    map[string]struct {
		SHA256 string `json:"sha256"`
		Bytes  int    `json:"bytes"`
		Gzip   bool   `json:"gzip"`
		Chunks int    `json:"chunks"`
		Inode  string `json:"inode"`
	} `json:"files"`
}

func loadFixture(t *testing.T) *fixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/theres-no-distance-1.json")
	require.NoError(t, err)
	var fx fixture
	require.NoError(t, json.Unmarshal(raw, &fx))
	return &fx
}

// replayCaller answers from the recording; an unrecorded call is a test bug.
type replayCaller struct {
	t      *testing.T
	fx     *fixture
	tamper func(calldata string, result []byte) []byte
}

func (c *replayCaller) Call(_ context.Context, contract string, data []byte, block *big.Int) ([]byte, error) {
	c.t.Helper()
	assert.Equal(c.t, c.fx.Contract, contract)
	assert.Equal(c.t, 0, block.Cmp(new(big.Int).SetUint64(c.fx.Block)), "reads must be pinned to the fixture's block")
	key := "0x" + hex.EncodeToString(data)
	res, ok := c.fx.Calls[key]
	require.True(c.t, ok, "unrecorded call %s", key)
	if res == nil {
		return nil, ErrNotFound
	}
	out, err := hex.DecodeString(strings.TrimPrefix(*res, "0x"))
	require.NoError(c.t, err)
	if c.tamper != nil {
		out = c.tamper(key, out)
	}
	return out, nil
}

func newReader(t *testing.T, fx *fixture, tamper func(string, []byte) []byte) *Reader {
	t.Helper()
	r, err := NewReader(&replayCaller{t: t, fx: fx, tamper: tamper}, fx.Chain, new(big.Int).SetUint64(fx.Block))
	require.NoError(t, err)
	return r
}

func TestReadTreeVerifiesCaseyReasTheresNoDistance(t *testing.T) {
	fx := loadFixture(t)
	tree, err := newReader(t, fx, nil).ReadTree(context.Background(), fx.Root)
	require.NoError(t, err)
	require.Len(t, tree.Files, len(fx.Files))
	for p, want := range fx.Files {
		f := tree.Files[p]
		require.NotNil(t, f, p)
		sum := sha256.Sum256(f.Plain)
		assert.Equal(t, want.SHA256, hex.EncodeToString(sum[:]), p)
		assert.Equal(t, want.Bytes, len(f.Plain), p)
		assert.Equal(t, want.Gzip, f.Gzip, p)
		assert.Equal(t, want.Chunks, f.Chunks, p)
		assert.Equal(t, want.Inode, f.Inode, p)
	}
	// p5.js is the one file written in several chunks (fxhash used 24000-byte chunks), so it is the
	// one that exercises the chunk-size discovery and the per-chunk check.
	assert.Equal(t, 10, tree.Files["p5.min.js"].Chunks)
	assert.True(t, strings.HasPrefix(string(tree.Files["index.html"].Plain), "<!DOCTYPE html>"))
}

func TestReadTreeRefusesTamperedContent(t *testing.T) {
	fx := loadFixture(t)
	readFileSel := hex.EncodeToString(keccak([]byte("readFile(bytes32)"))[:4])
	tampered := 0
	r := newReader(t, fx, func(calldata string, out []byte) []byte {
		// flip one byte inside every readFile body; the chunk hashes (and so the address) no longer match
		if strings.HasPrefix(calldata, "0x"+readFileSel) && len(out) > 96 {
			out = append([]byte(nil), out...)
			out[96] ^= 0x01
			tampered++
		}
		return out
	})
	_, err := r.ReadTree(context.Background(), fx.Root)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrVerification), err)
	assert.Positive(t, tampered)
}

func TestReadTreeRefusesTamperedDirectory(t *testing.T) {
	fx := loadFixture(t)
	readDirSel := hex.EncodeToString(keccak([]byte("readDirectory(bytes32)"))[:4])
	r := newReader(t, fx, func(calldata string, out []byte) []byte {
		if strings.HasPrefix(calldata, "0x"+readDirSel) {
			// rename a child: the directory address covers keccak(name) of every entry
			s := strings.Replace(string(out), "style.css", "stile.css", 1)
			return []byte(s)
		}
		return out
	})
	_, err := r.ReadTree(context.Background(), fx.Root)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrVerification), err)
}

func TestReadTreeRefusesSwappedChunkList(t *testing.T) {
	fx := loadFixture(t)
	inodesSel := hex.EncodeToString(keccak([]byte("inodes(bytes32)"))[:4])
	r := newReader(t, fx, func(calldata string, out []byte) []byte {
		if strings.HasPrefix(calldata, "0x"+inodesSel) {
			// flip one byte of the first chunk checksum inside the File tuple (the tail of the output is the
			// empty Directory tuple, so the position has to be decoded, not guessed)
			d := decoder{b: out}
			fo := d.word(1)
			co := d.word(fo/32 + 1)
			require.NoError(t, d.err)
			out = append([]byte(nil), out...)
			out[fo+co+32] ^= 0x01
		}
		return out
	})
	_, err := r.ReadTree(context.Background(), fx.Root)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrVerification), err)
}

func TestParseURI(t *testing.T) {
	cid, inner, query, err := ParseURI("onchfs://1639E8C989CDDD2C0F9E951B126A21B1964309854C0A8E0A9B1193A655C6712A/?fxhash=0x06ec&fxiteration=1")
	require.NoError(t, err)
	assert.Equal(t, "1639e8c989cddd2c0f9e951b126a21b1964309854c0a8e0a9b1193a655c6712a", cid)
	assert.Equal(t, "", inner)
	assert.Equal(t, "fxhash=0x06ec&fxiteration=1", query)

	cid, inner, query, err = ParseURI("onchfs://1639e8c989cddd2c0f9e951b126a21b1964309854c0a8e0a9b1193a655c6712a/sketch.js")
	require.NoError(t, err)
	assert.Equal(t, "sketch.js", inner)
	assert.Equal(t, "", query)
	_ = cid

	for _, bad := range []string{"https://x/y", "onchfs://abc", "onchfs://1639e8c989cddd2c0f9e951b126a21b1964309854c0a8e0a9b1193a655c6712a/../x"} {
		_, _, _, err := ParseURI(bad)
		assert.Error(t, err, bad)
	}
}

func TestDecodeHeadersFromRealMetadata(t *testing.T) {
	// the HPACK block fxhash wrote for index.html in the fixture tree
	fx := loadFixture(t)
	tree, err := newReader(t, fx, nil).ReadTree(context.Background(), fx.Root)
	require.NoError(t, err)
	h := tree.Files["index.html"].Headers
	assert.Equal(t, "text/html", h["content-type"], h)
	assert.Equal(t, "gzip", h["content-encoding"], h)
}
