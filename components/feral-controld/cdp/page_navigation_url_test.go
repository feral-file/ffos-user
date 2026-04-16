package cdp_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
)

func TestPageNavigationURL_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	responseBody := `[{"type":"page","title":"QR Onboarding","url":"file:///opt/feral/ui/launcher/index.html?step=qr","webSocketDebuggerUrl":"ws://localhost:9222/devtools/page/1"}]`
	bodyBytes := []byte(responseBody)
	mockResponse := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(responseBody)),
	}

	ts.mockHTTP.EXPECT().Get(gomock.Any()).Return(mockResponse, nil).Times(1)
	ts.mockIO.EXPECT().ReadAll(mockResponse.Body).Return(bodyBytes, nil).Times(1)
	ts.mockJSON.EXPECT().
		Unmarshal(bodyBytes, gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			targets := v.(*[]struct {
				Type string `json:"type"`
				URL  string `json:"url"`
			})
			*targets = []struct {
				Type string `json:"type"`
				URL  string `json:"url"`
			}{
				{
					Type: "page",
					URL:  "file:///opt/feral/ui/launcher/index.html?step=qr",
				},
			}
			return nil
		}).
		Times(1)

	url, err := ts.client.PageNavigationURL(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, "file:///opt/feral/ui/launcher/index.html?step=qr", url)
}

func TestPageNavigationURL_NoPageTarget(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	responseBody := `[{"type":"worker","title":"","url":"","webSocketDebuggerUrl":"ws://localhost:9222/devtools/page/2"}]`
	bodyBytes := []byte(responseBody)
	mockResponse := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(responseBody)),
	}

	ts.mockHTTP.EXPECT().Get(gomock.Any()).Return(mockResponse, nil).Times(1)
	ts.mockIO.EXPECT().ReadAll(mockResponse.Body).Return(bodyBytes, nil).Times(1)
	ts.mockJSON.EXPECT().
		Unmarshal(bodyBytes, gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			targets := v.(*[]struct {
				Type string `json:"type"`
				URL  string `json:"url"`
			})
			*targets = []struct {
				Type string `json:"type"`
				URL  string `json:"url"`
			}{
				{
					Type: "worker",
					URL:  "",
				},
			}
			return nil
		}).
		Times(1)

	_, err := ts.client.PageNavigationURL(context.Background())
	assert.ErrorIs(t, err, cdp.ErrNoPageTargetFound)
}

func TestPageNavigationURL_MultiplePageTargets(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	responseBody := `[{"type":"page","title":"A","url":"https://a","webSocketDebuggerUrl":"ws://localhost:9222/devtools/page/1"},{"type":"page","title":"B","url":"https://b","webSocketDebuggerUrl":"ws://localhost:9222/devtools/page/2"}]`
	bodyBytes := []byte(responseBody)
	mockResponse := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(responseBody)),
	}

	ts.mockHTTP.EXPECT().Get(gomock.Any()).Return(mockResponse, nil).Times(1)
	ts.mockIO.EXPECT().ReadAll(mockResponse.Body).Return(bodyBytes, nil).Times(1)
	ts.mockJSON.EXPECT().
		Unmarshal(bodyBytes, gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			targets := v.(*[]struct {
				Type string `json:"type"`
				URL  string `json:"url"`
			})
			*targets = []struct {
				Type string `json:"type"`
				URL  string `json:"url"`
			}{
				{
					Type: "page",
					URL:  "https://a",
				},
				{
					Type: "page",
					URL:  "https://b",
				},
			}
			return nil
		}).
		Times(1)

	_, err := ts.client.PageNavigationURL(context.Background())
	assert.ErrorIs(t, err, cdp.ErrMultiplePageTargetsFound)
}

func TestPageNavigationURL_ContextCanceled(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ts.client.PageNavigationURL(ctx)
	assert.ErrorContains(t, err, context.Canceled.Error(), fmt.Sprintf("expected context canceled, got %v", err))
}

func TestPageNavigationURL_HTTPGetError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	ts.mockHTTP.EXPECT().Get(gomock.Any()).Return(nil, errors.New("connection refused")).Times(1)

	_, err := ts.client.PageNavigationURL(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to fetch debug targets")
}

func TestPageNavigationURL_ReadAllError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	responseBody := `[]`
	mockResponse := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(responseBody)),
	}

	ts.mockHTTP.EXPECT().Get(gomock.Any()).Return(mockResponse, nil).Times(1)
	ts.mockIO.EXPECT().ReadAll(mockResponse.Body).Return(nil, errors.New("read failed")).Times(1)

	_, err := ts.client.PageNavigationURL(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read targets")
}
