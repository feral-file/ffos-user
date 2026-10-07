package devicectl

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// mintPairingCloseTimeout bounds how long painting the claim QR waits for an
// in-flight browser-pairing worker to unwind. The paint still happens if the
// wait runs out: the worker was already canceled, and the claim QR is the
// owner's newer intent either way.
const mintPairingCloseTimeout = 10 * time.Second

// MintPairingCloser is the mint-pairing surface the claim QR needs: end the
// pairing session in progress so its code cannot repaint over the claim QR.
// Owned here, by the consumer, so devicectl never imports mintpairing (the same
// seam shape as BrowserSessionCleanup).
type MintPairingCloser interface {
	CloseActivePairing(ctx context.Context) (closed bool, err error)
}

// SetMintPairingCloser injects the closer. Wired once at composition time,
// before commands are served. Unlike SetBrowserSessionCleanup it is not gated
// on a relayer URL: a code can be on screen without one, and the claim QR must
// still take the screen cleanly.
func SetMintPairingCloser(exec Executor, closer MintPairingCloser, logger *zap.Logger) {
	setter, ok := exec.(interface {
		setMintPairingCloser(MintPairingCloser)
	})
	if !ok {
		logger.Warn("Executor does not support mint pairing closer wiring")
		return
	}
	setter.setMintPairingCloser(closer)
}

func (e *executor) setMintPairingCloser(closer MintPairingCloser) {
	e.mintPairingCloser = closer
}

// closeMintPairingForClaimQR ends any browser-pairing session before the claim
// QR is painted. A close failure is logged and does not block the paint: the
// claim QR is the owner's request, and a code left behind is no worse than the
// race this exists to close.
func (e *executor) closeMintPairingForClaimQR() {
	if e.mintPairingCloser == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), mintPairingCloseTimeout)
	defer cancel()
	closed, err := e.mintPairingCloser.CloseActivePairing(ctx)
	if err != nil {
		e.logger.Warn("Could not close mint pairing before painting claim QR", zap.Error(err))
		return
	}
	if closed {
		e.logger.Info("Closed mint pairing so the claim QR keeps the screen")
	}
}
