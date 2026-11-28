package offlinecache

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"maps"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/mr-tron/base58"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func TestImportCapsule(t *testing.T) {
	const source = "https://example.org/artwork"
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer := "did:key:z" + base58.Encode(append([]byte{0xed, 1}, public...))
	store := NewStore(t.TempDir(), wrapper.NewOS(), wrapper.NewJSON(), zap.NewNop())
	cache := NewService(store, nil, nil, nil, wrapper.NewJSON(), 0, 0, nil, AdmissionOptions{}, zap.NewNop()).(*service)
	cache.started.Store(true)

	archive, signature := capsuleFixture(t, source, "<p>first</p>", signer, private)
	tampered := append([]byte(nil), archive...)
	tampered[len(tampered)-1] ^= 1
	_, err = cache.ImportCapsule(t.Context(), tampered, signature, signer)
	require.Error(t, err)
	key := SourceKey(source)
	_, err = store.LoadItem(key)
	require.ErrorIs(t, err, ErrItemNotFound)

	raw, err := cache.ImportCapsule(t.Context(), archive, signature, signer)
	require.NoError(t, err)
	require.Contains(t, string(raw), source)
	record, err := store.LoadItem(key)
	require.NoError(t, err)
	require.True(t, nativeReplayRecordReady(record))
	require.Equal(t, capsuleDigest(archive), record.Capsule.ArchiveSHA256)
	require.Equal(t, capsuleDigest([]byte("<p>first</p>")), record.Capsule.EngineSHA256)
	require.Equal(t, "155.0.8059.84", record.Capsule.EngineVersion)
	require.JSONEq(t, string(signature), string(record.Capsule.Signature))
	require.Len(t, record.Resources, 1)
	require.Equal(t, StateReady, cache.state[key])
	_, _, err = store.GC()
	require.NoError(t, err)
	saved, err := store.ReadBlob(record.Capsule.ArchiveSHA256)
	require.NoError(t, err)
	require.Equal(t, archive, saved)

	at := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	require.NoError(t, store.TouchItem(key, at))
	_, err = cache.ImportCapsule(t.Context(), archive, signature, signer)
	require.NoError(t, err)
	other, otherSignature := capsuleFixture(t, source, "<p>different</p>", signer, private)
	_, err = cache.ImportCapsule(t.Context(), other, otherSignature, signer)
	require.ErrorContains(t, err, "different offline capture")
	record, err = store.LoadItem(key)
	require.NoError(t, err)
	require.Equal(t, at, record.LastPlayedAt)
	require.Equal(t, capsuleDigest(archive), record.Capsule.ArchiveSHA256)

	keys := map[string]bool{key: true}
	_, _, err = cache.reserveForClear(keys)
	require.NoError(t, err)
	_, err = cache.ImportCapsule(t.Context(), archive, signature, signer)
	require.ErrorIs(t, err, ErrClearedDuringDownload)
	_, err = store.DeleteItem(key)
	require.NoError(t, err)
	cache.finishClear(keys)
	_, err = store.LoadItem(key)
	require.ErrorIs(t, err, ErrItemNotFound)
	_, tracked := cache.state[key]
	require.False(t, tracked)
}

func capsuleFixture(t *testing.T, source, artwork, signer string, private ed25519.PrivateKey) ([]byte, json.RawMessage) {
	t.Helper()
	digest := capsuleDigest([]byte(artwork))
	var frame bytes.Buffer
	require.NoError(t, png.Encode(&frame, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	frameDigest := capsuleDigest(append([]byte("1x1:RGB8:"), 0, 0, 0))
	frameHash := `{"phash":"0x0000000000000000","sha256":"` + frameDigest + `"}`
	members := map[string][]byte{
		"assets/" + digest: []byte(artwork),
		"playlist.json": []byte(`{"dpVersion":"1.1.0","title":"Capsule fixture","items":[{"source":"` + source +
			`","repro":{"assetsSHA256":["` + digest + `"],"frameHash":` + frameHash + `}}]}`),
		"meta/capture/resources.json": []byte(`{"entry":"` + source +
			`","coverage":{"complete":true},"resources":[{"method":"GET","url":"` + source +
			`","status":200,"contentType":"text/html","sha256":"` + digest + `"}]}`),
		"meta/engine/chromium.json": []byte(`{"version":"155.0.8059.84","imageRef":{"sha256":"` + digest + `"},"viewport":{"width":1,"height":1}}`),
		"meta/render/reference.json": []byte(`{"mode":"replay","entry":"` + source +
			`","assets":{"` + digest + `":"` + digest + `"},"frame":` + frameHash + `}`),
		"meta/render/reference.png": frame.Bytes(),
	}
	var archive bytes.Buffer
	compressor, err := zstd.NewWriter(&archive)
	require.NoError(t, err)
	writer := tar.NewWriter(compressor)
	for name, body := range members {
		require.NoError(t, writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}))
		_, err := writer.Write(body)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	require.NoError(t, compressor.Close())

	id := capsuleDigest(archive.Bytes())
	payload := "sha256:" + id
	signature, err := json.Marshal(map[string]any{"capsuleId": id, "signatures": []map[string]string{{
		"kid": signer, "alg": "ed25519", "payload_hash": payload,
		"sig": base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(payload))),
	}}})
	require.NoError(t, err)
	return archive.Bytes(), signature
}

func TestCapsuleReferenceWithoutEngineImage(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer := "did:key:z" + base58.Encode(append([]byte{0xed, 1}, public...))
	archive, _ := capsuleFixture(t, "https://example.org/artwork", "<p>work</p>", signer, private)
	files, err := readCapsuleFiles(archive)
	require.NoError(t, err)
	const manifest = "meta/engine/chromium.json"
	var engine map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(files[manifest], &engine))
	engine["imageRef"] = json.RawMessage("null")
	files[manifest], err = json.Marshal(engine)
	require.NoError(t, err)
	_, record, _, err := capsuleReplay(capsuleDigest(archive), files)
	require.NoError(t, err)
	require.Empty(t, record.Capsule.EngineSHA256)

	engine["imageRef"] = json.RawMessage(`{"sha256":"invalid"}`)
	files[manifest], err = json.Marshal(engine)
	require.NoError(t, err)
	_, _, _, err = capsuleReplay(capsuleDigest(archive), files)
	require.ErrorContains(t, err, "invalid capsule engine image digest")
}

func TestCapsuleNamedFiles(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer := "did:key:z" + base58.Encode(append([]byte{0xed, 1}, public...))
	archive, _ := capsuleFixture(t, "https://example.org/token", "<p>named file</p>", signer, private)
	files, err := readCapsuleFiles(archive)
	require.NoError(t, err)
	var ref map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(files["meta/render/reference.json"], &ref))
	digest := capsuleDigest([]byte("<p>named file</p>"))
	delete(files, "meta/capture/resources.json")
	files["assets/nested/index.html"] = files["assets/"+digest]
	delete(files, "assets/"+digest)
	ref["mode"] = json.RawMessage(`"files"`)
	ref["entry"] = json.RawMessage(`"nested/index.html?fxhash=abc"`)
	ref["assets"], err = json.Marshal(map[string]string{"nested/index.html": digest})
	require.NoError(t, err)
	files["meta/render/reference.json"], err = json.Marshal(ref)
	require.NoError(t, err)
	id := capsuleDigest(archive)
	raw, record, blobs, err := capsuleReplay(id, files)
	require.NoError(t, err)
	want := "https://" + id[:32] + "." + id[32:] + ".capsule.invalid/nested/index.html?fxhash=abc"
	require.Equal(t, want, record.Entry)
	require.Equal(t, "https://example.org/token", record.Item.Source)
	require.True(t, record.Capsule.Files)
	require.Contains(t, string(raw), record.Item.Source)
	require.Len(t, record.Resources, 2)
	require.Equal(t, []byte("<p>named file</p>"), blobs[digest])
	require.Contains(t, string(files["playlist.json"]), "https://example.org/token")
	for _, bad := range []string{"../x", "%2fescape", "a\\b", "/absolute", "bad#fragment"} {
		_, err := capsuleFileCapture(id, bad, map[string]string{bad: digest})
		require.Error(t, err, bad)
	}
}

func TestCapsulePlaylistSourceRejectsMalformedItems(t *testing.T) {
	for _, raw := range []string{`{"items":[]}`, `{"items":[null]}`, `{"ITEMS":[{}]}`, `{"items":[{},{}]}`} {
		if _, err := capsulePlaylistSource(json.RawMessage(raw), "https://example.org"); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestCapsuleReplayRedirectChain(t *testing.T) {
	const source = "https://example.org/start"
	const target = "https://example.org/artwork"
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer := "did:key:z" + base58.Encode(append([]byte{0xed, 1}, public...))
	archive, _ := capsuleFixture(t, source, "<p>edition</p>", signer, private)
	files, err := readCapsuleFiles(archive)
	require.NoError(t, err)
	const manifest = "meta/capture/resources.json"
	var capture capsuleCapture
	require.NoError(t, json.Unmarshal(files[manifest], &capture))
	artwork := capture.Resources[0]
	artwork.URL = target
	redirect := Resource{URL: source, Status: 302, RedirectTo: target, Headers: map[string]string{"Cache-Control": "public", "Content-Length": "0"}}
	empty := Resource{URL: target + "/empty", Status: 204}
	for _, tc := range []struct {
		name      string
		responses []Resource
		wantError string
	}{
		{"complete", []Resource{redirect, artwork, empty}, ""},
		{"missing destination", []Resource{redirect}, "incomplete redirect chain"},
		{"duplicate response", []Resource{redirect, artwork, artwork}, "invalid or unsupported"},
		{"empty artwork", []Resource{redirect, {URL: target, Status: 200}}, "does not contain artwork bytes"},
		{"inconsistent redirect status", []Resource{{URL: source, Status: 200, RedirectTo: target}, artwork}, "invalid capsule resource redirect"},
		{"missing body", []Resource{redirect, {URL: target, Status: 200, SHA256: capsuleDigest([]byte("other"))}}, "unknown asset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture.Resources = tc.responses
			payload, err := json.Marshal(capture)
			require.NoError(t, err)
			files[manifest] = payload
			_, record, blobs, err := capsuleReplay(capsuleDigest(archive), files)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				require.Nil(t, record)
				return
			}
			require.NoError(t, err)
			require.Equal(t, source, record.Entry)
			require.Equal(t, target, record.Resources[0].RedirectTo)
			require.Contains(t, record.Resources[0].HeaderFields, HeaderField{Name: "Location", Value: target})
			require.NotContains(t, record.Resources[0].HeaderFields, HeaderField{Name: "Content-Length", Value: "0"})
			require.Equal(t, []byte("<p>edition</p>"), blobs[artwork.SHA256])
			require.Equal(t, capsuleDigest(nil), record.Resources[2].SHA256)
		})
	}
}

func TestCapsuleReplayRejectsIncompleteEditions(t *testing.T) {
	const source = "https://example.org/artwork"
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	archive, _ := capsuleFixture(t, source, "artwork", "did:key:z"+base58.Encode(append([]byte{0xed, 1}, pub...)), priv)
	original, err := readCapsuleFiles(archive)
	require.NoError(t, err)
	const playlist = "playlist.json"
	const reference = "meta/render/reference.json"
	const engine = "meta/engine/chromium.json"
	const capture = "meta/capture/resources.json"
	for _, tc := range []struct {
		name, file, old, new, reason string
	}{
		{"no works", playlist, "", `{"items":[]}`, "static playlist"},
		{"future work", playlist, `"source"`, `"displayAt":"2030-01-01T00:00:00Z","source"`, "directly playable"},
		{"missing reference", reference, "", `{}`, "invalid capsule replay reference"},
		{"mixed capture modes", reference, `"replay"`, `"files"`, "cannot also claim"},
		{"unknown capture mode", reference, `"replay"`, `"other"`, "unsupported capsule reference mode"},
		{"incomplete capture", capture, `"complete":true`, `"complete":false`, "complete original-URL"},
		{"different origin", capture, source, "https://example.org/different", "complete original-URL"},
		{"missing engine version", engine, "155.0.8059.84", "", "invalid capsule engine pin"},
		{"different engine versions", playlist, `"assetsSHA256"`, `"engineVersion":{"chromium":"other"},"assetsSHA256"`, "version claims disagree"},
		{"different frame hashes", reference, "0x0000000000000000", "0x0000000000000001", "frame claims disagree"},
		{"wrong frame dimensions", engine, `"width":1`, `"width":2`, "reference frame checksum mismatch"},
		{"missing reference frame", "meta/render/reference.png", "", "", "reference frame checksum mismatch"},
		{"wrong asset bytes", "assets/" + capsuleDigest([]byte("artwork")), "", "other", "asset checksum mismatch"},
		{"unlisted asset", "assets/extra", "", "other", "unlisted asset"},
		{"playlist asset mismatch", playlist, capsuleDigest([]byte("artwork")), capsuleDigest(nil), "playlist assets disagree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := maps.Clone(original)
			if tc.old == "" {
				files[tc.file] = []byte(tc.new)
			} else {
				require.Contains(t, string(files[tc.file]), tc.old)
				files[tc.file] = bytes.Replace(files[tc.file], []byte(tc.old), []byte(tc.new), 1)
			}
			_, record, _, err := capsuleReplay(capsuleDigest(archive), files)
			require.ErrorContains(t, err, tc.reason)
			require.Nil(t, record)
		})
	}
}

func TestCapsuleImportAvailabilityAndStorage(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer := "did:key:z" + base58.Encode(append([]byte{0xed, 1}, pub...))
	const source = "https://example.org/sculpture"
	archive, signature := capsuleFixture(t, source, "edition", signer, priv)
	store := NewStore(t.TempDir(), wrapper.NewOS(), wrapper.NewJSON(), zap.NewNop())
	cache := NewService(store, nil, nil, nil, wrapper.NewJSON(), 0, 0, nil, AdmissionOptions{}, zap.NewNop()).(*service)
	_, err = cache.ImportCapsule(t.Context(), archive, signature, signer)
	require.ErrorIs(t, err, ErrServiceNotStarted)
	cache.started.Store(true)
	_, err = cache.ImportCapsule(t.Context(), nil, signature, signer)
	require.ErrorContains(t, err, "compressed size")
	key := SourceKey(source)
	cache.state[key] = StateDownloading
	_, err = cache.ImportCapsule(t.Context(), archive, signature, signer)
	require.ErrorIs(t, err, ErrItemBusy)
	delete(cache.state, key)
	cache.maxDiskBytes = 1
	_, err = cache.ImportCapsule(t.Context(), archive, signature, signer)
	require.ErrorIs(t, err, errCacheDiskPressure)
	_, err = store.LoadItem(key)
	require.ErrorIs(t, err, ErrItemNotFound)
	cache.maxDiskBytes = 32 << 20
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = cache.ImportCapsule(ctx, archive, signature, signer)
	require.ErrorIs(t, err, context.Canceled)
	_, err = cache.ImportCapsule(t.Context(), archive, signature, signer)
	require.NoError(t, err)
}

func TestCapsuleSignatureRequiresMatchingSigner(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer := "did:key:z" + base58.Encode(append([]byte{0xed, 1}, pub...))
	archive, sidecar := capsuleFixture(t, "https://example.org/edition", "frame", signer, priv)
	id := capsuleDigest(archive)
	validSig := ed25519.Sign(priv, []byte("sha256:"+id))
	wrongSig := ed25519.Sign(priv, []byte("a different edition"))
	invalidSidecar := bytes.Replace(sidecar, []byte(base64.RawURLEncoding.EncodeToString(validSig)), []byte(base64.RawURLEncoding.EncodeToString(wrongSig)), 1)
	for _, tc := range []struct {
		name, signer string
		sidecar      json.RawMessage
		reason       string
	}{
		{"invalid signature", signer, invalidSidecar, "does not match"},
		{"missing signer", "", sidecar, "requires an Ed25519"},
		{"invalid key", "did:key:z0", sidecar, "invalid capsule signer"},
		{"unknown signature algorithm", signer, bytes.ReplaceAll(sidecar, []byte("ed25519"), []byte("unknown")), "does not match"},
		{"different archive", signer, bytes.ReplaceAll(sidecar, []byte(id), []byte(capsuleDigest(nil))), "does not name"},
		{"oversize signature", signer, make([]byte, MaxCapsuleSignatureBytes+1), "size limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorContains(t, verifyCapsuleSignature(id, tc.sidecar, tc.signer), tc.reason)
		})
	}
}

func TestCapsuleArchiveMembers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []*tar.Header
		want    string
	}{
		{"directory and member", []*tar.Header{{Name: "assets/", Typeflag: tar.TypeDir}, {Name: "assets/art", Typeflag: tar.TypeReg}}, ""},
		{"duplicate name", []*tar.Header{{Name: "playlist.json", Typeflag: tar.TypeReg}, {Name: "playlist.json", Typeflag: tar.TypeReg}}, "duplicate member"},
		{"unsupported member", []*tar.Header{{Name: "alternative", Typeflag: tar.TypeLink, Linkname: "art"}}, "non-regular member"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var archive bytes.Buffer
			enc, err := zstd.NewWriter(&archive)
			require.NoError(t, err)
			w := tar.NewWriter(enc)
			for _, header := range tc.headers {
				require.NoError(t, w.WriteHeader(header))
			}
			require.NoError(t, w.Close())
			require.NoError(t, enc.Close())
			files, err := readCapsuleFiles(archive.Bytes())
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
				require.Nil(t, files)
				return
			}
			require.NoError(t, err)
			require.Len(t, files, 1)
			require.Contains(t, files, "assets/art")
		})
	}
	_, err := readCapsuleFiles([]byte("incomplete archive"))
	require.Error(t, err)
}

func TestCapsuleResponseHeaders(t *testing.T) {
	for _, tc := range []struct {
		name   string
		r      Resource
		reason string
	}{
		{"invalid name", Resource{Headers: map[string]string{"bad name": "value"}}, "invalid capsule resource header"},
		{"duplicate field", Resource{Headers: map[string]string{"Vary": "Origin", "vary": "Accept"}}, "duplicate capsule resource header"},
		{"invalid content type", Resource{ContentType: "invalid\nvalue"}, "invalid capsule content type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resource := tc.r
			require.ErrorContains(t, capsuleHeaderFields(&resource), tc.reason)
		})
	}
}

func TestNamedCapsuleUsesMountForNonHTTPSource(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer := "did:key:z" + base58.Encode(append([]byte{0xed, 1}, pub...))
	archive, _ := capsuleFixture(t, "ipfs://fixture", "edition", signer, priv)
	files, err := readCapsuleFiles(archive)
	require.NoError(t, err)
	delete(files, "meta/capture/resources.json")
	digest := capsuleDigest([]byte("edition"))
	files["assets/start.custom"] = files["assets/"+digest]
	delete(files, "assets/"+digest)
	const path = "meta/render/reference.json"
	var ref map[string]any
	require.NoError(t, json.Unmarshal(files[path], &ref))
	ref["mode"] = "files"
	ref["entry"] = "start.custom"
	ref["assets"] = map[string]string{"start.custom": digest}
	files[path], err = json.Marshal(ref)
	require.NoError(t, err)
	id := capsuleDigest(archive)
	raw, rec, _, err := capsuleReplay(id, files)
	require.NoError(t, err)
	require.Equal(t, rec.Entry, rec.Item.Source)
	require.Contains(t, rec.Entry, ".capsule.invalid/start.custom")
	require.Contains(t, string(raw), rec.Entry)
	require.Contains(t, string(files["playlist.json"]), "ipfs://fixture")
	require.Equal(t, "application/octet-stream", rec.Resources[0].ContentType)
}

type capsuleRecordWriteFailure struct {
	Store
	err error
}

func (s capsuleRecordWriteFailure) SaveItem(*ItemRecord) error { return s.err }

func TestCapsuleImportCleansUpAfterFailedRecordWrite(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer := "did:key:z" + base58.Encode(append([]byte{0xed, 1}, pub...))
	const source = "https://example.org/capsule"
	archive, signature := capsuleFixture(t, source, "edition", signer, priv)
	store := NewStore(t.TempDir(), wrapper.NewOS(), wrapper.NewJSON(), zap.NewNop())
	writeError := errors.New("record write failed")
	s := NewService(capsuleRecordWriteFailure{Store: store, err: writeError}, nil, nil, nil, wrapper.NewJSON(), 0, 0, nil, AdmissionOptions{}, zap.NewNop()).(*service)
	s.started.Store(true)
	_, err = s.ImportCapsule(t.Context(), archive, signature, signer)
	require.ErrorIs(t, err, writeError)
	_, err = store.LoadItem(SourceKey(source))
	require.ErrorIs(t, err, ErrItemNotFound)
	for _, id := range []string{capsuleDigest(archive), capsuleDigest([]byte("edition"))} {
		_, err = store.BlobSize(id)
		require.Error(t, err)
	}
	require.NotEqual(t, StateReady, s.state[SourceKey(source)])
}
