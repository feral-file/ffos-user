package devicectl

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// recordingMintCloser counts close and supersede requests and remembers the
// closer's error.
type recordingMintCloser struct {
	calls      int
	supersedes int
	err        error
}

func (c *recordingMintCloser) CloseActivePairing(context.Context) (bool, error) {
	c.calls++
	return true, c.err
}

func (c *recordingMintCloser) SupersedeDisplayRestores() { c.supersedes++ }

// A browser-pairing code on screen is closed when the claim QR is about to take
// the screen. This is the only place the close happens for a claim paint, so
// the nil and error cases must not block it.
func TestCloseMintPairingForClaimQR(t *testing.T) {
	t.Run("closes the active pairing once", func(t *testing.T) {
		closer := &recordingMintCloser{}
		e := &executor{logger: zap.NewNop(), mintPairingCloser: closer}

		e.closeMintPairingForClaimQR()

		assert.Equal(t, 1, closer.calls)
	})

	t.Run("no closer wired is a no-op", func(t *testing.T) {
		e := &executor{logger: zap.NewNop()}

		assert.NotPanics(t, e.closeMintPairingForClaimQR)
	})

	t.Run("a failed close is logged, not returned", func(t *testing.T) {
		closer := &recordingMintCloser{err: errors.New("worker did not unwind")}
		e := &executor{logger: zap.NewNop(), mintPairingCloser: closer}

		assert.NotPanics(t, e.closeMintPairingForClaimQR)
		assert.Equal(t, 1, closer.calls)
	})

	t.Run("every close supersedes pending restores, including a failed wait", func(t *testing.T) {
		closer := &recordingMintCloser{err: context.DeadlineExceeded}
		e := &executor{logger: zap.NewNop(), mintPairingCloser: closer}

		e.closeMintPairingForClaimQR()

		assert.Equal(t, 1, closer.supersedes, "a restore left pending by a timed-out close must not paint over the claim QR")
	})
}
