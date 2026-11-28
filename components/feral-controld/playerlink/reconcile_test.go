package playerlink

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type watchFeed struct{ updates chan SnapshotDelivery }

func (w *watchFeed) Watch(ctx context.Context, send func(SnapshotDelivery)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d := <-w.updates:
			send(d)
		}
	}
}
func TestReconcileOnlyRuntimeChangesAndJoinsWatch(t *testing.T) {
	w := &watchFeed{updates: make(chan SnapshotDelivery)}
	r := &Reconciler{Source: w, Logger: zap.NewNop()}
	steps := make(chan uint64, 4)
	r.RegisterReconciler("device-intent", func(context.Context) { steps <- r.Generation() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer cancel()
	send := func(instance string, generation uint64) {
		w.updates <- SnapshotDelivery{Snapshot: Snapshot{Instance: instance, Generation: generation, Revision: 1, Host: "responsive"}}
	}
	send("one", 1)
	select {
	case n := <-steps:
		require.Equal(t, uint64(1), n)
	case <-time.After(time.Second):
		t.Fatal("missing initial reconcile")
	}
	send("one", 2)
	send("one", 3)
	select {
	case <-steps:
		t.Fatal("artwork changes must not reconcile playback")
	case <-time.After(20 * time.Millisecond):
	}
	send("two", 1)
	select {
	case n := <-steps:
		require.Equal(t, uint64(2), n)
	case <-time.After(time.Second):
		t.Fatal("missing restart reconcile")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watch did not stop")
	}
}
