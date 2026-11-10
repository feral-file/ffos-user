package mediahealth

import "time"

// Two layers of evidence, each answering its own question (feral-file/
// ffos-user#392, "L1 as play evidence"):
//
//   - fetch: did the document arrive? The player's content-type check
//     (HEAD), the kiosk's own document status, L0's bytes.
//   - render: did it paint? A frame of the work (the nightly L1 render, a
//     capture of the wall) and the player's load, decode and watchdog
//     stages.
//
// A success in one layer never heals a failure in the other: an error page
// can look like a rendered frame, but a rendered artwork can never produce
// a 404. The layer of a failure is read from its REASON, not its category —
// a known-bad frame (a gateway error page served with a 200) is reported
// under not_media yet is render evidence.

// renderLayerReason reports whether reason is a render-layer failure.
// render_detect is the player's HEAD: fetch layer.
func renderLayerReason(reason Reason) bool {
	switch reason {
	case ReasonRenderLoad, ReasonRenderDecode, ReasonRenderWatchdog, ReasonRenderExhausted,
		ReasonRenderBlank, ReasonRenderKnownBad:
		return true
	}
	return false
}

// renderLayerBroken reports a broken verdict concluded from render
// evidence (a play or a frame): the only broken a frame of the work may
// heal. L0's verdicts and the fetch-layer play verdicts are never healed
// by a frame.
func (r *Record) renderLayerBroken() bool {
	return r.State == StateBroken && (r.Layer == LayerPlay || r.Layer == LayerL1) && renderLayerReason(r.Reason)
}

// fetchRunOpen reports an open failure run whose last counted failure is a
// fetch-layer one: the player's content-type check refused the document
// (401/403/429/4xx), not a device-local network miss. A frame that rendered
// answers the render question only — it must not end that run or clear its
// play_failed issue, or a work refused on every play but rendering nightly
// would never conclude (the PENDING #37 pattern).
func (r *Record) fetchRunOpen() bool {
	f := r.Plays.LastFail
	return r.Plays.ConsecutiveFail > 0 && f != nil && f.Stage == StageDetect && f.Code != CodeNetwork && !f.Superseded
}

// renderedFrame applies a frame of the work that rendered — the nightly L1
// render or a capture of the wall — and reports whether the record took it.
// It answers the render question only: it records ok (layer is l1 for the
// nightly render, play for the wall) over unknown, an ok, a suspect or a
// render-layer broken, never over an L0 or fetch-layer broken (a frame can
// be the gateway's error page); and it ends the failure run only when that
// run is a render one (fetchRunOpen).
//
// When the open fetch run's last failure is a definite absence (the
// content-type check got 400/404/410), the frame changes nothing: a
// rendered artwork can never produce a 404, an error page can look like a
// rendered frame — the HEAD wins.
func (r *Record) renderedFrame(at time.Time, layer Layer) bool {
	if r.State == StateBroken && !r.renderLayerBroken() {
		return false
	}
	if r.fetchRunOpen() && definiteAbsence(r.Plays.LastFail.Code) {
		return false
	}
	r.State = StateOK
	r.Layer = layer
	r.Category, r.Reason = "", ""
	r.CheckedAt = at
	r.ConsecutiveBroken = 0
	if r.L1 != nil {
		// The work rendered: an L1 blank pending confirmation, or a
		// negative from before, is history. A later headless blank earns
		// a fresh sequence instead of being corroborated by a negative
		// this frame overruled (PR #450 review, F4).
		r.L1.ConsecutiveBlank = 0
		r.L1.NegativeAt = time.Time{}
	}
	if !r.fetchRunOpen() {
		r.Plays.ConsecutiveFail = 0
		if r.Issue == IssuePlayFailed {
			r.clearIssue()
		}
	}
	return true
}

// frameRunOpen reports an open failure run whose last counted failure is a
// frame of the wall (blank or known-bad). An opaque embed's `ready` reports
// a blank frame as ready too, so it must not end such a run — otherwise
// every play's `ready` would reset the run before that play's blank frame
// arrived, and a blank work could never conclude.
func (r *Record) frameRunOpen() bool {
	f := r.Plays.LastFail
	return r.Plays.ConsecutiveFail > 0 && f != nil && f.Stage == StageFrame
}

// definiteAbsence reports a content-type check status that says the
// document is not there for anyone: 400, 404, 410 — L0's dead statuses.
// 401/403/429 are about the asker's identity or rate, not the document.
func definiteAbsence(code string) bool {
	switch code {
	case "400", "404", "410":
		return true
	}
	return false
}

// l1NightGap separates two L1 negatives that corroborate each other: a blank
// and its same-night confirmation (seconds apart, identical conditions) are
// one observation; two conclusions this far apart are different nights.
// Robust across midnight, unlike a calendar date.
const l1NightGap = 12 * time.Hour

// l1Negative reports a standing L1 negative on the record (suspect or broken
// from a blank or known-bad render): independent of the wall's frame, so the
// two corroborate each other.
func (r *Record) l1Negative() bool {
	return r.Layer == LayerL1 && (r.State == StateSuspect || r.State == StateBroken) && renderLayerReason(r.Reason)
}

// concludeRender writes a render negative (blank or known-bad, from L1 or
// the wall). broken requires both render verdicts being conclusive and the
// negative corroborated (the caller decides); otherwise it is suspect —
// advisory, never a hint. A broken render verdict gets the ordinary broken
// window (and its backoff), so it is hinted and counted toward rejecting
// an all-broken cast like any other. The gateway route is kept: a render
// verdict says nothing about which gateway serves the bytes, and the work
// is retried at the window's end.
func (r *Record) concludeRender(reason Reason, layer Layer, at time.Time, broken bool) {
	r.clearIssue()
	if !broken {
		r.State = StateSuspect
		r.Layer = layer
		r.Reason = reason
		r.Category = CategoryOf(reason, 0)
		r.CheckedAt = at
		return
	}
	switch {
	case r.State == StateBroken && r.Layer == layer && !at.Before(r.RecheckAfter):
		r.ConsecutiveBroken++
	case r.State == StateBroken && r.Layer == layer:
		// Inside the window: keep the count.
	default:
		r.ConsecutiveBroken = 1
	}
	r.State = StateBroken
	r.Layer = layer
	r.Reason = reason
	r.Category = CategoryOf(reason, 0)
	r.HTTPStatus = 0
	r.CheckedAt = at
	r.RecheckAfter = at.Add(brokenRecheck(r.ConsecutiveBroken))
	r.LastError = "render: " + string(reason)
	r.LastErrorAt = &at
}

// headGetWindow bounds one play's content-type check (the player's HEAD)
// and the kiosk's own GET of the same document: the iframe fallback loads
// right after the check, and the embedded load watchdog is 45 s.
const headGetWindow = 60 * time.Second

// revertHeadRefusal undoes a counted content-type check refusal that the
// kiosk's own GET of the same document overruled: the observer saw it
// served (2xx) within headGetWindow of the HEAD (player entries carry an
// attempt; an observed failure, attempt 0, is the GET itself and never
// reverted). The HEAD is the weakest fetch evidence — a different method,
// sometimes a different answer (405, a 404 on HEAD and a 200 on GET) — so
// the real GET outranks it. The count is reverted, the issue ends with the
// run, and a verdict this very failure concluded is withdrawn: its
// evidence was the HEAD alone. Reports whether anything changed.
func (r *Record) revertHeadRefusal(now time.Time) bool {
	f := r.Plays.LastFail
	if !r.fetchRunOpen() || f.Attempt == 0 || now.Before(f.At) || now.Sub(f.At) > headGetWindow {
		return false
	}
	if r.State == StateBroken && r.Layer == LayerPlay && r.Reason == ReasonRenderDetect && r.CheckedAt.Equal(f.At) {
		r.State, r.Layer = "", ""
		r.Category, r.Reason = "", ""
		r.HTTPStatus = 0
		r.RecheckAfter = now
		r.LastError, r.LastErrorAt = "", nil
	}
	r.Plays.ConsecutiveFail--
	if r.Plays.Fail > 0 {
		r.Plays.Fail--
	}
	f.Superseded = true
	switch {
	case r.Plays.ConsecutiveFail == 0 && r.Issue == IssuePlayFailed:
		r.clearIssue()
	case r.Plays.ConsecutiveFail > 0 && r.State == "":
		// A withdrawn verdict leaves the run's earlier counted failure:
		// back to one failure short of a verdict, reported as such.
		r.setIssue(IssuePlayFailed, now)
	}
	return true
}
