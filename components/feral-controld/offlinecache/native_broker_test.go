package offlinecache_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
)

func TestNativeBrokerPreservesOriginHeadersAndRejectsWrongVariant(t *testing.T) {
	store, _ := newTestStore(t)
	body := []byte("worker bytes")
	digest, err := store.WriteBlob(bytes.NewReader(body), 1024)
	require.NoError(t, err)
	source := "https://artist.example/work/?seed=abc"
	url := "https://artist.example/worker.js?version=1"
	headers := []offlinecache.HeaderField{{Name: "Content-Security-Policy", Value: "default-src 'self'"}, {Name: "Content-Security-Policy", Value: "script-src 'self'"}, {Name: "Cross-Origin-Opener-Policy", Value: "same-origin"}, {Name: "Content-Type", Value: "text/javascript"}}
	record := &offlinecache.ItemRecord{Item: dp1playlist.PlaylistItem{Source: source}, Coverage: offlinecache.Coverage{Complete: false}, Resources: []offlinecache.Resource{{URL: url, SHA256: digest, Status: 200, BrowserPolicyVersion: 2, HeaderFields: headers}}}
	require.NoError(t, store.SaveItem(record))
	broker := offlinecache.NewNativeBroker(store)
	request := func(input any) *httptest.ResponseRecorder {
		encoded, e := json.Marshal(input)
		require.NoError(t, e)
		r := httptest.NewRequest(http.MethodPost, "/v1/replay", bytes.NewReader(encoded))
		w := httptest.NewRecorder()
		broker.ServeHTTP(w, r)
		return w
	}
	params := map[string]any{"source": source, "url": url, "method": "GET", "headers": map[string]string{}}
	reply := request(params)
	require.Equal(t, 200, reply.Code)
	data := reply.Body.Bytes()
	size := binary.BigEndian.Uint32(data[:4])
	var metadata struct {
		Headers  []offlinecache.HeaderField `json:"headers"`
		Complete bool                       `json:"complete"`
		Size     int                        `json:"size"`
	}
	require.NoError(t, json.Unmarshal(data[4:4+size], &metadata))
	require.Equal(t, headers, metadata.Headers)
	require.False(t, metadata.Complete)
	require.Equal(t, len(body), metadata.Size)
	require.Equal(t, body, data[4+size:])
	params["url"] = strings.Replace(url, "version=1", "version=2", 1)
	require.Equal(t, 404, request(params).Code)
	params["url"] = url
	for _, method := range []string{"POST", "DELETE", "OPTIONS"} {
		params["method"] = method
		require.Equal(t, 422, request(params).Code)
	}
	params["method"] = "GET"
	params["headers"] = map[string]string{"Cookie": "session=private"}
	require.Equal(t, 422, request(params).Code)
	params["headers"] = map[string]string{}
	record.Resources[0].BrowserPolicyVersion = 0
	require.NoError(t, store.SaveItem(record))
	require.Equal(t, 422, request(params).Code)
	record.Resources[0].BrowserPolicyVersion = 2
	require.NoError(t, store.SaveItem(record))
	require.NoError(t, os.WriteFile(store.BlobPath(digest), []byte("corrupt"), 0600))
	require.Equal(t, 500, request(params).Code)
}

func TestNativeBrokerPolicyAndLegacyMediaHandoff(t *testing.T) {
	store, _ := newTestStore(t)
	source := "https://artist.example/opaque"
	digest, err := store.WriteBlob(strings.NewReader("native media bytes"), 1024)
	require.NoError(t, err)
	record := &offlinecache.ItemRecord{Item: dp1playlist.PlaylistItem{Source: source}, Resources: []offlinecache.Resource{{URL: source, SHA256: digest, Status: 200, ContentType: "image/png"}}}
	require.NoError(t, store.SaveItem(record))
	broker := offlinecache.NewNativeBroker(store)
	broker.Policy = func(url string, headers map[string]string) (string, []offlinecache.HeaderField) {
		return url + "?approved", []offlinecache.HeaderField{{Name: "User-Agent", Value: "approved-UA"}}
	}
	call := func(path string) map[string]any {
		w := httptest.NewRecorder()
		broker.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(`{"source":"`+source+`","url":"`+source+`","method":"GET","headers":{}}`)))
		require.Equal(t, 200, w.Code, w.Body.String())
		data := w.Body.Bytes()
		length := binary.BigEndian.Uint32(data[:4])
		require.Equal(t, int(length)+4, len(data), "classification/policy must have no resource body")
		var result map[string]any
		require.NoError(t, json.Unmarshal(data[4:], &result))
		return result
	}
	require.Equal(t, "image/png", call("/v1/replay")["nativeMime"])
	require.Equal(t, source+"?approved", call("/v1/policy")["requestUrl"])
	require.Equal(t, "isolated", call("/v1/policy")["replayMode"])
	record.Automatic = true
	require.NoError(t, store.SaveItem(record))
	require.Equal(t, "mixed", call("/v1/policy")["replayMode"])
	record.Resources[0].BrowserPolicyVersion = 2
	require.NoError(t, store.SaveItem(record))
	require.Equal(t, "image/png", call("/v1/replay")["nativeMime"])
	opened := call("/v1/open")
	require.Equal(t, store.BlobPath(digest), opened["path"])
	require.Equal(t, float64(len("native media bytes")), opened["size"])
}

func TestNativeBrokerSharedPolicyFixture(t *testing.T) {
	data, err := os.ReadFile("../../player/protocol/cache-fixtures.json")
	require.NoError(t, err)
	var fixture struct {
		Request  json.RawMessage `json:"request"`
		Response map[string]any  `json:"policyResponse"`
	}
	require.NoError(t, json.Unmarshal(data, &fixture))
	w := httptest.NewRecorder()
	offlinecache.NewNativeBroker(nil).ServeHTTP(w, httptest.NewRequest("POST", "/v1/policy", bytes.NewReader(fixture.Request)))
	require.Equal(t, 200, w.Code)
	body := w.Body.Bytes()
	n := binary.BigEndian.Uint32(body[:4])
	require.Equal(t, int(n)+4, len(body))
	var metadata map[string]any
	require.NoError(t, json.Unmarshal(body[4:], &metadata))
	require.Equal(t, fixture.Response, metadata)
}
