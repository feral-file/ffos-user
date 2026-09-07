package portal

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	uaSafariIOS   = "Mozilla/5.0 (iPhone; CPU iPhone OS 26_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Mobile/15E148 Safari/604.1"
	uaCNAIOS      = "Mozilla/5.0 (iPhone; CPU iPhone OS 26_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148"
	uaProbeIOS    = "CaptiveNetworkSupport-514.160.1.0.1 wispr"
	uaAndroidWV   = "Mozilla/5.0 (Linux; Android 14; Pixel 8 Build/AP2A; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/126.0.0.0 Mobile Safari/537.36"
	uaChromeAndro = "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36"
	uaDalvik      = "Dalvik/2.1.0 (Linux; U; Android 14; Pixel 8 Build/AP2A)"
)

// get issues a request through the limit-wrapped handler with a fixed client
// address and user agent, without following redirects.
func get(t *testing.T, h http.Handler, path, remote, ua string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = remote
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestFullBrowserUserAgentClassification(t *testing.T) {
	assert.True(t, isFullBrowserUserAgent(uaSafariIOS), "Safari")
	assert.True(t, isFullBrowserUserAgent(uaChromeAndro), "Chrome on Android")
	assert.True(t, isFullBrowserUserAgent("Mozilla/5.0 (X11; Linux) Gecko/20100101 Firefox/128.0"), "Firefox")
	assert.False(t, isFullBrowserUserAgent(uaCNAIOS), "iOS captive sheet has no Safari token")
	assert.False(t, isFullBrowserUserAgent(uaProbeIOS), "iOS probe")
	assert.False(t, isFullBrowserUserAgent(uaAndroidWV), "Android CaptivePortalLogin is a WebView")
	assert.False(t, isFullBrowserUserAgent(uaDalvik), "Android probe")
	assert.False(t, isFullBrowserUserAgent(""), "no agent")
}

// TestFullBrowserFetchAuthorizesThatClientsProbes: a probe redirects until the
// SAME client fetches the page with a full browser; afterwards each probe
// family gets its success answer, and a different client stays captive.
func TestFullBrowserFetchAuthorizesThatClientsProbes(t *testing.T) {
	h := NewServer(Config{APSSID: "FF1-abc"}).Handler()
	const phone, other = "10.42.0.23:51000", "10.42.0.24:51000"

	assert.Equal(t, http.StatusFound, get(t, h, "/hotspot-detect.html", phone, uaProbeIOS).Code, "captive until the page is fetched")

	page := get(t, h, "/", phone, uaSafariIOS)
	require.Equal(t, http.StatusOK, page.Code)

	rec := get(t, h, "/hotspot-detect.html", phone, uaProbeIOS)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "<TITLE>Success</TITLE>", "Apple's success body")
	assert.Equal(t, http.StatusNoContent, get(t, h, "/generate_204", phone, uaDalvik).Code)
	assert.Equal(t, http.StatusNoContent, get(t, h, "/gen_204", phone, uaDalvik).Code)
	assert.Equal(t, "Microsoft Connect Test", get(t, h, "/connecttest.txt", phone, "").Body.String())
	assert.Equal(t, "Microsoft NCSI", get(t, h, "/ncsi.txt", phone, "").Body.String())
	assert.Equal(t, http.StatusNoContent, get(t, h, "/some/unlisted/probe", phone, "").Code, "unenumerated probe shapes stop redirecting too")

	// The page itself keeps rendering for an authorized client (the form is
	// what they came for), and other clients are untouched.
	assert.Equal(t, http.StatusOK, get(t, h, "/", phone, uaSafariIOS).Code)
	assert.Equal(t, http.StatusFound, get(t, h, "/hotspot-detect.html", other, uaProbeIOS).Code, "a different client is still captive")
}

// TestCaptiveMiniBrowsersNeverAuthorize: the OS sheets close themselves when
// the network validates, so their own page fetch must keep the client captive
// — otherwise the Settings-join user's sheet slams shut mid-password.
func TestCaptiveMiniBrowsersNeverAuthorize(t *testing.T) {
	h := NewServer(Config{APSSID: "FF1-abc"}).Handler()
	for name, ua := range map[string]string{"iOS CNA": uaCNAIOS, "Android login WebView": uaAndroidWV, "no agent": ""} {
		remote := "10.42.0.30:51000"
		require.Equal(t, http.StatusOK, get(t, h, "/", remote, ua).Code, name)
		assert.Equal(t, http.StatusFound, get(t, h, "/hotspot-detect.html", remote, uaProbeIOS).Code, "%s must stay captive", name)
	}
}

// TestAuthorizationIsPerServer: a re-raised AP builds a fresh Server, and the
// phone that was authorized on the previous raise starts captive again.
func TestAuthorizationIsPerServer(t *testing.T) {
	const phone = "10.42.0.23:51000"
	first := NewServer(Config{APSSID: "FF1-abc"}).Handler()
	require.Equal(t, http.StatusOK, get(t, first, "/", phone, uaSafariIOS).Code)
	require.Equal(t, http.StatusOK, get(t, first, "/hotspot-detect.html", phone, uaProbeIOS).Code)

	second := NewServer(Config{APSSID: "FF1-abc"}).Handler()
	assert.Equal(t, http.StatusFound, get(t, second, "/hotspot-detect.html", phone, uaProbeIOS).Code)
}
