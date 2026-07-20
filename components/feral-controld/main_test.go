package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/config"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/state"

	go_daemon "github.com/coreos/go-systemd/v22/daemon"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

func boolPtr(b bool) *bool { return &b }

// spyProvisioning is an ordering/lifecycle spy for the provisioning runner seam.
// It records whether (and, via onStart, when relative to other startup steps) the
// provisioning domain was started.
type spyProvisioning struct {
	mu      sync.Mutex
	started bool
	onStart func()
}

func (s *spyProvisioning) Start(context.Context) {
	s.mu.Lock()
	s.started = true
	fn := s.onStart
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (s *spyProvisioning) Stop() {}

func (s *spyProvisioning) wasStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// orderIndex returns the first index of s in list, or -1.
func orderIndex(list []string, s string) int {
	for i, e := range list {
		if e == s {
			return i
		}
	}
	return -1
}

type testSetup struct {
	ctrl              *gomock.Controller
	ctx               context.Context
	cancel            context.CancelFunc
	logger            *zap.Logger
	app               *app
	config            *config.Config
	mockStateManager  *mocks.MockStateManager
	mockLoggerManager *mocks.MockLoggerManager

	// Mocked components
	mockCDP          *mocks.MockCDP
	mockRelayer      *mocks.MockRelayer
	mockDBus         *mocks.MockDBus
	mockMediator     *mocks.MockMediator
	mockOOMRecoverer *mocks.MockOOMRecoverer
	mockExecutor     *mocks.MockExecutor
	mockDeviceStatus *mocks.MockDeviceStatus
	mockStatusPoller *mocks.MockStatusPoller
	mockWatchdog     *mocks.MockWatchdog
	mockRefresher    *mocks.MockRefresher
	mockHub          *mocks.MockHub

	// Mocked wrappers
	mockClock      *mocks.MockClock
	mockOS         *mocks.MockOS
	mockSignal     *mocks.MockSignal
	mockDaemon     *mocks.MockDaemon
	mockHTTPClient *mocks.MockHTTPClient
	mockIO         *mocks.MockIO
	mockJSON       *mocks.MockJSON
	mockRandom     *mocks.MockRandomizer
	mockExec       *mocks.MockExec
	mockMath       *mocks.MockMath
}

func setup(t *testing.T) *testSetup {
	ctrl := gomock.NewController(t)
	l := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx, cancel := context.WithCancel(context.Background())

	// Create all mocks
	ts := &testSetup{
		ctrl:              ctrl,
		ctx:               ctx,
		cancel:            cancel,
		logger:            l,
		mockStateManager:  mocks.NewMockStateManager(ctrl),
		mockLoggerManager: mocks.NewMockLoggerManager(ctrl),
		mockCDP:           mocks.NewMockCDP(ctrl),
		mockRelayer:       mocks.NewMockRelayer(ctrl),
		mockDBus:          mocks.NewMockDBus(ctrl),
		mockMediator:      mocks.NewMockMediator(ctrl),
		mockOOMRecoverer:  mocks.NewMockOOMRecoverer(ctrl),
		mockExecutor:      mocks.NewMockExecutor(ctrl),
		mockDeviceStatus:  mocks.NewMockDeviceStatus(ctrl),
		mockStatusPoller:  mocks.NewMockStatusPoller(ctrl),
		mockWatchdog:      mocks.NewMockWatchdog(ctrl),
		mockRefresher:     mocks.NewMockRefresher(ctrl),
		mockClock:         mocks.NewMockClock(ctrl),
		mockOS:            mocks.NewMockOS(ctrl),
		mockSignal:        mocks.NewMockSignal(ctrl),
		mockDaemon:        mocks.NewMockDaemon(ctrl),
		mockHTTPClient:    mocks.NewMockHTTPClient(ctrl),
		mockIO:            mocks.NewMockIO(ctrl),
		mockJSON:          mocks.NewMockJSON(ctrl),
		mockRandom:        mocks.NewMockRandomizer(ctrl),
		mockExec:          mocks.NewMockExec(ctrl),
		mockMath:          mocks.NewMockMath(ctrl),
		mockHub:           mocks.NewMockHub(ctrl),
	}

	// Create test config
	ts.config = &config.Config{
		CDPConfig: &config.CDPConfig{
			Endpoint: "http://localhost:9222",
		},
		RelayerConfig: &config.RelayerConfig{
			Endpoint: "wss://test.relay.com",
			APIKey:   "test-api-key",
		},
		SentryConfig: &logger.SentryConfig{
			DSN:         "",
			Environment: "test",
		},
		EnableHub: boolPtr(true),
	}

	// Create test app with mocked components
	app := initializeTestApp(
		ctx,
		l,
		ts.mockClock,
		ts.mockOS,
		ts.mockSignal,
		ts.mockDaemon,
		ts.mockHTTPClient,
		ts.mockIO,
		ts.mockJSON,
		ts.mockRandom,
		ts.mockExec,
		ts.mockMath,
		ts.mockCDP,
		ts.mockRelayer,
		ts.mockDBus,
		ts.mockDeviceStatus,
		ts.mockStatusPoller,
		ts.mockWatchdog,
		ts.mockMediator,
		ts.mockOOMRecoverer,
		ts.mockExecutor,
		ts.mockRefresher,
		nil,
		ts.mockHub,
	)
	ts.app = app

	// Inject mock state manager
	state.InjectStateManagerForTesting(ts.mockStateManager)
	logger.InjectLoggerManagerForTesting(ts.mockLoggerManager)

	return ts
}

func (ts *testSetup) teardown() {
	state.ResetForTesting()
	logger.ResetForTesting()
	ts.cancel()
	ts.ctrl.Finish()
}

// Test App.run method with various scenarios

func TestApp_Run_Success(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup)
	}{
		{
			name: "successful startup without sentry",
			setupFunc: func(ts *testSetup) {
				// Mock successful state loading
				ts.mockStateManager.EXPECT().
					Load(ts.logger).
					Return(&state.State{
						Relayer: &state.RelayerState{TopicID: ""},
					}, nil)

				// CDP now connects in the background and never gates startup: run() calls
				// Start (fire-and-forget) and Close on shutdown.
				ts.mockCDP.EXPECT().Start(gomock.Any(), gomock.Any())
				ts.mockCDP.EXPECT().Close()

				// Mock Watchdog start and stop
				ts.mockWatchdog.EXPECT().Start(gomock.Any())
				ts.mockWatchdog.EXPECT().Stop()

				// Mock DBus start and stop
				ts.mockDBus.EXPECT().Start().Return(nil)
				ts.mockDBus.EXPECT().Stop().Return(nil)
				ts.mockDBus.EXPECT().Export(gomock.Any(), dbus.PATH, dbus.INTERFACE).Return(nil)

				// Mock Mediator start and stop
				ts.mockMediator.EXPECT().Start()
				ts.mockMediator.EXPECT().Stop()

				// Mock StatusPoller start and stop
				ts.mockStatusPoller.EXPECT().Start(gomock.Any())
				ts.mockStatusPoller.EXPECT().Stop()

				// Mock Refresher start and stop
				ts.mockRefresher.EXPECT().Start()
				ts.mockRefresher.EXPECT().Stop()

				// Mock Hub start and stop
				ts.mockHub.EXPECT().Start()
				ts.mockHub.EXPECT().Stop().Return(nil)

				// Mock OS ReadFile for mDNS device info
				ts.mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("test-hostname"), nil)

				// Mock Mediator InitializeMDNS
				ts.mockMediator.EXPECT().InitializeMDNS(gomock.Any(), gomock.Any(), gomock.Any())

				// Mock Daemon notify
				ts.mockDaemon.EXPECT().SdNotify(false, go_daemon.SdNotifyReady).Return(true, nil)

				// Mock OOM recoverer
				ts.mockOOMRecoverer.EXPECT().Start(gomock.Any())

				// Mock DBus call
				ts.mockDBus.EXPECT().
					Call(gomock.Any(), dbus.MONITORD_NAME, dbus.MONITORD_PATH, dbus.MONITORD_INTERFACE, dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS, true).
					Return([]interface{}{false}, nil)
			},
		},
		{
			name: "successful startup with sentry and relayer connection",
			setupFunc: func(ts *testSetup) {
				// Enable Sentry in config
				ts.config.SentryConfig.DSN = "https://test@sentry.io/123"

				// Mock state with topic ID
				ts.mockStateManager.EXPECT().
					Load(ts.logger).
					Return(&state.State{
						Relayer: &state.RelayerState{TopicID: "test-topic-123"},
					}, nil)

				// Mock logger manager set global topic ID
				ts.mockLoggerManager.EXPECT().SetGlobalTopicID("test-topic-123")

				// CDP now connects in the background and never gates startup: run() calls
				// Start (fire-and-forget) and Close on shutdown.
				ts.mockCDP.EXPECT().Start(gomock.Any(), gomock.Any())
				ts.mockCDP.EXPECT().Close()

				// Mock Watchdog start and stop
				ts.mockWatchdog.EXPECT().Start(gomock.Any())
				ts.mockWatchdog.EXPECT().Stop()

				// Mock DBus start and stop
				ts.mockDBus.EXPECT().Start().Return(nil)
				ts.mockDBus.EXPECT().Stop().Return(nil)
				ts.mockDBus.EXPECT().Export(gomock.Any(), dbus.PATH, dbus.INTERFACE).Return(nil)

				// Mock Mediator start and stop
				ts.mockMediator.EXPECT().Start()
				ts.mockMediator.EXPECT().Stop()

				// Mock StatusPoller start and stop
				ts.mockStatusPoller.EXPECT().Start(gomock.Any())
				ts.mockStatusPoller.EXPECT().Stop()

				// Mock Refresher start and stop
				ts.mockRefresher.EXPECT().Start()
				ts.mockRefresher.EXPECT().Stop()

				// Mock Daemon notify
				ts.mockDaemon.EXPECT().SdNotify(false, go_daemon.SdNotifyReady).Return(true, nil)

				// Mock DBus call
				ts.mockDBus.EXPECT().
					Call(gomock.Any(), dbus.MONITORD_NAME, dbus.MONITORD_PATH, dbus.MONITORD_INTERFACE, dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS, true).
					Return([]interface{}{true}, nil)

				// Mock Hub start and stop
				ts.mockHub.EXPECT().Start()
				ts.mockHub.EXPECT().Stop().Return(nil)

				// Mock OS ReadFile for mDNS device info
				ts.mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("test-hostname"), nil)

				// Mock Mediator InitializeMDNS
				ts.mockMediator.EXPECT().InitializeMDNS(gomock.Any(), gomock.Any(), gomock.Any())

				// Mock Relayer connect and close
				ts.mockRelayer.EXPECT().Connect(gomock.Any()).Return(nil)
				ts.mockRelayer.EXPECT().Close()

				// Mock OOM recoverer
				ts.mockOOMRecoverer.EXPECT().Start(gomock.Any())
			},
		},
		{
			name: "successful startup with hub disabled",
			setupFunc: func(ts *testSetup) {
				ts.config.EnableHub = boolPtr(false)

				// Mock successful state loading
				ts.mockStateManager.EXPECT().
					Load(ts.logger).
					Return(&state.State{
						Relayer: &state.RelayerState{TopicID: ""},
					}, nil)

				// CDP now connects in the background and never gates startup: run() calls
				// Start (fire-and-forget) and Close on shutdown.
				ts.mockCDP.EXPECT().Start(gomock.Any(), gomock.Any())
				ts.mockCDP.EXPECT().Close()

				// Mock Watchdog start and stop
				ts.mockWatchdog.EXPECT().Start(gomock.Any())
				ts.mockWatchdog.EXPECT().Stop()

				// Mock DBus start and stop
				ts.mockDBus.EXPECT().Start().Return(nil)
				ts.mockDBus.EXPECT().Stop().Return(nil)
				ts.mockDBus.EXPECT().Export(gomock.Any(), dbus.PATH, dbus.INTERFACE).Return(nil)

				// Mock Mediator start and stop
				ts.mockMediator.EXPECT().Start()
				ts.mockMediator.EXPECT().Stop()

				// Mock StatusPoller start and stop
				ts.mockStatusPoller.EXPECT().Start(gomock.Any())
				ts.mockStatusPoller.EXPECT().Stop()

				// Mock Refresher start and stop
				ts.mockRefresher.EXPECT().Start()
				ts.mockRefresher.EXPECT().Stop()

				// Mock Daemon notify
				ts.mockDaemon.EXPECT().SdNotify(false, go_daemon.SdNotifyReady).Return(true, nil)

				// Mock OOM recoverer
				ts.mockOOMRecoverer.EXPECT().Start(gomock.Any())

				// Mock DBus call
				ts.mockDBus.EXPECT().
					Call(gomock.Any(), dbus.MONITORD_NAME, dbus.MONITORD_PATH, dbus.MONITORD_INTERFACE, dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS, true).
					Return([]interface{}{false}, nil)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			tt.setupFunc(ts)

			// Create a context that will be canceled after a short time
			testCtx, cancel := context.WithTimeout(ts.ctx, 50*time.Millisecond)
			defer cancel()

			err := ts.app.run(testCtx, ts.config)
			assert.NoError(t, err)
		})
	}
}

func TestApp_Run_Errors(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup)
		wantErr   string
	}{
		{
			name: "state load failure",
			setupFunc: func(ts *testSetup) {
				// Mock state load failure
				ts.mockStateManager.EXPECT().
					Load(ts.logger).
					Return(nil, errors.New("failed to load state file"))
			},
			wantErr: "failed to load state file",
		},
		{
			name: "DBus start failure",
			setupFunc: func(ts *testSetup) {
				// Mock state load ok
				ts.mockStateManager.EXPECT().
					Load(ts.logger).
					Return(&state.State{
						Relayer: &state.RelayerState{TopicID: ""},
					}, nil)

				// CDP.Start runs after DBus.Start, so a DBus failure returns before it.

				// Mock Watchdog start and stop
				ts.mockWatchdog.EXPECT().Start(gomock.Any())
				ts.mockWatchdog.EXPECT().Stop()

				// Mock DBus start failure
				ts.mockDBus.EXPECT().
					Start().
					Return(errors.New("DBus service unavailable"))
			},
			wantErr: "DBus service unavailable",
		},
		{
			name: "DBus export failure",
			setupFunc: func(ts *testSetup) {
				// Mock state load ok
				ts.mockStateManager.EXPECT().
					Load(ts.logger).
					Return(&state.State{
						Relayer: &state.RelayerState{TopicID: ""},
					}, nil)

				// CDP.Start runs after DBus.Export, so an Export failure returns before it.

				// Mock Watchdog start and stop
				ts.mockWatchdog.EXPECT().Start(gomock.Any())
				ts.mockWatchdog.EXPECT().Stop()

				// Mock DBus start ok
				ts.mockDBus.EXPECT().Start().Return(nil)
				ts.mockDBus.EXPECT().Stop().Return(nil)

				// Mock DBus export failure
				ts.mockDBus.EXPECT().
					Export(gomock.Any(), dbus.PATH, dbus.INTERFACE).
					Return(errors.New("failed to export interface"))
			},
			wantErr: "failed to export interface",
		},
		{
			// PR #218 review regression: a failed initial relayer connection must NOT
			// abort run(). Aborting here happens before SdNotifyReady and before setupd's
			// wait_for_controld can see our D-Bus interface, so a relayer outage would
			// crash-loop controld and take BLE provisioning down with it. The new contract:
			// log, hand the connection to a background RetryableConnect, and proceed all
			// the way to READY.
			name: "relayer initial connect failure continues to READY",
			setupFunc: func(ts *testSetup) {
				// Mock state load ok - relayer ready (topic present)
				ts.mockStateManager.EXPECT().
					Load(ts.logger).
					Return(&state.State{
						Relayer: &state.RelayerState{TopicID: "test-topic"},
					}, nil)

				// Mock Watchdog start and stop
				ts.mockWatchdog.EXPECT().Start(gomock.Any())
				ts.mockWatchdog.EXPECT().Stop()

				// Mock DBus start ok
				ts.mockDBus.EXPECT().Start().Return(nil)
				ts.mockDBus.EXPECT().Stop().Return(nil)
				ts.mockDBus.EXPECT().Export(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)

				// Mock Mediator start and stop
				ts.mockMediator.EXPECT().Start()
				ts.mockMediator.EXPECT().Stop()

				// Mock connectivity check - connected and ready
				ts.mockDBus.EXPECT().
					Call(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), true).
					Return([]interface{}{true}, nil)

				// Initial relayer connect fails...
				ts.mockRelayer.EXPECT().
					Connect(gomock.Any()).
					Return(errors.New("initial relayer connection failed"))
				// ...which must fall back to a background retry. Block until run()'s ctx
				// ends and return nil so the retry goroutine performs no mock or logger
				// calls after the test completes.
				ts.mockRelayer.EXPECT().
					RetryableConnect(gomock.Any()).
					DoAndReturn(func(ctx context.Context) error {
						<-ctx.Done()
						return nil
					}).
					AnyTimes()
				// The connection is closed on shutdown even though the initial connect
				// failed (the background retry may have established it by then).
				ts.mockRelayer.EXPECT().Close()

				// run() must proceed past the relayer failure to the full startup sequence.
				ts.mockCDP.EXPECT().Start(gomock.Any(), gomock.Any())
				ts.mockCDP.EXPECT().Close()

				// Mock Hub start and stop
				ts.mockHub.EXPECT().Start()
				ts.mockHub.EXPECT().Stop().Return(nil)

				// Mock OS ReadFile for mDNS device info
				ts.mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("test-hostname"), nil)

				// Mock Mediator InitializeMDNS
				ts.mockMediator.EXPECT().InitializeMDNS(gomock.Any(), gomock.Any(), gomock.Any())

				// Mock StatusPoller start and stop
				ts.mockStatusPoller.EXPECT().Start(gomock.Any())
				ts.mockStatusPoller.EXPECT().Stop()

				// Mock Refresher start and stop
				ts.mockRefresher.EXPECT().Start()
				ts.mockRefresher.EXPECT().Stop()

				// READY must still be reached — this is the point of the regression.
				ts.mockDaemon.EXPECT().SdNotify(false, go_daemon.SdNotifyReady).Return(true, nil)

				// Mock OOM recoverer
				ts.mockOOMRecoverer.EXPECT().Start(gomock.Any())
			},
			wantErr: "",
		},
		{
			name: "daemon notify failure",
			setupFunc: func(ts *testSetup) {
				// Mock state load ok
				ts.mockStateManager.EXPECT().
					Load(ts.logger).
					Return(&state.State{
						Relayer: &state.RelayerState{TopicID: ""},
					}, nil)

				// CDP connects in the background; run() reaches Start + Close here.
				ts.mockCDP.EXPECT().Start(gomock.Any(), gomock.Any())
				ts.mockCDP.EXPECT().Close()

				// Mock Watchdog start and stop
				ts.mockWatchdog.EXPECT().Start(gomock.Any())
				ts.mockWatchdog.EXPECT().Stop()

				// Mock DBus start ok
				ts.mockDBus.EXPECT().Start().Return(nil)
				ts.mockDBus.EXPECT().Stop().Return(nil)
				ts.mockDBus.EXPECT().Export(gomock.Any(), dbus.PATH, dbus.INTERFACE).Return(nil)

				// Mock Mediator start and stop
				ts.mockMediator.EXPECT().Start()
				ts.mockMediator.EXPECT().Stop()

				// Mock DBus call
				ts.mockDBus.EXPECT().
					Call(gomock.Any(), dbus.MONITORD_NAME, dbus.MONITORD_PATH, dbus.MONITORD_INTERFACE, dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS, true).
					Return([]interface{}{false}, nil)

				// Mock StatusPoller start and stop
				ts.mockStatusPoller.EXPECT().Start(gomock.Any())
				ts.mockStatusPoller.EXPECT().Stop()

				// Mock Refresher start and stop
				ts.mockRefresher.EXPECT().Start()
				ts.mockRefresher.EXPECT().Stop()

				// Mock Hub start and stop
				ts.mockHub.EXPECT().Start()
				ts.mockHub.EXPECT().Stop().Return(nil)

				// Mock OS ReadFile for mDNS device info
				ts.mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("test-hostname"), nil)

				// Mock Mediator InitializeMDNS
				ts.mockMediator.EXPECT().InitializeMDNS(gomock.Any(), gomock.Any(), gomock.Any())

				// Mock Daemon notify failure
				ts.mockDaemon.EXPECT().SdNotify(false, go_daemon.SdNotifyReady).Return(false, errors.New("daemon notify failed"))

				// Mock OOM recoverer
				ts.mockOOMRecoverer.EXPECT().Start(gomock.Any())
			},
			wantErr: "", // No error expected
		},
		{
			name: "connectivity check failure",
			setupFunc: func(ts *testSetup) {
				// Mock state load ok
				ts.mockStateManager.EXPECT().
					Load(ts.logger).
					Return(&state.State{
						Relayer: &state.RelayerState{TopicID: ""},
					}, nil)

				// CDP connects in the background; run() reaches Start + Close here.
				ts.mockCDP.EXPECT().Start(gomock.Any(), gomock.Any())
				ts.mockCDP.EXPECT().Close()

				// Mock Watchdog start and stop
				ts.mockWatchdog.EXPECT().Start(gomock.Any())
				ts.mockWatchdog.EXPECT().Stop()

				// Mock DBus start ok
				ts.mockDBus.EXPECT().Start().Return(nil)
				ts.mockDBus.EXPECT().Stop().Return(nil)
				ts.mockDBus.EXPECT().Export(gomock.Any(), dbus.PATH, dbus.INTERFACE).Return(nil)

				// Mock Mediator start and stop
				ts.mockMediator.EXPECT().Start()
				ts.mockMediator.EXPECT().Stop()

				// Mock DBus call failure
				ts.mockDBus.EXPECT().
					Call(gomock.Any(), dbus.MONITORD_NAME, dbus.MONITORD_PATH, dbus.MONITORD_INTERFACE, dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS, true).
					Return(nil, errors.New("DBus call failed"))

				// Mock StatusPoller start and stop
				ts.mockStatusPoller.EXPECT().Start(gomock.Any())
				ts.mockStatusPoller.EXPECT().Stop()

				// Mock Refresher start and stop
				ts.mockRefresher.EXPECT().Start()
				ts.mockRefresher.EXPECT().Stop()

				// Mock Hub start and stop
				ts.mockHub.EXPECT().Start()
				ts.mockHub.EXPECT().Stop().Return(nil)

				// Mock OS ReadFile for mDNS device info
				ts.mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("test-hostname"), nil)

				// Mock Mediator InitializeMDNS
				ts.mockMediator.EXPECT().InitializeMDNS(gomock.Any(), gomock.Any(), gomock.Any())

				// Mock daemon notify
				ts.mockDaemon.EXPECT().SdNotify(false, go_daemon.SdNotifyReady).Return(true, nil)

				// Mock OOM recoverer
				ts.mockOOMRecoverer.EXPECT().Start(gomock.Any())
			},
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			tt.setupFunc(ts)

			ctx, cancel := context.WithTimeout(ts.ctx, 50*time.Millisecond)
			defer cancel()

			err := ts.app.run(ctx, ts.config)
			if tt.wantErr != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// Test getConnectivityStatus function

func TestGetConnectivityStatus(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup)
		wantConn  bool
		wantErr   string
	}{
		{
			name: "successful connectivity check - connected",
			setupFunc: func(ts *testSetup) {
				ts.mockDBus.EXPECT().
					Call(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), true).
					Return([]interface{}{true}, nil)
			},
			wantConn: true,
			wantErr:  "",
		},
		{
			name: "successful connectivity check - disconnected",
			setupFunc: func(ts *testSetup) {
				ts.mockDBus.EXPECT().
					Call(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), true).
					Return([]interface{}{false}, nil)
			},
			wantConn: false,
			wantErr:  "",
		},
		{
			name: "DBus call failure",
			setupFunc: func(ts *testSetup) {
				ts.mockDBus.EXPECT().
					Call(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), true).
					Return(nil, errors.New("DBus call failed"))
			},
			wantConn: false,
			wantErr:  "DBus call failed",
		},
		{
			name: "unexpected response length",
			setupFunc: func(ts *testSetup) {
				ts.mockDBus.EXPECT().
					Call(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), true).
					Return([]interface{}{true, false}, nil) // Too many responses
			},
			wantConn: false,
			wantErr:  "expected 1 response, got 2",
		},
		{
			name: "unexpected response type",
			setupFunc: func(ts *testSetup) {
				ts.mockDBus.EXPECT().
					Call(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), true).
					Return([]interface{}{"invalid"}, nil) // String instead of bool
			},
			wantConn: false,
			wantErr:  "expected bool, got string",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			tt.setupFunc(ts)

			connected, err := getConnectivityStatus(ts.ctx, ts.mockDBus, ts.logger)

			assert.Equal(t, tt.wantConn, connected)
			if tt.wantErr != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// Test initialization functions

func TestInitializeApp(t *testing.T) {
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))

	app := initializeApp(
		logger,
		"http://localhost:9222",
		"wss://test.relay.com",
		"test-api-key",
		nil,
		"com.feralfile.test",
		nil,
	)

	assert.NotNil(t, app)
	assert.Equal(t, logger, app.Logger)
	assert.NotNil(t, app.Ctx)
	assert.NotNil(t, app.CDP)
	assert.NotNil(t, app.Relayer)
	assert.NotNil(t, app.DBus)
	assert.NotNil(t, app.Mediator)
	assert.NotNil(t, app.OOMRecoverer)
	assert.NotNil(t, app.Executor)
	assert.NotNil(t, app.DeviceStatus)
	assert.NotNil(t, app.StatusPoller)
	assert.NotNil(t, app.Watchdog)
	assert.NotNil(t, app.PlaylistRefresher)
	assert.NotNil(t, app.Hub)

	// Test all wrappers are initialized
	assert.NotNil(t, app.Clock)
	assert.NotNil(t, app.OS)
	assert.NotNil(t, app.Signal)
	assert.NotNil(t, app.Daemon)
	assert.NotNil(t, app.HTTPClient)
	assert.NotNil(t, app.IO)
	assert.NotNil(t, app.JSON)
	assert.NotNil(t, app.Random)
	assert.NotNil(t, app.Exec)
	assert.NotNil(t, app.Math)
}

func TestInitializeTestApp(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	// Create minimal mocks for testing
	mockCDP := mocks.NewMockCDP(ctrl)
	mockRelayer := mocks.NewMockRelayer(ctrl)
	mockDBus := mocks.NewMockDBus(ctrl)
	mockMediator := mocks.NewMockMediator(ctrl)
	mockOOMRecoverer := mocks.NewMockOOMRecoverer(ctrl)
	mockExecutor := mocks.NewMockExecutor(ctrl)
	mockDeviceStatus := mocks.NewMockDeviceStatus(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockWatchdog := mocks.NewMockWatchdog(ctrl)
	mockRefresher := mocks.NewMockRefresher(ctrl)
	mockHub := mocks.NewMockHub(ctrl)

	// Mocked wrappers
	mockClock := mocks.NewMockClock(ctrl)
	mockOS := mocks.NewMockOS(ctrl)
	mockSignal := mocks.NewMockSignal(ctrl)
	mockDaemon := mocks.NewMockDaemon(ctrl)
	mockHTTPClient := mocks.NewMockHTTPClient(ctrl)
	mockIO := mocks.NewMockIO(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	mockRandom := mocks.NewMockRandomizer(ctrl)
	mockExec := mocks.NewMockExec(ctrl)
	mockMath := mocks.NewMockMath(ctrl)

	app := initializeTestApp(
		ctx,
		logger,
		mockClock,
		mockOS,
		mockSignal,
		mockDaemon,
		mockHTTPClient,
		mockIO,
		mockJSON,
		mockRandom,
		mockExec,
		mockMath,
		mockCDP,
		mockRelayer,
		mockDBus,
		mockDeviceStatus,
		mockStatusPoller,
		mockWatchdog,
		mockMediator,
		mockOOMRecoverer,
		mockExecutor,
		mockRefresher,
		nil,
		mockHub,
	)

	assert.NotNil(t, app)
	assert.Equal(t, logger, app.Logger)
	assert.Equal(t, ctx, app.Ctx)
	assert.Equal(t, mockCDP, app.CDP)
	assert.Equal(t, mockRelayer, app.Relayer)
	assert.Equal(t, mockDBus, app.DBus)
	assert.Equal(t, mockMediator, app.Mediator)
	assert.Equal(t, mockOOMRecoverer, app.OOMRecoverer)
	assert.Equal(t, mockExecutor, app.Executor)
	assert.Equal(t, mockDeviceStatus, app.DeviceStatus)
	assert.Equal(t, mockStatusPoller, app.StatusPoller)
	assert.Equal(t, mockWatchdog, app.Watchdog)
	assert.Equal(t, mockRefresher, app.PlaylistRefresher)
	assert.Equal(t, mockHub, app.Hub)

	// Test all wrappers are initialized
	assert.Equal(t, mockClock, app.Clock)
	assert.Equal(t, mockOS, app.OS)
	assert.Equal(t, mockSignal, app.Signal)
	assert.Equal(t, mockDaemon, app.Daemon)
	assert.Equal(t, mockHTTPClient, app.HTTPClient)
	assert.Equal(t, mockIO, app.IO)
	assert.Equal(t, mockJSON, app.JSON)
	assert.Equal(t, mockRandom, app.Random)
	assert.Equal(t, mockExec, app.Exec)
	assert.Equal(t, mockMath, app.Math)
}

// TestApp_Run_StartupOrdering pins the P2.5 invariant: the LAN hub and the
// provisioning (setup-AP) domain come up BEFORE the relayer connection, so a
// device that cannot reach the relayer can still be recovered over LAN and can
// still raise its setup AP. It drives run() with an ordering spy on provisioning
// and Do hooks on the hub/relayer mocks, then asserts both precede the relayer
// connect.
func TestApp_Run_StartupOrdering(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// controld owns setup so the provisioning domain is started.
	owner := config.SetupOwnerControld
	ts.config.SetupOwner = &owner

	var mu sync.Mutex
	var order []string
	record := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}

	spy := &spyProvisioning{onStart: func() { record("provisioning") }}
	ts.app.Provisioning = spy

	// Relayer ready (topic present) + connectivity true so the gate attempts a
	// relayer connect (the step ordering is measured against).
	ts.mockStateManager.EXPECT().Load(ts.logger).Return(&state.State{
		Relayer: &state.RelayerState{TopicID: "test-topic"},
	}, nil)

	ts.mockWatchdog.EXPECT().Start(gomock.Any())
	ts.mockWatchdog.EXPECT().Stop()
	ts.mockDBus.EXPECT().Start().Return(nil)
	ts.mockDBus.EXPECT().Stop().Return(nil)
	ts.mockDBus.EXPECT().Export(gomock.Any(), dbus.PATH, dbus.INTERFACE).Return(nil)
	ts.mockMediator.EXPECT().Start()
	ts.mockMediator.EXPECT().Stop()

	ts.mockHub.EXPECT().Start().Do(func() { record("hub") })
	ts.mockHub.EXPECT().Stop().Return(nil)
	ts.mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("test-hostname"), nil)
	ts.mockMediator.EXPECT().InitializeMDNS(gomock.Any(), gomock.Any(), gomock.Any())

	ts.mockDBus.EXPECT().
		Call(gomock.Any(), dbus.MONITORD_NAME, dbus.MONITORD_PATH, dbus.MONITORD_INTERFACE, dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS, true).
		Return([]interface{}{true}, nil)
	ts.mockRelayer.EXPECT().Connect(gomock.Any()).DoAndReturn(func(context.Context) error {
		record("relayer")
		return nil
	})
	ts.mockRelayer.EXPECT().Close()

	ts.mockRefresher.EXPECT().Start()
	ts.mockRefresher.EXPECT().Stop()
	ts.mockStatusPoller.EXPECT().Start(gomock.Any())
	ts.mockStatusPoller.EXPECT().Stop()
	ts.mockCDP.EXPECT().Start(gomock.Any(), gomock.Any())
	ts.mockCDP.EXPECT().Close()
	ts.mockDaemon.EXPECT().SdNotify(false, go_daemon.SdNotifyReady).Return(true, nil)
	ts.mockOOMRecoverer.EXPECT().Start(gomock.Any())

	testCtx, cancel := context.WithTimeout(ts.ctx, 50*time.Millisecond)
	defer cancel()
	err := ts.app.run(testCtx, ts.config)
	assert.NoError(t, err)

	assert.True(t, spy.wasStarted(), "provisioning must start when controld owns setup")

	mu.Lock()
	defer mu.Unlock()
	idxHub := orderIndex(order, "hub")
	idxProv := orderIndex(order, "provisioning")
	idxRelayer := orderIndex(order, "relayer")
	require.GreaterOrEqual(t, idxHub, 0, "hub must have started")
	require.GreaterOrEqual(t, idxProv, 0, "provisioning must have started")
	require.GreaterOrEqual(t, idxRelayer, 0, "relayer connect must have been attempted")
	assert.Less(t, idxHub, idxRelayer, "hub must start before the relayer connect")
	assert.Less(t, idxProv, idxRelayer, "provisioning must start before the relayer connect")
}

// TestApp_Run_ProvisioningDormant pins the dormant contract: with setupd owning
// setup (the default), run() never starts the provisioning domain, so no setup AP
// is ever raised.
func TestApp_Run_ProvisioningDormant(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Default config leaves SetupOwner unset -> setupd owns setup.
	spy := &spyProvisioning{}
	ts.app.Provisioning = spy

	ts.mockStateManager.EXPECT().Load(ts.logger).Return(&state.State{
		Relayer: &state.RelayerState{TopicID: ""},
	}, nil)

	ts.mockCDP.EXPECT().Start(gomock.Any(), gomock.Any())
	ts.mockCDP.EXPECT().Close()
	ts.mockWatchdog.EXPECT().Start(gomock.Any())
	ts.mockWatchdog.EXPECT().Stop()
	ts.mockDBus.EXPECT().Start().Return(nil)
	ts.mockDBus.EXPECT().Stop().Return(nil)
	ts.mockDBus.EXPECT().Export(gomock.Any(), dbus.PATH, dbus.INTERFACE).Return(nil)
	ts.mockMediator.EXPECT().Start()
	ts.mockMediator.EXPECT().Stop()
	ts.mockStatusPoller.EXPECT().Start(gomock.Any())
	ts.mockStatusPoller.EXPECT().Stop()
	ts.mockRefresher.EXPECT().Start()
	ts.mockRefresher.EXPECT().Stop()
	ts.mockHub.EXPECT().Start()
	ts.mockHub.EXPECT().Stop().Return(nil)
	ts.mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("test-hostname"), nil)
	ts.mockMediator.EXPECT().InitializeMDNS(gomock.Any(), gomock.Any(), gomock.Any())
	ts.mockDaemon.EXPECT().SdNotify(false, go_daemon.SdNotifyReady).Return(true, nil)
	ts.mockOOMRecoverer.EXPECT().Start(gomock.Any())
	ts.mockDBus.EXPECT().
		Call(gomock.Any(), dbus.MONITORD_NAME, dbus.MONITORD_PATH, dbus.MONITORD_INTERFACE, dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS, true).
		Return([]interface{}{false}, nil)

	testCtx, cancel := context.WithTimeout(ts.ctx, 50*time.Millisecond)
	defer cancel()
	err := ts.app.run(testCtx, ts.config)
	assert.NoError(t, err)

	assert.False(t, spy.wasStarted(),
		"provisioning must stay dormant when setupd owns setup; no setup AP is ever raised")
}
