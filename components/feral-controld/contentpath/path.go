// Package contentpath parses the content-addressed part of a URL path: the
// IPFS CID after `/ipfs/`, or the Arweave transaction id at the root of a
// gateway path. It is the one place that decides what counts as
// content-addressed, shared by the kiosk rewrite (uarewrite) and the
// media-health probe (mediahealth). The two used to carry separate parsers,
// and a path one shape of mistake slipped past both — `/ipfs//<cid>` — left
// a work dialing a rate-limited gateway with no fallback on either side
// (office FF1, fixture item 36). Stdlib only, so both can import it.
package contentpath

// IPFS parses an escaped URL path as `/ipfs/<cid>` with an optional
// remainder. Slash runs and repeated `ipfs` segments before the id are
// collapsed: `/ipfs//<cid>/x.png` and `/ipfs/ipfs/<cid>` are common tokenURI
// mistakes — the first the default replacement gateway only redirects, the
// second no gateway serves (it names the CID "ipfs") — and both load once
// normalized. rest is everything after the id, byte for byte ("" or "/…"):
// a work's own files live there and are not ours to tidy. Only a real CID
// (IsCID) closes the prefix, so a path that merely contains "ipfs" is not
// content-addressed.
func IPFS(escapedPath string) (cid, rest string, ok bool) {
	seg, after, found := nextSegment(escapedPath)
	if !found || seg != "ipfs" {
		return "", "", false
	}
	for {
		seg, after, found = nextSegment(after)
		if !found {
			return "", "", false
		}
		if IsCID(seg) {
			return seg, after, true
		}
		if seg != "ipfs" {
			return "", "", false
		}
	}
}

// Arweave parses an escaped URL path as `/<tx>` with an optional remainder,
// collapsing a leading slash run. The caller decides whether the host is an
// Arweave gateway: a 43-character base64url segment at the root of any
// other site is a coincidence, not a transaction.
func Arweave(escapedPath string) (tx, rest string, ok bool) {
	seg, after, found := nextSegment(escapedPath)
	if !found || !IsArweaveTx(seg) {
		return "", "", false
	}
	return seg, after, true
}

// IsArweaveTx reports whether s is an Arweave transaction id: 32 bytes as
// unpadded base64url, 43 characters.
func IsArweaveTx(s string) bool {
	if len(s) != 43 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// nextSegment skips a run of slashes and returns the following path segment
// and the remainder after it (from its slash, or "").
func nextSegment(s string) (seg, rest string, ok bool) {
	i := 0
	for i < len(s) && s[i] == '/' {
		i++
	}
	if i == len(s) {
		return "", "", false
	}
	j := i
	for j < len(s) && s[j] != '/' {
		j++
	}
	return s[i:j], s[j:], true
}
