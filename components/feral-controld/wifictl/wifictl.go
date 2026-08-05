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
	"fmt"
	"strconv"
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

	// emptyScanCacheTTL is the much shorter TTL an EMPTY scan result gets when
	// there is nothing better to serve. An empty result is not trustworthy
	// enough for the full TTL (see RefreshScanCache), but it still needs some
	// TTL: CachedScan is called from the captive-portal request path while the
	// AP holds the radio, and a zero-TTL empty cache would fire a live scan on
	// every page load — the one thing the cache exists to prevent.
	emptyScanCacheTTL = 30 * time.Second

	// ssidWaitTimeout / ssidWaitInterval bound the post-AP-bounce wait for the
	// join target to reappear in NM's scan results (see Join / waitForSSID).
	ssidWaitTimeout  = 20 * time.Second
	ssidWaitInterval = 2 * time.Second

	// scanReadyTimeout / scanReadyInterval bound the wait for the Wi-Fi device
	// to become scannable again after the setup AP is torn down (see
	// waitForScanReady). Shorter than ssidWaitTimeout on purpose: this waits
	// only for wpa_supplicant to re-attach the interface, which is a strictly
	// earlier milestone than "a specific neighboring SSID is visible", and the
	// AP is down for the whole wait so the phone is stranded meanwhile.
	scanReadyTimeout  = 10 * time.Second
	scanReadyInterval = 1 * time.Second

	// nmDeviceStateDisconnected mirrors NM_DEVICE_STATE_DISCONNECTED (30).
	// NetworkManager rejects RequestScan outright below this state — see
	// _nm_device_wifi_request_scan in src/core/devices/wifi/nm-device-wifi.c:
	//
	//	if (!priv->enabled || !priv->sup_iface
	//	    || nm_device_get_state(device) < NM_DEVICE_STATE_DISCONNECTED)
	//	        ... NM_DEVICE_ERROR_NOT_ALLOWED, "Scanning not allowed while unavailable"
	//
	// The numeric value is read from nmcli's `device show` output, which prints
	// the raw enum before the localized name ("30 (disconnected)"), so this
	// comparison does not depend on the device's locale.
	nmDeviceStateDisconnected = 30

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

// SavedWifiSSIDs returns the SSID each saved Wi-Fi profile actually targets —
// read from the profile itself (802-11-wireless.ssid), NOT from the profile
// name: profiles this codebase creates are SSID-named (`nmcli device wifi
// connect <ssid>`), but out-of-band profiles (factory image, an ops-side
// `nmcli connection add con-name …`) need not be, and a name-based comparison
// would misread such a device as relocated on every offline boot. anyHidden
// reports whether any profile targets a hidden network (802-11-wireless.hidden):
// hidden SSIDs never appear in scan output, so scan-presence evidence is
// unobtainable for them and callers must treat the whole saved set as
// unverifiable rather than "absent".
//
// Fail-closed contract: the caller uses this as evidence for a one-way
// decision, and a list silently missing a profile is exactly the false
// positive it must never act on — so any per-profile read failure AND any
// wifi profile whose ssid reads back EMPTY are errors, never skips. Profiles
// are resolved by UUID (same rationale as deleteWifiProfiles): resolving by
// name matches ANY profile type sharing the id, and `-g 802-11-wireless.ssid`
// against, say, an ethernet profile exits 0 with empty output — which the
// empty-is-error rule would misreport without the UUID pinning. The two
// fields are read with separate -g calls because multi-field -g output layout
// differs across nmcli versions.
func (c *Controller) SavedWifiSSIDs(ctx context.Context) (ssids []string, anyHidden bool, err error) {
	out, _, err := c.run(ctx, "-t", "-f", "UUID,TYPE,NAME", "connection", "show")
	if err != nil {
		return nil, false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		// UUID and TYPE never contain colons, so the third field is NAME with
		// terse-mode escaping intact (kept only for error legibility).
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		uuid, typ, name := parts[0], parts[1], unescapeTerse(parts[2])
		if typ != "802-11-wireless" {
			continue
		}
		out, _, err := c.run(ctx, "-g", "802-11-wireless.ssid", "connection", "show", "uuid", uuid)
		if err != nil {
			return nil, false, fmt.Errorf("reading ssid of profile %q (%s): %w", name, uuid, err)
		}
		// Strip ONLY nmcli's record terminator (a single trailing newline).
		// Leading/trailing spaces are valid SSID bytes, and the scan side
		// (parseSSIDsCapped) preserves them — TrimSpace here made a
		// whitespace-padded saved SSID compare unequal to its own scan
		// sighting, so an in-range network read as "absent" and could satisfy
		// every relocation confirmation: exactly the false positive this
		// function's fail-closed contract exists to prevent.
		ssid := unescapeTerse(strings.TrimSuffix(string(out), "\n"))
		if ssid == "" {
			return nil, false, fmt.Errorf("profile %q (%s) reports an empty ssid", name, uuid)
		}
		ssids = append(ssids, ssid)
		out, _, err = c.run(ctx, "-g", "802-11-wireless.hidden", "connection", "show", "uuid", uuid)
		if err != nil {
			return nil, false, fmt.Errorf("reading hidden flag of profile %q (%s): %w", name, uuid, err)
		}
		if strings.TrimSpace(string(out)) == "yes" {
			anyHidden = true
		}
	}
	return ssids, anyHidden, nil
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

// ScanAllSSIDs performs a forced live scan and returns every unique SSID in
// range, uncapped. Scan's maxSSIDs cap is a captive-portal DISPLAY concern;
// callers that use scan presence as EVIDENCE (the boot relocation check reads
// "no saved SSID in the scan" as "the device was moved" and raises the setup
// AP on it — a one-way door on this hardware) must see the full list: a
// weaker in-range saved network truncated out by a display cap would read as
// absent and fire a false relocation.
func (c *Controller) ScanAllSSIDs(ctx context.Context) ([]string, error) {
	// Same radio-readiness gate as RefreshScanCache, and it matters MORE here:
	// the relocation check runs its first sample right after the boot AP
	// sweep flips the radio AP→station, exactly when an early `wifi list`
	// exits 0 with an empty row set — which the caller must treat as
	// inconclusive and may only retry a bounded number of times. Fails open
	// after scanReadyTimeout, so a healthy radio pays nothing.
	c.waitForScanReady(ctx)
	args := []string{"-t", "-f", "SSID", "device", "wifi", "list", "--rescan", "yes"}
	// Pin to the configured interface like the other radio commands (Join,
	// waitForSSID, wifiDeviceState — including this call's own readiness
	// gate): on multi-radio hardware an unpinned list can report a different
	// radio's air, and "saved SSID absent" from the wrong radio is fabricated
	// relocation evidence.
	if c.iface != "" {
		args = append(args, "ifname", c.iface)
	}
	out, _, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return parseSSIDsUncapped(string(out)), nil
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
//
// A successful-but-EMPTY scan never replaces a usable cache. The portal's
// "search for networks again" button bounces the setup AP, so the refresh that
// follows always lands moments after the radio flipped from AP back to station
// mode — with NM's BSS list empty and its rescan request not yet honored,
// `nmcli device wifi list --rescan yes` exits 0 with no rows. That is
// indistinguishable at this layer from "no networks exist", and caching it for
// the full scanCacheTTL blanked the picker (dropping the portal to its manual
// SSID fallback) for ten minutes — the button reliably destroyed the very list
// it was pressed to refresh. Keeping the previous entries, with their ORIGINAL
// expiry so a stale list still ages out, means a transient empty scan costs
// nothing; with no usable previous entries the empty result is cached only
// briefly (emptyScanCacheTTL) so the next read retries soon.
//
// The returned slice is always the LIVE scan result, empty included, never the
// retained cache: callers retry on empty (see provisioning.ensureAPUp), and
// handing back the cache would hide the failed scan from them.
func (c *Controller) RefreshScanCache(ctx context.Context) ([]string, error) {
	// Do not ask for a scan the radio cannot serve yet (see waitForScanReady):
	// asking early does not fail loudly, it returns an empty list.
	c.waitForScanReady(ctx)

	ssids, err := c.Scan(ctx, true)
	if err != nil {
		return nil, err
	}

	now := c.clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(ssids) == 0 {
		if len(c.cacheSSIDs) > 0 && !c.cacheExp.IsZero() && now.Before(c.cacheExp) {
			c.logger.Warn("wifictl: forced scan saw no networks; keeping the previous scan cache",
				zap.Int("cached_ssids", len(c.cacheSSIDs)))
			return nil, nil
		}
		c.logger.Warn("wifictl: forced scan saw no networks and no usable cache; caching empty briefly",
			zap.Duration("ttl", emptyScanCacheTTL))
		c.cacheSSIDs = nil
		c.cacheExp = now.Add(emptyScanCacheTTL)
		return nil, nil
	}

	c.cacheSSIDs = ssids
	c.cacheExp = now.Add(scanCacheTTL)
	return ssids, nil
}

// waitForScanReady blocks until NetworkManager will actually honor a scan
// request on the Wi-Fi device, or the wait window closes.
//
// This exists because `nmcli device wifi list --rescan yes` is NOT the
// "scan and report what is in the air" primitive it reads as. It is "request a
// scan; print the BSS cache", and it goes quiet in exactly the case that
// matters here. From nmcli's wifi_list_rescan_cb (src/nmcli/devices.c), when
// RequestScan is refused with NM_DEVICE_ERROR_NOT_ALLOWED:
//
//   - device state >= DISCONNECTED (scan already running, or NM's rate limit):
//     retry every second until nmcli's own 15s timeout. Self-healing, fine.
//   - device state < DISCONNECTED (unmanaged, or UNAVAILABLE): give up
//     immediately and print the cache. nmcli's own comment: "If it's
//     unavailable, that usually means that we wait for wpa_supplicant to
//     start. In that case, also quit (without scan results)."
//
// That second branch never sets nmcli's return value, so the command exits 0
// with zero rows — a scan that never ran is indistinguishable from a scan that
// saw nothing. And it is precisely the state the device is in right after the
// setup AP comes down: tearing down the AP-mode connection makes NM drop and
// re-create the supplicant interface, so the device passes through UNAVAILABLE
// (priv->sup_iface NULL) for as long as wpa_supplicant needs to re-attach.
//
// The portal's "search for networks again" button bounces the AP by design, so
// the refresh behind it lands in that window every single time — which is how
// the button came to reliably empty the picker it was pressed to refill. The
// join path hit the same window from the other side and papered over it with
// waitForSSID; this is the same hazard at the scan.
//
// Fail-open in both directions: an unreadable device state proceeds to the scan
// (an nmcli output shape we do not recognize must never stall provisioning),
// and so does a timeout — the caller still gets nmcli's real answer, and
// RefreshScanCache no longer trusts an empty one.
func (c *Controller) waitForScanReady(ctx context.Context) {
	deadline := c.clock.Now().Add(scanReadyTimeout)
	for {
		state, known := c.wifiDeviceState(ctx)
		if !known || state >= nmDeviceStateDisconnected {
			return
		}
		if c.clock.Now().Add(scanReadyInterval).After(deadline) {
			c.logger.Warn("wifictl: wifi device still not scannable; scanning anyway",
				zap.Int("nm_device_state", state))
			return
		}
		c.logger.Debug("wifictl: wifi device not scannable yet; waiting",
			zap.Int("nm_device_state", state))
		if err := c.clock.SleepContext(ctx, scanReadyInterval); err != nil {
			return
		}
	}
}

// wifiDeviceState returns the NMDeviceState of the Wi-Fi device (pinned to
// iface when set), and whether it could be determined at all.
//
// It reads `device show` rather than `device status` deliberately: status
// renders STATE as a translated word only, while show renders it as the raw
// enum followed by that word ("30 (disconnected)"), which is what makes the
// comparison against nmDeviceStateDisconnected locale-independent. Anything
// unparseable reports known=false so the caller fails open.
func (c *Controller) wifiDeviceState(ctx context.Context) (int, bool) {
	args := []string{"-t", "-f", "GENERAL.DEVICE,GENERAL.TYPE,GENERAL.STATE", "device", "show"}
	if c.iface != "" {
		args = append(args, c.iface)
	}
	out, _, err := c.run(ctx, args...)
	if err != nil {
		c.logger.Debug("wifictl: wifi device state query failed", zap.Error(err))
		return 0, false
	}

	// Terse `device show` emits one "FIELD:value" line per field, grouped per
	// device in the order requested, so tracking the current block's TYPE is
	// enough to attribute a STATE line to a Wi-Fi device.
	var typ string
	for _, line := range strings.Split(string(out), "\n") {
		field, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch field {
		case "GENERAL.DEVICE":
			typ = ""
		case "GENERAL.TYPE":
			typ = value
		case "GENERAL.STATE":
			if typ != "wifi" {
				continue
			}
			// "30 (disconnected)" — the leading enum is the locale-stable part.
			num, _, _ := strings.Cut(value, " ")
			state, convErr := strconv.Atoi(num)
			if convErr != nil {
				c.logger.Debug("wifictl: unparseable device state", zap.String("value", value))
				return 0, false
			}
			return state, true
		}
	}
	return 0, false
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

// Join connects to ssid using psk (WPA2-PSK; empty for an open network). On
// failure it returns a *JoinError with a classified Kind. hidden marks the
// target as a non-broadcasting network: nmcli gets `hidden yes` (directed
// probing — without it NM only consults broadcast scan results and the join
// fails "not found" for a network that is right there), and the pre-connect
// visibility wait is skipped because a hidden SSID never appears in scan
// output, so waiting for it can only burn the whole window.
//
// nmcli's `device wifi connect` creates a saved connection profile named after
// the SSID as a side effect. Two cleanup steps mirror feral-setupd's wifi_utils
// semantics:
//   - Pre-delete any stale profile targeting this SSID before connecting. A
//     stale profile can make nmcli reuse dead credentials instead of the new
//     ones (https://bbs.archlinux.org/viewtopic.php?id=300321&p=2).
//   - On a failed join, delete the half-created profile so a later scan/list
//     never surfaces a broken saved network the device can't actually use.
func (c *Controller) Join(ctx context.Context, ssid, psk string, hidden bool) error {
	// Pre-delete: best-effort, a missing profile is the normal case.
	c.deleteWifiProfiles(ctx, ssid)

	if !hidden {
		// The AP-bounce join reaches here moments after the hotspot went down,
		// with the radio freshly flipped from AP back to station mode and NM's
		// BSS list empty or stale. `device wifi connect` consults that list
		// WITHOUT rescanning, so connecting immediately fails "no network with
		// SSID" even though the network exists. Wait for the target to become
		// visible first. (Hidden targets skip this: they are invisible to scans
		// by definition, and `hidden yes` makes NM probe for them directly.)
		c.waitForSSID(ctx, ssid)
	}

	args := []string{"device", "wifi", "connect", ssid}
	// An OPEN network takes no password argument at all: sending `password ""`
	// makes NM build a WPA-PSK security block around the empty key, which the
	// AP then rejects — the open network reads as "wrong password" forever.
	if psk != "" {
		args = append(args, "password", psk)
	}
	if hidden {
		args = append(args, "hidden", "yes")
	}
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

// wifiProfile is one saved 802-11-wireless profile's identity fields, read
// from the profile itself. SSID comes from 802-11-wireless.ssid (NOT the
// profile name — out-of-band profiles need not be SSID-named, which is exactly
// the D9 stale-credential dead end: a name-keyed delete misses the profile
// whose stale PSK NM then reuses). KeyMgmt is
// 802-11-wireless-security.key-mgmt; empty means the profile has no security
// section (an open network).
type wifiProfile struct {
	uuid    string
	name    string
	ssid    string
	keyMgmt string
	// readErr records a failed per-profile field read; ssid/keyMgmt are
	// unreliable when set. See wifiProfileList's failure-bias note.
	readErr error
}

// wifiProfileList reads every saved Wi-Fi profile's (uuid, ssid, key-mgmt).
// This is the single nmcli read path the SSID-scoped deletion below keys on;
// the escape-policy recheck (docs/network-recovery-ux.md §4.2) extends the
// same read with more field sets later. Per-profile field reads are separate
// -g calls because multi-field -g output layout differs across nmcli versions
// (same rationale as SavedWifiSSIDs). Profiles are resolved by UUID so a `-g`
// against a same-named non-Wi-Fi profile can never be misattributed.
//
// Failure bias: an unreadable PROFILE is returned with readErr set rather than
// dropped or failing the listing — the two consumers want opposite treatments
// (deletion skips it: "cannot confirm PSK" must never delete; the future
// recheck aborts on it: a blind activation off an unreadable list is worse
// than a retry), so the helper reports and lets each caller apply its own
// bias. Only the top-level listing failing is an error.
func (c *Controller) wifiProfileList(ctx context.Context) ([]wifiProfile, error) {
	out, _, err := c.run(ctx, "-t", "-f", "UUID,TYPE,NAME", "connection", "show")
	if err != nil {
		return nil, err
	}
	var profiles []wifiProfile
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		// UUID and TYPE never contain colons, so the third field is NAME with
		// terse-mode escaping intact.
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		uuid, typ, name := parts[0], parts[1], unescapeTerse(parts[2])
		if typ != "802-11-wireless" {
			continue
		}
		p := wifiProfile{uuid: uuid, name: name}
		ssidOut, _, ssidErr := c.run(ctx, "-g", "802-11-wireless.ssid", "connection", "show", "uuid", uuid)
		if ssidErr != nil {
			p.readErr = ssidErr
			profiles = append(profiles, p)
			continue
		}
		// Strip ONLY nmcli's record terminator: leading/trailing spaces are
		// valid SSID bytes (same rule as SavedWifiSSIDs).
		p.ssid = unescapeTerse(strings.TrimSuffix(string(ssidOut), "\n"))
		kmOut, _, kmErr := c.run(ctx, "-g", "802-11-wireless-security.key-mgmt", "connection", "show", "uuid", uuid)
		if kmErr != nil {
			p.readErr = kmErr
			profiles = append(profiles, p)
			continue
		}
		p.keyMgmt = strings.TrimSpace(string(kmOut))
		profiles = append(profiles, p)
	}
	return profiles, nil
}

// deletableKeyMgmt reports whether a profile's key management is in the
// PSK/open family the portal join flow is allowed to replace. A portal PSK
// join must never destroy an enterprise (802.1X/EAP) profile for the same
// SSID — those credentials cannot be re-entered through the portal, so the
// allow-list fails safe: anything unrecognized is kept.
func deletableKeyMgmt(keyMgmt string) bool {
	switch keyMgmt {
	case "", "none", "wpa-psk", "sae":
		// "" = no security section (open); "none" = WEP/static; "wpa-psk" =
		// WPA2-Personal; "sae" = WPA3-Personal. All re-creatable from the
		// portal form.
		return true
	default:
		return false
	}
}

// deleteWifiProfiles removes saved Wi-Fi profiles whose TARGET SSID is ssid
// and whose key management is PSK/open — never by profile name, and never
// enterprise profiles:
//   - Name matching misses the stale profile NM actually reuses when a profile
//     is not named after its SSID (the D9 dead end: the user types the correct
//     new password forever while NM re-offers the old PSK), and ssid is user
//     input from the captive portal — a submission equal to an unrelated
//     ethernet/VPN profile's name must never delete that profile.
//   - The key-mgmt scope keeps a portal PSK join from destroying an 802.1X
//     profile for the same SSID (see deletableKeyMgmt).
//
// Best-effort like the two call sites (pre-join stale-credential purge,
// post-failure cleanup of the half-created profile): a listing failure means
// no cleanup, and an unreadable profile is skipped — it cannot be CONFIRMED
// PSK, and the fail-safe direction for deletion is to keep it.
func (c *Controller) deleteWifiProfiles(ctx context.Context, ssid string) {
	profiles, err := c.wifiProfileList(ctx)
	if err != nil {
		c.logger.Debug("wifictl: profile listing for cleanup failed",
			zap.String("ssid", ssid), zap.Error(err))
		return
	}
	for _, p := range profiles {
		if p.readErr != nil {
			c.logger.Debug("wifictl: profile unreadable, keeping it",
				zap.String("name", p.name), zap.String("uuid", p.uuid), zap.Error(p.readErr))
			continue
		}
		if p.ssid != ssid || !deletableKeyMgmt(p.keyMgmt) {
			continue
		}
		if _, _, delErr := c.run(ctx, "connection", "delete", "uuid", p.uuid); delErr != nil {
			c.logger.Debug("wifictl: wifi profile delete failed",
				zap.String("ssid", ssid), zap.String("uuid", p.uuid), zap.Error(delErr))
		}
	}
}

// ActivationProfile is one saved Wi-Fi profile's activation identity for the
// provisioning recheck blink (docs/network-recovery-ux.md §4.2): UUID to
// activate, target SSID for the in-range filter, Hidden because a hidden SSID
// never appears in scans (it gets one attempt last, if budget remains), and
// LastUsed (connection.timestamp, Unix seconds, 0 = never activated) for
// most-recently-used ordering among in-range candidates.
type ActivationProfile struct {
	UUID     string
	SSID     string
	Hidden   bool
	LastUsed int64
}

// ActivationProfiles returns every saved Wi-Fi profile's activation identity,
// extending the wifiProfileList nmcli read path with the recheck's field set
// (one read path, two field sets — see wifiProfileList). Unlike the deletion
// consumer this is FAIL-CLOSED: any per-profile read failure fails the whole
// listing, because the caller ACTIVATES off this list and a blind
// `connection up` on a misread profile blocks up to its full activation
// deadline — half a bounded blink budget — starving the profile that would
// have worked (the §4.2 fail-bias: a listing error aborts the blink and
// re-raises, never a blind activation).
func (c *Controller) ActivationProfiles(ctx context.Context) ([]ActivationProfile, error) {
	out, _, err := c.run(ctx, "-t", "-f", "UUID,TYPE,NAME", "connection", "show")
	if err != nil {
		return nil, err
	}
	var profiles []ActivationProfile
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		uuid, typ, name := parts[0], parts[1], unescapeTerse(parts[2])
		if typ != "802-11-wireless" {
			continue
		}
		p := ActivationProfile{UUID: uuid}
		ssidOut, _, err := c.run(ctx, "-g", "802-11-wireless.ssid", "connection", "show", "uuid", uuid)
		if err != nil {
			return nil, fmt.Errorf("reading ssid of profile %q (%s): %w", name, uuid, err)
		}
		// Record-terminator strip only; padding bytes are valid SSID content
		// (same rule as SavedWifiSSIDs).
		p.SSID = unescapeTerse(strings.TrimSuffix(string(ssidOut), "\n"))
		hiddenOut, _, err := c.run(ctx, "-g", "802-11-wireless.hidden", "connection", "show", "uuid", uuid)
		if err != nil {
			return nil, fmt.Errorf("reading hidden flag of profile %q (%s): %w", name, uuid, err)
		}
		p.Hidden = strings.TrimSpace(string(hiddenOut)) == "yes"
		tsOut, _, err := c.run(ctx, "-g", "connection.timestamp", "connection", "show", "uuid", uuid)
		if err != nil {
			return nil, fmt.Errorf("reading timestamp of profile %q (%s): %w", name, uuid, err)
		}
		// A missing/blank timestamp (never activated) is 0, not an error: the
		// field is only an ORDERING hint, and a fresh profile is legitimately
		// timestamp-less. An unparseable non-blank value still fails closed.
		ts := strings.TrimSpace(string(tsOut))
		if ts != "" {
			n, convErr := strconv.ParseInt(ts, 10, 64)
			if convErr != nil {
				return nil, fmt.Errorf("unparseable timestamp %q of profile %q (%s)", ts, name, uuid)
			}
			p.LastUsed = n
		}
		profiles = append(profiles, p)
	}
	return profiles, nil
}

// ActivateProfile runs an EXPLICIT activation of the saved profile (`nmcli
// connection up uuid <uuid>`), honoring ctx via the exec seam. The recheck
// blink uses this rather than waiting for autoconnect because the repo's own
// evidence says passive autoconnect after an AP→station flip is unreliable
// (empty/stale BSS list; see waitForScanReady), and bench evidence
// (docs/network-recovery-ux.md §3) shows explicit activation is not gated by
// NM's autoconnect backoff on repeatedly failed profiles.
func (c *Controller) ActivateProfile(ctx context.Context, uuid string) error {
	out, _, err := c.run(ctx, "connection", "up", "uuid", uuid)
	if err != nil {
		return fmt.Errorf("activating profile %s: %w (%s)", uuid, err, strings.TrimSpace(string(out)))
	}
	return nil
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
// as nmcli returned them and capped at maxSSIDs (the portal display cap).
func parseSSIDs(out string) []string {
	return parseSSIDsCapped(out, maxSSIDs)
}

// parseSSIDsUncapped is parseSSIDs without the display cap — for callers that
// treat scan CONTENTS as evidence, where truncation would fabricate absence.
func parseSSIDsUncapped(out string) []string {
	return parseSSIDsCapped(out, 0)
}

func parseSSIDsCapped(out string, limit int) []string {
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
		if limit > 0 && len(ssids) >= limit {
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
