// Package wifictl is a thin, testable wrapper around nmcli for the Wi-Fi
// operations the SoftAP provisioning flow needs: enumerating saved profiles,
// scanning for nearby networks, and joining a network the user picked in the
// captive portal.
//
// Every subprocess call goes through the shared wrapper.Exec seam so tests can
// substitute a scripted nmcli. This package deliberately owns no relayer,
// D-Bus, or CDP coupling; it is the connectivity primitive the provisioning
// state machine composes.
package wifictl

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const (
	nmcliBin = "nmcli"

	// maxSSIDs caps how many networks we surface to the captive portal. Ported
	// from feral-setupd (constant::MAX_SSIDS) so the picker UI keeps its shape.
	maxSSIDs = 9

	// scanCacheTTL bounds how long a pre-AP scan stays usable. Ported from
	// feral-setupd (constant::SSID_CACHE_TTL, 10 minutes).
	scanCacheTTL = 10 * time.Minute

	// ssidWaitTimeout / ssidWaitInterval bound the post-AP-bounce wait for the
	// join target to reappear in NM's scan results (see Join / waitForSSID).
	ssidWaitTimeout  = 20 * time.Second
	ssidWaitInterval = 2 * time.Second

	// joinCleanupTimeout bounds the detached post-failure profile delete.
	joinCleanupTimeout = 10 * time.Second
)

// exitCoder is implemented by *os/exec.ExitError (via the promoted
// ProcessState.ExitCode) and by test fakes. Reading the code through this
// interface lets us classify nmcli failures without importing os/exec here,
// which also keeps gosec's G204 off these files.
type exitCoder interface {
	ExitCode() int
}

// Controller wraps nmcli. It is safe for concurrent use; the scan cache is
// guarded by mu.
type Controller struct {
	exec   wrapper.Exec
	clock  wrapper.Clock
	logger *zap.Logger

	// iface optionally pins operations to a specific Wi-Fi device. Empty lets
	// nmcli choose, which is correct on single-radio FF1 hardware.
	iface string

	mu         sync.Mutex
	cacheSSIDs []string
	cacheExp   time.Time
}

// New builds a Controller. iface may be empty to let nmcli pick the Wi-Fi
// device.
func New(exec wrapper.Exec, clock wrapper.Clock, logger *zap.Logger, iface string) *Controller {
	return &Controller{
		exec:   exec,
		clock:  clock,
		logger: logger,
		iface:  iface,
	}
}

// -----------------------------------------------------------------------------
// Saved profiles
// -----------------------------------------------------------------------------

// SavedProfiles returns the names of saved Wi-Fi connection profiles
// (802-11-wireless). The provisioning flow uses this to decide whether the
// device already has credentials and can skip the captive portal.
func (c *Controller) SavedProfiles(ctx context.Context) ([]string, error) {
	// -t terse, -f NAME,TYPE: one "NAME:TYPE" line per saved connection.
	out, _, err := c.run(ctx, "-t", "-f", "NAME,TYPE", "connection", "show")
	if err != nil {
		return nil, err
	}

	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		// TYPE is a colon-free enum and always the last field; a profile NAME
		// may itself contain an (escaped) colon, so split on the last one.
		idx := strings.LastIndex(line, ":")
		if idx < 0 {
			continue
		}
		name, typ := line[:idx], line[idx+1:]
		if typ == "802-11-wireless" {
			names = append(names, unescapeTerse(name))
		}
	}
	return names, nil
}

// HasSavedProfile reports whether any saved Wi-Fi profile exists.
func (c *Controller) HasSavedProfile(ctx context.Context) (bool, error) {
	names, err := c.SavedProfiles(ctx)
	if err != nil {
		return false, err
	}
	return len(names) > 0, nil
}

// -----------------------------------------------------------------------------
// Scanning (with pre-AP cache)
// -----------------------------------------------------------------------------

// Scan performs a live Wi-Fi scan and returns up to maxSSIDs unique SSIDs in
// signal order. force triggers a fresh rescan rather than reusing NM's own
// scan results.
func (c *Controller) Scan(ctx context.Context, force bool) ([]string, error) {
	args := []string{"-t", "-f", "SSID", "device", "wifi", "list"}
	if force {
		args = append(args, "--rescan", "yes")
	}
	out, _, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return parseSSIDs(string(out)), nil
}

// CachedScan returns a cached scan when it is still fresh, otherwise performs a
// live rescan and caches it.
//
// The cache exists because NetworkManager serializes Wi-Fi operations on a
// single radio: once the SoftAP hotspot is up, a station-mode scan is
// unreliable or outright blocked. So the provisioning flow scans BEFORE raising
// the AP and lets the captive portal read the pre-AP result from this cache
// while the AP holds the radio.
func (c *Controller) CachedScan(ctx context.Context) ([]string, error) {
	c.mu.Lock()
	if !c.cacheExp.IsZero() && c.clock.Now().Before(c.cacheExp) {
		ssids := append([]string(nil), c.cacheSSIDs...)
		c.mu.Unlock()
		return ssids, nil
	}
	c.mu.Unlock()
	return c.RefreshScanCache(ctx)
}

// RefreshScanCache performs a forced live scan and stores it for CachedScan.
// Call this while station mode still owns the radio (before the AP goes up).
func (c *Controller) RefreshScanCache(ctx context.Context) ([]string, error) {
	ssids, err := c.Scan(ctx, true)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.cacheSSIDs = ssids
	c.cacheExp = c.clock.Now().Add(scanCacheTTL)
	c.mu.Unlock()
	return ssids, nil
}

// -----------------------------------------------------------------------------
// Joining
// -----------------------------------------------------------------------------

// JoinErrorKind classifies why a join attempt failed so the captive portal can
// show an actionable message.
type JoinErrorKind int

const (
	// JoinErrUnknown is any failure we could not attribute to a specific cause.
	JoinErrUnknown JoinErrorKind = iota
	// JoinErrAuth means the pre-shared key was rejected (wrong password).
	JoinErrAuth
	// JoinErrSSIDNotFound means the requested SSID was not visible to nmcli.
	JoinErrSSIDNotFound
	// JoinErrTimeout means the activation did not complete in nmcli's window.
	JoinErrTimeout
)

func (k JoinErrorKind) String() string {
	switch k {
	case JoinErrAuth:
		return "auth-failure"
	case JoinErrSSIDNotFound:
		return "ssid-not-found"
	case JoinErrTimeout:
		return "timeout"
	default:
		return "unknown"
	}
}

// JoinError carries the classified cause plus the underlying nmcli error.
type JoinError struct {
	Kind JoinErrorKind
	// Output is the trimmed combined nmcli output, kept for logs and tests.
	Output string
	err    error
}

func (e *JoinError) Error() string {
	return "wifi join failed (" + e.Kind.String() + "): " + e.Output
}

func (e *JoinError) Unwrap() error { return e.err }

// Join connects to ssid using psk (WPA2-PSK). On failure it returns a
// *JoinError with a classified Kind.
//
// nmcli's `device wifi connect` creates a saved connection profile named after
// the SSID as a side effect. Two cleanup steps mirror feral-setupd's wifi_utils
// semantics:
//   - Pre-delete any same-named profile before connecting. A stale profile can
//     make nmcli reuse dead credentials instead of the new ones
//     (https://bbs.archlinux.org/viewtopic.php?id=300321&p=2).
//   - On a failed join, delete the half-created profile so a later scan/list
//     never surfaces a broken saved network the device can't actually use.
func (c *Controller) Join(ctx context.Context, ssid, psk string) error {
	// Pre-delete: best-effort, a missing profile is the normal case.
	c.deleteWifiProfiles(ctx, ssid)

	// The AP-bounce join reaches here moments after the hotspot went down, with
	// the radio freshly flipped from AP back to station mode and NM's BSS list
	// empty or stale. `device wifi connect` consults that list WITHOUT
	// rescanning, so connecting immediately fails "no network with SSID" even
	// though the network exists. Wait for the target to become visible first.
	c.waitForSSID(ctx, ssid)

	args := []string{"device", "wifi", "connect", ssid, "password", psk}
	if c.iface != "" {
		args = append(args, "ifname", c.iface)
	}
	out, code, err := c.run(ctx, args...)
	if err == nil {
		return nil
	}

	joinErr := &JoinError{
		Kind:   classifyJoin(code, string(out)),
		Output: strings.TrimSpace(string(out)),
		err:    err,
	}

	// Failed-join cleanup: drop the broken profile nmcli may have persisted.
	// Detached ctx: the failure may BE the caller's ctx canceling (daemon
	// shutdown mid-join), and running the delete on that dead ctx would skip
	// it — stranding a broken saved profile that biases the next boot to
	// "provisioned" and defers the setup AP a full offline window.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), joinCleanupTimeout)
	defer cancel()
	c.deleteWifiProfiles(cleanupCtx, ssid)
	return joinErr
}

// deleteWifiProfiles removes saved profiles named ssid, and ONLY Wi-Fi ones.
// `nmcli connection delete <name>` matches ANY profile type by ID, and ssid is
// user input from the captive portal — a submission equal to an unrelated
// ethernet/VPN profile's name must never delete that profile. So: resolve the
// name to UUIDs, filter to 802-11-wireless, delete by UUID. Best-effort like
// the two call sites (pre-join stale-credential purge, post-failure cleanup of
// the half-created profile): a listing failure just means no cleanup.
func (c *Controller) deleteWifiProfiles(ctx context.Context, ssid string) {
	out, _, err := c.run(ctx, "-t", "-f", "UUID,TYPE,NAME", "connection", "show")
	if err != nil {
		c.logger.Debug("wifictl: profile listing for cleanup failed",
			zap.String("ssid", ssid), zap.Error(err))
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		// UUID and TYPE never contain colons, so the third field is NAME with
		// terse-mode escaping intact.
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		uuid, typ, name := parts[0], parts[1], unescapeTerse(parts[2])
		if typ != "802-11-wireless" || name != ssid {
			continue
		}
		if _, _, delErr := c.run(ctx, "connection", "delete", "uuid", uuid); delErr != nil {
			c.logger.Debug("wifictl: wifi profile delete failed",
				zap.String("ssid", ssid), zap.String("uuid", uuid), zap.Error(delErr))
		}
	}
}

// waitForSSID blocks until ssid appears in a forced rescan or the wait window
// closes. Scan errors are retried, not fatal: right after the mode flip the
// device can transiently report busy/unavailable. On timeout it returns
// normally and lets the connect surface nmcli's real error — the network may
// genuinely be gone, and nmcli's message is the truthful outcome.
func (c *Controller) waitForSSID(ctx context.Context, ssid string) {
	deadline := c.clock.Now().Add(ssidWaitTimeout)
	for {
		args := []string{"-t", "-f", "SSID", "device", "wifi", "list", "--rescan", "yes"}
		if c.iface != "" {
			args = append(args, "ifname", c.iface)
		}
		out, _, err := c.run(ctx, args...)
		if err == nil && ssidInScan(string(out), ssid) {
			return
		}
		if err != nil {
			c.logger.Debug("wifictl: post-bounce rescan failed; retrying",
				zap.String("ssid", ssid), zap.Error(err))
		}
		if c.clock.Now().Add(ssidWaitInterval).After(deadline) {
			c.logger.Warn("wifictl: ssid not visible within wait window; connecting anyway",
				zap.String("ssid", ssid))
			return
		}
		if err := c.clock.SleepContext(ctx, ssidWaitInterval); err != nil {
			return
		}
	}
}

// ssidInScan reports whether ssid appears in nmcli terse scan output. Unlike
// parseSSIDs it is deliberately NOT capped at maxSSIDs: the join target may
// rank below the portal picker's cut in a dense environment.
func ssidInScan(out, ssid string) bool {
	for _, line := range strings.Split(out, "\n") {
		if unescapeTerse(line) == ssid {
			return true
		}
	}
	return false
}

// classifyJoin maps an nmcli exit code and its output onto a JoinErrorKind.
// It is a pure function so every branch is directly unit-testable.
//
// nmcli exit codes (man nmcli): 3 = timeout, 4 = activation failed,
// 10 = connection/device/AP does not exist. Exit 4 on a PSK join is
// overwhelmingly a rejected key, so we treat it as auth unless the output
// clearly names a not-found or timeout cause; the text checks also catch
// distros/versions that fold these into exit 1.
func classifyJoin(exitCode int, output string) JoinErrorKind {
	lower := strings.ToLower(output)

	switch {
	case exitCode == 10,
		strings.Contains(lower, "no network with ssid"),
		strings.Contains(lower, "ssid not found"),
		strings.Contains(lower, "network not found"):
		return JoinErrSSIDNotFound

	case exitCode == 3, strings.Contains(lower, "timeout"), strings.Contains(lower, "timed out"):
		return JoinErrTimeout

	case exitCode == 4,
		strings.Contains(lower, "secrets were required"),
		strings.Contains(lower, "no secrets"),
		strings.Contains(lower, "802-1x"),
		strings.Contains(lower, "802-11-wireless-security"),
		strings.Contains(lower, "invalid password"):
		return JoinErrAuth

	default:
		return JoinErrUnknown
	}
}

// -----------------------------------------------------------------------------
// nmcli plumbing
// -----------------------------------------------------------------------------

// run invokes nmcli through the injected exec seam and returns combined output,
// the process exit code (0 on success, -1 when the code is unknowable), and a
// wrapped error on non-zero exit.
func (c *Controller) run(ctx context.Context, args ...string) ([]byte, int, error) {
	out, err := c.exec.CommandContext(ctx, nmcliBin, args...).CombinedOutput()
	if err == nil {
		return out, 0, nil
	}
	return out, exitCode(err), err
}

func exitCode(err error) int {
	var ec exitCoder
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return -1
}

// parseSSIDs extracts unique, non-empty SSIDs from nmcli terse output, ordered
// as nmcli returned them and capped at maxSSIDs.
func parseSSIDs(out string) []string {
	seen := make(map[string]struct{})
	var ssids []string
	for _, line := range strings.Split(out, "\n") {
		ssid := unescapeTerse(line)
		if ssid == "" {
			continue
		}
		if _, dup := seen[ssid]; dup {
			continue
		}
		seen[ssid] = struct{}{}
		ssids = append(ssids, ssid)
		if len(ssids) >= maxSSIDs {
			break
		}
	}
	return ssids
}

// unescapeTerse reverses nmcli's terse-mode escaping, where ':' and '\' inside a
// field are backslash-escaped.
func unescapeTerse(field string) string {
	if !strings.Contains(field, "\\") {
		return field
	}
	var b strings.Builder
	b.Grow(len(field))
	escaped := false
	for _, r := range field {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
