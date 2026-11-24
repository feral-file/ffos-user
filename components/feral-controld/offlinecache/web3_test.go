package offlinecache_test

import (
	"context"
	"encoding/base64"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/contentpath"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/web3"
)

// digitalSoulURI is Han Saglam's Digital Soul, the work feral-file#3599
// names: the return of artpiece() on Ethereum, with no host to load it from.
const digitalSoulURI = "web3://0x4f32740cf412d647a622ba27c36fa4b76f71601f:1/artpiece"

const digitalSoulBody = "<!DOCTYPE html><html><head><title>DigitalSoul</title></head><body></body></html>"

func web3Source(t *testing.T, uri string) string {
	t.Helper()
	w, ok := contentpath.ParseWeb3(uri)
	require.True(t, ok, "ParseWeb3(%q)", uri)
	return w.SourceURL()
}

// fakeWeb3Resolver answers with a work, or an error, and counts its calls so
// a test can prove a fresh record is not re-read.
type fakeWeb3Resolver struct {
	body        string
	contentType string
	err         error
	calls       atomic.Int32
	lastReq     atomic.Value
}

func (r *fakeWeb3Resolver) Resolve(_ context.Context, req web3.Request) (*web3.Work, error) {
	r.calls.Add(1)
	r.lastReq.Store(req)
	if r.err != nil {
		return nil, r.err
	}
	ct := r.contentType
	if ct == "" {
		ct = "text/html"
	}
	return &web3.Work{
		Contract: req.Contract, ChainID: req.ChainID, Block: "97", Mode: web3.ModeAuto,
		Body: []byte(r.body), ContentType: ct, ReadFrom: "https://node.example", Confirmed: true,
	}, nil
}

func newWeb3Materializer(t *testing.T, store offlinecache.Store, r offlinecache.Web3CallResolver) *offlinecache.DeviceLocalMaterializer {
	t.Helper()
	return newTestMaterializerWithWeb3(t, store, nil, r, offlinecache.DeviceLocalMaterializerConfig{})
}

func TestWeb3MaterializerWritesARecordReplayServes(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	source := web3Source(t, digitalSoulURI)
	resolver := &fakeWeb3Resolver{body: digitalSoulBody}
	m := newWeb3Materializer(t, ts.store, resolver)

	items := []dp1playlist.PlaylistItem{{ID: "a", Source: source}, {ID: "b", Source: "https://cdn.example/x.png"}}
	require.Equal(t, 1, m.Ensure(context.Background(), items, 5*time.Second))

	// The resolver is asked the call the URL names, and nothing else: no
	// function name of the player's own reaches it.
	req, _ := resolver.lastReq.Load().(web3.Request)
	assert.Equal(t, "0x4f32740cf412d647a622ba27c36fa4b76f71601f", req.Contract)
	assert.Equal(t, uint64(1), req.ChainID)
	assert.Equal(t, "artpiece", req.Path)

	rec, err := ts.store.LoadItem(offlinecache.SourceKey(source))
	require.NoError(t, err)
	assert.True(t, rec.Automatic, "kept out of the app's Downloads list")
	assert.True(t, rec.Coverage.Complete)
	assert.Equal(t, source, rec.Item.Source)

	// A second Ensure finds the record fresh and makes no call.
	require.Equal(t, 1, m.Ensure(context.Background(), items, time.Second))
	assert.Equal(t, int32(1), resolver.calls.Load())

	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForPlaylist(context.Background(), []string{source}, false))

	w, _ := contentpath.ParseWeb3(digitalSoulURI)
	for i, url := range []string{
		source,           // the entry the kiosk navigates to
		w.Origin() + "/", // what a relative "/" in the document resolves to
	} {
		done := ts.expectSend("Fetch.fulfillRequest")
		ts.handler(requestPausedEvent(t, "r"+string(rune('a'+i)), url))
		params := awaitSend(t, done)
		assert.Equal(t, 200, params["responseCode"], url)
		body, err := base64.StdEncoding.DecodeString(params["body"].(string))
		require.NoError(t, err)
		assert.Equal(t, digitalSoulBody, string(body), url)
		ct, has := headerValue(t, params, "Content-Type")
		assert.True(t, has, url)
		assert.Equal(t, "text/html", ct, url)
		_, encoded := headerValue(t, params, "Content-Encoding")
		assert.False(t, encoded, "bodies are stored plain")
	}

	// The player's cross-origin HEAD probe, with its cache-busting params.
	done := ts.expectSend("Fetch.fulfillRequest")
	ts.handler(requestPausedEventWithMethod(t, "head", source+"?v=1&x-request=xhr", "HEAD"))
	params := awaitSend(t, done)
	assert.Equal(t, 200, params["responseCode"])
	acao, _ := headerValue(t, params, "Access-Control-Allow-Origin")
	assert.Equal(t, "*", acao)

	// A sub-resource the call did not return is not part of the work. The
	// miss continues to the network, where a .invalid host never resolves
	// (RFC 6761), so nothing is fetched.
	done = ts.expectSend("Fetch.continueRequest")
	ts.handler(requestPausedEvent(t, "miss", w.Origin()+"/lib.js"))
	awaitSend(t, done)
}

// A work whose URL carries a query is served under the query too: fxhash-style
// works read their seed from it, and the kiosk navigates to the exact string.
func TestWeb3MaterializerKeepsTheQueryAndAlsoServesThePath(t *testing.T) {
	store, _ := newTestStore(t)
	source := web3Source(t, digitalSoulURI+"?seed=0x06ec")
	m := newWeb3Materializer(t, store, &fakeWeb3Resolver{body: digitalSoulBody})

	require.Equal(t, 1, m.Ensure(context.Background(), []dp1playlist.PlaylistItem{{ID: "a", Source: source}}, 5*time.Second))
	rec, err := store.LoadItem(offlinecache.SourceKey(source))
	require.NoError(t, err)

	urls := map[string]bool{}
	for _, res := range rec.Resources {
		urls[res.URL] = true
		assert.Equal(t, "text/html", res.ContentType)
		assert.Equal(t, "*", res.Headers["Access-Control-Allow-Origin"])
		assert.False(t, res.FreshUntil.IsZero(), "an Automatic resource needs a freshness bound")
	}
	w, _ := contentpath.ParseWeb3(digitalSoulURI + "?seed=0x06ec")
	assert.True(t, urls[source], "the entry carries the seed: %v", urls)
	assert.True(t, urls[w.Origin()+"/"], "the origin root is served: %v", urls)
	assert.True(t, urls[w.Origin()+"/artpiece"], "the path without the query is served: %v", urls)
	assert.Equal(t, source, rec.Entry)
}

// The content type comes from the URL, never from the bytes: a contract call
// carries no header, and sniffing would let the work choose how it is shown.
func TestWeb3MaterializerServesTheDeclaredType(t *testing.T) {
	store, _ := newTestStore(t)
	source := web3Source(t, "web3://0x4f32740cf412d647a622ba27c36fa4b76f71601f:1/render/frame.svg")
	m := newWeb3Materializer(t, store, &fakeWeb3Resolver{body: "<svg/>", contentType: "image/svg+xml"})

	require.Equal(t, 1, m.Ensure(context.Background(), []dp1playlist.PlaylistItem{{ID: "a", Source: source}}, 5*time.Second))
	rec, err := store.LoadItem(offlinecache.SourceKey(source))
	require.NoError(t, err)
	require.NotEmpty(t, rec.Resources)
	for _, res := range rec.Resources {
		assert.Equal(t, "image/svg+xml", res.ContentType, res.URL)
	}
}

// A work that cannot be read stays unplayable and must not fail the display
// request, exactly as an onchfs work the chain RPCs cannot reach does.
func TestWeb3MaterializerLeavesAnUnreadableWorkAlone(t *testing.T) {
	store, _ := newTestStore(t)
	source := web3Source(t, digitalSoulURI)
	m := newWeb3Materializer(t, store, &fakeWeb3Resolver{err: errors.New("every endpoint failed")})

	assert.Equal(t, 0, m.Ensure(context.Background(), []dp1playlist.PlaylistItem{{ID: "a", Source: source}}, 2*time.Second))
	_, err := store.LoadItem(offlinecache.SourceKey(source))
	assert.ErrorIs(t, err, offlinecache.ErrItemNotFound, "nothing is saved for a work that could not be read")
}

// With no web3 resolver wired (its config kill switch, or a build that never
// wired one) a web3 work is left unplayable — and an onchfs work on the same
// playlist still plays. One scheme being off never takes another down.
func TestADisabledSchemeDoesNotTakeTheOtherDown(t *testing.T) {
	store, _ := newTestStore(t)
	onchfs := onchfsSource(t, "onchfs://"+onchfsTestRoot+"/?fxhash=1")
	web3src := web3Source(t, digitalSoulURI)
	m := newTestMaterializerWithWeb3(t, store, &fakeOnchfsResolver{tree: onchfsTestTree()}, nil,
		offlinecache.DeviceLocalMaterializerConfig{})

	items := []dp1playlist.PlaylistItem{{ID: "a", Source: onchfs}, {ID: "b", Source: web3src}}
	assert.Equal(t, 1, m.Ensure(context.Background(), items, 5*time.Second))

	rec, err := store.LoadItem(offlinecache.SourceKey(onchfs))
	require.NoError(t, err)
	assert.NotNil(t, rec, "the onchfs work still plays")
	_, err = store.LoadItem(offlinecache.SourceKey(web3src))
	assert.ErrorIs(t, err, offlinecache.ErrItemNotFound, "the web3 work, with no resolver, stays unplayable")
}

// The two schemes share one disk, so they share one retention pool and one
// LRU order: a web3 work nothing has played is the right thing to drop to
// make room for an onchfs work the playlist is about to show.
func TestRetentionPoolSpansBothSchemes(t *testing.T) {
	store, _ := newTestStore(t)
	web3src := web3Source(t, digitalSoulURI)
	m := newTestMaterializerWithWeb3(t, store, &fakeOnchfsResolver{tree: onchfsTestTree()},
		&fakeWeb3Resolver{body: digitalSoulBody}, offlinecache.DeviceLocalMaterializerConfig{})

	require.Equal(t, 1, m.Ensure(context.Background(), []dp1playlist.PlaylistItem{{ID: "b", Source: web3src}}, 5*time.Second))
	onchfs := onchfsSource(t, "onchfs://"+onchfsTestRoot+"/?fxhash=1")
	require.Equal(t, 1, m.Ensure(context.Background(), []dp1playlist.PlaylistItem{{ID: "a", Source: onchfs}}, 5*time.Second))

	// Both records are in the one pool the materializer prunes and evicts
	// from; neither scheme keeps a pool of its own.
	keys, err := store.ListItemKeys()
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, k := range keys {
		rec, err := store.LoadItem(k)
		require.NoError(t, err)
		if rec != nil && contentpath.IsDeviceLocalSource(rec.Item.Source) {
			seen[string(contentpath.DeviceLocalSourceScheme(rec.Item.Source))] = true
		}
	}
	assert.True(t, seen["onchfs"], "the onchfs record is in the device-local pool: %v", seen)
	assert.True(t, seen["web3"], "the web3 record is in the device-local pool: %v", seen)
}
