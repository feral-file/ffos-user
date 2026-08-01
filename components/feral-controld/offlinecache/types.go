// Package offlinecache captures, stores, and replays offline copies of
// software-based DP-1 playlist items so feral-player can display them
// without a live internet connection. The capture/replay edge cases this
// package is built to handle (redirects, 206 partial content, blob: URLs,
// the ~400MB CDP fulfill ceiling, CSP-broken-online artworks) were
// validated against real DP-1 feed content before this package was
// written; see docs/offline-artwork-capture.md for the reasoning behind
// each one.
package offlinecache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	go_http "net/http"
	go_url "net/url"
	"strings"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
)

// ItemState is the lifecycle state of one cached playlist item, reported to
// the mobile app via the getOfflineCacheStatus command and the
// offline_cache_status notification. It is computed on demand (job-queue
// state + on-disk Coverage), never persisted — see Store's package doc for
// why the on-disk format keeps no top-level manifest.
type ItemState string

const (
	StateNotCached    ItemState = "not_cached"
	StateQueued       ItemState = "queued"
	StateDownloading  ItemState = "downloading"
	StateReady        ItemState = "ready"
	StatePartial      ItemState = "partial"
	StateFailed       ItemState = "failed"
	StateBrokenOnline ItemState = "broken_online"
)

// Resource is one network resource observed while capturing an item. Only
// the fields replay routing and status reporting actually consume are
// kept: url/status/redirectTo drive replay's fulfill-or-redirect decision,
// sha256/contentType drive the fulfill body, and that is the entire
// persisted schema (see plan "Canonical on-disk format" for the fields
// deliberately dropped: role/note/failedRequests/size all derive from
// these or from the blob file itself).
type Resource struct {
	URL         string `json:"url"`
	Status      int    `json:"status"`
	SHA256      string `json:"sha256,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	// RedirectTo holds the Location header of a 3xx response. Replay
	// fulfills the redirect itself (rather than following it during
	// capture) so the real target is captured and cached as its own
	// Resource entry — CDP's Network.getResponseBody cannot return a body
	// for a redirect response anyway.
	RedirectTo string `json:"redirectTo,omitempty"`
	// Headers holds the subset of this response's headers that
	// replayableResponseHeaders allowlists — see that var's doc for
	// exactly what is kept and why. nil/empty for a response that had
	// none of them (the common case for a plain same-origin asset).
	Headers map[string]string `json:"headers,omitempty"`
	// Method is the HTTP method of the request that produced this
	// response. Empty means GET: capture only ever recorded GET
	// requests before this field existed, and GET is by far the common
	// case for the static assets software artworks load, so leaving it
	// unset for GET keeps the on-disk record free of redundant text
	// (see resourceKey/EffectiveMethod for the empty-means-GET
	// convention capture and replay both share).
	//
	// Method (not just URL) is part of this resource's identity —
	// resourceKey combines both into the map key both capture.go's
	// tracker and replay.go's replayer index by. Without it, an XHR/
	// fetch() CORS preflight (OPTIONS) and its paired actual request
	// (POST/PUT/...) to the identical URL would collide on a single
	// map entry, and a captured GET response could later be replayed
	// for an unrelated POST/DELETE/... request to the same URL — wrong
	// bytes served for a method-sensitive endpoint while
	// Coverage.Complete still claimed the item was faithfully cached.
	Method string `json:"method,omitempty"`
}

// IsRedirect reports whether replay must fulfill this resource with a
// Location header rather than a cached body.
func (r Resource) IsRedirect() bool {
	return r.Status >= 300 && r.Status < 400 && r.RedirectTo != ""
}

// EffectiveMethod returns r.Method, defaulting to GET — see Method's doc
// for why an empty value has always meant GET.
func (r Resource) EffectiveMethod() string {
	if r.Method == "" {
		return go_http.MethodGet
	}
	return r.Method
}

// resourceKey is the identity capture.go's tracker and replay.go's
// replayer both index resources by: method+URL, never URL alone (see
// Resource.Method's doc for the collision/mis-replay this closes).
// Normalizing the empty method to GET here, in one place, is what lets
// every pre-existing GET-only Resource (on disk or in a test fixture that
// predates this field) keep matching without a migration.
func resourceKey(method, url string) string {
	if method == "" {
		method = go_http.MethodGet
	}
	return strings.ToUpper(method) + " " + url
}

// requestOrigin returns rawURL's scheme://host[:port] origin (CDP's
// Storage.clearDataForOrigin expects exactly this shape, no path/query/
// fragment). Used by capture's resetTargetState to scope a per-item
// storage clear to the artwork's own origin.
func requestOrigin(rawURL string) (string, error) {
	u, err := go_url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("url %q has no scheme/host to derive an origin from", rawURL)
	}
	return u.Scheme + "://" + u.Host, nil
}

// replayableResponseHeaders is the allowlist of CDP response headers
// Resource.Headers persists and replay (Fetch.fulfillRequest and the
// static server's loopback GET) re-serves. It is deliberately narrow:
//
//   - Content-Length, Content-Encoding, Transfer-Encoding, Connection and
//     other hop-by-hop/transport headers describe the ORIGINAL transfer.
//     Capture always re-fetches with a plain unranged GET and stores the
//     fully-decoded body (capture.go's fetchAndStoreBody), and replay
//     always serves that stored body as-is at a different (replayed)
//     length/encoding, so persisting the original values would actively
//     lie to the browser about the response it is receiving.
//   - Set-Cookie must never be persisted to disk or replayed: doing so
//     would let an offline replay silently set stale/foreign cookies for
//     the artwork's origin.
//   - Content-Type is already tracked as its own Resource field, not
//     duplicated here.
//
// What remains is exactly the set of response headers Chromium's own
// CORS / cross-origin enforcement checks when it receives a fetched
// resource. Without these, a captured cross-origin module script, font,
// or fetch()/XHR response can replay with the correct status and body
// bytes yet still be rejected by the browser's own CORS check — silently
// breaking an artwork offline that worked fine online, which is exactly
// the failure mode this allowlist exists to close.
var replayableResponseHeaders = map[string]bool{
	"Access-Control-Allow-Origin":      true,
	"Access-Control-Allow-Credentials": true,
	"Access-Control-Allow-Methods":     true,
	"Access-Control-Allow-Headers":     true,
	"Access-Control-Expose-Headers":    true,
	"Cross-Origin-Resource-Policy":     true,
	"Cross-Origin-Embedder-Policy":     true,
	"Timing-Allow-Origin":              true,
}

// isReplayableHeaderName reports whether name (in any casing) is one of
// replayableResponseHeaders. Exposed as its own helper so staticserver.go
// can re-validate headers decoded off a redirect URL's query string as a
// second, defense-in-depth check against the same allowlist capture.go's
// filterReplayableHeaders already applied when the Resource was saved.
func isReplayableHeaderName(name string) bool {
	return replayableResponseHeaders[go_http.CanonicalHeaderKey(name)]
}

// filterReplayableHeaders returns the subset of raw (keyed however CDP's
// Network.responseReceived/redirectResponse delivered them, i.e. whatever
// case the origin server sent) that replayableResponseHeaders allows,
// canonicalized to Go's standard header casing so replay's
// Fetch.fulfillRequest and the static server always emit consistent
// header names regardless of how the original origin cased them. Returns
// nil (matching Resource.Headers' omitempty) when nothing in raw matched.
func filterReplayableHeaders(raw map[string]string) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]string
	for name, value := range raw {
		canonical := go_http.CanonicalHeaderKey(name)
		if !replayableResponseHeaders[canonical] {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(raw))
		}
		out[canonical] = value
	}
	return out
}

// ReasonCSPBlocked is the one Coverage.Reason token capture.go emits as a
// fixed string; every other reason capture.go records
// (fetch_failed:<url>, loading_failed(<errorText>):<url>,
// unresolved_at_deadline:<url>, http_error(<status>):<url>,
// unsupported_method(<method>):<url>) is free-text with the offending URL
// embedded, which is why Coverage.Reason itself is a plain string rather
// than an enum — see docs/controld-inbound-controller-messages.md's
// getOfflineCacheStatus section for the documented wire format clients
// actually receive.
const ReasonCSPBlocked = "csp_blocked"

// Coverage summarizes whether a captured item is complete enough for
// deterministic offline replay. Complete=false + Reason lets status
// reporting distinguish "still capturing" from permanently degraded
// captures (e.g. CSP-broken-online) without persisting a separate
// failedRequests list.
type Coverage struct {
	Complete bool   `json:"complete"`
	Reason   string `json:"reason,omitempty"`
}

// SourceKey derives the cache identity for a DP-1 item source URL:
// hex(sha256(source)) over the EXACT byte string as it appears in the
// resolved playlist, with deliberately NO normalization (no lowercasing,
// no query-param sorting, no trailing-slash trimming). Replay interception
// also matches the exact URL, so a "normalized" key would claim a cache
// hit for bytes captured under a different URL — the one direction that
// can serve wrong content. Under-normalizing merely costs a duplicate
// capture for trivially-different spellings of the same URL, which is
// rare and bounded.
//
// This is the identity EVERYWHERE inside the package: on-disk record
// filenames (items/<sourceKey>.json), every service-side state map, the
// capture queue, and replay scope resolution. The raw source string
// appears only at package boundaries (the wire contract,
// resolved-playlist call sites) and as reporting data carried alongside
// the key. Fixed-length keys also keep map keys and filenames free of
// arbitrary-length, externally-controlled URL strings — the same
// convention the store's playlists/by-url/ index already uses.
//
// The DP-1 item id is deliberately NOT an identity anywhere, for a reason
// that does not depend on how any particular resolver behaves: the DP-1
// core schema makes `id` OPTIONAL (only `source` is required) and defines
// it as a UUID v4 — a random identifier carrying no derivation from the
// artwork — so a spec-conforming playlist may omit it entirely or change
// it freely. Nothing durable may key on a field with that contract.
// Source is also what replay actually matches paused requests against
// (see resourceKey), so keying storage on it makes the storage identity
// and the lookup identity the same thing.
//
// Observed instability is the symptom, not the argument. Items
// materialized from the spec `dynamicQuery` profile carry whatever id the
// remote resolver returned (dp1-go mints none), and in the field those
// ids have been seen changing between resolutions of the same playlist —
// which is what orphaned id-keyed records. Do not generalize that to
// "dynamic playlists always regenerate ids": the legacy dynamicQueries/
// FFIndexer path in dp1.go happens to mint a deterministic UUIDv5 over
// (contract, chain, tokenNumber). That is an implementation detail of one
// resolver, not a contract — and the spec above is why neither behavior
// can be relied on.
//
// The trade-off this accepts, stated plainly: source is mutable where an
// id may not be. An FFIndexer-resolved item's source is the token's
// animation_url/image_url, so a CDN migration or a re-rendered preview
// changes it and orphans the cached record, costing a re-download. That
// is the correct outcome rather than a regression — the captured bytes
// are keyed to the exact URL replay will request, so a record under the
// old URL cannot serve the new one — but it is a real cost, and the
// reason a future "same artwork, new source" recovery would need a
// separate provenance-based alias rather than a change to this key.
func SourceKey(source string) string {
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:])
}

// ItemRecord is the single on-disk source of truth for one cached artwork
// source (items/<SourceKey(Item.Source)>.json in the Store). It merges the
// verbatim item, the entry URL Chromium actually loaded, and the capture
// index replay/export need, so there is exactly one file to read/write per
// source.
//
// The record is per-SOURCE, not per-playlist-item: multiple playlist items
// — within one playlist or across playlists — that share a source converge
// on this one record. Saving refreshes the cached artifact for all of
// them, and deleting it removes the cached artifact for all of them (no
// refcount, by design — see Store.DeleteItem's doc).
type ItemRecord struct {
	// Item is the DP-1 playlist item as resolved by dp1.DP1. Item.Source
	// is this record's identity (see SourceKey) and is never rewritten —
	// replay interception keys on the original URL — which preserves the
	// signed-playlist invariant from capture through to replay. The item's
	// optional DP-1 id is carried verbatim but is informational only:
	// when items sharing this source appear in several resolutions, the
	// record holds whichever item was captured last.
	Item dp1playlist.PlaylistItem `json:"item"`
	// Entry is the URL Chromium actually navigated to. Equal to
	// Item.Source unless Source itself redirected.
	Entry      string     `json:"entry"`
	Resources  []Resource `json:"resources"`
	Coverage   Coverage   `json:"coverage"`
	CapturedAt time.Time  `json:"capturedAt"`
}

// ResourceByURL returns the captured resource for url, if any. Method-
// oblivious (unlike resourceKey, which capture.go/replay.go actually
// index by): if url was captured under more than one method, this
// returns whichever one appears first in Resources. Test-only helper
// today — production replay/capture matching always goes through
// resourceKey instead, precisely because that ambiguity would be a
// correctness bug there.
func (r *ItemRecord) ResourceByURL(url string) (Resource, bool) {
	for _, res := range r.Resources {
		if res.URL == url {
			return res, true
		}
	}
	return Resource{}, false
}
