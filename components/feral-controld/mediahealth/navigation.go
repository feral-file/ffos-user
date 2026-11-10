package mediahealth

import (
	"errors"
	"net/url"
	"strconv"
	"time"

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
// kioskFailedWindow bounds how long a failed document navigation is kept
// for the wall frame's sake (Service.kioskFailed): a mount's frame comes
// within the load watchdog (45 s) plus the settle (15 s) of its
// navigations; older entries are a leak, not evidence.
const kioskFailedWindow = 10 * time.Minute

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
		now := s.now()
		if s.kioskServed == nil {
			s.kioskServed = map[string]time.Time{}
		}
		for k, t := range s.kioskServed {
			if now.Sub(t) > headGetWindow {
				delete(s.kioskServed, k)
			}
		}
		s.kioskServed[src] = now
		delete(s.kioskFailed, src) // the latest document answer is a good one
		if s.runSource == src {
			// A refusal earlier in this play was the HEAD's, not the
			// document's: the ready that follows is not an error page.
			s.runRefused = false
		}
		// The kiosk's own evidence wins over the probe's: the origin served
		// this document to the kiosk, so a probe-side issue is no longer
		// the truth. Not a success (the work may still fail to render),
		// so no outcome is applied and the play run is left alone.
		reverted, err := s.store.ApplyKioskAnswer(src)
		if err != nil && !errors.Is(err, ErrLowDisk) {
			s.logger.Warn("media health: recording a kiosk-served document failed", zap.Error(err))
		}
		if reverted && s.runSource == src {
			// The play's count was the HEAD's and is gone: its next real
			// failure counts (Service.runWithdrawn).
			s.runWithdrawn = true
		}
		return
	}
	if s.kioskFailed == nil {
		s.kioskFailed = map[string]kioskFailure{}
	}
	for k, f := range s.kioskFailed {
		if s.now().Sub(f.at) > kioskFailedWindow {
			delete(s.kioskFailed, k)
		}
	}
	s.kioskFailed[src] = kioskFailure{at: s.now(), status: nav.Status}
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
	if want == "" {
		return "", false
	}
	// The wall first, from the item on screen; then the playlist last
	// pushed (Service.pushed): a cast's first item loads before the next
	// poll reports the new wall.
	if src, ok := matchSource(s.wall, s.wallIndex, want, rawURL, s.store.gateways); ok {
		return src, true
	}
	return matchSource(s.pushed, 0, want, rawURL, s.store.gateways)
}

// matchSource finds the source in list that rawURL is a navigation of,
// scanning from start: by navigation key first, then — a request the kiosk
// loaded through a fallback gateway reports that gateway's URL — by the
// content id every gateway shares.
func matchSource(list []string, start int, want, rawURL string, gateways *Gateways) (string, bool) {
	n := len(list)
	if n == 0 {
		return "", false
	}
	if start < 0 || start >= n {
		start = 0
	}
	for step := 0; step < n; step++ {
		src := list[(start+step)%n]
		if src == "" || IsDataURI(src) {
			continue
		}
		if navigationKey(src) == want {
			return src, true
		}
	}
	if ref, ok := gateways.Ref(rawURL); ok {
		for step := 0; step < n; step++ {
			src := list[(start+step)%n]
			if other, ok := gateways.Ref(src); ok && other.Key() == ref.Key() && other.Rest == ref.Rest {
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
