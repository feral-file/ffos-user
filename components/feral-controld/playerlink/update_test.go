package playerlink

import (
	"context"
	"errors"
	"testing"
)

func TestCPPPlaylistUpdate(t *testing.T) {
	client := New(startCPPHost(t))
	defer func() { _ = client.Close() }()
	ctx := context.Background()
	items := []any{map[string]any{"id": "one", "source": "https://example.org/one"}}
	if err := client.Call(ctx, "playback.replace", map[string]any{"playlist": map[string]any{"items": items}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	before, err := client.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		UpdateID string `json:"updateId"`
		Outcome  string `json:"outcome"`
	}
	params := map[string]any{"items": []any{map[string]any{"id": "two", "source": "https://example.org/two"}}}
	if err := client.Call(ctx, "playback.update", params, &receipt, before.Preconditions()); err != nil {
		t.Fatal(err)
	}
	after, err := client.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Outcome != "accepted" || after.PendingUpdate == nil || after.PendingUpdate.ID != receipt.UpdateID || after.Selected.ID != before.Selected.ID || after.UpdateOutcome.Outcome != "deferred" {
		t.Fatalf("refresh replaced the loading occurrence or lost its durable result: %+v", after)
	}
	stale := before.Preconditions()
	err = client.Call(ctx, "playback.update", params, nil, stale)
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "stale_generation" {
		t.Fatalf("stale refresh was not rejected: %v", err)
	}
}
