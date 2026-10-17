package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// TestMiddlewareRejectsBrowserOrigins pins the stopgap that keeps web pages
// off the open LAN hub: any request a browser marks as page-initiated is
// refused before the handler runs, native callers pass, and the player's own
// log route keeps its separate check.
func TestMiddlewareRejectsBrowserOrigins(t *testing.T) {
	for _, tt := range []struct {
		name    string
		route   string
		headers map[string]string
		wantRun bool
		want    int
	}{
		{"native cast (app, ff-cli, relayer path)", "cast", nil, true, http.StatusOK},
		{"page with Origin", "cast", map[string]string{"Origin": "https://example.com"}, false, http.StatusForbidden},
		{"no-cors page with Sec-Fetch-Site only", "cast", map[string]string{"Sec-Fetch-Site": "cross-site"}, false, http.StatusForbidden},
		{"same-device page on cast", "cast", map[string]string{"Origin": "http://127.0.0.1:8080", "Sec-Fetch-Site": "same-site"}, false, http.StatusForbidden},
		{"page on status", "status", map[string]string{"Origin": "https://example.com"}, false, http.StatusForbidden},
		{"page on notification upgrade", "notification", map[string]string{"Origin": "https://example.com"}, false, http.StatusForbidden},
		{"player logs keep their own check", playerLogsRoute, map[string]string{"Origin": "http://127.0.0.1:8080"}, true, http.StatusOK},
		{"fetch-mode alone is not a browser marker", "cast", map[string]string{"Sec-Fetch-Mode": "cors"}, true, http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := &hub{logger: zap.NewNop(), reqSlots: make(chan struct{}, 1)}
			ran := false
			handler := h.withMiddleware(tt.route, func(w http.ResponseWriter, r *http.Request) {
				ran = true
				w.WriteHeader(http.StatusOK)
			})
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1111/api/cast", strings.NewReader(`{"command":"sshAccess","request":{}}`))
			req.RemoteAddr = "192.168.1.10:41000"
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			handler(rec, req)
			if ran != tt.wantRun || rec.Code != tt.want {
				t.Fatalf("ran=%v status=%d, want ran=%v status=%d", ran, rec.Code, tt.wantRun, tt.want)
			}
		})
	}
}

// TestIsLocalHost pins the DNS-rebinding boundary: IP literals, localhost,
// and the device's own name pass; any other name, including a short one a
// resolver could expand through a public search suffix, does not.
func TestIsLocalHost(t *testing.T) {
	prev := deviceHostname
	deviceHostname = func() (string, error) { return "FF1-NFZMNQSP", nil }
	t.Cleanup(func() { deviceHostname = prev })

	for host, want := range map[string]bool{
		"192.168.1.50:1111":                true,
		"192.168.1.50":                     true,
		"127.0.0.1:1111":                   true,
		"[::1]:1111":                       true,
		"[fe80::1a2b:3c4d:5e6f:7a8b]:1111": true,
		"[fe80::1%25wlan0]:1111":           true,
		"localhost:1111":                   true,
		"ff1-nfzmnqsp.local:1111":          true,
		"FF1-NFZMNQSP.LOCAL":               true,
		"ff1-nfzmnqsp.local.:1111":         true,
		"ff1-nfzmnqsp:1111":                true,
		"evil:1111":                        false,
		"ff1-other.local:1111":             false,
		"ff1-nfzmnqsp.lan:1111":            false,
		"ff1-nfzmnqsp.local.example.net":   false,
		"attacker.example:1111":            false,
		"evil-local:1111.example":          false,
		"":                                 false,
	} {
		if got := isLocalHost(host); got != want {
			t.Errorf("isLocalHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// TestMiddlewareRejectsRebindingHost is the review's path: a same-origin GET
// from a rebinding page carries neither browser header, only the attacker's
// Host, and must not reach status (which serves the topic ID).
func TestMiddlewareRejectsRebindingHost(t *testing.T) {
	for _, tt := range []struct {
		target  string
		wantRun bool
		want    int
	}{
		{"http://attacker.example:1111/api/v2/status", false, http.StatusForbidden},
		{"http://evil:1111/api/v2/status", false, http.StatusForbidden},
		{"http://192.168.1.50:1111/api/v2/status", true, http.StatusOK},
	} {
		h := &hub{logger: zap.NewNop(), reqSlots: make(chan struct{}, 1)}
		ran := false
		handler := h.withMiddleware("status_v2", func(w http.ResponseWriter, r *http.Request) {
			ran = true
			w.WriteHeader(http.StatusOK)
		})
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, tt.target, nil))
		if ran != tt.wantRun || rec.Code != tt.want {
			t.Fatalf("%s: ran=%v status=%d, want ran=%v status=%d", tt.target, ran, rec.Code, tt.wantRun, tt.want)
		}
	}
}
