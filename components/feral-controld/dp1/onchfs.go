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
// source unchanged when it is not one. It knows no chain beyond what the
// URI names; for an item, use the item's provenance (onchfsItemSource).
func OnchfsSource(source string) string {
	if o, ok := parseOnchfsSource(source); ok {
		return o.SourceURL()
	}
	return source
}

func parseOnchfsSource(source string) (contentpath.Onchfs, bool) {
	if o, ok := contentpath.ParseOnchfs(source); ok {
		return o, true
	}
	if u, err := url.Parse(source); err == nil && u.Scheme == "https" && deadOnchfsGatewayHosts[strings.ToLower(u.Host)] {
		return contentpath.ParseOnchfs("onchfs://" + strings.TrimPrefix(source[len("https://")+len(u.Host):], "/"))
	}
	return contentpath.Onchfs{}, false
}

// onchfsItemSource is OnchfsSource with the chain family taken from the
// item's provenance when the URI names no chain (the indexer's bare form).
//
// The family is a security input, not a hint. Tezos derives a file address
// from its content and EVM from its chunk list, so the address of any EVM
// file is also a valid Tezos address for bytes equal to that file's packed
// chunk checksums: a forged Tezos node could pass those bytes off as an
// Ethereum work (PR #461 review F1). Reading a work only from its own
// family closes that. A work with no provenance falls back to the
// resolver's ordering (EVM first, Tezos only on confirmed EVM absence;
// onchfs.Resolver).
func onchfsItemSource(item dp1playlist.PlaylistItem) string {
	o, ok := parseOnchfsSource(item.Source)
	if !ok {
		return item.Source
	}
	if o.Chain == "" && item.Provenance != nil && item.Provenance.Contract != nil {
		switch strings.ToLower(strings.TrimSpace(item.Provenance.Contract.Chain)) {
		case "evm":
			o.Chain = contentpath.OnchfsEVM
		case "tezos":
			o.Chain = onchfsTezosMainnet
		}
	}
	return o.SourceURL()
}

// OnchfsSourceVariants lists every device-local source source can be stored
// under: the work with no chain label, then each family and chain label.
// An item's source carries the label its provenance gave it
// (onchfsItemSource), which a caller holding only the raw onchfs:// URI
// cannot know, so a lookup by raw URI tries each (PR #461 review F7). nil
// when source is not an onchfs source.
func OnchfsSourceVariants(source string) []string {
	o, ok := parseOnchfsSource(source)
	if !ok {
		return nil
	}
	if o.Chain != "" {
		return []string{o.SourceURL()}
	}
	out := []string{o.SourceURL()}
	for _, chain := range []string{contentpath.OnchfsEVM, onchfsTezosMainnet, "eip155:1", "eip155:8453"} {
		o.Chain = chain
		out = append(out, o.SourceURL())
	}
	return out
}

// SameOnchfsWork reports whether a and b name the same onchfs work (root,
// path, query, fragment), whatever chain label either carries and whether
// either is raw or device-local.
func SameOnchfsWork(a, b string) bool {
	oa, okA := parseOnchfsOrDeviceSource(a)
	ob, okB := parseOnchfsOrDeviceSource(b)
	if !okA || !okB {
		return false
	}
	oa.Chain, ob.Chain = "", ""
	return oa == ob
}

func parseOnchfsOrDeviceSource(s string) (contentpath.Onchfs, bool) {
	if o, ok := contentpath.ParseOnchfsSourceURL(s); ok {
		return o, true
	}
	return parseOnchfsSource(s)
}

// onchfsTezosMainnet is the Tezos chain contentpath knows onchfs on.
const onchfsTezosMainnet = "tezos:NetXdQprcVkpaWU"

// mapOnchfsSources returns items with every onchfs source rewritten (see
// above). The slice is copied before the first change, never written in
// place: a caller's slice (a cached playlist, a dp1-go result) must not
// change under it.
func mapOnchfsSources(items []dp1playlist.PlaylistItem) []dp1playlist.PlaylistItem {
	out := items
	for i := range items {
		mapped := onchfsItemSource(items[i])
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
