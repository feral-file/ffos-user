package playerlink

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

type SnapshotDelivery struct {
	Snapshot Snapshot
	Initial  bool
	Gap      bool
}

// Watch uses a separate connection from commands. Each delivery is a complete
// replacement snapshot, including after a gap. The callback runs synchronously
// with bounded transport buffers; it must copy state quickly, not perform I/O.
// A disconnected watch returns an error. Its owner decides reconnect cadence;
// each new Watch starts with a fresh snapshot and has no event replay promise.
func (c *Client) Watch(ctx context.Context, deliver func(SnapshotDelivery)) error {
	if deliver == nil {
		return errors.New("native player watch requires a callback")
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	connection, err := dialer.DialContext(ctx, "unix", c.Path)
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()
	if err := validatePeer(connection); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	var hello Hello
	if err := exchange(connection, Request{Version: Version, ID: "watch-hello", Method: "hello", Params: map[string]any{"client": c.Name, "versions": []int{Version}}}, &hello); err != nil {
		return err
	}
	if hello.Version != Version || hello.Instance != hello.Snapshot.Instance {
		return errors.New("native watch negotiation mismatch")
	}
	var initial struct {
		Snapshot Snapshot `json:"snapshot"`
		Cursor   uint64   `json:"cursor"`
	}
	if err := exchange(connection, Request{Version: Version, ID: "watch-subscribe", Method: "state.subscribe", Params: map[string]any{}, DeadlineMS: time.Now().Add(4 * time.Second).UnixMilli()}, &initial); err != nil {
		return err
	}
	if err := initial.Snapshot.Validate(); err != nil {
		return err
	}
	if initial.Snapshot.Instance != hello.Instance || initial.Cursor != initial.Snapshot.Revision {
		return errors.New("native watch initial cursor mismatch")
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		return err
	}
	deliver(SnapshotDelivery{Snapshot: initial.Snapshot, Initial: true})
	cursor := initial.Cursor
	for {
		var event struct {
			Version  int      `json:"v"`
			Event    string   `json:"event"`
			Previous uint64   `json:"previous"`
			Cursor   uint64   `json:"cursor"`
			Snapshot Snapshot `json:"snapshot"`
		}
		if err := ReadFrame(connection, &event); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("native watch: %w", err)
		}
		if err := event.Snapshot.Validate(); err != nil {
			return err
		}
		if event.Version != Version || event.Event != "state" || event.Snapshot.Instance != hello.Instance || event.Cursor != event.Snapshot.Revision || event.Cursor <= cursor {
			return errors.New("native watch event identity/cursor mismatch")
		}
		deliver(SnapshotDelivery{Snapshot: event.Snapshot, Gap: event.Previous != cursor})
		cursor = event.Cursor
	}
}
