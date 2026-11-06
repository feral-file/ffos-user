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

// maxMediaHealthReportItems bounds the EMPTY-request getMediaHealth reply
// (the pre-paging form, kept as is for the app that exists). A larger wall
// is read page by page (mediaHealthQuery); the paged form is bounded by
// its limit instead.
const maxMediaHealthReportItems = 2048

// Paging bounds for getMediaHealth (#392, large walls).
const (
	mediaHealthPageDefault = 500
	mediaHealthPageMax     = 1000
)

// mediaHealthQuery is a parsed getMediaHealth request. paged is false for
// the empty request, whose reply shape is unchanged.
type mediaHealthQuery struct {
	paged     bool
	offset    int
	limit     int
	attention bool
}

// parseMediaHealthQuery accepts exactly {offset?, limit?, filter?} with
// in-range values and rejects everything else. Strict on purpose: the
// storm gate dedupes query-class commands on type + arguments, so the
// argument space must stay bounded — valid pages are bounded by the
// playlist's size, not chosen by the caller.
func parseMediaHealthQuery(args map[string]interface{}) (mediaHealthQuery, bool) {
	q := mediaHealthQuery{}
	if len(args) == 0 {
		return q, true
	}
	q.paged = true
	q.limit = mediaHealthPageDefault
	for key, raw := range args {
		switch key {
		case "offset", "limit":
			f, ok := raw.(float64)
			if !ok || f != float64(int(f)) {
				return q, false
			}
			v := int(f)
			if key == "offset" {
				if v < 0 {
					return q, false
				}
				q.offset = v
			} else {
				if v < 1 || v > mediaHealthPageMax {
					return q, false
				}
				q.limit = v
			}
		case "filter":
			switch raw {
			case "all":
			case "attention":
				q.attention = true
			default:
				return q, false
			}
		default:
			return q, false
		}
	}
	return q, true
}

// handleGetMediaHealth answers getMediaHealth: one verdict per item of the
// playlist on screen, read from the player's own item list so the rows line
// up with what the app already shows. No source URLs leave the daemon.
//
// Two forms. The EMPTY request is the original one: the first 2048 rows
// with `truncated`. A request with any of offset/limit/filter is PAGED:
// `offset`/`limit` select rows (from the rows the filter admits, each
// still carrying its absolute playlist `index`), `matched` counts them
// across the whole wall, `nextOffset` is present while more remain, and
// `castSeq` is the consistency token — a change between pages means the
// wall changed. Anything but a well-formed request is rejected, for the
// same storm-gate dedupe reason getRecentlyPlayed rejects arguments.
func (h *handler) handleGetMediaHealth(ctx context.Context, command commands.Command) (interface{}, error) {
	query, ok := parseMediaHealthQuery(command.Arguments)
	if !ok {
		return map[string]interface{}{"ok": false, "status": "error", "error": "invalid request"}, nil
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
	if query.paged {
		return h.pagedMediaHealth(items, playerStatus.SkippedIndices, playerStatus.Outcomes, query)
	}
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

// pagedMediaHealth builds the paged getMediaHealth reply: one store read
// over the whole wall (the rows are needed to filter), then the page.
func (h *handler) pagedMediaHealth(items []dp1playlist.PlaylistItem, skipped []int, outcomes *mediahealth.Outcomes, query mediaHealthQuery) (interface{}, error) {
	rows, err := h.mediaHealth.Report(items, skipped)
	if err != nil {
		return nil, err
	}
	if query.attention {
		kept := rows[:0]
		for _, row := range rows {
			if row.NeedsAttention() {
				kept = append(kept, row)
			}
		}
		rows = kept
	}
	matched := len(rows)
	start := min(query.offset, matched)
	end := min(start+query.limit, matched)
	reply := map[string]interface{}{
		"ok":      true,
		"status":  "ok",
		"items":   rows[start:end],
		"total":   len(items),
		"matched": matched,
		"offset":  query.offset,
		"limit":   query.limit,
	}
	if end < matched {
		reply["nextOffset"] = end
	}
	if outcomes != nil {
		reply["castSeq"] = outcomes.CastSeq
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
