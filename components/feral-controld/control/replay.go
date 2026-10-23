package control

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// windowSize is the number of counters below hi the window remembers (§6).
const windowSize = 64

var errReplay = errors.New("control: replayed or out-of-window ctr")

// Window is the §6 sliding replay window: Hi is the highest accepted ctr and
// bit i of Bitmap records whether ctr Hi-i was accepted (bit 0 is Hi itself).
// A window tolerates the reordering LAN and relayer delivery produce while
// still refusing every counter it has seen and every counter too old to judge.
//
// Window is a value type: Check never mutates, Accept returns the next value.
// The caller persists the returned window BEFORE treating the request as
// accepted; see Store.Update.
type Window struct {
	Hi     uint64
	Bitmap uint64
}

// Check reports whether ctr would be accepted.
func (w Window) Check(ctr uint64) error {
	if ctr == 0 {
		return fmt.Errorf("%w: ctr must be >= 1", errReplay)
	}
	if ctr > w.Hi {
		return nil
	}
	// ctr <= Hi. Accept only (Hi-64, Hi]; written as ctr+64 > Hi so Hi < 64
	// cannot underflow.
	if ctr+windowSize <= w.Hi {
		return fmt.Errorf("%w: ctr %d is below the window (hi %d)", errReplay, ctr, w.Hi)
	}
	if w.Bitmap&(1<<(w.Hi-ctr)) != 0 {
		return fmt.Errorf("%w: ctr %d already accepted", errReplay, ctr)
	}
	return nil
}

// Accept returns the window with ctr marked. It must only be called after
// Check(ctr) succeeded.
func (w Window) Accept(ctr uint64) Window {
	if ctr > w.Hi {
		shift := ctr - w.Hi
		if shift >= windowSize {
			w.Bitmap = 0
		} else {
			w.Bitmap <<= shift
		}
		w.Bitmap |= 1
		w.Hi = ctr
		return w
	}
	w.Bitmap |= 1 << (w.Hi - ctr)
	return w
}

// valid reports whether the window is internally consistent: an empty window
// has no bits; a non-empty one always has Hi marked and no bit for a counter
// below 1. Anything else on disk is corruption.
func (w Window) valid() bool {
	if w.Hi == 0 {
		return w.Bitmap == 0
	}
	if w.Bitmap&1 == 0 {
		return false
	}
	if w.Hi < windowSize {
		// Bits for counters <= 0 (i >= Hi) must be clear.
		return w.Bitmap>>w.Hi == 0
	}
	return true
}

// bitmapHex is the on-disk form: 16 lowercase hex digits. A JSON number would
// lose bits above 2^53 in every non-Go reader of the file.
func (w Window) bitmapHex() string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], w.Bitmap)
	return hex.EncodeToString(b[:])
}

func parseBitmapHex(s string) (uint64, error) {
	if len(s) != 16 || strings.ToLower(s) != s {
		return 0, errors.New("bitmap must be 16 lowercase hex digits")
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(b), nil
}
