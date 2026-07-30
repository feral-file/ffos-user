package mediator_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/feral-file/godbus"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/mdns"
	"github.com/feral-file/ffos-user/components/feral-controld/mediator"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/playersession"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
)

// stubSessionCDP is a minimal playersession.CDPSender double that always
// reports the player command handler installed, so a playersession.Session
// built on it reaches StageHandler readiness on the first probe — the
// connectivity tests below exercise the mediator's OWN cache/coalescing/push
// logic, not playersession's barrier polling.
type stubSessionCDP struct{}

func (stubSessionCDP) Initialized() bool { return true }
func (stubSessionCDP) NoLogSend(method string, params map[string]interface{}) (interface{}, error) {
	if method == cdp.METHOD_EVALUATE {
		return map[string]any{"ready": true}, nil
	}
	return map[string]any{}, nil
}

// wireConnectivitySession wires a playersession.Session into ts.mediator,
// exactly mirroring production wiring order (SetSession, THEN the session's
// first generation bump — main.go calls SetSession once at composition time,
// before CDP.Start ever bumps a generation). Because SetSession registers the
// "connectivity" reconciler, that first bump's generation-ready worker runs
// it immediately — with nothing known yet, that is ONE single-flight cold
// probe (§3.1), which this helper expects and drains before returning so
// subtests' OWN mockCDP/mockDbus expectations start from a clean slate.
func wireConnectivitySession(t *testing.T, ts *testSetup) *playersession.Session {
	t.Helper()
	ts.mockDbus.EXPECT().
		Call(gomock.Any(), dbus.MONITORD_NAME, dbus.MONITORD_PATH, dbus.MONITORD_INTERFACE, dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS, true).
		Return([]interface{}{false}, nil).
		Times(1)
	// The cold probe resolves a level, and the reconciler push then SENDS it
	// to the player unconditionally — absorb that push too so subtests' own
	// mockCDP.Send expectations start clean.
	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, map[string]interface{}{
			"expression": "window.handleConnectivityChange(false)",
		}).
		Return(map[string]interface{}{"result": "ok"}, nil).
		Times(1)

	session := playersession.New(context.Background(), stubSessionCDP{}, nil, nil, zap.NewNop())
	ts.mediator.SetSession(session)
	session.OnConnect()
	ts.waitForConnectivityPush(t)
	return session
}

// drainConnectivityPushes drains every completed connectivity push attempt
// until connPushDone stays quiet for quietFor, for tests that race multiple
// triggers (the edge handler, the reconciler) and need to observe the FULL
// coalesced sequence settle before asserting on the last delivered value.
func drainConnectivityPushes(t *testing.T, ts *testSetup, quietFor time.Duration) {
	t.Helper()
	for {
		select {
		case <-ts.connPushDone:
		case <-time.After(quietFor):
			return
		}
	}
}

type testSetup struct {
	ctrl               *gomock.Controller
	ctx                context.Context
	mockRelayer        *mocks.MockRelayer
	mockDbus           *mocks.MockDBus
	mockCDP            *mocks.MockCDP
	mockExecutor       *mocks.MockExecutor
	mockCommandHandler *mocks.MockCommandHandler
	mockRefresher      *mocks.MockRefresher
	mockJSON           *mocks.MockJSON
	mediator           mediator.Mediator
	logger             *zap.Logger

	// connPushDone fires once per completed connectivity push attempt (see
	// mediator.SetConnectivityPushHook) — the async completion seam the
	// connectivity tests wait on instead of asserting the CDP send
	// synchronously inline with the DBus signal handler call.
	connPushDone chan struct{}
}

// waitForConnectivityPush blocks until at least one connectivity push attempt
// has completed (see connPushDone), or fails the test. Call it after invoking
// the connectivity_change handler and before asserting on mockCDP.
func (ts *testSetup) waitForConnectivityPush(t *testing.T) {
	t.Helper()
	select {
	case <-ts.connPushDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the async connectivity push to complete")
	}
}

func setup(t *testing.T) *testSetup {
	ctrl := gomock.NewController(t)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	mockRelayer := mocks.NewMockRelayer(ctrl)
	mockDbus := mocks.NewMockDBus(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockExecutor := mocks.NewMockExecutor(ctrl)
	mockCommandHandler := mocks.NewMockCommandHandler(ctrl)
	mockRefresher := mocks.NewMockRefresher(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)

	med := mediator.New(
		ctx,
		mockRelayer,
		mockDbus,
		mockCDP,
		mockCommandHandler,
		mockExecutor,
		mockRefresher,
		mockJSON,
		logger,
	)

	// No session is wired by default: m.session stays nil, so pushConnectivity
	// no-ops and every test that does not care about connectivity keeps its
	// original, unmodified behavior. The two connectivity-focused test
	// functions below opt in via wireConnectivitySession.
	connPushDone := make(chan struct{}, 8)
	med.SetConnectivityPushHook(func() {
		select {
		case connPushDone <- struct{}{}:
		default:
		}
	})

	return &testSetup{
		ctrl:               ctrl,
		ctx:                ctx,
		mockRelayer:        mockRelayer,
		mockDbus:           mockDbus,
		mockCDP:            mockCDP,
		mockExecutor:       mockExecutor,
		mockCommandHandler: mockCommandHandler,
		mockRefresher:      mockRefresher,
		mockJSON:           mockJSON,
		mediator:           med,
		logger:             logger,
		connPushDone:       connPushDone,
	}
}

func (ts *testSetup) teardown() {
	ts.ctrl.Finish()
}

func TestMediator_StartStop(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Expect Start to register handlers
	ts.mockDbus.EXPECT().OnBusSignal(gomock.Any()).Times(1)
	ts.mockRelayer.EXPECT().OnRelayerMessage(gomock.Any()).Times(1)

	// Expect Stop to remove handlers
	ts.mockRelayer.EXPECT().RemoveRelayerMessage(gomock.Any()).Times(1)
	ts.mockDbus.EXPECT().RemoveBusSignal(gomock.Any()).Times(1)

	// Test Start
	ts.mediator.Start()

	// Test Stop
	ts.mediator.Stop()
}

func TestMediator_HandleDBusSignal_SysMetrics(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup) (godbus.DBusPayload, error)
	}{
		{
			name: "valid sysmetrics payload",
			setupFunc: func(ts *testSetup) (godbus.DBusPayload, error) {
				metricsData := []byte(`{"cpu": 50, "memory": 75}`)

				ts.mockExecutor.EXPECT().
					SaveLastSysMetrics(metricsData).
					Times(1)

				// A connected relayer short-circuits the heartbeat reconcile.
				ts.mockRelayer.EXPECT().IsConnected().Return(true).AnyTimes()

				payload := godbus.DBusPayload{
					Member: dbus.MONITORD_EVENT_SYSMETRICS,
					Body:   []interface{}{metricsData},
				}

				return payload, nil
			},
		},
		{
			name: "invalid number of arguments",
			setupFunc: func(ts *testSetup) (godbus.DBusPayload, error) {
				payload := godbus.DBusPayload{
					Member: dbus.MONITORD_EVENT_SYSMETRICS,
					Body:   []interface{}{[]byte("data"), "extra"},
				}

				return payload, errors.New("invalid number of arguments")
			},
		},
		{
			name: "invalid body type",
			setupFunc: func(ts *testSetup) (godbus.DBusPayload, error) {
				payload := godbus.DBusPayload{
					Member: dbus.MONITORD_EVENT_SYSMETRICS,
					Body:   []interface{}{"not-bytes"},
				}

				return payload, errors.New("invalid body type")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			payload, expectedError := tt.setupFunc(ts)

			// Register handler
			var capturedHandler func(context.Context, godbus.DBusPayload) ([]interface{}, error)
			ts.mockDbus.EXPECT().
				OnBusSignal(gomock.Any()).
				DoAndReturn(func(handler func(context.Context, godbus.DBusPayload) ([]interface{}, error)) {
					capturedHandler = handler
				}).Times(1)

			// Expect OnRelayerMessage to be called
			ts.mockRelayer.EXPECT().
				OnRelayerMessage(gomock.Any()).
				Times(1)

			ts.mediator.Start()

			// Call handler directly
			result, err := capturedHandler(ts.ctx, payload)

			// Verify
			if expectedError != nil {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), expectedError.Error())
			} else {
				assert.NoError(t, err)
				assert.Nil(t, result)
			}
		})
	}
}

func TestMediator_HandleDBusSignal_ConnectivityChange(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup) (godbus.DBusPayload, error)
	}{
		{
			name: "connectivity gained - relayer not connected",
			setupFunc: func(ts *testSetup) (godbus.DBusPayload, error) {
				// Expect CDP send to be called
				ts.mockCDP.EXPECT().
					Send(cdp.METHOD_EVALUATE, map[string]interface{}{
						"expression": "window.handleConnectivityChange(true)",
					}).
					Return(map[string]interface{}{"result": "ok"}, nil).
					Times(1)

				// Expect IsConnected to be called twice (once for logging, once for condition check)
				ts.mockRelayer.EXPECT().
					IsConnected().
					Return(false).
					Times(2)

				// Expect RetryableConnect to be called
				ts.mockRelayer.EXPECT().
					RetryableConnect(gomock.Any()).
					Return(nil).
					Times(1)

				// Expect payload to be sent
				payload := godbus.DBusPayload{
					Member: dbus.MONITORD_EVENT_CONNECTIVITY_CHANGE,
					Body:   []interface{}{true},
				}

				return payload, nil
			},
		},
		{
			name: "connectivity gained - relayer already connected",
			setupFunc: func(ts *testSetup) (godbus.DBusPayload, error) {
				// Expect CDP send to be called
				ts.mockCDP.EXPECT().
					Send(cdp.METHOD_EVALUATE, map[string]interface{}{
						"expression": "window.handleConnectivityChange(true)",
					}).
					Return(map[string]interface{}{"result": "ok"}, nil).
					Times(1)

				// Expect IsConnected to be called twice (once for logging, once for condition check)
				ts.mockRelayer.EXPECT().
					IsConnected().
					Return(true).
					Times(2)

				payload := godbus.DBusPayload{
					Member: dbus.MONITORD_EVENT_CONNECTIVITY_CHANGE,
					Body:   []interface{}{true},
				}

				return payload, nil
			},
		},
		{
			name: "connectivity lost",
			setupFunc: func(ts *testSetup) (godbus.DBusPayload, error) {
				// Expect CDP send to be called
				ts.mockCDP.EXPECT().
					Send(cdp.METHOD_EVALUATE, map[string]interface{}{
						"expression": "window.handleConnectivityChange(false)",
					}).
					Return(map[string]interface{}{"result": "ok"}, nil).
					Times(1)

				// Expect IsConnected to be called once for logging (condition short-circuits since connected=false)
				ts.mockRelayer.EXPECT().
					IsConnected().
					Return(false).
					Times(1)

				payload := godbus.DBusPayload{
					Member: dbus.MONITORD_EVENT_CONNECTIVITY_CHANGE,
					Body:   []interface{}{false},
				}

				return payload, nil
			},
		},
		{
			name: "CDP send error",
			setupFunc: func(ts *testSetup) (godbus.DBusPayload, error) {
				cdpError := errors.New("CDP send failed")

				// Expect CDP send to be called and return error
				ts.mockCDP.EXPECT().
					Send(cdp.METHOD_EVALUATE, map[string]interface{}{
						"expression": "window.handleConnectivityChange(true)",
					}).
					Return(nil, cdpError).
					Times(1)

				// Expect IsConnected to be called twice (once for logging, once for condition check)
				ts.mockRelayer.EXPECT().
					IsConnected().
					Return(false).
					Times(2)

				// Expect RetryableConnect to be called
				ts.mockRelayer.EXPECT().
					RetryableConnect(gomock.Any()).
					Return(nil).
					Times(1)

				payload := godbus.DBusPayload{
					Member: dbus.MONITORD_EVENT_CONNECTIVITY_CHANGE,
					Body:   []interface{}{true},
				}

				return payload, nil // Should not return error despite CDP failure
			},
		},
		{
			name: "invalid body type",
			setupFunc: func(ts *testSetup) (godbus.DBusPayload, error) {
				payload := godbus.DBusPayload{
					Member: dbus.MONITORD_EVENT_CONNECTIVITY_CHANGE,
					Body:   []interface{}{"not-bool"},
				}

				return payload, errors.New("invalid body type")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()
			wireConnectivitySession(t, ts)

			payload, expectedError := tt.setupFunc(ts)

			// Register handler
			var capturedHandler func(context.Context, godbus.DBusPayload) ([]interface{}, error)
			ts.mockDbus.EXPECT().
				OnBusSignal(gomock.Any()).
				DoAndReturn(func(handler func(context.Context, godbus.DBusPayload) ([]interface{}, error)) {
					capturedHandler = handler
				}).Times(1)

			// Expect OnRelayerMessage to be called
			ts.mockRelayer.EXPECT().
				OnRelayerMessage(gomock.Any()).
				Times(1)

			ts.mediator.Start()

			// Call handler directly
			result, err := capturedHandler(ts.ctx, payload)

			// Verify
			if expectedError != nil {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), expectedError.Error())
			} else {
				assert.NoError(t, err)
				assert.Nil(t, result)
				// The CDP push now runs on the async connectivity worker
				// (design doc section 3.1); wait for it before ctrl.Finish()
				// checks the mockCDP.Send expectation.
				ts.waitForConnectivityPush(t)
			}
		})
	}
}

// TestMediator_ConnectivityEdgeRacesReconciler_LastDeliveredValueIsCacheNewest
// pins M6: the connectivity reconciler (run by the session on every
// generation-ready) and the edge-triggered connectivity_change handler both
// enqueue through the SAME single worker rather than one of them calling
// pushConnectivity directly — so racing them concurrently must never produce
// a stale overwrite; the last value actually delivered to the player must be
// the cache's newest, however the two triggers interleave.
func TestMediator_ConnectivityEdgeRacesReconciler_LastDeliveredValueIsCacheNewest(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	session := wireConnectivitySession(t, ts) // cache: known=true, level=false

	var mu sync.Mutex
	var sentLevels []bool
	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, _ := params["expression"].(string)
			mu.Lock()
			sentLevels = append(sentLevels, strings.Contains(expr, "true"))
			mu.Unlock()
			return map[string]interface{}{"result": "ok"}, nil
		}).
		AnyTimes()
	ts.mockRelayer.EXPECT().IsConnected().Return(false).AnyTimes()
	ts.mockRelayer.EXPECT().RetryableConnect(gomock.Any()).Return(nil).AnyTimes()

	var capturedHandler func(context.Context, godbus.DBusPayload) ([]interface{}, error)
	ts.mockDbus.EXPECT().
		OnBusSignal(gomock.Any()).
		DoAndReturn(func(handler func(context.Context, godbus.DBusPayload) ([]interface{}, error)) {
			capturedHandler = handler
		}).Times(1)
	ts.mockRelayer.EXPECT().OnRelayerMessage(gomock.Any()).Times(1)
	ts.mediator.Start()

	// Race the edge (connectivity_change: connected=true, the NEWEST value)
	// against the reconciler (session.OnConnect() bumps the generation,
	// re-running the registered "connectivity" reconciler) firing at
	// approximately the same time.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = capturedHandler(ts.ctx, godbus.DBusPayload{
			Member: dbus.MONITORD_EVENT_CONNECTIVITY_CHANGE,
			Body:   []interface{}{true},
		})
	}()
	go func() {
		defer wg.Done()
		session.OnConnect()
	}()
	wg.Wait()

	drainConnectivityPushes(t, ts, 300*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, sentLevels, "expected at least one connectivity push from the race")
	assert.True(t, sentLevels[len(sentLevels)-1],
		"the last delivered value must be the cache's newest (connected=true), got %v", sentLevels)
}

// stubLinkState is a controllable status.LinkState for tests. It is mutable so a
// test can advertise one link state at InitializeMDNS time (to suppress the
// init-time Start) and another during the connectivity-change event.
type stubLinkState struct{ hasLink bool }

func (s *stubLinkState) HasLink(context.Context) bool { return s.hasLink }

func TestMediator_HandleDBusSignal_ConnectivityChange_WithMDNS(t *testing.T) {
	deviceInfo := mdns.DeviceInfo{ID: "test-device", Name: "Test Device", Port: 1111}

	tests := []struct {
		name string
		// linkUp is the link state during the connectivity-change event.
		linkUp    bool
		setupFunc func(*testSetup, *mocks.MockAdvertiser) godbus.DBusPayload
	}{
		{
			// Internet restored with a link present: tear down stale sockets and
			// re-register on the fresh interface set.
			name:   "internet gained, link up - re-registers advertiser",
			linkUp: true,
			setupFunc: func(ts *testSetup, mockAdvertiser *mocks.MockAdvertiser) godbus.DBusPayload {
				ts.mockCDP.EXPECT().
					Send(cdp.METHOD_EVALUATE, map[string]interface{}{
						"expression": "window.handleConnectivityChange(true)",
					}).
					Return(map[string]interface{}{"result": "ok"}, nil).
					Times(1)

				ts.mockRelayer.EXPECT().IsConnected().Return(true).Times(2)

				mockAdvertiser.EXPECT().Stop().Times(1)
				mockAdvertiser.EXPECT().Start(deviceInfo).Return(nil).Times(1)

				return godbus.DBusPayload{
					Member: dbus.MONITORD_EVENT_CONNECTIVITY_CHANGE,
					Body:   []interface{}{true},
				}
			},
		},
		{
			// The core LAN-recovery regression: internet reachability dropped but
			// a LAN link remains, so the advertiser MUST stay up (re-register),
			// not tear down. Future stories must not re-couple this to internet.
			name:   "internet lost but link up - advertiser stays up",
			linkUp: true,
			setupFunc: func(ts *testSetup, mockAdvertiser *mocks.MockAdvertiser) godbus.DBusPayload {
				ts.mockCDP.EXPECT().
					Send(cdp.METHOD_EVALUATE, map[string]interface{}{
						"expression": "window.handleConnectivityChange(false)",
					}).
					Return(map[string]interface{}{"result": "ok"}, nil).
					Times(1)

				ts.mockRelayer.EXPECT().IsConnected().Return(false).Times(1)

				mockAdvertiser.EXPECT().Stop().Times(1)
				mockAdvertiser.EXPECT().Start(deviceInfo).Return(nil).Times(1)

				return godbus.DBusPayload{
					Member: dbus.MONITORD_EVENT_CONNECTIVITY_CHANGE,
					Body:   []interface{}{false},
				}
			},
		},
		{
			// No link at all: stop and stay down.
			name:   "internet lost and link down - advertiser stays down",
			linkUp: false,
			setupFunc: func(ts *testSetup, mockAdvertiser *mocks.MockAdvertiser) godbus.DBusPayload {
				ts.mockCDP.EXPECT().
					Send(cdp.METHOD_EVALUATE, map[string]interface{}{
						"expression": "window.handleConnectivityChange(false)",
					}).
					Return(map[string]interface{}{"result": "ok"}, nil).
					Times(1)

				ts.mockRelayer.EXPECT().IsConnected().Return(false).Times(1)

				mockAdvertiser.EXPECT().Stop().Times(1)

				return godbus.DBusPayload{
					Member: dbus.MONITORD_EVENT_CONNECTIVITY_CHANGE,
					Body:   []interface{}{false},
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()
			wireConnectivitySession(t, ts)

			mockAdvertiser := mocks.NewMockAdvertiser(ts.ctrl)
			payload := tt.setupFunc(ts, mockAdvertiser)

			var capturedHandler func(context.Context, godbus.DBusPayload) ([]interface{}, error)
			ts.mockDbus.EXPECT().
				OnBusSignal(gomock.Any()).
				DoAndReturn(func(handler func(context.Context, godbus.DBusPayload) ([]interface{}, error)) {
					capturedHandler = handler
				}).Times(1)

			ts.mockRelayer.EXPECT().OnRelayerMessage(gomock.Any()).Times(1)

			// Initialize with link down so InitializeMDNS does not Start at init;
			// then set the link state that the connectivity-change event sees.
			link := &stubLinkState{hasLink: false}
			ts.mediator.Start()
			ts.mediator.InitializeMDNS(mockAdvertiser, deviceInfo, link)
			link.hasLink = tt.linkUp

			result, err := capturedHandler(ts.ctx, payload)

			assert.NoError(t, err)
			assert.Nil(t, result)
			// The CDP push now runs on the async connectivity worker (design
			// doc section 3.1); wait for it before ctrl.Finish() checks the
			// mockCDP.Send expectation.
			ts.waitForConnectivityPush(t)
		})
	}
}

// TestMediator_InitializeMDNS_LinkGated verifies mDNS starts at init only when a
// link is present, and is independent of internet reachability.
func TestMediator_InitializeMDNS_LinkGated(t *testing.T) {
	deviceInfo := mdns.DeviceInfo{ID: "test-device", Name: "Test Device", Port: 1111}

	t.Run("link up - starts at init", func(t *testing.T) {
		ts := setup(t)
		defer ts.teardown()

		mockAdvertiser := mocks.NewMockAdvertiser(ts.ctrl)
		mockAdvertiser.EXPECT().Start(deviceInfo).Return(nil).Times(1)

		ts.mediator.InitializeMDNS(mockAdvertiser, deviceInfo, &stubLinkState{hasLink: true})
	})

	t.Run("link down - does not start at init", func(t *testing.T) {
		ts := setup(t)
		defer ts.teardown()

		mockAdvertiser := mocks.NewMockAdvertiser(ts.ctrl)
		// No Start expectation: a satisfied controller asserts Start is never called.

		ts.mediator.InitializeMDNS(mockAdvertiser, deviceInfo, &stubLinkState{hasLink: false})
	})
}

// TestMediator_SetClaimed verifies the claim-state re-register path: a
// false->true transition re-registers the advertiser with the updated TXT
// (claimed), while a no-op transition does not churn it.
func TestMediator_SetClaimed(t *testing.T) {
	deviceInfo := mdns.DeviceInfo{ID: "test-device", Name: "Test Device", Port: 1111}
	claimedInfo := deviceInfo
	claimedInfo.Claimed = true

	t.Run("claim flip re-registers with updated TXT", func(t *testing.T) {
		ts := setup(t)
		defer ts.teardown()

		mockAdvertiser := mocks.NewMockAdvertiser(ts.ctrl)
		// Start at init (link up), then Stop+Start for the claim flip.
		mockAdvertiser.EXPECT().Start(deviceInfo).Return(nil).Times(1)
		mockAdvertiser.EXPECT().Stop().Times(1)
		mockAdvertiser.EXPECT().Start(claimedInfo).Return(nil).Times(1)

		ts.mediator.InitializeMDNS(mockAdvertiser, deviceInfo, &stubLinkState{hasLink: true})
		ts.mediator.SetClaimed(true)
	})

	t.Run("no-op transition does not re-register", func(t *testing.T) {
		ts := setup(t)
		defer ts.teardown()

		mockAdvertiser := mocks.NewMockAdvertiser(ts.ctrl)
		mockAdvertiser.EXPECT().Start(deviceInfo).Return(nil).Times(1)
		// Already unclaimed; SetClaimed(false) must not touch the advertiser.

		ts.mediator.InitializeMDNS(mockAdvertiser, deviceInfo, &stubLinkState{hasLink: true})
		ts.mediator.SetClaimed(false)
	})
}

// TestMediator_SysMetricsSelfHealsMDNS is the F1 regression: a LAN link that
// comes up while the internet stays down never fires connectivity_change, so the
// advertiser would otherwise never start and the recovery hub would be
// undiscoverable. The periodic SYSMETRICS reconcile must start it once a link
// appears, and must not churn a healthy advertiser.
func TestMediator_SysMetricsSelfHealsMDNS(t *testing.T) {
	deviceInfo := mdns.DeviceInfo{ID: "test-device", Name: "Test Device", Port: 1111}
	metricsData := []byte(`{"cpu":1}`)

	sysMetrics := godbus.DBusPayload{
		Member: dbus.MONITORD_EVENT_SYSMETRICS,
		Body:   []interface{}{metricsData},
	}

	captureHandler := func(ts *testSetup) *func(context.Context, godbus.DBusPayload) ([]interface{}, error) {
		var h func(context.Context, godbus.DBusPayload) ([]interface{}, error)
		ts.mockDbus.EXPECT().
			OnBusSignal(gomock.Any()).
			DoAndReturn(func(handler func(context.Context, godbus.DBusPayload) ([]interface{}, error)) {
				h = handler
			}).Times(1)
		ts.mockRelayer.EXPECT().OnRelayerMessage(gomock.Any()).Times(1)
		return &h
	}

	t.Run("link up without internet starts the advertiser via SYSMETRICS", func(t *testing.T) {
		ts := setup(t)
		defer ts.teardown()

		mockAdvertiser := mocks.NewMockAdvertiser(ts.ctrl)
		// Init link-down → not started. connectivity_change never fires (internet
		// stays down), so this Start can only come from the SYSMETRICS reconcile.
		mockAdvertiser.EXPECT().Start(deviceInfo).Return(nil).Times(1)
		ts.mockExecutor.EXPECT().SaveLastSysMetrics(metricsData).Times(1)
		// A connected relayer short-circuits the heartbeat's relayer reconcile.
		ts.mockRelayer.EXPECT().IsConnected().Return(true).AnyTimes()

		handler := captureHandler(ts)
		link := &stubLinkState{hasLink: false}
		ts.mediator.Start()
		ts.mediator.InitializeMDNS(mockAdvertiser, deviceInfo, link)

		// LAN link appears; internet still down (no connectivity_change).
		link.hasLink = true
		_, err := (*handler)(ts.ctx, sysMetrics)
		assert.NoError(t, err)
	})

	t.Run("reconcile does not churn an already-advertising device", func(t *testing.T) {
		ts := setup(t)
		defer ts.teardown()

		mockAdvertiser := mocks.NewMockAdvertiser(ts.ctrl)
		// Init link-up → started once; a later SYSMETRICS with link still up must
		// NOT Start again.
		mockAdvertiser.EXPECT().Start(deviceInfo).Return(nil).Times(1)
		ts.mockExecutor.EXPECT().SaveLastSysMetrics(metricsData).Times(1)
		// A connected relayer short-circuits the heartbeat's relayer reconcile.
		ts.mockRelayer.EXPECT().IsConnected().Return(true).AnyTimes()

		handler := captureHandler(ts)
		ts.mediator.Start()
		ts.mediator.InitializeMDNS(mockAdvertiser, deviceInfo, &stubLinkState{hasLink: true})

		_, err := (*handler)(ts.ctx, sysMetrics)
		assert.NoError(t, err)
	})
}

// TestMediator_SysMetricsReconcilesRelayer is the regression test for the
// stranded-relayer recovery gap: the startup gate's connectivity snapshot can
// be wrong-and-final (evaluated while D-Bus was down on an already-online
// network), and sys-monitord emits connectivity_change only on TRANSITIONS —
// so nothing edge-triggered will ever connect the relayer, no topic is
// assigned, and auto-claim strands. The periodic SYSMETRICS heartbeat must
// reconcile the relayer connection without any connectivity_change ever being
// delivered.
func TestMediator_SysMetricsReconcilesRelayer(t *testing.T) {
	metricsData := []byte(`{"cpu":1}`)
	sysMetrics := godbus.DBusPayload{
		Member: dbus.MONITORD_EVENT_SYSMETRICS,
		Body:   []interface{}{metricsData},
	}

	captureHandler := func(ts *testSetup) *func(context.Context, godbus.DBusPayload) ([]interface{}, error) {
		var h func(context.Context, godbus.DBusPayload) ([]interface{}, error)
		ts.mockDbus.EXPECT().
			OnBusSignal(gomock.Any()).
			DoAndReturn(func(handler func(context.Context, godbus.DBusPayload) ([]interface{}, error)) {
				h = handler
			}).Times(1)
		ts.mockRelayer.EXPECT().OnRelayerMessage(gomock.Any()).Times(1)
		return &h
	}
	expectConnectivity := func(ts *testSetup) *gomock.Call {
		return ts.mockDbus.EXPECT().Call(
			gomock.Any(),
			dbus.MONITORD_NAME,
			dbus.MONITORD_PATH,
			dbus.MONITORD_INTERFACE,
			dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS,
			false,
		)
	}

	t.Run("already-online device with no connectivity_change connects the relayer", func(t *testing.T) {
		ts := setup(t)
		defer ts.teardown()

		ts.mockExecutor.EXPECT().SaveLastSysMetrics(metricsData).Times(1)
		ts.mockRelayer.EXPECT().IsConnected().Return(false).Times(1)
		expectConnectivity(ts).Return([]interface{}{true}, nil).Times(1)
		ts.mockRelayer.EXPECT().RetryableConnect(gomock.Any()).Return(nil).Times(1)

		handler := captureHandler(ts)
		ts.mediator.Start()
		_, err := (*handler)(ts.ctx, sysMetrics)
		assert.NoError(t, err)
	})

	t.Run("connected relayer short-circuits without D-Bus traffic", func(t *testing.T) {
		ts := setup(t)
		defer ts.teardown()

		ts.mockExecutor.EXPECT().SaveLastSysMetrics(metricsData).Times(1)
		// No Call and no RetryableConnect expectations: any D-Bus query or
		// connect attempt fails the test.
		ts.mockRelayer.EXPECT().IsConnected().Return(true).Times(1)

		handler := captureHandler(ts)
		ts.mediator.Start()
		_, err := (*handler)(ts.ctx, sysMetrics)
		assert.NoError(t, err)
	})

	t.Run("unanswerable connectivity query is assumed offline", func(t *testing.T) {
		ts := setup(t)
		defer ts.teardown()

		ts.mockExecutor.EXPECT().SaveLastSysMetrics(metricsData).Times(1)
		ts.mockRelayer.EXPECT().IsConnected().Return(false).Times(1)
		expectConnectivity(ts).Return(nil, errors.New("dbus: client not started")).Times(1)
		// No RetryableConnect: assumed offline.

		handler := captureHandler(ts)
		ts.mediator.Start()
		_, err := (*handler)(ts.ctx, sysMetrics)
		assert.NoError(t, err)
	})

	t.Run("offline device stays disconnected", func(t *testing.T) {
		ts := setup(t)
		defer ts.teardown()

		ts.mockExecutor.EXPECT().SaveLastSysMetrics(metricsData).Times(1)
		ts.mockRelayer.EXPECT().IsConnected().Return(false).Times(1)
		expectConnectivity(ts).Return([]interface{}{false}, nil).Times(1)

		handler := captureHandler(ts)
		ts.mediator.Start()
		_, err := (*handler)(ts.ctx, sysMetrics)
		assert.NoError(t, err)
	})

	t.Run("overlapping heartbeats collapse to one in-flight connect", func(t *testing.T) {
		ts := setup(t)
		defer ts.teardown()

		ts.mockExecutor.EXPECT().SaveLastSysMetrics(metricsData).Times(2)
		ts.mockRelayer.EXPECT().IsConnected().Return(false).AnyTimes()
		// Times(1) on the query and the connect IS the single-flight
		// assertion: the second heartbeat lands while the first is still
		// inside RetryableConnect and must do nothing.
		expectConnectivity(ts).Return([]interface{}{true}, nil).Times(1)
		entered := make(chan struct{})
		release := make(chan struct{})
		ts.mockRelayer.EXPECT().
			RetryableConnect(gomock.Any()).
			DoAndReturn(func(context.Context) error {
				close(entered)
				<-release
				return nil
			}).Times(1)

		handler := captureHandler(ts)
		ts.mediator.Start()

		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = (*handler)(ts.ctx, sysMetrics)
		}()
		<-entered
		_, err := (*handler)(ts.ctx, sysMetrics)
		assert.NoError(t, err)
		close(release)
		<-done
	})
}

func TestMediator_HandleDBusSignal_ACKAndUnknown(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Test unknown signal - a member the mediator does not handle.
	payload := godbus.DBusPayload{
		Member: godbus.Member("unknown_signal"),
		Body:   []interface{}{},
	}

	// Register handler
	var capturedHandler func(context.Context, godbus.DBusPayload) ([]interface{}, error)
	ts.mockDbus.EXPECT().
		OnBusSignal(gomock.Any()).
		DoAndReturn(func(handler func(context.Context, godbus.DBusPayload) ([]interface{}, error)) {
			capturedHandler = handler
		}).Times(1)

	// Expect OnRelayerMessage to be called
	ts.mockRelayer.EXPECT().
		OnRelayerMessage(gomock.Any()).
		Times(1)

	ts.mediator.Start()

	// Call handler directly
	result, err := capturedHandler(ts.ctx, payload)

	// Verify - unknown signals should return nil without error
	assert.NoError(t, err)
	assert.Nil(t, result)
}

// TestMediator_TopicAssignmentFiresObserver: the empty->non-empty system-topic
// persist must fire the topic observer (the factory-fresh re-trigger for the
// auto-claim flow, whose bounded topic wait may already have expired), and a
// topic ROTATION on a device that already had one must not.
func TestMediator_TopicAssignmentFiresObserver(t *testing.T) {
	cases := []struct {
		name      string
		prevTopic string
		wantFired bool
	}{
		{name: "first assignment fires", prevTopic: "", wantFired: true},
		{name: "rotation does not re-fire", prevTopic: "old-topic", wantFired: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			fired := false
			ts.mediator.SetTopicObserver(func() { fired = true })

			ts.mockJSON.EXPECT().Marshal(gomock.Any()).Return([]byte("{}"), nil).AnyTimes()
			s := &state.State{Relayer: &state.RelayerState{TopicID: tc.prevTopic}}
			mockStateManager := mocks.NewMockStateManager(ts.ctrl)
			mockStateManager.EXPECT().
				SetRelayerTopicID("assigned-topic").
				DoAndReturn(func(newTopicID string) (bool, error) {
					hadTopicBefore := s.Relayer.TopicID != ""
					s.Relayer.TopicID = newTopicID
					return hadTopicBefore, nil
				}).
				Times(1)
			state.InjectStateManagerForTesting(mockStateManager)

			var capturedHandler relayer.Handler
			ts.mockDbus.EXPECT().OnBusSignal(gomock.Any()).Times(1)
			ts.mockRelayer.EXPECT().
				OnRelayerMessage(gomock.Any()).DoAndReturn(func(handler relayer.Handler) {
				capturedHandler = handler
			}).Times(1)
			ts.mediator.Start()

			topicID := "assigned-topic"
			err := capturedHandler(ts.ctx, relayer.Payload{
				MessageID: relayer.MESSAGE_ID_SYSTEM,
				Message:   relayer.Message{TopicID: &topicID},
			})
			state.ResetForTesting()

			assert.NoError(t, err)
			assert.Equal(t, tc.wantFired, fired)
			assert.Equal(t, topicID, s.Relayer.TopicID, "topic persisted before/regardless of observer")
		})
	}
}

func TestMediator_HandleRelayerMessage_System(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup) (relayer.Payload, error)
	}{
		{
			name: "valid system message",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				topicID := "test-topic-id"

				// Expect JSON marshal for logging
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte("{}"), nil).
					AnyTimes()

				// Mock state manager write
				mockStateManager := mocks.NewMockStateManager(ts.ctrl)
				mockStateManager.EXPECT().
					SetRelayerTopicID(topicID).
					Return(true, nil).
					Times(1)

				// Inject mock state manager
				state.InjectStateManagerForTesting(mockStateManager)

				payload := relayer.Payload{
					MessageID: relayer.MESSAGE_ID_SYSTEM,
					Message: relayer.Message{
						TopicID: &topicID,
					},
				}

				return payload, nil
			},
		},
		{
			name: "system message missing topic ID",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				// Expect JSON marshal for logging
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte("{}"), nil).
					AnyTimes()

				payload := relayer.Payload{
					MessageID: relayer.MESSAGE_ID_SYSTEM,
					Message: relayer.Message{
						TopicID: nil,
					},
				}

				return payload, errors.New("payload doesn't contain topicID")
			},
		},
		{
			name: "system message state save error",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				topicID := "test-topic-id"
				saveError := errors.New("save failed")

				// Expect JSON marshal for logging
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte("{}"), nil).
					AnyTimes()

				// Mock state manager write, failing
				mockStateManager := mocks.NewMockStateManager(ts.ctrl)
				mockStateManager.EXPECT().
					SetRelayerTopicID(topicID).
					Return(false, saveError).
					Times(1)

				// Inject mock state manager
				state.InjectStateManagerForTesting(mockStateManager)

				payload := relayer.Payload{
					MessageID: relayer.MESSAGE_ID_SYSTEM,
					Message: relayer.Message{
						TopicID: &topicID,
					},
				}

				return payload, saveError
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			payload, expectedError := tt.setupFunc(ts)

			// Register handler
			var capturedHandler relayer.Handler
			ts.mockDbus.EXPECT().
				OnBusSignal(gomock.Any()).
				Times(1)

			// Expect OnRelayerMessage to be called
			ts.mockRelayer.EXPECT().
				OnRelayerMessage(gomock.Any()).DoAndReturn(func(handler relayer.Handler) {
				capturedHandler = handler
			}).Times(1)

			ts.mediator.Start()

			// Call handler directly
			err := capturedHandler(ts.ctx, payload)

			// Reset state after test
			state.ResetForTesting()

			// Verify
			if expectedError != nil {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), expectedError.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestMediator_HandleRelayerMessage_ProcessCommand(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup) (relayer.Payload, error)
	}{
		{
			name: "command processing success",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := string(commands.CMD_CONNECT)
				args := map[string]interface{}{"arg1": "value1"}
				result := map[string]interface{}{
					"type":      "RPC",
					"messageID": "test-message",
					"message":   map[string]interface{}{"result": "success"},
				}

				payload := relayer.Payload{
					MessageID: "test-message",
					Message: relayer.Message{
						Command: &cmd,
						Request: args,
					},
				}

				// Expect JSON marshal for logging
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte("{}"), nil).
					AnyTimes()

				// Expect the command handler to process
				ts.mockCommandHandler.EXPECT().
					Process(gomock.Any(), commands.Command{
						Type:      commands.Type(cmd),
						Arguments: args,
					}).
					Return(result, nil).
					Times(1)

				// Expect the result to be sent
				ts.mockRelayer.EXPECT().
					Send(gomock.Any(), relayer.Response{
						Type:      "RPC",
						MessageID: "test-message",
						Message:   result,
					}).
					Return(nil).
					Times(1)

				return payload, nil
			},
		},
		{
			name: "command processing error",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := string(commands.CMD_CONNECT)
				args := map[string]interface{}{"arg1": "value1"}
				execError := errors.New("execution failed")

				payload := relayer.Payload{
					MessageID: "test-message",
					Message: relayer.Message{
						Command: &cmd,
						Request: args,
					},
				}

				// Expect JSON marshal for logging
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte("{}"), nil).
					AnyTimes()

				// Expect the command handler to process and return error
				ts.mockCommandHandler.EXPECT().
					Process(gomock.Any(), commands.Command{
						Type:      commands.Type(cmd),
						Arguments: args,
					}).
					Return(nil, execError).
					Times(1)

				return payload, execError
			},
		},
		{
			name: "command processing nil result",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := string(commands.CMD_CONNECT)
				args := map[string]interface{}{"arg1": "value1"}
				payload := relayer.Payload{
					MessageID: "test-message",
					Message: relayer.Message{
						Command: &cmd,
						Request: args,
					},
				}

				// Expect JSON marshal for logging
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte("{}"), nil).
					AnyTimes()

				// Expect the command handler to process and return nil result
				ts.mockCommandHandler.EXPECT().
					Process(gomock.Any(), commands.Command{
						Type:      commands.Type(cmd),
						Arguments: args,
					}).
					Return(nil, nil).
					Times(1)

				return payload, nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			payload, expectedError := tt.setupFunc(ts)

			// Register handler
			var capturedHandler relayer.Handler
			ts.mockDbus.EXPECT().
				OnBusSignal(gomock.Any()).
				Times(1)

			// Expect OnRelayerMessage to be called
			ts.mockRelayer.EXPECT().
				OnRelayerMessage(gomock.Any()).
				DoAndReturn(func(handler relayer.Handler) {
					capturedHandler = handler
				}).Times(1)

			ts.mediator.Start()

			// Call handler directly
			err := capturedHandler(ts.ctx, payload)

			// Verify
			if expectedError != nil {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), expectedError.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestMediator_HandleRelayerMessage_RateLimited verifies the relayer ingress
// path reports command-storm rejections legibly: when the command router
// rejects a command with a RateLimitedError, the mediator sends a structured
// "rate_limited" RPC response back to the caller instead of dropping it
// silently (feral-file/ffos-user#208).
func TestMediator_HandleRelayerMessage_RateLimited(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := string(commands.CMD_DISPLAY_PLAYLIST)
	args := map[string]interface{}{"url": "https://example/feed"}
	payload := relayer.Payload{
		MessageID: "msg-rate-limited",
		Message: relayer.Message{
			Command: &cmd,
			Request: args,
		},
	}

	ts.mockJSON.EXPECT().Marshal(gomock.Any()).Return([]byte("{}"), nil).AnyTimes()

	ts.mockCommandHandler.EXPECT().
		Process(gomock.Any(), commands.Command{Type: commands.Type(cmd), Arguments: args}).
		Return(nil, &commandrouter.RateLimitedError{Command: commands.Type(cmd), Reason: "rate limit exceeded"}).
		Times(1)

	var sent relayer.Response
	ts.mockRelayer.EXPECT().
		Send(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, data interface{}) error {
			sent = data.(relayer.Response)
			return nil
		}).
		Times(1)

	var capturedHandler relayer.Handler
	ts.mockDbus.EXPECT().OnBusSignal(gomock.Any()).Times(1)
	ts.mockRelayer.EXPECT().
		OnRelayerMessage(gomock.Any()).
		DoAndReturn(func(handler relayer.Handler) { capturedHandler = handler }).
		Times(1)

	ts.mediator.Start()
	err := capturedHandler(ts.ctx, payload)
	assert.NoError(t, err)

	assert.Equal(t, "RPC", sent.Type)
	assert.Equal(t, "msg-rate-limited", sent.MessageID)
	msg, ok := sent.Message.(map[string]any)
	if assert.True(t, ok, "rate-limited response carries a structured body") {
		assert.Equal(t, "rate_limited", msg["error"])
		assert.Equal(t, cmd, msg["command"])
	}
}
