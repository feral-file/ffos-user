package status

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// linkCheckTimeout bounds the nmcli probe so a hung NetworkManager never blocks
// a lifecycle or status-serving caller.
const linkCheckTimeout = 3 * time.Second

// LinkState reports whether the device currently has a usable local network
// link, independent of internet reachability. It is the seam the LAN
// hub + mDNS lifecycle key on (the hub is the BLE-replacement recovery channel,
// so it must be reachable on any LAN even with no upstream internet).
//
// It lives here — beside its nmcli-backed LinkChecker implementation — rather
// than in the mediator that consumes it, because the central mocks package must
// not import mediator (mediator imports devicectl, and devicectl tests import
// mocks; a mediator import would form a cycle). status is already safely
// importable from mocks. The in-progress wifictl package will re-point this.
type LinkState interface {
	HasLink(ctx context.Context) bool
}

// LinkChecker reports whether the device currently has a usable local network
// link (an ethernet/wifi device NetworkManager considers connected),
// independent of internet reachability.
//
// This is deliberately the single, small seam the LAN hub + mDNS lifecycle key
// on. The hub is the BLE-replacement recovery channel, so it and mDNS discovery
// must come up on any LAN even with no upstream internet — link state, not
// sys-monitord's internet-reachability flag, is the correct trigger.
//
// It is backed by `nmcli` today. The in-progress wifictl package is expected to
// own richer NetworkManager state later and re-point consumers at it; do NOT
// import or wire wifictl in here — keep this a self-contained local probe so
// the two efforts stay decoupled.
type LinkChecker struct {
	exec   wrapper.Exec
	logger *zap.Logger
}

// NewLinkChecker builds a LinkChecker over the given exec wrapper.
func NewLinkChecker(exec wrapper.Exec, logger *zap.Logger) *LinkChecker {
	return &LinkChecker{exec: exec, logger: logger}
}

// HasLink returns true when at least one ethernet or wifi device is in
// NetworkManager's "connected" state. It is best-effort: any probe failure is
// treated as "no link" so callers fail closed (advertiser stays down) rather
// than advertising on an interface that cannot actually carry traffic. Note it
// counts the device's own setup hotspot as a link — deliberate for mDNS/hub
// discoverability (a phone joined to the hotspot is a LAN peer); the
// provisioning AP-trigger guard must use ExternalLink instead.
func (c *LinkChecker) HasLink(ctx context.Context) bool {
	if c == nil || c.exec == nil {
		return false
	}
	res, err := c.linkProbe(ctx, "")
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("Link-state probe failed, assuming no link", zap.Error(err))
		}
		return false
	}
	return res.link
}

// ExternalLink reports whether the device has a usable local link on a
// connection OTHER than excludeProfile (the device's own setup hotspot): an
// ethernet or wifi device in NetworkManager's "connected" state whose active
// connection name differs. It exists for the provisioning AP-trigger guard,
// which must never count the hotspot it raised — or failed to tear down — as
// an uplink and suppress the AP off its own residue.
//
// Unlike HasLink it surfaces probe failures instead of failing closed: for the
// guard, a false "no link" is destructive (it authorizes raising the AP, which
// drops a live Wi-Fi association on the single radio and cannot be undone
// without a human), so the caller treats an error as "unknown" and defers.
func (c *LinkChecker) ExternalLink(ctx context.Context, excludeProfile string) (bool, error) {
	if c == nil || c.exec == nil {
		return false, errors.New("link checker not initialized")
	}
	res, err := c.linkProbe(ctx, excludeProfile)
	return res.link, err
}

// WiredLink reports whether the device has a live ethernet link: an ethernet
// device in NetworkManager's ACTIVATED state. It exists for callers that must
// distinguish a wire from a Wi-Fi association — the `startWifiSetup` admission
// gate and the wired-devices-never-auto-raise rule
// (docs/app-triggered-wifi-setup.md; docs/network-recovery-ux.md constraint 6).
// It must NOT be conflated with ExternalLink: that probe counts an associated
// Wi-Fi station as a link, so using it as a wired guard would reject every
// Wi-Fi target device the setup flow exists for.
//
// Verdict semantics, pinned by docs/network-recovery-ux.md constraint 6:
//   - The survey is valid when the nmcli output contains at least one ethernet
//     or Wi-Fi device row (the shared `surveyed` rule below) — corrupt or empty
//     output proves nothing about wire state and surfaces as an error.
//   - Given a valid survey, the wire verdict is computed from ethernet rows
//     only; a valid survey with no ethernet row is confirmed-no-wire
//     (false, nil).
//
// Errors surface rather than defaulting in either direction: the admission
// caller fails closed (rejects the command), and the escape-policy window
// treats an error as a pause, never as evidence.
func (c *LinkChecker) WiredLink(ctx context.Context) (bool, error) {
	if c == nil || c.exec == nil {
		return false, errors.New("link checker not initialized")
	}
	res, err := c.linkProbe(ctx, "")
	return res.wired, err
}

// nmDeviceStateActivated is NetworkManager's NM_DEVICE_STATE_ACTIVATED (100):
// the device has an active connection with an address — a usable LAN link.
// Compared numerically because the textual rendering is localized (see
// linkProbe); the numeric enum also already covers externally-managed devices,
// which NM ≥1.36 renders as "connected (externally)" but still numbers 100.
const nmDeviceStateActivated = 100

// linkResult carries one probe's verdicts. link is the combined uplink verdict
// (any ACTIVATED ethernet or wifi device, minus the exclusion); wired is the
// ethernet-only verdict. They are computed in one pass so WiredLink shares the
// exact survey-validity rule of the other probes — two separate nmcli reads
// could disagree about whether the output surveyed anything at all.
type linkResult struct {
	link  bool
	wired bool
}

// linkProbe reports whether any ethernet or wifi device is in NetworkManager's
// ACTIVATED state, skipping devices whose active connection is excludeProfile
// (empty = no exclusion). Shared nmcli probe behind HasLink, ExternalLink, and
// WiredLink; error handling is the caller's, since they have different failure
// biases. The exclusion applies only to the combined link verdict, not the
// wired one — it exists to discount the device's own Wi-Fi hotspot, which can
// never be an ethernet row.
func (c *LinkChecker) linkProbe(ctx context.Context, excludeProfile string) (linkResult, error) {
	probeCtx, cancel := context.WithTimeout(ctx, linkCheckTimeout)
	defer cancel()

	// `device show` rather than `device status` deliberately: status renders
	// STATE as a translated word only ("verbunden"), which any English text
	// match would read as CONFIRMED link absence — the one verdict that
	// authorizes raising the setup AP — on every non-English locale. show
	// renders the raw enum first ("100 (connected)"), which is locale-stable;
	// same approach as wifictl.wifiDeviceState. Terse -t emits one
	// "FIELD:value" line per field, grouped per device, with GENERAL.DEVICE
	// opening each block. GENERAL.CONNECTION is the active profile name, which
	// is how the hotspot is told apart from a station association on the same
	// wifi device; Cut at the first ':' keeps a name containing colons intact
	// (terse mode backslash-escapes them, and ff1-softap contains none).
	cmd := c.exec.CommandContext(probeCtx, "nmcli", "-t", "-f",
		"GENERAL.DEVICE,GENERAL.TYPE,GENERAL.STATE,GENERAL.CONNECTION", "device", "show")
	output, err := cmd.Output()
	if err != nil {
		return linkResult{}, err
	}

	// A "no link" verdict counts as CONFIRMED only if the survey saw at least
	// one candidate uplink: an ethernet/wifi block with a parseable numeric
	// state. Corrupt or empty output, or a listing with no ethernet/wifi
	// devices at all (loopback/p2p only), proves nothing about uplink state —
	// returning (false, nil) off it would authorize raising the setup AP over
	// a possibly-healthy link, so it surfaces as a probe failure instead
	// (ExternalLink defers; HasLink keeps failing closed to false).
	type record struct {
		typ, conn string
		state     int
		hasState  bool
	}
	var cur record
	var res linkResult
	surveyed := false
	// flush evaluates the finished device block. GENERAL.DEVICE is relied on as
	// the block delimiter below: it is what triggers flushing the previous
	// record and resetting cur for the next device, so it MUST be the first
	// field nmcli emits per block. That holds because -f lists it first in the
	// requested field order, which nmcli's terse mode honors. A GENERAL.TYPE/
	// STATE/CONNECTION line arriving before its block's GENERAL.DEVICE would be
	// attributed to whatever record is currently open (the previous device, or
	// none) and then lost when GENERAL.DEVICE later resets cur.
	flush := func() {
		if cur.typ != "ethernet" && cur.typ != "wifi" {
			return
		}
		if !cur.hasState {
			return
		}
		surveyed = true
		if cur.state != nmDeviceStateActivated {
			return
		}
		// The wired verdict is recorded before the exclusion check on purpose:
		// the exclusion discounts the device's own Wi-Fi hotspot, and an
		// ethernet row must never be hidden by a profile-name collision with it.
		if cur.typ == "ethernet" {
			res.wired = true
		}
		if excludeProfile != "" && cur.conn == excludeProfile {
			return
		}
		res.link = true
	}
	for _, line := range strings.Split(string(output), "\n") {
		field, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch field {
		case "GENERAL.DEVICE":
			flush()
			cur = record{}
		case "GENERAL.TYPE":
			cur.typ = value
		case "GENERAL.STATE":
			// "100 (connected)" — the leading enum is the locale-stable part.
			num, _, _ := strings.Cut(value, " ")
			if n, convErr := strconv.Atoi(num); convErr == nil {
				cur.state, cur.hasState = n, true
			}
		case "GENERAL.CONNECTION":
			cur.conn = value
		}
	}
	flush()

	if !surveyed {
		return linkResult{}, errors.New("no ethernet/wifi device rows in nmcli output")
	}
	return res, nil
}
