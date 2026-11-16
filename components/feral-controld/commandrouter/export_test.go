package commandrouter

// SetPlayerAsleepForTest replaces the handler's sleep predicate (test builds
// only): mocks cannot satisfy devicectl.PlayerCastWakeGate's unexported method,
// so the wired predicate is always false for them.
func SetPlayerAsleepForTest(h Handler, asleep func() bool) {
	h.(*handler).asleep = asleep
}
