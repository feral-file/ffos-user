// ff-artwork-cache runs one best-effort job for its parent player. stdin is a
// private seqpacket socket: losing the parent cancels an in-flight HTTP body.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"feralfile.local/player/egress/internal/netpolicy"
)

type request struct {
	Key, URL, UserAgent string
	LAN                 bool
	MaxAgeDays          int
	MaxBytes            int64
}

func main() {
	var r request
	packet := make([]byte, 65536)
	n, err := os.Stdin.Read(packet)
	if err != nil || n == len(packet) || json.Unmarshal(packet[:n], &r) != nil || r.MaxAgeDays < 1 || r.MaxAgeDays > 36500 || r.MaxBytes < 1 || r.MaxBytes > 1<<50 {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); cancel() }()
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return netpolicy.Dial(ctx, network, addr, r.LAN)
	}, DisableCompression: true, ResponseHeaderTimeout: 20 * time.Second}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, 10)
	_ = run(ctx, filepath.Join(home, ".cache", "player-artworks"), r, client)
}

func run(ctx context.Context, root string, r request, client *http.Client) error {
	if r.Key != "" {
		u, err := url.Parse(r.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
			return nil
		}
		if !validKey(r.Key) {
			return errors.New("invalid cache key")
		}
	}
	media := filepath.Join(root, "media")
	if err := os.MkdirAll(media, 0700); err != nil {
		return err
	}
	// One writer across overlapping occurrences, sweeps, and host restarts.
	lock, err := os.Open(root)
	if err != nil {
		return err
	}
	defer lock.Close()
	mode := syscall.LOCK_EX
	if r.Key == "" {
		mode |= syscall.LOCK_NB
	}
	if err = syscall.Flock(int(lock.Fd()), mode); err != nil {
		return err
	}
	partial := filepath.Join(media, ".partial")
	if err = os.Remove(partial); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if r.Key == "" {
		_, err = prune(root, r, 0, time.Now())
		return err
	}
	return fill(ctx, root, r, client)
}
