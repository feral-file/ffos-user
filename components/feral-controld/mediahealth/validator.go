package mediahealth

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/gabriel-vasile/mimetype"
)

// ContentVerdict is the outcome of validating a probe's body bytes.
// Declared and Sniffed are populated whenever available, including on OK
// verdicts, so the record can persist them.
type ContentVerdict struct {
	OK       bool
	Reason   Reason // empty when OK
	Detail   string // human-facing message for lastError; empty when OK
	Declared string // Content-Type header, normalized (lowercase, no parameters)
	Sniffed  string // magic-byte-detected type, normalized
}

// ContentValidator applies ff-indexer-v2's L0 rule ladder
// (docs/media_viewability.md) to the first bytes of a media URL's body.
// Ported from the indexer's internal/uri/content_validator.go with the
// rules and their order intact, so a device verdict and an indexer verdict
// for the same bytes agree; the in-code regression inputs travel with it.
//
// Every rule errs HEALTHY on ambiguity: a false broken hides a real artwork
// from the wall, which is strictly worse than letting a broken one through
// to the player's own retry/skip. Pure functions over an in-memory prefix —
// no I/O — so the same validator serves the cast-time pass, the lookahead
// and the tests.
type ContentValidator struct {
	probeMaxBytes   int
	knownBadMarkers []string // lowercase substrings from config; matched against HTML bodies
}

// NewContentValidator builds a validator. probeMaxBytes is the probe window
// (truncation is judged against it); knownBadMarkers are case-insensitive
// substrings identifying gateway error pages served with HTTP 200,
// operator-editable via config so a new gateway quirk is an edit, not a
// release.
func NewContentValidator(probeMaxBytes int, knownBadMarkers []string) *ContentValidator {
	lowered := make([]string, 0, len(knownBadMarkers))
	for _, m := range knownBadMarkers {
		if m = strings.ToLower(strings.TrimSpace(m)); m != "" {
			lowered = append(lowered, m)
		}
	}
	return &ContentValidator{probeMaxBytes: probeMaxBytes, knownBadMarkers: lowered}
}

// kuboDirectoryListingSignature reports how a body matches Kubo's
// dir-index-html directory listing, or "" when it does not. These pages are
// well-formed HTML with a 200 status — declared and sniffed types agree, so
// only body structure can catch them.
//
// Detection requires a CONJUNCTION of two independent template signals,
// never a lone substring — "ipfs-hash" is an ordinary CSS class name and a
// single marker false-brokens legitimate HTML artworks that merely contain
// it. Modern Kubo (0.13+) puts the gateway meta description at ~byte 70 and
// the requested path as the document title at ~byte 1.8K — both inside the
// probe window even for multi-MB listings — while every "Index of" heading
// and class="ipfs-hash" cell sits past the window. Legacy dir-index
// templates render a contiguous "Index of /ipfs/..." heading plus ipfs-hash
// cells. A Kubo fork that rewrites its template is missed and errs healthy.
func kuboDirectoryListingSignature(lowerBody string) string {
	const kuboMetaDescription = `<meta name="description" content="a directory of content-addressed files hosted on ipfs.">`
	pathTitle := strings.Contains(lowerBody, "<title>/ipfs/") || strings.Contains(lowerBody, "<title>/ipns/")
	if pathTitle && strings.Contains(lowerBody, kuboMetaDescription) {
		return "kubo meta description + path title"
	}
	legacyHeading := strings.Contains(lowerBody, "index of /ipfs") || strings.Contains(lowerBody, "index of /ipns")
	if legacyHeading && strings.Contains(lowerBody, `class="ipfs-hash"`) {
		return `"index of" heading + ipfs-hash cells`
	}
	return ""
}

// Validate applies the rule ladder; the first failing rule wins, anything
// inconclusive is OK. totalLength is the byte count the response undertook
// to deliver (the satisfied range's length for a 206 — NOT the Content-Range
// total, which would false-broken legitimately short ranges —
// Content-Length otherwise) and -1 when unknown.
func (v *ContentValidator) Validate(declaredContentType string, body []byte, totalLength int64) ContentVerdict {
	declared := normalizeContentType(declaredContentType)

	// 1. Empty body: nothing to display, regardless of what headers claim.
	if len(body) == 0 || totalLength == 0 {
		return ContentVerdict{Reason: ReasonZeroLength, Detail: "empty response body", Declared: declared}
	}

	// 2. Truncated: the connection ended before both the declared length and
	// our read cap — the server does not have the full bytes it advertised.
	if totalLength > 0 && int64(len(body)) < totalLength && len(body) < v.probeMaxBytes {
		return ContentVerdict{
			Reason:   ReasonTruncated,
			Detail:   fmt.Sprintf("body ended at %d of %d declared bytes", len(body), totalLength),
			Declared: declared,
		}
	}

	// Sniffing is version-sensitive: mimetype inspects only its own internal
	// window (3072 bytes at the pinned v1.4.9) and matcher behavior changes
	// between releases. Every rule below must stay conclusive under any
	// sniff outcome, and the regression tests assert verdicts, never sniffed
	// types. The dependency is pinned in go.mod for exactly this reason.
	sniffed := normalizeContentType(mimetype.Detect(body).String())
	verdict := ContentVerdict{OK: true, Declared: declared, Sniffed: sniffed}

	// 3–5 only apply to HTML bodies; a real media payload cannot be a
	// directory listing or a gateway error page.
	if sniffed == "text/html" {
		lowerBody := strings.ToLower(string(body))
		if signature := kuboDirectoryListingSignature(lowerBody); signature != "" {
			verdict.OK = false
			verdict.Reason = ReasonDirectoryListing
			verdict.Detail = fmt.Sprintf("IPFS gateway directory listing (%s)", signature)
			return verdict
		}
		if marker := firstMarker(lowerBody, v.knownBadMarkers); marker != "" {
			verdict.OK = false
			verdict.Reason = ReasonKnownErrorPage
			verdict.Detail = fmt.Sprintf("known gateway error page (marker %q)", marker)
			return verdict
		}
	}

	// 6. Declared media but text body: an HTML, plain-text, or JSON payload
	// cannot be the image, video, or audio the metadata promised. Text-based
	// media subtypes (SVG, playlists, ASCII bitmaps) are exempt — for them a
	// text body is expected. Declared HTML/JSON/text is never flagged: HTML
	// artworks are legitimate.
	if isMediaClass(declared) && !isTextBasedMediaType(declared) &&
		(sniffed == "text/html" || sniffed == "text/plain" || sniffed == "application/json") {
		verdict.OK = false
		verdict.Reason = ReasonTypeMismatch
		verdict.Detail = fmt.Sprintf("declared %s but body is %s", declared, sniffed)
		return verdict
	}

	// 7. Container header parse for formats where sniffing alone is too
	// shallow: the magic bytes match on the first few bytes, so a corrupt
	// header still sniffs as the format. Video is left to the sniffer.
	if reason, detail := validateContainer(sniffed, body); reason != "" {
		verdict.OK = false
		verdict.Reason = reason
		verdict.Detail = detail
		return verdict
	}

	return verdict
}

// normalizeContentType lowercases and strips parameters
// ("Image/PNG; charset=x" → "image/png").
func normalizeContentType(ct string) string {
	ct = strings.ToLower(strings.TrimSpace(ct))
	return strings.TrimSpace(strings.Split(ct, ";")[0])
}

// isMediaClass reports whether a declared type promises binary media content.
func isMediaClass(declared string) bool {
	return strings.HasPrefix(declared, "image/") ||
		strings.HasPrefix(declared, "video/") ||
		strings.HasPrefix(declared, "audio/")
}

// isTextBasedMediaType reports whether a declared media type is a text
// format, for which a text-sniffed body is expected rather than evidence of
// an error page. RFC 6839 +xml/+json suffixes cover image/svg+xml and any
// structured-text subtype; the explicit list covers text formats without a
// suffix. Expects a normalized type.
func isTextBasedMediaType(declared string) bool {
	if strings.HasSuffix(declared, "+xml") || strings.HasSuffix(declared, "+json") {
		return true
	}
	switch declared {
	case "image/svg",
		"audio/x-mpegurl", "audio/mpegurl",
		"image/x-xbitmap",
		"image/x-portable-anymap", "image/x-portable-bitmap",
		"image/x-portable-graymap", "image/x-portable-pixmap":
		return true
	}
	return false
}

func firstMarker(lowerBody string, markers []string) string {
	for _, m := range markers {
		if strings.Contains(lowerBody, m) {
			return m
		}
	}
	return ""
}

// validateContainer parses the header of recognized image containers.
// Returns ("", "") when the container is valid, unrecognized, or there are
// not enough bytes to judge — inconclusive errs healthy.
func validateContainer(sniffed string, body []byte) (Reason, string) {
	var ok bool
	var detail string
	switch sniffed {
	case "image/png":
		ok, detail = validPNGHeader(body)
	case "image/gif":
		ok, detail = validGIFHeader(body)
	case "image/webp":
		ok, detail = validWebPHeader(body)
	case "image/jpeg":
		ok, detail = validJPEGStructure(body)
	default:
		return "", ""
	}
	if ok {
		return "", ""
	}
	return ReasonContainerInvalid, detail
}

// validPNGHeader checks the PNG signature is followed by a well-formed IHDR
// chunk: spec-mandated chunk length 13, "IHDR" type, non-zero dimensions
// (signature 8B + length 4B + "IHDR" 4B + width 4B + height 4B = 24B).
func validPNGHeader(body []byte) (bool, string) {
	if len(body) < 24 {
		return true, ""
	}
	if !bytes.Equal(body[12:16], []byte("IHDR")) {
		return false, "PNG signature without IHDR chunk"
	}
	if l := binary.BigEndian.Uint32(body[8:12]); l != 13 {
		return false, fmt.Sprintf("PNG IHDR chunk with invalid length %d", l)
	}
	width := binary.BigEndian.Uint32(body[16:20])
	height := binary.BigEndian.Uint32(body[20:24])
	if width == 0 || height == 0 {
		return false, fmt.Sprintf("PNG with zero dimension (%dx%d)", width, height)
	}
	return true, ""
}

// validGIFHeader checks the GIF logical screen descriptor has non-zero
// dimensions (header 6B + width 2B + height 2B = 10B).
func validGIFHeader(body []byte) (bool, string) {
	if len(body) < 10 {
		return true, ""
	}
	width := binary.LittleEndian.Uint16(body[6:8])
	height := binary.LittleEndian.Uint16(body[8:10])
	if width == 0 || height == 0 {
		return false, fmt.Sprintf("GIF with zero dimension (%dx%d)", width, height)
	}
	return true, ""
}

// validWebPHeader checks the RIFF container declares the WEBP fourcc and a
// plausible chunk size ("RIFF" 4B + size 4B + "WEBP" 4B = 12B).
func validWebPHeader(body []byte) (bool, string) {
	if len(body) < 12 {
		return true, ""
	}
	if !bytes.Equal(body[8:12], []byte("WEBP")) {
		return false, "RIFF container without WEBP fourcc"
	}
	if binary.LittleEndian.Uint32(body[4:8]) == 0 {
		return false, "RIFF container with zero size"
	}
	return true, ""
}

// validJPEGStructure walks JPEG marker segments from SOI as far as the probe
// window allows, the way the decoder Chromium uses (libjpeg-turbo) reads
// them. Bytes where a marker should start are skipped up to the next real
// marker — libjpeg warns "N extraneous bytes before marker" and decodes on —
// so only what the decoder itself rejects is corrupt: a segment length
// below 2. Running out of window while the structure is still consistent,
// or while skipping stray bytes, is inconclusive (OK).
//
// Rejecting stray bytes outright marked two complete 2048x2048 JPEGs
// corrupt_file and skipped them for a day: each carries 38 bytes of an
// orphaned Photoshop metadata block after its Exif segment, which Chromium
// steps over (office FF1, 2026-11-06). ff-indexer-v2's walk has the same
// false positive.
func validJPEGStructure(body []byte) (bool, string) {
	i := 2
	for i+4 <= len(body) {
		if body[i] != 0xFF {
			next := nextJPEGMarker(body, i)
			if next < 0 {
				return true, ""
			}
			i = next
			continue
		}
		marker := body[i+1]
		if marker == 0xFF {
			i++
			continue
		}
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			i += 2
			continue
		}
		if marker == 0xDA {
			return true, ""
		}
		segLen := int(binary.BigEndian.Uint16(body[i+2 : i+4]))
		if segLen < 2 {
			return false, fmt.Sprintf("JPEG segment with invalid length %d at offset %d", segLen, i)
		}
		i += 2 + segLen
	}
	return true, ""
}

// nextJPEGMarker returns the offset of the next marker at or after from: a
// 0xFF followed by a byte that is neither a fill byte (0xFF) nor a stuffed
// zero. -1 when the held bytes end first.
func nextJPEGMarker(body []byte, from int) int {
	for j := from; j+1 < len(body); j++ {
		if body[j] == 0xFF && body[j+1] != 0x00 && body[j+1] != 0xFF {
			return j
		}
	}
	return -1
}
