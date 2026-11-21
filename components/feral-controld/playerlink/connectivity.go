package playerlink

import (
	"context"
	"errors"
	"time"
)

// ConnectivityPlayer exposes host connectivity, independently of artwork/page
// readiness. The mediator retains the authoritative device observation.
type ConnectivityPlayer interface {
	SetConnectivity(context.Context, bool) error
	Watch(context.Context, func(SnapshotDelivery)) error
}

// SetConnectivity applies a current device observation. Only a definite stale
// rejection is retried here; an uncertain transport outcome goes to the owner,
// which reconciles the latest level rather than replaying this old request.
func (c *Client) SetConnectivity(ctx context.Context, online bool) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		if err := c.Call(ctx, "state.get", nil, nil, nil); err != nil {
			return err
		}
		var receipt struct {
			Outcome string `json:"outcome"`
		}
		params := map[string]any{"online": online}
		if c.Sources != nil {
			params["sources"] = c.Sources
		}
		err := c.Call(ctx, "connectivity.set", params, &receipt, nil)
		var remote *RemoteError
		if errors.As(err, &remote) && remote.Code == "stale_generation" && attempt == 0 {
			continue
		}
		if err != nil {
			return err
		}
		if receipt.Outcome != "applied" {
			return errors.New("native player returned an invalid connectivity receipt")
		}
		return nil
	}
	return errors.New("native connectivity generation could not settle")
}
