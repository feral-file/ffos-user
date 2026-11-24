package dp1

import (
	"slices"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
)

// A device-local source is one the device resolves itself rather than a host
// it dials: an onchfs tree read from chain (onchfs.go) or a web3 contract
// call (web3.go). Both are rewritten to a reserved `.invalid` origin before
// an item leaves this package, so the player, the offline-cache key, media
// health and the replay scope all see one identity per work.
//
// This file is the seam: every exit calls mapDeviceLocalSources, and every
// caller outside dp1 that holds a raw URI calls the three functions below
// rather than a scheme-specific one. Before web3 each of those callers named
// onchfs directly (`dp1.OnchfsSource` at the commandrouter boundary), which
// is why adding a second scheme had to touch each of them — the mechanism
// feral-file#3599 retires.

// DeviceLocalSource returns the device-local source for a raw onchfs:// or
// web3:// source, or source unchanged when it is neither. Callers that take
// a source from outside (the app's download and clear commands) use it so a
// raw URI and the source the player carries reach the same cache key.
func DeviceLocalSource(source string) string {
	if mapped := OnchfsSource(source); mapped != source {
		return mapped
	}
	return Web3Source(source)
}

// DeviceLocalSourceVariants lists every device-local source source can be
// stored under, or nil when it is not a device-local scheme. onchfs has
// several because the chain label depends on the item's provenance, which a
// caller holding only the raw URI cannot know; a web3 work has exactly one.
func DeviceLocalSourceVariants(source string) []string {
	if v := OnchfsSourceVariants(source); v != nil {
		return v
	}
	if mapped := Web3Source(source); mapped != source {
		return []string{mapped}
	}
	return nil
}

// SameDeviceLocalWork reports whether a and b name the same device-local
// work, whatever form either is in.
func SameDeviceLocalWork(a, b string) bool {
	return SameOnchfsWork(a, b) || SameWeb3Work(a, b)
}

// mapDeviceLocalSources returns items with every device-local source
// rewritten. The slice is copied before the first change, never written in
// place: a caller's slice (a cached playlist, a dp1-go result) must not
// change under it.
func mapDeviceLocalSources(items []dp1playlist.PlaylistItem) []dp1playlist.PlaylistItem {
	out := items
	for i := range items {
		mapped := deviceLocalItemSource(items[i])
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

// deviceLocalItemSource maps one item's source. onchfs takes the chain family
// from the item's provenance (onchfsItemSource); web3 needs nothing from the
// item, because its URL carries the chain.
func deviceLocalItemSource(item dp1playlist.PlaylistItem) string {
	if mapped := onchfsItemSource(item); mapped != item.Source {
		return mapped
	}
	return Web3Source(item.Source)
}
