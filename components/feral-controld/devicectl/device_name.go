package devicectl

import (
	"context"
	"fmt"

	"github.com/feral-file/ffos-user/components/feral-controld/devicename"
)

type deviceNameCommand struct {
	// Name is a pointer so an ABSENT or null field is distinguishable from an
	// explicit empty string. They mean opposite things: `""` is the supported
	// clear operation, while `{}`, `null`, or `{"name":null}` is a malformed
	// request — and an omitted `request` object reaches this handler as `null`.
	// Decoding both into a bare string would let an incomplete controller
	// request silently erase an owner-set label.
	Name *string `json:"name"`
}

// setDeviceName stores the owner's name for this unit and re-advertises it.
//
// The write is the whole command: there is nothing to apply to the panel, the
// player, or the network stack. What the name changes is what controllers
// display, which reaches them two ways — the mDNS TXT record (via the observer
// below) and device status, which the poller pushes on its own cadence.
//
// An empty name is a valid request, not an error: clearing the field is how an
// owner undoes a rename, and the unit falls back to advertising its serial.
func (e *executor) setDeviceName(_ context.Context, args []byte) (interface{}, error) {
	var cmd deviceNameCommand
	if err := e.json.Unmarshal(args, &cmd); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if cmd.Name == nil {
		return nil, fmt.Errorf("invalid arguments: name is required (send \"\" to clear)")
	}

	// One mutation lock for the whole record, shared with the factory reset's
	// clear. Two concerns, both real without it: concurrent renames stage
	// through the SAME `device-name.json.tmp` path, so one request can rename
	// the other's bytes and then announce a name the disk does not hold; and a
	// rename admitted before a reset staged itself could otherwise land after
	// the reset cleared the record, leaving a rolled-back unit carrying the
	// previous owner's label.
	e.deviceNameMu.Lock()
	defer e.deviceNameMu.Unlock()

	// Re-checked INSIDE the lock. The command router's own reset check happens
	// before dispatch, so on its own it only proves no reset had staged when
	// this request was admitted — the window between that check and this write
	// is exactly what the race needs. factoryReset latches resetStaged before
	// it takes this lock, so a rename that loses the race sees the latch here.
	if e.resetStaged.Load() {
		return nil, fmt.Errorf("factory reset in progress")
	}

	record := &devicename.Record{Name: *cmd.Name}
	if err := devicename.Save(e.os, e.json, record); err != nil {
		return nil, err
	}

	// Sanitize ran inside Save, so the observer and the reply both carry the
	// stored form rather than what the caller typed — a controller that echoed
	// its own input would drift from the device on the first name that needed
	// cleaning.
	stored := record.Name

	// Notified under the lock, so the advertised name cannot be re-registered
	// out of order with respect to a concurrent clear.
	if e.nameObserver != nil {
		e.nameObserver(stored)
	}

	e.logger.Info("Device name set")

	return map[string]interface{}{
		"deviceName": stored,
	}, nil
}

// clearDeviceName drops the stored name and re-advertises the serial.
//
// Split out of factoryReset so the reset's clear takes the same mutation lock
// as a rename, and so the advertised name follows the record: the reset path
// otherwise notified only the claim observer, whose re-registration republishes
// the mediator's cached — now stale — name. On the success path the unit
// reboots into the factory image and none of this matters; this is for the
// rollback path, where a resold frame must not keep announcing the previous
// owner's vocabulary.
//
// Best-effort by design: a name that outlives a failed reset is cosmetic, and
// failing the reset over it would trade a real outcome for a label.
func (e *executor) clearDeviceName() error {
	e.deviceNameMu.Lock()
	defer e.deviceNameMu.Unlock()

	if err := devicename.Clear(e.os); err != nil {
		return err
	}

	if e.nameObserver != nil {
		e.nameObserver("")
	}
	return nil
}
