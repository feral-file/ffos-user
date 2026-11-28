package mediator

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/playerlink"
)

func (m *mediator) SetConnectivityPlayer(player playerlink.ConnectivityPlayer) {
	m.player = player
	if player == nil {
		return
	}
	m.playerCtx, m.stopPlayer = context.WithCancel(m.daemonCtx)
	m.playerDone = make(chan struct{})
	go m.watchConnectivity()
}

func (m *mediator) watchConnectivity() {
	defer close(m.playerDone)
	ctx, cancel := context.WithCancel(m.playerCtx)
	defer cancel()
	// Watch reconnects restore the current level after a host restart. The
	// separate bounded retry clock also repairs a failed command while the
	// subscription remains healthy (or a failed initial connectivity probe).
	retryDone := make(chan struct{})
	go func() {
		defer close(retryDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if m.connRetry.Load() {
					m.enqueueConnectivityPush()
				}
			}
		}
	}()
	defer func() { cancel(); <-retryDone }()
	delay := time.Second
	for ctx.Err() == nil {
		err := m.player.Watch(ctx, func(delivery playerlink.SnapshotDelivery) {
			if delivery.Initial || delivery.Gap {
				delay = time.Second
				m.enqueueConnectivityPush()
			}
		})
		if ctx.Err() != nil {
			return
		}
		m.logger.Debug("Native connectivity watch disconnected", zap.Error(err))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if delay < 10*time.Second {
			delay = min(2*delay, 10*time.Second)
		}
	}
}
