package refresher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/cenkalti/backoff/v4"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const (
	PLAYLIST_REFRESH_INTERVAL      = 20 * time.Second
	PLAYER_STATUS_POLLING_INTERVAL = 5 * time.Second
)

//go:generate mockgen -source=refresher.go -destination=../mocks/refresher.go -package=mocks -mock_names=Refresher=MockRefresher
type Refresher interface {
	Start()
	Stop()
}

type refresher struct {
	mu sync.RWMutex

	context      context.Context
	cdp          cdp.CDP
	statusPoller status.Poller
	dp1          dp1.DP1

	clock  wrapper.Clock
	logger *zap.Logger

	done    chan struct{}
	started bool
}

func New(
	ctx context.Context,
	dp1 dp1.DP1,
	statusPoller status.Poller,
	cdp cdp.CDP,
	clock wrapper.Clock,
	logger *zap.Logger,
) Refresher {
	return &refresher{
		context:      ctx,
		cdp:          cdp,
		statusPoller: statusPoller,
		dp1:          dp1,
		clock:        clock,
		logger:       logger,
		done:         make(chan struct{}),
	}
}

func (r *refresher) Start() {
	r.mu.Lock()

	if r.started {
		r.mu.Unlock()
		return
	}

	r.started = true
	r.mu.Unlock()

	go r.background()
}

func (r *refresher) background() {
	r.logger.Info("Refresher background goroutine started")

	// Process playing playlist with backoff
	bo := backoff.NewConstantBackOff(PLAYER_STATUS_POLLING_INTERVAL)
	_ = backoff.Retry(func() error {
		if err := r.processPlayingPlaylist(); err != nil {
			r.logger.Error("Failed to process playing playlist", zap.Error(err))
			return err
		}
		return nil
	}, bo)

	// Start ticker to refresh playlist
	ticker := r.clock.NewTicker(PLAYLIST_REFRESH_INTERVAL)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := r.processPlayingPlaylist(); err != nil {
				r.logger.Error("Failed to process playing playlist", zap.Error(err))
			}
		case <-r.done:
			ticker.Stop()
			r.logger.Info("Refresher background goroutine stopped due to done channel")
			return
		case <-r.context.Done():
			ticker.Stop()
			r.logger.Info("Refresher background goroutine stopped due to context cancellation")
			return
		}
	}

}

func (r *refresher) Stop() {
	r.mu.Lock()
	if !r.started {
		r.mu.Unlock()
		return
	}

	r.started = false
	r.mu.Unlock()

	select {
	case <-r.done:
		// Already closed
	default:
		close(r.done)
	}

	r.logger.Info("Refresher stopped")
}

// processPlayingPlaylist processes the playing playlist and sends it to CDP
func (r *refresher) processPlayingPlaylist() error {
	// Get player status
	playerStatus, err := r.statusPoller.FetchPlayerStatus(r.context)
	if err != nil {
		return err
	}
	if playerStatus == nil {
		r.logger.Warn("Player status is nil")
		return nil
	}

	if playerStatus.Command != relayer.CMD_DISPLAY_PLAYLIST {
		r.logger.Debug("Player command is not display any playlist", zap.String("command", string(playerStatus.Command)))
		return nil
	}

	// Process playlist
	var playlist *dp1.Playlist
	switch {
	case playerStatus.PlaylistURL != nil:
		playlist, err = r.dp1.ProcessPlaylistURL(r.context, *playerStatus.PlaylistURL, false)
		if err != nil {
			return err
		}
	case playerStatus.Playlist != nil:
		if len(playerStatus.Playlist.DynamicQueries) == 0 {
			r.logger.Debug("Playlist has no dynamic queries, skipping")
			return nil
		}

		playlist, err = r.dp1.ProcessDynamicPlaylist(r.context, *playerStatus.Playlist, false)
		if err != nil {
			return err
		}
	default:
		return errors.New("player status has no playlist URL or playlist")
	}

	// Send playlist to CDP
	payload := relayer.Payload{}
	cmd := relayer.CMD_DISPLAY_PLAYLIST
	payload.Message.Command = &cmd
	payload.Message.Args = map[string]interface{}{
		"dp1_call": playlist,
		"refresh":  true,
	}

	if _, err := r.sendCDPRequest(payload); err != nil {
		return err
	}

	return nil
}

// sendCDPRequest marshals payload and sends to CDP
func (r *refresher) sendCDPRequest(payload relayer.Payload) (interface{}, error) {
	p, err := payload.JSON()
	if err != nil {
		return nil, err
	}

	result, err := r.cdp.Send(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression": fmt.Sprintf("window.handleCDPRequest(%s)", string(p)),
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}
