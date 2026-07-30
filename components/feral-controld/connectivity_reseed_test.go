package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
)

// TestAwaitConnectivityHandlerReady_WaitsForHydration pins the cross-repo
// re-seed fix: the CDP on-connect callback fires as soon as a page TARGET
// exists — routinely before the player app's React effect installs
// window.handleConnectivityChange — so the re-seed must poll the handler into
// existence instead of firing one evaluate into a still-hydrating page and
// giving up (which left the page ignorant of its connectivity for its whole
// lifetime).
func TestAwaitConnectivityHandlerReady_WaitsForHydration(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	probes := 0
	mockCDP.EXPECT().
		NoLogSend(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, _ := params["expression"].(string)
			assert.True(t, strings.Contains(expr, "handleConnectivityChange"),
				"probe must check the connectivity handler, got: %s", expr)
			probes++
			if probes < 3 {
				return map[string]interface{}{"ready": false}, nil
			}
			return map[string]interface{}{"ready": true}, nil
		}).Times(3)

	ready := awaitConnectivityHandlerReady(context.Background(), mockCDP,
		time.Millisecond, time.Second)
	assert.True(t, ready)
}

// TestAwaitConnectivityHandlerReady_ToleratesProbeErrors: an evaluate dying
// mid-teardown (a Page.reload replacing the document under the poll) must be
// retried, not treated as a verdict.
func TestAwaitConnectivityHandlerReady_ToleratesProbeErrors(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	probes := 0
	mockCDP.EXPECT().
		NoLogSend(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, _ map[string]interface{}) (interface{}, error) {
			probes++
			if probes == 1 {
				return nil, errors.New("Cannot find context with specified id")
			}
			return map[string]interface{}{"ready": true}, nil
		}).Times(2)

	ready := awaitConnectivityHandlerReady(context.Background(), mockCDP,
		time.Millisecond, time.Second)
	assert.True(t, ready)
}

// TestAwaitConnectivityHandlerReady_TimesOutOnDeadPage: a page that never
// installs the handler (broken load) bounds the wait; the caller then skips
// the push with a Warn instead of spinning forever.
func TestAwaitConnectivityHandlerReady_TimesOutOnDeadPage(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().
		NoLogSend(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(map[string]interface{}{"ready": false}, nil).
		AnyTimes()

	ready := awaitConnectivityHandlerReady(context.Background(), mockCDP,
		time.Millisecond, 5*time.Millisecond)
	assert.False(t, ready)
}

// TestAwaitConnectivityHandlerReady_ObservesShutdown: daemon shutdown must
// stop the poll between ticks rather than waiting out the full timeout.
func TestAwaitConnectivityHandlerReady_ObservesShutdown(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().
		NoLogSend(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, _ map[string]interface{}) (interface{}, error) {
			cancel() // shutdown lands while the page is still not ready
			return map[string]interface{}{"ready": false}, nil
		}).Times(1)

	done := make(chan bool, 1)
	go func() {
		done <- awaitConnectivityHandlerReady(ctx, mockCDP, time.Hour, time.Hour)
	}()
	select {
	case ready := <-done:
		assert.False(t, ready)
	case <-time.After(5 * time.Second):
		t.Fatal("poll must exit on ctx cancellation, not sleep out the interval")
	}
}
