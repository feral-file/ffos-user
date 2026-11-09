package mediahealth

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
	return r.Plays.ConsecutiveFail > 0 && f != nil && f.Stage == StageDetect && f.Code != CodeNetwork
}
