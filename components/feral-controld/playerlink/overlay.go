package playerlink

import (
	"context"
	"errors"
	"time"
)

// SetOverlay applies device-owned narration. Receipt acceptance does not prove
// presentation; consumers report the separately observed host snapshot.
func (c *Client) SetOverlay(ctx context.Context, owner, state string, fields map[string]any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if fields == nil {
		fields = map[string]any{}
	}
	for attempt := 0; attempt < 2; attempt++ {
		// Schedules and other authorized clients can advance playback independently.
		// Narration carries current device intent, not an old artwork's generation.
		if err := c.Call(ctx, "state.get", nil, nil, nil); err != nil {
			return err
		}
		var receipt struct {
			Outcome string `json:"outcome"`
		}
		err := c.Call(ctx, "overlay.set", map[string]any{"owner": owner, "state": state, "fields": fields}, &receipt, nil)
		var remote *RemoteError
		if errors.As(err, &remote) && remote.Code == "stale_generation" && attempt == 0 {
			continue
		}
		// Only a definite pre-execution stale rejection is re-read once. An uncertain
		// transport outcome is never replayed here; the next reconnect/change resyncs.
		if err != nil {
			return err
		}
		if receipt.Outcome != "applied" {
			return errors.New("native player returned an invalid overlay receipt")
		}
		return nil
	}
	return errors.New("native overlay generation could not settle")
}
