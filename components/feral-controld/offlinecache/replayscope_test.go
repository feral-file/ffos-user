package offlinecache_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const replayScopeFile = "replay-scope.json"

// bootedKiosk is a controld process as it stands right after a start: a
// real replayer over the given store, and a kiosk replay whose every dial
// lands on one scripted DevTools peer. Nothing has called SyncPlaylist.
type bootedKiosk struct {
	kr   offlinecache.KioskReplay
	conn *fakeWSConn
	logs *observer.ObservedLogs
}

// bootKiosk boots with the device known to have no network, the one case
// the remembered scope is restored for.
func bootKiosk(t *testing.T, store offlinecache.Store) *bootedKiosk {
	t.Helper()
	return bootKioskWithSignal(t, store, func() bool { return true })
}

func bootKioskWithSignal(t *testing.T, store offlinecache.Store, offline func() bool) *bootedKiosk {
	t.Helper()
	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	mockStatic := mocks.NewMockOfflineCacheStaticServer(ctrl)
	mockStatic.EXPECT().BaseURL().Return(testStaticBaseURL).AnyTimes()
	mockHTTP, mockDialer, conn := scriptedKiosk(t, ctrl)

	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	replayer := offlinecache.NewReplayer(store, mockStatic, offlinecache.MissPolicyFailClosed,
		constants.WEBAPP_URL, wrapper.NewJSON(), logger)
	kr := offlinecache.NewKioskReplay(replayer, store, "http://127.0.0.1:9222",
		mockHTTP, mockDialer, wrapper.NewJSON(), wrapper.NewIO(), wrapper.NewClock(), logger)
	if offline != nil {
		setOffline(t, kr, offline)
	}
	return &bootedKiosk{kr: kr, conn: conn, logs: logs}
}

// scriptedKiosk returns the HTTP client and dialer whose every dial lands
// on the one returned DevTools peer.
func scriptedKiosk(t *testing.T, ctrl *gomock.Controller) (*mocks.MockHTTPClient, *mocks.MockWebSocketDialer, *fakeWSConn) {
	t.Helper()
	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockDialer := mocks.NewMockWebSocketDialer(ctrl)
	conn := newFakeWSConn()
	t.Cleanup(func() { _ = conn.Close() })

	mockHTTP.EXPECT().NewRequest(http.MethodGet, "http://127.0.0.1:9222/json", nil).DoAndReturn(
		func(method, url string, _ io.Reader) (*http.Request, error) { return http.NewRequest(method, url, nil) }).AnyTimes()
	mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(`[{"type":"page","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/page/1"}]`))}, nil
	}).AnyTimes()
	mockDialer.EXPECT().DialContext(gomock.Any(), "ws://127.0.0.1:9222/devtools/page/1", nil).
		Return(conn, nil, nil).AnyTimes()
	return mockHTTP, mockDialer, conn
}

func setOffline(t *testing.T, kr offlinecache.KioskReplay, offline func() bool) {
	t.Helper()
	sink, ok := kr.(interface{ SetOffline(func() bool) })
	require.True(t, ok, "kiosk replay takes the no-network signal")
	sink.SetOffline(offline)
}

// attach runs AttachOnReconnect against the scripted peer and returns the
// methods it sent, in order.
func (b *bootedKiosk) attach(t *testing.T, wantCalls int) []string {
	t.Helper()
	errCh := make(chan error, 1)
	go func() { errCh <- b.kr.AttachOnReconnect(context.Background()) }()
	methods := make([]string, 0, wantCalls)
	for i := 0; i < wantCalls; i++ {
		methods = append(methods, ackOutbound(t, b.conn)["method"].(string))
	}
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("AttachOnReconnect did not complete")
	}
	return methods
}

// request delivers a paused kiosk request and returns replay's answer.
func (b *bootedKiosk) request(t *testing.T, requestID, url string) map[string]interface{} {
	t.Helper()
	evt, err := json.Marshal(map[string]interface{}{
		"method": "Fetch.requestPaused",
		"params": json.RawMessage(requestPausedEvent(t, requestID, url)),
	})
	require.NoError(t, err)
	b.conn.pushReply(evt)
	return ackOutbound(t, b.conn)
}

// rememberScope plays the previous run: a SyncPlaylist that succeeded for
// sources, which is what leaves the scope file behind.
func rememberScope(t *testing.T, store offlinecache.Store, sources ...string) {
	t.Helper()
	require.NoError(t, syncErr(syncingKiosk(t, store).SyncPlaylist(context.Background(), sources)))
}

// syncingKiosk is a kiosk replay whose every scope change succeeds.
func syncingKiosk(t *testing.T, store offlinecache.Store) offlinecache.KioskReplay {
	t.Helper()
	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)
	mockReplayer := mocks.NewMockOfflineCacheReplayer(ctrl)
	mockReplayer.EXPECT().EnableForPlaylist(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockReplayer.EXPECT().Disable(gomock.Any()).Return(nil).AnyTimes()
	return offlinecache.NewKioskReplay(mockReplayer, store, "http://127.0.0.1:9222",
		mocks.NewMockHTTPClient(ctrl), mocks.NewMockWebSocketDialer(ctrl),
		wrapper.NewJSON(), wrapper.NewIO(), wrapper.NewClock(), zap.NewNop())
}

// TestKioskReplay_AttachOnReconnect_RestoresRememberedScopeBeforeAnySync is
// the regression test for feral-file/feral-file#3635: an FF1 rebooted with
// no network showed the player's bundled fallback first, because replay had
// no scope until a refresher pass had asked the hydrated player what it was
// showing. The kept work must be served to a request that arrives right
// after the attach, with no displayPlaylist and no refresher pass.
func TestKioskReplay_AttachOnReconnect_RestoresRememberedScopeBeforeAnySync(t *testing.T) {
	store, _ := newTestStore(t)
	kept := seedItem(t, store, "kept", "<html>kept work</html>")
	rememberScope(t, store, kept.URL)

	b := bootKiosk(t, store)
	methods := b.attach(t, 2)
	assert.Equal(t, []string{"Fetch.enable", "Target.setAutoAttach"}, methods,
		"interception is armed as soon as the session exists, ahead of child auto-attach")

	// The player's first load of the kept work: answered from the cache.
	answer := b.request(t, "req-1", kept.URL)
	require.Equal(t, "Fetch.fulfillRequest", answer["method"], "the kept copy answers the first request")
	params := answer["params"].(map[string]interface{})
	body, err := base64.StdEncoding.DecodeString(params["body"].(string))
	require.NoError(t, err)
	assert.Equal(t, "<html>kept work</html>", string(body))
}

// TestKioskReplay_AttachOnReconnect_RestoredScopePassesMissesThrough pins
// the network-present half: nothing has confirmed what the reloaded page
// shows, so under fail_closed a request the restored scope does not hold
// must still reach the network, not be failed.
func TestKioskReplay_AttachOnReconnect_RestoredScopePassesMissesThrough(t *testing.T) {
	store, _ := newTestStore(t)
	kept := seedItem(t, store, "kept", "<html>kept work</html>")
	rememberScope(t, store, kept.URL)

	b := bootKiosk(t, store)
	b.attach(t, 2)

	answer := b.request(t, "req-1", "https://other.example/live.html")
	assert.Equal(t, "Fetch.continueRequest", answer["method"],
		"a miss on a restored scope passes through even under fail_closed")
}

// TestKioskReplay_AttachOnReconnect_NothingRememberedLeavesInterceptionOff
// pins that a device with no remembered scope attaches exactly as before:
// Fetch is never enabled, so every request goes to the network untouched.
func TestKioskReplay_AttachOnReconnect_NothingRememberedLeavesInterceptionOff(t *testing.T) {
	store, _ := newTestStore(t)
	seedItem(t, store, "cached-but-never-shown", "<html>")

	b := bootKiosk(t, store)
	assert.Equal(t, []string{"Target.setAutoAttach"}, b.attach(t, 1))
}

// TestKioskReplay_AttachOnReconnect_ClearedItemIsNotRestored covers a clear
// whose scope resync never ran (the clear landed and the device lost power):
// the remembered file still names the item, the record is gone, and the
// item must not come back.
func TestKioskReplay_AttachOnReconnect_ClearedItemIsNotRestored(t *testing.T) {
	store, root := newTestStore(t)
	kept := seedItem(t, store, "kept", "<html>kept</html>")
	cleared := seedItem(t, store, "cleared", "<html>cleared</html>")
	rememberScope(t, store, kept.URL, cleared.URL)

	deleted, err := store.DeleteItem(offlinecache.SourceKey(cleared.URL))
	require.NoError(t, err)
	require.True(t, deleted)
	require.FileExists(t, filepath.Join(root, replayScopeFile))

	b := bootKiosk(t, store)
	b.attach(t, 2)
	assert.Equal(t, "Fetch.continueRequest", b.request(t, "req-1", cleared.URL)["method"],
		"a cleared item is never served from the cache after the clear")
	assert.Equal(t, "Fetch.fulfillRequest", b.request(t, "req-2", kept.URL)["method"])
}

// TestKioskReplay_AttachOnReconnect_EverythingClearedRestoresNothing: when
// every remembered item has been cleared, the attach arms nothing at all.
func TestKioskReplay_AttachOnReconnect_EverythingClearedRestoresNothing(t *testing.T) {
	store, _ := newTestStore(t)
	cleared := seedItem(t, store, "cleared", "<html>cleared</html>")
	rememberScope(t, store, cleared.URL)
	_, err := store.DeleteItem(offlinecache.SourceKey(cleared.URL))
	require.NoError(t, err)

	b := bootKiosk(t, store)
	assert.Equal(t, []string{"Target.setAutoAttach"}, b.attach(t, 1))
}

// TestKioskReplay_SyncPlaylist_EmptyScopeForgetsRememberedScope: a sync that
// ends with nothing cached in scope (the playlist was cleared, or the kiosk
// moved to uncached work) removes the file, so the old scope is not
// restored at the next start.
func TestKioskReplay_SyncPlaylist_EmptyScopeForgetsRememberedScope(t *testing.T) {
	store, root := newTestStore(t)
	kept := seedItem(t, store, "kept", "<html>kept</html>")
	rememberScope(t, store, kept.URL)
	require.FileExists(t, filepath.Join(root, replayScopeFile))

	rememberScope(t, store, sourceFor("not-cached"))
	assert.NoFileExists(t, filepath.Join(root, replayScopeFile))

	b := bootKiosk(t, store)
	assert.Equal(t, []string{"Target.setAutoAttach"}, b.attach(t, 1))
}

// TestKioskReplay_AttachOnReconnect_CorruptRememberedScopeIsIgnored: a
// damaged file, or one naming things that are not source keys, restores
// nothing and never fails the attach.
func TestKioskReplay_AttachOnReconnect_CorruptRememberedScopeIsIgnored(t *testing.T) {
	for name, content := range map[string]string{
		"truncated json": `{"sourceKeys":["`,
		"not json":       "\x00\x01garbage",
		"wrong shape":    `{"sourceKeys":"abc"}`,
		"not keys":       `{"sourceKeys":["../../etc/passwd","https://example.com/a?sig=secret",""]}`,
		"empty":          ``,
	} {
		t.Run(name, func(t *testing.T) {
			store, root := newTestStore(t)
			seedItem(t, store, "kept", "<html>kept</html>")
			require.NoError(t, os.WriteFile(filepath.Join(root, replayScopeFile), []byte(content), 0o600))

			b := bootKiosk(t, store)
			assert.Equal(t, []string{"Target.setAutoAttach"}, b.attach(t, 1))
		})
	}
}

// TestKioskReplay_RememberedScopeKeepsSourceURLsOutOfFileAndLogs: sources
// can be signed URLs. The remembered file holds keys only, and neither the
// restore nor a failed restore writes a query string to the log.
func TestKioskReplay_RememberedScopeKeepsSourceURLsOutOfFileAndLogs(t *testing.T) {
	store, root := newTestStore(t)
	signed := "https://cdn.example.com/work/index.html?X-Amz-Signature=topsecret"
	seedSniffedItem(t, store, "signed", signed, "text/html", "<html>signed</html>")
	rememberScope(t, store, signed)

	data, err := os.ReadFile(filepath.Join(root, replayScopeFile)) //nolint:gosec // path is inside this test's t.TempDir.
	require.NoError(t, err)
	assert.NotContains(t, string(data), "topsecret")
	assert.NotContains(t, string(data), "cdn.example.com")
	assert.Contains(t, string(data), offlinecache.SourceKey(signed))

	b := bootKiosk(t, store)
	b.attach(t, 2)
	require.Equal(t, 1, b.logs.FilterMessageSnippet("restored remembered replay scope").Len())
	for _, entry := range b.logs.All() {
		assert.NotContains(t, entry.Message, "topsecret")
		for _, field := range entry.Context {
			assert.NotContains(t, field.String, "topsecret")
			if field.Interface != nil {
				if e, ok := field.Interface.(error); ok {
					assert.NotContains(t, e.Error(), "topsecret")
				}
			}
		}
	}
}

// TestReplayer_EnableForPlaylist_LoadErrorRedactsTheSource: the one error on
// this path that names a source must not carry its query string, because
// every SyncPlaylist caller logs it.
func TestReplayer_EnableForPlaylist_LoadErrorRedactsTheSource(t *testing.T) {
	ts := setupReplay(t, offlinecache.MissPolicyFailClosed)
	defer ts.ctrl.Finish()
	err := ts.replayer.EnableForPlaylist(context.Background(),
		[]string{"https://cdn.example.com/missing.html?X-Amz-Signature=topsecret"}, false)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "topsecret")
	assert.Contains(t, err.Error(), "cdn.example.com/missing.html")
}

// TestKioskReplay_AttachOnReconnect_GivesWayToADisplayInFlight: a display
// path holding the playback lock is mid sync+send and knows what is on
// screen; the restore must neither wait for it nor arm over it.
func TestKioskReplay_AttachOnReconnect_GivesWayToADisplayInFlight(t *testing.T) {
	store, _ := newTestStore(t)
	kept := seedItem(t, store, "kept", "<html>kept</html>")
	rememberScope(t, store, kept.URL)

	b := bootKiosk(t, store)
	b.kr.LockPlayback()
	defer b.kr.UnlockPlayback()
	assert.Equal(t, []string{"Target.setAutoAttach"}, b.attach(t, 1))
}

// TestKioskReplay_AttachOnReconnect_KeepsASyncThatFinishedBeforeRestore: a
// display path can sync the freshly attached session and release the
// playback lock before the restore gets to it. That scope is authoritative
// (strict here: everything in it is cached), and the restore must not
// reinstall it as a mixed one, which would pass a miss through under
// fail_closed.
func TestKioskReplay_AttachOnReconnect_KeepsASyncThatFinishedBeforeRestore(t *testing.T) {
	store, _ := newTestStore(t)
	kept := seedItem(t, store, "kept", "<html>kept</html>")
	rememberScope(t, store, kept.URL)

	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)
	mockHTTP, mockDialer, conn := scriptedKiosk(t, ctrl)
	mockReplayer := mocks.NewMockOfflineCacheReplayer(ctrl)
	kr := offlinecache.NewKioskReplay(mockReplayer, store, "http://127.0.0.1:9222",
		mockHTTP, mockDialer, wrapper.NewJSON(), wrapper.NewIO(), wrapper.NewClock(), zap.NewNop())
	setOffline(t, kr, func() bool { return true })

	// The sync lands inside Attach: after the session is the replayer's,
	// before the restore runs. The only scope ever installed is its strict
	// one; a mixed EnableForPlaylist is an unexpected call and fails here.
	mockReplayer.EXPECT().EnableForPlaylist(gomock.Any(), []string{kept.URL}, false).Return(nil).Times(1)
	mockReplayer.EXPECT().Attach("", gomock.Any()).Do(func(string, offlinecache.CDPSession) {
		kr.LockPlayback()
		defer kr.UnlockPlayback()
		assert.NoError(t, syncErr(kr.SyncPlaylist(context.Background(), []string{kept.URL})))
	})

	b := &bootedKiosk{kr: kr, conn: conn}
	assert.Equal(t, []string{"Target.setAutoAttach"}, b.attach(t, 1))
}

// TestKioskReplay_SyncPlaylist_RetriesRememberingAfterAFailedWrite: a write
// can replace the file and still fail (the rename lands, the directory sync
// does not). The file then holds the new scope while the last one recorded
// as saved is the old one, so a sync back to the old scope must write again
// rather than leave the other scope on disk for the next start.
func TestKioskReplay_SyncPlaylist_RetriesRememberingAfterAFailedWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	store, root := newTestStore(t)
	a := seedItem(t, store, "a", "<html>a</html>")
	b := seedItem(t, store, "b", "<html>b</html>")
	kr := syncingKiosk(t, store)
	scopeOnDisk := func() string {
		data, err := os.ReadFile(filepath.Join(root, replayScopeFile)) //nolint:gosec // path is inside this test's t.TempDir.
		require.NoError(t, err)
		return string(data)
	}

	require.NoError(t, syncErr(kr.SyncPlaylist(context.Background(), []string{a.URL})))
	require.Contains(t, scopeOnDisk(), offlinecache.SourceKey(a.URL))

	// Write and search but no read: the rename succeeds, opening the
	// directory to sync it does not.
	require.NoError(t, os.Chmod(root, 0o300))       //nolint:gosec // G302: a test directory, made unreadable on purpose.
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) }) //nolint:gosec // G302: restores the test directory for cleanup.
	require.NoError(t, syncErr(kr.SyncPlaylist(context.Background(), []string{b.URL})))
	require.NoError(t, os.Chmod(root, 0o700)) //nolint:gosec // G302: restores the test directory.
	require.Contains(t, scopeOnDisk(), offlinecache.SourceKey(b.URL), "the failed write did replace the file")

	require.NoError(t, syncErr(kr.SyncPlaylist(context.Background(), []string{a.URL})))
	assert.Contains(t, scopeOnDisk(), offlinecache.SourceKey(a.URL), "the next sync writes its scope again")
}

// TestBootstrapCore_LazyKioskReplayRestoresRememberedScopeAtStart: with the
// offline cache feature off, replay dials the kiosk only for an onchfs
// scope. A scope remembered from the last run counts, so such a work is
// also served after a start with no network.
func TestBootstrapCore_LazyKioskReplayRestoresRememberedScopeAtStart(t *testing.T) {
	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)
	root := t.TempDir()
	newCore := func(dials *int) offlinecache.Core {
		mockHTTP := mocks.NewMockHTTPClient(ctrl)
		mockDialer := mocks.NewMockWebSocketDialer(ctrl)
		mockHTTP.EXPECT().NewRequest(http.MethodGet, "http://127.0.0.1:9222/json", nil).DoAndReturn(
			func(method, url string, _ io.Reader) (*http.Request, error) { return http.NewRequest(method, url, nil) }).AnyTimes()
		mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK,
				Body: io.NopCloser(strings.NewReader(`[{"type":"page","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/page/1"}]`))}, nil
		}).AnyTimes()
		mockDialer.EXPECT().DialContext(gomock.Any(), "ws://127.0.0.1:9222/devtools/page/1", nil).DoAndReturn(
			func(context.Context, string, http.Header) (wrapper.WebSocketConn, *http.Response, error) {
				*dials++
				conn := newFakeWSConn()
				go conn.drainAndAckRemaining(t)
				t.Cleanup(func() { _ = conn.Close() })
				return conn, nil, nil
			}).AnyTimes()
		opts := offlinecache.OptionsFromConfig(nil, "http://127.0.0.1:9222")
		opts.RootDir = root
		opts.MaxDiskPercent = 0 // the disk budget reads this machine's free space; not under test
		core, _ := offlinecache.BootstrapCore(opts, mockHTTP, mockDialer, wrapper.NewOS(), wrapper.NewIO(),
			wrapper.NewJSON(), wrapper.NewClock(), zap.NewNop(), true)
		return core
	}

	firstDials := 0
	first := newCore(&firstDials)
	cached := seedItem(t, first.Store, "onchfs-like", "<html>")
	n, err := first.KioskReplay.SyncPlaylist(context.Background(), []string{cached.URL})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// The next start: nothing armed in this process yet.
	secondDials := 0
	second := newCore(&secondDials)
	setOffline(t, second.KioskReplay, func() bool { return true })
	require.NoError(t, second.KioskReplay.AttachOnReconnect(context.Background()))
	assert.Equal(t, 1, secondDials, "a remembered scope makes the lazy replay attach at start")
}

// TestKioskReplay_AttachOnReconnect_NetworkPresentRestoresNothing is the
// shared-URL sequence: playlist A is remembered with an old capture of a
// URL that uncached playlist B also requests. With network present the
// attach must arm nothing, so B's request reaches the live origin exactly
// as it did before the remembered scope existed; a restored scope would
// have answered it with A's old bytes.
func TestKioskReplay_AttachOnReconnect_NetworkPresentRestoresNothing(t *testing.T) {
	for name, signal := range map[string]func() bool{
		"online":          func() bool { return false },
		"no signal wired": nil,
	} {
		t.Run(name, func(t *testing.T) {
			store, _ := newTestStore(t)
			shared := seedItem(t, store, "playlist-a-shared-script", "old bytes")
			rememberScope(t, store, shared.URL)

			b := bootKioskWithSignal(t, store, signal)
			assert.Equal(t, []string{"Target.setAutoAttach"}, b.attach(t, 1),
				"Fetch is never enabled, so no request can be answered from the remembered scope")
		})
	}
}
