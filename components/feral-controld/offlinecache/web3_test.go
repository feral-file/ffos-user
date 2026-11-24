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
	// The entry, and only the entry. A method work must not also claim the
	// origin root: that URL belongs to whichever work on this contract is
	// the root, and replay's scope map would hand it to whichever record
	// loaded last (see web3Resources).
	for i, url := range []string{
		source, // the entry the kiosk navigates to
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

// The query survives on the entry — fxhash-style works read their seed from
// it and the kiosk navigates to the exact string — and the entry is the only
// URL stored. The bare path is NOT stored: it is another work's entry
// verbatim (`/artpiece` is the entry of `web3://…/artpiece`), and replay's
// scope map would answer that work from this one's bytes (round 3, F-B).
func TestWeb3MaterializerStoresTheEntryAloneWithItsQuery(t *testing.T) {
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
	assert.Len(t, urls, 1, "the entry is the only URL a method work may claim: %v", urls)
	assert.False(t, urls[w.Origin()+"/"], "the origin root belongs to a root work: %v", urls)
	assert.False(t, urls[w.Origin()+"/artpiece"], "the bare path is another work's entry: %v", urls)
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

// the kiosk navigates to one work's source and is served another's bytes, with
// no way to notice: a device-local source is exempt from every media-health
// verdict, so nothing probes it and no outcome is recorded against it.
func TestTwoWeb3WorksOnOneContractCannotClaimEachOthersURLs(t *testing.T) {
	store, _ := newTestStore(t)
	plain := web3Source(t, digitalSoulURI)
	seeded := web3Source(t, digitalSoulURI+"?seed=0x06ec")
	m := newWeb3Materializer(t, store, &fakeWeb3Resolver{body: digitalSoulBody})

	// One Ensure per work, each as the item the display opens on, so each is
	// read in the foreground and waited for. Handing both to one Ensure
	// would wait only for the first and leave the second to the background,
	// which makes the assertion wall-clock dependent — it passed alone and
	// failed under full-suite load.
	for _, src := range []string{plain, seeded} {
		require.Equal(t, 1, m.Ensure(context.Background(),
			[]dp1playlist.PlaylistItem{{ID: src, Source: src}}, 10*time.Second),
			"%s must be materialized before its URLs can be compared", src)
	}

	// Build the same map replay builds, in playlist order.
	owner := map[string]string{}
	for _, src := range []string{plain, seeded} {
		rec, err := store.LoadItem(offlinecache.SourceKey(src))
		require.NoError(t, err)
		for _, res := range rec.Resources {
			if prev, taken := owner[res.URL]; taken && prev != src {
				t.Errorf("%s is stored by two works: %s and %s — replay keeps only the last",
					res.URL, prev, src)
			}
			owner[res.URL] = src
		}
	}

	// The sharpest form: each work's own entry must still be its own.
	for _, src := range []string{plain, seeded} {
		if got := owner[src]; got != src {
			t.Errorf("the entry %s is answered from %s, so the wall shows the wrong work", src, got)
		}
	}
}

// A root-path work is the one case where the entry IS the origin root, so that
// is the only time the root may be stored.
func TestWeb3OriginRootIsStoredOnlyByARootWork(t *testing.T) {
	store, _ := newTestStore(t)
	root := web3Source(t, "web3://0x4f32740cf412d647a622ba27c36fa4b76f71601f:1")
	m := newWeb3Materializer(t, store, &fakeWeb3Resolver{body: digitalSoulBody})
	require.Equal(t, 1, m.Ensure(context.Background(), []dp1playlist.PlaylistItem{{ID: "a", Source: root}}, 10*time.Second))

	w, ok := contentpath.ParseWeb3("web3://0x4f32740cf412d647a622ba27c36fa4b76f71601f:1")
	require.True(t, ok)
	rec, err := store.LoadItem(offlinecache.SourceKey(root))
	require.NoError(t, err)
	var urls []string
	for _, res := range rec.Resources {
		urls = append(urls, res.URL)
	}
	assert.Contains(t, urls, w.Origin()+"/", "a root work's entry is the origin root: %v", urls)

	// And a method work must not claim it.
	store2, _ := newTestStore(t)
	m2 := newWeb3Materializer(t, store2, &fakeWeb3Resolver{body: digitalSoulBody})
	method := web3Source(t, digitalSoulURI)
	require.Equal(t, 1, m2.Ensure(context.Background(), []dp1playlist.PlaylistItem{{ID: "a", Source: method}}, 10*time.Second))
	rec2, err := store2.LoadItem(offlinecache.SourceKey(method))
	require.NoError(t, err)
	for _, res := range rec2.Resources {
		assert.NotEqual(t, w.Origin()+"/", res.URL,
			"a work with a method path must not claim the origin root a sibling could be")
	}
}

// Replay matches URLs exactly and Chromium percent-encodes what it is handed,
// so the stored entry is in Chromium's spelling while the contract call keeps
// the playlist's (round 5 review, F8).
func TestWeb3EntryIsStoredInTheBrowsersSpelling(t *testing.T) {
	cases := []struct{ uri, path string }{
		{"web3://0x4f32740cf412d647a622ba27c36fa4b76f71601f:1/artpiece?seed=it's", "/artpiece?seed=it%27s"},
		{"web3://0x4f32740cf412d647a622ba27c36fa4b76f71601f:1/artpiece/string!a^b", "/artpiece/string!a%5Eb"},
		{"web3://0x4f32740cf412d647a622ba27c36fa4b76f71601f:1/greet/string!hello world", "/greet/string!hello%20world"},
		{"web3://0x4f32740cf412d647a622ba27c36fa4b76f71601f:1/a%21b?q=%27x%27&r=é", "/a%21b?q=%27x%27&r=%C3%A9"},
	}
	for _, c := range cases {
		store, _ := newTestStore(t)
		source := web3Source(t, c.uri)
		m := newWeb3Materializer(t, store, &fakeWeb3Resolver{body: digitalSoulBody})
		require.Equal(t, 1, m.Ensure(context.Background(), []dp1playlist.PlaylistItem{{ID: "a", Source: source}}, 5*time.Second), c.uri)
		rec, err := store.LoadItem(offlinecache.SourceKey(source))
		require.NoError(t, err)
		w, _ := contentpath.ParseWeb3(c.uri)
		require.Len(t, rec.Resources, 1, c.uri)
		assert.Equal(t, w.Origin()+c.path, rec.Resources[0].URL, c.uri)
	}
}
