// Package contentpath parses the content-addressed part of a URL path: the
// IPFS CID after `/ipfs/`, or the Arweave transaction id at the root of a
// gateway path. It is the one place that decides what counts as
// content-addressed, shared by the kiosk rewrite (uarewrite) and the
// media-health probe (mediahealth). The two used to carry separate parsers,
// and a path one shape of mistake slipped past both — `/ipfs//<cid>` — left
// a work dialing a rate-limited gateway with no fallback on either side
// (office FF1, fixture item 36). Stdlib only, so both can import it.
package contentpath

import "net/url"

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

// SlashBeforeQuery turns a bare CID with a query — `…/ipfs/<cid>?fxhash=…`,
// the shape fxhash gentk v1/v2 metadata carries — into the directory form
// `…/ipfs/<cid>/?fxhash=…`, on the same host, query and fragment kept. ok is
// false for any other shape (no query, a remainder after the CID, not an
// IPFS path), including the slashed form itself, so applying it twice is a
// no-op.
//
// Why: every gateway measured (raribleuserdata, filebase) answers the bare
// shape with a 301 to `/<cid>/` that DROPS the query, so the work's snippet
// reads a null hash and the sketch never runs, while the iframe still
// reports ready (PR #438 review, 2026-11-08, three works on a shared
// playlist). New playlists come out slashed (ff-indexer-v2#178); every
// playlist already shared carries the bare shape, so the device heals it:
// the kiosk with a synthetic redirect (uarewrite), the probe by dialing the
// slashed wire (mediahealth). A file CID with a trailing slash and a query
// is still served by the gateways we route to (checked on filebase), so
// the slash is safe for non-directory sources too. The path comes out
// normalized (slash runs and repeated `ipfs` segments collapsed), since the
// URL is rebuilt from the parsed CID.
func SlashBeforeQuery(rawURL string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || (u.RawQuery == "" && !u.ForceQuery) {
		return "", false
	}
	cid, rest, ok := IPFS(u.EscapedPath())
	if !ok || rest != "" {
		return "", false
	}
	u.Path = "/ipfs/" + cid + "/"
	u.RawPath = ""
	return u.String(), true
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
