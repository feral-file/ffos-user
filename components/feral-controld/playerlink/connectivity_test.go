package playerlink

import (
	"context"
	"testing"
)

func TestCPPConnectivityWithoutRenderer(t *testing.T) {
	c := New(startCPPHost(t))
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	for _, online := range []bool{false, true, false} {
		if err := c.SetConnectivity(ctx, online); err != nil {
			t.Fatal(err)
		}
		var state struct {
			Online bool `json:"online"`
		}
		if err := c.Call(ctx, "state.get", nil, &state, nil); err != nil {
			t.Fatal(err)
		}
		if state.Online != online {
			t.Fatalf("connectivity=%v, want %v", state.Online, online)
		}
	}
}
