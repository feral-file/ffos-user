package offlinecache

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var errAutomaticUncacheable = errors.New("automatic cache: response requires live playback")

// Short-lived media stays on the browser's normal path. Repeated full-file
// refreshes of large assets would otherwise consume bandwidth throughout play.
const automaticMinFreshLifetime = time.Hour

type automaticCaptureKey struct{}

// Automatic capture is a performance optimization, not an offline snapshot.
// A separate context bit keeps explicit Download's existing semantics intact.
func isAutomaticCapture(ctx context.Context) bool {
	intent, _ := ctx.Value(automaticCaptureKey{}).(*atomic.Bool)
	return intent != nil && intent.Load()
}

func (r Resource) freshAt(now time.Time) bool {
	return r.FreshUntil.IsZero() || now.Before(r.FreshUntil)
}

func (r *ItemRecord) freshForPlayback(now time.Time) bool {
	if !r.Automatic {
		return true
	}
	if len(r.Resources) == 0 {
		return false
	}
	for _, resource := range r.Resources {
		if resource.FreshUntil.IsZero() || !resource.freshAt(now) {
			return false
		}
	}
	return true
}

// automaticFreshUntil deliberately has no heuristic TTL. The source-keyed
// store cannot represent cookies or arbitrary Vary dimensions; let Chromium
// handle those responses, and all responses requiring revalidation.
func automaticFreshUntil(h http.Header, now time.Time) time.Time {
	if len(h.Values("Set-Cookie")) != 0 {
		return time.Time{}
	}
	for _, field := range strings.Split(strings.Join(h.Values("Vary"), ","), ",") {
		switch strings.ToLower(strings.TrimSpace(field)) {
		case "", "accept-encoding", "origin":
		default:
			return time.Time{}
		}
	}
	var lifetime time.Duration
	hasMaxAge := false
	for _, field := range strings.Split(strings.Join(h.Values("Cache-Control"), ","), ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(field), "=")
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "no-store", "no-cache", "private":
			return time.Time{}
		case "max-age":
			if hasMaxAge {
				return time.Time{}
			}
			hasMaxAge = true
			seconds, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(value), "\""), 10, 64)
			// Bound duration arithmetic, including hostile header values.
			if err != nil || seconds <= 0 || seconds > int64((10*365*24*time.Hour)/time.Second) {
				return time.Time{}
			}
			lifetime = time.Duration(seconds) * time.Second
		}
	}
	date, dateErr := http.ParseTime(h.Get("Date"))
	if !hasMaxAge {
		expires, err := http.ParseTime(h.Get("Expires"))
		if err != nil {
			return time.Time{}
		}
		if dateErr == nil {
			lifetime = expires.Sub(date)
		} else {
			lifetime = expires.Sub(now)
		}
	}
	age := time.Duration(0)
	if raw := h.Get("Age"); raw != "" {
		seconds, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || seconds < 0 || seconds > int64((10*365*24*time.Hour)/time.Second) {
			return time.Time{}
		}
		age = time.Duration(seconds) * time.Second
	}
	if dateErr == nil {
		age = max(age, now.Sub(date))
	}
	if lifetime <= age {
		return time.Time{}
	}
	return now.Add(lifetime - age)
}
