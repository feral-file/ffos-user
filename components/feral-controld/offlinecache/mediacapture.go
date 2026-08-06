package offlinecache

import (
	"context"
	"fmt"
	"io"
	go_http "net/http"
	go_url "net/url"
	"slices"
	"strings"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// MediaCapturer downloads a single-file, non-software playlist item's
// source directly over HTTP — no browser involved. See classify.go's
// MediaClass doc for why ClassMedia/ClassUnknown items never need
// capture.go's headless-Chromium pipeline: the kiosk player renders each
// of these as a native element (<img>/<video>/<audio>/<object>/a
// non-scripted iframe) that requests the bare item.Source directly, so
// there is exactly one "dependency" to cache — the file itself.
//
//go:generate mockgen -source=mediacapture.go -destination=../mocks/offlinecache_mediacapture.go -package=mocks -mock_names=MediaCapturer=MockOfflineCacheMediaCapturer
type MediaCapturer interface {
	// Capture fetches item.Source with a single GET (following redirects
	// transparently — see fetchResource's doc for why the RESOLVED bytes
	// are stored under the ORIGINAL source URL) and saves the resulting
	// single-resource ItemRecord to the store. It returns the saved
	// record.
	Capture(ctx context.Context, item dp1playlist.PlaylistItem) (*ItemRecord, error)
}

type mediaCapturer struct {
	httpClient   wrapper.HTTPClient
	store        Store
	json         wrapper.JSON
	clock        wrapper.Clock
	maxDiskBytes int64 // <=0 means unlimited, mirrors capturer.maxResourceBytes
	logger       *zap.Logger
}

// NewMediaCapturer constructs a MediaCapturer. Unlike NewCapturer, this
// needs no Downloader/dialer — there is no browser to acquire or dial.
func NewMediaCapturer(
	httpClient wrapper.HTTPClient,
	store Store,
	jsonWrapper wrapper.JSON,
	clockWrapper wrapper.Clock,
	maxDiskBytes int64,
	logger *zap.Logger,
) MediaCapturer {
	return &mediaCapturer{
		httpClient:   httpClient,
		store:        store,
		json:         jsonWrapper,
		clock:        clockWrapper,
		maxDiskBytes: maxDiskBytes,
		logger:       logger,
	}
}

func (c *mediaCapturer) Capture(ctx context.Context, item dp1playlist.PlaylistItem) (*ItemRecord, error) {
	// Source is the item's cache identity (see SourceKey) and the one URL
	// this path downloads; the DP-1 item id is optional per spec and
	// deliberately not required here.
	if item.Source == "" {
		return nil, fmt.Errorf("offline cache: item must have a source")
	}

	// A single-resource capture only ever needs one reservation (unlike
	// capturer's multi-resource captureDiskBudget.record loop) — see
	// newDiskBudgetFromStore's doc for why this is the store's REMAINING
	// room, not the whole configured ceiling.
	budget := newDiskBudgetFromStore(c.store, c.maxDiskBytes, c.logger)
	capBytes, ok := budget.reserve()
	if !ok {
		return nil, fmt.Errorf("offline cache: disk budget exhausted, cannot download %s", item.Source)
	}

	// Unlike capture.go's software path — where a resource fetch failure
	// is recorded as an honest partial-coverage miss because OTHER
	// resources may still have succeeded — this item has exactly one
	// resource, so a failure here means the whole item failed to
	// download. Returning a hard error (rather than saving a
	// permanently-broken single-resource record) is what makes
	// service.process's existing StateFailed path apply, matching how
	// capturer.Capture already reports a failed Page.navigate: the
	// entry point itself never loaded.
	resource, err := c.fetchResource(ctx, item.Source, capBytes)
	if err != nil {
		return nil, fmt.Errorf("offline cache: download %s: %w", item.Source, err)
	}

	// One downloaded file is normally the WHOLE item for this path (see
	// MediaCapturer's doc), so Complete=true is the honest default. The
	// one exception this path can detect without a browser: a JSON .gltf
	// manifest whose spec-defined buffer/image URIs point at separate
	// external files. Those dependencies are NOT captured here (see
	// docs/offline-artwork-capture.md §7's known-limitations entry), and
	// the fail-closed replay policy will fail them offline — so reporting
	// such an item as fully cached would tell the controller "ready"
	// about an item that cannot actually render offline. Downgrading to
	// partial coverage keeps status honest while still serving what WAS
	// captured; self-contained .glb and data:-URI-only .gltf manifests
	// keep Complete=true.
	coverage := Coverage{Complete: true}
	if isGLTFManifest(item.Source, resource.ContentType) {
		if deps := c.gltfExternalDependencies(resource); len(deps) > 0 {
			reasons := make([]string, 0, len(deps))
			for _, dep := range deps {
				reasons = append(reasons, "gltf_external_dependency:"+dep)
			}
			coverage = Coverage{Complete: false, Reason: strings.Join(reasons, reasonSeparator)}
		}
	}

	rec := &ItemRecord{
		Item:       item,
		Entry:      item.Source,
		Resources:  []Resource{resource},
		Coverage:   coverage,
		CapturedAt: c.clock.Now().UTC(),
	}
	if err := c.store.SaveItem(rec); err != nil {
		return nil, fmt.Errorf("offline cache: save item %s: %w", item.Source, err)
	}
	return rec, nil
}

// maxGLTFManifestParseBytes bounds how much of a just-downloaded blob
// gltfExternalDependencies will read back into memory. A real .gltf
// manifest is JSON in the KB-to-MB range; anything larger is mislabeled
// or pathological, and the check quietly keeps the default complete
// coverage rather than buffering an arbitrarily large file — the same
// never-buffer-a-whole-gigabyte posture the download path itself holds.
const maxGLTFManifestParseBytes = 16 << 20 // 16 MiB

// isGLTFManifest reports whether a fetched resource looks like a JSON
// .gltf manifest — never binary .glb (model/gltf-binary), which embeds
// its buffers and is self-contained by design. Content-Type is the
// primary signal (model/gltf+json), but many CDNs serve .gltf under a
// generic type (application/json, application/octet-stream, or none at
// all), so the URL path's extension is accepted as a fallback — the
// same unreliable-Content-Type reasoning behind classify.go's
// isStreamingURL.
func isGLTFManifest(sourceURL, contentType string) bool {
	ct := strings.ToLower(contentType)
	if strings.HasPrefix(ct, "model/gltf-binary") {
		return false
	}
	if strings.HasPrefix(ct, "model/gltf+json") {
		return true
	}
	path := sourceURL
	if u, err := go_url.Parse(sourceURL); err == nil {
		path = u.Path
	}
	return strings.HasSuffix(strings.ToLower(path), ".gltf")
}

// gltfExternalDependencies parses the just-stored manifest blob and
// returns the buffer/image URIs that reference separate external files
// (anything other than an embedded data: URI). Per the glTF 2.0 spec,
// buffers[].uri and images[].uri are the only places a manifest can
// point at out-of-band payload files, so this check is exact rather
// than heuristic — which is exactly why the equivalent check for SVG
// (where external references can hide in href attributes, CSS url()
// values, @imports, ...) is deliberately NOT attempted here.
//
// Best-effort in the conservative direction: an unreadable, oversized,
// or non-JSON blob returns nil (keeping the item's default complete
// coverage) rather than failing the capture — a manifest this check
// cannot parse is one no renderer will parse either, and inventing a
// capture failure out of the checker's own limits would turn today's
// working ready/complete items into failures on any parse edge case.
func (c *mediaCapturer) gltfExternalDependencies(res Resource) []string {
	size, err := c.store.BlobSize(res.SHA256)
	if err != nil || size > maxGLTFManifestParseBytes {
		return nil
	}
	raw, err := c.store.ReadBlob(res.SHA256)
	if err != nil {
		c.logger.Warn("offline cache: failed to read back gltf manifest for dependency check, keeping complete coverage",
			zap.String("sha256", res.SHA256), zap.Error(err))
		return nil
	}
	var doc struct {
		Buffers []struct {
			URI string `json:"uri"`
		} `json:"buffers"`
		Images []struct {
			URI string `json:"uri"`
		} `json:"images"`
	}
	if err := c.json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	var deps []string
	appendExternal := func(uri string) {
		if uri == "" || strings.HasPrefix(uri, "data:") {
			return
		}
		deps = append(deps, uri)
	}
	for _, b := range doc.Buffers {
		appendExternal(b.URI)
	}
	for _, img := range doc.Images {
		appendExternal(img.URI)
	}
	return deps
}

// fetchResource downloads sourceURL and streams the body straight into
// the blob store (never buffered whole in memory — the same streaming
// contract capture.go's fetchAndStoreBody relies on for gigabyte-scale
// assets), capped at capBytes.
//
// The resulting Resource is keyed on sourceURL — the ORIGINAL,
// pre-redirect URL — never the resolved final URL any redirect chain
// landed on. wrapper.HTTPClient's underlying transport follows redirects
// transparently, so this fetch's response is already the fully-resolved
// 2xx body. Storing it under sourceURL means replay can fulfill the
// kiosk's request for the bare item.Source directly with status 200 and
// the resolved body — the player's <img>/<video>/<audio> element only
// ever requests that bare source URL (see classify.go's MediaClass doc),
// so there is no redirect hop to faithfully replay in the first place,
// unlike capture.go's software path (where an artwork's own JS can
// legitimately inspect/depend on an intermediate redirect response).
func (c *mediaCapturer) fetchResource(ctx context.Context, sourceURL string, capBytes int64) (Resource, error) {
	req, err := c.httpClient.NewRequest(go_http.MethodGet, sourceURL, nil)
	if err != nil {
		return Resource{}, fmt.Errorf("build request: %w", err)
	}
	// ff-player's <video crossOrigin="anonymous"> element always sends an
	// Origin header, and CDN/S3-backed CORS configs commonly only emit
	// Access-Control-Allow-Origin (and friends) when a request carries
	// one — a bare Origin-less GET can get a byte-identical response
	// with those headers silently absent. This capture path has no
	// browser, so it never sends one on its own; setting it explicitly
	// to the kiosk's real origin makes the CDN's response here match
	// what the live player would see, so filterReplayableHeaders below
	// actually has CORS headers to capture instead of finding none and
	// silently leaving Resource.Headers empty — which would otherwise
	// make every offline replay of this resource fail Chromium's own
	// CORS enforcement despite byte-correct status/body (see
	// docs/offline-artwork-capture.md §3.3/§4.6).
	req.Header.Set("Origin", strings.TrimSuffix(constant.WEBAPP_URL, "/"))

	// The only bound on this path: Bootstrap hands this capturer a client
	// with no http.Client.Timeout (see its bodyClient comment) and ctx is
	// the worker's daemon-lifetime context, so without this a wedged
	// origin would pin the single serial capture worker for the life of
	// the process. Shared with the software path's per-resource fetches
	// rather than a bespoke ceiling here — the two are downloading the
	// same kind of asset (§4.4's 1.1 GB video is reachable either way),
	// so they should tolerate the same slowness and give up on the same
	// stall.
	transfer := beginResourceTransfer(ctx)
	defer transfer.Close()

	resp, err := c.httpClient.Do(req.WithContext(transfer.Context()))
	if err != nil {
		return Resource{}, fmt.Errorf("fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < go_http.StatusOK || resp.StatusCode >= go_http.StatusMultipleChoices {
		return Resource{}, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	// This path has no browser to sniff for it, so where the origin
	// declared no Content-Type it must produce its own fallback or
	// Resource.SniffedContentType's invariant ("declared, else a sniffed
	// guess") would simply not hold here. That gap would land exactly
	// where it hurts most: classifyContentType maps an empty
	// Content-Type to ClassUnknown, which routes to THIS capturer — so a
	// headerless media origin, the very condition the field exists to
	// survive, would be the one case with nothing to fall back to, and
	// ff-player's HEAD content-type probe would get no answer for an
	// extensionless <img>/<video>/<audio> source.
	//
	// Sniffing wraps the body rather than re-reading the finished blob:
	// these assets are gigabyte-scale (see the streaming contract above),
	// and only the leading bytes are ever needed.
	body := transfer.Body(resp.Body)
	declared := resp.Header.Get("Content-Type")
	var sniffer *bodySniffer
	if declared == "" {
		sniffer = &bodySniffer{reader: body}
		body = sniffer
	}

	hash, err := c.store.WriteBlob(body, capBytes)
	if err != nil {
		return Resource{}, fmt.Errorf("write blob: %w", err)
	}

	return Resource{
		URL:    sourceURL,
		Status: go_http.StatusOK,
		SHA256: hash,
		// resp.Header.Get, not the raw resolved Content-Type of some
		// intermediate hop: Go's http.Client already reports the FINAL
		// response's headers here regardless of how many redirects were
		// followed to reach it.
		ContentType: declared,
		// Empty unless the origin declared nothing AND the leading bytes
		// were recognizable — see bodySniffer.contentType and
		// Resource.SniffedContentType.
		SniffedContentType: sniffer.contentType(),
		// Same allowlist-and-reason as capture.go's filterReplayableHeaders
		// (§4.6 of docs/offline-artwork-capture.md): a cross-origin
		// <video crossOrigin="anonymous"> element CORS-checks its
		// response exactly like a fetch()/XHR would, so these headers
		// must be replayed alongside the body or Chromium's own CORS
		// enforcement can reject an otherwise byte-correct replay.
		Headers: filterReplayableHeaders(headerFirstValues(resp.Header)),
	}, nil
}

// headerFirstValues flattens an http.Header (which allows repeated
// values per key) down to one value per key, matching the shape
// filterReplayableHeaders expects (the same shape CDP's own
// Network.responseReceived/redirectResponse events already deliver to
// capture.go, which is a single value per header name). None of the
// headers replayableResponseHeaders allowlists are meaningful repeated
// across multiple values in practice, so taking the first is a safe
// simplification, not a lossy one for this allowlist's purposes.
func headerFirstValues(h go_http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

// sniffWindowBytes is how many leading bytes bodySniffer retains, matching
// http.DetectContentType's own window — it never inspects more, so keeping
// more would be dead memory held for the whole (gigabyte-scale) transfer.
const sniffWindowBytes = 512

// bodySniffer is a pass-through io.Reader that retains the leading
// sniffWindowBytes of what flows through it, so fetchResource can classify
// a headerless response WITHOUT buffering the asset or re-reading the
// finished blob off disk. A nil *bodySniffer is valid and reports no type:
// fetchResource only allocates one when the origin declared nothing, so
// the nil case is "there was a declaration, nothing to sniff".
type bodySniffer struct {
	reader io.Reader
	head   []byte
}

func (s *bodySniffer) Read(p []byte) (int, error) {
	n, err := s.reader.Read(p)
	if n > 0 && len(s.head) < sniffWindowBytes {
		s.head = append(s.head, p[:min(n, sniffWindowBytes-len(s.head))]...)
	}
	return n, err
}

// sniffCatchAlls are http.DetectContentType's two "I cannot tell"
// answers — one per branch of its binary/text split (net/http's sniff
// table ends in a textSig entry that matches anything printable, exactly
// as the binary side falls through to octet-stream). Neither is a
// positive identification, and this path must not report one as though
// it were: a headerless SVG without an XML prolog and a headerless
// .gltf manifest both land on the text catch-all, and both are
// ClassUnknown items that route to THIS capturer, so reporting
// text/plain for them is not hypothetical.
var sniffCatchAlls = []string{"application/octet-stream", "text/plain; charset=utf-8"}

// contentType returns what the retained bytes sniff to, or "" when there
// is no honest answer.
//
// An empty body has nothing to classify, and a catch-all verdict is a
// non-answer wearing an answer's clothes. Recording either would be
// worse than silence for this field's one consumer: ff-player's renderer
// selection reads a returned type as authoritative and stops falling
// back to extension inference (see Resource.SniffedContentType), so a
// sniffed text/plain would demote precisely the .svg/.gltf items above
// from their correct renderer — the same class of offline-only breakage
// this whole split exists to prevent. Nothing of value is given up:
// every container the probe actually cares about (image/gif, image/png,
// video/mp4, audio/mpeg, application/pdf, prologued SVG as text/xml)
// carries a real signature.
//
// Chromium's own sniffed verdict on the capture.go path is kept verbatim
// by contrast, catch-alls included: that is what the live browser
// actually concluded about the response, not a guess this daemon
// synthesized in its absence.
func (s *bodySniffer) contentType() string {
	if s == nil || len(s.head) == 0 {
		return ""
	}
	sniffed := go_http.DetectContentType(s.head)
	if slices.Contains(sniffCatchAlls, sniffed) {
		return ""
	}
	return sniffed
}
