package commandrouter

import (
	"context"
	"errors"
	"sync"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/contentpolicy"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/seriesplayback"
)

// SetSeriesPlayback wires the device-level series-playback override store, if
// h supports it (the concrete *handler built by New, not the gate wrapper) —
// same pattern as SetContentPolicy.
func SetSeriesPlayback(h Handler, store *seriesplayback.Store, logger *zap.Logger) {
	setter, ok := h.(interface{ setSeriesPlayback(*seriesplayback.Store) })
	if !ok {
		logger.Warn("Command handler does not support series playback wiring")
		return
	}
	setter.setSeriesPlayback(store)
}

func (h *handler) setSeriesPlayback(store *seriesplayback.Store) { h.seriesPlayback = store }

// ResetSeriesPlayback returns the device-level series-playback override to
// "follow the document" and removes it from disk. Used by factory reset: the
// owner's override must not outlive their claim, same as content policy and
// the device name (see ResetContentPolicy).
func ResetSeriesPlayback(h Handler) error {
	target, ok := h.(*handler)
	if !ok || target.seriesPlayback == nil {
		return errors.New("series playback unavailable")
	}
	target.seriesPlayback.Lock()
	defer target.seriesPlayback.Unlock()
	_, err := target.seriesPlayback.ResetLocked()
	return err
}

// currentSeriesPlaybackMode reads the device-level override without taking
// the store's lock, mirroring contentpolicy.Store.Snapshot: the cast path
// this feeds (the static dp1_call branch) already runs ahead of the content-
// policy lock and the kiosk playback lock, and must not add a third lock to
// that ordering for a value this cheap to read lock-free. nil means "follow
// the document" — either because no override is set, or because no store was
// wired (a build before this seam, or a test).
func (h *handler) currentSeriesPlaybackMode() *dp1.SeriesPlaybackMode {
	if h.seriesPlayback == nil {
		return nil
	}
	return h.seriesPlayback.Snapshot().Mode
}

// displayedSeriesCast is the most recently cast playlist's resolved series
// groups (feral-file/ffos-user#425's AC5): kept so a device-setting change
// can re-narrow and re-cast WITHOUT a catalog round trip, since the full
// edition set was already fetched to resolve the ORIGINAL cast regardless of
// which mode it narrowed to (see dp1.CastPlan's doc).
//
// seq is a write counter guarding the one race this cache has: recastForMode
// re-enters Process for CMD_DISPLAY_PLAYLIST, and that re-entrant call's own
// dp1_call branch immediately overwrites this cache with a group-less plan
// (the rebuilt, already-narrowed document carries no seriesRegistry marker
// to resolve) — see recastForMode's doc for how seq lets it tell "nothing
// else wrote this cache since" from "a concurrent cast took over the screen
// while we were re-casting" and only restores the group data in the former
// case.
//
// Scoped to ONE playlist, matching the FF1 kiosk's single screen: a new
// CMD_DISPLAY_PLAYLIST cast (of any kind — see rememberSeriesCast) always
// replaces this wholesale, never merges into it.
type displayedSeriesCast struct {
	mu             sync.Mutex
	seq            uint64
	valid          bool
	shell          dp1.Playlist
	plan           dp1.CastPlan
	contentContext contentpolicy.Context
}

// rememberSeriesCast records what Process just resolved for a
// CMD_DISPLAY_PLAYLIST cast. Called for EVERY such cast, not only a static
// dp1_call with series groups: plan is the zero value (no segments) for the
// playlistUrl and dynamic branches, and for a static cast with none, which
// correctly CLEARS any series this device was previously tracking — a stale
// entry must not outlive what is actually on screen.
//
// Called eagerly, before this cast's own preflight/CDP send, so the
// returned seq lets Process retract the entry with forgetSeriesCast when the
// cast is then rejected (preflight, policy, or the player). The retraction
// is load-bearing, not tidiness: a rejected entry left valid would let a
// LATER setSeriesPlayback re-cast content that never reached the screen, and
// that re-cast does NOT fail the same way — narrowing changes which sources
// the preflight checks, so a series whose editions all 404 but whose
// representative is reachable is rejected in full yet accepted narrowed to
// "first", replacing whatever is actually on screen (PR #429 review F2).
func (h *handler) rememberSeriesCast(playlist dp1.Playlist, plan dp1.CastPlan, contentContext contentpolicy.Context) uint64 {
	h.lastSeriesCast.mu.Lock()
	defer h.lastSeriesCast.mu.Unlock()
	h.lastSeriesCast.seq++
	if !plan.HasSeriesGroups() {
		h.lastSeriesCast.valid = false
		h.lastSeriesCast.shell = dp1.Playlist{}
		h.lastSeriesCast.plan = dp1.CastPlan{}
		h.lastSeriesCast.contentContext = ""
		return h.lastSeriesCast.seq
	}
	shell := playlist
	shell.Items = nil // rebuilt from plan.Narrow on re-cast; the shell carries only playlist-level metadata.
	h.lastSeriesCast.valid = true
	h.lastSeriesCast.shell = shell
	h.lastSeriesCast.plan = plan
	h.lastSeriesCast.contentContext = contentContext
	return h.lastSeriesCast.seq
}

// forgetSeriesCast retracts the entry rememberSeriesCast recorded as seq,
// for a cast that never reached the screen. A no-op when anything has
// written the cache since (seq moved on): that later write describes a
// newer cast and must stand.
//
// Deliberately does NOT advance seq. recastForMode tells "only my own
// re-entrant cast wrote this cache" by seq == seqBefore+1; when that
// re-entrant cast fails, this retraction must not disturb the count, so
// recastForMode still restores the series that IS on screen (the failed
// re-cast left it there).
func (h *handler) forgetSeriesCast(seq uint64) {
	h.lastSeriesCast.mu.Lock()
	defer h.lastSeriesCast.mu.Unlock()
	if h.lastSeriesCast.seq != seq {
		return
	}
	h.lastSeriesCast.valid = false
	h.lastSeriesCast.shell = dp1.Playlist{}
	h.lastSeriesCast.plan = dp1.CastPlan{}
	h.lastSeriesCast.contentContext = ""
}

// clearSeriesCast invalidates the displayed-series cache without recording a
// replacement — used when a command other than CMD_DISPLAY_PLAYLIST may have
// taken over the screen but has no plan of its own to remember.
// CMD_DISPLAY_DEFAULT_PLAYLIST is exactly that case (feralfile-bot F2,
// 2026-10-30): clearVerdictForDefaultPlayback's own doc already establishes
// that a successful player response does not PROVE default playback
// replaced the current content — it may have no-opped under
// onlyIfNoPlaylist — but leaving the series cache valid on that uncertainty
// risks the opposite failure: a later setSeriesPlayback's recastForMode
// would replace genuine default content with the stale series. Clearing on
// an uncertain accept is the same safe direction clearVerdictForDefaultPlayback
// already takes for the verdict (an omission, never a stale claim): the
// worst case of a GENUINE no-op is one missed network-free re-narrow, which
// the series' own next CMD_DISPLAY_PLAYLIST cast repopulates.
func (h *handler) clearSeriesCast() {
	h.lastSeriesCast.mu.Lock()
	defer h.lastSeriesCast.mu.Unlock()
	h.lastSeriesCast.seq++
	h.lastSeriesCast.valid = false
	h.lastSeriesCast.shell = dp1.Playlist{}
	h.lastSeriesCast.plan = dp1.CastPlan{}
	h.lastSeriesCast.contentContext = ""
}

// handleSeriesPlayback serves getSeriesPlayback/setSeriesPlayback. Both are
// controld-owned and never forwarded to window.handleCDPRequest: the player
// has no notion of a series to configure (see commands.CMD_GET_SERIES_PLAYBACK's
// doc) — unlike content policy, there is no player acknowledgement to await.
func (h *handler) handleSeriesPlayback(ctx context.Context, command commands.Command) (interface{}, error) {
	if h.seriesPlayback == nil {
		return seriesPlaybackFailure("seriesPlaybackUnavailable"), nil
	}

	if command.Type == commands.CMD_SET_SERIES_PLAYBACK {
		return h.applySeriesPlayback(ctx, command.Arguments)
	}

	// getSeriesPlayback takes no arguments — rejecting a non-empty request,
	// not ignoring it, for the same storm-gate dedupe reason
	// handleContentPolicy rejects a non-empty getContentPolicy (gate.go: the
	// query tier's dedupe key is type+arguments).
	if len(command.Arguments) != 0 {
		return seriesPlaybackFailure("invalidRequest"), nil
	}
	h.seriesPlayback.Lock()
	defer h.seriesPlayback.Unlock()
	if !h.seriesPlayback.DurableLocked() {
		return seriesPlaybackFailure("seriesPlaybackUnavailable"), nil
	}
	setting := h.seriesPlayback.CurrentLocked()
	return map[string]interface{}{"ok": true, "seriesPlayback": seriesPlaybackModeOrNil(setting.Mode), "active": true}, nil
}

// applySeriesPlayback persists the requested override and, when a series is
// currently displayed, re-casts it under the new resolution.
func (h *handler) applySeriesPlayback(ctx context.Context, args map[string]interface{}) (interface{}, error) {
	mode, ok := parseSeriesPlaybackRequest(args)
	if !ok {
		return seriesPlaybackFailure("invalidRequest"), nil
	}

	h.seriesPlayback.Lock()
	setting, err := h.seriesPlayback.UpdateLocked(mode)
	if errors.Is(err, seriesplayback.ErrDurabilityUncertain) {
		if confirmErr := h.seriesPlayback.ConfirmDurableLocked(); confirmErr != nil {
			h.seriesPlayback.Unlock()
			h.logger.Error("series playback setting written but its directory entry could not be made durable; reporting unavailable",
				zap.Error(err), zap.NamedError("confirm", confirmErr))
			return seriesPlaybackFailure("seriesPlaybackUnavailable"), nil
		}
		err = nil
	}
	h.seriesPlayback.Unlock()
	if err != nil {
		h.logger.Error("series playback setting write failed", zap.Error(err))
		return seriesPlaybackFailure("seriesPlaybackUnavailable"), nil
	}

	// Best-effort from here: the setting is already saved and reported below
	// regardless of what this does. What is currently on screen is a
	// separate concern — see scheduleRecast/recastForMode's own docs.
	h.scheduleRecast(ctx, setting.Mode)

	return map[string]interface{}{"ok": true, "seriesPlayback": seriesPlaybackModeOrNil(setting.Mode), "active": true}, nil
}

// seriesRecastCoalescer bounds how many real re-casts a burst of
// setSeriesPlayback calls performs. Without it, applySeriesPlayback would
// need to hold something across recastForMode's own re-entrant, network-bound
// preflight to stop two concurrent recastForMode calls from racing
// lastSeriesCast.valid through each other's preflight window (feralfile-bot
// F7) — but that lock would also hold the command gate's shared heavy-tier
// dispatch weight for the SUM of every stacked call's preflight instead of
// at most two of them (F7 fix review, PR #426 finding F1: the gate's
// MaxConcurrent semaphore is shared across every command type, and the
// weight is released only when the call returns). Coalescing avoids both:
// at most one recastForMode runs at a time, and a call that arrives while
// one is running only records the newest mode for the in-flight run's own
// final pass, rather than queuing a second full run or racing it directly.
type seriesRecastCoalescer struct {
	mu          sync.Mutex
	running     bool
	havePending bool
	pendingMode *dp1.SeriesPlaybackMode
}

// scheduleRecast re-narrows and re-casts the currently displayed series for
// mode (see recastForMode's own doc for the no-op cases), coalescing with any
// recast already in flight: if one is running, mode is recorded as the
// latest request and this call returns immediately — relying on the running
// call to perform exactly one more pass for whatever mode is newest once it
// finishes, then check again. A single burst of N calls arriving while one
// recast runs therefore performs at most two real re-casts (the first
// caller's own, plus one coalesced follow-up for the latest superseding
// mode), not N serialized ones; only sustained, continuous arrivals — a new
// request landing after every check, not just during the first pass — could
// extend that past two, and only ever by picking up the single latest mode
// per pass, never by queuing or re-running a superseded one. Either way,
// only the one call actually driving a recastForMode call at any moment
// holds its gate dispatch weight for a preflight's duration — every other
// stacked call returns as soon as its own persist (and, if it loses the race
// to start the run, its enqueue) completes.
func (h *handler) scheduleRecast(ctx context.Context, mode *dp1.SeriesPlaybackMode) {
	h.seriesRecast.mu.Lock()
	if h.seriesRecast.running {
		h.seriesRecast.havePending = true
		h.seriesRecast.pendingMode = mode
		h.seriesRecast.mu.Unlock()
		return
	}
	h.seriesRecast.running = true
	h.seriesRecast.mu.Unlock()

	current := mode
	for {
		h.recastForMode(ctx, current)

		h.seriesRecast.mu.Lock()
		if !h.seriesRecast.havePending {
			h.seriesRecast.running = false
			h.seriesRecast.mu.Unlock()
			return
		}
		current = h.seriesRecast.pendingMode
		h.seriesRecast.havePending = false
		h.seriesRecast.pendingMode = nil
		h.seriesRecast.mu.Unlock()
	}
}

// recastForMode re-narrows the currently displayed series (if any) under
// device and re-casts it, re-entering Process for CMD_DISPLAY_PLAYLIST
// exactly as CMD_RECENTLY_PLAYED_REPLAY already does to replay a stored
// record (handler.go) — an established pattern for feeding an internally
// rebuilt dp1_call back through the whole normal cast pipeline (policy
// filter, preflight, scheduler) rather than re-implementing any of it here.
//
// A no-op when nothing is currently tracked as displaying a series — which
// is the honest answer, not a missing re-fetch: once a series is narrowed
// and cast, the player's playlist carries no seriesRegistry marker (AC3), so
// there is no way to learn "a series is on screen" again except from THIS
// cache. A cache that was never populated this boot (process just started,
// or the current display never went through a static dp1_call with series
// groups) has no series identity to re-fetch editions FOR.
func (h *handler) recastForMode(ctx context.Context, device *dp1.SeriesPlaybackMode) {
	h.lastSeriesCast.mu.Lock()
	if !h.lastSeriesCast.valid {
		h.lastSeriesCast.mu.Unlock()
		return
	}
	shell := h.lastSeriesCast.shell
	plan := h.lastSeriesCast.plan
	contentContext := h.lastSeriesCast.contentContext
	seqBefore := h.lastSeriesCast.seq
	h.lastSeriesCast.mu.Unlock()

	rebuilt := shell
	rebuilt.Items = plan.Narrow(device)
	// The source document's signature covered a different item set; this
	// rebuild is controld's own, same as CMD_RECENTLY_PLAYED_REPLAY's.
	rebuilt.Signatures = nil
	rebuilt.Signature = ""

	b, err := h.json.Marshal(rebuilt)
	if err != nil {
		h.logger.Error("series playback: failed to rebuild the displayed playlist for re-cast", zap.Error(err))
		return
	}
	var dp1Call map[string]interface{}
	if err := h.json.Unmarshal(b, &dp1Call); err != nil {
		h.logger.Error("series playback: failed to re-decode the rebuilt playlist for re-cast", zap.Error(err))
		return
	}

	recastArgs := map[string]interface{}{"dp1_call": dp1Call}
	if contentContext != "" {
		recastArgs["contentContext"] = string(contentContext)
	}
	_, procErr := h.Process(ctx, commands.Command{Type: commands.CMD_DISPLAY_PLAYLIST, Arguments: recastArgs})

	// The re-entrant Process call's own dp1_call branch just ran
	// rememberSeriesCast on the rebuilt (already-narrowed) document, which
	// carries no seriesRegistry marker and so cleared this cache. Restore
	// the FULL group data so a LATER setting change can still re-narrow
	// without a catalog call — but only if nothing else wrote the cache in
	// between: seq advances by exactly 1 for that re-entrant write alone: a
	// concurrent displayPlaylist landing in the same window advances it
	// further, and that cast's own cache entry must stand, not this one's.
	h.lastSeriesCast.mu.Lock()
	if h.lastSeriesCast.seq == seqBefore+1 {
		h.lastSeriesCast.valid = true
		h.lastSeriesCast.shell = shell
		h.lastSeriesCast.plan = plan
		h.lastSeriesCast.contentContext = contentContext
	}
	h.lastSeriesCast.mu.Unlock()

	if procErr != nil {
		h.logger.Warn("series playback: re-cast after a setting change failed", zap.Error(procErr))
	}
}

func parseSeriesPlaybackRequest(args map[string]interface{}) (*dp1.SeriesPlaybackMode, bool) {
	if len(args) != 1 {
		return nil, false
	}
	raw, present := args["seriesPlayback"]
	if !present {
		return nil, false
	}
	if raw == nil {
		return nil, true
	}
	s, ok := raw.(string)
	if !ok {
		return nil, false
	}
	mode, ok := dp1.ParseSeriesPlaybackMode(s)
	if !ok {
		return nil, false
	}
	return &mode, true
}

func seriesPlaybackModeOrNil(mode *dp1.SeriesPlaybackMode) interface{} {
	if mode == nil {
		return nil
	}
	return string(*mode)
}

func seriesPlaybackFailure(code string) interface{} {
	return map[string]interface{}{"ok": false, "error": code}
}
