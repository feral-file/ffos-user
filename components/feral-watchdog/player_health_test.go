package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
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

func TestHealthySocketResetsRestartBudget(t *testing.T) {
	path := useFallbackStateFile(t, CHROMIUM_MAX_FALLBACK_REBOOTS)
	m := NewChromiumMonitor(zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	m.probe = func(context.Context) error { return nil }
	require.NoError(t, m.check(context.Background()))
	require.True(t, m.hasEverConnected)
	require.NoFileExists(t, path)
}

func TestNativeHealthSignalChecksReply(t *testing.T) {
	for _, tc := range []struct {
		name, method, reply string
		want                string
	}{
		{"apply hold", "health.set", `{"outcome":"applied"}`, ""},
		{"declined hold", "health.set", `{"outcome":"accepted"}`, "did not apply"},
		{"invalid receipt", "health.set", `[]`, "unmarshal"},
		{"unsupported version", "hello", `{"version":2,"instance":"host"}`, "invalid player negotiation"},
		{"missing instance", "hello", `{"version":1}`, "invalid player negotiation"},
		{"wrong negotiation shape", "hello", `"invalid"`, "unmarshal"},
		{"wrong health shape", "health.get", `"invalid"`, "unmarshal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "ff-signal-")
			require.NoError(t, err)
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			path := filepath.Join(dir, "p.sock")
			listener, err := net.Listen("unix", path)
			require.NoError(t, err)
			defer func() { _ = listener.Close() }()
			require.NoError(t, os.Chmod(path, 0600))
			type request struct {
				Method, Instance, Key string
				Generation            uint64
				Params                map[string]any
			}
			requests := make(chan request, 3)
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer close(requests)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				for {
					var header [4]byte
					if _, err = io.ReadFull(conn, header[:]); err != nil {
						return
					}
					size := binary.BigEndian.Uint32(header[:])
					if size > 4096 {
						return
					}
					body := make([]byte, size)
					if _, err = io.ReadFull(conn, body); err != nil {
						return
					}
					var req request
					if err = json.Unmarshal(body, &req); err != nil {
						return
					}
					requests <- req
					reply := tc.reply
					if req.Method != tc.method {
						switch req.Method {
						case "hello":
							reply = `{"version":1,"instance":"host"}`
						case "health.get":
							reply = `{"host":"responsive","instance":"host","generation":7,"revision":2}`
						default:
							return
						}
					}
					data := []byte(`{"v":1,"id":"watchdog","result":` + reply + `}`)
					binary.BigEndian.PutUint32(header[:], uint32(len(data))) //nolint:gosec // All reply bodies are fixed test fixtures.
					if _, err = conn.Write(append(header[:], data...)); err != nil {
						return
					}
					if req.Method == tc.method {
						return
					}
				}
			}()
			err = nativePlayerRequest(context.Background(), path, map[string]bool{"thermal": true})
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
			<-done
			var last request
			for req := range requests {
				last = req
			}
			require.Equal(t, tc.method, last.Method)
			if tc.method == "health.set" {
				require.Equal(t, "host", last.Instance)
				require.EqualValues(t, 7, last.Generation)
				require.Len(t, last.Key, 32)
				require.Equal(t, true, last.Params["thermal"])
			}
		})
	}
}

func TestNativeHealthSocketLocation(t *testing.T) {
	t.Setenv("FF_PLAYER_SOCKET", "/tmp/explicit")
	require.Equal(t, "/tmp/explicit", nativePlayerSocket())
	t.Setenv("FF_PLAYER_SOCKET", "")
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/run")
	require.Equal(t, "/tmp/run/ff-player/player.sock", nativePlayerSocket())
	t.Setenv("XDG_RUNTIME_DIR", "")
	require.Equal(t, filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "ff-player/player.sock"), nativePlayerSocket())
}
