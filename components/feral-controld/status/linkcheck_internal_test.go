package status

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// countingExec fails every invocation immediately and counts them — the seam
// for asserting how many nmcli calls the diagnostic fallback actually issues.
// Hand-rolled (not mocks.MockExec) so this internal test needs no gomock
// controller and stays free of the central mocks package.
type countingExec struct {
	calls  int
	output string
	err    error
}

func (c *countingExec) CommandContext(context.Context, string, ...string) wrapper.ExecCmd {
	c.calls++
	return countingCmd{output: c.output, err: c.err}
}

type countingCmd struct {
	output string
	err    error
}

func (c countingCmd) String() string { return "counting" }
func (c countingCmd) Run() error     { return c.err }
func (c countingCmd) Start() error   { return c.err }
func (c countingCmd) Wait() error    { return c.err }
func (c countingCmd) Output() ([]byte, error) {
	if c.err != nil {
		return nil, c.err
	}
	return []byte(c.output), nil
}
func (c countingCmd) CombinedOutput() ([]byte, error) { return c.Output() }
func (c countingCmd) Pid() int                        { return 0 }

// TestDiagnosticLinkDetailNoRetryAfterTimeout pins the fast-fail retry guard:
// a first probe that consumed the full timeout means NetworkManager is wedged
// — dropping the carrier field cannot fix that, and a retry would double the
// ladder link rung's worst case to 6s, breaking its documented ~16s pass
// budget on exactly the sick devices the ladder exists for.
func TestDiagnosticLinkDetailNoRetryAfterTimeout(t *testing.T) {
	exec := &countingExec{err: errors.New("context deadline exceeded")}
	base := time.Unix(1700000000, 0)
	elapsed := time.Duration(0)
	c := &LinkChecker{exec: exec, logger: zap.NewNop(), now: func() time.Time {
		t := base.Add(elapsed)
		elapsed += linkCheckTimeout + time.Second // next reading: a full timeout later
		return t
	}}

	_, _, err := c.DiagnosticLinkDetail(context.Background(), "ff1-softap")
	assert.Error(t, err)
	assert.Equal(t, 1, exec.calls, "a slow failure must not be retried")
}

// TestDiagnosticLinkDetailNoRetryOnUnsurveyedOutput: an unsurveyed parse
// fails identically with or without the carrier field — the retry cannot
// help, and skipping it keeps the informative sentinel as the returned error.
func TestDiagnosticLinkDetailNoRetryOnUnsurveyedOutput(t *testing.T) {
	exec := &countingExec{output: "GENERAL.DEVICE:lo\nGENERAL.TYPE:loopback\nGENERAL.STATE:100 (connected (externally))\n"}
	c := &LinkChecker{exec: exec, logger: zap.NewNop(), now: time.Now}

	_, _, err := c.DiagnosticLinkDetail(context.Background(), "ff1-softap")
	assert.ErrorIs(t, err, errNoUplinkRows)
	assert.Equal(t, 1, exec.calls, "an unsurveyed output must not be retried")
}
