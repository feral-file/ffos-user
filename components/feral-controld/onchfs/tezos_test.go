package onchfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tezosFixture is a recording of every view a verified read of one Tezos
// tree made at one block (see the fixture's "recorded" field).
type tezosFixture struct {
	Block string                      `json:"block"`
	Root  string                      `json:"root"`
	Calls map[string]*json.RawMessage `json:"calls"`
	Files map[string]struct {
		SHA256 string `json:"sha256"`
		Bytes  int    `json:"bytes"`
		Chunks int    `json:"chunks"`
		Inode  string `json:"inode"`
	} `json:"files"`
}

var tezosFixtures = []string{"testdata/tezos-ecd21507.json", "testdata/tezos-decf8b8a.json"}

func loadTezosFixture(t *testing.T, path string) *tezosFixture {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a fixed testdata path from tezosFixtures
	require.NoError(t, err)
	var fx tezosFixture
	require.NoError(t, json.Unmarshal(raw, &fx))
	return &fx
}

// replayViewer answers from a recording. The input is canonicalized (keys
// sorted) so the key does not depend on how the reader spelled its JSON.
type replayViewer struct {
	t      *testing.T
	fx     *tezosFixture
	tamper func(view, input string, out json.RawMessage) json.RawMessage
	// tampered counts answers the tamper actually changed, so a forgery test
	// cannot pass on an untouched recording.
	tampered int
	mu       sync.Mutex // the walk reads concurrently
}

func (v *replayViewer) View(_ context.Context, block, view string, input json.RawMessage) (json.RawMessage, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	assert.Equal(v.t, v.fx.Block, block, "reads must be pinned to the fixture's block")
	var generic any
	require.NoError(v.t, json.Unmarshal(input, &generic))
	canon, err := json.Marshal(generic)
	require.NoError(v.t, err)
	out, ok := v.fx.Calls[view+":"+string(canon)]
	require.True(v.t, ok, "unrecorded view %s:%s", view, canon)
	if out == nil {
		return nil, ErrNotFound
	}
	if v.tamper != nil {
		changed := v.tamper(view, string(canon), *out)
		if string(changed) != string(*out) {
			v.tampered++
		}
		return changed, nil
	}
	return *out, nil
}

func TestTezosReadTreeVerifiesRecordedTrees(t *testing.T) {
	for _, path := range tezosFixtures {
		t.Run(path, func(t *testing.T) {
			fx := loadTezosFixture(t, path)
			tree, err := NewTezosReader(&replayViewer{t: t, fx: fx}, fx.Block).ReadTree(context.Background(), fx.Root)
			require.NoError(t, err)
			assert.Equal(t, TezosMainnet, tree.Chain)
			require.Len(t, tree.Files, len(fx.Files))
			for p, want := range fx.Files {
				f := tree.Files[p]
				require.NotNil(t, f, p)
				sum := sha256.Sum256(f.Stored)
				assert.Equal(t, want.SHA256, hex.EncodeToString(sum[:]), p)
				assert.Equal(t, want.Bytes, len(f.Stored), p)
				assert.Equal(t, want.Chunks, f.Chunks, p)
				assert.Equal(t, want.Inode, f.Inode, p)
			}
			index := tree.Files["index.html"]
			assert.Equal(t, "text/html", index.ContentType)
			assert.Contains(t, strings.ToLower(string(index.Plain)), "<html")
		})
	}
	// the larger tree carries a library written in 13 chunks: read_file
	// returns it whole and the content address proves it without chunk reads
	fx := loadTezosFixture(t, tezosFixtures[1])
	tree, err := NewTezosReader(&replayViewer{t: t, fx: fx}, fx.Block).ReadTree(context.Background(), fx.Root)
	require.NoError(t, err)
	assert.Equal(t, 13, tree.Files["p5.min.js"].Chunks)
	assert.Equal(t, "application/javascript", tree.Files["p5.min.js"].ContentType)
}

// mutateMichelson rewrites the first "bytes" or "string" value equal to from.
func mutateMichelson(out json.RawMessage, from, to string) json.RawMessage {
	return json.RawMessage(strings.Replace(string(out), from, to, 1))
}

func TestTezosReadTreeRefusesForgeries(t *testing.T) {
	fx := loadTezosFixture(t, tezosFixtures[0])
	index := fx.Files["index.html"].Inode
	cases := map[string]func(view, input string, out json.RawMessage) json.RawMessage{
		"flipped content byte": func(view, input string, out json.RawMessage) json.RawMessage {
			if view != "read_file" {
				return out
			}
			var m michelson
			require.NoError(t, json.Unmarshal(out, &m))
			var content michelson
			require.NoError(t, json.Unmarshal(m.Args[0], &content))
			b := []byte(*content.Bytes)
			if b[0] == '0' { // swap the first hex digit for another one
				b[0] = '1'
			} else {
				b[0] = '0'
			}
			return mutateMichelson(out, *content.Bytes, string(b))
		},
		"renamed directory entry": func(view, input string, out json.RawMessage) json.RawMessage {
			if view == "get_inode_at" && strings.Contains(input, fx.Root) {
				return mutateMichelson(out, `"index.html"`, `"indez.html"`)
			}
			return out
		},
		"node answers for another inode": func(view, input string, out json.RawMessage) json.RawMessage {
			if view == "get_inode_at" && strings.Contains(input, index) {
				return mutateMichelson(out, index, fx.Files["fxhash.min.js"].Inode)
			}
			return out
		},
		"changed headers": func(view, input string, out json.RawMessage) json.RawMessage {
			if view == "read_file" && strings.Contains(input, index) {
				var m michelson
				require.NoError(t, json.Unmarshal(out, &m))
				var meta michelson
				require.NoError(t, json.Unmarshal(m.Args[1], &meta))
				tampered := mutateMichelson(out, `"`+*meta.Bytes+`"`, `"00`+*meta.Bytes+`"`)
				require.NotEqual(t, string(out), string(tampered))
				return tampered
			}
			return out
		},
		"not micheline": func(view, input string, out json.RawMessage) json.RawMessage {
			if view == "read_file" {
				return json.RawMessage(`{"prim":"Pair","args":[{"int":"1"}]}`)
			}
			return out
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			v := &replayViewer{t: t, fx: fx, tamper: tamper}
			_, err := NewTezosReader(v, fx.Block).ReadTree(context.Background(), fx.Root)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrVerification), err)
			assert.Positive(t, v.tampered)
		})
	}
}

func TestTezosReadTreeNotFound(t *testing.T) {
	fx := loadTezosFixture(t, tezosFixtures[0])
	// the probe recorded one absent root: the real root with its first byte zeroed
	missing := "00" + fx.Root[2:]
	_, err := NewTezosReader(&replayViewer{t: t, fx: fx}, fx.Block).ReadTree(context.Background(), missing)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound), err)
}

func TestTezosRPC(t *testing.T) {
	const rejected = `[{"kind":"temporary","id":"proto.025-PsUshuai.michelson_v1.runtime_error"},` +
		`{"kind":"temporary","id":"proto.025-PsUshuai.michelson_v1.script_rejected","location":32,"with":{"string":"ONCHFS_INODE_NOT_FOUND"}}]`
	var gotPath string
	var gotBody map[string]any
	answer := func(w http.ResponseWriter, status int, body string) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method == http.MethodGet {
			answer(w, 200, `{"chain_id":"NetXdQprcVkpaWU","level":15794960,"hash":"BLx"}`)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		switch gotBody["view"] {
		case "read_file":
			answer(w, 200, `{"data":{"prim":"Pair","args":[{"bytes":"00"},{"bytes":""}]}}`)
		case "get_inode_at":
			answer(w, 500, rejected)
		case "read_chunk":
			answer(w, 500, `[{"kind":"temporary","id":"proto.025-PsUshuai.gas_exhausted.operation"}]`)
		default:
			http.Redirect(w, r, "https://elsewhere.example/", http.StatusFound)
		}
	}))
	defer srv.Close()
	node := &TezosRPC{Endpoint: srv.URL + "/", Client: NewHTTPClient(DefaultRequestTimeout)}

	chainID, level, err := node.Head(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "NetXdQprcVkpaWU", chainID)
	assert.Equal(t, int64(15794960), level)

	out, err := node.View(context.Background(), "15794957", "read_file", json.RawMessage(`{"bytes":"ab"}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"prim":"Pair","args":[{"bytes":"00"},{"bytes":""}]}`, string(out))
	assert.Equal(t, "/chains/main/blocks/15794957/helpers/scripts/run_script_view", gotPath)
	assert.Equal(t, TezosContract, gotBody["contract"])
	assert.Equal(t, "NetXdQprcVkpaWU", gotBody["chain_id"])
	assert.Equal(t, "Optimized", gotBody["unparsing_mode"])

	_, err = node.View(context.Background(), "15794957", "get_inode_at", json.RawMessage(`{}`))
	assert.True(t, errors.Is(err, ErrNotFound), err)

	_, err = node.View(context.Background(), "15794957", "read_chunk", json.RawMessage(`{}`))
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNotFound), "a node failure is not an absent inode")

	_, err = node.View(context.Background(), "15794957", "other", json.RawMessage(`{}`))
	require.Error(t, err, "a redirect is not followed")

	_, err = node.View(context.Background(), "head;rm", "read_file", json.RawMessage(`{}`))
	require.Error(t, err, "the block goes into the URL path, so only a level or a hash is accepted")
}
