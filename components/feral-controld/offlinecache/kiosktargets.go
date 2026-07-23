package offlinecache

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// enableChildTargetAutoAttach turns on CDP flat-mode auto-attach on the
// kiosk's top-level page session and wires the resulting target lifecycle
// into replayer's per-target Attach/Detach.
//
// Why this exists: feral-player embeds a software artwork in an <iframe>.
// When that iframe is cross-origin from the kiosk page, Chromium's site
// isolation runs it in its own renderer process with its OWN CDP target
// (an out-of-process iframe, OOPIF). The top-level page session never sees
// that iframe's requests, so replay's Fetch interception — attached only
// to the top-level target — silently misses everything the artwork itself
// loads and it falls through to the (offline) network. Auto-attach makes
// each such child target visible as its own flat-mode session so replay
// can intercept it too.
//
// Ordering contract (the whole reason waitForDebuggerOnStart is set):
// with waitForDebuggerOnStart=true, a newly created child target is PAUSED
// before it issues even its first (document) request. On
// Target.attachedToTarget we (1) attach replay's handler + Fetch.enable to
// the child, THEN (2) call Runtime.runIfWaitingForDebugger to let it
// proceed. Without the pause, the child's first request could race ahead
// of our Fetch.enable and reach the network before interception is armed —
// reintroducing the exact "first request always misses" failure this fix
// targets, just intermittently instead of deterministically.
//
// Not recursive: this attaches only the page's direct child targets. A
// further nested cross-origin iframe (inside the artwork's own iframe)
// would need setAutoAttach reissued on that child's session; that is a
// deliberate non-goal here (matches the capture-side nested-target
// limitation documented in docs/offline-artwork-capture.md §8).
func enableChildTargetAutoAttach(
	ctx context.Context,
	root CDPSession,
	replayer Replayer,
	jsonWrapper wrapper.JSON,
	logger *zap.Logger,
) error {
	// Register the lifecycle handlers BEFORE setAutoAttach so the initial
	// burst of Target.attachedToTarget events it emits for already-present
	// iframes is not missed. Both handlers hand off to a goroutine: they
	// run on the CDP read pump, and their work Sends CDP commands (child
	// Fetch.enable / Runtime.runIfWaitingForDebugger, and Detach may take
	// transitionMu which can be held across a Send) — doing that inline
	// would deadlock the pump against the very replies it must read (see
	// CDPSession.On and replayer.transitionMu docs).
	root.On("Target.attachedToTarget", func(params json.RawMessage) {
		go handleTargetAttached(root, replayer, jsonWrapper, logger, params)
	})
	root.On("Target.detachedFromTarget", func(params json.RawMessage) {
		go handleTargetDetached(replayer, jsonWrapper, logger, params)
	})

	if _, err := root.Send(ctx, "Target.setAutoAttach", map[string]interface{}{
		"autoAttach":             true,
		"waitForDebuggerOnStart": true,
		"flatten":                true,
		// Scope to iframes only. Workers are intentionally out of scope
		// for replay (symmetric with capture's documented worker
		// limitation); attaching them would arm Fetch on targets replay
		// has no cached resources for and no reason to intercept.
		"filter": []map[string]interface{}{{"type": "iframe"}},
	}); err != nil {
		return fmt.Errorf("Target.setAutoAttach: %w", err)
	}
	return nil
}

// targetAttachedEvent is the subset of Target.attachedToTarget we use. The
// sessionId is the freshly-attached child's flat-mode routing id; the
// nested targetInfo.type lets us ignore anything that slipped past the
// iframe filter.
type targetAttachedEvent struct {
	SessionID  string `json:"sessionId"`
	TargetInfo struct {
		TargetID string `json:"targetId"`
		Type     string `json:"type"`
	} `json:"targetInfo"`
}

type targetDetachedEvent struct {
	SessionID string `json:"sessionId"`
}

// handleTargetAttached arms replay on a newly attached child target and
// then resumes it. Runs on its own goroutine (never the read pump). Once a
// routable sessionId is in hand it ALWAYS resumes the target via
// Runtime.runIfWaitingForDebugger, even when replay is not currently
// enabled — a target paused by waitForDebuggerOnStart that is never
// resumed would hang forever, freezing that iframe on screen. The only
// non-resuming paths are the early returns below (unparseable event, or an
// event with no sessionId): there is then no session to route a resume to,
// and in practice CDP always includes sessionId on this event, so no real
// target is left dangling.
func handleTargetAttached(
	root CDPSession,
	replayer Replayer,
	jsonWrapper wrapper.JSON,
	logger *zap.Logger,
	params json.RawMessage,
) {
	var evt targetAttachedEvent
	if err := jsonWrapper.Unmarshal(params, &evt); err != nil {
		logger.Warn("offline cache replay: failed to parse Target.attachedToTarget", zap.Error(err))
		return
	}
	if evt.SessionID == "" {
		// No sessionId means we cannot route commands to it; nothing we
		// can safely attach or resume.
		logger.Warn("offline cache replay: Target.attachedToTarget without sessionId, ignoring",
			zap.String("target_id", evt.TargetInfo.TargetID), zap.String("type", evt.TargetInfo.Type))
		return
	}

	child := root.ForSession(evt.SessionID)
	replayer.Attach(evt.SessionID, child)

	// Resume the paused child now that interception is armed. Use a
	// background context: this runs off the CDP event pump, decoupled
	// from any request context, and the session's own send timeout still
	// bounds it.
	if _, err := child.Send(context.Background(), "Runtime.runIfWaitingForDebugger", map[string]interface{}{}); err != nil {
		logger.Debug("offline cache replay: Runtime.runIfWaitingForDebugger on child target failed",
			zap.String("session_id", evt.SessionID), zap.Error(err))
	}
}

// handleTargetDetached drops a child target that has gone away (iframe
// navigated away or was removed). Runs on its own goroutine (never the
// read pump, since Detach may take transitionMu).
func handleTargetDetached(
	replayer Replayer,
	jsonWrapper wrapper.JSON,
	logger *zap.Logger,
	params json.RawMessage,
) {
	var evt targetDetachedEvent
	if err := jsonWrapper.Unmarshal(params, &evt); err != nil {
		logger.Warn("offline cache replay: failed to parse Target.detachedFromTarget", zap.Error(err))
		return
	}
	if evt.SessionID == "" {
		return
	}
	replayer.Detach(evt.SessionID)
}
