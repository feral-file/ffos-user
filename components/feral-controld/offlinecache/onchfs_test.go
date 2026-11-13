package offlinecache_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/contentpath"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/onchfs"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const onchfsTestRoot = "1639e8c989cddd2c0f9e951b126a21b1964309854c0a8e0a9b1193a655c6712a"

// onchfsSource is the device-local source dp1 hands the player for the
// indexer's `onchfs://<root>/?fxhash=…`.
func onchfsSource(t *testing.T, uri string) string {
	t.Helper()
	o, ok := contentpath.ParseOnchfs(uri)
	require.True(t, ok)
	return o.SourceURL()
}

func onchfsTestTree() *onchfs.Tree {
	file := func(p, ct, body string) *onchfs.File {
		return &onchfs.File{Path: p, Plain: []byte(body), Stored: []byte(body), ContentType: ct}
	}
	return &onchfs.Tree{Root: onchfsTestRoot, Chain: "eip155:1", Files: map[string]*onchfs.File{
		"index.html":     file("index.html", "text/html", "<html>root</html>"),
		"sketch.js":      file("sketch.js", "application/javascript", "draw()"),
		"lib/a b.js":     file("lib/a b.js", "application/javascript", "lib()"),
		"sub/index.html": file("sub/index.html", "text/html", "<html>sub</html>"),
		"data.bin":       file("data.bin", "", "raw"),
	}}
}

// fakeOnchfsResolver answers with a tree, an error, or after a gate opens.
type fakeOnchfsResolver struct {
	tree  *onchfs.Tree
	err   error
	gate  chan struct{}
	calls atomic.Int32
}

func (r *fakeOnchfsResolver) Resolve(ctx context.Context, root, _ string) (*onchfs.Tree, error) {
	r.calls.Add(1)
	if r.gate != nil {
		select {
		case <-r.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	t := *r.tree
	t.Root = root
	return &t, nil
}

func newTestMaterializer(t *testing.T, store offlinecache.Store, r offlinecache.OnchfsResolver, cfg offlinecache.OnchfsMaterializerConfig) *offlinecache.OnchfsMaterializer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg.Store, cfg.Resolver, cfg.Logger, cfg.Base = store, r, zaptest.NewLogger(t), ctx
	return offlinecache.NewOnchfsMaterializer(cfg)
}

func TestOnchfsMaterializerWritesARecordReplayServes(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	source := onchfsSource(t, "onchfs://"+onchfsTestRoot+"/?fxhash=0x06ec&fxiteration=1")
	resolver := &fakeOnchfsResolver{tree: onchfsTestTree()}
	m := newTestMaterializer(t, ts.store, resolver, offlinecache.OnchfsMaterializerConfig{})

	items := []dp1playlist.PlaylistItem{{ID: "a", Source: source}, {ID: "b", Source: "https://cdn.example/x.png"}}
	require.Equal(t, 1, m.Ensure(context.Background(), items, 5*time.Second))
	rec, err := ts.store.LoadItem(offlinecache.SourceKey(source))
	require.NoError(t, err)
	assert.True(t, rec.Automatic, "kept out of the app's Downloads list")
	assert.True(t, rec.Coverage.Complete)
	assert.Equal(t, source, rec.Item.Source)

	// a second Ensure finds the record fresh and reads nothing from chain
	require.Equal(t, 1, m.Ensure(context.Background(), items, time.Second))
	assert.Equal(t, int32(1), resolver.calls.Load())

	ts.stubFetchEnable()
	require.NoError(t, ts.replayer.EnableForPlaylist(context.Background(), []string{source}, false))

	origin := "https://cy46rsmjzxosyd46sunre2rbwglegcmfjqfi4cu3cgj2mvogoeva.onchfs.invalid"
	serves := []struct {
		url, method, body, contentType string
	}{
		{source, "", "<html>root</html>", "text/html"}, // the entry, seed query included
		{origin + "/", "", "<html>root</html>", "text/html"},
		{origin + "/sketch.js", "", "draw()", "application/javascript"},
		{origin + "/lib/a%20b.js", "", "lib()", "application/javascript"},
		{origin + "/sub/", "", "<html>sub</html>", "text/html"},
		{origin + "/data.bin", "", "raw", ""},
	}
	for i, s := range serves {
		done := ts.expectSend("Fetch.fulfillRequest")
		ts.handler(requestPausedEventWithMethod(t, "r"+string(rune('a'+i)), s.url, s.method))
		params := awaitSend(t, done)
		assert.Equal(t, 200, params["responseCode"], s.url)
		body, err := base64.StdEncoding.DecodeString(params["body"].(string))
		require.NoError(t, err)
		assert.Equal(t, s.body, string(body), s.url)
		ct, has := headerValue(t, params, "Content-Type")
		assert.Equal(t, s.contentType != "", has, "no Content-Type is invented for %s", s.url)
		assert.Equal(t, s.contentType, ct, s.url)
		_, encoded := headerValue(t, params, "Content-Encoding")
		assert.False(t, encoded, "bodies are stored plain")
	}

	// the player's cross-origin HEAD probe, with its cache-busting params
	done := ts.expectSend("Fetch.fulfillRequest")
	ts.handler(requestPausedEventWithMethod(t, "head", source+"&v=1&x-request=xhr", "HEAD"))
	params := awaitSend(t, done)
	assert.Equal(t, 200, params["responseCode"])
	acao, _ := headerValue(t, params, "Access-Control-Allow-Origin")
	assert.Equal(t, "*", acao)

	// Anything else on the origin is not part of the work. The record is
	// Automatic, which makes the scope mixed (EnableForPlaylist), so the
	// miss continues to the network, where a .invalid host never resolves
	// (RFC 6761): it fails there, and nothing is fetched.
	done = ts.expectSend("Fetch.continueRequest")
	ts.handler(requestPausedEvent(t, "miss", origin+"/missing.js"))
	awaitSend(t, done)
}

func TestOnchfsMaterializerEntryShapes(t *testing.T) {
	cases := map[string]struct {
		uri, wantURL string
		redirect     string
	}{
		"a file inside the tree":       {"onchfs://" + onchfsTestRoot + "/sketch.js", "/sketch.js", ""},
		"a directory with its slash":   {"onchfs://" + onchfsTestRoot + "/sub/?x=1", "/sub/?x=1", ""},
		"a directory without it":       {"onchfs://" + onchfsTestRoot + "/sub?x=1", "/sub?x=1", "/sub/?x=1"},
		"the root without a slash too": {"onchfs://" + onchfsTestRoot + "?x=1", "/?x=1", ""},
	}
	origin := "https://cy46rsmjzxosyd46sunre2rbwglegcmfjqfi4cu3cgj2mvogoeva.onchfs.invalid"
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store, _ := newTestStore(t)
			source := onchfsSource(t, tc.uri)
			assert.Equal(t, origin+tc.wantURL, source)
			m := newTestMaterializer(t, store, &fakeOnchfsResolver{tree: onchfsTestTree()}, offlinecache.OnchfsMaterializerConfig{})
			require.Equal(t, 1, m.Ensure(context.Background(), []dp1playlist.PlaylistItem{{Source: source}}, 5*time.Second))
			rec, err := store.LoadItem(offlinecache.SourceKey(source))
			require.NoError(t, err)
			res, ok := rec.ResourceByURL(source)
			require.True(t, ok)
			if tc.redirect != "" {
				assert.Equal(t, 301, res.Status)
				assert.Equal(t, origin+tc.redirect, res.RedirectTo)
				// the redirect's target, query included, is served too
				followed, ok := rec.ResourceByURL(origin + tc.redirect)
				require.True(t, ok, "PR #461 review F5")
				assert.Equal(t, 200, followed.Status)
				body, err := store.ReadBlob(followed.SHA256)
				require.NoError(t, err)
				assert.Equal(t, "<html>sub</html>", string(body))
			} else {
				assert.Equal(t, 200, res.Status)
			}
		})
	}
}

func TestOnchfsMaterializerFailuresLeaveNoRecord(t *testing.T) {
	cases := map[string]*fakeOnchfsResolver{
		"not on any chain":  {err: onchfs.ErrNotFound},
		"every node lying":  {err: onchfs.ErrVerification},
		"no entry to open":  {tree: &onchfs.Tree{Files: map[string]*onchfs.File{"other.js": {Plain: []byte("x")}}}},
		"too large to keep": {err: onchfs.ErrTooLarge},
		"no node answering": {err: errors.New("connection refused")},
	}
	for name, resolver := range cases {
		t.Run(name, func(t *testing.T) {
			store, _ := newTestStore(t)
			source := onchfsSource(t, "onchfs://"+onchfsTestRoot)
			m := newTestMaterializer(t, store, resolver, offlinecache.OnchfsMaterializerConfig{})
			assert.Zero(t, m.Ensure(context.Background(), []dp1playlist.PlaylistItem{{Source: source}}, 5*time.Second))
			keys, err := store.ListItemKeys()
			require.NoError(t, err)
			assert.Empty(t, keys)
		})
	}
}

// A slow read does not hold the cast: Ensure returns at its wait, the job
// finishes in the background, OnReady tells the caller to widen the scope,
// and a second Ensure for the same work joins the running job.
func TestOnchfsMaterializerFinishesSlowWorkInTheBackground(t *testing.T) {
	store, _ := newTestStore(t)
	gate := make(chan struct{})
	resolver := &fakeOnchfsResolver{tree: onchfsTestTree(), gate: gate}
	readyCalls := make(chan struct{}, 4)
	m := newTestMaterializer(t, store, resolver, offlinecache.OnchfsMaterializerConfig{
		OnReady: func() { readyCalls <- struct{}{} },
	})
	items := []dp1playlist.PlaylistItem{{Source: onchfsSource(t, "onchfs://"+onchfsTestRoot)}}

	assert.Zero(t, m.Ensure(context.Background(), items, 20*time.Millisecond))
	assert.Zero(t, m.Ensure(context.Background(), items, 20*time.Millisecond))
	close(gate)
	select {
	case <-readyCalls:
	case <-time.After(5 * time.Second):
		t.Fatal("OnReady was not called")
	}
	assert.Equal(t, int32(1), resolver.calls.Load(), "the second Ensure joined the running job")
	assert.Equal(t, 1, m.Ensure(context.Background(), items, time.Second))
}

// sweepingStore runs the store's GC inside SaveItem before saving, the
// worst moment a Service worker's GC could run against this producer.
type sweepingStore struct {
	offlinecache.Store
	swept atomic.Int64
}

func (s *sweepingStore) SaveItem(rec *offlinecache.ItemRecord) error {
	if n, _, err := s.GC(); err == nil {
		s.swept.Add(int64(n))
	}
	return s.Store.SaveItem(rec)
}

func TestOnchfsMaterializerRewritesBlobsSweptBeforeTheRecordWasSaved(t *testing.T) {
	base, _ := newTestStore(t)
	store := &sweepingStore{Store: base}
	source := onchfsSource(t, "onchfs://"+onchfsTestRoot)
	m := newTestMaterializer(t, store, &fakeOnchfsResolver{tree: onchfsTestTree()}, offlinecache.OnchfsMaterializerConfig{})
	require.Equal(t, 1, m.Ensure(context.Background(), []dp1playlist.PlaylistItem{{Source: source}}, 5*time.Second))
	assert.Positive(t, store.swept.Load(), "the GC did sweep the unreferenced blobs")
	rec, err := base.LoadItem(offlinecache.SourceKey(source))
	require.NoError(t, err)
	for _, res := range rec.Resources {
		if res.SHA256 == "" {
			continue
		}
		_, err := base.ReadBlob(res.SHA256)
		assert.NoError(t, err, res.URL)
	}
}

// Standalone (no Service), the materializer is the only writer: it keeps
// its records under its byte budget, oldest played first, and collects the
// blobs they leave behind.
func TestOnchfsMaterializerStandaloneKeepsItsBudget(t *testing.T) {
	store, _ := newTestStore(t)
	clock := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	tick := func() { mu.Lock(); clock = clock.Add(time.Minute); mu.Unlock() }

	treeOf := func(body string) *onchfs.Tree {
		return &onchfs.Tree{Files: map[string]*onchfs.File{"index.html": {Plain: []byte(body), ContentType: "text/html"}}}
	}
	roots := []string{
		"1111111111111111111111111111111111111111111111111111111111111111",
		"2222222222222222222222222222222222222222222222222222222222222222",
		"3333333333333333333333333333333333333333333333333333333333333333",
	}
	var sources []string
	for i, root := range roots {
		resolver := &fakeOnchfsResolver{tree: treeOf(string(rune('a'+i)) + "0123456789")} // 11 bytes each
		m := newTestMaterializer(t, store, resolver, offlinecache.OnchfsMaterializerConfig{Standalone: true, MaxBytes: 25, Now: now})
		source := onchfsSource(t, "onchfs://"+root)
		sources = append(sources, source)
		require.Equal(t, 1, m.Ensure(context.Background(), []dp1playlist.PlaylistItem{{Source: source}}, 5*time.Second))
		tick()
	}
	_, err := store.LoadItem(offlinecache.SourceKey(sources[0]))
	assert.Error(t, err, "the oldest record went to keep the budget")
	for _, s := range sources[1:] {
		_, err := store.LoadItem(offlinecache.SourceKey(s))
		assert.NoError(t, err)
	}
	used, err := store.DiskUsage()
	require.NoError(t, err)
	assert.Less(t, used, int64(1<<20))
}

// PR #461 review F3: the store BootstrapCore builds runs GC only while no
// publication is in progress, so a GC cannot mark before a record is saved
// and sweep after.
func TestBootstrapCoreStoreExcludesGCDuringAPublication(t *testing.T) {
	opts := offlinecache.OptionsFromConfig(nil, "http://127.0.0.1:9222")
	opts.RootDir, opts.MaxDiskPercent = t.TempDir(), 0
	core, _ := offlinecache.BootstrapCore(opts, nil, nil, wrapper.NewOS(), wrapper.NewIO(), wrapper.NewJSON(), wrapper.NewClock(), zaptest.NewLogger(t), true)
	pub, ok := core.Store.(interface{ Publish(func() error) error })
	require.True(t, ok, "the core store is the publish guard")

	inPublish, release, gcDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		_ = pub.Publish(func() error { close(inPublish); <-release; return nil })
	}()
	<-inPublish
	go func() { _, _, _ = core.Store.GC(); close(gcDone) }()
	select {
	case <-gcDone:
		t.Fatal("GC ran during a publication")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-gcDone:
	case <-time.After(5 * time.Second):
		t.Fatal("GC never ran after the publication")
	}
}

// publishRecordingStore records whether each write happened inside Publish.
type publishRecordingStore struct {
	offlinecache.Store
	mu       sync.Mutex
	inside   bool
	outsides []string
}

func (s *publishRecordingStore) Publish(fn func() error) error {
	s.mu.Lock()
	s.inside = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.inside = false; s.mu.Unlock() }()
	return fn()
}

func (s *publishRecordingStore) note(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inside {
		s.outsides = append(s.outsides, op)
	}
}

func (s *publishRecordingStore) WriteBlob(r io.Reader, n int64) (string, error) {
	s.note("WriteBlob")
	return s.Store.WriteBlob(r, n)
}

func (s *publishRecordingStore) SaveItem(rec *offlinecache.ItemRecord) error {
	s.note("SaveItem")
	return s.Store.SaveItem(rec)
}

func TestOnchfsMaterializerPublishesBlobsAndRecordTogether(t *testing.T) {
	base, _ := newTestStore(t)
	store := &publishRecordingStore{Store: base}
	m := newTestMaterializer(t, store, &fakeOnchfsResolver{tree: onchfsTestTree()}, offlinecache.OnchfsMaterializerConfig{})
	require.Equal(t, 1, m.Ensure(context.Background(), []dp1playlist.PlaylistItem{{Source: onchfsSource(t, "onchfs://"+onchfsTestRoot)}}, 5*time.Second))
	assert.Empty(t, store.outsides, "every blob and the record are written inside one publication")
}

// A record whose blobs went missing is not fresh: the next Ensure rebuilds
// it instead of reporting an unplayable work ready.
func TestOnchfsMaterializerRebuildsARecordWhoseBlobsWentMissing(t *testing.T) {
	store, _ := newTestStore(t)
	resolver := &fakeOnchfsResolver{tree: onchfsTestTree()}
	m := newTestMaterializer(t, store, resolver, offlinecache.OnchfsMaterializerConfig{})
	source := onchfsSource(t, "onchfs://"+onchfsTestRoot)
	items := []dp1playlist.PlaylistItem{{Source: source}}
	require.Equal(t, 1, m.Ensure(context.Background(), items, 5*time.Second))

	rec, err := store.LoadItem(offlinecache.SourceKey(source))
	require.NoError(t, err)
	require.NoError(t, os.Remove(store.BlobPath(rec.Resources[0].SHA256)))

	require.Equal(t, 1, m.Ensure(context.Background(), items, 5*time.Second))
	assert.Equal(t, int32(2), resolver.calls.Load(), "the damaged record was rebuilt from chain")
	_, err = store.ReadBlob(rec.Resources[0].SHA256)
	assert.NoError(t, err)
}

// PR #461 review F4: a root that is a single file, not a directory.
func TestOnchfsMaterializerServesASingleFileRoot(t *testing.T) {
	store, _ := newTestStore(t)
	tree := &onchfs.Tree{Files: map[string]*onchfs.File{"": {Plain: []byte("<svg/>"), ContentType: "image/svg+xml"}}}
	source := onchfsSource(t, "onchfs://"+onchfsTestRoot+"/?fxhash=123")
	m := newTestMaterializer(t, store, &fakeOnchfsResolver{tree: tree}, offlinecache.OnchfsMaterializerConfig{})
	require.Equal(t, 1, m.Ensure(context.Background(), []dp1playlist.PlaylistItem{{Source: source}}, 5*time.Second))
	rec, err := store.LoadItem(offlinecache.SourceKey(source))
	require.NoError(t, err)
	res, ok := rec.ResourceByURL(source)
	require.True(t, ok)
	assert.Equal(t, "image/svg+xml", res.ContentType)
	body, err := store.ReadBlob(res.SHA256)
	require.NoError(t, err)
	assert.Equal(t, "<svg/>", string(body))
}

// budgetedStore refuses a blob that would take the store past limit bytes,
// the way the cache's disk budget does.
type budgetedStore struct {
	offlinecache.Store
	limit int64
}

func (s *budgetedStore) WriteBlob(r io.Reader, _ int64) (string, error) {
	used, err := s.DiskUsage()
	if err != nil {
		return "", err
	}
	return s.Store.WriteBlob(r, s.limit-used)
}

func bigTree(fill byte) *onchfs.Tree {
	body := bytes.Repeat([]byte{fill}, 80<<10)
	return &onchfs.Tree{Files: map[string]*onchfs.File{"index.html": {Plain: body, ContentType: "text/html"}}}
}

// PR #461 review F6: a full cache makes room for a new onchfs work by
// evicting older ones, oldest played first, never one the current playlist
// shows.
func TestOnchfsMaterializerReclaimsSpaceForANewWork(t *testing.T) {
	roots := []string{
		"1111111111111111111111111111111111111111111111111111111111111111",
		"2222222222222222222222222222222222222222222222222222222222222222",
		"3333333333333333333333333333333333333333333333333333333333333333",
	}
	sources := make([]string, len(roots))
	for i, r := range roots {
		sources[i] = onchfsSource(t, "onchfs://"+r)
	}
	run := func(t *testing.T, protectFirst bool, evict func(string) error) (offlinecache.Store, int) {
		base, _ := newTestStore(t)
		store := &budgetedStore{Store: base, limit: 230 << 10}
		clock := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
		var mu sync.Mutex
		now := func() time.Time { mu.Lock(); defer mu.Unlock(); clock = clock.Add(time.Minute); return clock }
		ready := 0
		for i, source := range sources {
			m := newTestMaterializer(t, store, &fakeOnchfsResolver{tree: bigTree(byte('a' + i))},
				offlinecache.OnchfsMaterializerConfig{Now: now, Evict: evict})
			items := []dp1playlist.PlaylistItem{{Source: source}}
			if protectFirst && i == 2 {
				items = append(items, dp1playlist.PlaylistItem{Source: sources[0]}, dp1playlist.PlaylistItem{Source: sources[1]})
			}
			ready = m.Ensure(context.Background(), items, 5*time.Second)
		}
		return base, ready
	}

	t.Run("the oldest work makes room", func(t *testing.T) {
		store, ready := run(t, false, nil)
		assert.Equal(t, 1, ready)
		_, err := store.LoadItem(offlinecache.SourceKey(sources[0]))
		assert.Error(t, err, "the oldest work was evicted")
		for _, s := range sources[1:] {
			_, err := store.LoadItem(offlinecache.SourceKey(s))
			assert.NoError(t, err)
		}
	})
	t.Run("works the current playlist shows are kept", func(t *testing.T) {
		store, _ := run(t, true, nil)
		for _, s := range sources[:2] {
			_, err := store.LoadItem(offlinecache.SourceKey(s))
			assert.NoError(t, err)
		}
		_, err := store.LoadItem(offlinecache.SourceKey(sources[2]))
		assert.Error(t, err, "nothing evictable: the new work does not fit")
	})
	t.Run("with a Service, eviction goes through it", func(t *testing.T) {
		var evicted []string
		var store offlinecache.Store
		evict := func(source string) error {
			evicted = append(evicted, source)
			if _, err := store.DeleteItem(offlinecache.SourceKey(source)); err != nil {
				return err
			}
			_, _, err := store.GC()
			return err
		}
		base, _ := newTestStore(t)
		store = base
		limited := &budgetedStore{Store: base, limit: 230 << 10}
		clock := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
		for i, source := range sources {
			clock = clock.Add(time.Minute)
			at := clock
			m := newTestMaterializer(t, limited, &fakeOnchfsResolver{tree: bigTree(byte('a' + i))},
				offlinecache.OnchfsMaterializerConfig{Evict: evict, Now: func() time.Time { return at }})
			require.Equal(t, 1, m.Ensure(context.Background(), []dp1playlist.PlaylistItem{{Source: source}}, 5*time.Second))
		}
		assert.Equal(t, []string{sources[0]}, evicted)
	})
}

// PR #461 review F8: standalone pruning keeps every work the current
// playlist shows, even past its budget.
func TestOnchfsMaterializerStandalonePruneKeepsThePlaylist(t *testing.T) {
	store, _ := newTestStore(t)
	clock := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); clock = clock.Add(time.Minute); return clock }
	roots := []string{
		"1111111111111111111111111111111111111111111111111111111111111111",
		"2222222222222222222222222222222222222222222222222222222222222222",
		"3333333333333333333333333333333333333333333333333333333333333333",
	}
	sources := make([]string, len(roots))
	for i, r := range roots {
		sources[i] = onchfsSource(t, "onchfs://"+r)
	}
	for i := range 2 {
		m := newTestMaterializer(t, store, &fakeOnchfsResolver{tree: bigTree(byte('a' + i))},
			offlinecache.OnchfsMaterializerConfig{Standalone: true, MaxBytes: 200 << 10, Now: now})
		require.Equal(t, 1, m.Ensure(context.Background(), []dp1playlist.PlaylistItem{{Source: sources[i]}}, 5*time.Second))
	}
	m := newTestMaterializer(t, store, &fakeOnchfsResolver{tree: bigTree('c')},
		offlinecache.OnchfsMaterializerConfig{Standalone: true, MaxBytes: 200 << 10, Now: now})
	playlist := []dp1playlist.PlaylistItem{{Source: sources[2]}, {Source: sources[0]}, {Source: sources[1]}}
	require.Equal(t, 3, m.Ensure(context.Background(), playlist, 5*time.Second))
	for _, s := range sources {
		_, err := store.LoadItem(offlinecache.SourceKey(s))
		assert.NoError(t, err, "every work of the current playlist is kept")
	}
}

// PR #461 review F9: a file is served under the spelling a browser requests
// (`!` literal), under the escaped spellings too, and a file literally
// named with a percent sequence keeps its own URL.
func TestOnchfsMaterializerServesBrowserSpelledPaths(t *testing.T) {
	file := func(body string) *onchfs.File {
		return &onchfs.File{Plain: []byte(body), ContentType: "application/javascript"}
	}
	tree := &onchfs.Tree{Files: map[string]*onchfs.File{
		"index.html":      {Plain: []byte(`<script src="lib!v1.js"></script>`), ContentType: "text/html"},
		"lib!v1.js":       file("bang"),
		"a%21b.js":        file("literal-percent"),
		"a!b.js":          file("a-bang-b"),
		"sp ace.js":       file("space"),
		"x^y.js":          file("caret"),
		"dir!/index.html": {Plain: []byte("dir"), ContentType: "text/html"},
	}}
	store, _ := newTestStore(t)
	source := onchfsSource(t, "onchfs://"+onchfsTestRoot)
	m := newTestMaterializer(t, store, &fakeOnchfsResolver{tree: tree}, offlinecache.OnchfsMaterializerConfig{})
	require.Equal(t, 1, m.Ensure(context.Background(), []dp1playlist.PlaylistItem{{Source: source}}, 5*time.Second))
	rec, err := store.LoadItem(offlinecache.SourceKey(source))
	require.NoError(t, err)
	origin := "https://cy46rsmjzxosyd46sunre2rbwglegcmfjqfi4cu3cgj2mvogoeva.onchfs.invalid"
	for url, want := range map[string]string{
		"/lib!v1.js":   "bang",
		"/lib%21v1.js": "bang",
		"/a%21b.js":    "literal-percent",
		"/a!b.js":      "a-bang-b",
		"/sp%20ace.js": "space",
		"/x^y.js":      "caret",
		"/x%5Ey.js":    "caret",
		"/dir!/":       "dir",
		"/dir%21/":     "dir",
	} {
		res, ok := rec.ResourceByURL(origin + url)
		require.True(t, ok, url)
		body, err := store.ReadBlob(res.SHA256)
		require.NoError(t, err)
		assert.Equal(t, want, string(body), url)
	}
}
