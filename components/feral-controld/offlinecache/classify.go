package offlinecache

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// MediaClass is the coarse classification of a playlist item's source, used
// to gate offline downloads to software-only items. The player already
// handles native image/video/audio playback; the headless-capture pipeline
// exists for interactive/software artworks that need a real browser to
// render.
type MediaClass string

const (
	ClassSoftware MediaClass = "software"
	ClassMedia    MediaClass = "media"
	ClassUnknown  MediaClass = "unknown"
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

// mediaContentTypePrefixes are Content-Type prefixes the player already
// handles natively; offline capture explicitly does not target these.
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
