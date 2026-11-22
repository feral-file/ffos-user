package hub

import (
	"net"
	"net/netip"
	"strings"
)

// The hub serves local sources only (feral-file#3551, spec §7.2 "LAN").
//
// The LAN-open rule lets anyone on the device's own network run the
// undoable command set without credentials, so "the device's own network"
// must mean exactly that: an FF1 behind a port forward, a DMZ host or a
// carrier that hands out public addresses must not turn the open set into
// an Internet-open one. Remote control is relay v2's job, so EVERY hub
// request (envelopes and the WebSocket upgrade included) from a non-local
// source is refused, not just plaintext.
//
// A source is local when, after unmapping an IPv4-mapped IPv6 address, it is
//   - loopback;
//   - private or link-local: RFC 1918, 169.254/16, fe80::/10, ULA fc00::/7;
//   - CGNAT 100.64.0.0/10, which is where Tailscale addresses live (ffos's
//     feral_owner filter already limits tailscale0 to the owner's tailnet IP);
//   - or on-link: inside the prefix of an address on one of this device's
//     interfaces. That keeps a campus network that hands out public
//     addresses, and a phone using its global IPv6 address on the same home
//     LAN, local.
//
// Interface addresses are read per request, not once at start: DHCP and
// SLAAC renumber while the daemon runs. The read happens only when the fixed
// ranges did not already decide, so a typical home LAN pays nothing.

// cgnatPrefix is RFC 6598 shared address space (Tailscale's 100.x range).
var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

// isLocalSource reports whether an http.Request.RemoteAddr is a local
// source. Unparseable addresses are NOT local: the hub fails closed.
// addrs lists the interface addresses (net.InterfaceAddrs in production); a
// failure to list them leaves only the fixed ranges.
func isLocalSource(remoteAddr string, addrs func() ([]net.Addr, error)) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	// A link-local IPv6 source carries its zone ("fe80::1%wlan0").
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnatPrefix.Contains(ip) {
		return true
	}
	if addrs == nil {
		return false
	}
	list, err := addrs()
	if err != nil {
		return false
	}
	for _, a := range list {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		base, ok := netip.AddrFromSlice(n.IP)
		if !ok {
			continue
		}
		base = base.Unmap()
		ones, _ := n.Mask.Size()
		if base.Is4() && ones > 32 {
			// A 16-byte mask on a v4 address (net's IPv4-in-IPv6 form).
			ones -= 96
		}
		p, err := base.Prefix(ones)
		if err != nil || ones == 0 {
			// A /0 would make the whole Internet "on-link"; never trust it.
			continue
		}
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// interfaceAddrs is the address source for isLocalSource. A variable so
// tests can place a network on-link (like deviceHostname).
var interfaceAddrs = net.InterfaceAddrs
