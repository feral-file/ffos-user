package offlinecache

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/mediahealth"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// navFakeSession is a CDPSession that records Sends and delivers events to
// the handlers the observer registered.
type navFakeSession struct {
	mu       sync.Mutex
	sent     []string
	handlers map[string][]func(json.RawMessage)
	closed   bool
	sendErr  error
}

func newNavFakeSession() *navFakeSession {
	return &navFakeSession{handlers: map[string][]func(json.RawMessage){}}
}

func (f *navFakeSession) Send(_ context.Context, method string, _ map[string]interface{}) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, method)
	return json.RawMessage(`{}`), f.sendErr
}

func (f *navFakeSession) On(method string, handler func(json.RawMessage)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method] = append(f.handlers[method], handler)
}

func (f *navFakeSession) ForSession(string) CDPSession { return f }

func (f *navFakeSession) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *navFakeSession) emit(t *testing.T, method string, params map[string]interface{}) {
	t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(t, err)
	f.mu.Lock()
	hs := append([]func(json.RawMessage){}, f.handlers[method]...)
	f.mu.Unlock()
	for _, h := range hs {
		h(raw)
	}
}

func (f *navFakeSession) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.sent...)
}

type navSink struct{ ch chan mediahealth.Navigation }

func (s *navSink) ObserveNavigation(nav mediahealth.Navigation) { s.ch <- nav }

func (s *navSink) next(t *testing.T) mediahealth.Navigation {
	t.Helper()
	select {
	case nav := <-s.ch:
		return nav
	case <-time.After(2 * time.Second):
		t.Fatal("no observation delivered")
		return mediahealth.Navigation{}
	}
}

func (s *navSink) none(t *testing.T) {
	t.Helper()
	select {
	case nav := <-s.ch:
		t.Fatalf("unexpected observation %+v", nav)
	case <-time.After(50 * time.Millisecond):
	}
}

func newTestObserver(t *testing.T, sessions ...*navFakeSession) (*KioskNavigationObserver, *navSink) {
	t.Helper()
	sink := &navSink{ch: make(chan mediahealth.Navigation, 16)}
	o := NewKioskNavigationObserver("http://127.0.0.1:9222", nil, nil, wrapper.NewJSON(), nil, sink, zap.NewNop())
	i := 0
	o.dial = func(context.Context, string, wrapper.HTTPClient, wrapper.WebSocketDialer, wrapper.JSON, wrapper.IO, *zap.Logger) (CDPSession, error) {
		if i >= len(sessions) {
			return nil, errors.New("no more sessions")
		}
		s := sessions[i]
		i++
		return s, nil
	}
	t.Cleanup(func() { _ = o.Close() })
	return o, sink
}

func docRequest(id, url string) map[string]interface{} {
	return map[string]interface{}{"requestId": id, "type": "Document", "request": map[string]interface{}{"url": url}}
}

func TestKioskNavigationObserver_ReportsDocumentStatuses(t *testing.T) {
	s := newNavFakeSession()
	o, sink := newTestObserver(t, s)
	require.NoError(t, o.AttachOnReconnect(context.Background()))
	assert.Equal(t, []string{"Network.enable"}, s.methods(), "passive: the Network domain only, nothing that pauses")

	// A frame's document, with the player's query appended, answered 403.
	s.emit(t, "Network.requestWillBeSent", docRequest("r1", "https://o/w?display_mode=crop"))
	s.emit(t, "Network.responseReceived", map[string]interface{}{"requestId": "r1", "type": "Document", "response": map[string]interface{}{"status": 403}})
	nav := sink.next(t)
	assert.Equal(t, mediahealth.Navigation{URL: "https://o/w?display_mode=crop", Status: 403}, nav)

	// A redirect re-sends the same requestId: the ORIGINAL URL is kept.
	s.emit(t, "Network.requestWillBeSent", docRequest("r2", "https://arweave.net/tx"))
	s.emit(t, "Network.requestWillBeSent", docRequest("r2", "https://abc.arweave.net/tx"))
	s.emit(t, "Network.responseReceived", map[string]interface{}{"requestId": "r2", "type": "Document", "response": map[string]interface{}{"status": 404}})
	assert.Equal(t, mediahealth.Navigation{URL: "https://arweave.net/tx", Status: 404}, sink.next(t))

	// A failure, canceled or not, is reported as such.
	s.emit(t, "Network.requestWillBeSent", docRequest("r3", "https://dead.example/w"))
	s.emit(t, "Network.loadingFailed", map[string]interface{}{"requestId": "r3", "type": "Document", "errorText": "net::ERR_NAME_NOT_RESOLVED", "canceled": false})
	assert.Equal(t, mediahealth.Navigation{URL: "https://dead.example/w", Error: "net::ERR_NAME_NOT_RESOLVED"}, sink.next(t))
	s.emit(t, "Network.requestWillBeSent", docRequest("r4", "https://o/w"))
	s.emit(t, "Network.loadingFailed", map[string]interface{}{"requestId": "r4", "type": "Document", "errorText": "net::ERR_ABORTED", "canceled": true})
	assert.True(t, sink.next(t).Canceled)

	// Subresources are not the frame's document.
	s.emit(t, "Network.requestWillBeSent", map[string]interface{}{"requestId": "r5", "type": "Image", "request": map[string]interface{}{"url": "https://o/i.png"}})
	s.emit(t, "Network.responseReceived", map[string]interface{}{"requestId": "r5", "type": "Image", "response": map[string]interface{}{"status": 404}})
	// A response for a request never seen as a Document is unknown.
	s.emit(t, "Network.responseReceived", map[string]interface{}{"requestId": "r9", "type": "Document", "response": map[string]interface{}{"status": 404}})
	sink.none(t)
}

func TestKioskNavigationObserver_ReconnectSupersedesTheSession(t *testing.T) {
	s1, s2 := newNavFakeSession(), newNavFakeSession()
	o, sink := newTestObserver(t, s1, s2)
	require.NoError(t, o.AttachOnReconnect(context.Background()))
	s1.emit(t, "Network.requestWillBeSent", docRequest("r1", "https://o/w"))
	require.NoError(t, o.AttachOnReconnect(context.Background()))
	assert.True(t, s1.closed, "the previous session is closed")
	// An event from the superseded session is not attributed to the live one.
	s1.emit(t, "Network.responseReceived", map[string]interface{}{"requestId": "r1", "type": "Document", "response": map[string]interface{}{"status": 404}})
	sink.none(t)
	s2.emit(t, "Network.requestWillBeSent", docRequest("r1", "https://o/w2"))
	s2.emit(t, "Network.responseReceived", map[string]interface{}{"requestId": "r1", "type": "Document", "response": map[string]interface{}{"status": 410}})
	assert.Equal(t, 410, sink.next(t).Status)

	require.NoError(t, o.Close())
	assert.True(t, s2.closed)
	assert.Error(t, o.AttachOnReconnect(context.Background()), "closed: never dials again")
}

func TestKioskNavigationObserver_NetworkEnableFailureClosesTheSession(t *testing.T) {
	s := newNavFakeSession()
	s.sendErr = errors.New("boom")
	o, _ := newTestObserver(t, s)
	assert.Error(t, o.AttachOnReconnect(context.Background()))
	assert.True(t, s.closed, "an unarmed session is not left open")
}

func TestKioskNavigationObserver_DialFailureIsReported(t *testing.T) {
	o, _ := newTestObserver(t) // no sessions to hand out
	assert.Error(t, o.AttachOnReconnect(context.Background()))
}
