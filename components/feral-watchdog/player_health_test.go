package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestNativeHealthValidatesProtocolNotArtwork(t *testing.T) {
	for _, test := range []struct {
		name, instance, host string
		generation           uint64
		valid                bool
	}{
		{"failed renderer still has usable host", "runtime", "responsive", 1, true},
		{"instance changed on connection", "other", "responsive", 1, false},
		{"unresponsive host", "runtime", "blocked", 1, false},
		{"invalid generation", "runtime", "responsive", 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "ff-health-")
			require.NoError(t, err)
			defer func() { _ = os.RemoveAll(dir) }()
			path := filepath.Join(dir, "p.sock")
			listener, err := net.Listen("unix", path)
			require.NoError(t, err)
			defer func() { _ = listener.Close() }()
			require.NoError(t, os.Chmod(path, 0600))
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				for _, result := range []any{map[string]any{"version": 1, "instance": "runtime"}, map[string]any{"instance": test.instance, "host": test.host, "generation": test.generation, "revision": 1, "renderer": "failed"}} {
					var header [4]byte
					if _, err = io.ReadFull(conn, header[:]); err != nil {
						return
					}
					n := binary.BigEndian.Uint32(header[:])
					if n > 4096 {
						return
					}
					if _, err = io.CopyN(io.Discard, conn, int64(n)); err != nil {
						return
					}
					body, _ := json.Marshal(map[string]any{"v": 1, "id": "watchdog", "result": result})
					if len(body) > 4194304 {
						return
					}
					binary.BigEndian.PutUint32(header[:], uint32(len(body))) //nolint:gosec // Test frame length is bounded above.
					// Deliberately fragment both header and payload.
					for _, b := range append(header[:], body...) {
						if _, err = conn.Write([]byte{b}); err != nil {
							return
						}
					}
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err = probeNativePlayer(ctx, path)
			if test.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			<-done
		})
	}
}

func TestNativeMonitorIgnoresRetiredCDPReport(t *testing.T) {
	m := NewChromiumMonitor("unused", zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	m.SetCDPStuck(true)
	require.False(t, m.cdpStuck)
	m.probe = func(context.Context) error { return nil }
	require.NoError(t, m.check(context.Background()))
	require.True(t, m.hasEverConnected)
	// Unlike an HTTP /json/version reply, this probe has exercised exactly
	// the host command event loop. No second daemon confirmation is needed.
	require.True(t, m.cdpConfirmedHealthy)
}
