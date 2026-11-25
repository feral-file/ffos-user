package dp1

import (
	"github.com/feral-file/ffos-user/components/feral-controld/contentpath"
)

// web3 works (feral-file#3599) leave this package with a device-local source,
// for the same reason onchfs works do (onchfs.go): the kiosk loads http(s)
// only, and no host serves a contract call. The device makes the call itself
// and replay serves the returned bytes at the source
// contentpath.Web3.SourceURL gives.
//
// Unlike onchfs there is no chain to discover. ERC-4804 puts the chain id in
// the URL and defaults it to 1, so the mapping needs nothing from the item's
// provenance and a URI that omitted the chain and one that wrote `:1` give
// one source and one cache key. There is correspondingly no variant set: a
// work has exactly one device-local source.
//
// Like the onchfs mapping this is pure and idempotent, runs at every exit an
// item can take, and runs after signature verification, which covers the
// document's original bytes rather than the decoded items.

// Web3Source returns the device-local source for a web3:// source, or source
// unchanged when it is not one.
func Web3Source(source string) string {
	if w, ok := contentpath.ParseWeb3(source); ok {
		return w.SourceURL()
	}
	return source
}

// SameWeb3Work reports whether a and b name the same contract call, whether
// either is raw or device-local, and whether either spelled the chain id.
func SameWeb3Work(a, b string) bool {
	wa, okA := parseWeb3OrDeviceSource(a)
	wb, okB := parseWeb3OrDeviceSource(b)
	return okA && okB && wa == wb
}

func parseWeb3OrDeviceSource(s string) (contentpath.Web3, bool) {
	if w, ok := contentpath.ParseWeb3SourceURL(s); ok {
		return w, true
	}
	return contentpath.ParseWeb3(s)
}
