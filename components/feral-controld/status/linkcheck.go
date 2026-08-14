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
	// now feeds the diagnostic probe's fast-fail retry guard; injected so the
	// no-retry-after-timeout branch is testable without a real 3s hang.
	now func() time.Time
}

// NewLinkChecker builds a LinkChecker over the given exec wrapper.
func NewLinkChecker(exec wrapper.Exec, logger *zap.Logger) *LinkChecker {
	return &LinkChecker{exec: exec, logger: logger, now: time.Now}
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

// ExternalLinkDetail is ExternalLink plus the ethernet-only verdict from the
// SAME nmcli read: linkProbe already computes both in one pass, and the
// provisioning machine's health snapshot (network-recovery-ux §4.7) needs the
// link TYPE without spending a second probe — the polling path must never run
// nmcli, so the machine caches what its own tick probes saw, and this is how
// a tick probe learns the type for free. Same surface-errors bias as
// ExternalLink.
func (c *LinkChecker) ExternalLinkDetail(ctx context.Context, excludeProfile string) (link, wired bool, err error) {
	if c == nil || c.exec == nil {
		return false, false, errors.New("link checker not initialized")
	}
	res, err := c.linkProbe(ctx, excludeProfile)
	return res.link, res.wired, err
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

// LinkTelemetry reports the per-type link verdicts (ethernet ACTIVATED, wifi
// ACTIVATED) from one nmcli read, for the netmetrics gauges
// (docs/wan-outage-observability.md stage 0). Unlike every other probe here it
// applies no hotspot exclusion — a radio ACTIVATED as the device's own setup
// AP reads as wifi=true, which is honest telemetry during an outage. Errors
// surface so the caller can export "unknown" instead of a fabricated 0; it
// must never feed a recovery or AP-raise decision.
func (c *LinkChecker) LinkTelemetry(ctx context.Context) (wired, wifi bool, err error) {
	if c == nil || c.exec == nil {
		return false, false, errors.New("link checker not initialized")
	}
	res, err := c.linkProbe(ctx, "")
	return res.wired, res.wifi, err
}

// DiagnosticLinkDetail reports L2-level connectivity — carrier/association
// rather than full NM activation — for the netlog diagnosis ladder's link
// rung: a device mid-DHCP (IP_CONFIG..ACTIVATED) or an ethernet port with raw
// carrier counts as an uplink, so the lease rung can then judge its IP layer.
// Gating the ladder on ACTIVATED-only misread venue DHCP failures as
// link-down (cable/radio out) and made the no-lease class unreachable —
// exactly the failure the taxonomy exists to name. Exclusion and
// survey-validity/error semantics match ExternalLinkDetail.
//
// It must NOT be wired into the AP-raise guard, hub/mDNS lifecycle, or
// admission gates: those key on ACTIVATED deliberately, and counting a
// mid-activation device as "link" would change their suppression behavior.
func (c *LinkChecker) DiagnosticLinkDetail(ctx context.Context, excludeProfile string) (link, wired bool, err error) {
	if c == nil || c.exec == nil {
		return false, false, errors.New("link checker not initialized")
	}
	// The carrier field rides a SEPARATE nmcli invocation, never the shared
	// probe: if a deployed NetworkManager rejected WIRED-PROPERTIES.CARRIER,
	// a shared invocation would take down every ACTIVATED-only verdict with
	// it — AP-raise deferred, hub/mDNS down, admission gate closed —
	// fleet-wide, for the sake of a diagnostics-only field. Here a rejection
	// costs one retry without the field (the state-window rule still covers
	// the DHCP-stuck case), and the diagnostic path is rate-limited to once
	// per 60s, so the extra exec is cheap.
	start := c.now()
	res, err := c.probe(ctx, excludeProfile, true)
	// Retry only on a FAST failure: a rejected field errors immediately and
	// dropping it can help, while a timeout means NetworkManager is wedged —
	// retrying would double the ladder link rung's worst case to 6s and break
	// its documented ~16s pass budget for a failure the fallback cannot fix.
	// An unsurveyed parse (errNoUplinkRows) fails identically either way, so
	// it skips the retry too, and keeps its informative error.
	if err != nil && !errors.Is(err, errNoUplinkRows) && c.now().Sub(start) < linkCheckTimeout {
		res, err = c.probe(ctx, excludeProfile, false)
	}
	return res.diagLink, res.diagWired, err
}

// nmDeviceStateActivated is NetworkManager's NM_DEVICE_STATE_ACTIVATED (100):
// the device has an active connection with an address — a usable LAN link.
// Compared numerically because the textual rendering is localized (see
// linkProbe); the numeric enum also already covers externally-managed devices,
// which NM ≥1.36 renders as "connected (externally)" but still numbers 100.
const nmDeviceStateActivated = 100

// nmDeviceStateIPConfig is NM_DEVICE_STATE_IP_CONFIG (70): L2 is established
// (cable negotiated / station associated) and the device is requesting
// addresses. States 70..100 therefore mean "the link itself is up" even when
// DHCP never completes — the window the diagnostic verdicts (see
// DiagnosticLinkDetail) must count, and the ACTIVATED-only verdicts must not.
// DEACTIVATING (110) and FAILED (120) sit numerically above ACTIVATED, so
// range checks below must bound both ends.
const nmDeviceStateIPConfig = 70

// linkResult carries one probe's verdicts. link is the combined uplink verdict
// (any ACTIVATED ethernet or wifi device, minus the exclusion); wired is the
// ethernet-only verdict; wifi is the wifi-only verdict. They are computed in
// one pass so WiredLink shares the exact survey-validity rule of the other
// probes — two separate nmcli reads could disagree about whether the output
// surveyed anything at all.
type linkResult struct {
	link  bool
	wired bool
	wifi  bool
	// diagLink/diagWired are the L2-level verdicts for the netlog diagnosis
	// ladder (DiagnosticLinkDetail): a device counts from IP_CONFIG onward,
	// or on raw ethernet carrier, so a venue DHCP failure reads as "link up,
	// no lease" instead of "cable out". Kept separate because every other
	// consumer (AP-raise guard, hub/mDNS lifecycle, admission gates) keys on
	// ACTIVATED deliberately — loosening those would change suppression
	// behavior.
	diagLink  bool
	diagWired bool
}

// linkProbe reports whether any ethernet or wifi device is in NetworkManager's
// ACTIVATED state, skipping devices whose active connection is excludeProfile
// (empty = no exclusion). Shared nmcli probe behind HasLink, ExternalLink, and
// WiredLink; error handling is the caller's, since they have different failure
// biases. The exclusion applies only to the combined link verdict, not the
// wired one — it exists to discount the device's own Wi-Fi hotspot, which can
// never be an ethernet row.
func (c *LinkChecker) linkProbe(ctx context.Context, excludeProfile string) (linkResult, error) {
	return c.probe(ctx, excludeProfile, false)
}

// probe is the parser behind linkProbe and DiagnosticLinkDetail. withCarrier
// adds WIRED-PROPERTIES.CARRIER to the field list and must stay OFF for the
// shared probe (see DiagnosticLinkDetail for why the field is quarantined).
func (c *LinkChecker) probe(ctx context.Context, excludeProfile string, withCarrier bool) (linkResult, error) {
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
	fields := "GENERAL.DEVICE,GENERAL.TYPE,GENERAL.STATE,GENERAL.CONNECTION"
	if withCarrier {
		// Diagnostic-only field (see DiagnosticLinkDetail); nmcli emits it
		// solely for ethernet blocks, so it never perturbs wifi parsing.
		fields += ",WIRED-PROPERTIES.CARRIER"
	}
	cmd := c.exec.CommandContext(probeCtx, "nmcli", "-t", "-f", fields, "device", "show")
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
		carrier   bool
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
		// Diagnostic L2 verdict: IP_CONFIG..ACTIVATED (the range check excludes
		// DEACTIVATING/FAILED, which number higher), or raw ethernet carrier —
		// a cable NM has not activated is still a live link for diagnosis. Same
		// exclusion as the combined link verdict: the device's own hotspot is
		// never an uplink.
		l2 := (cur.state >= nmDeviceStateIPConfig && cur.state <= nmDeviceStateActivated) ||
			(cur.typ == "ethernet" && cur.carrier)
		excluded := excludeProfile != "" && cur.conn == excludeProfile
		if l2 && !excluded {
			res.diagLink = true
			if cur.typ == "ethernet" {
				res.diagWired = true
			}
		}
		if cur.state != nmDeviceStateActivated {
			return
		}
		// The wired verdict is recorded before the exclusion check on purpose:
		// the exclusion discounts the device's own Wi-Fi hotspot, and an
		// ethernet row must never be hidden by a profile-name collision with it.
		if cur.typ == "ethernet" {
			res.wired = true
		}
		// The wifi verdict is likewise raw (pre-exclusion): it feeds telemetry
		// (LinkTelemetry), where "the radio is ACTIVATED as our own AP" is
		// itself signal during an outage and must not be hidden. It must NOT be
		// consumed by any AP-raise guard — that is what the exclusion-aware
		// combined `link` verdict is for.
		if cur.typ == "wifi" {
			res.wifi = true
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
		case "WIRED-PROPERTIES.CARRIER":
			// nmcli may localize boolean value words; only the exact English
			// "on" counts, and anything else safely degrades to the numeric
			// state rule above (locale-stable), never to a false positive.
			cur.carrier = value == "on"
		}
	}
	flush()

	if !surveyed {
		return linkResult{}, errNoUplinkRows
	}
	return res, nil
}

// errNoUplinkRows marks a probe whose output surveyed no candidate uplink
// (see the survey-validity comment in probe). A sentinel so the diagnostic
// fallback can tell "the FIELD LIST was rejected" (worth retrying without the
// carrier field) from "the survey itself proved nothing" (identical either
// way).
var errNoUplinkRows = errors.New("no ethernet/wifi device rows in nmcli output")
