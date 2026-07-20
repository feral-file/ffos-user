package devicectl

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// newTestUploader builds a logUploader wired to a temp log dir and the given
// pre-sign API URL, using the real OS/JSON/HTTP seams (the httptest server is a
// real HTTP endpoint).
func newTestUploader(t *testing.T, logsDir, apiURL string, extraLogs []string) *logUploader {
	t.Helper()
	return &logUploader{
		http:      wrapper.NewHTTPClient(),
		os:        wrapper.NewOS(),
		json:      wrapper.NewJSON(),
		logsDir:   logsDir,
		extraLogs: extraLogs,
		apiURL:    apiURL,
		logger:    zap.NewNop(),
	}
}

func TestLogUploader_Upload_RequestShape(t *testing.T) {
	logsDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(logsDir, "app.log"), []byte("hello"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(logsDir, "sub"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(logsDir, "sub", "x.log"), []byte("world"), 0o600))

	// An extra log that exists is folded in under its base name; a missing one is
	// silently skipped.
	extraDir := t.TempDir()
	extraPresent := filepath.Join(extraDir, "updaterd.log")
	require.NoError(t, os.WriteFile(extraPresent, []byte("updater"), 0o600))
	extraMissing := filepath.Join(extraDir, "does-not-exist.log")

	var (
		presignBody    []byte
		presignHeader  http.Header
		putHit         bool
		putContentType string
		putBody        []byte
	)

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/ff1/log-submissions":
			presignBody, _ = io.ReadAll(r.Body)
			presignHeader = r.Header.Clone()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object_key":         "logs/ff1-abc/123.zip",
				"upload":             map[string]any{"url": srv.URL + "/s3-upload"},
				"expires_in_seconds": 60,
			})
		case r.Method == http.MethodPut && r.URL.Path == "/s3-upload":
			putHit = true
			putContentType = r.Header.Get("Content-Type")
			putBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	u := newTestUploader(t, logsDir, srv.URL+"/v2/ff1/log-submissions", []string{extraPresent, extraMissing})

	err := u.Upload(
		context.Background(),
		"api-key-123",
		"dbus",
		logUploadBuildInfo{DeviceID: "ff1-abc", Branch: "main/stable", Version: "1.2.3"},
		"  bundle-9  ",
	)
	require.NoError(t, err)

	// Pre-sign request: headers + JSON field shape.
	assert.Equal(t, "api-key-123", presignHeader.Get("x-api-key"))
	assert.Equal(t, "application/json", presignHeader.Get("Content-Type"))

	var got map[string]any
	require.NoError(t, json.Unmarshal(presignBody, &got))
	assert.Equal(t, "ff1-abc", got["device_id"])
	assert.Equal(t, "dbus", got["source"])
	assert.Equal(t, "main/stable", got["branch"]) // branch is NOT %2F-encoded here (that is claim-URL only)
	assert.Equal(t, "1.2.3", got["version"])
	assert.Equal(t, []any{"device-logs"}, got["tags"])
	assert.Equal(t, "bundle-9", got["support_bundle_id"]) // trimmed
	_, hasTitle := got["title"]
	assert.False(t, hasTitle, "title must be omitted (v2 API ignores it)")

	// S3 PUT: content-type and a valid zip carrying every collected file.
	require.True(t, putHit, "expected an S3 PUT")
	assert.Equal(t, "application/zip", putContentType)

	zr, err := zip.NewReader(bytes.NewReader(putBody), int64(len(putBody)))
	require.NoError(t, err)
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	assert.True(t, names["app.log"], "zip should contain app.log")
	assert.True(t, names["sub/x.log"], "zip should contain nested sub/x.log")
	assert.True(t, names["updaterd.log"], "zip should contain the extra log at its base name")
	assert.False(t, names["does-not-exist.log"], "missing extra log must not appear")
}

func TestLogUploader_OmitsBlankSupportBundleID(t *testing.T) {
	logsDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(logsDir, "app.log"), []byte("x"), 0o600))

	var presignBody []byte
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			presignBody, _ = io.ReadAll(r.Body)
			_ = json.NewEncoder(w).Encode(map[string]any{"upload": map[string]any{"url": srv.URL + "/put"}})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u := newTestUploader(t, logsDir, srv.URL+"/v2/ff1/log-submissions", nil)
	err := u.Upload(context.Background(), "k", "dbus", logUploadBuildInfo{DeviceID: "d"}, "   ")
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(presignBody, &got))
	_, has := got["support_bundle_id"]
	assert.False(t, has, "blank support_bundle_id must be omitted")
}

func TestLogUploader_NoLogFiles(t *testing.T) {
	// Empty (but existing) log dir and no extra logs => nothing to zip.
	u := newTestUploader(t, t.TempDir(), "http://unused.invalid", nil)
	_, err := u.zipLogs()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no log files found")
}

// fakeUploader records the Upload call and signals completion.
type fakeUploader struct {
	mu     sync.Mutex
	called bool
	apiKey string
	source string
	info   logUploadBuildInfo
	bundle string
	done   chan struct{}
}

func (f *fakeUploader) Upload(_ context.Context, apiKey, source string, info logUploadBuildInfo, supportBundleID string) error {
	f.mu.Lock()
	f.called = true
	f.apiKey = apiKey
	f.source = source
	f.info = info
	f.bundle = supportBundleID
	f.mu.Unlock()
	close(f.done)
	return nil
}

func TestUploadLogsInProcess_RoutesToUploader(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// Build info gathering reads hostname and the FF1 build descriptor; make both
	// fail so device_id falls back to "FF1" and branch/version stay empty (the
	// non-fatal path), keeping the test free of a real filesystem.
	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().ReadFile(gomock.Any()).Return(nil, os.ErrNotExist).AnyTimes()
	mockOS.EXPECT().IsNotExist(gomock.Any()).Return(true).AnyTimes()

	fake := &fakeUploader{done: make(chan struct{})}
	e := &executor{
		logger:             zap.NewNop(),
		os:                 mockOS,
		json:               wrapper.NewJSON(),
		logUploaderFactory: func() logUploaderIface { return fake },
	}

	res, err := e.uploadLogsInProcess(context.Background(), "api-key-77", "  bundle-x  ")
	require.NoError(t, err)
	assert.Equal(t, CmdOK, res)

	select {
	case <-fake.done:
	case <-time.After(2 * time.Second):
		t.Fatal("uploader.Upload was not invoked")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	assert.True(t, fake.called)
	assert.Equal(t, "api-key-77", fake.apiKey)
	assert.Equal(t, logUploadSource, fake.source) // "dbus"
	assert.Equal(t, "FF1", fake.info.DeviceID)    // hostname fallback
	assert.Equal(t, "  bundle-x  ", fake.bundle)  // trimming happens inside the uploader
}
