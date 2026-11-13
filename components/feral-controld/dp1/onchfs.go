package dp1

import (
	"net/url"
	"slices"
	"strings"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/contentpath"
)

// onchfs works (ffos-user#427) leave this package with a device-local source.
//
// An item whose source is `onchfs://<root>[/path][?query]` cannot be loaded
// by the kiosk, and no host serves it since fxhash closed. The device reads
// such a work from chain into the offline cache's store and replay serves it
// at the source contentpath.Onchfs.SourceURL gives. Every item that leaves
// this package therefore already carries that URL, so the player, the
// offline-cache key (sha256 of the exact source), media health and the
// replay scope all see one identity per work, whichever path resolved it.
//
// The rewrite is applied at each exit an item can take: Playlist.UnmarshalJSON
// (every decoded document, static or fetched), processDynamicPlaylist (DP-1
// hydration, the Raster walk, the legacy indexer), expandSeriesItems and
// CastPlan.Narrow (catalog editions). It is idempotent and a pure function of
// the source, so applying it twice, or to a playlist the player echoed back,
// changes nothing. It runs after signature verification, which covers the
// document's original bytes, never the decoded items.
//
// It does not depend on whether the device's reader is enabled: with the
// reader off, the device-local URL is exactly as unplayable as the raw
// `onchfs://` source was, and one identity per work is kept either way.
//
// A source on fxhash's dead onchfs gateway (`https://onchfs.fxhash2.xyz/
// <root>…`, still present in playlists published before the shutdown) is
// read as the onchfs URI it was derived from, as ff-indexer-v2 does at
// ingest (types.CanonicalOnChFSURI). The host itself is never dialed.

// deadOnchfsGatewayHosts are fxhash's onchfs gateways, all gone with fxhash
// (no DNS since 2026-08-17). Literal hosts, never a pattern: only these
// served `/<root>/<path>` for onchfs.
var deadOnchfsGatewayHosts = map[string]bool{
	"onchfs.fxhash2.xyz": true,
	"onchfs.fxhash.xyz":  true,
}

// OnchfsSource returns the device-local source for an onchfs source, or
// source unchanged when it is not one.
func OnchfsSource(source string) string {
	if o, ok := contentpath.ParseOnchfs(source); ok {
		return o.SourceURL()
	}
	if u, err := url.Parse(source); err == nil && u.Scheme == "https" && deadOnchfsGatewayHosts[strings.ToLower(u.Host)] {
		if o, ok := contentpath.ParseOnchfs("onchfs://" + strings.TrimPrefix(source[len("https://")+len(u.Host):], "/")); ok {
			return o.SourceURL()
		}
	}
	return source
}

// mapOnchfsSources returns items with every onchfs source rewritten (see
// above). The slice is copied before the first change, never written in
// place: a caller's slice (a cached playlist, a dp1-go result) must not
// change under it.
func mapOnchfsSources(items []dp1playlist.PlaylistItem) []dp1playlist.PlaylistItem {
	out := items
	for i := range items {
		mapped := OnchfsSource(items[i].Source)
		if mapped == items[i].Source {
			continue
		}
		if &out[0] == &items[0] {
			out = slices.Clone(items)
		}
		out[i].Source = mapped
	}
	return out
}
