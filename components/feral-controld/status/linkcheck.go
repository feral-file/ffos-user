package status

import (
	"context"
	"errors"
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
	up, err := c.linkProbe(ctx, "")
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("Link-state probe failed, assuming no link", zap.Error(err))
		}
		return false
	}
	return up
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
	return c.linkProbe(ctx, excludeProfile)
}

// linkProbe reports whether any ethernet or wifi device is in NetworkManager's
// "connected" state, skipping devices whose active connection is
// excludeProfile (empty = no exclusion). Shared nmcli probe behind HasLink and
// ExternalLink; error handling is the caller's, since the two have opposite
// failure biases.
func (c *LinkChecker) linkProbe(ctx context.Context, excludeProfile string) (bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, linkCheckTimeout)
	defer cancel()

	// -t terse, -f DEVICE,TYPE,STATE,CONNECTION yields lines like
	// "wlp2s0:wifi:connected:HomeWifi". CONNECTION is the active profile name,
	// which is how the hotspot is told apart from a station association on the
	// same wifi device. Terse mode backslash-escapes ':' inside values, so
	// SplitN(4) keeps a connection name containing colons intact in parts[3];
	// the escaping never affects the comparison against excludeProfile
	// (ff1-softap contains no ':').
	cmd := c.exec.CommandContext(probeCtx, "nmcli", "-t", "-f", "DEVICE,TYPE,STATE,CONNECTION", "device")
	output, err := cmd.Output()
	if err != nil {
		return false, err
	}

	parsedAny := false
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		parts := strings.SplitN(line, ":", 4)
		if len(parts) != 4 {
			continue
		}
		parsedAny = true
		devType, state, conn := parts[1], parts[2], parts[3]
		if devType != "ethernet" && devType != "wifi" {
			continue
		}
		// nmcli reports GENERAL.STATE as "connected" (numeric 100) once a device
		// has an active connection with an address — a usable LAN link. Prefix
		// match, not equality: NM ≥1.36 renders externally-managed devices as
		// "connected (externally)", and an exact match would read that healthy
		// wire as a CONFIRMED absence — the one verdict that authorizes raising
		// the setup AP. "connecting (...)" does not share the prefix, so a
		// still-negotiating device correctly stays a non-link.
		if !strings.HasPrefix(state, "connected") {
			continue
		}
		if excludeProfile != "" && conn == excludeProfile {
			continue
		}
		return true, nil
	}
	if !parsedAny {
		// Not one row split into four fields: this is corrupt or empty output,
		// not a survey that confirmed every device link-less. Returning
		// (false, nil) here would hand ExternalLink's caller a CONFIRMED
		// absence — the one verdict that authorizes raising the setup AP —
		// off data that proved nothing, so surface it as a probe failure
		// (ExternalLink defers; HasLink keeps failing closed to false).
		return false, errors.New("nmcli device output had no parseable rows")
	}
	return false, nil
}
