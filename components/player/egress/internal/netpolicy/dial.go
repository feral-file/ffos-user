package netpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var blocked = []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20"}

func public(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || (ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip)) {
		return false
	}
	for _, s := range blocked {
		if netip.MustParsePrefix(s).Contains(ip) {
			return false
		}
	}
	return true
}

func permittedRoute(raw []byte, allowLAN bool) bool {
	var routes []struct{ Dst, Type string }
	if json.Unmarshal(raw, &routes) != nil || len(routes) != 1 {
		return false
	}
	route := routes[0]
	if route.Type != "" && route.Type != "unicast" {
		return false
	}
	if route.Dst == "default" {
		return true
	}
	if !allowLAN {
		return false
	}
	_, prefixErr := netip.ParsePrefix(route.Dst)
	_, addrErr := netip.ParseAddr(route.Dst)
	return prefixErr == nil || addrErr == nil
}

func Dial(ctx context.Context, network, address string, allowLAN bool) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, errors.New("invalid port")
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else {
		localName := !strings.Contains(host, ".") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".home.arpa")
		if host == "localhost" || strings.HasSuffix(host, ".localhost") || (localName && !allowLAN) {
			return nil, errors.New("local name")
		}
		ips, err = (&net.Resolver{PreferGo: true}).LookupNetIP(ctx, "ip", host+".")
		if err != nil {
			return nil, err
		}
	}
	for _, ip := range ips {
		if !public(ip) && !(allowLAN && ip.Unmap().IsPrivate()) {
			return nil, errors.New("non-public address")
		}
	}
	for _, ip := range ips {
		// Check the matched FIB prefix. A more-specific route can lead to
		// another local subnet even when it goes through a gateway.
		raw, err := exec.CommandContext(ctx, "/usr/bin/ip", "-j", "route", "get", "fibmatch", ip.Unmap().String()).Output()
		if err != nil || !permittedRoute(raw, allowLAN) {
			continue
		}
		d := net.Dialer{Timeout: 8 * time.Second}
		if c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port)); err == nil {
			return c, nil
		}
	}
	return nil, errors.New("no permitted route")
}
