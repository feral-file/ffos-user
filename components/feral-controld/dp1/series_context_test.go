package dp1

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

type cancelTransport func(*http.Request) (*http.Response, error)

func (f cancelTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestSeriesRequestHonorsOperationCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	d := &dp1{httpClient: wrapper.NewHTTPClientFrom(&http.Client{Transport: cancelTransport(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})})}
	done := make(chan error, 1)
	go func() { _, _, err := d.seriesEditions(ctx, "playlist", itemAsSeries()); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request never started")
	}
	cancel()
	select {
	case err := <-done:
		require.True(t, errors.Is(err, context.Canceled))
	case <-time.After(time.Second):
		t.Fatal("catalog request ignored cancellation")
	}
}
func itemAsSeries() dp1playlist.PlaylistItem { return dp1playlist.PlaylistItem{ID: "item"} }
func TestInteractiveSeriesBudgetDoesNotChangeBackgroundBudget(t *testing.T) {
	ctx := context.Background()
	require.Equal(t, seriesExpansionBudget, seriesWalkBudget(ctx))
	require.Equal(t, 2*time.Second, seriesWalkBudget(WithInteractiveSeriesResolution(ctx)))
}
