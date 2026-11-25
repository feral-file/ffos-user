package commandrouter_test

import (
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
)

// newTestLogger is zaptest.NewLogger for tests whose handler may start a
// background DP-1 audit (dp1.AuditDocumentAsync). The audit logs through this
// logger after the command returned; a zaptest logger must not be written to
// once its test has finished, so cleanup drains audits first. Cleanups run
// last-registered-first, so this one (registered after the logger's own) runs
// before the logger is torn down.
func newTestLogger(t *testing.T) *zap.Logger {
	t.Helper()
	l := zaptest.NewLogger(t)
	t.Cleanup(dp1.WaitForAudits)
	return l
}
