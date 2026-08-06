package offlinecache

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// TestSafeHeadlessDebugPort_CollisionWithKioskCDPPortIsCoercedToDefault
// is the regression test for the misconfiguration hazard
// safeHeadlessDebugPort's doc describes: if offlineCache.headlessDebugPort
// is accidentally set to the kiosk's own CDP port, Downloader.Acquire's
// readiness probe would succeed against the live kiosk endpoint instead
// of a freshly spawned headless process, letting capture attach to and
// navigate the kiosk's own on-screen page. The colliding port must be
// coerced back to DefaultHeadlessDebugPort (white-box, package
// offlinecache — Bootstrap does not expose the wired downloader's port
// for a black-box assertion, see cdpsession_wedge_test.go/
// dial_wedge_test.go/downloader_wedge_test.go for the same pattern).
func TestSafeHeadlessDebugPort_CollisionWithKioskCDPPortIsCoercedToDefault(t *testing.T) {
	got := safeHeadlessDebugPort(9222, "http://127.0.0.1:9222", zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	assert.Equal(t, DefaultHeadlessDebugPort, got)
}

// TestSafeHeadlessDebugPort_NonCollidingConfiguredPortIsKept pins that a
// correctly configured (distinct) headless debug port is never rewritten.
func TestSafeHeadlessDebugPort_NonCollidingConfiguredPortIsKept(t *testing.T) {
	got := safeHeadlessDebugPort(9333, "http://127.0.0.1:9222", zaptest.NewLogger(t))
	assert.Equal(t, 9333, got)
}

// TestSafeHeadlessDebugPort_UnparseableKioskEndpointSkipsCheck covers a
// malformed kioskCDPEndpoint (should never happen in practice — it is
// always sourced from config.CDPConfig.Endpoint, never independently
// configurable — but a parse failure must fail open to the configured
// port rather than guessing a collision that may not exist).
func TestSafeHeadlessDebugPort_UnparseableKioskEndpointSkipsCheck(t *testing.T) {
	got := safeHeadlessDebugPort(9223, "not a url", zaptest.NewLogger(t))
	assert.Equal(t, 9223, got)
}

// TestSafeHeadlessDebugPort_CollisionAtTheDefaultFallbackStepsPastIt pins
// that the coercion target itself can never reintroduce the same
// collision it exists to prevent: if the kiosk's own (unusually
// configured) CDP port happens to equal DefaultHeadlessDebugPort, simply
// falling back to that default would silently relocate the hazard rather
// than closing it, so the result must be stepped to a further,
// non-colliding port instead.
func TestSafeHeadlessDebugPort_CollisionAtTheDefaultFallbackStepsPastIt(t *testing.T) {
	kioskEndpoint := "http://127.0.0.1:" + strconv.Itoa(DefaultHeadlessDebugPort)
	got := safeHeadlessDebugPort(DefaultHeadlessDebugPort, kioskEndpoint, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	assert.NotEqual(t, DefaultHeadlessDebugPort, got, "the forced port must not collide with the kiosk port it was forced away from")
}

// TestSafeHeadlessDebugPort_NumericComparisonIgnoresStringFormatting pins
// that ports are compared as integers, not by comparing configured's
// strconv.Itoa against the endpoint's raw port substring — a comparison
// that would (incorrectly) treat e.g. a leading-zero-formatted endpoint
// port as distinct from an equal numeric configured port.
func TestSafeHeadlessDebugPort_NumericComparisonIgnoresStringFormatting(t *testing.T) {
	got := safeHeadlessDebugPort(9222, "http://127.0.0.1:09222", zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	assert.NotEqual(t, 9222, got, "09222 and 9222 are the same numeric port and must still be detected as colliding")
}
