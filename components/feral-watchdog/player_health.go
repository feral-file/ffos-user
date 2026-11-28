package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

func nativePlayerSocket() string {
	if path := os.Getenv("FF_PLAYER_SOCKET"); path != "" {
		return path
	}
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		runtime = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return filepath.Join(runtime, "ff-player/player.sock")
}

// probeNativePlayer checks the host's actual command event loop. A renderer
// error is a healthy host reporting a failed work, not a process restart cause.
// The socket is private to the service UID; validate its owner before dialing.
func probeNativePlayer(ctx context.Context, path string) error {
	return nativePlayerRequest(ctx, path, nil)
}

func nativePlayerRequest(ctx context.Context, path string, health map[string]bool) error {
	if err := validatePlayerSocket(path); err != nil {
		return err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err = validateNativePeer(conn); err != nil {
		return err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(3 * time.Second)
	}
	if err = conn.SetDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	exchange := func(request any) (json.RawMessage, error) {
		data, err := json.Marshal(request)
		if err != nil {
			return nil, err
		}
		if len(data) > 4194304 {
			return nil, errors.New("request exceeds frame limit")
		}
		frame := make([]byte, 4+len(data))
		binary.BigEndian.PutUint32(frame, uint32(len(data))) //nolint:gosec // len is nonnegative and capped at 4 MiB above.
		copy(frame[4:], data)
		for len(frame) > 0 {
			n, err := conn.Write(frame)
			if err != nil {
				return nil, err
			}
			if n == 0 {
				return nil, io.ErrShortWrite
			}
			frame = frame[n:]
		}
		var size [4]byte
		if _, err = io.ReadFull(conn, size[:]); err != nil {
			return nil, err
		}
		length := binary.BigEndian.Uint32(size[:])
		if length == 0 || length > 4194304 {
			return nil, errors.New("invalid player frame length")
		}
		data = make([]byte, length)
		if _, err = io.ReadFull(conn, data); err != nil {
			return nil, err
		}
		var reply struct {
			Version int             `json:"v"`
			ID      string          `json:"id"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if err = json.Unmarshal(data, &reply); err != nil {
			return nil, err
		}
		if reply.Version != 1 || reply.ID != "watchdog" || len(reply.Error) > 0 || len(reply.Result) == 0 {
			return nil, errors.New("invalid player health reply")
		}
		return reply.Result, nil
	}
	hello, err := exchange(map[string]any{"v": 1, "id": "watchdog", "method": "hello", "params": map[string]any{"client": "watchdog", "versions": []int{1}}})
	if err != nil {
		return err
	}
	var negotiation struct {
		Version  int    `json:"version"`
		Instance string `json:"instance"`
	}
	if err = json.Unmarshal(hello, &negotiation); err != nil {
		return err
	}
	if negotiation.Version != 1 || negotiation.Instance == "" {
		return errors.New("invalid player negotiation")
	}
	result, err := exchange(map[string]any{"v": 1, "id": "watchdog", "method": "health.get", "deadlineMs": deadline.UnixMilli(), "params": map[string]any{}})
	if err != nil {
		return err
	}
	var state struct {
		Instance   string `json:"instance"`
		Host       string `json:"host"`
		Generation uint64 `json:"generation"`
		Revision   uint64 `json:"revision"`
	}
	if err = json.Unmarshal(result, &state); err != nil {
		return err
	}
	if state.Instance != negotiation.Instance || state.Host != "responsive" || state.Generation == 0 || state.Revision == 0 {
		return errors.New("native host is not responsive")
	}
	if health != nil {
		var entropy [16]byte
		if _, err = rand.Read(entropy[:]); err != nil {
			return err
		}
		result, err = exchange(map[string]any{"v": 1, "id": "watchdog", "method": "health.set", "params": health, "instance": state.Instance, "generation": state.Generation, "key": hex.EncodeToString(entropy[:]), "deadlineMs": deadline.UnixMilli()})
		if err != nil {
			return err
		}
		var receipt struct {
			Outcome string `json:"outcome"`
		}
		if err = json.Unmarshal(result, &receipt); err != nil {
			return err
		}
		if receipt.Outcome != "applied" {
			return errors.New("host did not apply health signal")
		}
	}
	return nil
}
