package offlinecache

import (
	"context"
	"fmt"
	"io"
	"net/http"
	go_url "net/url"
	"strings"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// MediaClass is the coarse classification of a playlist item's source,
// used to route offline downloads to the right capture pipeline (see
// service.go's captureForClass):
//
//   - ClassSoftware (an HTML/JS entry document) can load an unenumerable,
//     runtime-computed dependency graph, so it needs a real headless
//     browser to run its code and observe what it actually requests
//     (capture.go).
//   - ClassMedia and ClassUnknown are both routed to the browser-free
//     direct-download path (mediacapture.go): the kiosk player renders
//     every one of these as a native element (<img>/<video>/<audio>/
//     <object>/iframe-without-scripting) that requests the bare source
//     URL directly, so the "dependency graph" is exactly one file — no
//     browser is needed to discover dependencies that do not exist.
//     ClassUnknown (an empty or unrecognized Content-Type) is treated
//     the same as ClassMedia rather than rejected: the goal is offline
//     caching for every single-file artwork, and a best-effort direct
//     download degrades safely (an honest capture failure) if the
//     resolved bytes turn out not to be a single self-contained file.
//   - ClassStreaming (HLS/.m3u8 or DASH/.mpd live or VOD manifests) is
//     the one class offline caching does not support at all: a manifest
//     points at a set of segments fetched progressively during
//     playback, not a single fixed byte sequence, so there is nothing a
//     one-shot download or a static blob-store replay could faithfully
//     serve. Both manifest families must be excluded here, not just
//     HLS: a DASH manifest that instead fell through to ClassUnknown's
//     single-file path would cache only the manifest with
//     Coverage.Complete=true, then fail every segment request offline
//     under the default fail-closed miss policy while status still
//     reports the item as fully cached (see feral-file/ffos-user#229
//     review discussion).
type MediaClass string

const (
	ClassSoftware  MediaClass = "software"
	ClassMedia     MediaClass = "media"
	ClassStreaming MediaClass = "streaming"
	ClassUnknown   MediaClass = "unknown"
)

//go:generate mockgen -source=classify.go -destination=../mocks/offlinecache_classify.go -package=mocks -mock_names=Classifier=MockOfflineCacheClassifier
type Classifier interface {
	// Classify resolves url's final Content-Type (after redirects, which
	// http.Client follows by default) and classifies it.
	Classify(ctx context.Context, url string) (MediaClass, error)
}

type classifier struct {
	httpClient wrapper.HTTPClient
}

func NewClassifier(httpClient wrapper.HTTPClient) Classifier {
	return &classifier{httpClient: httpClient}
}

// streamingURLSuffixes identifies a manifest-based streaming source by
// URL alone, checked before any network round trip: some CDNs serve a
// manifest with a generic or even missing Content-Type (e.g. behind a
// signed-URL proxy that does not preserve it), so the URL's own
// extension is the more reliable signal here, not merely a fallback for
// it. Covers both manifest families this daemon must reject — HLS
// (.m3u8) and DASH (.mpd) — see MediaClass's doc on why DASH must be
// excluded here too, not just HLS.
var streamingURLSuffixes = []string{".m3u8", ".mpd"}

// isStreamingURL reports whether rawURL's path ends in one of
// streamingURLSuffixes. A parse failure falls back to a plain suffix
// check on the raw string — item.Source is validated for real elsewhere
// (a malformed URL fails the eventual fetch/navigate with a clearer
// error); this helper only needs to be a reasonable best-effort signal,
// never a security boundary.
func isStreamingURL(rawURL string) bool {
	path := rawURL
	if u, err := go_url.Parse(rawURL); err == nil {
		path = u.Path
	}
	path = strings.ToLower(path)
	for _, suffix := range streamingURLSuffixes {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

// streamingContentTypePrefixes are the Content-Type values an HLS or
// DASH manifest resolves to when an origin does set one; checked ahead
// of mediaContentTypePrefixes/softwareContentTypePrefixes so a
// streaming manifest is never misclassified as a downloadable media
// file (or, for DASH specifically, as ClassUnknown — see MediaClass's
// doc for why that matters).
var streamingContentTypePrefixes = []string{
	"application/vnd.apple.mpegurl", // HLS
	"application/x-mpegurl",         // HLS
	"audio/mpegurl",                 // HLS
	"application/dash+xml",          // DASH
}

// mediaContentTypePrefixes are Content-Type prefixes the player renders
// as a native <img>/<video>/<audio> element — see MediaClass's doc for
// why these (and ClassUnknown) are downloaded directly rather than
// routed through the headless-browser pipeline.
var mediaContentTypePrefixes = []string{"image/", "video/", "audio/"}

// softwareContentTypePrefixes are the shapes a browser-rendered artwork's
// entry document is expected to have.
var softwareContentTypePrefixes = []string{
	"text/html",
	"application/xhtml+xml",
	"application/javascript",
	"text/javascript",
}

// ClassifyProbeRangeBytes bounds the GET fallback's requested slice, and
// doubles as the cap on how many response bytes this process will ever
// pull locally when an origin ignores the Range header. Classification
// only ever inspects the Content-Type header — never the body — but
// item.Source can point at a multi-GB video or an effectively unbounded
// live stream, so trusting an unranged GET (or trusting resp.Body.Close()
// to not drain an unread body internally, which is transport-dependent)
// risks pulling that entire asset through the daemon's process just to
// read one header. Exported so tests can assert against it precisely.
const ClassifyProbeRangeBytes = 4096

func (c *classifier) Classify(ctx context.Context, url string) (MediaClass, error) {
	// Checked before any network round trip — see isStreamingURL's doc
	// for why the URL's own extension is trusted ahead of a HEAD probe.
	if isStreamingURL(url) {
		return ClassStreaming, nil
	}
	// HEAD first (cheap, no body at all); some origins reject HEAD, so
	// fall back to a range-bounded GET on a non-2xx/redirect status.
	class, status, err := c.headClassify(ctx, url)
	if err != nil {
		return ClassUnknown, err
	}
	if status >= http.StatusBadRequest {
		return c.rangedGETClassify(ctx, url)
	}
	return class, nil
}

func (c *classifier) headClassify(ctx context.Context, url string) (MediaClass, int, error) {
	req, err := c.httpClient.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return ClassUnknown, 0, fmt.Errorf("offline cache: build classify HEAD request: %w", err)
	}
	req = req.WithContext(ctx)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ClassUnknown, 0, fmt.Errorf("offline cache: classify HEAD request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	return classifyContentType(contentType), resp.StatusCode, nil
}

// rangedGETClassify classifies url via GET when HEAD was rejected, bounding
// how many response bytes this process will ever pull from the origin. It
// asks for only the first ClassifyProbeRangeBytes bytes via a Range header
// — a compliant origin answers 206 Partial Content and this never touches
// more than that small slice. Origins that ignore Range (answering 200
// with the full body — e.g. a multi-GB video that doesn't support ranged
// requests) are still bounded: the body is drained through a capped
// io.CopyN before the connection is torn down, so this can never buffer or
// stream an unbounded asset through the daemon merely to read a header.
func (c *classifier) rangedGETClassify(ctx context.Context, url string) (MediaClass, error) {
	req, err := c.httpClient.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return ClassUnknown, fmt.Errorf("offline cache: build classify GET request: %w", err)
	}
	req = req.WithContext(ctx)
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", ClassifyProbeRangeBytes-1))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ClassUnknown, fmt.Errorf("offline cache: classify GET request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bound the read regardless of whether the origin honored Range (206)
	// or ignored it (200, streaming the full asset): io.CopyN never reads
	// past the cap, so a short body (io.EOF before the cap) is the only
	// possible error here and is not actionable — classification only
	// needs the header read above, not the body itself.
	_, _ = io.CopyN(io.Discard, resp.Body, ClassifyProbeRangeBytes)

	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	return classifyContentType(contentType), nil
}

func classifyContentType(contentType string) MediaClass {
	if contentType == "" {
		return ClassUnknown
	}
	for _, prefix := range streamingContentTypePrefixes {
		if strings.HasPrefix(contentType, prefix) {
			return ClassStreaming
		}
	}
	for _, prefix := range mediaContentTypePrefixes {
		if strings.HasPrefix(contentType, prefix) {
			return ClassMedia
		}
	}
	for _, prefix := range softwareContentTypePrefixes {
		if strings.HasPrefix(contentType, prefix) {
			return ClassSoftware
		}
	}
	return ClassUnknown
}
