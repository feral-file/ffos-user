package offlinecache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

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
// The kiosk navigates to the exact source URL, query and all, so that URL is
// the entry and must be stored verbatim (replay matches method and URL
// exactly). Two more spellings are stored for the same bytes, because a work
// may name itself in its own markup:
//
//   - the origin root (`https://<host>/`), which is what a relative "/" in
//     the document resolves to;
//   - the path without the query, for a document that links to itself by
//     path.
//
// All three carry the content type the URL's own extension gave the work
// (web3.Work.ContentType — a contract call has no header to carry one, and
// sniffing would let the bytes choose how they are rendered) and
// Access-Control-Allow-Origin, which the player's cross-origin HEAD probe
// needs. None carries Content-Encoding: the bytes are stored plain.
//
// Anything else the document asks for misses. A work whose markup pulls a
// sub-resource by a relative path has nothing to pull it from — the call
// returned one document — and an absolute URL to another origin leaves the
// replay scope and goes to the network as it would from any page. Both
// measured works are self-contained, which is what a work behind a view
// function has to be.
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
	entry := entryURL(source)
	urls := []string{entry, w.Origin() + "/"}
	// The path without a query, when the source carried one. A work that
	// links to itself by path (`<a href="/artpiece">`) then resolves.
	if w.HasQuery {
		bare := w
		bare.Query, bare.HasQuery = "", false
		bare.Fragment, bare.HasFragment = "", false
		urls = append(urls, bare.SourceURL())
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
