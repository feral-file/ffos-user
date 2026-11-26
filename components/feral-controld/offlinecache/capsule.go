package offlinecache

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"maps"
	"mime"
	"net/url"
	"path"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/display-protocol/dp1-go/extension/contentrating"
	"github.com/display-protocol/dp1-go/playlist"
	"github.com/klauspost/compress/zstd"
	"github.com/mr-tron/base58"
	"golang.org/x/net/http/httpguts"
)

const (
	// Leave room in the 4 MiB LAN command body for the archive, signed claims, and sealed envelope encodings.
	MaxCapsuleBytes          = 1536 << 10
	maxCapsuleExpanded       = 32 << 20
	maxCapsuleMembers        = 512
	MaxCapsuleSignatureBytes = 8 << 10
)

type CapsuleRecord struct {
	EngineSHA256  string          `json:"engineSHA256,omitempty"`
	EngineVersion string          `json:"engineVersion"`
	Files         bool            `json:"files,omitempty"`
	ArchiveSHA256 string          `json:"archiveSHA256"`
	Signature     json.RawMessage `json:"signature"`
}

type capsuleCapture struct {
	Entry      string     `json:"entry"`
	Resources  []Resource `json:"resources"`
	Coverage   Coverage   `json:"coverage"`
	CapturedAt time.Time  `json:"capturedAt"`
}

func capsuleDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func verifyCapsuleSignature(id string, sidecar json.RawMessage, signer string) error {
	if len(sidecar) > MaxCapsuleSignatureBytes || len(signer) > 128 {
		return errors.New("capsule signature exceeds its size limit")
	}
	key, ok := strings.CutPrefix(signer, "did:key:z")
	if !ok {
		return errors.New("capsule requires an Ed25519 did:key signer")
	}
	public, err := base58.Decode(key)
	if err != nil || len(public) != ed25519.PublicKeySize+2 || public[0] != 0xed || public[1] != 1 {
		return errors.New("invalid capsule signer")
	}
	var doc struct {
		CapsuleID  string `json:"capsuleId"`
		Signatures []struct {
			Key         string `json:"kid"`
			Algorithm   string `json:"alg"`
			PayloadHash string `json:"payload_hash"`
			Signature   string `json:"sig"`
		} `json:"signatures"`
	}
	if err := json.Unmarshal(sidecar, &doc); err != nil || doc.CapsuleID != id {
		return errors.New("capsule signature does not name the received archive")
	}
	payload := "sha256:" + id
	for _, signature := range doc.Signatures {
		if signature.Key != signer || signature.Algorithm != "ed25519" || signature.PayloadHash != payload {
			continue
		}
		value, err := base64.RawURLEncoding.DecodeString(signature.Signature)
		if err == nil && ed25519.Verify(public[2:], []byte(payload), value) {
			return nil
		}
	}
	return errors.New("capsule signature does not match the expected signer")
}

func readCapsuleFiles(archive []byte) (map[string][]byte, error) {
	decoder, err := zstd.NewReader(bytes.NewReader(archive), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(maxCapsuleExpanded))
	if err != nil {
		return nil, err
	}
	defer decoder.Close()

	limited := &io.LimitedReader{R: decoder, N: maxCapsuleExpanded + 1}
	reader := tar.NewReader(limited)
	files := make(map[string][]byte)
	for count := 0; ; count++ {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if count == maxCapsuleMembers || header.Size >= limited.N {
			return nil, errors.New("capsule archive exceeds the member or expanded size limit")
		}
		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
			// Only read named members. Nothing is extracted to a filesystem.
		default:
			return nil, errors.New("capsule archive contains a non-regular member")
		}
		if _, exists := files[header.Name]; exists {
			return nil, errors.New("capsule archive contains a duplicate member")
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		files[header.Name] = body
	}
	if _, err := io.Copy(io.Discard, limited); err != nil {
		return nil, err
	}
	if limited.N == 0 {
		return nil, errors.New("capsule archive exceeds the expanded size limit")
	}
	return files, nil
}

func capsuleHTTPURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" &&
		u.User == nil && u.Fragment == "" && len(value) <= MaxSourceURLBytes
}

func capsuleHeaderFields(resource *Resource) error {
	// The expected signer vouches for these archived responses. Version 2
	// selects the native replay's explicit header list, including any
	// policy headers carried by the capsule.
	if resource.RedirectTo != "" && (resource.Status < 300 || resource.Status >= 400 ||
		!capsuleHTTPURL(resource.RedirectTo)) {
		return errors.New("invalid capsule resource redirect")
	}
	headers := make(map[string]string, len(resource.Headers)+2)
	for key, value := range resource.Headers {
		name := strings.ToLower(key)
		if !httpguts.ValidHeaderFieldName(key) || !httpguts.ValidHeaderFieldValue(value) {
			return errors.New("invalid capsule resource header")
		}
		if _, duplicate := headers[name]; duplicate {
			return errors.New("duplicate capsule resource header")
		}
		switch name {
		case "connection", "content-encoding", "content-length", "content-range", "keep-alive", "location",
			"proxy-authenticate", "proxy-authorization", "set-cookie", "te", "trailer", "transfer-encoding", "upgrade":
			continue
		}
		headers[name] = value
	}
	if _, ok := headers["content-type"]; !ok && resource.ContentType != "" {
		if !httpguts.ValidHeaderFieldValue(resource.ContentType) {
			return errors.New("invalid capsule content type")
		}
		headers["content-type"] = resource.ContentType
	}
	if resource.RedirectTo != "" {
		headers["location"] = resource.RedirectTo
	}
	fields, ok := browserReplayHeaders(headers)
	if !ok {
		return errors.New("unsupported capsule browser response headers")
	}
	resource.HeaderFields = fields
	resource.Headers = nil
	resource.BrowserPolicyVersion = 2
	resource.FreshUntil = time.Time{}
	return nil
}

func capsuleFrameDigest(body []byte, width, height int) (string, error) {
	config, err := png.DecodeConfig(bytes.NewReader(body))
	if err != nil || config.Width != width || config.Height != height ||
		width <= 0 || height <= 0 || width > 4096 || height > 4096 || width*height > 3840*2160 {
		return "", errors.New("invalid capsule reference frame dimensions")
	}
	frame, err := png.Decode(bytes.NewReader(body))
	if err != nil {
		return "", errors.New("invalid capsule reference PNG")
	}
	digest := sha256.New()
	_, _ = fmt.Fprintf(digest, "%dx%d:RGB8:", width, height)
	row := make([]byte, width*3)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			pixel := color.NRGBAModel.Convert(frame.At(x, y)).(color.NRGBA)
			row[x*3], row[x*3+1], row[x*3+2] = pixel.R, pixel.G, pixel.B
		}
		_, _ = digest.Write(row)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// A files-mode capsule has no HTTP origin or archived response headers. Give
// each signed archive its own unreachable origin. Relative links and the edition
// query work normally, without impersonating the token's source or a gateway.
func capsuleFileCapture(id, entry string, assets map[string]string) (capsuleCapture, error) {
	var capture capsuleCapture
	name, query, hasQuery := strings.Cut(entry, "?")
	if _, found := assets[name]; !found || strings.Contains(entry, "#") {
		return capture, errors.New("invalid capsule file entry")
	}
	host := id[:32] + "." + id[32:] + ".capsule.invalid"
	for _, filename := range slices.Sorted(maps.Keys(assets)) {
		if !fs.ValidPath(filename) || filename == "." || strings.ContainsAny(filename, "%?#:\\") || strings.IndexFunc(filename, unicode.IsControl) >= 0 {
			return capture, errors.New("invalid capsule asset path")
		}
		u := url.URL{Scheme: "https", Host: host, Path: "/" + filename}
		mimeType := mime.TypeByExtension(path.Ext(filename))
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		resource := Resource{Method: "GET", URL: u.String(), Status: 200, ContentType: mimeType, SHA256: assets[filename]}
		capture.Resources = append(capture.Resources, resource)
		if filename == name {
			u.RawQuery = query
			u.ForceQuery = hasQuery
			capture.Entry = u.String()
			if hasQuery {
				resource.URL = capture.Entry
				capture.Resources = append(capture.Resources, resource)
			}
		}
	}
	capture.Coverage.Complete = true
	return capture, nil
}

func capsulePlaylistSource(raw json.RawMessage, source string) (json.RawMessage, error) {
	// Keep DP-1 extension fields in the runtime playlist; the signed original
	// (including its source and provenance) stays in the retained archive.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(fields["items"], &items); err != nil {
		return nil, err
	}
	if len(items) != 1 || items[0] == nil {
		return nil, errors.New("capsule requires one playlist item object")
	}
	items[0]["source"], _ = json.Marshal(source)
	fields["items"], _ = json.Marshal(items)
	return json.Marshal(fields)
}

func capsuleReplay(id string, files map[string][]byte) (json.RawMessage, *ItemRecord, map[string][]byte, error) {
	raw := json.RawMessage(files["playlist.json"])
	if len(raw) > 60<<10 {
		return nil, nil, nil, errors.New("capsule playlist exceeds the native item size limit")
	}
	var document struct {
		playlist.Playlist
		DynamicQueries []json.RawMessage `json:"dynamicQueries"`
	}
	if err := contentrating.ValidatePlaylistFragment(raw); err != nil {
		return nil, nil, nil, errors.New("invalid capsule playlist fragment")
	}
	if err := json.Unmarshal(raw, &document); err != nil || len(document.Items) != 1 ||
		document.DynamicQuery != nil || len(document.DynamicQueries) != 0 {
		return nil, nil, nil, errors.New("capsule requires a static playlist with one item")
	}
	item := document.Items[0]
	// Series resolution and future activation can replace the signed item
	// or defer it past the command that selected the offline capture.
	if item.DisplayAt != nil || (item.Provenance != nil && item.Provenance.Type == "seriesRegistry") {
		return nil, nil, nil, errors.New("capsule item must be directly playable")
	}
	var reference struct {
		Mode   string             `json:"mode"`
		Entry  string             `json:"entry"`
		Assets map[string]string  `json:"assets"`
		Frame  playlist.FrameHash `json:"frame"`
	}
	if err := json.Unmarshal(files["meta/render/reference.json"], &reference); err != nil ||
		len(reference.Assets) == 0 {
		return nil, nil, nil, errors.New("invalid capsule replay reference")
	}
	var capture capsuleCapture
	switch reference.Mode {
	case "files":
		if _, ok := files["meta/capture/resources.json"]; ok {
			return nil, nil, nil, errors.New("file capsule cannot also claim an HTTP capture")
		}
		var err error
		capture, err = capsuleFileCapture(id, reference.Entry, reference.Assets)
		if err != nil {
			return nil, nil, nil, err
		}
		// Retain a web edition's source as the cache lookup key. Non-web
		// sources enter the existing HTTP replay path through their mount URL.
		if !capsuleHTTPURL(item.Source) {
			item.Source = capture.Entry
			raw, err = capsulePlaylistSource(raw, item.Source)
			if err != nil {
				return nil, nil, nil, err
			}
		}
	case "replay":
		if err := json.Unmarshal(files["meta/capture/resources.json"], &capture); err != nil || !capsuleHTTPURL(item.Source) || capture.Entry != item.Source || reference.Entry != capture.Entry || !capture.Coverage.Complete || len(capture.Resources) > maxCapsuleMembers {
			return nil, nil, nil, errors.New("capsule requires a complete original-URL replay capture")
		}
	default:
		return nil, nil, nil, errors.New("unsupported capsule reference mode")
	}
	var engine struct {
		Version  string `json:"version"`
		ImageRef *struct {
			SHA256 string `json:"sha256"`
		} `json:"imageRef"`
		Viewport struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"viewport"`
	}
	if err := json.Unmarshal(files["meta/engine/chromium.json"], &engine); err != nil || engine.Version == "" {
		return nil, nil, nil, errors.New("invalid capsule engine pin")
	}
	var engineDigest string
	if engine.ImageRef != nil {
		engineDigest = engine.ImageRef.SHA256
		if !validSourceKey(engineDigest) {
			return nil, nil, nil, errors.New("invalid capsule engine image digest")
		}
	}
	if item.Repro != nil {
		if version := item.Repro.EngineVersion["chromium"]; version != "" && version != engine.Version {
			return nil, nil, nil, errors.New("capsule Chromium version claims disagree")
		}
	}
	if item.Repro == nil || item.Repro.FrameHash == nil || *item.Repro.FrameHash != reference.Frame ||
		len(reference.Frame.Phash) != 18 || !strings.HasPrefix(reference.Frame.Phash, "0x") {
		return nil, nil, nil, errors.New("capsule reference frame claims disagree")
	}
	if _, err := hex.DecodeString(reference.Frame.Phash[2:]); err != nil {
		return nil, nil, nil, errors.New("invalid capsule perceptual frame hash")
	}
	frameDigest, err := capsuleFrameDigest(files["meta/render/reference.png"], engine.Viewport.Width, engine.Viewport.Height)
	if err != nil || frameDigest != reference.Frame.SHA256 {
		return nil, nil, nil, errors.New("capsule reference frame checksum mismatch")
	}
	blobs := make(map[string][]byte, len(reference.Assets))
	for name, digest := range reference.Assets {
		body, found := files["assets/"+name]
		if !validSourceKey(digest) || (reference.Mode == "replay" && name != digest) || !found || capsuleDigest(body) != digest {
			return nil, nil, nil, errors.New("capsule asset checksum mismatch")
		}
		blobs[digest] = body
	}
	for name := range files {
		if asset, found := strings.CutPrefix(name, "assets/"); found {
			if _, ok := reference.Assets[asset]; !ok {
				return nil, nil, nil, errors.New("capsule contains an unlisted asset")
			}
		}
	}
	if !slices.Equal(slices.Sorted(slices.Values(item.Repro.AssetsSHA256)), slices.Sorted(maps.Values(reference.Assets))) {
		return nil, nil, nil, errors.New("capsule playlist assets disagree with the reference")
	}
	used := make(map[string]bool)
	requests := make(map[string]*Resource)
	for i := range capture.Resources {
		resource := &capture.Resources[i]
		resource.Method = strings.ToUpper(resource.EffectiveMethod())
		key := resource.Method + " " + resource.URL
		if requests[key] != nil || !capsuleHTTPURL(resource.URL) || (resource.Method != "GET" && resource.Method != "HEAD") ||
			resource.Status < 200 || resource.Status > 599 || resource.Status == 206 {
			return nil, nil, nil, errors.New("invalid or unsupported capsule replay response")
		}
		requests[key] = resource
		if err := capsuleHeaderFields(resource); err != nil {
			return nil, nil, nil, err
		}
		if resource.SHA256 != "" {
			if _, ok := blobs[resource.SHA256]; !ok {
				return nil, nil, nil, errors.New("capsule resource references an unknown asset")
			}
			used[resource.SHA256] = true
		}
		if resource.Method == "GET" && resource.RedirectTo == "" && resource.SHA256 == "" {
			empty := capsuleDigest(nil)
			blobs[empty] = nil
			used[empty] = true
			resource.SHA256 = empty
		}
	}
	entryURL := capture.Entry
	for redirects := 0; ; redirects++ {
		entry := requests["GET "+entryURL]
		if redirects >= 16 || entry == nil || entry.Status >= 400 {
			return nil, nil, nil, errors.New("capsule replay entry has an incomplete redirect chain")
		}
		if entry.RedirectTo == "" {
			if entry.Status >= 300 || len(blobs[entry.SHA256]) == 0 {
				return nil, nil, nil, errors.New("capsule replay entry does not contain artwork bytes")
			}
			break
		}
		entryURL = entry.RedirectTo
	}
	if len(used) != len(blobs) {
		return nil, nil, nil, errors.New("capsule contains an unused asset")
	}

	if capture.CapturedAt.IsZero() {
		capture.CapturedAt, _ = time.Parse(time.RFC3339, document.Created)
	}
	record := &ItemRecord{Capsule: &CapsuleRecord{ArchiveSHA256: id, EngineVersion: engine.Version, EngineSHA256: engineDigest, Files: reference.Mode == "files"}, Item: item, Entry: capture.Entry, Resources: capture.Resources,
		Coverage: capture.Coverage, CapturedAt: capture.CapturedAt}
	return raw, record, blobs, nil
}

// ImportCapsule admits an owner's signed capsule into the ordinary offline
// cache. The archive retains the publisher's engine identity and reference frame.
func (s *service) ImportCapsule(ctx context.Context, archive []byte, sidecar json.RawMessage, signer string) (json.RawMessage, error) {
	if !s.started.Load() {
		return nil, ErrServiceNotStarted
	}
	if len(archive) == 0 || len(archive) > MaxCapsuleBytes {
		return nil, errors.New("capsule archive exceeds its compressed size limit")
	}
	barrier := s.ClearBarrier()
	id := capsuleDigest(archive)
	if err := verifyCapsuleSignature(id, sidecar, signer); err != nil {
		return nil, err
	}
	files, err := readCapsuleFiles(archive)
	if err != nil {
		return nil, fmt.Errorf("read capsule: %w", err)
	}
	raw, record, blobs, err := capsuleReplay(id, files)
	if err != nil {
		return nil, err
	}
	record.Capsule.Signature = sidecar
	blobs[id] = archive

	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if len(encoded) > 16<<20 {
		return nil, errors.New("capsule manifest exceeds the native offline record size limit")
	}
	if err := s.saveCapsule(ctx, record, blobs, barrier, int64(len(encoded))); err != nil {
		return nil, err
	}
	return raw, nil
}

func (s *service) saveCapsule(ctx context.Context, record *ItemRecord, blobs map[string][]byte, barrier uint64, recordBytes int64) error {
	if !s.captureMu.TryLock() {
		return ErrItemBusy
	}
	defer s.captureMu.Unlock()

	wroteBlob := false
	publish := func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		key := SourceKey(record.Item.Source)
		if s.downloadEpoch[key] > barrier || s.clearing[key] != 0 {
			return ErrClearedDuringDownload
		}
		if s.state[key] == StateQueued || s.state[key] == StateDownloading {
			return ErrItemBusy
		}
		existing, err := s.store.LoadItem(key)
		switch {
		case err == nil:
			if existing.Automatic || existing.Entry != record.Entry || !sameCapsuleResponses(existing.Resources, record.Resources) ||
				(existing.Capsule != nil && existing.Capsule.ArchiveSHA256 != record.Capsule.ArchiveSHA256) {
				return errors.New("a different offline capture already exists for this capsule source")
			}
			record.LastPlayedAt = existing.LastPlayedAt
		case !errors.Is(err, ErrItemNotFound):
			return err
		}

		pending := make(map[string][]byte)
		needed := recordBytes
		for digest, body := range blobs {
			if err := ctx.Err(); err != nil {
				return err
			}
			if size, err := s.store.BlobSize(digest); err == nil && size == int64(len(body)) {
				if current, err := s.store.ReadBlob(digest); err == nil && bytes.Equal(current, body) {
					continue
				}
			}
			pending[digest] = body
			needed += int64(len(body))
		}
		if s.storage != nil || s.maxDiskBytes > 0 {
			limit, err := s.diskBudget()
			if err != nil {
				return err
			}
			used, err := s.store.DiskUsage()
			if err != nil {
				return err
			}
			if needed > limit-used {
				return errCacheDiskPressure
			}
		}
		for _, body := range pending {
			if err := ctx.Err(); err != nil {
				return err
			}
			wroteBlob = true
			if _, err := s.store.WriteBlob(bytes.NewReader(body), int64(len(body))); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.store.SaveItem(record); err != nil {
			return err
		}
		s.state[key] = StateReady
		s.sourceByKey[key] = record.Item.Source
		return nil
	}

	var err error
	if store, ok := s.store.(publisher); ok {
		err = store.Publish(publish)
	} else {
		err = publish()
	}
	if err != nil && wroteBlob {
		_, _, cleanupErr := s.store.GC()
		err = errors.Join(err, cleanupErr)
	}
	return err
}

func sameCapsuleResponses(left, right []Resource) bool {
	if len(left) != len(right) {
		return false
	}
	for i, a := range left {
		b := right[i]
		// Legacy import metadata is not consumed by native replay.
		a.Headers, b.Headers = nil, nil
		a.Method, b.Method = a.EffectiveMethod(), b.EffectiveMethod()
		if !slices.EqualFunc(a.HeaderFields, b.HeaderFields, func(x, y HeaderField) bool {
			return strings.EqualFold(x.Name, y.Name) && x.Value == y.Value
		}) {
			return false
		}
		a.HeaderFields, b.HeaderFields = nil, nil
		if !reflect.DeepEqual(a, b) {
			return false
		}
	}
	return true
}
