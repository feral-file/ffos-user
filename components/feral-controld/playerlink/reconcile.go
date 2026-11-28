package playerlink

import (
	"context"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Reconciler observes runtime instances, never page documents. It restores only
// device-owned intent; the host restores its playlist before exposing its socket.
// Artwork navigations and occurrence changes must not trigger these callbacks.
type Reconciler struct {
	Source interface {
		Watch(context.Context, func(SnapshotDelivery)) error
	}
	Logger     *zap.Logger
	steps      []func(context.Context)
	generation atomic.Uint64
	hook       func()
}

func (r *Reconciler) RegisterReconciler(_ string, fn func(context.Context)) {
	r.steps = append(r.steps, fn)
}
func (r *Reconciler) Generation() uint64          { return r.generation.Load() }
func (r *Reconciler) SetGenerationHook(fn func()) { r.hook = fn }

func (r *Reconciler) Run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	updates := make(chan SnapshotDelivery, 1)
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			err := r.Source.Watch(ctx, func(d SnapshotDelivery) {
				select {
				case updates <- d:
				default:
					select {
					case <-updates:
					default:
					}
					select {
					case updates <- d:
					default:
					}
				}
			})
			if ctx.Err() != nil {
				return
			}
			r.Logger.Debug("Native player watch reconnecting", zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
	instance := ""
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-updates:
			if d.Snapshot.Validate() != nil || d.Snapshot.Instance == instance {
				continue
			}
			instance = d.Snapshot.Instance
			r.generation.Add(1)
			if r.hook != nil {
				r.hook()
			}
			for _, step := range r.steps {
				if ctx.Err() != nil {
					return
				}
				step(ctx)
			}
		}
	}
}
