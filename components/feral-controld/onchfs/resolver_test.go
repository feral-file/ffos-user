package onchfs

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// testChain builds a chainReader whose reads are answered by fn per endpoint.
func testChain(chain string, endpoints []string, fn func(ep string) (*Tree, error)) *chainReader {
	return &chainReader{
		chain: chain, endpoints: endpoints, logger: zap.NewNop(), cooling: map[string]time.Time{}, now: time.Now,
		readFrom: func(_ context.Context, ep, _ string) (*Tree, error) { return fn(ep) },
	}
}

func TestResolverAsksEveryChainAndKeepsTheVerifiedTree(t *testing.T) {
	root := strings.Repeat("ab", 32)
	tree := &Tree{Root: root, Chain: TezosMainnet}
	r := &Resolver{logger: zap.NewNop(), chains: []*chainReader{
		testChain("eip155:1", []string{"e"}, func(string) (*Tree, error) { return nil, ErrNotFound }),
		testChain("eip155:8453", []string{"b"}, func(string) (*Tree, error) { return nil, ErrNotFound }),
		testChain(TezosMainnet, []string{"t"}, func(string) (*Tree, error) { return tree, nil }),
	}}
	got, err := r.Resolve(context.Background(), root, "")
	require.NoError(t, err)
	assert.Same(t, tree, got)

	_, err = r.Resolve(context.Background(), root, "eip155:1")
	assert.True(t, errors.Is(err, ErrNotFound), "an explicit authority asks that chain only")

	_, err = r.Resolve(context.Background(), root, "eip155:5")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNotFound), "an unconfigured chain is not proof of absence")
}

func TestResolverOutcomes(t *testing.T) {
	root := strings.Repeat("ab", 32)
	nodeDown := errors.New("connection refused")
	cases := map[string]struct {
		eth, tez     error
		wantNotFound bool
		wantTooLarge bool
	}{
		"absent everywhere":                 {ErrNotFound, ErrNotFound, true, false},
		"one chain unreachable is unknown":  {nodeDown, ErrNotFound, false, false},
		"too large is reported as such":     {ErrTooLarge, ErrNotFound, false, true},
		"every node lying is not an answer": {ErrVerification, ErrNotFound, false, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := &Resolver{logger: zap.NewNop(), chains: []*chainReader{
				testChain("eip155:1", []string{"e"}, func(string) (*Tree, error) { return nil, tc.eth }),
				testChain(TezosMainnet, []string{"t"}, func(string) (*Tree, error) { return nil, tc.tez }),
			}}
			_, err := r.Resolve(context.Background(), root, "")
			require.Error(t, err)
			assert.Equal(t, tc.wantNotFound, errors.Is(err, ErrNotFound), err)
			assert.Equal(t, tc.wantTooLarge, errors.Is(err, ErrTooLarge), err)
		})
	}
}

func TestChainReaderFailsOverAndCoolsDown(t *testing.T) {
	root := strings.Repeat("ab", 32)
	good := &Tree{Root: root}
	var mu sync.Mutex
	var asked []string
	answers := map[string]error{"liar": ErrVerification, "down": errors.New("HTTP 502"), "wrong": errWrongChain}
	c := testChain("eip155:1", []string{"liar", "down", "wrong", "ok"}, func(ep string) (*Tree, error) {
		mu.Lock()
		asked = append(asked, ep)
		mu.Unlock()
		if err, bad := answers[ep]; bad {
			return nil, err
		}
		return good, nil
	})
	now := time.Unix(1_000_000, 0)
	c.now = func() time.Time { return now }

	got, err := c.read(context.Background(), root, false)
	require.NoError(t, err)
	assert.Same(t, good, got)
	assert.Equal(t, []string{"liar", "down", "wrong", "ok"}, asked)

	// the three that failed are tried last until their cooldown ends
	asked = nil
	_, err = c.read(context.Background(), root, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"ok"}, asked)
	assert.Equal(t, []string{"ok", "liar", "down", "wrong"}, c.order())

	now = now.Add(endpointCooldown + time.Second)
	assert.Equal(t, []string{"liar", "down", "wrong", "ok"}, c.order())
}

func TestChainReaderStopsOnAnswersAboutTheWork(t *testing.T) {
	for _, want := range []error{ErrNotFound, ErrTooLarge} {
		asked := 0
		c := testChain("eip155:1", []string{"a", "b"}, func(string) (*Tree, error) { asked++; return nil, want })
		_, err := c.read(context.Background(), strings.Repeat("ab", 32), false)
		assert.True(t, errors.Is(err, want))
		assert.Equal(t, 1, asked, "%v is about the work; another node would say the same", want)
	}
}

// evmNode is a JSON-RPC endpoint for the RPCCaller and readEVM tests.
func evmNode(t *testing.T, chainID uint64, fs *fakeFS) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		reply := func(result any, rpcErr *rpcError) {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result, "error": rpcErr})
		}
		switch req.Method {
		case "eth_chainId":
			reply(fmt.Sprintf("0x%x", chainID), nil)
		case "eth_blockNumber":
			reply("0x10", nil)
		case "eth_call":
			var call struct{ Data string }
			require.NoError(t, json.Unmarshal(req.Params[0], &call))
			var block string
			require.NoError(t, json.Unmarshal(req.Params[1], &block))
			assert.Equal(t, "0xd", block, "pinned pinDepth below the head")
			out, err := fs.Call(r.Context(), "", mustHex(strings.TrimPrefix(call.Data, "0x")), nil)
			if errors.Is(err, ErrNotFound) {
				reply(nil, &rpcError{Code: 3, Message: "execution reverted"})
				return
			}
			require.NoError(t, err)
			reply("0x"+hexString(out), nil)
		}
	}))
}

func TestReadEVMChecksTheChainAndPinsTheBlock(t *testing.T) {
	fs := newFakeFS()
	index := fs.addFile(t, []byte("<html>"), map[string]string{"content-type": "text/html"})
	root := fs.addDir(map[string]string{"index.html": index})

	eth := evmNode(t, 1, fs)
	defer eth.Close()
	c := &chainReader{chain: "eip155:1", client: NewHTTPClient(time.Second)}
	tree, err := c.readEVM(context.Background(), eth.URL, root)
	require.NoError(t, err)
	assert.Equal(t, "13", tree.Block)
	assert.Equal(t, "eip155:1", tree.Chain)
	assert.Equal(t, "<html>", string(tree.Files["index.html"].Plain))

	// a node configured for Base that is really on Ethereum
	c = &chainReader{chain: "eip155:8453", client: NewHTTPClient(time.Second)}
	_, err = c.readEVM(context.Background(), eth.URL, root)
	assert.True(t, errors.Is(err, errWrongChain), err)

	_, err = (&chainReader{chain: "eip155:1", client: NewHTTPClient(time.Second)}).readEVM(context.Background(), eth.URL, strings.Repeat("cd", 32))
	assert.True(t, errors.Is(err, ErrNotFound), err)
}

func TestRPCCallerRefusesRedirectsAndHugeAnswers(t *testing.T) {
	redirect := httptest.NewServer(http.RedirectHandler("https://elsewhere.example/", http.StatusTemporaryRedirect))
	defer redirect.Close()
	_, err := NewRPCCaller(redirect.URL).ChainID(context.Background())
	require.Error(t, err)

	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":"0x`)
		chunk := strings.Repeat("00", 1<<16)
		for written := 0; written <= maxRPCBody; written += len(chunk) {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	}))
	defer huge.Close()
	_, err = NewRPCCaller(huge.URL).Call(context.Background(), "0x0", nil, big.NewInt(1))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}

func TestReadTreeLimits(t *testing.T) {
	small := func(l Limits) Limits {
		base := DefaultLimits
		if l.MaxPaths != 0 {
			base.MaxPaths = l.MaxPaths
		}
		if l.MaxTreeBytes != 0 {
			base.MaxTreeBytes = l.MaxTreeBytes
		}
		if l.MaxDirEntries != 0 {
			base.MaxDirEntries = l.MaxDirEntries
		}
		if l.MaxDepth != 0 {
			base.MaxDepth = l.MaxDepth
		}
		if l.MaxFileBytes != 0 {
			base.MaxFileBytes = l.MaxFileBytes
		}
		return base
	}
	fs := newFakeFS()
	a := fs.addFile(t, []byte("aaaa"), nil)
	b := fs.addFile(t, []byte("bbbb"), nil)
	inner := fs.addDir(map[string]string{"b.js": b})
	root := fs.addDir(map[string]string{"a.js": a, "lib": inner})
	reader, err := NewReader(fs, "eip155:1", big.NewInt(1))
	require.NoError(t, err)

	files, err := readTree(context.Background(), reader, root, DefaultLimits)
	require.NoError(t, err)
	assert.Len(t, files, 2)
	assert.Equal(t, "bbbb", string(files["lib/b.js"].Plain))

	for name, l := range map[string]Limits{
		"paths":       {MaxPaths: 2},
		"tree bytes":  {MaxTreeBytes: 7},
		"dir entries": {MaxDirEntries: 1},
		"depth":       {MaxDepth: -1},
		"file bytes":  {MaxFileBytes: 3},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readTree(context.Background(), reader, root, small(l))
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrTooLarge), err)
		})
	}
}

func TestMergeEndpoints(t *testing.T) {
	got := MergeEndpoints(map[string][]string{
		"eip155:1":   {"https://node.example/", "http://plain.example", "https://user:pw@x.example", "https://q.example/?key=1", "nope"},
		TezosMainnet: {"ftp://x"},
		"eip155:5":   {"https://goerli.example"},
	}, zap.NewNop())
	assert.Equal(t, []string{"https://node.example"}, got["eip155:1"], "bad entries are dropped, the rest applies")
	assert.Equal(t, DefaultEndpoints[TezosMainnet], got[TezosMainnet], "a list that salvages to nothing keeps the defaults")
	assert.Equal(t, DefaultEndpoints["eip155:8453"], got["eip155:8453"], "an unlisted chain keeps its defaults")
	assert.NotContains(t, got, "eip155:5")

	got["eip155:8453"][0] = "mutated"
	assert.NotEqual(t, "mutated", DefaultEndpoints["eip155:8453"][0], "the defaults are copied, not shared")
}

// PR #461 review F1: with no chain named, a Tezos node must not be able to
// answer for a root before the EVM chains have had their say, because any
// EVM file address is also a Tezos address for forged bytes.
func TestResolverAsksTezosOnlyAfterConfirmedEVMAbsence(t *testing.T) {
	root := strings.Repeat("ab", 32)
	evmTree := &Tree{Root: root, Chain: "eip155:1"}
	forged := &Tree{Root: root, Chain: TezosMainnet}
	nodeDown := errors.New("connection refused")
	type answers map[string]func() (*Tree, error)
	tree := func(t *Tree) func() (*Tree, error) { return func() (*Tree, error) { return t, nil } }
	fail := func(err error) func() (*Tree, error) { return func() (*Tree, error) { return nil, err } }

	cases := map[string]struct {
		eth, base, tez answers
		chain          string
		want           *Tree
		wantNotFound   bool
		tezosAsked     bool
	}{
		"an EVM work wins over a forged Tezos answer": {
			eth: answers{"e1": tree(evmTree)}, base: answers{"b1": fail(ErrNotFound), "b2": fail(ErrNotFound)},
			tez: answers{"t1": tree(forged)}, want: evmTree},
		"Tezos is asked once every EVM chain confirms absence": {
			eth: answers{"e1": fail(ErrNotFound), "e2": fail(ErrNotFound)}, base: answers{"b1": fail(ErrNotFound), "b2": fail(ErrNotFound)},
			tez: answers{"t1": tree(forged)}, want: forged, tezosAsked: true},
		"one EVM denial is not absence": {
			eth: answers{"e1": fail(ErrNotFound), "e2": fail(nodeDown)}, base: answers{"b1": fail(ErrNotFound), "b2": fail(ErrNotFound)},
			tez: answers{"t1": tree(forged)}},
		"a denial the next node contradicts is a lie": {
			eth: answers{"e1": fail(ErrNotFound), "e2": tree(evmTree)}, base: answers{"b1": fail(ErrNotFound), "b2": fail(ErrNotFound)},
			tez: answers{"t1": tree(forged)}, want: evmTree},
		"absent everywhere": {
			eth: answers{"e1": fail(ErrNotFound), "e2": fail(ErrNotFound)}, base: answers{"b1": fail(ErrNotFound), "b2": fail(ErrNotFound)},
			tez: answers{"t1": fail(ErrNotFound)}, wantNotFound: true, tezosAsked: true},
		"an EVM family never asks Tezos": {
			chain: "eip155",
			eth:   answers{"e1": fail(ErrNotFound)}, base: answers{"b1": fail(ErrNotFound)},
			tez: answers{"t1": tree(forged)}, wantNotFound: true},
		"a Tezos work is read from Tezos alone": {
			chain: TezosMainnet,
			eth:   answers{"e1": tree(evmTree)}, base: answers{"b1": tree(evmTree)},
			tez: answers{"t1": tree(forged)}, want: forged, tezosAsked: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			tezosAsked := false
			chainOf := func(chain string, a answers) *chainReader {
				eps := make([]string, 0, len(a))
				for ep := range a {
					eps = append(eps, ep)
				}
				sort.Strings(eps)
				return testChain(chain, eps, func(ep string) (*Tree, error) {
					if chain == TezosMainnet {
						mu.Lock()
						tezosAsked = true
						mu.Unlock()
					}
					return a[ep]()
				})
			}
			r := &Resolver{logger: zap.NewNop(), chains: []*chainReader{
				chainOf("eip155:1", tc.eth), chainOf("eip155:8453", tc.base), chainOf(TezosMainnet, tc.tez),
			}}
			got, err := r.Resolve(context.Background(), root, tc.chain)
			if tc.want != nil {
				require.NoError(t, err)
				assert.Same(t, tc.want, got)
			} else {
				require.Error(t, err)
				assert.Equal(t, tc.wantNotFound, errors.Is(err, ErrNotFound), err)
			}
			assert.Equal(t, tc.tezosAsked, tezosAsked)
		})
	}
}

// PR #461 review F2: the walk's per-inode cache keeps no raw bytes once a
// file is built; on EVM they are windows into whole RPC answers.
func TestWalkerKeepsNoRawInodeBytes(t *testing.T) {
	fs := newFakeFS()
	cid := fs.addFile(t, []byte("hello"), map[string]string{"content-type": "text/plain"})
	r, err := NewReader(fs, "eip155:1", big.NewInt(1))
	require.NoError(t, err)
	w := &walker{src: r, limits: DefaultLimits, sem: make(chan struct{}, 1), cancel: func() {},
		nodes: map[string]*node{}, files: map[string]*File{}}
	n, err := w.load(context.Background(), cid, "")
	require.NoError(t, err)
	assert.Nil(t, n.in.stored)
	assert.Nil(t, n.in.metadata)
	assert.Equal(t, "text/plain", n.file.ContentType)
}

// PR #461 review F10: many gzip files that each expand within MaxFileBytes
// must not all expand before the tree budget refuses them.
func TestReadTreeChargesDecompressionBeforeItHappens(t *testing.T) {
	fs := newFakeFS()
	names := map[string]string{}
	for i := range 32 {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		body := make([]byte, 4<<20)
		body[0] = byte(i) // distinct inodes
		_, _ = zw.Write(body)
		require.NoError(t, zw.Close())
		names[fmt.Sprintf("part-%02d.bin", i)] = fs.addFile(t, buf.Bytes(), map[string]string{"content-encoding": "gzip"})
	}
	root := fs.addDir(names)
	reader, err := NewReader(fs, "eip155:1", big.NewInt(1))
	require.NoError(t, err)
	limits := DefaultLimits
	limits.MaxTreeBytes = 16 << 20

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err = readTree(context.Background(), reader, root, limits)
	runtime.ReadMemStats(&after)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTooLarge), err)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(100<<20),
		"expansion stops at the tree budget instead of every file expanding first")
}

// peakGoroutines records the most goroutines alive at any RPC call.
type peakGoroutines struct {
	*fakeFS
	mu   sync.Mutex
	peak int
}

func (p *peakGoroutines) Call(ctx context.Context, contract string, data []byte, block *big.Int) ([]byte, error) {
	n := runtime.NumGoroutine()
	p.mu.Lock()
	p.peak = max(p.peak, n)
	p.mu.Unlock()
	return p.fakeFS.Call(ctx, contract, data, block)
}

// PR #461 review F12: a tree of directories that name the next one many
// times is refused without a goroutine per path.
func TestReadTreeBoundsTraversalBeforeSchedulingIt(t *testing.T) {
	fs := &peakGoroutines{fakeFS: newFakeFS()}
	next := fs.addFile(t, []byte("x"), nil)
	for range 3 {
		names := map[string]string{}
		for i := range 1024 {
			names[fmt.Sprintf("n%04d", i)] = next
		}
		next = fs.addDir(names)
	}
	reader, err := NewReader(fs, "eip155:1", big.NewInt(1))
	require.NoError(t, err)
	base := runtime.NumGoroutine()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err = reader.ReadTree(context.Background(), next)
	runtime.ReadMemStats(&after)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTooLarge), err)
	assert.LessOrEqual(t, fs.peak-base, DefaultLimits.ConcurrentReads+2, "a fixed worker pool, not a goroutine per path")
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(32<<20))
}
