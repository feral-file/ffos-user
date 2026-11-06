package mediahealth

// Ported from ff-indexer-v2 internal/uri/content_validator_test.go so the
// device and the indexer agree verdict-for-verdict on the same bytes. The
// inputs are built in code (the indexer keeps no fixture directory). Every
// case asserts the VERDICT; sniffed types are asserted only where the
// indexer does, because the sniffer's window and matchers move between
// versions (go.mod pins mimetype for that reason).

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

const testProbeMaxBytes = 32 * 1024

func minimalPNG(width, height uint32) []byte {
	buf := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	buf = append(buf, 0x00, 0x00, 0x00, 0x0D)
	buf = append(buf, []byte("IHDR")...)
	buf = binary.BigEndian.AppendUint32(buf, width)
	buf = binary.BigEndian.AppendUint32(buf, height)
	buf = append(buf, 8, 2, 0, 0, 0)
	buf = append(buf, 0x00, 0x00, 0x00, 0x00)
	return buf
}

func minimalGIF(width, height uint16) []byte {
	buf := []byte("GIF89a")
	buf = binary.LittleEndian.AppendUint16(buf, width)
	buf = binary.LittleEndian.AppendUint16(buf, height)
	buf = append(buf, 0x00, 0x00, 0x00)
	return buf
}

func minimalWebP() []byte {
	buf := []byte("RIFF")
	buf = binary.LittleEndian.AppendUint32(buf, 1024)
	buf = append(buf, []byte("WEBP")...)
	buf = append(buf, []byte("VP8 ")...)
	return buf
}

func minimalJPEG() []byte {
	buf := []byte{0xFF, 0xD8}
	buf = append(buf, 0xFF, 0xE0, 0x00, 0x10)
	buf = append(buf, []byte("JFIF\x00")...)
	buf = append(buf, make([]byte, 16-2-5)...)
	buf = append(buf, 0xFF, 0xDA, 0x00, 0x0C)
	buf = append(buf, make([]byte, 10)...)
	return buf
}

func kuboDirectoryListing() []byte {
	return []byte(`<!DOCTYPE html><html><head><title>/ipfs/QmFoo</title></head>
<body><h1>Index of /ipfs/QmFoo</h1>
<table><tr><td class="ipfs-hash">QmBar</td></tr></table></body></html>`)
}

// modernKuboListingWindow is the first probe window of a Kubo 0.13+
// dir-index-html listing as served by ipfs.io and dweb.link: the meta
// description sits at ~byte 70 and the path-as-title after the inlined
// favicon, while every "Index of" heading and class="ipfs-hash" cell sits
// hundreds of KB later. Detection must succeed on this head alone.
func modernKuboListingWindow() []byte {
	return []byte(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="description" content="A directory of content-addressed files hosted on IPFS.">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <link rel="shortcut icon" href="data:image/x-icon;base64,AAABAAEAEBAAAAEAIABoBAAAFgAAACgAAAAQAAAAIAAAACAAAAABACAAAAAAAAAEAAA=">
  <title>/ipfs/QmdmQXB2mzChmMeKY47C43LxUdg1NDJ5MWcKMKxDu7RgQm/</title>
  <style>.flex{display:flex}.flex-wrap{flex-flow:wrap}.nowrap{white-space:nowrap}</style>
</head>
<body><main><header class="flex flex-wrap"><div><strong>`)
}

func TestContentValidator_Validate(t *testing.T) {
	v := NewContentValidator(testProbeMaxBytes, []string{"gateway time-out", "504 Gateway Time-out"})

	tests := []struct {
		name        string
		declared    string
		body        []byte
		totalLength int64
		wantOK      bool
		wantReason  Reason
		wantSniffed string
	}{
		{name: "valid PNG declared as PNG", declared: "image/png", body: minimalPNG(100, 100), totalLength: -1, wantOK: true, wantSniffed: "image/png"},
		{name: "valid GIF", declared: "image/gif", body: minimalGIF(10, 10), totalLength: -1, wantOK: true, wantSniffed: "image/gif"},
		{name: "valid WebP", declared: "image/webp", body: minimalWebP(), totalLength: -1, wantOK: true, wantSniffed: "image/webp"},
		{name: "valid JPEG", declared: "image/jpeg", body: minimalJPEG(), totalLength: -1, wantOK: true, wantSniffed: "image/jpeg"},
		{name: "HTML artwork declared as HTML is never flagged", declared: "text/html; charset=utf-8", body: []byte("<!DOCTYPE html><html><body><canvas></canvas></body></html>"), totalLength: -1, wantOK: true, wantSniffed: "text/html"},
		{name: "declared PNG sniffed as different image class errs healthy", declared: "image/png", body: minimalGIF(5, 5), totalLength: -1, wantOK: true, wantSniffed: "image/gif"},
		{name: "SVG content", declared: "image/svg+xml", body: []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`), totalLength: -1, wantOK: true, wantSniffed: "image/svg+xml"},
		{name: "empty declared type with valid body errs healthy", declared: "", body: minimalPNG(64, 64), totalLength: -1, wantOK: true, wantSniffed: "image/png"},
		{name: "full 32KB window read of a larger resource is not truncated", declared: "image/png", body: append(minimalPNG(2048, 2048), make([]byte, testProbeMaxBytes-33)...)[:testProbeMaxBytes], totalLength: 10 << 20, wantOK: true, wantSniffed: "image/png"},

		{name: "empty body", declared: "image/png", body: nil, totalLength: 0, wantOK: false, wantReason: ReasonZeroLength},
		{name: "body shorter than declared length", declared: "image/png", body: minimalPNG(100, 100), totalLength: 500_000, wantOK: false, wantReason: ReasonTruncated},
		{name: "Kubo directory listing (feral-file#3482)", declared: "text/html", body: kuboDirectoryListing(), totalLength: -1, wantOK: false, wantReason: ReasonDirectoryListing, wantSniffed: "text/html"},
		{name: "configured known error page marker", declared: "text/html", body: []byte("<html><head><title>504 Gateway Time-out</title></head><body>nginx</body></html>"), totalLength: -1, wantOK: false, wantReason: ReasonKnownErrorPage, wantSniffed: "text/html"},
		{name: "declared image but HTML error body (bug #76 class)", declared: "image/png", body: []byte("<!DOCTYPE html><html><body>Something went wrong</body></html>"), totalLength: -1, wantOK: false, wantReason: ReasonTypeMismatch, wantSniffed: "text/html"},
		{name: "declared video but plain text body", declared: "video/mp4", body: []byte("this CID is not pinned anymore, sorry"), totalLength: -1, wantOK: false, wantReason: ReasonTypeMismatch, wantSniffed: "text/plain"},
		{name: "PNG signature with corrupt IHDR", declared: "image/png", body: append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, bytes.Repeat([]byte{0xAB}, 32)...), totalLength: -1, wantOK: false, wantReason: ReasonContainerInvalid, wantSniffed: "image/png"},
		{name: "PNG with zero dimensions", declared: "image/png", body: minimalPNG(0, 100), totalLength: -1, wantOK: false, wantReason: ReasonContainerInvalid},
		{name: "GIF with zero dimensions", declared: "image/gif", body: minimalGIF(0, 0), totalLength: -1, wantOK: false, wantReason: ReasonContainerInvalid},
		{name: "JPEG with corrupted segment structure", declared: "image/jpeg", body: []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x04, 0x01, 0x02, 0x00, 0x00, 0x00, 0x00}, totalLength: -1, wantOK: false, wantReason: ReasonContainerInvalid, wantSniffed: "image/jpeg"},

		{name: "PNG prefix too short to judge", declared: "image/png", body: []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00}, totalLength: -1, wantOK: true},
		{name: "unknown binary type is not judged", declared: "application/octet-stream", body: bytes.Repeat([]byte{0x42}, 64), totalLength: -1, wantOK: true},

		{name: "declared image but JSON error body", declared: "image/png", body: []byte(`{"error":"upstream fetch failed","status":200,"detail":"cid not found"}`), totalLength: -1, wantOK: false, wantReason: ReasonTypeMismatch},
		{name: "declared video but JSON error body", declared: "video/mp4", body: []byte(`{"error":"gateway timeout"}`), totalLength: -1, wantOK: false, wantReason: ReasonTypeMismatch},
		{name: "declared JSON with JSON body is never flagged", declared: "application/json", body: []byte(`{"name":"token #1"}`), totalLength: -1, wantOK: true},

		{name: "SVG with large preamble before the root element", declared: "image/svg+xml", body: append([]byte("<!--"+strings.Repeat("license ", 512)+"-->\n"), []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect width="10" height="10"/></svg>`)...), totalLength: -1, wantOK: true},
		{name: "SVG with XML declaration and large preamble", declared: "image/svg+xml", body: append([]byte(`<?xml version="1.0" encoding="UTF-8"?><!--`+strings.Repeat("c", 4000)+"-->"), []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`)...), totalLength: -1, wantOK: true},
		{name: "ASCII PNM is a text image format", declared: "image/x-portable-anymap", body: []byte("P3\n2 2\n255\n255 0 0  0 255 0\n0 0 255  255 255 255\n"), totalLength: -1, wantOK: true},
		{name: "XBM is C source text", declared: "image/x-xbitmap", body: []byte("#define img_width 8\n#define img_height 8\nstatic unsigned char img_bits[] = { 0xFF };"), totalLength: -1, wantOK: true},
		{name: "HLS playlist is a text audio format", declared: "audio/x-mpegurl", body: []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:10,\nseg0.ts\n"), totalLength: -1, wantOK: true},

		{name: "modern Kubo listing detected from the probe window alone", declared: "text/html", body: modernKuboListingWindow(), totalLength: -1, wantOK: false, wantReason: ReasonDirectoryListing},
		{name: "HTML artwork using an ipfs-hash class is not a listing", declared: "text/html", body: []byte(`<!DOCTYPE html><html><head><title>chain study #4</title></head><body><div class="ipfs-hash">QmSeed42</div><script>render()</script></body></html>`), totalLength: -1, wantOK: true},
		{name: "HTML artwork mentioning index of /ipfs is not a listing", declared: "text/html", body: []byte(`<!DOCTYPE html><html><head><title>archive piece</title></head><body><p>sourced from an index of /ipfs snapshots</p></body></html>`), totalLength: -1, wantOK: true},
		{name: "path-as-title alone is not a listing", declared: "text/html", body: []byte(`<!DOCTYPE html><html><head><title>/ipfs/QmFoo/</title></head><body><canvas></canvas></body></html>`), totalLength: -1, wantOK: true},
		{
			name:     "PNG with invalid IHDR chunk length",
			declared: "image/png",
			body: append(append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A},
				0x00, 0x00, 0x00, 0x00),
				[]byte("IHDR\x00\x00\x00\x64\x00\x00\x00\x64\x08\x02\x00\x00\x00")...),
			totalLength: -1,
			wantOK:      false,
			wantReason:  ReasonContainerInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := v.Validate(tt.declared, tt.body, tt.totalLength)
			assert.Equal(t, tt.wantOK, got.OK)
			if !tt.wantOK {
				assert.Equal(t, tt.wantReason, got.Reason)
				assert.NotEmpty(t, got.Detail)
			} else {
				assert.Empty(t, string(got.Reason))
				assert.Empty(t, got.Detail)
			}
			if tt.wantSniffed != "" {
				assert.Equal(t, tt.wantSniffed, got.Sniffed)
			}
		})
	}
}

func TestContentValidator_MarkerNormalization(t *testing.T) {
	v := NewContentValidator(testProbeMaxBytes, []string{"  BLOCKED CONTENT  ", ""})
	got := v.Validate("text/html", []byte("<html><body>blocked content</body></html>"), -1)
	assert.False(t, got.OK)
	assert.Equal(t, ReasonKnownErrorPage, got.Reason)
}

// Every Reason constant must map to a category: an unmapped reason would
// reach the app as an empty label.
func TestCategoryOf_CoversEveryReason(t *testing.T) {
	reasons := []Reason{
		ReasonHTTPStatus, ReasonDNS, ReasonTypeMismatch, ReasonContainerInvalid,
		ReasonDirectoryListing, ReasonKnownErrorPage, ReasonZeroLength, ReasonTruncated,
		ReasonInvalidURL, ReasonUnsupportedScheme, ReasonTransport, ReasonDataURIInvalid,
		ReasonRenderDetect, ReasonRenderLoad, ReasonRenderDecode, ReasonRenderWatchdog,
		ReasonRenderExhausted, ReasonWebGLLost, ReasonRenderBlank, ReasonRenderKnownBad,
	}
	for _, r := range reasons {
		assert.NotEmpty(t, CategoryOf(r, 404), "reason %q has no category", r)
	}
	assert.Equal(t, CategorySourceMissing, CategoryOf(ReasonHTTPStatus, 404))
	assert.Equal(t, CategorySourceMissing, CategoryOf(ReasonHTTPStatus, 410))
	assert.Equal(t, CategorySourceBlocked, CategoryOf(ReasonHTTPStatus, 403))
	assert.Equal(t, CategoryDeviceLimit, CategoryOf(ReasonWebGLLost, 0))
	assert.Equal(t, CategoryRenderBlank, CategoryOf(ReasonRenderBlank, 0))
	assert.Empty(t, CategoryOf(Reason("made_up"), 0))
}
