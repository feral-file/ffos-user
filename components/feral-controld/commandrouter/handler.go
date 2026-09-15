package commandrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/display-protocol/dp1-go/extension/contentrating"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/contentpolicy"
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
	contentPolicy   *contentpolicy.Store
	// policyRefresher re-sends the current playlist after an accepted policy
	// change. nil (tests, a build wired before the seam) degrades to the old
	// behavior: the change applies to the next cast or periodic refresh.
	policyRefresher PolicyRefresher
}

func SetContentPolicy(h Handler, policy *contentpolicy.Store, logger *zap.Logger) {
	setter, ok := h.(interface{ setContentPolicy(*contentpolicy.Store) })
	if !ok {
		logger.Warn("Command handler does not support content policy wiring")
		return
	}
	setter.setContentPolicy(policy)
}

// PolicyRefresher is the narrow slice of the playlist refresher the policy path
// needs: re-send what is on screen. Consumer-owned, like RecoverySession.
type PolicyRefresher interface{ ForceRefresh() }

// SetPolicyRefresher wires the re-send used after a policy change, if h
// supports it (the concrete *handler built by New, not the gate wrapper).
func SetPolicyRefresher(h Handler, refresher PolicyRefresher, logger *zap.Logger) {
	setter, ok := h.(interface{ setPolicyRefresher(PolicyRefresher) })
	if !ok {
		logger.Warn("Command handler does not support policy refresher wiring")
		return
	}
	setter.setPolicyRefresher(refresher)
}

func (h *handler) setPolicyRefresher(refresher PolicyRefresher) { h.policyRefresher = refresher }

func (h *handler) setContentPolicy(policy *contentpolicy.Store) {
	h.contentPolicy = policy
	if h.scheduler == nil || policy == nil {
		return
	}
	// A displayAt cutover is the one cast this router does not mediate: it
	// replays a later cohort of a document that was policy-filtered once, when
	// it was cast. Without this projection a policy tightened afterwards would
	// never reach those cohorts. Reads the store's lock-free Snapshot because
	// this runs on the scheduler's push path — see playlistschedule.Projector.
	h.scheduler.SetProjector(func(playlist *dp1.Playlist, contentContext string) (*dp1.Playlist, bool) {
		// An EMPTY context on a scheduler source means the schedule was
		// persisted before this field existed, not that it was curated: the
		// router sets the field on every source it hands the scheduler. Guessing
		// curated here would strip the mature items an owner cast as personal
		// at the first cutover after an upgrade, so the cohort is cast as the
		// original cast admitted it — matching what the refresher does with the
		// same signal.
		if contentContext == "" {
			return playlist, false
		}
		origin, err := contentpolicy.NormalizeContext(contentContext)
		if err != nil {
			origin = contentpolicy.ContextCurated
		}
		projected, empty, projectErr := policy.Snapshot().Project(&playlist.Playlist, origin)
		if projectErr != nil {
			// Fail open on a malformed cache rather than silently blanking a
			// scheduled wall: the router already validated this document.
			return playlist, false
		}
		out := *playlist
		out.Playlist = *projected
		return &out, empty
	})
}

// SyncContentPolicy pushes the stored policy to the current player generation.
// It runs on reconnect, when the player has just come up on ITS defaults.
//
// Serialized against scheduler-owned pushes, not only against casts: a due
// timer or wake recompute holds pushMu and reads the lock-free policy snapshot,
// so without this barrier it could deliver a cohort to the freshly defaulted
// player before this acknowledgement lands — and the scheduler records that
// cohort as delivered, so nothing replays it once the sync succeeds. An
// acknowledged showMatureContent:true would then visibly fail after a player
// restart until some later cast, refresh or cutover.
//
// Lock order is the same one displayPlaylist and setContentPolicy use: content
// policy store, then pushMu.
// ResetContentPolicy returns the device to default content policy and pushes
// that default to the current player generation. Used by factory reset: the
// owner's audience setting falls with the claim.
//
// The durable reset stands even if the player cannot be reached — the point is
// that the NEXT owner does not inherit the setting, and a player that comes up
// later is synced by the reconnect reconciler.
func ResetContentPolicy(h Handler) error {
	target, ok := h.(*handler)
	if !ok || target.contentPolicy == nil {
		return errors.New("content policy unavailable")
	}
	target.contentPolicy.Lock()
	defer target.contentPolicy.Unlock()
	if _, err := target.contentPolicy.ResetLocked(); err != nil {
		return err
	}
	var syncErr error
	sync := func() { syncErr = target.syncContentPolicyLocked() }
	if target.scheduler != nil {
		target.scheduler.WithPlayerPush(sync)
	} else {
		sync()
	}
	return syncErr
}

func SyncContentPolicy(h Handler) error {
	target, ok := h.(*handler)
	if !ok || target.contentPolicy == nil {
		return errors.New("content policy unavailable")
	}
	target.contentPolicy.Lock()
	defer target.contentPolicy.Unlock()
	var err error
	sync := func() { err = target.syncContentPolicyLocked() }
	if target.scheduler != nil {
		target.scheduler.WithPlayerPush(sync)
	} else {
		sync()
	}
	return err
}

// syncContentPolicyLocked is SyncContentPolicy's body. The caller holds the
// content-policy store lock and, when a scheduler exists, the player-push lock.
func (h *handler) syncContentPolicyLocked() error {
	p := h.contentPolicy.CurrentLocked()
	result, err := h.sendContentPolicyCDP(commands.CMD_SET_CONTENT_POLICY, map[string]interface{}{"contentPolicy": p})
	if err != nil {
		return err
	}
	if !policyAckMatches(result, p) {
		return errors.New("player content policy acknowledgement mismatch")
	}
	return nil
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
	commands.CMD_DEVICE_STATUS:      true,
	commands.CMD_PROFILE:            true, // == CMD_SYS_METRICS ("deviceMetrics")
	commands.CMD_DDC_PANEL_STATUS:   true,
	commands.CMD_GET_CONTENT_POLICY: true,
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

	// The player owns the retained, castable DP-1 item; the public history
	// query deliberately exposes only its bounded label snapshot. Old players
	// reply with bare ok:false for this unknown command, which is an explicit
	// unsupported capability rather than a false empty history.
	if commandType == commands.CMD_GET_RECENTLY_PLAYED {
		// Takes no arguments, and a non-empty request is rejected rather than
		// forwarded, for the same reason getContentPolicy rejects one: the
		// storm gate's dedupe key is type+arguments, so junk arguments let one
		// LAN caller mint unlimited distinct keys and hold a global command
		// slot each while the serialized CDP request runs (gate.go).
		if len(command.Arguments) != 0 {
			return map[string]interface{}{
				"ok":     false,
				"status": "error",
				"error":  "getRecentlyPlayed takes no arguments",
			}, nil
		}
		result, err := h.sendCDPRequest(command)
		if err != nil {
			return nil, err
		}
		return boundedRecentlyPlayedReply(recentPlayerReply(result)), nil
	}
	if commandType == commands.CMD_RESOLVE_RECENTLY_PLAYED {
		return map[string]interface{}{
			"ok":     false,
			"status": "error",
			"error":  "resolveRecentlyPlayed is internal",
		}, nil
	}

	// Replay never accepts a phone-supplied source. It resolves an opaque
	// device-local record, rebuilds a one-work unsigned DP-1 call, then invokes
	// this handler's ordinary displayPlaylist branch. That preserves scheduler
	// authority, playback/replay-scope locking, source preflight, and the
	// future policy gate at the normal composition boundary.
	if commandType == commands.CMD_PLAY_RECENTLY_PLAYED {
		// Exactly {"recordId": ...}. The storm gate dedupes on the whole
		// arguments map while this command uses only recordId, so an ignored
		// extra field ({"recordId":"x","nonce":1}) would miss the heavy-tier
		// dedupe and run the same resolve-and-replay work again.
		if len(command.Arguments) != 1 {
			return map[string]interface{}{
				"ok":     false,
				"status": "error",
				"error":  "playRecentlyPlayed takes only recordId",
			}, nil
		}
		recordID, _ := command.Arguments["recordId"].(string)
		if recordID == "" {
			return map[string]interface{}{
				"ok":     false,
				"status": "error",
				"error":  "recordId is required",
			}, nil
		}
		resolved, err := h.sendCDPRequest(commands.Command{
			Type:      commands.CMD_RESOLVE_RECENTLY_PLAYED,
			Arguments: map[string]interface{}{"recordId": recordID},
		})
		if err != nil {
			return nil, err
		}
		message := recentPlayerReply(resolved)
		if !playerresponse.OK(message) {
			return message, nil
		}
		playerMessage, ok := message["message"].(map[string]interface{})
		if !ok {
			return map[string]interface{}{"ok": false, "status": "error", "error": "invalid recently played reply"}, nil
		}
		item, ok := playerMessage["item"].(map[string]interface{})
		if !ok {
			return map[string]interface{}{"ok": false, "status": "error", "error": "recently played record has no item"}, nil
		}
		title, _ := item["title"].(string)
		if title == "" {
			title = "Recently played"
		}
		// Extracting one item invalidates the original full-playlist
		// signature. Preserve only the applicable defaults, which carry
		// artist-level controls for this work; machine settings remain owned by
		// the player and are intentionally not snapshotted here.
		dp1Call := map[string]interface{}{
			"dpVersion": "1.0",
			"title":     title,
			"items":     []interface{}{item},
		}
		if defaults, ok := playerMessage["defaults"].(map[string]interface{}); ok {
			dp1Call["defaults"] = defaults
		}
		// The record carries the context the work actually played under. The
		// replay must re-enter displayPlaylist with that same context, or a
		// work that played as "personal" under strictPersonal:false is
		// re-admitted as "curated" and filtered out — History would offer a
		// work it can never put back. An unrecognized stored value falls back
		// to the strict curated default rather than failing the replay.
		replayArgs := map[string]interface{}{"dp1_call": dp1Call}
		if recorded, ok := playerMessage["contentContext"].(string); ok && recorded != "" {
			if normalized, ctxErr := contentpolicy.NormalizeContext(recorded); ctxErr == nil {
				replayArgs["contentContext"] = string(normalized)
			} else {
				h.logger.Warn("recently played record has an unrecognized content context; replaying as curated",
					zap.String("contentContext", recorded))
			}
		}
		result, err := h.Process(ctx, commands.Command{
			Type:      commands.CMD_DISPLAY_PLAYLIST,
			Arguments: replayArgs,
		})
		if err != nil {
			return nil, err
		}
		// The acknowledgement is bounded and names the requested occurrence;
		// callers must still wait for status/render outcome, particularly when
		// the same work is deliberately replayed twice.
		return map[string]interface{}{
			"recordId": recordID,
			"message":  boundedReplayAck(result),
		}, nil
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

	if commandType == commands.CMD_GET_CONTENT_POLICY || commandType == commands.CMD_SET_CONTENT_POLICY {
		return h.handleContentPolicy(command)
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
		// scopeSyncEnabled is SyncPlaylist's enabled-count for the same one
		// consumer: a rescue with an errorless sync that armed ZERO cached
		// sources is still a dead cast — HasReplayableItem now grants a
		// rescue only on a confirmed hit, but that hit can go stale (the
		// record cleared between lookup and sync), so the installed
		// scope's own count remains the final authority (#310 review).
		var scopeSyncEnabled int
		// probeVerdicts retains the preflight's per-source answers so the final
		// projection can be re-checked against them without re-probing. See the
		// re-check below the reprojection.
		var probeVerdicts map[string]offlinecache.SourceProbeResult
		if commandType == commands.CMD_DISPLAY_PLAYLIST {
			delete(command.Arguments, "retireBlockedCurrent") // internal refresh-only signal
			// The content-policy lock is taken further down, immediately before
			// the filter, NOT here: URL and dynamic resolution between here and
			// there are network-bound (the shared 30s HTTP timeout on a
			// caller-supplied URL), and holding this lock across them would
			// block getContentPolicy and setContentPolicy for that long — an
			// owner could not promptly apply a more restrictive policy, and a
			// slow playlist origin would become a lock-based denial path. From
			// the filter onward it is held through the player send, which is
			// the ordering the policy contract needs.
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
					// downloaded successfully before) already crossed the
					// daemon's source-trust boundary. Legacy controld does not
					// cryptographically verify playlist signatures here.
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

				// Validate the RAW bytes before the typed decode. A
				// wrong-typed rating (contentRating: 123) fails generic JSON
				// decoding first, so ordering it the other way round reported
				// "failed to unmarshal playlist" — a 500 to the hub — for
				// exactly the malformed-label case the contract classifies as
				// playlistInvalid.
				if err = contentrating.ValidatePlaylistFragment(playlistBytes); err != nil {
					err = &PlaylistInvalidError{Reason: err.Error()}
					return nil, err
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

			// Public ingress, so an explicitly supplied empty or null
			// contentContext is a malformed request, not an omitted field —
			// see NormalizeRequestContext. Restored scheduler state omits the
			// key entirely, which stays compatible.
			rawContext, hasContext := command.Arguments["contentContext"]
			contentContext, contextErr := contentpolicy.NormalizeRequestContext(rawContext, hasContext)
			if contextErr != nil {
				return nil, contextErr
			}
			// Filter under the policy lock, then RELEASE it: the source
			// preflight below is network-bound (a 10s phase ceiling, and up to
			// four heavy casts can be admitted at once), and holding this lock
			// across it would make History and Content controls wait on
			// whatever an unauthenticated LAN caller's playlist origin chooses
			// to do. Filtering still happens BEFORE probing, as the plan
			// requires — only the waiting moved out of the lock. The lock is
			// reacquired below, before the scheduler prepare and player send,
			// and the projection is reapplied there under the policy in force
			// at that moment.
			filterUnderPolicy := func() error {
				if h.contentPolicy == nil {
					return nil
				}
				h.contentPolicy.Lock()
				defer h.contentPolicy.Unlock()
				filtered, filterErr := h.contentPolicy.FilterLocked(&playlist.Playlist, contentContext)
				if filterErr != nil {
					if errors.Is(filterErr, contentpolicy.ErrContentBlocked) {
						return &ContentBlockedError{}
					}
					return filterErr
				}
				playlist.Playlist = *filtered
				return nil
			}
			if filterErr := filterUnderPolicy(); filterErr != nil {
				return nil, filterErr
			}
			command.Arguments["contentContext"] = string(contentContext)
			schedulerSource.ContentContext = string(contentContext)

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
			if h.sourceProber != nil && playlist != nil && len(playlist.Items) > 0 &&
				len(playlist.Items) <= maxPreflightItems {
				sources := make([]string, 0, len(playlist.Items))
				// scheduledPlaylist: any item carrying displayAt makes this
				// a scheduler-filtered playlist, and the probe would see
				// the FULL item list before that filtering. An item outside
				// the current cohort can be dead NOW and live at display
				// time — publish-then-upload is the normal premiere
				// ordering, so a scheduled drop's sources routinely 404
				// until go-live, and rejecting would silently lose the cast
				// the scheduler was about to defer-accept and arm a timer
				// for (the {ok:true, deferred:true} path below). Since the
				// probe can therefore never affect a scheduled cast, it is
				// skipped entirely for them — see the branch below.
				scheduledPlaylist := false
				for _, item := range playlist.Items {
					sources = append(sources, item.Source)
					if item.DisplayAt != nil && *item.DisplayAt != "" {
						scheduledPlaylist = true
					}
				}
				if scheduledPlaylist {
					// Scheduled playlists skip the preflight ENTIRELY, not
					// just its rejection: the probe could never reject them
					// (dead-now is not dead-at-display-time), so on this
					// path it buys only log lines — at a price of up to
					// probePhaseCeiling of added latency sitting directly
					// in front of PrepareWithSource arming the timer and
					// sending the current cohort, which can make a due
					// cutover visibly late (#308 review). The scheduler's
					// timing contract wins; dead scheduled sources surface
					// when the cohort actually fails to render, and via
					// the playlist-refresher follow-up tracked in #304.
					h.logger.Debug("displayPlaylist: displayAt-scheduled playlist; source preflight skipped",
						zap.Int("items", len(sources)))
				} else {
					probeResults := h.sourceProber.ProbeSources(ctx, sources)
					// Keyed by the RAW source, taken from the input slice by
					// index, NOT by result.Source: that field is
					// query-redacted and truncated for the daemon log (see
					// SourceProbeResult's doc), so keying on it would miss
					// every signed URL — and a miss makes the re-check below
					// fail open and forward a cast already proven dead.
					// ProbeSources returns one result per source in input
					// order, which is what makes the index safe.
					probeVerdicts = make(map[string]offlinecache.SourceProbeResult, len(probeResults))
					for i, r := range probeResults {
						if i < len(sources) {
							probeVerdicts[sources[i]] = r
						}
					}
					// Per-item log detail is capped: the hub accepts a 4 MiB
					// playlist with no item cap, so an all-dead hostile cast
					// must not be able to mint one log line per item on a
					// device whose logs are size-rotated files. The first few
					// carry the query-redacted, truncated sources an operator
					// greps for (redaction because uploadLogs ships this log
					// off-device and signed URLs carry credentials in their
					// query strings — see SourceProbeResult.Source); the rest
					// collapse into counts.
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
			}

			if h.sourceProber != nil && playlist != nil && len(playlist.Items) > maxPreflightItems {
				// Over the preflight item budget: skip before building any
				// per-item state (fail open) — see maxPreflightItems.
				h.logger.Warn("displayPlaylist: playlist exceeds the preflight item budget; source preflight skipped",
					zap.Int("items", len(playlist.Items)),
					zap.Int("budget", maxPreflightItems))
			}

			// Preflight is done, so reacquire the policy lock and hold it
			// from here through the scheduler prepare and the player send —
			// the ordering the policy contract needs. Reapply the projection
			// under the policy in force NOW: one tightened while the probe ran
			// must not be outrun by this cast. A policy RELAXED during the
			// probe does not re-admit what was already filtered out, matching
			// the rest of this path — those items were never probed, and a
			// relaxed policy takes effect on the next cast, refresh or cutover.
			//
			// LOCK ORDER, load-bearing: content policy BEFORE the kiosk replay
			// playback lock, which is acquired a few lines below. The
			// playlist-refresher takes the same two in that order
			// (processPlayingPlaylist: policy, then LockPlayback), and both are
			// non-reentrant, so acquiring them the other way round here would
			// let a concurrent cast and refresh deadlock permanently — no
			// further policy update, refresh or playback command until
			// controld restarts.
			if h.contentPolicy != nil && playlist != nil {
				h.contentPolicy.Lock()
				defer h.contentPolicy.Unlock()
				reprojected, filterErr := h.contentPolicy.FilterLocked(&playlist.Playlist, contentContext)
				if filterErr != nil {
					if errors.Is(filterErr, contentpolicy.ErrContentBlocked) {
						err = &ContentBlockedError{}
						return nil, err
					}
					err = filterErr
					return nil, err
				}
				removed := len(reprojected.Items) != len(playlist.Items)
				playlist.Playlist = *reprojected
				command.Arguments["dp1_call"] = playlist
				// A policy tightened while the probe ran can remove the very
				// item whose reachability made this cast acceptable, leaving
				// only sources already proven dead. Re-check the retained
				// verdicts against the final set rather than re-probing: the
				// answers are seconds old and the items are a subset of the
				// ones probed.
				if removed && !rescuedByCache {
					if deadResults, allDead := allSourcesDead(playlist, probeVerdicts); allDead {
						if h.offlineCache != nil && h.kioskReplay != nil && h.offlineCache.HasReplayableItem(playlistSources(playlist)...) {
							rescuedByCache = true
							rescueProbeResults = deadResults
							h.logger.Warn("displayPlaylist: the policy projection left only unreachable sources but cached captures exist; casting for offline replay",
								zap.Int("items", len(playlist.Items)))
						} else {
							err = &SourceUnreachableError{Results: deadResults}
							return nil, err
						}
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
			// scopeSyncErr/scopeSyncEnabled surface the outcome for the ONE
			// caller that must treat a failure — or an empty armed scope —
			// as fatal: the cache-rescued all-dead cast (see
			// rescuedByCache's and scopeSyncEnabled's docs). Ordinary live
			// casts keep the best-effort contract: log and proceed.
			scopeSyncEnabled, scopeSyncErr = h.kioskReplay.SyncPlaylist(ctx, sources)
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
				if rescuedByCache && (scopeSyncErr != nil || scopeSyncEnabled == 0) {
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
				if rescuedByCache && (scopeSyncErr != nil || scopeSyncEnabled == 0) {
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

func (h *handler) handleContentPolicy(command commands.Command) (interface{}, error) {
	if h.contentPolicy == nil {
		return policyFailure("contentPolicyUnavailable"), nil
	}
	// Argument validation happens BEFORE the lock. A malformed request is
	// rejected on its own shape, so it must not queue behind a cast that is
	// holding the policy lock — otherwise the cheapest possible bad request
	// still pays a cast's latency.
	var show, strict bool
	if command.Type == commands.CMD_SET_CONTENT_POLICY {
		if len(command.Arguments) != 2 {
			return policyFailure("invalidRequest"), nil
		}
		var okShow, okStrict bool
		show, okShow = command.Arguments["showMatureContent"].(bool)
		strict, okStrict = command.Arguments["strictPersonal"].(bool)
		if !okShow || !okStrict {
			return policyFailure("invalidRequest"), nil
		}
	} else if len(command.Arguments) != 0 {
		// getContentPolicy takes no arguments. Rejecting a non-empty request is
		// not pedantry: the storm gate's dedupe key is type+arguments, so
		// silently ignoring junk arguments would let one LAN caller mint
		// unlimited distinct keys and defeat the query-tier dedupe that bounds
		// this command (gate.go).
		return policyFailure("invalidRequest"), nil
	}

	h.contentPolicy.Lock()
	defer h.contentPolicy.Unlock()
	policy := h.contentPolicy.CurrentLocked()
	if command.Type == commands.CMD_SET_CONTENT_POLICY {
		// Serialized against scheduler-owned pushes, not just against casts.
		// A timer push holding pushMu has already read the old policy through
		// the lock-free Snapshot and built its payload; without this barrier
		// setContentPolicy could persist, be acknowledged, and answer
		// active:true while that pending cutover still delivered the old
		// cohort. The lock order is the same one displayPlaylist uses — policy
		// store, then pushMu — and the projector deliberately takes no store
		// lock, so it cannot invert (see playlistschedule.Projector).
		var reply interface{}
		update := func() { reply = h.applyContentPolicyLocked(show, strict) }
		if h.scheduler != nil {
			h.scheduler.WithPlayerPush(update)
		} else {
			update()
		}
		return reply, nil
	}
	// A store that could not read its file keeps admitting on safe defaults,
	// but it must not present those defaults as the saved user setting.
	if !h.contentPolicy.DurableLocked() {
		return policyFailure("contentPolicyUnavailable"), nil
	}
	result, err := h.sendContentPolicyCDP(commands.CMD_GET_CONTENT_POLICY, map[string]interface{}{})
	if err != nil || !policyAckMatches(result, policy) {
		return policyFailure(policyFailureCode(result, err)), nil
	}
	return map[string]interface{}{"ok": true, "contentPolicy": policy, "active": true}, nil
}

// applyContentPolicyLocked persists the requested policy and reconciles the
// player. The caller holds the content-policy store lock, and — when a
// scheduler exists — the player-push lock, so no cutover can interleave
// between the durable write and its acknowledgement.
func (h *handler) applyContentPolicyLocked(show, strict bool) interface{} {
	// Acknowledgement FIRST, then the durable write. Persisting first and then
	// discovering the player refused would leave a caller told this failed with
	// a device that had nevertheless changed what it admits — and since the file
	// is the only thing a restart restores, the refused policy would come back
	// as the active one on the next boot. Only acknowledged values are ever
	// written, so neither is possible and there is no second persisted state to
	// reconcile.
	candidate := h.contentPolicy.CandidateLocked(show, strict)
	result, err := h.sendContentPolicyCDP(commands.CMD_SET_CONTENT_POLICY, map[string]interface{}{"contentPolicy": candidate})
	if err != nil || !policyAckMatches(result, candidate) {
		return policyFailure(policyFailureCode(result, err))
	}
	// The player accepted but the write may still fail. The daemon then keeps
	// its old policy, so the player is the one out of step — and waiting for a
	// reconnect to fix that leaves the two enforcement points diverged for as
	// long as the device stays up. Put the player back on the stored policy
	// immediately, under the barriers already held here.
	policy, err := h.contentPolicy.UpdateLocked(show, strict)
	// The playlist ON SCREEN was projected under the OLD policy. Enabling
	// mature content cannot bring back items the previous projection removed,
	// and disabling it leaves blocked items up, until something re-resolves —
	// which otherwise means the periodic refresh, minutes later, while the
	// Content screen has already reported success. ForceRefresh only signals a
	// channel, so it is safe to call with this lock held; the pass it wakes
	// takes the lock itself, after this returns.
	committed := false
	defer func() {
		if committed && h.policyRefresher != nil {
			h.policyRefresher.ForceRefresh()
		}
	}()
	if errors.Is(err, contentpolicy.ErrDurabilityUncertain) {
		// The file IS in place and every surface agrees on it — memory, the
		// snapshot, and the player. What is unconfirmed is only whether the
		// directory entry survives a power loss, so retry that fsync before
		// deciding what to report.
		committed = true
		// ConfirmDurableLocked clears the store's unconfirmed flag on success,
		// which is what lets a later getContentPolicy report the policy as
		// saved again. While it stays set, DurableLocked reads false and every
		// RPC answers contentPolicyUnavailable — not just this one.
		if confirmErr := h.contentPolicy.ConfirmDurableLocked(); confirmErr == nil {
			return map[string]interface{}{"ok": true, "contentPolicy": policy, "active": true}
		} else {
			// Still unconfirmed. The API's success means "saved", and a restart
			// could still revert this, so do not claim it: report unavailable
			// and leave the applied, self-consistent state in place rather than
			// rolling the player back to a policy the file no longer holds.
			h.logger.Error("content policy written but its directory entry could not be made durable; reporting unavailable",
				zap.Error(err), zap.NamedError("confirm", confirmErr))
			return policyFailure("contentPolicyUnavailable")
		}
	}
	if err != nil {
		previous := h.contentPolicy.CurrentLocked()
		rollback, rollbackErr := h.sendContentPolicyCDP(commands.CMD_SET_CONTENT_POLICY, map[string]interface{}{"contentPolicy": previous})
		if rollbackErr != nil || !policyAckMatches(rollback, previous) {
			// Could not confirm the restore; the player may still be on the
			// rejected values until the next reconnect sync. Say unavailable
			// either way, and leave evidence for that case specifically.
			h.logger.Error("content policy write failed and the player could not be restored to the stored policy",
				zap.Error(err), zap.NamedError("rollback", rollbackErr))
		} else {
			h.logger.Warn("content policy write failed; player restored to the stored policy", zap.Error(err))
		}
		return policyFailure("contentPolicyUnavailable")
	}
	committed = true
	return map[string]interface{}{"ok": true, "contentPolicy": policy, "active": true}
}

// allSourcesDead reports whether EVERY item left in playlist has a retained
// preflight verdict and every one of those verdicts is definitively dead. A
// single item with no verdict (never probed, or the preflight was skipped)
// makes it false: this must only ever reject a cast the preflight itself would
// have rejected, never one it never judged.
func allSourcesDead(playlist *dp1.Playlist, verdicts map[string]offlinecache.SourceProbeResult) ([]offlinecache.SourceProbeResult, bool) {
	if playlist == nil || len(playlist.Items) == 0 || len(verdicts) == 0 {
		return nil, false
	}
	results := make([]offlinecache.SourceProbeResult, 0, len(playlist.Items))
	for _, item := range playlist.Items {
		r, probed := verdicts[item.Source]
		if !probed || r.Verdict != offlinecache.ProbeDead {
			return nil, false
		}
		results = append(results, r)
	}
	return results, true
}

// playlistSources lists the item source URLs of playlist, the cache's identity
// for a replay lookup.
func playlistSources(playlist *dp1.Playlist) []string {
	if playlist == nil {
		return nil
	}
	sources := make([]string, 0, len(playlist.Items))
	for _, item := range playlist.Items {
		sources = append(sources, item.Source)
	}
	return sources
}

func policyFailure(code string) interface{} {
	return map[string]interface{}{"ok": false, "error": code}
}

// policyFailureCode classifies a player reply the same way recentPlayerReply
// classifies the history commands, so the app can tell "this device cannot do
// it" from "this device is temporarily out of sync".
//
// sendErr is the transport outcome and is decisive: a send that never reached
// the player says nothing about its capabilities.
//
// A player that predates these commands answers the unknown command with a
// bare {"ok":false} carrying no error code and no contentPolicy — identical
// in shape to the legacy history reply — so that shape maps to unsupported.
// Anything else (a modern failure code, or an ok reply whose policy does not
// match) stays contentPolicyUnavailable.
func policyFailureCode(result interface{}, sendErr error) string {
	if sendErr != nil {
		return "contentPolicyUnavailable"
	}
	m, ok := result.(map[string]interface{})
	if !ok {
		return "contentPolicyUnavailable"
	}
	if msg, ok := m["message"].(map[string]interface{}); ok {
		m = msg
	}
	if code, _ := m["error"].(string); code == "unsupported" {
		return code
	}
	// Same whole-shape rule as the history reply: only an exactly bare
	// {"ok":false} is a pre-feature player. Anything carrying its own
	// explanation — an error, a code such as "busy", a result — is a modern,
	// possibly retryable failure and must not be reported as a capability the
	// device lacks.
	if isBareLegacyFailure(m) {
		return "unsupported"
	}
	return "contentPolicyUnavailable"
}

func (h *handler) sendContentPolicyCDP(commandType commands.Type, request map[string]interface{}) (interface{}, error) {
	cmd := commands.Command{Type: commandType, Arguments: request}
	b, err := cmd.JSON()
	if err != nil {
		return nil, err
	}
	genBefore := h.currentGeneration()
	result, err := h.cdp.Send(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression":    fmt.Sprintf("window.handleCDPRequest(%s)", b),
		"awaitPromise":  true,
		"returnByValue": true,
	})
	if err != nil {
		return nil, err
	}
	if genAfter := h.currentGeneration(); genAfter != genBefore {
		return nil, fmt.Errorf("content policy acknowledgement raced player generation change: %w", ErrGenerationRace)
	}
	return result, nil
}

func policyAckMatches(result interface{}, want contentpolicy.Policy) bool {
	m, ok := result.(map[string]interface{})
	if !ok {
		return false
	}
	if msg, ok := m["message"].(map[string]interface{}); ok {
		m = msg
	}
	okValue, _ := m["ok"].(bool)
	active, _ := m["active"].(bool)
	if !okValue || !active {
		return false
	}
	// The acknowledgement must carry a COMPLETE v1 policy. Decoding into a
	// plain Policy made `{"version":1}` decode to the all-false default, which
	// compares equal to it — so a player that echoed nothing at all would look
	// like it had acknowledged the default policy and let it be committed.
	raw, present := m["contentPolicy"]
	if !present {
		return false
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return false
	}
	got, err := contentpolicy.ParseComplete(b)
	if err != nil {
		return false
	}
	return got == want
}

// recentPlayerReply normalizes the CDP envelope only enough to classify the
// history capability. No raw DP-1 item is added to the public list response.
func recentPlayerReply(result interface{}) map[string]interface{} {
	response, ok := result.(map[string]interface{})
	if !ok {
		return map[string]interface{}{"ok": false, "status": "error", "error": "invalid player reply"}
	}
	message, ok := response["message"].(map[string]interface{})
	if !ok {
		message = response
		response = map[string]interface{}{"message": message}
	}
	if okValue, _ := message["ok"].(bool); !okValue {
		if _, hasStatus := message["status"]; !hasStatus {
			if isBareLegacyFailure(message) {
				// ff-player before #729 has only the generic unknown-command
				// reply: ok:false and nothing else.
				message["status"] = "unsupported"
				message["error"] = "Recently played is not supported by this player"
			} else {
				// A modern player that failed and said why. Give it the
				// explicit error status and KEEP its own error: calling an
				// evicted or malformed record "unsupported" would tell the app
				// the device cannot do this at all, when the right answer is a
				// real, possibly retryable failure.
				message["status"] = "error"
			}
		}
	}
	return response
}

// boundedReplayAck reduces the recursive displayPlaylist acknowledgement to the
// documented fields before it leaves the daemon.
//
// Same rule, and the same reason, as boundedRecentlyPlayedReply: this reply is
// reachable from the unauthenticated LAN hub, and the thing being replayed is a
// RETAINED DP-1 item whose source can be a signed URL carrying credentials in
// its query string. A player acknowledgement that echoed the request — or added
// diagnostics — would hand exactly that to the caller, through the one command
// whose whole design keeps the retained item device-local.
//
// Only ok/status/error survive. Anything else is dropped rather than
// allow-listed later: the caller is told whether the replay was accepted, which
// is all this reply ever promised.
func boundedReplayAck(result interface{}) map[string]interface{} {
	message, ok := result.(map[string]interface{})
	if !ok {
		return map[string]interface{}{"ok": false}
	}
	if nested, isNested := message["message"].(map[string]interface{}); isNested {
		message = nested
	}
	bounded := map[string]interface{}{"ok": false}
	if okValue, present := message["ok"].(bool); present {
		bounded["ok"] = okValue
	}
	for _, key := range []string{"status", "error"} {
		if value, present := message[key].(string); present && value != "" {
			bounded[key] = truncateLabel(value)
		}
	}
	return bounded
}

// maxRecentlyPlayedRecords bounds how many history rows leave the daemon. The
// player retains 50; this is generous headroom, not a contract, and exists so a
// misbehaving or replaced player cannot make an unauthenticated LAN request
// return an unbounded body.
const maxRecentlyPlayedRecords = 200

// maxRecentlyPlayedLabelBytes bounds one label field, and
// maxRecentlyPlayedReplyBytes the whole reply's label payload. Rows alone are
// not a bound: the LAN hub accepts a 4 MiB inline playlist from an
// unauthenticated caller, its metadata becomes retained history labels, and
// those come back through this reply — so 200 rows can still be megabytes.
// Over-long labels are truncated rather than dropped, because a clipped title
// still identifies the work for replay; once the aggregate cap is reached the
// remaining rows are omitted, the same as the row cap.
const (
	maxRecentlyPlayedLabelBytes = 512
	maxRecentlyPlayedReplyBytes = 128 * 1024
	// maxRecentlyPlayedRecordIDBytes bounds the opaque replay handle. It is
	// never truncated — see the loop below — so the bound has to drop the row
	// instead, and it is generous: the player mints ids like
	// "rp-1788892946764001".
	maxRecentlyPlayedRecordIDBytes = 256
)

// truncateLabel clips s to at most maxRecentlyPlayedLabelBytes, on a rune
// boundary so the result stays valid UTF-8 on the wire.
func truncateLabel(s string) string {
	if len(s) <= maxRecentlyPlayedLabelBytes {
		return s
	}
	cut := maxRecentlyPlayedLabelBytes
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	return s[:cut]
}

// boundedRecentlyPlayedReply rebuilds a SUCCESSFUL getRecentlyPlayed reply from
// a strict allow-list instead of forwarding whatever the player returned.
//
// The documented contract is that this query exposes only a bounded label
// snapshot: record id, timestamp, active flag, item id, title, artist,
// thumbnail. Item sources and the full retained DP-1 item stay device-local —
// they can be signed URLs carrying credentials in their query strings, and this
// reply is reachable from the unauthenticated LAN hub. Forwarding the player's
// object verbatim made that contract true only for as long as the player
// happened to honor it; a daemon that OWNS the shape cannot be widened by a
// change on the other side of CDP.
//
// Failures are left to recentPlayerReply, which has already classified them:
// only ok:true replies are rebuilt here.
func boundedRecentlyPlayedReply(response map[string]interface{}) map[string]interface{} {
	message, ok := response["message"].(map[string]interface{})
	if !ok {
		return response
	}
	if okValue, _ := message["ok"].(bool); !okValue {
		return response
	}

	bounded := map[string]interface{}{"ok": true}
	if status, present := message["status"].(string); present {
		bounded["status"] = status
	}
	for _, key := range []string{"activeOccurrenceKnown", "incomplete"} {
		if flag, present := message[key].(bool); present {
			bounded[key] = flag
		}
	}
	raw, _ := message["records"].([]interface{})
	records := make([]interface{}, 0, len(raw))
	labelBytes := 0
	for _, entry := range raw {
		if len(records) >= maxRecentlyPlayedRecords || labelBytes >= maxRecentlyPlayedReplyBytes {
			break
		}
		record, isRecord := entry.(map[string]interface{})
		if !isRecord {
			continue
		}
		// recordId is the replay handle, NOT a label: playRecentlyPlayed
		// forwards it verbatim to the player's resolver, so truncating it
		// would advertise a row that deterministically fails to play. It is
		// passed through losslessly, and a value too long to be a plausible
		// handle drops the row instead — an omitted row is honest, an
		// unresolvable one is not.
		recordID, _ := record["recordId"].(string)
		if recordID == "" || len(recordID) > maxRecentlyPlayedRecordIDBytes {
			continue
		}
		bounded := map[string]interface{}{"recordId": recordID}
		labelBytes += len(recordID)
		if playedAt, present := record["playedAtMs"].(float64); present {
			bounded["playedAtMs"] = playedAt
		}
		if isActive, present := record["isActive"].(bool); present {
			bounded["isActive"] = isActive
		}
		for _, key := range []string{"itemId", "title", "artist", "thumbnailUrl"} {
			if label, present := record[key].(string); present && label != "" {
				clipped := truncateLabel(label)
				bounded[key] = clipped
				labelBytes += len(clipped)
			}
		}
		records = append(records, bounded)
	}
	bounded["records"] = records
	return map[string]interface{}{"message": bounded}
}

// isBareLegacyFailure reports whether an unwrapped reply is EXACTLY {"ok":false}
// — no error, no code, no result field, nothing. That is the only shape a
// pre-feature player produces for an unknown command (ff-player's command
// switch returns a bare {ok:false}), so it is the only shape that may be read
// as "this device lacks the capability".
//
// Deliberately a whole-shape check rather than a deny-list of known
// explanatory keys: a deny-list silently mislabels the next field someone adds
// (a "result", a "code") as a missing capability, which tells the app to hide
// a feature the device actually has.
func isBareLegacyFailure(message map[string]interface{}) bool {
	if len(message) != 1 {
		return false
	}
	okValue, hasOK := message["ok"].(bool)
	return hasOK && !okValue
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
