package offlinecache

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	go_http "net/http"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// largeAssetThreshold is the point at which replay redirects the kiosk to
// the static server instead of fulfilling inline via CDP
// Fetch.fulfillRequest. Fulfilling requires base64-encoding the body into
// a single JSON string over the DevTools websocket, which hits a V8
// string-length ceiling (~0x1fffffe8 characters, roughly 400-536MB
// depending on encoding overhead) on Chromium's side; this stays
// comfortably under that so the CDP path never gets close to it.
const largeAssetThreshold = 200 * 1024 * 1024 // 200MB

// MissPolicy controls what replay does when a request has no matching
// captured resource for the currently enabled item.
type MissPolicy string

const (
	// MissPolicyFailClosed fails the request as a network error. This is
	// the recommended default: it guarantees deterministic offline
	// behavior and surfaces a partial capture honestly instead of quietly
	// depending on connectivity that offline mode is supposed to remove.
	MissPolicyFailClosed MissPolicy = "fail_closed"
	// MissPolicyPassThrough lets the request continue to the real network.
	// Only sensible when the device is known to be online (progressive
	// capture); exposed as a config toggle rather than hardcoded.
	MissPolicyPassThrough MissPolicy = "pass_through"
)

// Replayer serves a cached item's captured resources back to the kiosk
// Chromium via CDP Fetch domain interception, keyed on the exact URL the
// item's DP-1 source pointed at — the source itself is never rewritten
// (see ItemRecord.Item's doc), which preserves the signed-playlist
// invariant end to end.
//
//go:generate mockgen -source=replay.go -destination=../mocks/offlinecache_replay.go -package=mocks -mock_names=Replayer=MockOfflineCacheReplayer
type Replayer interface {
	// Attach registers the Fetch.requestPaused handler on session (the
	// kiosk's event-driven CDP session). It does not enable the Fetch
	// domain by itself — EnableForItem does that — so call Attach once per
	// CDP connection (including after a reconnect; see cdp.CDP's
	// onConnect hook) and then scope interception on/off per displayed
	// item with EnableForItem/Disable.
	Attach(session CDPSession)
	// EnableForItem loads itemID's captured record and enables Fetch
	// interception scoped to it. Call before displaying a single cached
	// item. Equivalent to EnableForPlaylist(ctx, []string{itemID}, false):
	// a lone item is by definition not a "mixed" scope.
	EnableForItem(ctx context.Context, itemID string) error
	// EnableForPlaylist loads every itemID's captured record and enables
	// Fetch interception scoped to the union of their captured
	// resources. A DP-1 playlist plays multiple items in sequence inside
	// the same kiosk page/CDP target without Go being told exactly which
	// one is on screen at any instant (feral-player advances the
	// playlist client-side), so replay must recognize any cached item's
	// URLs for as long as that playlist is showing rather than trying to
	// track a single "current" item from the daemon side.
	//
	// mixed must be true whenever itemIDs is not the complete set of
	// items the displayed playlist actually contains (i.e. the caller
	// filtered out some uncached items before calling this — see
	// KioskReplay.SyncPlaylist). It relaxes the miss policy to
	// pass-through for the duration of this scope: with Fetch.enable
	// patterned on "*", any request Chromium makes while ANY item from
	// the playlist is on screen goes through this handler, including
	// requests that belong to a sibling item this call was never told
	// about because it isn't cached. Failing those as fail_closed would
	// misreport a live, reachable resource as offline-missing just
	// because it happens to share a CDP target with a cached item.
	// mixed=false (a single item, or a playlist where every item is
	// cached) keeps the configured missPolicy's strict guarantee, since
	// every resource the page can legitimately need is then known.
	EnableForPlaylist(ctx context.Context, itemIDs []string, mixed bool) error
	// Disable turns interception off; all requests pass through
	// untouched. Call when moving to an uncached/online item.
	Disable(ctx context.Context) error
}

type replayer struct {
	store        Store
	staticServer StaticServer
	missPolicy   MissPolicy
	json         wrapper.JSON
	logger       *zap.Logger

	mu      sync.RWMutex
	session CDPSession
	// resources is the flat url->resource lookup for every item currently
	// in scope, i.e. the union of each enabled item's Resources. A flat
	// map (rather than per-item scoping) is what lets EnableForPlaylist
	// serve a multi-item playlist correctly: nil when disabled.
	resources map[string]Resource
	// mixedScope mirrors EnableForPlaylist's mixed parameter for the
	// currently-enabled scope; read alongside resources under the same
	// lock so a miss decision always sees a consistent (resources, mixed)
	// pair. See EnableForPlaylist's doc for why this exists.
	mixedScope bool
}

// NewReplayer constructs a Replayer. staticServer's BaseURL is used to let
// the static-asset follow-up request (from a large-asset 302) pass
// through untouched instead of being re-intercepted.
func NewReplayer(store Store, staticServer StaticServer, missPolicy MissPolicy, jsonWrapper wrapper.JSON, logger *zap.Logger) Replayer {
	if missPolicy == "" {
		missPolicy = MissPolicyFailClosed
	}
	return &replayer{store: store, staticServer: staticServer, missPolicy: missPolicy, json: jsonWrapper, logger: logger}
}

func (r *replayer) Attach(session CDPSession) {
	r.mu.Lock()
	previous := r.session
	r.session = session
	r.mu.Unlock()
	session.On("Fetch.requestPaused", r.onRequestPaused)

	// A reconnect (kiosk restart, including OOM recovery) calls Attach
	// again with a freshly-dialed session. The old session's own
	// connection is normally already dead by then, but closing it here
	// deterministically stops its read-pump goroutine and releases its
	// socket instead of relying on that connection's own read error to
	// eventually notice — avoiding a goroutine/fd leak across repeated
	// reconnects.
	if previous != nil && previous != session {
		if err := previous.Close(); err != nil {
			r.logger.Debug("offline cache replay: closing superseded CDP session", zap.Error(err))
		}
	}
}

func (r *replayer) EnableForItem(ctx context.Context, itemID string) error {
	return r.EnableForPlaylist(ctx, []string{itemID}, false)
}

func (r *replayer) EnableForPlaylist(ctx context.Context, itemIDs []string, mixed bool) error {
	resources := make(map[string]Resource)
	for _, itemID := range itemIDs {
		rec, err := r.store.LoadItem(itemID)
		if err != nil {
			return fmt.Errorf("offline cache replay: load item %s: %w", itemID, err)
		}
		for _, res := range rec.Resources {
			resources[res.URL] = res
		}
	}

	r.mu.Lock()
	session := r.session
	r.resources = resources
	r.mixedScope = mixed
	r.mu.Unlock()

	if session == nil {
		return fmt.Errorf("offline cache replay: no CDP session attached")
	}
	if _, err := session.Send(ctx, "Fetch.enable", map[string]interface{}{
		"patterns": []map[string]interface{}{{"urlPattern": "*"}},
	}); err != nil {
		return fmt.Errorf("offline cache replay: Fetch.enable: %w", err)
	}
	return nil
}

func (r *replayer) Disable(ctx context.Context) error {
	r.mu.Lock()
	session := r.session
	r.resources = nil
	r.mixedScope = false
	r.mu.Unlock()

	if session == nil {
		return nil
	}
	if _, err := session.Send(ctx, "Fetch.disable", map[string]interface{}{}); err != nil {
		return fmt.Errorf("offline cache replay: Fetch.disable: %w", err)
	}
	return nil
}

// onRequestPaused is registered as a CDPSession event handler, which runs
// on the session's read-pump goroutine. It must not call session.Send
// synchronously (see cdpsession.go's On doc: the pump could not then
// deliver that Send's own reply, deadlocking against itself), so the
// actual response decision — which itself calls Send — is handed off to a
// new goroutine per paused request.
func (r *replayer) onRequestPaused(params json.RawMessage) {
	go r.processRequestPaused(params)
}

func (r *replayer) processRequestPaused(params json.RawMessage) {
	var evt struct {
		RequestID string `json:"requestId"`
		Request   struct {
			URL string `json:"url"`
		} `json:"request"`
	}
	if err := r.json.Unmarshal(params, &evt); err != nil {
		r.logger.Warn("offline cache replay: failed to parse Fetch.requestPaused", zap.Error(err))
		return
	}

	r.mu.RLock()
	session := r.session
	resources := r.resources
	mixed := r.mixedScope
	r.mu.RUnlock()
	if session == nil {
		return
	}

	ctx := context.Background() // decoupled from any caller; this runs off the CDP event pump, not a request context

	// The static server's own URL is replay's own follow-up (a large-asset
	// 302 target): it must always pass through, or intercepting it too
	// (Fetch.enable's pattern is "*") would loop back into this handler.
	if r.staticServer != nil && strings.HasPrefix(evt.Request.URL, r.staticServer.BaseURL()) {
		r.continueRequest(ctx, session, evt.RequestID)
		return
	}

	// Looking up a key on a nil map is safe in Go (returns zero value,
	// ok=false), so no explicit nil-check is needed here when disabled.
	resource, found := resources[evt.Request.URL]
	if !found {
		r.handleMiss(ctx, session, evt.RequestID, mixed)
		return
	}

	switch {
	case resource.IsRedirect():
		r.fulfill(ctx, session, evt.RequestID, statusOrDefault(resource.Status, go_http.StatusFound), "", nil, resource.RedirectTo)
	case resource.SHA256 != "":
		r.fulfillFromBlob(ctx, session, evt.RequestID, resource, mixed)
	default:
		// Captured but with no body (e.g. its fetch failed at capture
		// time) — nothing to serve, so treat exactly like a miss.
		r.handleMiss(ctx, session, evt.RequestID, mixed)
	}
}

func statusOrDefault(status, fallback int) int {
	if status == 0 {
		return fallback
	}
	return status
}

func (r *replayer) fulfillFromBlob(ctx context.Context, session CDPSession, requestID string, resource Resource, mixed bool) {
	size, err := r.store.BlobSize(resource.SHA256)
	if err != nil {
		r.logger.Warn("offline cache replay: blob size lookup failed, treating as miss",
			zap.String("url", resource.URL), zap.Error(err))
		r.handleMiss(ctx, session, requestID, mixed)
		return
	}

	if size > largeAssetThreshold {
		location := r.staticServer.URLFor(resource.SHA256, resource.ContentType)
		r.fulfill(ctx, session, requestID, go_http.StatusFound, "", nil, location)
		return
	}

	data, err := r.store.ReadBlob(resource.SHA256)
	if err != nil {
		r.logger.Warn("offline cache replay: blob read failed, treating as miss",
			zap.String("url", resource.URL), zap.Error(err))
		r.handleMiss(ctx, session, requestID, mixed)
		return
	}
	r.fulfill(ctx, session, requestID, inlineFulfillStatus(resource.Status), resource.ContentType, data, "")
}

// inlineFulfillStatus normalizes a captured resource's status for the
// inline-blob fulfill path (used for anything under largeAssetThreshold).
// Capture only ever has the complete body to hand back here — a 206 was
// what the browser's own request observed live, but fetchAndStoreBody
// (capture.go) always issues an unranged GET and stores the whole asset,
// so replaying that stored 206 verbatim with the full body and no
// Content-Range header is not a valid partial-content response and can
// break range-aware <video>/<audio> elements. 200 is the honest status
// for what is actually being returned; only staticServer's redirect path
// (>largeAssetThreshold) serves real Range/Content-Range semantics via
// http.ServeContent.
func inlineFulfillStatus(status int) int {
	if status == go_http.StatusPartialContent {
		return go_http.StatusOK
	}
	return statusOrDefault(status, go_http.StatusOK)
}

// fulfill issues Fetch.fulfillRequest. body/location are mutually
// exclusive in practice (a redirect has no body; a served asset has no
// Location), but both are threaded through one helper to keep the CDP
// params-building logic in a single place.
func (r *replayer) fulfill(ctx context.Context, session CDPSession, requestID string, status int, contentType string, body []byte, location string) {
	var headers []map[string]interface{}
	if contentType != "" {
		headers = append(headers, map[string]interface{}{"name": "Content-Type", "value": contentType})
	}
	if location != "" {
		headers = append(headers, map[string]interface{}{"name": "Location", "value": location})
	}

	params := map[string]interface{}{
		"requestId":       requestID,
		"responseCode":    status,
		"responseHeaders": headers,
	}
	if len(body) > 0 {
		params["body"] = base64.StdEncoding.EncodeToString(body)
	}

	if _, err := session.Send(ctx, "Fetch.fulfillRequest", params); err != nil {
		r.logger.Warn("offline cache replay: Fetch.fulfillRequest failed",
			zap.String("request_id", requestID), zap.Error(err))
	}
}

func (r *replayer) continueRequest(ctx context.Context, session CDPSession, requestID string) {
	if _, err := session.Send(ctx, "Fetch.continueRequest", map[string]interface{}{"requestId": requestID}); err != nil {
		r.logger.Warn("offline cache replay: Fetch.continueRequest failed",
			zap.String("request_id", requestID), zap.Error(err))
	}
}

func (r *replayer) handleMiss(ctx context.Context, session CDPSession, requestID string, mixed bool) {
	// mixed means the enabled scope is a strict subset of a playlist's
	// items (see EnableForPlaylist's doc): a miss here cannot be
	// distinguished from "belongs to an uncached sibling item that must
	// still reach the live network", so pass-through is the only choice
	// that does not break that sibling's playback. The configured
	// missPolicy's fail_closed guarantee only holds when the full set of
	// resources the page can request is actually known.
	if mixed || r.missPolicy == MissPolicyPassThrough {
		r.continueRequest(ctx, session, requestID)
		return
	}
	// Fetch.failRequest reports a genuine network-error condition to the
	// page, which is the honest "offline and not cached" shape, distinct
	// from fulfilling with an HTTP error status.
	if _, err := session.Send(ctx, "Fetch.failRequest", map[string]interface{}{
		"requestId":   requestID,
		"errorReason": "Failed",
	}); err != nil {
		r.logger.Warn("offline cache replay: Fetch.failRequest failed",
			zap.String("request_id", requestID), zap.Error(err))
	}
}
