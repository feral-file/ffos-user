package control

import (
	"encoding/json"
	"errors"
)

// Route is the transport a PLAINTEXT command arrived on. It matters only for
// plaintext (§7.2); an envelope's authority never depends on its route.
type Route int

const (
	RouteLAN Route = iota
	RouteRelayer
)

// Decision is a plaintext-policy verdict. Code is the public error code to
// send when Allow is false (body: PublicErrorBody, LAN status:
// PublicErrorStatus).
type Decision struct {
	Allow bool
	Code  string
}

var allow = Decision{Allow: true}

func deny(code string) Decision { return Decision{Code: code} }

// Policy decides a plaintext (non-envelope) command per §7.2. Internal
// callers (OOM recovery, playRecentlyPlayed re-entry) never reach it, and
// the ff1Envelope carrier is routed to Authority.Open before it.
func Policy(state State, route Route, cmd string, req json.RawMessage) Decision {
	tier := TierOf(cmd)
	switch state {
	case StateLegacy:
		// Phase-0 rules (hub middleware) still apply; this layer adds none.
		return allow
	case StateEnrolled:
		if route == RouteLAN {
			return deny(CodeEnrollmentRequired)
		}
		if tier == TierController {
			return allow
		}
		return deny(CodeEnvelopeRequired)
	case StateEnforced:
		if route == RouteLAN {
			return deny(CodeEnrollmentRequired)
		}
		if cmd == "displayPlaylist" && isPlayCastShape(req) {
			return allow
		}
		return deny(CodeEnvelopeRequired)
	default: // StateLocked, or anything unrecognized: fail closed.
		if route == RouteRelayer && tier == TierController {
			return allow
		}
		return deny(CodeLocked)
	}
}

// Policy applies the package Policy in the authority's current state.
//
// One exception: a RETIRED authority (factory reset in progress, or a reset
// that rolled back after the control directory was deleted) accepts a
// plaintext factoryReset on either route. Locked otherwise refuses every
// owner-tier plaintext and every envelope, which would leave a rolled-back
// device with no remote way to finish the reset its owner asked for; the
// reset only wipes a device that was already being wiped.
func (a *Authority) Policy(route Route, cmd string, req json.RawMessage) Decision {
	if errors.Is(a.LockErr(), ErrRetired) {
		// Retired: factoryReset on any route, and nothing else on any route
		// (review L2: locked alone still let relayer controller-tier
		// plaintext through).
		if cmd == cmdFactoryReset {
			return allow
		}
		return deny(CodeLocked)
	}
	return Policy(a.State(), route, cmd, req)
}

const cmdFactoryReset = "factoryReset"

// isPlayCastShape is the exact enforced-state relayer allowance (§7.2): a
// request with exactly the members intent and dp1_call, intent exactly
// {"action":"now_display"}, and dp1_call an object. This is the shape a Play
// browser session sends; anything wider needs an envelope.
func isPlayCastShape(req json.RawMessage) bool {
	m, err := strictObject(req, []string{"intent", "dp1_call"}, nil)
	if err != nil || !isJSONObject(m["dp1_call"]) {
		return false
	}
	intent, err := strictObject(m["intent"], []string{"action"}, nil)
	if err != nil {
		return false
	}
	return expectString(intent, "action", "now_display") == nil
}
