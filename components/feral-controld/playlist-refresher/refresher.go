package refresher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const (
	PLAYLIST_REFRESH_INTERVAL      = 5 * time.Minute
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
	r.done = make(chan struct{}) // Recreate the done channel for each start
	r.mu.Unlock()

	go r.background()
}

func (r *refresher) background() {
	r.logger.Info("Refresher background goroutine started")

	// Process playing playlist until it succeeds
	for {
		if err := r.processPlayingPlaylist(); err != nil {
			r.logger.Error("Failed to process playing playlist", zap.Error(err))
			r.clock.Sleep(PLAYER_STATUS_POLLING_INTERVAL)
			continue
		}
		break
	}

	// Start ticker to refresh playlist
	ticker := r.clock.NewTicker(PLAYLIST_REFRESH_INTERVAL)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C():
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

	if playerStatus.Command != string(commands.CMD_DISPLAY_PLAYLIST) {
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
		if !playerStatus.Playlist.HasDynamicContent() {
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
	command := commands.Command{
		Type: commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{
			"dp1_call": playlist,
			"refresh":  true,
		},
	}

	if _, err := r.sendCDPRequest(command); err != nil {
		return err
	}

	return nil
}

// sendCDPRequest marshals payload and sends to CDP
func (r *refresher) sendCDPRequest(command commands.Command) (interface{}, error) {
	p, err := command.JSON()
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
