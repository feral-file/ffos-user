package main

import (
	"context"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Cache complete, independently playable representations. A manifest can
// reference remote segments and cannot be replayed as a standalone local fd.
func expires(h http.Header, now time.Time) time.Time {
	typ, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		return time.Time{}
	}
	switch typ {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/avif", "image/bmp", "video/mp4", "video/webm", "video/quicktime", "audio/mpeg", "audio/mp4", "audio/ogg", "audio/flac", "audio/wav":
	default:
		return time.Time{}
	}
	if h.Get("Set-Cookie") != "" || h.Get("Content-Range") != "" || (h.Get("Content-Encoding") != "" && h.Get("Content-Encoding") != "identity") {
		return time.Time{}
	}
	for _, field := range strings.Split(strings.ToLower(strings.Join(h.Values("Vary"), ",")), ",") {
		switch strings.TrimSpace(field) {
		case "", "accept-encoding", "user-agent", "origin":
		default:
			return time.Time{}
		}
	}
	var seconds int64
	for _, directive := range strings.Split(strings.Join(h.Values("Cache-Control"), ","), ",") {
		key, value, _ := strings.Cut(strings.TrimSpace(strings.ToLower(directive)), "=")
		switch key {
		case "private", "no-store", "no-cache":
			return time.Time{}
		case "max-age":
			if seconds != 0 {
				return time.Time{}
			}
			seconds, err = strconv.ParseInt(strings.Trim(value, "\""), 10, 64)
			if err != nil || seconds <= 0 || seconds > 315360000 {
				return time.Time{}
			}
		}
	}
	date, err := http.ParseTime(h.Get("Date"))
	if err != nil || date.After(now) {
		date = now
	}
	var end time.Time
	if seconds > 0 {
		end = date.Add(time.Duration(seconds) * time.Second)
	} else {
		end, _ = http.ParseTime(h.Get("Expires"))
	}
	if raw := h.Get("Age"); raw != "" {
		age, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || age < 0 || age > 315360000 {
			return time.Time{}
		}
		remaining := end.Sub(date) - time.Duration(age)*time.Second
		if aged := now.Add(remaining); aged.Before(end) {
			end = aged
		}
	}
	return end
}

func fill(ctx context.Context, root string, r request, client *http.Client) error {
	target := filepath.Join(root, "media", r.Key)
	if info, err := os.Stat(target); err == nil && info.ModTime().After(time.Now()) && lastUse(info).After(time.Now().AddDate(0, 0, -r.MaxAgeDays)) {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", r.UserAgent)
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	end := expires(resp.Header, time.Now())
	size := resp.ContentLength
	if resp.StatusCode != http.StatusOK || !end.After(time.Now()) || size <= 0 || size > r.MaxBytes {
		return nil
	}
	room, err := prune(root, r, size, time.Now())
	if err != nil || !room {
		return err
	}
	path := filepath.Join(root, "media", ".partial")
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(path)

	// Recheck free space while streaming: another application can consume the reserve.
	defer func() {
		if out != nil {
			_ = out.Close()
		}
	}()
	start := time.Now()
	const bytesPerSecond = 2 << 20
	for remaining := size; remaining > 0; {
		room, err := available(root)
		if err != nil || room < remaining {
			return err
		}
		n, err := io.CopyN(out, resp.Body, min(remaining, 256<<10))
		if err != nil {
			return err
		}
		remaining -= n
		// Pace speculative network and disk traffic behind the live decoder.
		due := start.Add(time.Duration(float64(size-remaining) / bytesPerSecond * float64(time.Second)))
		if delay := time.Until(due); delay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	if ctx.Err() != nil || !end.After(time.Now()) {
		return ctx.Err()
	}
	if err = out.Close(); err != nil {
		return err
	}
	out = nil
	if err = os.Chtimes(path, time.Now(), end); err != nil {
		return err
	}
	return os.Rename(path, target)
}
