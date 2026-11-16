package mediahealth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/gabriel-vasile/mimetype"

	"github.com/feral-file/ffos-user/components/feral-controld/contentpath"
)

// data: URIs never reach the network: their bytes travel inside the
// playlist body. The probe settles them synchronously — a PROVEN malformed
// one is VerdictDead, a well-formed one has its payload judged in place
// (checkDataURIPayload): VerdictInline when it passes, VerdictDead when it
// cannot be what the wall shows. The metadata parse is a
// bounded metadata scan copied in shape from offlinecache's
// dataURIMediaType (this package cannot import it); dataURIMetadataBytes
// and the comma rule must stay in step with that function, and a test in
// offlinecache pins the two parsers equal.

const dataURIScheme = "data:"

// dataURIMetadataBytes bounds how far into a data: URI the parser scans for
// the comma that ends the metadata section. RFC 2397 puts the media type
// before that comma and the payload after it, and the payload is routinely
// megabytes of base64; scanning only a bounded prefix keeps a malformed URI
// a fixed-cost check.
const dataURIMetadataBytes = 1024

// IsDeviceHeld reports whether a source's bytes are held by the device
// itself rather than fetched from a host: a data: URI, or an onchfs work the
// device read from chain and serves from the offline cache's store
// (contentpath.IsOnchfsSourceURL, ffos-user#427). Neither has an origin to
// probe, and a failure to show one says nothing about the network, so the
// passes that judge origins (the kiosk observer, inferred failures, the L1
// render probe) skip both. data: URIs keep their own payload check
// (probeDataURI); an onchfs tree is verified byte for byte when it is read.
func IsDeviceHeld(rawURL string) bool {
	return IsDataURI(rawURL) || contentpath.IsOnchfsSourceURL(rawURL)
}

// errDeviceHeld is the inconclusive result of probing an onchfs source:
// there is no origin to dial. It is never recorded (probeWritesRecord).
var errDeviceHeld = errors.New("media health: onchfs source is read from chain by the device, not probed")

// IsDataURI reports whether rawURL is a data: URI (scheme case-insensitive,
// as RFC 3986 defines it).
func IsDataURI(rawURL string) bool {
	return len(rawURL) >= len(dataURIScheme) &&
		strings.EqualFold(rawURL[:len(dataURIScheme)], dataURIScheme)
}

// dataURIMediaType returns the media type a data: URI declares. It reports
// (type, proven, err): err is non-nil when no comma was found inside the
// scan bound; proven is true when the WHOLE URI fit inside that bound, so
// the absence of a comma is a parse fact rather than a scan-limit artifact.
// It never touches the payload; checkDataURIPayload judges that.
func dataURIMediaType(rawURL string) (mediaType string, proven bool, err error) {
	meta := rawURL[len(dataURIScheme):]
	window := meta[:min(len(meta), dataURIMetadataBytes)]
	comma := strings.IndexByte(window, ',')
	if comma < 0 {
		return "", len(rawURL) <= len(dataURIScheme)+dataURIMetadataBytes,
			fmt.Errorf("malformed data: URI, no ',' within the first %d bytes", dataURIMetadataBytes)
	}
	meta = strings.TrimSpace(meta[:comma])
	if cut, ok := trimSuffixFold(meta, ";base64"); ok {
		meta = strings.TrimSpace(cut)
	}
	if meta == "" {
		return "text/plain;charset=us-ascii", true, nil
	}
	return strings.ToLower(meta), true, nil
}

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

// dataURIPayloadBytes bounds how much of a data: URI's payload is decoded
// for the type check: the probe window, like a fetched body. A payload is
// routinely megabytes of base64, and the sniff needs only its head.
const dataURIPayloadBytes = DefaultProbeMaxBytes

// checkDataURIPayload judges a well-formed data: URI by ff-indexer-v2's data:
// rule: a declared type the kiosk cannot display is unsupported_mime_type,
// an empty payload zero_length, and a payload whose sniffed type does not
// match the declared one type_mismatch.
// It returns ("", "", sniffed) for a payload that passes, and decoded=false
// when the payload does not decode at all — no verdict either way: the
// decode below follows Chromium's rules, but a reading we cannot reproduce
// is not proof the wall cannot.
//
// The kiosk renders a data: URI by its DECLARED type, so a mismatch is
// what the wall shows, not a pedantic label: an SVG declared
// application/json shows Chromium's JSON viewer over a black frame, and a
// poem declared text/markdown shows nothing (office FF1, 2026-11-09). The
// player reports both as ready, so without this check they read `unknown`
// forever.
func checkDataURIPayload(rawURL, declared string) (reason Reason, detail, sniffed string, decoded bool) {
	declared = normalizeContentType(declared)
	if !displayableDataType(declared) {
		return ReasonUnsupportedMimeType, fmt.Sprintf("data: URI declares %s, which the kiosk does not display", declared), "", true
	}
	isBase64, payload, ok := dataURIPayload(rawURL)
	if !ok {
		return "", "", "", false
	}
	data, whole, ok := decodeDataURIHead(payload, isBase64, dataURIPayloadBytes)
	if !ok {
		return "", "", "", false
	}
	if len(data) == 0 {
		if whole {
			return ReasonZeroLength, "empty data: payload", "", true
		}
		return "", "", "", false
	}
	sniffed = normalizeContentType(mimetype.Detect(data).String())
	if !dataTypesMatch(declared, sniffed) {
		return ReasonTypeMismatch, fmt.Sprintf("data: URI declares %s but its payload is %s", declared, sniffed), sniffed, true
	}
	return "", "", sniffed, true
}

// dataURIPayload splits a data: URI at the comma that ends its metadata
// (the same bounded scan as dataURIMediaType) and reports whether the
// metadata ends in ;base64.
func dataURIPayload(rawURL string) (isBase64 bool, payload string, ok bool) {
	meta := rawURL[len(dataURIScheme):]
	window := meta[:min(len(meta), dataURIMetadataBytes)]
	comma := strings.IndexByte(window, ',')
	if comma < 0 {
		return false, "", false
	}
	_, isBase64 = trimSuffixFold(strings.TrimSpace(meta[:comma]), ";base64")
	return isBase64, meta[comma+1:], true
}

// decodeDataURIHead decodes at most limit bytes of a payload the way
// Chromium does: percent-decoding first (an invalid escape stays as
// written), then, for ;base64, WHATWG forgiving-base64 (ASCII whitespace
// ignored, padding optional). whole reports that the bytes returned are the
// entire payload. ok is false when the bytes cannot be decoded.
func decodeDataURIHead(payload string, isBase64 bool, limit int) (data []byte, whole, ok bool) {
	// A percent escape is 3 raw characters per byte and base64 is 4 per 3,
	// so 4*limit raw characters always cover limit decoded bytes.
	raw := payload[:min(len(payload), 4*limit)]
	whole = len(raw) == len(payload)
	if !whole {
		// Never cut an escape in half.
		if i := strings.LastIndexByte(raw, '%'); i >= 0 && i >= len(raw)-2 {
			raw = raw[:i]
		}
	}
	text := percentDecodeLenient(raw)
	if !isBase64 {
		if len(text) > limit {
			return text[:limit], false, true
		}
		return text, whole, true
	}
	b64 := make([]byte, 0, len(text))
	for _, c := range text {
		switch c {
		case ' ', '\t', '\n', '\f', '\r':
		default:
			b64 = append(b64, c)
		}
	}
	if whole {
		if len(b64)%4 == 0 {
			for range 2 {
				if n := len(b64); n > 0 && b64[n-1] == '=' {
					b64 = b64[:n-1]
				}
			}
		}
		if len(b64)%4 == 1 {
			return nil, false, false
		}
	} else {
		b64 = b64[:len(b64)-len(b64)%4]
	}
	out, err := base64.RawStdEncoding.DecodeString(string(b64))
	if err != nil {
		return nil, false, false
	}
	if len(out) > limit {
		return out[:limit], false, true
	}
	return out, whole, true
}

// percentDecodeLenient decodes %XX escapes and leaves a malformed one as
// written, as the URL parser does for a data: URL's body.
func percentDecodeLenient(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHexDigit(s[i+1]) && isHexDigit(s[i+2]) {
			out = append(out, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
			continue
		}
		out = append(out, s[i])
	}
	return out
}

func isHexDigit(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

// displayableDataType is ff-indexer-v2's isSupportedMimeType (image/*,
// video/*, text/*, application/json, application/pdf) plus the families
// the player has its own renderers for (audio/*, model/*). Anything else
// reaches Chromium as a download and the frame stays blank.
func displayableDataType(declared string) bool {
	family, _, _ := strings.Cut(declared, "/")
	switch family {
	case "image", "video", "audio", "text", "model":
		return true
	}
	return declared == "application/json" || declared == "application/pdf"
}

// dataTypesMatch is ff-indexer-v2's mimeTypesMatch (data_uri_checker.go)
// with two device-side allowances, both about what Chromium does with the
// declared type rather than what the label says:
//   - an image/, video/ or audio/ type matches any sniffed type in the same
//     family: those elements sniff their own content, so data:image/jpg
//     holding a PNG still draws;
//   - text/html matches any text, XML or JSON body: the HTML parser renders
//     whatever it is handed.
func dataTypesMatch(declared, detected string) bool {
	if declared == detected {
		return true
	}
	if (declared == "image/svg" && detected == "image/svg+xml") || (declared == "image/svg+xml" && detected == "image/svg") {
		return true
	}
	declaredFamily, _, _ := strings.Cut(declared, "/")
	detectedFamily, _, _ := strings.Cut(detected, "/")
	switch declaredFamily {
	case "image", "video", "audio":
		if declaredFamily == detectedFamily {
			return true
		}
	}
	textual := detectedFamily == "text" || strings.HasSuffix(detected, "+xml") || detected == "application/json" || detected == "application/xml"
	if declared == "text/html" && textual {
		return true
	}
	if isTextBasedMediaType(declared) && detectedFamily == "text" {
		return true
	}
	if (declared == "text/plain" || declared == "application/json") && detectedFamily == "text" {
		return true
	}
	return false
}
