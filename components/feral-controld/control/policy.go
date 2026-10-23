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
	// RouteRelayV2 is the device-ID relay (ff-relayer#26). Its address is
	// the public device ID and it authenticates nobody, so no plaintext
	// command is ever admitted on it, in any state (§7.2 v2 column). Not
	// wired to a transport yet: the v2 integration lands that.
	RouteRelayV2
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
	if route == RouteRelayV2 {
		return relayV2Refusal(state)
	}
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
	// The v2 refusal runs before the retired exception: on the public
	// device-ID relay anyone could otherwise wipe a device whose reset rolled
	// back. The exception stays for the LAN and the legacy relayer.
	if route == RouteRelayV2 {
		return relayV2Refusal(a.State())
	}
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

// relayV2Refusal is the public code for any plaintext on RouteRelayV2:
// legacy has no owner yet (enroll first), locked refuses everything, and an
// owned device wants an envelope.
func relayV2Refusal(state State) Decision {
	switch state {
	case StateLegacy:
		return deny(CodeEnrollmentRequired)
	case StateEnrolled, StateEnforced:
		return deny(CodeEnvelopeRequired)
	default:
		return deny(CodeLocked)
	}
}

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
