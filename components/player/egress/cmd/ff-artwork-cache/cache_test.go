package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReusableWholeMedia(t *testing.T) {
	hits := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Cache-Control", "public, max-age=600")
		io.WriteString(w, "media")
	}))
	defer origin.Close()
	root := t.TempDir()
	r := request{Key: strings.Repeat("a", 64), URL: origin.URL, MaxBytes: 100, MaxAgeDays: 30}
	for i := 0; i < 2; i++ {
		if err := run(context.Background(), root, r, origin.Client()); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(root, "media", r.Key))
	if err != nil || string(raw) != "media" || hits != 1 {
		t.Fatalf("body=%q, requests=%d, err=%v", raw, hits, err)
	}
}
func TestResponseEligibility(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		mime, control, vary, age string
		fresh                    bool
	}{
		{"video/mp4", "max-age=600", "", "5", true},
		{"video/mp4", "private, max-age=600", "", "", false},
		{"video/mp4", "no-store, max-age=600", "", "", false},
		{"audio/mpeg", "max-age=20", "", "30", false},
		{"image/png", "max-age=60", "Cookie", "", false},
		{"application/vnd.apple.mpegurl", "max-age=60", "", "", false},
	} {
		h := http.Header{}
		h.Set("Content-Type", tc.mime)
		h.Set("Cache-Control", tc.control)
		h.Set("Vary", tc.vary)
		h.Set("Age", tc.age)
		if expires(h, now).After(now) != tc.fresh {
			t.Errorf("unexpected freshness for %v", tc)
		}
	}
}
func TestSharedRetention(t *testing.T) {
	root := t.TempDir()
	media := filepath.Join(root, "media")
	browser := filepath.Join(root, strings.Repeat("b", 64))
	for _, d := range []string{media, browser} {
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	old := now.AddDate(0, 0, -31)
	asset := filepath.Join(media, strings.Repeat("c", 64))
	for _, p := range []string{asset, filepath.Join(browser, "asset")} {
		if err := os.WriteFile(p, []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(asset, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(browser, old, old); err != nil {
		t.Fatal(err)
	}
	d, err := os.Open(browser)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = syscall.Flock(int(d.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	r := request{MaxAgeDays: 30, MaxBytes: 4}
	if _, err = prune(root, r, 0, now); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(asset); !os.IsNotExist(err) {
		t.Fatalf("expected media eviction: %v", err)
	}
	if _, err = os.Stat(browser); err != nil {
		t.Fatalf("live browser evicted: %v", err)
	}
	if err = syscall.Flock(int(d.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if _, err = prune(root, r, 0, now.AddDate(0, 0, 31)); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(browser); !os.IsNotExist(err) {
		t.Fatalf("expired browser retained: %v", err)
	}
}

func TestLocalSourcesNeverTouchCache(t *testing.T) {
	for _, source := range []string{"/home/feralfile/art.mp4", "./art.mp4", "file:///home/feralfile/art.mp4", "fd://3", "data:image/png;base64,AA==", "ff-render://bundle/setup-artwork/index.html"} {
		t.Run(source, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "cache")
			r := request{Key: strings.Repeat("a", 64), URL: source, MaxBytes: 100, MaxAgeDays: 30}
			if err := run(context.Background(), root, r, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("local source touched cache: %v", err)
			}
		})
	}
}
