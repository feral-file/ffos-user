package commandrouter

// MaxPreflightItemsForTest re-exports the unexported preflight item budget
// for the external test package (test builds only).
const MaxPreflightItemsForTest = maxPreflightItems

// SetPlayerAsleepForTest replaces the handler's sleep predicate (test builds
// only): mocks cannot satisfy devicectl.PlayerSleepGate's unexported method,
// so the wired predicate is always false for them.
func SetPlayerAsleepForTest(h Handler, asleep func() bool) {
	h.(*handler).asleep = asleep
}
