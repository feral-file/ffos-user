package ff1config

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestIsLocalBundlePlayerURL(t *testing.T) {
	t.Parallel()
	if !IsLocalBundlePlayerURL(DefaultWebappURL) {
		t.Fatalf("expected default webapp URL to be local bundle")
	}
	if !IsLocalBundlePlayerURL("http://127.0.0.1:8080") {
		t.Fatalf("expected host:port without slash")
	}
	if !IsLocalBundlePlayerURL("  http://127.0.0.1:8080/  ") {
		t.Fatalf("expected leading/trailing whitespace to be ignored")
	}
	if IsLocalBundlePlayerURL("https://127.0.0.1:8080/") {
		t.Fatalf("https must not match")
	}
	if IsLocalBundlePlayerURL("http://127.0.0.1:9090/") {
		t.Fatalf("wrong port must not match")
	}
	if IsLocalBundlePlayerURL("https://display.feralfile.com/") {
		t.Fatalf("remote must not match")
	}
}

func TestLauncherMessageNavigateURL_encodesQuery(t *testing.T) {
	t.Parallel()
	u := LauncherMessageNavigateURL("a b&c")
	if u == LauncherMessageURLPrefix+"a b&c" {
		t.Fatalf("expected query escaping, got %q", u)
	}
}

func TestPollTCPUntilOpen_succeeds(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("close listener: %v", err)
		}
	})
	addr := ln.Addr().String()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := PollTCPUntilOpen(ctx, addr); err != nil {
		t.Fatal(err)
	}
}

func TestPollTCPUntilOpen_respectsShortDeadline(t *testing.T) {
	t.Parallel()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err := PollTCPUntilOpen(ctx, "127.0.0.1:1")
	if err == nil {
		t.Fatal("expected error when nothing listens")
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("expected bounded wait (~parent deadline), took %v", took)
	}
}
