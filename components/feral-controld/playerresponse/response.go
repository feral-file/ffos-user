package playerresponse

// OK checks whether the CDP result from the player reports ok:true. Most
// player commands return { "message": { "ok": true } }; a few older paths
// return top-level { "ok": true }.
func OK(result interface{}) bool {
	m, ok := result.(map[string]interface{})
	if !ok {
		return false
	}
	if msg, ok := m["message"].(map[string]interface{}); ok {
		okVal, _ := msg["ok"].(bool)
		return okVal
	}
	okVal, _ := m["ok"].(bool)
	return okVal
}

// Refusal codes (cross-repo recovery design §4.2), mirroring ff-player's
// CanvasService.refreshArtwork `code` field byte-for-byte.
const (
	CodeHandlerPending      = "handler_pending"
	CodeNoArtwork           = "no_artwork"
	CodePreviewUpdateFailed = "preview_update_failed"
)

// [minor #18] These are NOT legacy/pre-`code` strings — that premise was
// wrong: origin/main's refreshArtwork refusal sent a bare {ok:false} with no
// `error` reason string at all, and CanvasService.refreshArtwork started
// shipping BOTH the `error` reason string and the `code` field together, in
// the SAME release. There was never a wire format that carried one without
// the other. Matching on the exact reason string here is same-release
// redundancy / defense-in-depth (a second, independent signal against the
// primary `code` field), not backward compatibility with an older player
// generation. A reply carrying `code` is matched on that field directly and
// never touches these; this fallback only matters if `code` is ever absent
// or empty on an otherwise-classifiable refusal.
const (
	reasonHandlerPending      = "No playlist handler registered yet"
	reasonNoArtwork           = "No active artwork to refresh"
	reasonPreviewUpdateFailed = "Playlist handler could not update preview URL"
)

// Refusal extracts a refused player response's reason text and classified
// code (design doc §3.2's classification table, §4.2). This is the single
// place that boundary-classifies a player refusal — every consumer (boot
// recovery's evaluateRefreshArtwork, and any future caller) must go through
// here so the reason-string fallback never has to be reimplemented
// elsewhere.
//
// ok is false when result is not a refusal shape OK recognizes as ok:false
// (including a bare ok:true reply, which is not a refusal at all — callers
// check OK/err separately). When ok is true, code is the machine-readable
// classification: the reply's own `message.code` field when present,
// otherwise a fallback match against the exact `error` reason string (see
// the reason* constants' doc for why this is same-release redundancy, not a
// backward-compatibility path). code is "" when the refusal is genuine but
// unclassifiable (an unrecognized reason, or no reason at all) — design doc
// §3.2 rows 15/16 ("bare non-ACK / unknown code").
func Refusal(result interface{}) (reason string, code string, ok bool) {
	m, isMap := result.(map[string]interface{})
	if !isMap {
		return "", "", false
	}
	target := m
	if msg, hasMsg := m["message"].(map[string]interface{}); hasMsg {
		target = msg
	}
	if okVal, _ := target["ok"].(bool); okVal {
		return "", "", false
	}
	reason, _ = target["error"].(string)
	if c, _ := target["code"].(string); c != "" {
		return reason, c, true
	}
	switch reason {
	case reasonHandlerPending:
		return reason, CodeHandlerPending, true
	case reasonNoArtwork:
		return reason, CodeNoArtwork, true
	case reasonPreviewUpdateFailed:
		return reason, CodePreviewUpdateFailed, true
	default:
		return reason, "", true
	}
}
