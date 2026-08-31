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
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/netip"
	"os"
	"strings"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// Info describes the credentials the running AP advertises. The captive portal
// and any QR/onboarding surface read these back.
type Info struct {
	SSID      string
	PSK       string
	PortalURL string
}

// Status reports whether the AP is currently up.
type Status struct {
	Active bool
	SSID   string
}

// Backend is the AP lifecycle abstraction. Implementations must be idempotent:
// Up on an already-up AP succeeds by REPLACING it (a brief bounce, never a
// same-name duplicate), and Down on an already-down AP succeeds.
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

	// ProfileName is the fixed NetworkManager profile name for the setup AP, so
	// Up and Down always target the same connection regardless of SSID.
	// Exported so the provisioning link probe (status.LinkChecker.ExternalLink)
	// can exclude the device's own hotspot from external-link detection —
	// including the window where a failed teardown leaves the profile behind.
	ProfileName = "ff1-softap"

	// ssidPrefix namespaces the setup SSID by product.
	ssidPrefix = "FF1-"

	// pskModulus caps the derived passphrase at 8 decimal digits — exactly
	// WPA2-PSK's minimum length, and the shortest key a user can be asked to
	// type on a phone keyboard.
	pskModulus = 100_000_000

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
	if logger == nil {
		logger = zap.NewNop()
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

	// Replace, never stack: NM happily keeps multiple profiles with the same
	// con-name (distinct UUIDs), and `device wifi hotspot` always CREATES one.
	// A leftover profile from an ungraceful previous run — or from a raise
	// whose compensating Down failed — would accumulate a duplicate on every
	// re-raise, breaking the "con-name pins exactly one profile" assumption
	// Down and Status rely on. That is why a failed cleanup is FATAL for this
	// raise, not best-effort: creating over an undeletable leftover is exactly
	// the duplicate-stacking hazard, and the provisioning machine retries the
	// whole raise on its tick, so refusing here is safe. Down itself still
	// tolerates a missing profile (that is the common case).
	if err := b.Down(ctx); err != nil {
		return Info{}, fmt.Errorf("cleanup of existing %s profile before create: %w", ProfileName, err)
	}

	// `device wifi hotspot` creates an AP-mode connection with shared IPv4 in
	// one call; con-name pins it so Down/Status can find it deterministically.
	args := []string{"device", "wifi", "hotspot", "con-name", ProfileName,
		"ssid", info.SSID, "password", info.PSK}
	if b.iface != "" {
		args = append(args, "ifname", b.iface)
	}
	if _, err := b.run(ctx, args...); err != nil {
		return Info{}, err
	}
	// Read the address NetworkManager actually assigned instead of publishing
	// its usual 10.42.0.1 as a constant. Shared mode may choose another subnet
	// to avoid a collision, and only the active profile is authoritative. A
	// direct on-link address is the deterministic browser fallback once the
	// user accepts the no-internet Wi-Fi: it bypasses Private DNS and cellular
	// DNS entirely.
	info.PortalURL = b.activePortalURL(ctx)
	return info, nil
}

// activePortalURL returns the HTTP portal address on the hotspot's own IPv4
// subnet. Address discovery is best-effort: a failure here must not tear down
// an otherwise usable AP whose captive portal can still open automatically.
// The player omits the manual-address instruction when PortalURL is blank.
func (b *nmBackend) activePortalURL(ctx context.Context) string {
	out, err := b.run(ctx, "-g", "IP4.ADDRESS", "connection", "show", "id", ProfileName)
	if err != nil {
		b.logger.Warn("setup AP active address unavailable; omitting direct portal fallback", zap.Error(err))
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		prefix, parseErr := netip.ParsePrefix(strings.TrimSpace(line))
		if parseErr != nil {
			continue
		}
		addr := prefix.Addr().Unmap()
		if addr.Is4() && !addr.IsUnspecified() && !addr.IsLoopback() {
			return "http://" + addr.String()
		}
	}
	b.logger.Warn("setup AP active address missing; omitting direct portal fallback")
	return ""
}

func (b *nmBackend) Down(ctx context.Context) error {
	// Deleting the profile deactivates it too. Tolerate "unknown connection" so
	// Down is idempotent when the AP was never up or already torn down.
	out, err := b.run(ctx, "connection", "delete", ProfileName)
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
		if line == ProfileName {
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
// device_id. The PSK is a deterministic 8-digit code (see numericPSK).
func (b *nmBackend) credentials() (Info, error) {
	raw, err := b.hostname()
	if err != nil {
		return Info{}, fmt.Errorf("read device id: %w", err)
	}
	id := strings.TrimSpace(raw)
	if id == "" {
		return Info{}, fmt.Errorf("device id is empty")
	}
	// Provisioned /etc/hostname already carries the product prefix (e.g.
	// "FF1-8EVTK3RE"); prefixing unconditionally advertised "FF1-FF1-…".
	ssid := id
	if !strings.HasPrefix(id, ssidPrefix) {
		ssid = ssidPrefix + id
	}
	return Info{
		SSID: ssid,
		PSK:  numericPSK(id),
	}, nil
}

// numericPSK derives the WPA2 passphrase from the device_id: the first 4 bytes
// of SHA-256(device_id), reduced to exactly 8 decimal digits (zero-padded).
// Digits-only because users type this on a phone keyboard while reading it off
// the TV; the previous PSK==device_id scheme (mixed case + dash) was too
// error-prone. Deterministic so the same device always advertises the same
// key. This is convenience-level gating, the same security posture as before:
// the QR on screen carries the credentials, the id was already readable from
// the SSID, and the AP is short-lived and offline.
func numericPSK(id string) string {
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("%08d", binary.BigEndian.Uint32(sum[:4])%pskModulus)
}

func (b *nmBackend) run(ctx context.Context, args ...string) ([]byte, error) {
	out, err := b.exec.CommandContext(ctx, nmcliBin, args...).CombinedOutput()
	if err != nil {
		// The error string travels up to callers that log it (ensureAPUp/Down),
		// and the hotspot-create args carry the WPA2 PSK after "password" —
		// nmcli's own error output can echo the command line too. Redact the
		// secret from both the arg list and the captured output so a failed
		// raise can never leak the key into logs.
		return out, fmt.Errorf("nmcli %s: %w: %s",
			strings.Join(redactPassword(args), " "), err,
			strings.TrimSpace(redactSecrets(string(out), args)))
	}
	return out, nil
}

// redactPassword returns a copy of args with every value following a
// "password" argument replaced by a placeholder.
func redactPassword(args []string) []string {
	out := append([]string(nil), args...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "password" {
			out[i+1] = "[redacted]"
		}
	}
	return out
}

// redactSecrets removes every password value present in args from s.
func redactSecrets(s string, args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "password" && args[i+1] != "" {
			s = strings.ReplaceAll(s, args[i+1], "[redacted]")
		}
	}
	return s
}

func readEtcHostname() (string, error) {
	b, err := os.ReadFile(defaultHostnamePath)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
