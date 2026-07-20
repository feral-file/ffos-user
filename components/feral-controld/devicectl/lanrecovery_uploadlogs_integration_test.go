package devicectl

// Offline LAN-recovery: uploadLogs in-process presign+PUT (plan item P3.3).
//
// Companion to hub/lanrecovery_integration_test.go. That test proves the ex-BLE
// recovery commands traverse the real hub -> storm gate -> executor pipeline
// offline. This test covers the one leg that cannot be observed from the hub
// package: the controld-owned in-process log upload, whose HTTP client and
// endpoint are only reachable through devicectl's unexported logUploaderFactory
// seam. Here we drive the REAL executor's uploadLogs command and assert the
// ported v2 wire contract — a pre-sign POST followed by an S3 PUT — lands on a
// mocked HTTP client, with no relayer and no real network involved.
//
// The upload is deliberately fire-and-forget in production (it runs on a
// detached context so the command ACKs immediately). The test synchronizes on
// the two mocked HTTP calls before asserting, so it is deterministic and
// -race clean.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/ddc"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func TestLANRecovery_UploadLogsInProcess_PresignThenPut(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	// controld runs the log upload in-process (the only owner now).
	// Executor leaf seams. The executor's own OS reads (device_id / build
	// descriptor) all miss, so device_id falls back to "FF1" and branch/version
	// are empty — none of that gates the upload.
	mockCDP := mocks.NewMockCDP(ctrl)
	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().ReadFile(gomock.Any()).Return(nil, os.ErrNotExist).AnyTimes()
	mockOS.EXPECT().IsNotExist(gomock.Any()).Return(true).AnyTimes()
	mockExec := mocks.NewMockExec(ctrl)
	mockMath := mocks.NewMockMath(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	mockDevSts := mocks.NewMockDeviceStatus(ctrl)
	mockPoller := mocks.NewMockStatusPoller(ctrl)
	panelDDC := ddc.New(mockExec, mockClock, logger)

	executor := New(
		mockCDP, mockDevSts, mockPoller, panelDDC,
		wrapper.NewJSON(), mockOS, mockExec, mockMath, mockClock, logger,
	).(*executor)

	// A real logUploader, but pointed at a test endpoint and a mocked HTTP
	// client, injected through the (unexported) factory seam. Its logs dir is a
	// real temp dir with one file so the zip step succeeds.
	logsDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(logsDir, "app.log"), []byte("hello log"), 0o600))

	const apiURL = "https://presign.test/v2/ff1/log-submissions"
	const s3URL = "https://s3.presign.test/upload?sig=abc"

	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	reqs := make(chan *http.Request, 2)
	var presignBody []byte
	var putBodyLen int
	mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(req *http.Request) (*http.Response, error) {
		// Read the body synchronously; the uploader will not touch it again.
		if req.Body != nil {
			b, _ := io.ReadAll(req.Body)
			_ = req.Body.Close()
			switch req.Method {
			case http.MethodPost:
				presignBody = b
			case http.MethodPut:
				putBodyLen = len(b)
			}
		}
		reqs <- req
		if req.Method == http.MethodPost {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"object_key":"k","upload":{"url":"` + s3URL + `"},"expires_in_seconds":60}`))),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(nil)),
		}, nil
	}).Times(2)

	executor.logUploaderFactory = func() logUploaderIface {
		return &logUploader{
			http:      mockHTTP,
			os:        wrapper.NewOS(),
			json:      wrapper.NewJSON(),
			logsDir:   logsDir,
			extraLogs: nil,
			apiURL:    apiURL,
			logger:    zap.NewNop(), // detached worker: never touch the test logger
		}
	}

	cmd := commands.Command{
		Type: commands.CMD_UPLOAD_LOGS,
		Arguments: map[string]any{
			"userId":          "user-1",
			"apiKey":          "secret-key",
			"title":           "crash report",
			"supportBundleID": "bundle-42",
		},
	}

	// The command returns immediately (fire-and-forget); the upload runs detached.
	result, err := executor.Execute(ctx, cmd)
	require.NoError(t, err)
	require.Equal(t, CmdOK, result)

	// Synchronize on the two HTTP legs before asserting.
	first := recvReq(t, reqs)
	second := recvReq(t, reqs)

	assert.Equal(t, http.MethodPost, first.Method, "first leg is the pre-sign POST")
	assert.Equal(t, apiURL, first.URL.String())
	assert.Equal(t, "secret-key", first.Header.Get("x-api-key"))
	assert.Equal(t, "application/json", first.Header.Get("Content-Type"))

	var presign map[string]any
	require.NoError(t, json.Unmarshal(presignBody, &presign))
	assert.Equal(t, "FF1", presign["device_id"], "device_id falls back to FF1 offline")
	assert.Equal(t, "dbus", presign["source"], "relayer command inherits the dbus source tag")
	assert.Equal(t, "bundle-42", presign["support_bundle_id"])

	assert.Equal(t, http.MethodPut, second.Method, "second leg is the S3 PUT")
	assert.Equal(t, s3URL, second.URL.String())
	assert.Equal(t, "application/zip", second.Header.Get("Content-Type"))
	assert.Greater(t, putBodyLen, 0, "the zipped log archive is PUT to the presigned URL")
}

// recvReq waits for one mocked HTTP request or fails on timeout.
func recvReq(t *testing.T, ch <-chan *http.Request) *http.Request {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the in-process log upload HTTP request")
		return nil
	}
}
