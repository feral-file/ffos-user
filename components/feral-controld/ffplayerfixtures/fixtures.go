// Package ffplayerfixtures holds LITERAL wire-payload strings the shipped
// ff-player produces, copied verbatim (not reconstructed) from its source.
// Every constant's doc comment names the exact ff-player file:lines it was
// copied from, AS OF the commit checked out at /Users/yehboyang/ff-player
// when this package was written.
//
// This is the tracked, executable half of the controld<->ff-player wire
// contract described in docs/player-session-recovery.md. Hand-built mocks
// in each package's own tests exercise the SHAPE of the contract; these
// fixtures exercise the ACTUAL BYTES a real player produces, so a drift on
// either side of the contract fails a test instead of passing both sides'
// independently-mocked suites.
//
// MAINTENANCE: whenever the cited ff-player source changes in a way that
// changes what it sends over the wire (a renamed field, a new refusal
// reason, a different envelope shape, a bumped protocol/contract version),
// update the corresponding constant here to match — literally, not just in
// spirit — and re-run `go test ./...` in this module to see every consumer
// that needs a matching update. A stale fixture that silently drifts from
// the real player is worse than no fixture at all: it certifies a contract
// ff-player no longer honors.
package ffplayerfixtures

// --- refreshArtwork refusal replies -----------------------------------------
//
// Envelope: CDPRequestHandler.ts's handleCommandRequest default case
// (ff-player src/services/cdp-handler/CDPRequestHandler.ts:157-164) wraps
// every CanvasService.processMessage() result as
// `{messageID, message: responseMessage}` and JSON.stringify's the whole
// thing (line 167) as handleCDPRequest's return value. The refreshArtwork
// command routes to CanvasService.refreshArtwork() (CanvasService.ts:650-651,
// method body at :929-978).

const (
	// RefusalHandlerPending: no playlist handler registered yet.
	// Source: ff-player src/services/CanvasService.ts:951-955.
	RefusalHandlerPending = `{"messageID":"1","message":{"ok":false,"error":"No playlist handler registered yet","code":"handler_pending"}}`

	// RefusalNoArtwork: nothing active to refresh.
	// Source: ff-player src/services/CanvasService.ts:939-943.
	RefusalNoArtwork = `{"messageID":"1","message":{"ok":false,"error":"No active artwork to refresh","code":"no_artwork"}}`

	// RefusalPreviewUpdateFailed: a registered handler that could not update
	// the preview URL.
	// Source: ff-player src/services/CanvasService.ts:970-974.
	RefusalPreviewUpdateFailed = `{"messageID":"1","message":{"ok":false,"error":"Playlist handler could not update preview URL","code":"preview_update_failed"}}`

	// RefusalOldPlayerBareNotOK models a player build that predates this
	// contract entirely: a bare {ok:false} with neither `error` nor `code`.
	// Not copied from a specific ff-player source line — the whole point is
	// that this shape is what the ABSENCE of the current
	// error-string/code contract looks like on the wire (the shape
	// origin/main sent before the cross-repo recovery redesign landed).
	// playerresponse.Refusal must still recognize this as a genuine,
	// unclassifiable refusal (code == "", ok == true), never mistake it for
	// a non-refusal.
	RefusalOldPlayerBareNotOK = `{"messageID":"1","message":{"ok":false}}`
)

// --- window.__ffosPlayerStatus payloads -------------------------------------
//
// Shape and installation: ff-player
// src/services/cdp-handler/CDPRequestHandler.ts:57-64 —
//
//	(window as any).__ffosPlayerStatus = () =>
//	  JSON.stringify({
//	    protocol: 1,
//	    route: window.location.pathname,
//	    handlerRegistered: canvasService.onRefreshArtwork !== null,
//	    hasArtwork: canvasService.hasActiveArtwork(),
//	    bootHydration: canvasService.bootHydrationState(),
//	  });
//
// bootHydration's five values are enumerated at
// ff-player src/services/CanvasService.ts:825 (bootHydrationState doc) and
// its implementation immediately below. One fixture per value, with
// route/handlerRegistered/hasArtwork combinations representative of when
// each value actually occurs in practice (not just structurally valid).

const (
	// PlayerStatusBootHydrationPending: still hydrating; handler/artwork not
	// yet meaningful.
	PlayerStatusBootHydrationPending = `{"protocol":1,"route":"/playlist","handlerRegistered":true,"hasArtwork":false,"bootHydration":"pending"}`

	// PlayerStatusBootHydrationOK: hydration completed normally with
	// something cast.
	PlayerStatusBootHydrationOK = `{"protocol":1,"route":"/playlist","handlerRegistered":true,"hasArtwork":true,"bootHydration":"ok"}`

	// PlayerStatusBootHydrationHaltedCleared: a disconnect cleared the cast
	// mid-boot; the wall is intentionally dark.
	PlayerStatusBootHydrationHaltedCleared = `{"protocol":1,"route":"/playlist","handlerRegistered":true,"hasArtwork":false,"bootHydration":"halted_cleared"}`

	// PlayerStatusBootHydrationHaltedPreserving: a sleep/error navigation
	// interrupted hydration; the playlist is preserved and restores on
	// wake/reboot. route reflects the sleep navigation that halted it.
	PlayerStatusBootHydrationHaltedPreserving = `{"protocol":1,"route":"/sleep","handlerRegistered":true,"hasArtwork":true,"bootHydration":"halted_preserving"}`

	// PlayerStatusBootHydrationFailed: initCastInfo threw; the page is
	// wedged. handlerRegistered false is representative (a hydration
	// failure this early typically means the handler never installed
	// either), though the classifier does not depend on that combination.
	PlayerStatusBootHydrationFailed = `{"protocol":1,"route":"/playlist","handlerRegistered":false,"hasArtwork":false,"bootHydration":"failed"}`

	// PlayerStatusRouteError: the daemon-chosen error page owns the wall.
	// bootHydration is "ok" here deliberately — the error page can be
	// reached from a fully-hydrated state (ErrorNavigation), not only mid-boot.
	PlayerStatusRouteError = `{"protocol":1,"route":"/error","handlerRegistered":true,"hasArtwork":true,"bootHydration":"ok"}`

	// PlayerStatusProtocolMismatch models a hypothetical future protocol
	// version this build of controld does not understand. Every field past
	// `protocol` and `route` must be treated as undecodable — route ITSELF
	// must still gate the error-page safety check (see
	// docs/player-session-recovery.md §3.1, §5.2).
	PlayerStatusProtocolMismatchRouteError = `{"protocol":2,"route":"/error","handlerRegistered":true,"hasArtwork":true,"bootHydration":"ok"}`
)

// --- checkStatus replies (the stamp transport) ------------------------------
//
// Envelope: same {messageID, message: ...} wrapping as the refusal replies
// above. message shape: ff-player src/services/CanvasService.ts:700-742
// (CanvasService.getStatus(), the checkStatus command handler — dispatch at
// :634-635); the `stamp` field's contract is documented at
// ff-player src/models/cast_request_reply.model.ts:68-75 and
// CanvasService.ts:710-720. A player carrying this feature ALWAYS includes
// the `stamp` key — "" when the session hasn't stamped the document yet,
// the stamp string once it has. Only a player that predates the stamp
// contract entirely omits the key.
//
// Every real getStatus reply also carries deviceSettings/isPaused/sleepMode/
// loopMode/shuffle (CanvasService.ts:722-741) regardless of stamp support —
// these three fixtures include them at their idle-player defaults
// (Scaling.Fit="fit", ViewMode.landscape="landscape",
// TombstoneMode.Timed="timed", LoopMode.playlist="playlist") to stay
// byte-accurate to the real reply shape, per this package's copied-verbatim
// invariant. castCommand/playlist/playlistUrl/items/defaultDuration are
// omitted because they decode from `undefined` fields on an idle player with
// nothing cast, and JSON.stringify drops undefined object properties.

const (
	// CheckStatusReplyStampPresentEmpty: a new player whose current document
	// has no stamp of its own yet (a fresh mount, or a document the session
	// never stamped — e.g. right after a feral-watchdog-driven navigation).
	CheckStatusReplyStampPresentEmpty = `{"messageID":"1","message":{"ok":true,"index":0,"stamp":"","deviceSettings":{"scaling":"fit","orientation":"landscape","tombstone":"timed"},"isPaused":false,"sleepMode":false,"loopMode":"playlist","shuffle":false}}`

	// CheckStatusReplyStampPresentValue: a new player echoing back a real
	// stamp the session previously wrote.
	CheckStatusReplyStampPresentValue = `{"messageID":"1","message":{"ok":true,"index":0,"stamp":"42-1a2b3c","deviceSettings":{"scaling":"fit","orientation":"landscape","tombstone":"timed"},"isPaused":false,"sleepMode":false,"loopMode":"playlist","shuffle":false}}`

	// CheckStatusReplyStampAbsent: an OLD player that predates the stamp
	// contract entirely — the `stamp` key is not present at all, not merely
	// empty. This is the ONLY shape that must classify as "source
	// unavailable" (present == false); both stamp variants above must
	// classify as present == true regardless of the string value. The
	// sibling deviceSettings/isPaused/sleepMode/loopMode/shuffle fields
	// predate the stamp contract by a wide margin, so an old player still
	// carries them; only `stamp` itself is missing.
	CheckStatusReplyStampAbsent = `{"messageID":"1","message":{"ok":true,"index":0,"deviceSettings":{"scaling":"fit","orientation":"landscape","tombstone":"timed"},"isPaused":false,"sleepMode":false,"loopMode":"playlist","shuffle":false}}`
)
