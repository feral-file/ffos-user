package status

import (
	"context"
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
// than advertising on an interface that cannot actually carry traffic. The
// provisioning AP-trigger guard keys on this same probe: the setup AP raises on
// link loss (no wire, no Wi-Fi association), never on internet loss alone.
func (c *LinkChecker) HasLink(ctx context.Context) bool {
	if c == nil || c.exec == nil {
		return false
	}
	return c.hasLinkOfType(ctx, "ethernet", "wifi")
}

// hasLinkOfType reports whether any device whose TYPE is in wantTypes is in
// NetworkManager's "connected" state. Best-effort nmcli probe behind HasLink,
// returning false on any probe failure.
func (c *LinkChecker) hasLinkOfType(ctx context.Context, wantTypes ...string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, linkCheckTimeout)
	defer cancel()

	// -t terse, -f DEVICE,TYPE,STATE yields lines like "wlp2s0:wifi:connected".
	cmd := c.exec.CommandContext(probeCtx, "nmcli", "-t", "-f", "DEVICE,TYPE,STATE", "device")
	output, err := cmd.Output()
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("Link-state probe failed, assuming no link", zap.Error(err))
		}
		return false
	}

	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		devType, state := parts[1], parts[2]
		if !containsString(wantTypes, devType) {
			continue
		}
		// nmcli reports GENERAL.STATE as "connected" (numeric 100) once a device
		// has an active connection with an address — a usable LAN link.
		if state == "connected" {
			return true
		}
	}
	return false
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
