package mediahealth

import (
	"errors"
	"net/url"
	"strconv"

	"go.uber.org/zap"
)

// Navigation is one artwork-frame navigation the daemon observed on the
// kiosk over CDP (offlinecache.KioskNavigationObserver): the HTTP status
// or the network error of the DOCUMENT the player's embed loaded.
//
// Why this exists: a page may not learn anything about a cross-origin
// iframe's document — not its status, not whether an error page loaded —
// so the player reports `ready` for Chromium's own 404 page and the play
// layer saw a success where the wall showed an error (#392, "Infinite
// Descent" on the office FF1). DevTools is not bound by that rule: it
// reports every request the page makes with its real status, whatever
// the origin. The observation is folded into the SAME play-run and
// corroboration rules as the player's own ring entries, so it never
// concludes on its own: two failed plays, or one over a standing L0
// verdict, and only while the device is online.
type Navigation struct {
	// URL is the request URL as the frame asked for it — the source plus
	// whatever the player appended (see playerQueryParams), BEFORE the
	// kiosk gateway rewrite, which is invisible here by design.
	URL string
	// Status is the final response's HTTP status (after redirects), 0 when
	// no response arrived.
	Status int
	// Error is Chromium's error text (net::ERR_NAME_NOT_RESOLVED, ...)
	// when the load failed before a response.
	Error string
	// Canceled marks a load the page itself abandoned (the player replaces
	// a frame's URL, or gives up on it at its watchdog): never evidence.
	Canceled bool
}

// playerQueryParams are the query keys the player appends to a source when
// it mounts an embed (`display_mode` carries the display settings to the
// work). Stripped before matching a navigation to a wall source.
var playerQueryParams = map[string]bool{"display_mode": true}

// ObserveNavigation folds one observed frame navigation into the record
// of the wall source it belongs to. A 2xx proves the mount, not the work
// (the player's own `ready`/decode entries judge the content), so it is
// never a success — but it does clear a probe-side Issue (ApplyKioskAnswer).
// A 3xx is a hop, ignored. A definite status ≥ 400 is a `detect` failure with that
// status — the same shape as the player's own content-type check, so the
// existing table applies (404/410 → source_missing, 401/403/429 →
// source_blocked, 5xx → render_failed) — and a network error is a `load`
// failure carrying Chromium's reason. Navigations for URLs not on the
// wall (the player's own page, overlays) are ignored.
func (s *Service) ObserveNavigation(nav Navigation) {
	if nav.Canceled || (nav.Status == 0 && nav.Error == "") || (nav.Status >= 300 && nav.Status < 400) {
		return
	}
	online := s.online != nil && s.online()
	s.mu.Lock()
	defer s.mu.Unlock()
	src, ok := s.matchWallSource(nav.URL)
	if !ok {
		return
	}
	if nav.Status >= 200 && nav.Status < 300 {
		// The kiosk's own evidence wins over the probe's: the origin served
		// this document to the kiosk, so a probe-side issue is no longer
		// the truth. Not a success (the work may still fail to render),
		// so no outcome is applied and the play run is left alone.
		if err := s.store.ApplyKioskAnswer(src); err != nil && !errors.Is(err, ErrLowDisk) {
			s.logger.Warn("media health: recording a kiosk-served document failed", zap.Error(err))
		}
		return
	}
	o := Outcome{Source: src, Kind: OutcomeFail}
	if nav.Status >= 400 {
		o.Stage, o.Code = StageDetect, strconv.Itoa(nav.Status)
	} else {
		o.Stage, o.Code = StageLoad, nav.Error
	}
	counted := s.applyEntry(o, online)
	if nav.Status >= 400 {
		// Any error status on the DOCUMENT itself means the frame shows an
		// error page, so the `ready` the player reports next is not a
		// success. definiteRefusal (applied inside applyEntry) excludes
		// 5xx and 405/408/416 for the player's own content-type check,
		// where the real fetch may still succeed; here this IS the real
		// fetch.
		s.runRefused = true
	}
	s.logger.Info("media health: kiosk frame navigation failed",
		zap.String("source", redactForLog(src)),
		zap.Int("status", nav.Status),
		zap.String("error", nav.Error),
		zap.Bool("counted", counted))
}

// matchWallSource finds the wall source a navigation URL belongs to:
// same URL once the fragment (never sent) and the player's own query
// params are stripped from both. Walked from the current index forward so
// that, of two fragment variants of one URL, the one the player is about
// to show wins. Caller holds s.mu.
func (s *Service) matchWallSource(rawURL string) (string, bool) {
	want := navigationKey(rawURL)
	if want == "" || len(s.wall) == 0 {
		return "", false
	}
	n := len(s.wall)
	start := s.wallIndex
	if start < 0 || start >= n {
		start = 0
	}
	for step := 0; step < n; step++ {
		src := s.wall[(start+step)%n]
		if src == "" || IsDataURI(src) {
			continue
		}
		if navigationKey(src) == want {
			return src, true
		}
	}
	// A request the kiosk loaded through a fallback gateway reports that
	// gateway's URL: match it by content id, the identity every gateway
	// shares.
	if ref, ok := s.store.gateways.Ref(rawURL); ok {
		for step := 0; step < n; step++ {
			src := s.wall[(start+step)%n]
			if other, ok := s.store.gateways.Ref(src); ok && other.Key() == ref.Key() && other.Rest == ref.Rest {
				return src, true
			}
		}
	}
	return "", false
}

// navigationKey is a URL without its fragment and without the player's own
// query params, in canonical query order so both sides compare equal
// whatever their original encoding.
func navigationKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	u.Fragment = ""
	if u.RawQuery != "" {
		q := u.Query()
		for k := range playerQueryParams {
			q.Del(k)
		}
		u.RawQuery = q.Encode()
	}
	return u.String()
}
