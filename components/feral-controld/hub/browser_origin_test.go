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
			req := httptest.NewRequest(http.MethodPost, "/api/cast", strings.NewReader(`{"command":"sshAccess","request":{}}`))
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
