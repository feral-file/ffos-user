package commandrouter

import (
	"context"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mediahealth"
)

// MediaHealth is the router's view of mediahealth.Service
// (feral-file/ffos-user#392): the cast-time L0 pass, the per-item hint
// field, and the getMediaHealth report. Consumer-owned so the router never
// depends on the service's construction; nil (tests, a build with the
// feature disabled) skips the pass entirely — fail-open, the pre-#304
// accept-anything behavior — and sends no hints.
type MediaHealth interface {
	// Preflight probes sources (bounded) and records every verdict; one
	// result per source in input order. See mediahealth.Prober.Preflight.
	Preflight(ctx context.Context, sources []string) []mediahealth.ProbeResult
	// HintArgument returns the value for the displayPlaylist request's
	// `mediaHealth` field for items — the conclusively broken ones — or nil
	// when there are none or the installed player does not declare the
	// contract.
	HintArgument(items []dp1playlist.PlaylistItem) interface{}
	// Report builds the getMediaHealth rows for the items on screen, with
	// the player's own report of the indices it is skipping.
	Report(items []dp1playlist.PlaylistItem, skipped []int) ([]mediahealth.ReportItem, error)
}

// SetMediaHealth injects the media-health service onto h, if h supports it
// (the concrete *handler built by New — NOT the storm-protection gate
// wrapper, so callers must wire it against the raw handler before NewGate
// wraps it, mirroring SetSessionGeneration's contract).
func SetMediaHealth(h Handler, svc MediaHealth, logger *zap.Logger) {
	setter, ok := h.(interface{ setMediaHealth(MediaHealth) })
	if !ok {
		logger.Warn("Command handler does not support media health wiring")
		return
	}
	setter.setMediaHealth(svc)
}

func (h *handler) setMediaHealth(svc MediaHealth) {
	h.mediaHealth = svc
}

// attachMediaHealthHints sets args["mediaHealth"] for the playlist about to
// be sent, or removes it. Called immediately before each displayPlaylist
// send, on the FINAL item list (after the content-policy projection and the
// scheduler's displayAt filtering), so the hints describe exactly the items
// the player receives. The field is controld-owned: any value a controller
// sent was dropped at the top of Process.
func (h *handler) attachMediaHealthHints(args map[string]interface{}, playlist *dp1.Playlist) {
	delete(args, "mediaHealth")
	if h.mediaHealth == nil || playlist == nil {
		return
	}
	if hints := h.mediaHealth.HintArgument(playlist.Items); hints != nil {
		args["mediaHealth"] = hints
	}
}

// maxMediaHealthReportItems bounds the getMediaHealth reply the same way
// getRecentlyPlayed is bounded: the unauthenticated LAN API must not be able
// to mint a reply proportional to a hostile cast's size. Real playlists are
// an order of magnitude smaller.
const maxMediaHealthReportItems = 2048

// handleGetMediaHealth answers getMediaHealth: one verdict per item of the
// playlist on screen, read from the player's own item list so the rows line
// up with what the app already shows. No source URLs leave the daemon.
func (h *handler) handleGetMediaHealth(ctx context.Context, command commands.Command) (interface{}, error) {
	// Takes no arguments, and a non-empty request is rejected rather than
	// ignored, for the same storm-gate dedupe reason getRecentlyPlayed
	// rejects one (gate.go).
	if len(command.Arguments) != 0 {
		return map[string]interface{}{"ok": false, "status": "error", "error": "getMediaHealth takes no arguments"}, nil
	}
	if h.mediaHealth == nil {
		return map[string]interface{}{"ok": false, "status": "error", "error": "mediaHealthUnavailable"}, nil
	}
	playerStatus, err := h.statusPoller.FetchPlayerStatus(ctx)
	if err != nil {
		return nil, err
	}
	if playerStatus == nil || playerStatus.Items == nil {
		return map[string]interface{}{"ok": true, "status": "empty", "items": []interface{}{}, "total": 0}, nil
	}
	items := *playerStatus.Items
	total := len(items)
	truncated := false
	if len(items) > maxMediaHealthReportItems {
		items = items[:maxMediaHealthReportItems]
		truncated = true
	}
	rows, err := h.mediaHealth.Report(items, playerStatus.SkippedIndices)
	if err != nil {
		return nil, err
	}
	reply := map[string]interface{}{
		"ok":        true,
		"status":    "ok",
		"items":     rows,
		"total":     total,
		"truncated": truncated,
	}
	if playerStatus.Outcomes != nil {
		reply["castSeq"] = playerStatus.Outcomes.CastSeq
	}
	return reply, nil
}

// replyKeyOutcomes is the player's raw outcome ring on a checkStatus reply;
// replyKeySkipped its raw report of hint-skipped indices. Both are inputs
// to the media observation store, never a controller surface.
const (
	replyKeyOutcomes = "outcomes"
	replyKeySkipped  = "skippedIndices"
)

// stripOutcomesReply removes the raw outcome ring from a direct checkStatus
// reply, in both shapes the player answers (bare, and enveloped under
// `message`). The ring carries item sources — presigned CDN URLs included —
// and play history across casts; it is an input to the observation store
// (the status poller reads it on its own CDP path), never a controller
// surface. The poller's notification mapper already drops it; this is the
// same boundary for the direct reply (PR #438 review, round 4, F16).
func stripOutcomesReply(result interface{}) {
	m, ok := result.(map[string]interface{})
	if !ok {
		return
	}
	delete(m, replyKeyOutcomes)
	delete(m, replyKeySkipped)
	if msg, ok := m["message"].(map[string]interface{}); ok {
		delete(msg, replyKeyOutcomes)
		delete(msg, replyKeySkipped)
	}
}
