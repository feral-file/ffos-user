package offlinecache

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A responsive source must release a cast even while another source is stuck.
// The fake waits for both requests to enter, so this exercises cancellation of
// an in-flight request, not merely skipping the unstarted tail of a playlist.
type castLatencyClient struct {
	blockingProbeHTTPClient
	slowEntered chan struct{}
	once        sync.Once
	requests    atomic.Int32
	active      atomic.Int32
}

func (c *castLatencyClient) Do(req *http.Request) (*http.Response, error) {
	c.requests.Add(1)
	c.active.Add(1)
	defer c.active.Add(-1)
	if req.URL.Path == "/alive" {
		select {
		case <-c.slowEntered:
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	c.once.Do(func() { close(c.slowEntered) })
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestSourceProber_ReachableSourceCancelsSlowPeersAndSkipsTail(t *testing.T) {
	client := &castLatencyClient{slowEntered: make(chan struct{})}
	guard := sourceGuard{resolver: staticResolver{ip: "93.184.216.34"}}
	prober := newSourceProberWith(guard, client)
	sources := make([]string, 500)
	for i := range sources {
		sources[i] = fmt.Sprintf("https://93.184.216.34/slow-%d", i)
	}
	sources[0] = "https://93.184.216.34/alive"
	sources[499] = sources[0] + "#same-source"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan []SourceProbeResult, 1)
	go func() { done <- prober.ProbeSources(ctx, sources) }()
	select {
	case results := <-done:
		require.Len(t, results, len(sources))
		require.Equal(t, ProbeAlive, results[0].Verdict)
		require.Equal(t, results[0], results[499], "duplicates retain the completed witness")
		for _, result := range results[1:499] {
			require.Equal(t, ProbeInconclusive, result.Verdict, "canceled or unstarted is never dead")
		}
		require.Zero(t, client.active.Load(), "return only after in-flight requests release their resources")
		require.LessOrEqual(t, client.requests.Load(), int32(probeConcurrency), "no second wave after a reachable source")
		require.NoError(t, ctx.Err(), "early acceptance must not cancel the caller")
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("a reachable artwork waited for unrelated slow sources")
	}
}

func TestSourceProber_InlineSourceNeedsNoNetworkPreflight(t *testing.T) {
	client := &castLatencyClient{slowEntered: make(chan struct{})}
	guard := sourceGuard{resolver: staticResolver{ip: "93.184.216.34"}}
	results := newSourceProberWith(guard, client).ProbeSources(context.Background(), []string{
		"https://93.184.216.34/slow", "data:text/html,artwork",
	})
	require.Equal(t, ProbeInconclusive, results[0].Verdict)
	require.Equal(t, ProbeInline, results[1].Verdict)
	require.Zero(t, client.requests.Load(), "inline content already rules out an all-dead cast")
}
