package control

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
)

// Route is the transport a PLAINTEXT command arrived on. It matters only for
// plaintext (§7.2); an envelope's authority never depends on its route.
type Route int

const (
	RouteLAN Route = iota
	RouteRelayer
	// RouteRelayV2 is relay v2 (ff-relayer#26), on either of its channels.
	// The relay authenticates nobody and one channel is addressed by the
	// public device ID, so no plaintext command is admitted on it, with one
	// exception: the request that asks an ownerless device to show its
	// code (§7.2 v2 column, relayV2Plaintext).
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
//
// The LAN rule (feral-file#3551): on the device's own network anyone may
// run what the next command can undo, which is the controller tier (see
// commandTiers); what cannot be undone is the owner's alone, by envelope.
// "LAN" here is the hub, which serves local sources only (hub
// localsource.go). lanOpen is the owner's switch and matters only once
// there is an owner: closed, an owned device refuses all LAN plaintext.
// Before an owner exists the one owner-tier plaintext admitted is
// showPairingQRCode, which puts the first-owner invitation on the physical
// screen (§5.1); enroll itself is an envelope.
func Policy(state State, lanOpen bool, route Route, cmd string, req json.RawMessage) Decision {
	if route == RouteRelayV2 {
		return relayV2Plaintext(state, cmd, req)
	}
	tier := TierOf(cmd)
	switch state {
	case StateLegacy:
		// The legacy (topic) relayer keeps its pre-protocol behavior: new
		// images carry no relayer block, so that route carries no traffic
		// (§15); it goes when the v1 client code is deleted.
		if route == RouteRelayer || tier == TierController || cmd == cmdShowPairingQRCode {
			return allow
		}
		return deny(CodeEnrollmentRequired)
	case StateEnrolled:
		if route == RouteLAN {
			if lanOpen && tier == TierController {
				return allow
			}
			return deny(CodeEnrollmentRequired)
		}
		if tier == TierController {
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
	// The v2 rule runs before the retired exception: on the public
	// device-ID relay anyone could otherwise wipe a device whose reset rolled
	// back. The exception stays for the LAN and the legacy relayer.
	if route == RouteRelayV2 {
		return relayV2Plaintext(a.State(), cmd, req)
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
	return Policy(a.State(), a.LANOpen(), route, cmd, req)
}

const (
	cmdFactoryReset      = "factoryReset"
	cmdShowPairingQRCode = string(commands.CMD_SHOW_PAIRING_QR_CODE)
)

// relayV2Plaintext is the whole plaintext policy of RouteRelayV2. It admits
// one request: showPairingQRCode with exactly {"show":true,"purpose":"enroll"}
// on a legacy device, which a phone that cannot reach the LAN sends to get
// the first-owner code on the physical screen (§5.1). The code itself never
// travels: whoever asks still needs sight of the screen.
//
// The shape is exact on purpose. show:false must never be admitted here: it
// latches the pairing confirmation and takes the code down, so any stranger
// on the public channel could stop a new device from ever showing one. A
// request without purpose:"enroll" would repaint a claimed device's topic QR.
//
// Admission here is not sufficient: the transport also runs the executor's
// precheck (claimed, already showing, per-boot budget) before anything runs.
func relayV2Plaintext(state State, cmd string, req json.RawMessage) Decision {
	if state == StateLegacy && cmd == cmdShowPairingQRCode && isShowEnrollCodeShape(req) {
		return allow
	}
	return relayV2Refusal(state)
}

// isShowEnrollCodeShape: exactly the members show and purpose, show the
// literal true and purpose "enroll".
func isShowEnrollCodeShape(req json.RawMessage) bool {
	m, err := strictObject(req, []string{"show", "purpose"}, nil)
	if err != nil || string(bytes.TrimSpace(m["show"])) != "true" {
		return false
	}
	return expectString(m, "purpose", "enroll") == nil
}

// relayV2Refusal is the public code for a refused plaintext on RouteRelayV2:
// legacy has no owner yet (enroll first), locked refuses everything, and an
// owned device wants an envelope.
func relayV2Refusal(state State) Decision {
	switch state {
	case StateLegacy:
		return deny(CodeEnrollmentRequired)
	case StateEnrolled:
		return deny(CodeEnvelopeRequired)
	default:
		return deny(CodeLocked)
	}
}

// isPlayCastShape is "the exact Play shape" (§5.7), the one request a
// display grant may send: exactly the members intent and dp1_call, intent
// exactly {"action":"now_display"}, and dp1_call an object.
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
