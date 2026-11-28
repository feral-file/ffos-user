package offlinecache

import (
	"archive/tar"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
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
