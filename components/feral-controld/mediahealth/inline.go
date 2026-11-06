package mediahealth

import (
	"fmt"
	"strings"
)

// data: URIs never reach the network: their bytes travel inside the
// playlist body. The probe settles them synchronously — a well-formed one
// is VerdictInline, a PROVEN malformed one VerdictDead. The parse is a
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
// Payload bytes are deliberately never decoded or validated — the caster
// authored them, and percent-encoded base64 would false-reject.
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
