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
}

// IsRedirect reports whether replay must fulfill this resource with a
// Location header rather than a cached body.
func (r Resource) IsRedirect() bool {
	return r.Status >= 300 && r.Status < 400 && r.RedirectTo != ""
}

// ReasonCSPBlocked is the one Coverage.Reason token capture.go emits as a
// fixed string; every other reason capture.go records
// (fetch_failed:<url>, loading_failed(<errorText>):<url>,
// unresolved_at_deadline:<url>) is free-text with the offending URL
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

// ItemRecord is the single on-disk source of truth for one cached DP-1 item
// (items/<itemId>.json in the Store). It merges the verbatim item, the
// entry URL Chromium actually loaded, and the capture index replay/export
// need, so there is exactly one file to read/write per item.
type ItemRecord struct {
	// ItemID is the DP-1 item id and this record's identity/filename.
	ItemID string `json:"itemId"`
	// Item is the DP-1 playlist item as resolved by dp1.DP1. Source is
	// never rewritten — replay interception keys on the original URL —
	// which preserves the signed-playlist invariant from capture through
	// to replay.
	Item dp1playlist.PlaylistItem `json:"item"`
	// Entry is the URL Chromium actually navigated to. Equal to
	// Item.Source unless Source itself redirected.
	Entry      string     `json:"entry"`
	Resources  []Resource `json:"resources"`
	Coverage   Coverage   `json:"coverage"`
	CapturedAt time.Time  `json:"capturedAt"`
}

// ResourceByURL returns the captured resource for url, if any.
func (r *ItemRecord) ResourceByURL(url string) (Resource, bool) {
	for _, res := range r.Resources {
		if res.URL == url {
			return res, true
		}
	}
	return Resource{}, false
}
