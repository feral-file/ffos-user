// Package portal serves the FF1 setup captive portal: the offline HTML page a
// phone reaches once it joins the setup access point, where the user picks a
// Wi-Fi network and submits credentials.
//
// The portal owns only the HTTP listener. It deliberately does NOT call
// wifictl.Join itself: joining requires taking the setup AP down first (the
// single radio can't host the AP and associate as a station at once) and
// re-raising it on failure. That AP-bounce sequencing belongs to the
// provisioning state machine, so the portal hands submitted credentials off
// through the JoinFunc seam and reports outcomes it reads back through the
// StatusFunc seam. Because the AP bounces mid-join, the phone that submitted the
// form is disconnected and must re-associate; it then re-fetches GET /status to
// learn how the join went, which is why the outcome lives in the machine (fed
// here via StatusFunc) and survives portal restarts.
//
// This package carries no relayer, D-Bus, or CDP coupling and no wifictl/softap
// import: it depends only on the three injected seams below.
package portal

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"net"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

//go:embed templates/*.html
var assets embed.FS

var tmpl = template.Must(template.ParseFS(assets, "templates/*.html"))

// JoinState is the coarse lifecycle of the current or last credential submission,
// serialized in GET /status and rendered on the portal page.
type JoinState string

const (
	// JoinIdle means no credentials have been submitted yet this AP session.
	JoinIdle JoinState = "idle"
	// JoinInProgress means a join is being attempted (the AP may be bouncing).
	JoinInProgress JoinState = "joining"
	// JoinSucceeded means the last join associated with the target network.
	JoinSucceeded JoinState = "succeeded"
	// JoinFailed means the last join was rejected or errored.
	JoinFailed JoinState = "failed"
)

// Status is the snapshot GET /status returns and the page renders. It is owned
// and mutated by the provisioning machine; the portal only reads it through
// StatusFunc so the last outcome survives a portal restart / AP re-raise.
type Status struct {
	State JoinState `json:"state"`
	// SSID is the target network of the current or last attempt, when known.
	SSID string `json:"ssid,omitempty"`
	// Reason is a short machine-readable cause (e.g. "auth-failure").
	Reason string `json:"reason,omitempty"`
	// Message is the user-facing description shown on the page.
	Message string `json:"message,omitempty"`
}

// ScanFunc returns the SSIDs to offer in the picker. In production this is
// wifictl.CachedScan: while the setup AP holds the radio a live scan is
// unreliable, so the portal reads the pre-AP cache.
type ScanFunc func(ctx context.Context) ([]string, error)

// JoinFunc hands submitted credentials to the provisioning machine. It MUST
// return promptly (the machine performs the AP-bounce + join asynchronously);
// a non-nil error means the submission was rejected outright (e.g. empty SSID)
// and the form is re-rendered.
type JoinFunc func(ssid, password string) error

// StatusFunc reports the current/last join outcome. Backed by the provisioning
// machine so the outcome persists across portal restarts.
type StatusFunc func() Status

// Config wires the portal's listener and its three seams.
type Config struct {
	// Addr is the bind address. Production binds ":80" (a sysctl lowers the
	// unprivileged port floor; the portal adds no capabilities logic). Tests
	// inject "127.0.0.1:0" or use Handler directly.
	Addr string
	// APSSID is the setup AP's SSID, templated into the "reconnect to ..."
	// pre-warning the page shows around the AP bounce.
	APSSID string
	Scan   ScanFunc
	Join   JoinFunc
	Status StatusFunc
	Logger *zap.Logger
}

// Server is the captive-portal HTTP server.
type Server struct {
	cfg    Config
	logger *zap.Logger
	mux    *http.ServeMux

	mu   sync.Mutex
	http *http.Server
	ln   net.Listener
}

// NewServer builds a portal Server. Call Start to bind and serve, or use
// Handler with httptest.
func NewServer(cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	s := &Server{cfg: cfg, logger: logger, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler exposes the routes for httptest without binding a socket.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleRoot)
	s.mux.HandleFunc("/connect", s.handleConnect)
	s.mux.HandleFunc("/status", s.handleStatus)

	// OS captive-portal probes. Returning a redirect (rather than the 204 /
	// success body each OS looks for) is what makes the phone decide it is
	// behind a captive portal and surface the "Sign in to network" sheet
	// pointing at our page.
	for _, p := range []string{
		"/generate_204",        // Android
		"/gen_204",             // Android (older)
		"/hotspot-detect.html", // iOS / macOS
		"/library/test/success.html",
		"/connecttest.txt", // Windows
		"/ncsi.txt",        // Windows NCSI
	} {
		s.mux.HandleFunc(p, s.redirectToPortal)
	}
}

// Start binds Addr and serves in the background. It returns once the listener is
// bound (so bind errors surface synchronously) or with the bind error.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.mu.Lock()
	s.ln = ln
	s.http = srv
	s.mu.Unlock()

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logger.Warn("portal: serve ended", zap.Error(err))
		}
	}()
	s.logger.Info("portal: listening", zap.String("addr", ln.Addr().String()))
	return nil
}

// Addr reports the actual bound address (useful when Addr used port 0).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return s.cfg.Addr
	}
	return s.ln.Addr().String()
}

// Stop gracefully shuts the listener down. Safe to call when never started.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	srv := s.http
	s.http = nil
	s.ln = nil
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// -----------------------------------------------------------------------------
// Handlers
// -----------------------------------------------------------------------------

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	// ServeMux routes every unmatched path here ("/" is a prefix match). Treat
	// any non-root path as a captive probe and bounce it to the portal page, so
	// OS probe variants we did not enumerate still land on setup.
	if r.URL.Path != "/" {
		s.redirectToPortal(w, r)
		return
	}
	s.renderIndex(w, r)
}

// renderIndex writes the picker page. It is path-independent so the /connect
// rejection path can re-render the form without tripping handleRoot's
// non-root-path redirect.
func (s *Server) renderIndex(w http.ResponseWriter, r *http.Request) {
	var ssids []string
	if s.cfg.Scan != nil {
		got, err := s.cfg.Scan(r.Context())
		if err != nil {
			// A failed scan is non-fatal: the template falls back to a manual
			// SSID text field so the user can still type a hidden network.
			s.logger.Warn("portal: scan failed, offering manual entry", zap.Error(err))
		}
		ssids = got
	}

	data := struct {
		APSSID     string
		SSIDs      []string
		LastStatus *Status
	}{APSSID: s.cfg.APSSID, SSIDs: ssids}

	if s.cfg.Status != nil {
		st := s.cfg.Status()
		if st.State != JoinIdle && st.State != "" {
			data.LastStatus = &st
		}
	}

	s.render(w, "index.html", data)
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	ssid := r.PostFormValue("ssid")
	password := r.PostFormValue("password")

	if s.cfg.Join != nil {
		if err := s.cfg.Join(ssid, password); err != nil {
			// Rejected outright (e.g. empty SSID): re-render the picker so the
			// user can correct it. The AP has not bounced in this case.
			s.logger.Info("portal: join submission rejected", zap.Error(err))
			s.renderIndex(w, r)
			return
		}
	}

	// Accepted: the machine will bounce the AP and attempt the join. Show the
	// "connecting, reconnect if this drops" page.
	s.render(w, "result.html", struct {
		SSID   string
		APSSID string
	}{SSID: ssid, APSSID: s.cfg.APSSID})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	var st Status
	if s.cfg.Status != nil {
		st = s.cfg.Status()
	}
	if st.State == "" {
		st.State = JoinIdle
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(st); err != nil {
		s.logger.Warn("portal: encode status failed", zap.Error(err))
	}
}

func (s *Server) redirectToPortal(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.logger.Error("portal: render failed", zap.String("template", name), zap.Error(err))
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
