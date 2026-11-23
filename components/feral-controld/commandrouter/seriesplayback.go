package commandrouter

import (
	"context"
	"errors"
	"sync"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/contentpolicy"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/playerresponse"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/seriesplayback"
)

// recastSchedulerSourceHint is the internal-only command.Arguments key
// recastForMode attaches to its re-entrant CMD_DISPLAY_PLAYLIST call, the
// same pattern as "retireBlockedCurrent"/"mediaHealth" (handler.go): a Go
// playlistschedule.Source value a decoded wire command could never produce
// (JSON decoding never yields this struct type), so the type assertion
// reading it IS the boundary against a caller injecting one. It carries the
// ORIGINAL display's scheduler source (the playlistUrl or inline dynamic
// playlist that started it) across the rebuilt-as-static-dp1_call re-display,
// so handler.go can restore it instead of letting the static branch's own
// zero-value schedulerSource overwrite the scheduler's durable source
// (feralfile-bot F1 on PR #454, 2026-11-10).
const recastSchedulerSourceHint = "recastSchedulerSource"

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

// SeriesPlaybackSource exposes h's own device-level series-playback getter
// (currentSeriesPlaybackMode) to callers outside this package — namely
// main.go, which wires status.Poller.SetSeriesPlaybackSource against the
// same handler it builds here, so the poller's pushed player_status and
// this package's own direct checkStatus reply (nativeStatusReply,
// called from the native snapshot boundary) read the identical device-level store and can
// never disagree. ok is false (fn nil) when h is not a *handler built by
// New — same concrete-handler-only, pre-NewGate contract as
// SetOverlaySource/SetSeriesPlayback; the method itself is nil-safe even
// without a store wired (it returns nil, meaning "follow the document"),
// so ok says nothing about whether a store was wired, only whether this
// handler type supports the seam at all.
func SeriesPlaybackSource(h Handler) (fn func() *dp1.SeriesPlaybackMode, ok bool) {
	target, ok := h.(*handler)
	if !ok {
		return nil, false
	}
	return target.currentSeriesPlaybackMode, true
}

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
// the store's lock, mirroring contentpolicy.Store.Snapshot: the display request paths
// this feeds (all three CMD_DISPLAY_PLAYLIST branches — playlistUrl, dp1_call
// static, dp1_call dynamic, feral-file/ffos-user#452) already run ahead of
// the content-policy lock and the kiosk playback lock, and must not add a
// fourth lock to that ordering for a value this cheap to read lock-free. nil
// means "follow the document" — either because no override is set, or
// because no store was wired (a build before this seam, or a test).
func (h *handler) currentSeriesPlaybackMode() *dp1.SeriesPlaybackMode {
	if h.seriesPlayback == nil {
		return nil
	}
	return h.seriesPlayback.Snapshot().Mode
}

// displayedSeriesCast is the most recently displayed playlist's resolved series
// groups (feral-file/ffos-user#425's AC5): kept so a device-setting change
// can re-narrow and re-display WITHOUT a catalog round trip, since the full
// edition set was already fetched to resolve the ORIGINAL display request regardless of
// which mode it narrowed to (see dp1.CastPlan's doc).
//
// seq is a write counter guarding the one race this cache has: recastForMode
// re-enters Process for CMD_DISPLAY_PLAYLIST, and that re-entrant call's own
// dp1_call branch immediately overwrites this cache with a group-less plan
// (the rebuilt, already-narrowed document carries no seriesRegistry marker
// to resolve) — see recastForMode's doc for how seq lets it tell "nothing
// else wrote this cache since" from "a concurrent display request took over the screen
// while we were re-displaying" and only restores the group data in the former
// case.
//
// accepted is tracked separately from seq and valid: an entry is recorded
// before its request's preflight and player send (see rememberSeriesCast), so
// valid alone only means "a display request of this series was attempted". Only an
// accepted entry describes what is on screen, and recastForMode acts on
// nothing else. seq cannot stand in for it: a failed re-display that overlaps a
// still-pending display request moves seq on, after which that request's own settle no
// longer matches and could not have retracted the entry (PR #431 review F2).
//
// Scoped to ONE playlist, matching the FF1 kiosk's single screen: a new
// CMD_DISPLAY_PLAYLIST display request request (of any kind — see rememberSeriesCast) always
// replaces this wholesale, never merges into it.
type displayedSeriesCast struct {
	mu             sync.Mutex
	seq            uint64
	valid          bool
	accepted       bool
	shell          dp1.Playlist
	plan           dp1.CastPlan
	contentContext contentpolicy.Context
	// source is the ORIGINAL display's scheduler source (playlistUrl or inline
	// dynamic playlist) — kept so recastForMode can carry it through its
	// rebuilt-as-static-dp1_call re-display via recastSchedulerSourceHint,
	// rather than letting the static branch's own zero-value schedulerSource
	// silently reset the scheduler's durable source (feralfile-bot F1 on PR
	// #454, 2026-11-10).
	source playlistschedule.Source
}

// rememberSeriesCast records what Process just resolved for a
// CMD_DISPLAY_PLAYLIST display request request. Called for EVERY such display request, not only a static
// dp1_call with series groups: plan is the zero value (no segments) for the
// playlistUrl and dynamic branches, and for a static display request with none, which
// correctly CLEARS any series this device was previously tracking — a stale
// entry must not outlive what is actually on screen.
//
// Called eagerly, before this request's own preflight/CDP send, so the entry
// starts unaccepted and Process settles it with settleSeriesCast once the
// player's answer is known. Acting only on accepted entries is load-bearing,
// not tidiness: a rejected entry would let a LATER setSeriesPlayback re-display
// content that never reached the screen, and that re-display does NOT fail the
// same way — narrowing changes which sources the preflight checks, so a
// series whose editions all 404 but whose representative is reachable is
// rejected in full yet accepted narrowed to "first", replacing whatever is
// actually on screen (PR #429 review F2).
func (h *handler) rememberSeriesCast(playlist dp1.Playlist, plan dp1.CastPlan, contentContext contentpolicy.Context, source playlistschedule.Source) uint64 {
	h.lastSeriesCast.mu.Lock()
	defer h.lastSeriesCast.mu.Unlock()
	h.lastSeriesCast.seq++
	h.lastSeriesCast.accepted = false
	if !plan.HasSeriesGroups() {
		h.lastSeriesCast.valid = false
		h.lastSeriesCast.shell = dp1.Playlist{}
		h.lastSeriesCast.plan = dp1.CastPlan{}
		h.lastSeriesCast.contentContext = ""
		h.lastSeriesCast.source = playlistschedule.Source{}
		return h.lastSeriesCast.seq
	}
	shell := playlist
	shell.Items = nil // rebuilt from plan.Narrow on re-display; the shell carries only playlist-level metadata.
	h.lastSeriesCast.valid = true
	h.lastSeriesCast.shell = shell
	h.lastSeriesCast.plan = plan
	h.lastSeriesCast.contentContext = contentContext
	h.lastSeriesCast.source = source
	return h.lastSeriesCast.seq
}

// settleSeriesCast records the player's answer for the entry
// rememberSeriesCast recorded as seq: an accepted display request (including a
// displayAt-deferred acceptance) marks it accepted, anything else retracts
// it. A no-op when anything has written the cache since (seq moved on):
// that later write describes a newer display request and must stand. If seq moved on
// while this display request was still unaccepted, the entry it wrote is already gone
// (overwritten), so there is nothing left to retract.
//
// Deliberately does NOT advance seq. recastForMode tells "only my own
// re-entrant display request wrote this cache" by seq == seqBefore+1; settling that
// re-entrant display request must not disturb the count, so recastForMode still
// restores the series that IS on screen when the re-display fails.
func (h *handler) settleSeriesCast(seq uint64, accepted bool) {
	h.lastSeriesCast.mu.Lock()
	defer h.lastSeriesCast.mu.Unlock()
	if h.lastSeriesCast.seq != seq {
		return
	}
	if accepted {
		h.lastSeriesCast.accepted = true
		return
	}
	h.lastSeriesCast.valid = false
	h.lastSeriesCast.shell = dp1.Playlist{}
	h.lastSeriesCast.plan = dp1.CastPlan{}
	h.lastSeriesCast.contentContext = ""
	h.lastSeriesCast.source = playlistschedule.Source{}
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
// the series' own next CMD_DISPLAY_PLAYLIST display request request repopulates.
func (h *handler) clearSeriesCast() {
	h.lastSeriesCast.mu.Lock()
	defer h.lastSeriesCast.mu.Unlock()
	h.lastSeriesCast.seq++
	h.lastSeriesCast.valid = false
	h.lastSeriesCast.accepted = false
	h.lastSeriesCast.shell = dp1.Playlist{}
	h.lastSeriesCast.plan = dp1.CastPlan{}
	h.lastSeriesCast.contentContext = ""
	h.lastSeriesCast.source = playlistschedule.Source{}
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
// currently displayed, re-displays it under the new resolution.
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

// seriesRecastCoalescer bounds how many real re-displays a burst of
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

// scheduleRecast re-narrows and re-displays the currently displayed series for
// mode (see recastForMode's own doc for the no-op cases), coalescing with any
// recast already in flight: if one is running, mode is recorded as the
// latest request and this call returns immediately — relying on the running
// call to perform exactly one more pass for whatever mode is newest once it
// finishes, then check again. A single burst of N calls arriving while one
// recast runs therefore performs at most two real re-displays (the first
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
// device and re-displays it, re-entering Process for CMD_DISPLAY_PLAYLIST
// exactly as CMD_RECENTLY_PLAYED_REPLAY already does to replay a stored
// record (handler.go) — an established pattern for feeding an internally
// rebuilt dp1_call back through the whole normal display request pipeline (policy
// filter, preflight, scheduler) rather than re-implementing any of it here.
//
// A no-op when nothing is currently tracked as displaying a series — which
// is the honest answer, not a missing re-fetch: once a series is narrowed
// and display, the player's playlist carries no seriesRegistry marker (AC3), so
// there is no way to learn "a series is on screen" again except from THIS
// cache. A cache that was never populated this boot (process just started,
// or the current display never went through a static dp1_call with series
// groups) has no series identity to re-fetch editions FOR.
//
// Also a no-op while the tracked series display request is still unaccepted (its
// preflight or player send is pending) or was never accepted: only a series
// the player took is on screen to re-narrow. The accepted residual is a
// setting change that lands while a series display request is still pending: that display request
// shows the mode it resolved with, and the new mode applies from the next
// display request or setting change.
func (h *handler) recastForMode(ctx context.Context, device *dp1.SeriesPlaybackMode) {
	h.lastSeriesCast.mu.Lock()
	if !h.lastSeriesCast.valid || !h.lastSeriesCast.accepted {
		h.lastSeriesCast.mu.Unlock()
		return
	}
	shell := h.lastSeriesCast.shell
	plan := h.lastSeriesCast.plan
	contentContext := h.lastSeriesCast.contentContext
	source := h.lastSeriesCast.source
	seqBefore := h.lastSeriesCast.seq
	h.lastSeriesCast.mu.Unlock()

	rebuilt := shell
	rebuilt.Items = plan.Narrow(device)
	// The source document's signature covered a different item set; this
	// rebuild is controld's own, same as CMD_RECENTLY_PLAYED_REPLAY's.
	rebuilt.Signatures = nil
	rebuilt.Signature = ""
	// shell is the exact *Playlist a dynamic-origin display's
	// ProcessPlaylistURLForCast/ProcessDynamicPlaylistForCast returned, so
	// it still carries the ORIGINAL DynamicQuery/DynamicQueries — rebuilt
	// here holds the already-resolved, already-narrowed Items instead.
	// Left alone, MarshalJSON would still emit dynamicQuery(.pagination),
	// so re-entering Process below would see HasDynamicContent() true and
	// re-run the whole dynamic resolution (a fresh network round trip) on
	// every setSeriesPlayback call, instead of the free, local re-narrow
	// this cache exists to provide (run-reviewer F1 on #454, 2026-11-10).
	// A static-origin display's shell never has these set, so this is a no-op
	// for it.
	rebuilt.DynamicQuery = nil
	rebuilt.DynamicQueries = nil
	rebuilt.DynamicQueryPagination = nil

	b, err := h.json.Marshal(rebuilt)
	if err != nil {
		h.logger.Error("series playback: failed to rebuild the displayed playlist for re-display", zap.Error(err))
		return
	}
	var dp1Call map[string]interface{}
	if err := h.json.Unmarshal(b, &dp1Call); err != nil {
		h.logger.Error("series playback: failed to re-decode the rebuilt playlist for re-display", zap.Error(err))
		return
	}

	recastArgs := map[string]interface{}{"dp1_call": dp1Call}
	if contentContext != "" {
		recastArgs["contentContext"] = string(contentContext)
	}
	// Carries the ORIGINAL display's scheduler source through this rebuilt
	// dp1_call, which Process below classifies as static (no dynamic
	// fields survive, see above) and would otherwise prepare with a
	// zero-value Source — silently resetting the scheduler's durable
	// playlistUrl/inline-dynamic source on every setSeriesPlayback call
	// (feralfile-bot F1 on PR #454, 2026-11-10). See
	// recastSchedulerSourceHint's doc for why this cannot be forged by an
	// external caller.
	if !source.IsZero() {
		recastArgs[recastSchedulerSourceHint] = source
	}
	procResult, procErr := h.Process(ctx, commands.Command{Type: commands.CMD_DISPLAY_PLAYLIST, Arguments: recastArgs})

	// Candidate picks for the push below, computed now (pure, no shared
	// state) but not yet tagged with a Seq or sent: pushed only when the
	// re-display was actually ACCEPTED by the player — the same two-part
	// check handler.go's own defer uses, not procErr == nil alone: Process
	// can legitimately return (result, nil) with result's own ok field
	// false (a designed player refusal — e.g. handler_pending,
	// no_artwork — carries no Go error), which an earlier, procErr-only
	// check let through as if the re-display had displayed (run-reviewer
	// consolidated F2 on this fix, 2026-11-16). The re-entrant Process
	// call above runs its own copy of this same push (handler.go), but on
	// an empty CastPlan — rebuilt carries no seriesRegistry marker, so
	// that call's own ResolveSeriesCastPlan resolves no groups to push
	// picks for — so this is the only place a setSeriesPlayback-driven
	// re-narrow's own random pick (feral-file/ffos-user#459, F6 reopened)
	// is ever recorded.
	//
	// rebuilt.Items is plan.Narrow's own UNFILTERED output; the re-entrant
	// Process call above applies its own content-policy filtering to it
	// internally, and this function has no visibility into whether a
	// candidate pick survived that (run-reviewer consolidated F1 on pass 1
	// of this fix, 2026-11-16 — handler.go's own push re-checks against
	// its final, post-filter playlist.Items directly, which this call
	// site cannot see). Each candidate's own edition is instead
	// re-checked here against a lock-free content-policy snapshot — the
	// same established read narrowStable's own displayFilter uses — and
	// dropped if the policy would withhold it. Accepted, narrower residual
	// risk: this does not also re-check schedule (displayAt) exclusion the
	// way handler.go's own membership check does, since recovering that
	// visibility here would need the same cross-call plumbing threading
	// Process's final item list back to this caller, which this fix does
	// not build (scope: fix the push's own correctness, not add a new
	// Process-return surface).
	var picks []dp1.RandomGroupPick
	if procErr == nil && playerresponse.OK(procResult) && h.acceptedPickRecorder != nil {
		candidates := plan.RandomPicksFromNarrowed(device, rebuilt.Items)
		picks = survivingContentPolicyPicks(h.contentPolicy, contentContext, &rebuilt, candidates)
	}

	// The re-entrant Process call's own dp1_call branch just ran
	// rememberSeriesCast on the rebuilt (already-narrowed) document, which
	// carries no seriesRegistry marker and so cleared this cache. Restore
	// the FULL group data so a LATER setting change can still re-narrow
	// without a catalog call — but only if nothing else wrote the cache in
	// between: seq advances by exactly 1 for that re-entrant write alone: a
	// concurrent displayPlaylist landing in the same window advances it
	// further, and that request's own cache entry must stand, not this one's.
	// Restored as accepted whether or not the re-display succeeded: the plan
	// was accepted when captured above, and a failed re-display leaves that
	// same series on screen.
	//
	// picks is tagged with seqBefore+1 under this SAME check, not before
	// it: that value is only known to be this re-entrant call's own real
	// seq when nothing else wrote lastSeriesCast.seq in the meantime —
	// exactly what this check already verifies for the restore above.
	// Tagging it unconditionally, before this check, let an untagged
	// guess collide with (or appear no older than) a genuinely newer,
	// independent display's own real Seq for the same group — silently
	// defeating the Seq ordering guard this fix's pass 1 added (F3) in
	// precisely the race it was meant to close (run-reviewer consolidated
	// F1 on pass 2 of this fix, 2026-11-16). Nil'd out instead of pushed
	// when the check fails: the narrowStable live-display self-heal this
	// whole fix exists to bypass only in the withheld-pick case is still
	// there to catch an ordinary one.
	h.lastSeriesCast.mu.Lock()
	if h.lastSeriesCast.seq == seqBefore+1 {
		h.lastSeriesCast.valid = true
		h.lastSeriesCast.accepted = true
		h.lastSeriesCast.shell = shell
		h.lastSeriesCast.plan = plan
		h.lastSeriesCast.contentContext = contentContext
		h.lastSeriesCast.source = source
		for i := range picks {
			picks[i].Seq = seqBefore + 1
		}
	} else {
		picks = nil
	}
	h.lastSeriesCast.mu.Unlock()

	if len(picks) > 0 {
		h.acceptedPickRecorder(picks)
	}

	if procErr != nil {
		h.logger.Warn("series playback: re-display after a setting change failed", zap.Error(procErr))
	}
}

// survivingContentPolicyPicks keeps only the candidates whose own edition
// policy (a lock-free Snapshot, matching playlist-refresher's own
// established read — see narrowStable's displayFilter) still admits under
// context. This is recastForMode's own, narrower stand-in for handler.go's
// post-filter membership check against the final playlist.Items: it has no
// equivalent visibility into what the re-entrant Process call's filtering
// actually kept, so it predicts content-policy admission directly instead
// (see recastForMode's own doc on the residual, accepted schedule-exclusion
// gap this leaves). nil policy means nothing is filtered — matching
// Process's own filterUnderPolicy guard — so every candidate survives.
func survivingContentPolicyPicks(policy *contentpolicy.Store, context contentpolicy.Context, playlist *dp1.Playlist, candidates []dp1.RandomGroupPick) []dp1.RandomGroupPick {
	if policy == nil || len(candidates) == 0 {
		return candidates
	}
	snap := policy.Snapshot()
	survived := make([]dp1.RandomGroupPick, 0, len(candidates))
	for _, pick := range candidates {
		item, found := findPlaylistItemBySource(playlist, pick.Source)
		if !found || snap.Allows(item, context) {
			survived = append(survived, pick)
		}
	}
	return survived
}

// survivingRandomPicks keeps only the candidates whose Source is present in
// final — the playlist items as they actually stood at the moment a display request
// was accepted, after every content-policy and scheduler filtering stage
// (handler.go's own push reads this fresh from playlist.Items at defer
// time). candidates must come from RandomPicksFromNarrowed's own, PRE-
// filter lockstep walk; checking by content (Source membership) here,
// rather than re-running that walk against final, is required because
// filtering can remove items from the middle of a segment's own
// contribution, which would silently desync a second lockstep pass over
// the already-filtered list (run-reviewer consolidated F1 on this fix,
// 2026-11-16).
func survivingRandomPicks(candidates []dp1.RandomGroupPick, final []dp1playlist.PlaylistItem) []dp1.RandomGroupPick {
	if len(candidates) == 0 {
		return nil
	}
	present := make(map[string]struct{}, len(final))
	for _, item := range final {
		present[item.Source] = struct{}{}
	}
	survived := make([]dp1.RandomGroupPick, 0, len(candidates))
	for _, pick := range candidates {
		if _, ok := present[pick.Source]; ok {
			survived = append(survived, pick)
		}
	}
	return survived
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
