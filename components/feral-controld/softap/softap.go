// Package softap owns the FF1 setup access point lifecycle: raising a WPA2
// hotspot with a captive portal so a phone can hand the device Wi-Fi
// credentials, and tearing it back down once provisioning completes.
//
// Backend is the containment boundary. The provisioning state machine depends
// only on that interface, never on nmcli, so a later move to hostapd/dnsmasq is
// a single new implementation in this package with no change to callers.
//
// Every subprocess call goes through the shared wrapper.Exec seam for
// testability. This package deliberately carries no relayer, D-Bus, or CDP
// coupling.
package softap

import (
	"context"
	"fmt"
	"os"
	"strings"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// Info describes the credentials the running AP advertises. The captive portal
// and any QR/onboarding surface read these back.
type Info struct {
	SSID string
	PSK  string
}

// Status reports whether the AP is currently up.
type Status struct {
	Active bool
	SSID   string
}

// Backend is the AP lifecycle abstraction. Implementations must be idempotent:
// Up on an already-up AP and Down on an already-down AP both succeed.
type Backend interface {
	// Up raises the AP and returns the advertised credentials.
	Up(ctx context.Context) (Info, error)
	// Down tears the AP down. Missing/inactive is treated as success.
	Down(ctx context.Context) error
	// Status reports whether the AP is active.
	Status(ctx context.Context) (Status, error)
}

const (
	nmcliBin = "nmcli"

	// conName is the fixed NetworkManager profile name for the setup AP, so Up
	// and Down always target the same connection regardless of SSID.
	conName = "ff1-softap"

	// ssidPrefix namespaces the setup SSID by product.
	ssidPrefix = "FF1-"

	// minPSKLen is the WPA2-PSK minimum passphrase length. A device_id shorter
	// than this is padded to reach it.
	minPSKLen = 8

	// defaultHostnamePath is the source of the MAC-derived device_id.
	defaultHostnamePath = "/etc/hostname"
)

// HostnameFunc yields the device_id used to derive the SSID and PSK. It is a
// field so tests can inject a value without touching /etc/hostname.
type HostnameFunc func() (string, error)

// nmBackend implements Backend via nmcli's shared-mode hotspot (AP mode with
// shared IPv4, so NetworkManager runs the DHCP/NAT that the captive portal
// needs).
type nmBackend struct {
	exec     wrapper.Exec
	logger   *zap.Logger
	hostname HostnameFunc

	// iface optionally pins the AP to a Wi-Fi device. Empty lets nmcli choose.
	iface string
}

// NewNetworkManager builds the nmcli-backed AP. hostname may be nil to read the
// device_id from /etc/hostname.
func NewNetworkManager(exec wrapper.Exec, logger *zap.Logger, iface string, hostname HostnameFunc) Backend {
	if hostname == nil {
		hostname = readEtcHostname
	}
	return &nmBackend{
		exec:     exec,
		logger:   logger,
		hostname: hostname,
		iface:    iface,
	}
}

func (b *nmBackend) Up(ctx context.Context) (Info, error) {
	info, err := b.credentials()
	if err != nil {
		return Info{}, err
	}

	// `device wifi hotspot` creates an AP-mode connection with shared IPv4 in
	// one call; con-name pins it so Down/Status can find it deterministically.
	args := []string{"device", "wifi", "hotspot", "con-name", conName,
		"ssid", info.SSID, "password", info.PSK}
	if b.iface != "" {
		args = append(args, "ifname", b.iface)
	}
	if _, err := b.run(ctx, args...); err != nil {
		return Info{}, err
	}
	return info, nil
}

func (b *nmBackend) Down(ctx context.Context) error {
	// Deleting the profile deactivates it too. Tolerate "unknown connection" so
	// Down is idempotent when the AP was never up or already torn down.
	out, err := b.run(ctx, "connection", "delete", conName)
	if err == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(string(out)), "unknown connection") {
		return nil
	}
	return err
}

func (b *nmBackend) Status(ctx context.Context) (Status, error) {
	out, err := b.run(ctx, "-t", "-f", "NAME", "connection", "show", "--active")
	if err != nil {
		return Status{}, err
	}
	active := false
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == conName {
			active = true
			break
		}
	}
	if !active {
		return Status{Active: false}, nil
	}
	// Report the SSID we would advertise; credential derivation is
	// deterministic from the device_id.
	info, err := b.credentials()
	if err != nil {
		return Status{Active: true}, nil
	}
	return Status{Active: true, SSID: info.SSID}, nil
}

// credentials derives the SSID (FF1-<device_id>) and WPA2 PSK from the
// device_id. The PSK IS the device_id, padded to the WPA2 minimum when short.
func (b *nmBackend) credentials() (Info, error) {
	raw, err := b.hostname()
	if err != nil {
		return Info{}, fmt.Errorf("read device id: %w", err)
	}
	id := strings.TrimSpace(raw)
	if id == "" {
		return Info{}, fmt.Errorf("device id is empty")
	}
	return Info{
		SSID: ssidPrefix + id,
		PSK:  padPSK(id),
	}, nil
}

// padPSK repeats the device_id until it satisfies the WPA2 8-char minimum. The
// result is deterministic so the same device always advertises the same key;
// ids already >= minPSKLen (the MAC-derived alphanumeric norm) pass through
// unchanged.
func padPSK(id string) string {
	if len(id) >= minPSKLen {
		return id
	}
	reps := (minPSKLen + len(id) - 1) / len(id)
	return strings.Repeat(id, reps)[:minPSKLen]
}

func (b *nmBackend) run(ctx context.Context, args ...string) ([]byte, error) {
	out, err := b.exec.CommandContext(ctx, nmcliBin, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("nmcli %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func readEtcHostname() (string, error) {
	b, err := os.ReadFile(defaultHostnamePath)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
