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
	// ClassInline is a source that carries its own bytes in the URL
	// itself — an RFC 2397 data: URI, which playlists use for inline
	// cover art and small SVG/GIF items.
	//
	// It is classified WITHOUT any network round trip (the media type is
	// read straight out of the URI) and then skipped at queue time
	// rather than downloaded, because there is nothing to fetch: the
	// bytes already live in the playlist body, which SavePlaylist
	// persists, so the player reads them back offline from the playlist
	// record itself. Queuing one would send "data:image/png;base64,..."
	// to http.Client and fail with "unsupported protocol scheme", which
	// is exactly the spurious failure this class exists to stop.
	ClassInline MediaClass = "inline"
)

//go:generate mockgen -source=classify.go -destination=../mocks/offlinecache_classify.go -package=mocks -mock_names=Classifier=MockOfflineCacheClassifier
type Classifier interface {
	// Classify resolves url's final Content-Type (after redirects, which
	// http.Client follows by default) and classifies it.
	Classify(ctx context.Context, url string) (MediaClass, error)
}

type classifier struct {
	httpClient wrapper.HTTPClient
	guard      sourceGuard
}

// NewClassifier builds the classifier. resolver is the DNS seam the
// source guard uses to reject hostnames that point back at this device
// or its LAN — pass net.DefaultResolver in production; see
// ErrUnsafeSource for why that check lives on this path.
func NewClassifier(httpClient wrapper.HTTPClient, resolver AddrResolver) Classifier {
	return &classifier{
		httpClient: httpClient,
		guard:      sourceGuard{resolver: resolver},
	}
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

// dataURIScheme is matched case-insensitively: RFC 3986 defines the
// scheme component as case-insensitive, and playlists in the wild carry
// both "data:" and "DATA:".
const dataURIScheme = "data:"

// maxDataURIMetadataBytes bounds how far into a data: URI the parser
// below scans for the comma that ends the metadata section.
//
// RFC 2397 puts the media type and its parameters BEFORE that comma and
// the payload after it, and the payload is routinely megabytes of base64
// (playlists carry inline cover images that large). Scanning only a
// bounded prefix means a malformed URI — one with no comma anywhere —
// costs a fixed 1 KiB scan instead of walking the whole blob, on a path
// that runs once per playlist item.
const maxDataURIMetadataBytes = 1024

// defaultDataURIMediaType is what RFC 2397 section 2 prescribes when the
// media type is omitted entirely ("data:,Hello").
const defaultDataURIMediaType = "text/plain;charset=us-ascii"

func isDataURI(rawURL string) bool {
	return len(rawURL) >= len(dataURIScheme) &&
		strings.EqualFold(rawURL[:len(dataURIScheme)], dataURIScheme)
}

// dataURIMediaType returns the media type a data: URI declares, read
// straight out of the URI rather than probed over the network.
//
// The payload is deliberately never decoded: classification needs only
// the declared type, and base64-decoding a multi-megabyte inline asset
// to learn something already stated in its first few bytes would burn
// memory on a constrained device for nothing. That also keeps a
// deliberately malformed payload from being a decode-bomb — nothing here
// allocates proportionally to the URI's length.
func dataURIMediaType(rawURL string) (string, error) {
	meta := rawURL[len(dataURIScheme):]
	comma := strings.IndexByte(meta[:min(len(meta), maxDataURIMetadataBytes)], ',')
	if comma < 0 {
		return "", fmt.Errorf(
			"offline cache: malformed data: URI, no ',' within the first %d bytes",
			maxDataURIMetadataBytes)
	}
	meta = strings.TrimSpace(meta[:comma])

	// ";base64" is a transfer-encoding marker, not part of the media
	// type, and RFC 2397 puts it last. Trimmed only as a true suffix so
	// a parameter that merely contains the word (";name=base64.png")
	// survives intact.
	if trimmed, ok := trimSuffixFold(meta, ";base64"); ok {
		meta = strings.TrimSpace(trimmed)
	}
	if meta == "" {
		return defaultDataURIMediaType, nil
	}
	return strings.ToLower(meta), nil
}

// trimSuffixFold removes suffix from s case-insensitively, reporting
// whether it was present.
func trimSuffixFold(s, suffix string) (string, bool) {
	if len(s) < len(suffix) {
		return s, false
	}
	split := len(s) - len(suffix)
	if !strings.EqualFold(s[split:], suffix) {
		return s, false
	}
	return s[:split], true
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
	// data: first, ahead of the guard: these are never dialed, so the
	// guard deliberately refuses them (see sourceGuard.check's doc) and
	// reaching it would reject a perfectly valid inline item. A
	// malformed one is still a real classification failure — the item is
	// unusable to the player too, so it must not be silently accepted.
	if isDataURI(url) {
		if _, err := dataURIMediaType(url); err != nil {
			return ClassUnknown, err
		}
		return ClassInline, nil
	}

	// Before ANY network round trip, and before the streaming-suffix
	// shortcut below: an unsafe source must not reach a probe, and must
	// not be able to earn a benign-looking classification either — see
	// ErrUnsafeSource for the threat model.
	if err := c.guard.check(ctx, url); err != nil {
		return ClassUnknown, err
	}

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
