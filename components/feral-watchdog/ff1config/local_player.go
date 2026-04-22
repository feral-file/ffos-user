package ff1config

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// LauncherMessageURLPrefix matches feral-setupd `constant::MSG_URL_PREFIX` for CDP navigation to the launcher message step.
const LauncherMessageURLPrefix = "file:///opt/feral/ui/launcher/index.html?step=message&message="

// LocalPlayerUnavailableMessage matches feral-setupd `constant::LOCAL_PLAYER_UNAVAILABLE_MSG`.
const LocalPlayerUnavailableMessage = "This FF1 could not reach the built-in art player on this device. The player files or HTTP server may be missing. Reboot once; if it still fails, contact support@feralfile.com."

// IsLocalBundlePlayerURL is true for the bundled static player on loopback port 8080 (HTTP only).
func IsLocalBundlePlayerURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	if !strings.EqualFold(u.Scheme, "http") {
		return false
	}
	if u.Hostname() != "127.0.0.1" {
		return false
	}
	return u.Port() == "8080"
}

// LauncherMessageNavigateURL builds a file:// launcher URL with a query-encoded message body.
func LauncherMessageNavigateURL(message string) string {
	return LauncherMessageURLPrefix + url.QueryEscape(message)
}

const (
	localPlayerTCPWait      = 30 * time.Second
	localPlayerTCPPollEvery = 250 * time.Millisecond
)

// WaitLocalBundlePlayerTCP polls until something accepts TCP on 127.0.0.1:8080 or ctx is done.
// Matches feral-setupd `webapp::wait_local_bundle_player_tcp` (CDP Page.navigate does not surface net errors reliably).
func WaitLocalBundlePlayerTCP(ctx context.Context) error {
	return PollTCPUntilOpen(ctx, "127.0.0.1:8080")
}

// PollTCPUntilOpen is exported for tests; production code should use WaitLocalBundlePlayerTCP.
func PollTCPUntilOpen(ctx context.Context, address string) error {
	waitCtx, cancel := context.WithTimeout(ctx, localPlayerTCPWait)
	defer cancel()
	d := &net.Dialer{Timeout: localPlayerTCPPollEvery}
	for {
		c, err := d.DialContext(waitCtx, "tcp", address)
		if err == nil {
			_ = c.Close()
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("wait tcp %s: %w", address, waitCtx.Err())
		case <-time.After(localPlayerTCPPollEvery):
		}
	}
}
