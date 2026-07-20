package offlinecache

import (
	"context"
	"encoding/json"
	"fmt"
	go_http "net/http"
	"strings"
	"sync"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// captureWindowDefault bounds how long capture observes network activity
// after navigation before finalizing the record, when the caller does not
// specify one. Software artworks typically finish their initial resource
// fetches within a few seconds; this is generous enough to tolerate a
// slow origin without hanging a download job forever — anything still in
// flight when the window elapses is simply absent from the record and
// Coverage reflects that.
const captureWindowDefault = 20 * time.Second

// Capturer drives one headless-Chromium capture of a playlist item's
// source URL and writes the result to the Store.
//
// Byte fetching is deliberately NOT done through CDP's
// Network.getResponseBody: Chromium base64-encodes response bodies over
// the DevTools websocket, which both risks the same large-body limits that
// force replay onto the static-server fallback for big assets (see
// staticserver.go) and pointlessly re-transfers bytes controld's own
// HTTP client can fetch directly and stream to disk. Instead, capture uses
// CDP Network events (requestWillBeSent/responseReceived/loadingFailed)
// only to learn WHICH URLs the page actually requested, their status, and
// redirect targets, then re-fetches each successful URL's bytes with a
// plain HTTP GET. This is correct for the static JS/CSS/HTML/image/video
// assets software artworks are built from; it is NOT correct for
// cookie-gated or per-request-dynamic resources. That trade-off is
// accepted here and documented in offline-artwork-capture.md.
//
//go:generate mockgen -source=capture.go -destination=../mocks/offlinecache_capture.go -package=mocks -mock_names=Capturer=MockOfflineCacheCapturer
type Capturer interface {
	// Capture navigates to item.Source in a fresh headless Chromium page
	// (acquired from Downloader), observes network activity for up to
	// captureWindowMs (0 uses captureWindowDefault), fetches every
	// discovered resource's bytes, and saves the resulting ItemRecord to
	// the store. It returns the saved record.
	Capture(ctx context.Context, item dp1playlist.PlaylistItem, captureWindowMs int) (*ItemRecord, error)
}

type capturer struct {
	downloader Downloader
	dialer     wrapper.WebSocketDialer
	httpClient wrapper.HTTPClient
	store      Store
	json       wrapper.JSON
	io         wrapper.IO
	clock      wrapper.Clock
	logger     *zap.Logger
}

func NewCapturer(
	downloader Downloader,
	dialer wrapper.WebSocketDialer,
	httpClient wrapper.HTTPClient,
	store Store,
	jsonWrapper wrapper.JSON,
	ioWrapper wrapper.IO,
	clockWrapper wrapper.Clock,
	logger *zap.Logger,
) Capturer {
	return &capturer{
		downloader: downloader,
		dialer:     dialer,
		httpClient: httpClient,
		store:      store,
		json:       jsonWrapper,
		io:         ioWrapper,
		clock:      clockWrapper,
		logger:     logger,
	}
}

func (c *capturer) Capture(ctx context.Context, item dp1playlist.PlaylistItem, captureWindowMs int) (*ItemRecord, error) {
	if item.ID == "" || item.Source == "" {
		return nil, fmt.Errorf("offline cache: item must have an id and a source")
	}
	window := captureWindowDefault
	if captureWindowMs > 0 {
		window = time.Duration(captureWindowMs) * time.Millisecond
	}

	endpoint, err := c.downloader.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("offline cache: acquire headless chromium: %w", err)
	}
	defer c.downloader.Release()

	session, err := DialPageSession(ctx, endpoint, c.httpClient, c.dialer, c.json, c.io, c.logger)
	if err != nil {
		return nil, fmt.Errorf("offline cache: dial capture session: %w", err)
	}
	defer func() { _ = session.Close() }()

	tracker := newCaptureTracker()
	c.attachHandlers(session, tracker)

	if _, err := session.Send(ctx, "Network.enable", map[string]interface{}{}); err != nil {
		return nil, fmt.Errorf("offline cache: Network.enable: %w", err)
	}
	if _, err := session.Send(ctx, "Page.enable", map[string]interface{}{}); err != nil {
		return nil, fmt.Errorf("offline cache: Page.enable: %w", err)
	}

	navCtx, navCancel := context.WithTimeout(ctx, window)
	defer navCancel()
	if _, err := session.Send(navCtx, "Page.navigate", map[string]interface{}{"url": item.Source}); err != nil {
		return nil, fmt.Errorf("offline cache: Page.navigate: %w", err)
	}

	// There is no single reliable "artwork fully loaded" signal across
	// arbitrary software artworks (some keep polling/streaming
	// indefinitely), so a bounded wall-clock observation window is the
	// simplest signal that is both testable and safe against a runaway
	// capture holding the single download slot forever.
	select {
	case <-navCtx.Done():
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	resources, coverage := c.resolveResources(ctx, tracker)

	rec := &ItemRecord{
		ItemID:     item.ID,
		Item:       item,
		Entry:      item.Source,
		Resources:  resources,
		Coverage:   coverage,
		CapturedAt: c.clock.Now().UTC(),
	}
	if err := c.store.SaveItem(rec); err != nil {
		return nil, fmt.Errorf("offline cache: save item %s: %w", item.ID, err)
	}
	return rec, nil
}

// resolveResources turns the tracker's observed network activity into the
// final Resource list, fetching bytes for every successful (2xx) resource
// and building the Coverage summary from anything that failed.
func (c *capturer) resolveResources(ctx context.Context, tracker *captureTracker) ([]Resource, Coverage) {
	urls, resources, failures := tracker.snapshot()

	result := make([]Resource, 0, len(urls))
	var failureReasons []string
	for _, u := range urls {
		res, ok := resources[u]
		if !ok {
			continue
		}
		if res.Status >= go_http.StatusOK && res.Status < go_http.StatusMultipleChoices && res.SHA256 == "" {
			hash, fetchErr := c.fetchAndStoreBody(ctx, u)
			if fetchErr != nil {
				failureReasons = append(failureReasons, fmt.Sprintf("fetch_failed:%s", u))
			} else {
				res.SHA256 = hash
			}
		}
		result = append(result, res)
	}

	for u, reason := range failures {
		failureReasons = append(failureReasons, fmt.Sprintf("loading_failed(%s):%s", reason, u))
	}

	coverage := Coverage{Complete: len(failureReasons) == 0}
	if len(failureReasons) > 0 {
		coverage.Reason = strings.Join(failureReasons, "; ")
	}
	return result, coverage
}

func (c *capturer) fetchAndStoreBody(ctx context.Context, url string) (string, error) {
	req, err := c.httpClient.NewRequest(go_http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient.Do(req.WithContext(ctx))
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < go_http.StatusOK || resp.StatusCode >= go_http.StatusMultipleChoices {
		return "", fmt.Errorf("unexpected status %d fetching %s", resp.StatusCode, url)
	}
	data, err := c.io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return c.store.WriteBlob(data)
}

// attachHandlers wires the Network domain events capture needs. Handlers
// run on the CDPSession's read pump goroutine (see cdpsession.go's On
// doc), so they only mutate the tracker (mutex-guarded) and never block or
// call Send.
func (c *capturer) attachHandlers(session CDPSession, tracker *captureTracker) {
	session.On("Network.requestWillBeSent", func(params json.RawMessage) {
		var evt struct {
			RequestID string `json:"requestId"`
			Request   struct {
				URL string `json:"url"`
			} `json:"request"`
			RedirectResponse *struct {
				URL    string `json:"url"`
				Status int    `json:"status"`
			} `json:"redirectResponse"`
		}
		if err := c.json.Unmarshal(params, &evt); err != nil {
			c.logger.Debug("offline cache capture: failed to parse requestWillBeSent", zap.Error(err))
			return
		}
		// CDP's redirectResponse carries the URL that redirected and its
		// status; the new URL for the same requestId is this event's
		// request.url — capturing that pairing here is the only way to
		// reconstruct a redirect chain, since CDP does not send a separate
		// "final" event naming the hop that was skipped.
		if evt.RedirectResponse != nil {
			tracker.recordResource(evt.RedirectResponse.URL, evt.RedirectResponse.Status, "", evt.Request.URL)
		}
		tracker.trackRequest(evt.RequestID, evt.Request.URL)
	})

	session.On("Network.responseReceived", func(params json.RawMessage) {
		var evt struct {
			RequestID string `json:"requestId"`
			Response  struct {
				URL      string `json:"url"`
				Status   int    `json:"status"`
				MimeType string `json:"mimeType"`
			} `json:"response"`
		}
		if err := c.json.Unmarshal(params, &evt); err != nil {
			c.logger.Debug("offline cache capture: failed to parse responseReceived", zap.Error(err))
			return
		}
		tracker.recordResource(evt.Response.URL, evt.Response.Status, evt.Response.MimeType, "")
	})

	session.On("Network.loadingFailed", func(params json.RawMessage) {
		var evt struct {
			RequestID     string `json:"requestId"`
			ErrorText     string `json:"errorText"`
			BlockedReason string `json:"blockedReason,omitempty"`
		}
		if err := c.json.Unmarshal(params, &evt); err != nil {
			c.logger.Debug("offline cache capture: failed to parse loadingFailed", zap.Error(err))
			return
		}
		url := tracker.urlForRequest(evt.RequestID)
		reason := evt.ErrorText
		// blockedReason=="csp" is the CSP-broken-online case (e.g. a CDN
		// serving a restrictive Content-Security-Policy that blocks the
		// artwork's own script): the artwork fails to load even with a
		// live network connection, which status reporting should
		// distinguish from a capture defect.
		if evt.BlockedReason == "csp" {
			reason = ReasonCSPBlocked
		}
		tracker.recordFailure(url, reason)
	})
}

// captureTracker accumulates network activity observed during one capture
// session. All access is mutex-guarded because events are delivered on the
// CDPSession's read-pump goroutine while resolveResources reads the final
// state from the caller's goroutine after the observation window closes.
type captureTracker struct {
	mu sync.Mutex

	// requestURL maps a CDP requestId to its current URL so
	// Network.loadingFailed (which only carries the id) can be attributed
	// to a URL recorded by an earlier requestWillBeSent.
	requestURL map[string]string
	resources  map[string]Resource
	order      []string // URLs in first-seen order, for deterministic output
	failures   map[string]string
}

func newCaptureTracker() *captureTracker {
	return &captureTracker{
		requestURL: make(map[string]string),
		resources:  make(map[string]Resource),
		failures:   make(map[string]string),
	}
}

// isIgnoredCaptureURL excludes page-internal URL schemes that are always
// regenerated fresh on every page load (a new random blob: UUID or an
// inline data: payload, never a stable network resource) and would
// otherwise be recorded as false misses if captured and looked up again
// on a later load.
func isIgnoredCaptureURL(url string) bool {
	return strings.HasPrefix(url, "blob:") || strings.HasPrefix(url, "data:")
}

func (t *captureTracker) recordResource(url string, status int, contentType, redirectTo string) {
	if url == "" || isIgnoredCaptureURL(url) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.resources[url]; !exists {
		t.order = append(t.order, url)
	}
	t.resources[url] = Resource{URL: url, Status: status, ContentType: contentType, RedirectTo: redirectTo}
}

func (t *captureTracker) recordFailure(url, reason string) {
	if url == "" || isIgnoredCaptureURL(url) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failures[url] = reason
}

func (t *captureTracker) trackRequest(requestID, url string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.requestURL[requestID] = url
}

func (t *captureTracker) urlForRequest(requestID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.requestURL[requestID]
}

// snapshot returns copies of the tracker's state for lock-free use after
// the observation window closes.
func (t *captureTracker) snapshot() ([]string, map[string]Resource, map[string]string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	urls := append([]string{}, t.order...)
	resources := make(map[string]Resource, len(t.resources))
	for k, v := range t.resources {
		resources[k] = v
	}
	failures := make(map[string]string, len(t.failures))
	for k, v := range t.failures {
		failures[k] = v
	}
	return urls, resources, failures
}
