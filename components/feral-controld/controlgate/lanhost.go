package controlgate

import "net"

// DefaultRouteIPv4 returns the source address of the default IPv4 route as a
// literal, or "" when there is none or it is not a LAN address. Connecting a
// UDP socket only asks the kernel for a route: no packet is sent, and the
// destination (TEST-NET-1) is never contacted.
func DefaultRouteIPv4() string {
	conn, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return ""
	}
	defer conn.Close() //nolint:errcheck // nothing was sent; close cannot lose data.
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	return lanIPv4Literal(addr.IP)
}

// lanIPv4Literal keeps only what a controller accepts as the claim QR's host
// (§11): a private or link-local IPv4 address. Anything else is left out, so
// the QR never points a phone at an address it would refuse.
func lanIPv4Literal(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil && (v4.IsPrivate() || v4.IsLinkLocalUnicast()) {
		return v4.String()
	}
	return ""
}
