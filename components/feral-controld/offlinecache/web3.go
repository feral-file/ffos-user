package offlinecache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/contentpath"
	"github.com/feral-file/ffos-user/components/feral-controld/web3"
)

// The web3 half of the device-local materializer (feral-file#3599). See
// devicelocal.go for the machinery; this file is only how one resolved
// contract call becomes the resources replay serves.
//
// A web3 work is one document, not a tree: the call returns the whole thing
// (Han Saglam's Digital Soul is 9 KiB of self-contained HTML, Art Blocks'
// generator 224 KiB). So there is one blob and the resources are the few URLs
// the kiosk can ask for it by, where the onchfs half has one per file.

// web3Resources turns a resolved call into the resources replay answers with
// and the one blob behind them.
//
// The kiosk navigates to the source URL, query and all, so that URL is the
// entry (replay matches method and URL exactly) — in the spelling Chromium
// requests, not the playlist's (browserEntryURL).
//
// **It is the only URL stored, and that is a correctness rule, not economy.**
// A web3 host is a pure function of contract and chain (the owner's recorded
// choice), so every work on one contract shares one origin — unlike onchfs,
// whose host comes from the work's own root. Replay builds one flat
// method+URL map over every record in the playlist and the last record wins
// (replay.go, EnableForPlaylist), so any URL a sibling work could also emit
// is a URL that can be answered from the wrong work's bytes. Two aliases
// were stored here and both collided: the origin root, which every edition
// of a contract would claim, and the path without its query, which is
// another work's entry exactly (`/artpiece` is the entry of
// `web3://…/artpiece`, and the alias of `web3://…/artpiece?seed=…`). The
// kiosk would navigate to one work's source and be served another's
// document, and nothing could report it — a device-local source is exempt
// from every media-health verdict, so no probe runs and no outcome is
// recorded (round 3 review, F-B).
//
// The origin root is therefore stored only when the entry *is* the origin
// root: a URI with no method path, which no sibling with a method can be.
//
// What this gives up: a document that references itself by `/` or by its
// bare path now misses. That is the same "a sub-resource misses" limitation
// the scheme already documents, and it fails closed — a miss reaches a
// `.invalid` host and dies — where the alias failed open, by serving a
// different work.
//
// Each resource carries the content type the URL's own extension gave the
// work (web3.Work.ContentType — a contract call has no header to carry one,
// and sniffing would let the bytes choose how they are rendered) and
// Access-Control-Allow-Origin, which the player's cross-origin HEAD probe
// needs. None carries Content-Encoding: the bytes are stored plain.
func web3Resources(w contentpath.Web3, source string, work *web3.Work, freshUntil time.Time) ([]Resource, map[string][]byte, error) {
	if work == nil || len(work.Body) == 0 {
		return nil, nil, fmt.Errorf("%w: the call returned an empty body", web3.ErrNotFound)
	}
	sum := sha256.Sum256(work.Body)
	sha := hex.EncodeToString(sum[:])
	blobs := map[string][]byte{sha: work.Body}

	contentType := work.ContentType
	if contentType == "" {
		contentType = "text/html"
	}
	entry := browserEntryURL(w, source)
	urls := []string{entry}
	// The origin root, only when this work's entry is the origin root. A
	// work with a method path must never claim it: a sibling on the same
	// contract would claim the same URL (see above).
	if root := w.Origin() + "/"; entry == root {
		urls = append(urls, root)
	}

	seen := map[string]bool{}
	resources := make([]Resource, 0, len(urls))
	for _, u := range urls {
		if seen[u] {
			continue
		}
		seen[u] = true
		resources = append(resources, Resource{
			URL:         u,
			Status:      200,
			SHA256:      sha,
			ContentType: contentType,
			Headers:     map[string]string{"Access-Control-Allow-Origin": "*"},
			FreshUntil:  freshUntil,
		})
	}
	return resources, blobs, nil
}

// browserEntryURL is the entry URL as Chromium requests it, fragment dropped.
//
// Replay matches URLs exactly, and Chromium percent-encodes a URL it is
// handed: `?seed=it's` goes out as `?seed=it%27s` and `/greet/string!hello
// world` as `hello%20world`. It is respelled from source itself, the string
// the kiosk is given, and not from w: w came back through
// ParseWeb3SourceURL, whose Go re-escaping (`!` becomes `%21`) is not
// Chromium's. The contract call is built from the playlist's own spelling
// and is untouched (round 5 review, F8). Existing `%XX` escapes stay as
// written, as a browser leaves them.
func browserEntryURL(w contentpath.Web3, source string) string {
	entry := entryURL(source)
	rest, ok := strings.CutPrefix(entry, w.Origin()+"/")
	if !ok {
		return entry
	}
	path, query, hasQuery := strings.Cut(rest, "?")
	var b strings.Builder
	b.WriteString(w.Origin())
	b.WriteByte('/')
	b.WriteString(whatwgEscape(path, false))
	if hasQuery {
		b.WriteByte('?')
		b.WriteString(whatwgEscape(query, true))
	}
	return b.String()
}

// whatwgEscape applies the WHATWG URL standard's path percent-encode set
// (controls, space, non-ASCII, and " # < > ? ` { }) to a path, or the
// special-query set (controls, space, non-ASCII, " # < > and ') to a query.
// Every other byte, % included, is kept.
func whatwgEscape(s string, query bool) string {
	set := "\"#<>?`{}"
	if query {
		set = "\"#<>'"
	}
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x20 || c >= 0x7f || strings.IndexByte(set, c) >= 0 {
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&15])
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
