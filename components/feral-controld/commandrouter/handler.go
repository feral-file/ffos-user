package commandrouter

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/devicectl"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mintpairing"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/playerresponse"
	"github.com/feral-file/ffos-user/components/feral-controld/playersession"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

//go:generate mockgen -source=handler.go -destination=../mocks/command.go -package=mocks -mock_names=Handler=MockCommandHandler
type Handler interface {
	Process(ctx context.Context, command commands.Command) (interface{}, error)
}

type handler struct {
	executor     devicectl.Executor
	cdp          cdp.CDP
	dp1          dp1.DP1
	json         wrapper.JSON
	statusPoller status.Poller
	mintPairing  mintpairing.Service
	// offlineCache may be nil (feature disabled / not yet wired), mirroring
	// the mintPairing nil-guard pattern above.
	offlineCache offlinecache.Service
	// kioskReplay may be nil for the same reason as offlineCache above.
	// Kept as a separate nilable dependency (rather than folded into
	// offlineCache) because it is specifically about the kiosk's live CDP
	// Fetch-interception scope, not the download/store side of caching.
	kioskReplay offlinecache.KioskReplay
	scheduler   playlistschedule.Scheduler
	logger      *zap.Logger

	// sourceProber, when set (SetSourceProber), is the cast-time source
	// preflight (#304). Independent of offlineCache above on purpose: the
	// probe must run whether or not offline caching is enabled — it only
	// LIVES in that package because that is where dialing untrusted
	// playlist URLs is made safe. nil (tests, a build wired before the
	// seam) skips the preflight entirely, degrading to the old
	// accept-anything behavior — fail-open, documented at the call site.
	sourceProber offlinecache.SourceProber

	// sessionGeneration, when set (SetSessionGeneration), is
	// playersession.Session.Generation narrowed to a func() uint64 seam
	// (design doc §4). nil reads as generation 0 always, which never
	// appears to move.
	sessionGeneration func() uint64

	// recoverySession, when set (SetRecoverySession), is the
	// playersession.Session the refreshArtwork recovery escalation (§3)
	// drives via NavigateHomeInline — the caller here holds no external lock
	// while calling it (the escalation is outside every WithPlayerPush closure
	// and gate.go has no mutex), which is exactly Inline's synchronous-reply
	// contract. nil (tests, a build wired before Phase 2b) makes the
	// escalation a no-op, degrading to the pre-existing error return.
	//
	// The "no external lock" premise now has ONE thing holding it up that is
	// not obvious from the escalation site: Process does take the
	// kioskReplay playback lock, but only on the CMD_DISPLAY_PLAYLIST branch
	// (heldPlaybackLock), while this escalation is CMD_REFRESH_ARTWORK-only,
	// so the two never overlap. Extending the escalation to displayPlaylist
	// would deadlock on that non-reentrant lock — re-check this before doing
	// so, rather than trusting the sentence above.
	recoverySession RecoverySession
}

// RecoverySession is the narrow slice of playersession.Session the relayer's
// refreshArtwork recovery path needs. Consumer-owned, mirroring
// setupui.NavigationSession and devicectl.BootRecoverySession;
// *playersession.Session satisfies it.
type RecoverySession interface {
	NavigateHomeInline(opts playersession.NavOptions) error
}

// ErrGenerationRace marks sendCDPRequest's generation re-check failure:
// the send itself succeeded, but the page generation moved while the
// reply was in flight, so the reply cannot be trusted as describing the
// current document. errors.Is-able so the refreshArtwork recovery
// escalation below can EXCLUDE it: a healthy page racing an unrelated
// generation change (a connectivity reconciler bump, a stamp-mismatch
// bump, ...) is not evidence the page is broken, and escalating it into
// NavigateHomeInline({PurgeCache:true}) would be a visible, unwarranted
// restart. The caller retries instead, per the re-check's stated purpose.
var ErrGenerationRace = errors.New("commandrouter: command reply raced a page generation change")

// SetRecoverySession injects the session the refreshArtwork recovery
// escalation drives against, if h supports it (the concrete *handler built
// by New — NOT the storm-protection gate wrapper, mirroring
// SetSessionGeneration's contract).
func SetRecoverySession(h Handler, sess RecoverySession, logger *zap.Logger) {
	setter, ok := h.(interface{ setRecoverySession(RecoverySession) })
	if !ok {
		logger.Warn("Command handler does not support recovery session wiring")
		return
	}
	setter.setRecoverySession(sess)
}

func (h *handler) setRecoverySession(sess RecoverySession) {
	h.recoverySession = sess
}

// SetSessionGeneration injects the generation-getter seam onto h, if h
// supports it (the concrete *handler built by New — NOT the storm-protection
// gate wrapper, so callers must wire it against the raw handler before
// NewGate wraps it). logger.Warn's on a handler that does not support it
// rather than panicking, mirroring devicectl.SetSessionGeneration.
func SetSessionGeneration(h Handler, fn func() uint64, logger *zap.Logger) {
	setter, ok := h.(interface{ setSessionGeneration(func() uint64) })
	if !ok {
		logger.Warn("Command handler does not support session generation re-check")
		return
	}
	setter.setSessionGeneration(fn)
}

func (h *handler) setSessionGeneration(fn func() uint64) {
	h.sessionGeneration = fn
}

// SetSourceProber injects the cast-time source preflight onto h, if h
// supports it (the concrete *handler built by New — NOT the
// storm-protection gate wrapper, so callers must wire it against the raw
// handler before NewGate wraps it, mirroring SetSessionGeneration's
// contract).
func SetSourceProber(h Handler, prober offlinecache.SourceProber, logger *zap.Logger) {
	setter, ok := h.(interface {
		setSourceProber(offlinecache.SourceProber)
	})
	if !ok {
		logger.Warn("Command handler does not support source preflight wiring")
		return
	}
	setter.setSourceProber(prober)
}

func (h *handler) setSourceProber(prober offlinecache.SourceProber) {
	h.sourceProber = prober
}

func (h *handler) currentGeneration() uint64 {
	if h.sessionGeneration == nil {
		return 0
	}
	return h.sessionGeneration()
}

func New(
	executor devicectl.Executor,
	cdp cdp.CDP,
	dp1 dp1.DP1,
	statusPoller status.Poller,
	mintPairing mintpairing.Service,
	offlineCache offlinecache.Service,
	kioskReplay offlinecache.KioskReplay,
	scheduler playlistschedule.Scheduler,
	json wrapper.JSON,
	logger *zap.Logger,
) Handler {
	return &handler{
		executor:     executor,
		cdp:          cdp,
		dp1:          dp1,
		statusPoller: statusPoller,
		mintPairing:  mintPairing,
		offlineCache: offlineCache,
		kioskReplay:  kioskReplay,
		scheduler:    scheduler,
		json:         json,
		logger:       logger,
	}
}

// servedDuringFactoryReset names the commands still answered while a factory
// reset is staged: pure reporting, no persisted write, no screen ownership, no
// boot staging. An allowlist rather than a denylist on purpose — a command
// added later is rejected during the reset window until someone deliberately
// decides it belongs here, which is the safe direction for a guard protecting
// a device mid-wipe. CMD_FACTORY_RESET itself is absent: a duplicate while one
// is already staged has nothing to add, and once the stuck-reset watchdog
// releases the latch a retry is accepted normally again.
var servedDuringFactoryReset = map[commands.Type]bool{
	commands.CMD_DEVICE_STATUS:    true,
	commands.CMD_PROFILE:          true, // == CMD_SYS_METRICS ("deviceMetrics")
	commands.CMD_DDC_PANEL_STATUS: true,
}

// Process processes the command and returns the result
func (h *handler) Process(ctx context.Context, command commands.Command) (interface{}, error) {
	commandType := command.Type
	if commandType == "" {
		h.logger.Warn("Received command with no type", zap.Any("command", command))
		return nil, nil
	}

	// A staged factory reset closes the command surface. This is the ONE place
	// it can be enforced completely: every transport (relayer mediator, LAN
	// hub, OOM recovery) and every command family (device control, mint
	// pairing, offline cache, player) funnels through this function, while
	// devicectl.Execute below sees only the device-control subset.
	//
	// Why it must close at all: the reset unclaims the device but leaves the
	// former owner's relayer session open across the pre-reboot window, and the
	// candidate boot can ROLL BACK to the running subvolume — so any write
	// landing here survives a reset the new owner believes happened. `connect`
	// re-persists the claim, `sshAccess` writes authorized_keys, the toggles
	// write state sentinels, the offline-cache commands write and delete cached
	// artwork, and updateToLatestVersion arms a competing bootctl one-shot that
	// can displace the reset's own. Mint pairing and the player commands also
	// take the screen the reset narration owns.
	// No nil guard on h.executor deliberately: every other collaborator here is
	// optional and nil-checked, but the executor is not — there is one
	// construction (main.go) and it always passes a real one. A `!= nil` here
	// would turn a future mis-wire into a SILENTLY DISABLED reset guard, which
	// is the same fail-open shape ResetStaged was made a required interface
	// method to prevent. A nil panics loudly at startup instead.
	if h.executor.ResetStaged() && !servedDuringFactoryReset[commandType] {
		h.logger.Warn("Rejecting command while a factory reset is staged",
			zap.String("command", commandType.String()))
		return nil, fmt.Errorf("factory reset in progress: %s is not accepted", commandType)
	}

	var result interface{}
	var err error

	if commandType == commands.CMD_START_MINT_PAIRING_SESSION {
		if h.mintPairing == nil {
			return map[string]any{
				"ok": false,
				"error": map[string]any{
					"code":      "disabled",
					"message":   "mint pairing is not enabled",
					"retryable": false,
				},
			}, nil
		}
		return h.mintPairing.HandleStartPairingSession(ctx, command.Arguments)
	}

	if commandType == commands.CMD_CLOSE_MINT_PAIRING_SESSION {
		if h.mintPairing == nil {
			return map[string]any{
				"ok": false,
				"error": map[string]any{
					"code":      "disabled",
					"message":   "mint pairing is not enabled",
					"retryable": false,
				},
			}, nil
		}
		return h.mintPairing.HandleClosePairingSession(ctx, command.Arguments)
	}

	if commandType == commands.CMD_MINT_PAIRING_APPROVAL {
		if h.mintPairing == nil {
			return map[string]any{
				"ok": false,
				"error": map[string]any{
					"code":      "not_found",
					"message":   "mint pairing is not enabled",
					"retryable": false,
				},
			}, nil
		}
		return h.mintPairing.HandleApprovalDecision(ctx, command.Arguments)
	}

	if isOfflineCacheCommand(commandType) {
		return h.handleOfflineCacheCommand(ctx, commandType, command.Arguments)
	}

	if commandType.DeviceCtlCommand() {
		// Handle device control command
		result, err = h.executor.Execute(ctx,
			commands.Command{
				Type:      commandType,
				Arguments: command.Arguments,
			})
		if err != nil {
			h.logger.Error("Failed to execute command", zap.Error(err))
			return nil, err
		}

		return result, nil
	} else {
		var playlist *dp1.Playlist
		var schedulerSnapshot playlistschedule.Snapshot
		var schedulerSource playlistschedule.Source
		// replayScopeTouched records whether THIS request reached
		// syncReplayScope (even a failed sync counts — it still bumps the
		// playback generation). The corrective resync in the failure defer
		// below is gated on it: the resync exists to revert a
		// mistakenly-applied NEW scope, so a failure before any scope
		// change (a malformed payload, a resolution error, a preflight
		// rejection) has nothing to revert — and the resync is
		// network-bound (a live FetchPlayerStatus plus playlist
		// resolution), so running it anyway would delay an otherwise
		// immediate error reply, in the worst case past the hub's write
		// deadline.
		var replayScopeTouched bool
		// rescuedByCache marks an all-dead cast that proceeded ONLY because
		// the offline cache holds a prior capture. Such a cast has exactly
		// one way to actually show artwork — replay serving from cache — so
		// unlike an ordinary live cast (where replay is a best-effort
		// enhancement and a scope-sync failure is just logged), a rescued
		// cast REQUIRES the replay scope to arm: if syncReplayScope fails
		// below, the cast is rejected with the preflight's own error rather
		// than forwarded to render every origin already proven dead (#308
		// review). rescueProbeResults carries the probe evidence for that
		// late rejection; scopeSyncErr is the closure's outcome seam.
		var rescuedByCache bool
		var rescueProbeResults []offlinecache.SourceProbeResult
		var scopeSyncErr error
		if commandType == commands.CMD_DISPLAY_PLAYLIST {
			status.RecordPlaybackAttempt()
			defer func() {
				if err != nil {
					status.RecordPlaybackFailure()
					// SyncPlaylist below already switched replay's live
					// Fetch-interception scope to the NEW playlist before
					// this CDP send ran (see that call site's doc for why
					// that ordering is required). Since the send itself
					// failed, the kiosk never actually switched — it is
					// still showing whatever it displayed before — so
					// leaving scope pointed at the new playlist would
					// misclassify the still-on-screen old playlist's own
					// requests as misses. Re-syncing to the player's
					// actual current status reverts that. Gated: a failure
					// BEFORE any scope change has nothing to revert (see
					// replayScopeTouched's doc).
					if replayScopeTouched {
						h.resyncKioskReplayScopeToCurrentDisplay(ctx)
					}
					return
				}
				h.logger.Info("result from CDP", zap.Any("result", result))
				if !playerresponse.OK(result) {
					h.logger.Warn("Playback verification failed: player did not respond with ok")
					status.RecordPlaybackFailure()
					// Same rationale as the err != nil branch above: the
					// send succeeded at the transport level but the
					// player itself rejected the command, so it is still
					// displaying whatever it had before. (A player
					// rejection implies the send ran, which implies scope
					// was synced first — the gate matches that reality
					// rather than assuming it.)
					if replayScopeTouched {
						h.resyncKioskReplayScopeToCurrentDisplay(ctx)
					}
				}
			}()
			// heldPlaybackLock records whether this path acquired the
			// playback coordinator (only when it actually syncs replay
			// scope below). Releasing it is deferred so the lock spans
			// BOTH the scope sync and the CDP send at the end of this
			// branch, making that pair atomic against concurrent
			// displayPlaylist commands and playlist-refresher passes (see
			// KioskReplay.LockPlayback's doc). Registered AFTER the
			// failure/rejection resync defer above so it runs BEFORE it
			// (defers are LIFO): that resync re-acquires this same
			// non-reentrant lock, so this path must have released it
			// first.
			var heldPlaybackLock bool
			defer func() {
				if heldPlaybackLock {
					h.kioskReplay.UnlockPlayback()
				}
			}()
			switch {
			case command.Arguments["playlistUrl"] != nil:
				url, ok := command.Arguments["playlistUrl"].(string)
				if !ok || url == "" {
					return nil, fmt.Errorf("playlistUrl is not a string or empty")
				}

				playlist, err = h.dp1.ProcessPlaylistURLForCast(ctx, url)
				if err != nil {
					// Live DP-1 resolution failed — most commonly, the
					// device has no network right now. Fall back to the
					// exact playlist body last saved by downloadPlaylist
					// for this same URL, if any, rather than hard-failing
					// a playlist that is actually fully cached and
					// replayable offline (see docs/offline-artwork-
					// capture.md §6 and Service.CachedPlaylistForURL's
					// doc). This is a "last known good" copy, not a live
					// re-resolution: it will not reflect anything
					// published at url after it was downloaded, and (by
					// construction, since it can only exist if it was
					// downloaded successfully before) was already
					// signature-verified once at that time.
					cachedPlaylist, cacheErr := h.loadCachedPlaylistForURL(url)
					if cacheErr != nil {
						return nil, err
					}
					h.logger.Warn("offline cache: displayPlaylist falling back to cached copy after live DP-1 resolution failure",
						zap.String("playlist_url", url), zap.Error(err))
					playlist = cachedPlaylist
					err = nil
				}
				schedulerSource = playlistschedule.Source{PlaylistURL: url}

			case command.Arguments["dp1_call"] != nil:
				playlistMap, ok := command.Arguments["dp1_call"].(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("playlist is not a map")
				}

				var playlistBytes []byte
				playlistBytes, err = h.json.Marshal(playlistMap)
				if err != nil {
					return nil, fmt.Errorf("failed to marshal playlist: %w", err)
				}

				if err = h.json.Unmarshal(playlistBytes, &playlist); err != nil {
					return nil, fmt.Errorf("failed to unmarshal playlist: %w", err)
				}

				if playlist.HasDynamicContent() {
					schedulerSource = playlistschedule.Source{DynamicPlaylist: playlist}
					playlist, err = h.dp1.ProcessDynamicPlaylistForCast(ctx, *playlist)
					if err != nil {
						h.logger.Error("Failed to process dynamic playlist", zap.Error(err))
						return nil, err
					}
				}

			default:
				return nil, fmt.Errorf("unknown payload type")
			}

			// Cast-time source preflight (#304). Without it, a cast whose
			// every source 400s is forwarded, self-reported ok by the
			// player (an iframe cannot see HTTP status), and shown as
			// "playing" — so neither the casting end nor the device ever
			// learns the links are dead. The probe rejects the cast ONLY
			// when every item earned a definitive dead verdict (an actual
			// HTTP >= 400 answer — or a malformed data: URI, the one
			// non-HTTP verdict that is equally definitive because it is a
			// parse, not a network guess): network errors, timeouts, guard
			// refusals, and well-formed data: items all count in the cast's favor, so
			// an offline device casting a fully-cached playlist (the
			// cached-copy fallback above) still plays. A partially-dead
			// playlist also still plays — rejecting it would punish nine
			// good artworks for one dead link — with the dead items logged.
			//
			// Placed deliberately with the slow network-bound work: after
			// DP-1 resolution (so dynamic items are probed as resolved) and
			// BEFORE LockPlayback below, for the same reason resolution is
			// (see that comment). err must be assigned, not just returned,
			// so the deferred playback-failure accounting above records the
			// rejection.
			if h.sourceProber != nil && playlist != nil && len(playlist.Items) > 0 {
				sources := make([]string, 0, len(playlist.Items))
				// scheduledPlaylist: any item carrying displayAt makes this
				// a scheduler-filtered playlist, and the probe sees the
				// FULL item list before that filtering. An item outside
				// the current cohort can be dead NOW and live at display
				// time — publish-then-upload is the normal premiere
				// ordering, so a scheduled drop's sources routinely 404
				// until go-live. Rejecting would silently lose the cast
				// the scheduler was about to defer-accept and arm a timer
				// for (the {ok:true, deferred:true} path below), so for
				// scheduled playlists the probe is observability-only:
				// dead items are logged, the rejection stands down.
				scheduledPlaylist := false
				for _, item := range playlist.Items {
					sources = append(sources, item.Source)
					if item.DisplayAt != nil && *item.DisplayAt != "" {
						scheduledPlaylist = true
					}
				}
				probeResults := h.sourceProber.ProbeSources(ctx, sources)
				// Per-item log detail is capped: the hub accepts a 4 MiB
				// playlist with no item cap, so an all-dead hostile cast
				// must not be able to mint one log line per item on a
				// device whose logs are size-rotated files. The first few
				// carry the truncated sources an operator greps for; the
				// rest collapse into counts.
				dead, inconclusive := 0, 0
				for i, r := range probeResults {
					switch r.Verdict {
					case offlinecache.ProbeDead:
						dead++
						if dead <= maxProbeLogDetailItems {
							h.logger.Warn("displayPlaylist: item source is unreachable",
								zap.Int("item", i),
								zap.String("source", r.Source),
								zap.Int("status", r.Status),
								zap.Error(r.Err))
						}
					case offlinecache.ProbeInconclusive:
						inconclusive++
					}
				}
				if dead > maxProbeLogDetailItems {
					h.logger.Warn("displayPlaylist: additional item sources unreachable",
						zap.Int("count", dead-maxProbeLogDetailItems))
				}
				if inconclusive > 0 {
					h.logger.Debug("displayPlaylist: item source probes inconclusive",
						zap.Int("count", inconclusive))
				}
				if dead == len(probeResults) {
					switch {
					case scheduledPlaylist:
						// See scheduledPlaylist's doc above: dead-now is
						// not dead-at-display-time for a scheduled drop,
						// and rejecting would lose the deferred cast.
						h.logger.Warn("displayPlaylist: every item source is unreachable but the playlist is displayAt-scheduled; deferring to the scheduler",
							zap.Int("items", len(sources)))
					case h.offlineCache != nil && h.kioskReplay != nil && h.offlineCache.HasReplayableItem(sources...):
						// A definitively dead ORIGIN is not a dead CAST
						// when the offline cache holds a prior capture:
						// replay serves cached items regardless of origin
						// state, and origin rot is exactly the case the
						// cache exists for (#305 review F4). Checked only
						// on the all-dead path — one record read per
						// source, worst case — so the common accept path
						// pays nothing. The rescue is conditional on
						// replay actually being able to serve: kioskReplay
						// must be wired here, and the scope sync below
						// must succeed (see rescuedByCache's doc) — a
						// record on disk that replay cannot arm rescues
						// nothing.
						rescuedByCache = true
						rescueProbeResults = probeResults
						h.logger.Warn("displayPlaylist: every item source is unreachable but cached captures exist; casting for offline replay",
							zap.Int("items", len(sources)))
					default:
						err = &SourceUnreachableError{Results: probeResults}
						return nil, err
					}
				}
			}

			// Player CanvasService rejects displayPlaylist without a known
			// intent.action ("Unknown DP1 action: undefined" → ok:false).
			// Controller casts are force-display, same contract as
			// playlistschedule push (now_display, never soft refresh).
			ensureDisplayPlaylistIntent(command.Arguments)

			// Acquire the playback coordinator here and hold it (via the
			// deferred unlock above) across both the replay scope sync
			// (syncReplayScope below) and the CDP send, so scope-sync +
			// navigation cannot interleave with another display/refresh's
			// own sync+send. Deliberately acquired AFTER the (possibly
			// slow, network-bound) DP-1 resolution above, which does not
			// touch scope.
			if h.kioskReplay != nil && playlist != nil {
				h.kioskReplay.LockPlayback()
				heldPlaybackLock = true
			}
		}

		// syncReplayScope points replay's Fetch-interception scope at p's
		// currently-cached items. feral-player advances through a
		// multi-item playlist client-side without telling controld which
		// item is on screen at any instant, so scope covers every cached
		// item in the playlist rather than a single "current" one (see
		// Replayer.EnableForPlaylist's doc). Best-effort: a sync failure
		// must never block the actual display command, since offline
		// replay is a strict enhancement over the live path.
		//
		// Callers invoke this ONLY once a CDP send for p is actually
		// going to happen, immediately before it — never for a cast the
		// scheduler defers or rejects. Scope must keep tracking what is
		// genuinely on screen: a deferred (future-only) cast leaves the
		// previous playlist displaying, and switching interception to the
		// new playlist anyway would — under a fail_closed scope — start
		// blocking the on-screen playlist's own requests even with live
		// network, until a later corrective pass noticed
		// (feral-file/ffos-user#229 review finding).
		syncReplayScope := func(p *dp1.Playlist) {
			if h.kioskReplay == nil || p == nil {
				return
			}
			// Set BEFORE the sync call: a failed SyncPlaylist below still
			// bumps the playback generation (MarkPlaybackChanged), so scope
			// state has been touched either way and the failure defer's
			// corrective resync must run — see replayScopeTouched's doc.
			replayScopeTouched = true
			sources := make([]string, 0, len(p.Items))
			for _, item := range p.Items {
				sources = append(sources, item.Source)
			}
			// scopeSyncErr surfaces the outcome for the ONE caller that
			// must treat a failure as fatal — the cache-rescued all-dead
			// cast (see rescuedByCache's doc). Ordinary live casts keep
			// the best-effort contract: log and proceed.
			scopeSyncErr = h.kioskReplay.SyncPlaylist(ctx, sources)
			if scopeSyncErr != nil {
				h.logger.Warn("offline cache: failed to sync kiosk replay scope for playlist", zap.Error(scopeSyncErr))
			}
			// Announce this authoritative scope change (under the
			// lock) so a concurrent corrective resync that sampled
			// the generation earlier will defer to it instead of
			// clobbering it with a stale playlist's scope — see
			// KioskReplay.PlaybackGeneration's doc. Bumped even if the
			// sync above logged an error: this path is authoritative
			// for what SHOULD be on screen, and the resync must not
			// override that intent with an older snapshot.
			h.kioskReplay.MarkPlaybackChanged()
		}

		if commandType == commands.CMD_REFRESH_ARTWORK {
			_, err = h.cdp.Send("Network.clearBrowserCache", map[string]interface{}{})
			if err != nil {
				h.logger.Warn("Failed to clear Chromium browser cache before artwork refresh", zap.Error(err))
			}
		}

		// Forward to CDP. displayPlaylist and displayDefaultPlaylist share the
		// scheduler push lock with RecomputeNow so a stale timed push cannot land
		// after a newer cast or OOM-recovery fallback.
		// displayDefaultPlaylist is player-owned fallback today; it may no-op
		// successfully, so this path must not clear scheduler authority until
		// controld can prove that default playback replaced the current playlist.
		switch {
		case commandType == commands.CMD_DISPLAY_PLAYLIST && h.scheduler != nil:
			h.scheduler.WithPlayerPush(func() {
				schedulerSnapshot = h.scheduler.Snapshot()
				// Filter displayAt playlists to the active set before the player
				// sees them. The scheduler keeps the full list for timer/wake updates.
				fullPlaylist := playlist
				playlist = h.scheduler.PrepareWithSource(playlist, schedulerSource)
				if playlist == nil {
					err = fmt.Errorf("playlist has invalid displayAt")
					h.scheduler.Restore(schedulerSnapshot)
					return
				}
				if len(playlist.Items) == 0 {
					// The scheduler retained and armed the future schedule; the
					// player rejects an empty displayPlaylist, so leave its current
					// artwork in place until a cohort becomes eligible. Replay
					// scope is deliberately NOT synced on this path (see
					// syncReplayScope's caller contract): nothing new reaches
					// the screen, so interception must stay pointed at the
					// playlist that keeps displaying.
					h.scheduler.Commit()
					// A relayer RPC and hub request both need an explicit acceptance
					// response even though no CDP write was valid. This also prevents
					// playback metrics from treating the deferred schedule as a failure.
					result = map[string]interface{}{
						"message": map[string]interface{}{"ok": true, "deferred": true},
					}
					return
				}
				// Scope the FULL playlist's items, not just the filtered
				// active cohort: the scheduler's own timer/wake/retry
				// cutovers push later cohorts of this same playlist
				// directly (playlistschedule's push), with no replay-scope
				// hook of their own, so the scope installed here must
				// already cover every cohort a cutover can display. The
				// cost is only precision, in the safe direction: uncached
				// future items keep the scope "mixed", whose miss policy
				// is pass-through rather than fail_closed (see
				// Replayer.EnableForPlaylist), and the playlist-refresher's
				// periodic pass re-syncs as downloads complete.
				syncReplayScope(fullPlaylist)
				// A cache-rescued cast can only show artwork through replay,
				// so a scope-sync failure means the rescue's one path to the
				// screen is gone: reject with the preflight's own evidence
				// instead of forwarding a cast whose every origin is proven
				// dead (see rescuedByCache's doc). Restore mirrors the other
				// pre-send failure exits from this closure.
				if rescuedByCache && scopeSyncErr != nil {
					err = &SourceUnreachableError{Results: rescueProbeResults}
					h.scheduler.Restore(schedulerSnapshot)
					return
				}
				command.Arguments["dp1_call"] = playlist
				result, err = h.sendCDPRequest(command)
				if err != nil || !playerresponse.OK(result) {
					h.scheduler.Restore(schedulerSnapshot)
				} else {
					h.scheduler.Commit()
				}
			})
		case commandType == commands.CMD_DISPLAY_DEFAULT_PLAYLIST && h.scheduler != nil:
			h.scheduler.WithPlayerPush(func() {
				result, err = h.sendCDPRequest(command)
			})
		default:
			if commandType == commands.CMD_DISPLAY_PLAYLIST {
				// No scheduler configured: every cast reaches the player
				// unfiltered, so scope-sync immediately precedes the send.
				syncReplayScope(playlist)
				// Same fatal-for-rescue rule as the scheduler branch above
				// — see rescuedByCache's doc.
				if rescuedByCache && scopeSyncErr != nil {
					err = &SourceUnreachableError{Results: rescueProbeResults}
					return nil, err
				}
				command.Arguments["dp1_call"] = playlist
			}
			result, err = h.sendCDPRequest(command)
		}
		if err != nil {
			// No restore-on-error here: every CMD_DISPLAY_PLAYLIST failure path
			// above already calls scheduler.Restore before returning from the
			// WithPlayerPush closure, so there is nothing left to undo by the
			// time control reaches this point. That is also load-bearing, not
			// just a redundancy removal — a restore here would run outside
			// pushMu, violating the scheduler's documented pushMu-before-mu lock
			// ordering (Restore must run under the same push lock as the failed
			// send it is undoing).
			//
			// refreshArtwork's evaluate needs a live player page
			// (window.handleCDPRequest), but a refresh is most needed exactly
			// when the page is broken — e.g. Chromium serving stale cached
			// chunks after a player bundle swap (#234), where the app never
			// boots. The cache was already cleared above; the recovery
			// primitive is navigate-to-entry (design doc §3), never
			// reload-in-place — the static export is flat files only, so a
			// client-route reload 404s. NavigateHomeInline runs its own gates
			// (sleep/error-page/overlay) synchronously and reports a
			// SYNCHRONOUS error, which is exactly what this caller needs: it
			// holds no external lock (this escalation is outside every
			// WithPlayerPush closure, gate.go has no mutex, and the kioskReplay
			// playback lock is taken only on the displayPlaylist branch — see
			// recoverySession's field doc), so Inline's bounded wait cannot
			// deadlock it. Only when the escalation itself fails (or no
			// session is wired) is the command truly dead.
			if commandType != commands.CMD_REFRESH_ARTWORK {
				return nil, err
			}
			// A generation-race failure means the send itself worked but
			// raced an unrelated page change — not evidence of a broken page.
			// Escalating it into a destructive navigate would visibly restart
			// a healthy page for no reason; the caller retries instead.
			if errors.Is(err, ErrGenerationRace) {
				return nil, err
			}
			if h.recoverySession == nil {
				return nil, err
			}
			if navErr := h.recoverySession.NavigateHomeInline(playersession.NavOptions{PurgeCache: true}); navErr != nil {
				return nil, err
			}
			h.logger.Warn("refreshArtwork: player page unresponsive; recovered with cache clear + navigate", zap.Error(err))
			err = nil
			// "navigate" replaces the old "recovered":"reload" value — the
			// relayer/app consumer only reads `ok` from this reply (verified:
			// no in-repo consumer parses `recovered`), so this is a diagnostic
			// label, not a wire contract change.
			result = map[string]interface{}{
				"message": map[string]interface{}{"ok": true, "recovered": "navigate"},
			}
		}

		// Force refresh status poller
		if h.statusPoller != nil {
			h.statusPoller.ForceRefresh()
		}

		return result, nil
	}
}

// ensureDisplayPlaylistIntent sets intent.action=now_display when the cast
// request has no action yet. Controllers historically send only playlistUrl /
// dp1_call; the player still requires a known DP1 action. Soft refresh keeps
// its own path (playlist-refresher sets refresh:true and does not use this
// helper). An explicit controller intent is preserved.
func ensureDisplayPlaylistIntent(args map[string]interface{}) {
	if args == nil {
		return
	}
	if intent, ok := args["intent"].(map[string]interface{}); ok {
		if action, _ := intent["action"].(string); action != "" {
			return
		}
		intent["action"] = "now_display"
		return
	}
	args["intent"] = map[string]interface{}{
		"action": "now_display",
	}
}

// sendCDPRequest marshals payload and sends to CDP
func (h *handler) sendCDPRequest(command commands.Command) (interface{}, error) {
	p, err := command.JSON()
	if err != nil {
		h.logger.Error("Failed to marshal payload", zap.Error(err))
		return nil, err
	}

	genBefore := h.currentGeneration()
	result, err := h.cdp.Send(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression": fmt.Sprintf("window.handleCDPRequest(%s)", string(p)),
	})
	if err != nil {
		h.logger.Error("Failed to send CDP request", zap.Error(err))
		return nil, err
	}

	// Generation re-check (design doc §4): unlike devicectl's sleep apply,
	// a command reply answered by a document that is no longer current is not
	// something the relayer/hub caller can safely trust as "delivered" — a
	// cast or control command silently landing on (or being silently
	// swallowed by) a torn-down page must surface loudly rather than report
	// success, so the caller retries instead of believing a phantom ACK.
	if genAfter := h.currentGeneration(); genAfter != genBefore {
		h.logger.Warn("CDP request reply raced a page generation change; reporting failure",
			zap.Uint64("generation_before", genBefore), zap.Uint64("generation_after", genAfter))
		return nil, fmt.Errorf("command reply raced a page navigation (generation changed from %d to %d); retry: %w", genBefore, genAfter, ErrGenerationRace)
	}

	return result, nil
}
