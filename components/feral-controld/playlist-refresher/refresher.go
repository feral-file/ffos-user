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
	PLAYLIST_REFRESH_INTERVAL      = 1 * time.Minute
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

	// playlistURLMu guards playlistURLState. Only the playlist-refresher goroutine mutates it;
	// the mutex keeps future call-site additions safe.
	playlistURLMu    sync.Mutex
	playlistURLState map[string]*urlPlaylistRefreshState

	done    chan struct{}
	started bool
}

// urlPlaylistRefreshState holds the last successful URL refresh snapshot for conditional GET
// (If-None-Match) and for visualized change detection between resolved playlists.
type urlPlaylistRefreshState struct {
	etag         string
	lastPlaylist *dp1.Playlist
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

		playlistURLState: make(map[string]*urlPlaylistRefreshState),
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
		// URL-backed playlists: use conditional GET (If-None-Match + ETag) so we skip work when the
		// origin returns 304. When the document changes (new ETag or full 200 body), we compare
		// "visualized" fields—playlist/item intermission notes and item identity (add/remove)—
		// against the last resolved snapshot. If those differ, we issue a full displayPlaylist with
		// now_display so ff-player restarts the cast from the updated program; if only non-visual
		// fields changed, we keep the prior behavior (refresh:true) so CanvasService swaps items
		// without replaying from the top.
		if err := r.processPlaylistURLRefresh(*playerStatus.PlaylistURL); err != nil {
			return err
		}
		return nil
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

	// Send playlist to CDP (embedded/dynamic playlist path only; URL path returns above).
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

func (r *refresher) processPlaylistURLRefresh(playlistURL string) error {
	r.playlistURLMu.Lock()
	ifNoneMatch := ""
	if st := r.playlistURLState[playlistURL]; st != nil {
		ifNoneMatch = st.etag
	}
	r.playlistURLMu.Unlock()

	res, err := r.dp1.ProcessPlaylistURLConditional(r.context, playlistURL, false, ifNoneMatch)
	if err != nil {
		return err
	}
	if res.NotModified {
		r.logger.Debug("Playlist URL unchanged (HTTP 304), skipping CDP")
		return nil
	}
	playlist := res.Playlist
	if playlist == nil {
		return errors.New("dp1 returned empty playlist after URL fetch")
	}

	r.playlistURLMu.Lock()
	prev := r.playlistURLState[playlistURL]
	r.playlistURLMu.Unlock()

	// First successful refresh for this URL: align the player cache without forcing a new "now"
	// display session (same as historical refresh-only behavior).
	var replay bool
	if prev != nil {
		replay = visiblePlaylistChanged(prev.lastPlaylist, playlist)
	}

	args := map[string]interface{}{
		"dp1_call": playlist,
	}
	if replay {
		args["playlistUrl"] = playlistURL
		args["intent"] = map[string]interface{}{
			// Matches ff-player DP1Action.NowDisplay — issues a new replay surface from index 0.
			"action": "now_display",
		}
	} else {
		args["refresh"] = true
	}

	command := commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: args,
	}

	if _, err := r.sendCDPRequest(command); err != nil {
		return err
	}

	r.playlistURLMu.Lock()
	r.playlistURLState[playlistURL] = &urlPlaylistRefreshState{
		etag:         res.ETag,
		lastPlaylist: playlist,
	}
	r.playlistURLMu.Unlock()

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
