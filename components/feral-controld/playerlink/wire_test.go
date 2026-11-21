package playerlink

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSharedConformance(t *testing.T) {
	body, err := os.ReadFile("testdata/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Parse []struct {
			Name, Wire string
			Valid      bool
		}
		Frames []struct{ Name, Body, Hex, Error string }
	}
	if err := json.Unmarshal(body, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures.Parse {
		t.Run(fixture.Name, func(t *testing.T) {
			if err := ValidateJSON([]byte(fixture.Wire)); (err == nil) != fixture.Valid {
				t.Fatalf("valid=%v: %v", fixture.Valid, err)
			}
		})
	}
	for _, fixture := range fixtures.Frames {
		t.Run(fixture.Name, func(t *testing.T) {
			wire, err := hex.DecodeString(fixture.Hex)
			if err != nil {
				t.Fatal(err)
			}
			var value any
			err = ReadFrame(bytes.NewReader(wire), &value)
			if (err != nil) != (fixture.Error != "") {
				t.Fatalf("frame result: %v", err)
			}
			if fixture.Error == "" {
				var output bytes.Buffer
				if err := WriteFrame(&output, value); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(output.Bytes(), wire) {
					t.Fatal("frame bytes differ")
				}
			}
		})
	}
}

// This runs the real C++ host, with no protocol mock on either side. CI/build
// orchestration supplies the independently compiled executable explicitly.
func TestCPPInteroperability(t *testing.T) {
	socket := startCPPHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := New(socket)
	defer func() { _ = client.Close() }()
	var before Snapshot
	if err := client.Call(ctx, "state.get", nil, &before, nil); err != nil {
		t.Fatal(err)
	}
	testCPPPlayback(t, ctx, client, before)
	testCPPRequestFixtures(t, socket)
}

func startCPPHost(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "ff-interop-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket, _ := launchCPPHost(t, directory)
	return socket
}

func launchCPPHost(t *testing.T, directory string) (string, func()) {
	t.Helper()
	binary := os.Getenv("FF_NATIVE_CORE")
	if binary == "" {
		t.Skip("set FF_NATIVE_CORE to compiled ff-player-core for cross-language test")
	}
	socket := filepath.Join(directory, "player.sock")
	command := exec.Command(binary, filepath.Join(directory, "state.sqlite"), socket) //nolint:gosec // G204: explicit test executable supplied by the build, no shell or network input.
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	stop := sync.OnceFunc(func() { _ = command.Process.Kill(); _ = command.Wait() })
	t.Cleanup(stop)
	for i := 0; i < 100; i++ {
		if conn, err := net.DialTimeout("unix", socket, time.Millisecond*50); err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return socket, stop
}

func TestCPPHostRestart(t *testing.T) {
	directory, err := os.MkdirTemp("", "ff-restart-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket, stop := launchCPPHost(t, directory)
	client := New(socket)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	before, err := client.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	list := map[string]any{"items": []any{map[string]any{"source": "https://example.org/restart"}}}
	if err := client.Call(ctx, "playback.replace", map[string]any{"playlist": list}, nil, nil); err != nil {
		t.Fatal(err)
	}
	stop()
	launchCPPHost(t, directory)
	// The first read may discover the closed stream. Reconcile with a fresh read.
	after, err := client.Snapshot(ctx)
	if err != nil {
		after, err = client.Snapshot(ctx)
	}
	if err != nil || after.Instance == before.Instance || !bytes.Contains(after.Playlist, []byte("https://example.org/restart")) {
		t.Fatalf("restart lost the playlist or session: %v, %+v", err, after)
	}
}

func testCPPPlayback(t *testing.T, ctx context.Context, client *Client, before Snapshot) {
	t.Helper()
	playlist := map[string]any{"id": "interop", "title": "test", "items": []any{map[string]any{"source": "https://example.org/", "duration": 1}}}
	var accepted map[string]any
	precondition := &Preconditions{Instance: before.Instance, Generation: before.Generation, Key: "one-playback"}
	if err := client.Call(ctx, "playback.replace", map[string]any{"playlist": playlist}, &accepted, precondition); err != nil {
		t.Fatal(err)
	}
	if accepted["outcome"] != "accepted" {
		t.Fatalf("acceptance: %v", accepted)
	}
	var duplicate map[string]any
	if err := client.Call(ctx, "playback.replace", map[string]any{"playlist": playlist}, &duplicate, precondition); err != nil {
		t.Fatal(err)
	}
	if duplicate["occurrence"] != accepted["occurrence"] {
		t.Fatal("duplicate replayed")
	}
	var after Snapshot
	if err := client.Call(ctx, "state.get", nil, &after, nil); err != nil {
		t.Fatal(err)
	}
	if after.Generation <= before.Generation || after.Presented != nil {
		t.Fatalf("bad state: %+v", after)
	}
	precondition.Key = "stale"
	err := client.Call(ctx, "playback.stop", nil, nil, precondition)
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "stale_generation" {
		t.Fatalf("stale operation: %v", err)
	}
}

func TestCPPWatchAndPersistentFence(t *testing.T) {
	socket := startCPPHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := New(socket)
	defer func() { _ = client.Close() }()
	watchCtx, stopWatch := context.WithCancel(ctx)
	deliveries := make(chan SnapshotDelivery, 8)
	finished := make(chan error, 1)
	go func() {
		finished <- client.Watch(watchCtx, func(delivery SnapshotDelivery) { deliveries <- delivery })
	}()
	next := func() SnapshotDelivery {
		t.Helper()
		select {
		case delivery := <-deliveries:
			return delivery
		case err := <-finished:
			t.Fatalf("watch ended: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return SnapshotDelivery{}
	}
	initial := next()
	if !initial.Initial || initial.Gap {
		t.Fatal("watch did not start with a clean snapshot")
	}
	var state Snapshot
	if err := client.Call(ctx, "state.get", nil, &state, nil); err != nil {
		t.Fatal(err)
	}
	connection := client.connection
	playlist := map[string]any{"items": []any{map[string]any{"source": "https://example.org/"}}}
	if err := client.Call(ctx, "playback.replace", map[string]any{"playlist": playlist}, nil, nil); err != nil {
		t.Fatal(err)
	}
	changed := next()
	if changed.Initial || changed.Gap || changed.Snapshot.Generation <= initial.Snapshot.Generation {
		t.Fatal("replacement event missing")
	}
	// The receipt advances the generation without a new negotiation or read.
	if err := client.Call(ctx, "playback.pause", map[string]any{"paused": true}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if client.connection != connection {
		t.Fatal("commands did not reuse the connection")
	}
	paused := next()
	if !paused.Snapshot.Paused {
		t.Fatal("pause event missing")
	}
	other := New(socket)
	defer func() { _ = other.Close() }()
	if err := other.Call(ctx, "playback.replace", map[string]any{"playlist": playlist}, nil, nil); err != nil {
		t.Fatal(err)
	}
	_ = next()
	// A watch sees this replacement, but does not bless queued commands from
	// the old command lane. The caller must explicitly reconcile their intent.
	err := client.Call(ctx, "playback.pause", map[string]any{"paused": false}, nil, nil)
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "stale_generation" {
		t.Fatalf("external change hidden: %v", err)
	}
	stopWatch()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("watch cancellation: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("watch did not release connection on cancellation")
	}
	// A reconnect delivers current state, without requiring old event history.
	err = client.Watch(ctx, func(delivery SnapshotDelivery) {
		if !delivery.Initial || delivery.Snapshot.Generation <= paused.Snapshot.Generation {
			t.Error("reconnect lost current state")
		}
		cancel()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("reconnected watch cancellation: %v", err)
	}
}

func testCPPRequestFixtures(t *testing.T, path string) {
	t.Helper()
	body, err := os.ReadFile("testdata/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Requests []struct {
			Name, Error string
			Overrides   map[string]any
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	connection, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if err := connection.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var hello Hello
	if err := exchange(connection, Request{Version: 1, ID: "hello-fixtures", Method: "hello", Params: map[string]any{"client": "shared-fixtures", "versions": []int{1}}}, &hello); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures.Requests {
		t.Run(fixture.Name, func(t *testing.T) {
			request := map[string]any{"v": 1, "id": "fixture", "method": "state.get", "params": map[string]any{},
				"instance": hello.Instance, "generation": hello.Snapshot.Generation, "deadlineMs": time.Now().Add(time.Second).UnixMilli(), "key": fixture.Name}
			for key, value := range fixture.Overrides {
				request[key] = value
			}
			if err := WriteFrame(connection, request); err != nil {
				t.Fatal(err)
			}
			var response Response
			if err := ReadFrame(connection, &response); err != nil {
				t.Fatal(err)
			}
			if response.Error == nil || response.Error.Code != fixture.Error {
				t.Fatalf("wanted %s, got %+v", fixture.Error, response)
			}
		})
	}
}
