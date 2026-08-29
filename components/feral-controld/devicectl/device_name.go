package devicectl

import (
	"context"
	"fmt"

	"github.com/feral-file/ffos-user/components/feral-controld/devicename"
)

type deviceNameCommand struct {
	Name string `json:"name"`
}

// setDeviceName stores the owner's name for this unit and re-advertises it.
//
// The write is the whole command: there is nothing to apply to the panel, the
// player, or the network stack. What the name changes is what controllers
// display, which reaches them two ways — the mDNS TXT record (via the
// observer below) and device status, which the poller pushes on its own
// cadence.
//
// An empty or all-whitespace name is a valid request, not an error: clearing
// the field is how an owner undoes a rename, and the unit falls back to
// advertising its serial. That is why this takes no "required" validation —
// devicename.Sanitize already decides what a name can be, and every rejection
// it could make is silent normalization rather than a refusal the owner would
// have to act on.
func (e *executor) setDeviceName(_ context.Context, args []byte) (interface{}, error) {
	var cmd deviceNameCommand
	if err := e.json.Unmarshal(args, &cmd); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	record := &devicename.Record{Name: cmd.Name}
	if err := devicename.Save(e.os, e.json, record); err != nil {
		return nil, err
	}

	// Sanitize ran inside Save, so the observer and the reply both carry the
	// stored form rather than what the caller typed — a controller that echoed
	// its own input would drift from the device on the first name that needed
	// cleaning.
	stored := record.Name

	if e.nameObserver != nil {
		e.nameObserver(stored)
	}

	e.logger.Info("Device name set")

	return map[string]interface{}{
		"deviceName": stored,
	}, nil
}
