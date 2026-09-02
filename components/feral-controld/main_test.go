package main

import (
	"context"
	"errors"
	"net/http"
	stdos "os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/config"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/provisioning"
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
				ts.mockStateManager.EXPECT().
					ClaimSnapshot().
					Return(state.ClaimInfo{TopicID: "", TopicReady: false}).
					AnyTimes()

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

				// Mock OS ReadFile for mDNS device info. The device-name
				// record is read alongside the hostname: an unnamed unit is
				// the ordinary case, and a missing file must leave the
				// advertised name as the serial.
				ts.mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("test-hostname"), nil)
				ts.mockOS.EXPECT().ReadFile(constants.DEVICE_NAME_FILE).Return(nil, stdos.ErrNotExist)
				ts.mockOS.EXPECT().IsNotExist(stdos.ErrNotExist).Return(true)

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
				// Relayer Close is unconditional at shutdown (a later reconcile
				// may have connected); it is a no-op on a nil conn.
				ts.mockRelayer.EXPECT().Close()
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
				ts.mockStateManager.EXPECT().
					ClaimSnapshot().
					Return(state.ClaimInfo{TopicID: "test-topic-123", TopicReady: true}).
					AnyTimes()

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

				// Mock OS ReadFile for mDNS device info. The device-name
				// record is read alongside the hostname: an unnamed unit is
				// the ordinary case, and a missing file must leave the
				// advertised name as the serial.
				ts.mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("test-hostname"), nil)
				ts.mockOS.EXPECT().ReadFile(constants.DEVICE_NAME_FILE).Return(nil, stdos.ErrNotExist)
				ts.mockOS.EXPECT().IsNotExist(stdos.ErrNotExist).Return(true)

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
			// A transient D-Bus startup failure (session-bus socket race, bus name
			// still held by a dying predecessor) must NOT abort startup: controld
			// is the sole SoftAP/LAN-recovery owner, and aborting here happened
			// BEFORE the hub or provisioning started — a crash-looping daemon left
			// an offline device with neither recovery surface. run() must log,
			// retry Start in the background, and still bring the hub up and reach
			// READY; D-Bus consumers degrade gracefully (the connectivity query
			// errors → treated as offline) until a retry lands.
			name: "DBus start failure continues to READY with recovery surfaces",
			setupFunc: func(ts *testSetup) {
				ts.mockStateManager.EXPECT().
					Load(ts.logger).
					Return(&state.State{
						Relayer: &state.RelayerState{TopicID: ""},
					}, nil)
				ts.mockStateManager.EXPECT().
					ClaimSnapshot().
					Return(state.ClaimInfo{TopicID: "", TopicReady: false}).
					AnyTimes()

				// Initial Start fails; the background retry parks in SleepContext
				// until the test context ends, so no second Start is attempted.
				ts.mockDBus.EXPECT().
					Start().
					Return(errors.New("session bus unavailable"))
				ts.mockClock.EXPECT().
					SleepContext(gomock.Any(), gomock.Any()).
					DoAndReturn(func(ctx context.Context, _ time.Duration) error {
						<-ctx.Done()
						return ctx.Err()
					}).
					AnyTimes()
				ts.mockDBus.EXPECT().Stop().Return(nil).AnyTimes()

				ts.mockCDP.EXPECT().Start(gomock.Any(), gomock.Any())
				ts.mockCDP.EXPECT().Close()
				ts.mockWatchdog.EXPECT().Start(gomock.Any())
				ts.mockWatchdog.EXPECT().Stop()
				ts.mockMediator.EXPECT().Start()
				ts.mockMediator.EXPECT().Stop()
				ts.mockStatusPoller.EXPECT().Start(gomock.Any())
				ts.mockStatusPoller.EXPECT().Stop()
				ts.mockRefresher.EXPECT().Start()
				ts.mockRefresher.EXPECT().Stop()
				ts.mockOOMRecoverer.EXPECT().Start(gomock.Any())

				// The LAN recovery hub still comes up...
				ts.mockHub.EXPECT().Start()
				ts.mockHub.EXPECT().Stop().Return(nil)
				ts.mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("test-hostname"), nil)
				ts.mockOS.EXPECT().ReadFile(constants.DEVICE_NAME_FILE).Return(nil, stdos.ErrNotExist)
				ts.mockOS.EXPECT().IsNotExist(stdos.ErrNotExist).Return(true)
				ts.mockMediator.EXPECT().InitializeMDNS(gomock.Any(), gomock.Any(), gomock.Any())

				// ...the degraded connectivity query reads as offline (no relayer
				// connect attempted)...
				ts.mockDBus.EXPECT().
					Call(gomock.Any(), dbus.MONITORD_NAME, dbus.MONITORD_PATH, dbus.MONITORD_INTERFACE, dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS, true).
					Return(nil, errors.New("DBusClient not started"))
				// Close is unconditional at shutdown (a later reconcile may
				// have connected), and a no-op on a nil conn.
				ts.mockRelayer.EXPECT().Close()

				// ...and the daemon still reaches READY.
				ts.mockDaemon.EXPECT().SdNotify(false, go_daemon.SdNotifyReady).Return(true, nil)
			},
		},
		{
			// The recovery half of the case above: after a failed initial Start
			// (name-conflict style), the background retry must Start the client
			// again while the rest of run() — including D-Bus consumers — is
			// already live, and shutdown must still Stop the client that the
			// retry brought up. The Start-vs-Call safety of that overlap is
			// pinned down in dbus/restartable_test.go; this exercises the run()
			// wiring: failed start → retry → success → shutdown.
			name: "DBus start retry succeeds and shutdown stops the client",
			setupFunc: func(ts *testSetup) {
				ts.mockStateManager.EXPECT().
					Load(ts.logger).
					Return(&state.State{
						Relayer: &state.RelayerState{TopicID: ""},
					}, nil)
				ts.mockStateManager.EXPECT().
					ClaimSnapshot().
					Return(state.ClaimInfo{TopicID: "", TopicReady: false}).
					AnyTimes()

				// Initial Start fails; the first backoff returns immediately so
				// the retry fires inside the test window and succeeds; any later
				// sleeps park until shutdown.
				ts.mockDBus.EXPECT().
					Start().
					Return(errors.New("failed to request name: 3"))
				ts.mockDBus.EXPECT().Start().Return(nil)
				var sleeps int32
				ts.mockClock.EXPECT().
					SleepContext(gomock.Any(), gomock.Any()).
					DoAndReturn(func(ctx context.Context, _ time.Duration) error {
						if atomic.AddInt32(&sleeps, 1) == 1 {
							return nil
						}
						<-ctx.Done()
						return ctx.Err()
					}).
					AnyTimes()
				ts.mockDBus.EXPECT().Stop().Return(nil).AnyTimes()

				ts.mockCDP.EXPECT().Start(gomock.Any(), gomock.Any())
				ts.mockCDP.EXPECT().Close()
				ts.mockWatchdog.EXPECT().Start(gomock.Any())
				ts.mockWatchdog.EXPECT().Stop()
				ts.mockMediator.EXPECT().Start()
				ts.mockMediator.EXPECT().Stop()
				ts.mockStatusPoller.EXPECT().Start(gomock.Any())
				ts.mockStatusPoller.EXPECT().Stop()
				ts.mockRefresher.EXPECT().Start()
				ts.mockRefresher.EXPECT().Stop()
				ts.mockOOMRecoverer.EXPECT().Start(gomock.Any())

				ts.mockHub.EXPECT().Start()
				ts.mockHub.EXPECT().Stop().Return(nil)
				ts.mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("test-hostname"), nil)
				ts.mockOS.EXPECT().ReadFile(constants.DEVICE_NAME_FILE).Return(nil, stdos.ErrNotExist)
				ts.mockOS.EXPECT().IsNotExist(stdos.ErrNotExist).Return(true)
				ts.mockMediator.EXPECT().InitializeMDNS(gomock.Any(), gomock.Any(), gomock.Any())

				// The startup connectivity query may run before or after the
				// retry lands; either way an error reads as offline, so no
				// relayer connect is attempted and READY is still reached.
				ts.mockDBus.EXPECT().
					Call(gomock.Any(), dbus.MONITORD_NAME, dbus.MONITORD_PATH, dbus.MONITORD_INTERFACE, dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS, true).
					Return(nil, errors.New("DBusClient not started"))
				// Close is unconditional at shutdown (a later reconcile may
				// have connected), and a no-op on a nil conn.
				ts.mockRelayer.EXPECT().Close()

				ts.mockDaemon.EXPECT().SdNotify(false, go_daemon.SdNotifyReady).Return(true, nil)
			},
		},
		{
			// Corrupt/unreadable persisted state must NOT abort startup: controld
			// is the sole SoftAP/LAN-recovery owner, and returning the error
			// crash-looped the daemon, stranding an offline device with no
			// recovery surface. run() quarantines the file and continues on an
			// empty state — every lifecycle expectation below firing IS the
			// assertion that the recovery surfaces still come up.
			name: "state load failure quarantines and continues startup",
			setupFunc: func(ts *testSetup) {
				ts.config.EnableHub = boolPtr(false)

				ts.mockStateManager.EXPECT().
					Load(ts.logger).
					Return(nil, errors.New("unexpected end of JSON input"))
				// Quarantine keeps the corrupt bytes for diagnosis.
				ts.mockOS.EXPECT().
					Rename(constants.STATE_FILE, constants.STATE_FILE+".corrupt").
					Return(nil)
				// The empty fallback state serves the rest of startup.
				ts.mockStateManager.EXPECT().
					GetState().
					Return(&state.State{
						Relayer:         &state.RelayerState{},
						ConnectedDevice: &state.Device{},
					}).
					AnyTimes()
				ts.mockStateManager.EXPECT().
					ClaimSnapshot().
					Return(state.ClaimInfo{TopicID: "", TopicReady: false}).
					AnyTimes()

				ts.mockCDP.EXPECT().Start(gomock.Any(), gomock.Any())
				ts.mockCDP.EXPECT().Close()
				ts.mockWatchdog.EXPECT().Start(gomock.Any())
				ts.mockWatchdog.EXPECT().Stop()
				ts.mockDBus.EXPECT().Start().Return(nil)
				ts.mockDBus.EXPECT().Stop().Return(nil)
				ts.mockMediator.EXPECT().Start()
				ts.mockMediator.EXPECT().Stop()
				ts.mockStatusPoller.EXPECT().Start(gomock.Any())
				ts.mockStatusPoller.EXPECT().Stop()
				ts.mockRefresher.EXPECT().Start()
				ts.mockRefresher.EXPECT().Stop()
				ts.mockDaemon.EXPECT().SdNotify(false, go_daemon.SdNotifyReady).Return(true, nil)
				ts.mockOOMRecoverer.EXPECT().Start(gomock.Any())

				ts.mockDBus.EXPECT().
					Call(gomock.Any(), dbus.MONITORD_NAME, dbus.MONITORD_PATH, dbus.MONITORD_INTERFACE, dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS, true).
					Return([]interface{}{false}, nil)
				// Relayer Close is unconditional at shutdown (a later reconcile
				// may have connected); it is a no-op on a nil conn.
				ts.mockRelayer.EXPECT().Close()
			},
		},
		{
			// Factory-fresh boot: online with NO topic. The startup connect must
			// fire anyway — connecting with an empty topic is the designed
			// topic-assignment path, and a device that boots already online never
			// gets the connectivity-change event the mediator's restore handler
			// needs, so this gate is its only trigger.
			name: "online with empty topic connects for topic assignment",
			setupFunc: func(ts *testSetup) {
				ts.config.EnableHub = boolPtr(false)

				ts.mockStateManager.EXPECT().
					Load(ts.logger).
					Return(&state.State{
						Relayer: &state.RelayerState{TopicID: ""},
					}, nil)
				ts.mockStateManager.EXPECT().
					ClaimSnapshot().
					Return(state.ClaimInfo{TopicID: "", TopicReady: false}).
					AnyTimes()

				ts.mockCDP.EXPECT().Start(gomock.Any(), gomock.Any())
				ts.mockCDP.EXPECT().Close()
				ts.mockWatchdog.EXPECT().Start(gomock.Any())
				ts.mockWatchdog.EXPECT().Stop()
				ts.mockDBus.EXPECT().Start().Return(nil)
				ts.mockDBus.EXPECT().Stop().Return(nil)
				ts.mockMediator.EXPECT().Start()
				ts.mockMediator.EXPECT().Stop()
				ts.mockStatusPoller.EXPECT().Start(gomock.Any())
				ts.mockStatusPoller.EXPECT().Stop()
				ts.mockRefresher.EXPECT().Start()
				ts.mockRefresher.EXPECT().Stop()
				ts.mockDaemon.EXPECT().SdNotify(false, go_daemon.SdNotifyReady).Return(true, nil)
				ts.mockOOMRecoverer.EXPECT().Start(gomock.Any())

				// Online...
				ts.mockDBus.EXPECT().
					Call(gomock.Any(), dbus.MONITORD_NAME, dbus.MONITORD_PATH, dbus.MONITORD_INTERFACE, dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS, true).
					Return([]interface{}{true}, nil)

				// ...must connect despite the empty topic.
				ts.mockRelayer.EXPECT().Connect(gomock.Any()).Return(nil)
				ts.mockRelayer.EXPECT().Close()
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
				ts.mockStateManager.EXPECT().
					ClaimSnapshot().
					Return(state.ClaimInfo{TopicID: "", TopicReady: false}).
					AnyTimes()

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
				// Relayer Close is unconditional at shutdown (a later reconcile
				// may have connected); it is a no-op on a nil conn.
				ts.mockRelayer.EXPECT().Close()
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

func TestApp_Run_StartsAndStopsOfflineCacheWhenEnabled(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	ts.mockCDP.EXPECT().Start(gomock.Any(), gomock.Any())
	ts.mockCDP.EXPECT().Close()
	ts.mockWatchdog.EXPECT().Start(gomock.Any())
	ts.mockWatchdog.EXPECT().Stop()
	ts.mockDBus.EXPECT().Start().Return(nil)
	ts.mockDBus.EXPECT().Stop().Return(nil)
	ts.mockMediator.EXPECT().Start()
	ts.mockMediator.EXPECT().Stop()
	ts.mockStatusPoller.EXPECT().Start(gomock.Any())
	ts.mockStatusPoller.EXPECT().Stop()
	ts.mockRefresher.EXPECT().Start()
	ts.mockRefresher.EXPECT().Stop()
	ts.mockHub.EXPECT().Start()
	ts.mockHub.EXPECT().Stop().Return(nil)
	ts.mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("test-hostname"), nil)
	ts.mockOS.EXPECT().ReadFile(constants.DEVICE_NAME_FILE).Return(nil, stdos.ErrNotExist)
	ts.mockOS.EXPECT().IsNotExist(stdos.ErrNotExist).Return(true)
	ts.mockMediator.EXPECT().InitializeMDNS(gomock.Any(), gomock.Any(), gomock.Any())
	ts.mockDaemon.EXPECT().SdNotify(false, go_daemon.SdNotifyReady).Return(true, nil)
	ts.mockOOMRecoverer.EXPECT().Start(gomock.Any())
	ts.mockDBus.EXPECT().
		Call(gomock.Any(), dbus.MONITORD_NAME, dbus.MONITORD_PATH, dbus.MONITORD_INTERFACE, dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS, true).
		Return([]interface{}{false}, nil)
	ts.mockStateManager.EXPECT().
		Load(ts.logger).
		Return(&state.State{Relayer: &state.RelayerState{TopicID: ""}}, nil)
	ts.mockStateManager.EXPECT().
		ClaimSnapshot().
		Return(state.ClaimInfo{TopicID: "", TopicReady: false}).
		AnyTimes()
	// Relayer.Close is called unconditionally on shutdown regardless of the
	// connectivity-gate outcome above (see run()'s own doc on why).
	ts.mockRelayer.EXPECT().Close()

	ctrl := ts.ctrl
	mockOfflineService := mocks.NewMockOfflineCacheService(ctrl)
	mockStaticServer := mocks.NewMockOfflineCacheStaticServer(ctrl)
	mockOfflineService.EXPECT().Start(gomock.Any()).Return(nil).Times(1)
	mockOfflineService.EXPECT().Stop().Times(1)
	// Listen must succeed BEFORE Serve is ever launched — see main.go's
	// doc on why binding is synchronous now rather than folded into a
	// single background ListenAndServe call.
	mockStaticServer.EXPECT().Listen().Return(nil).Times(1)
	mockStaticServer.EXPECT().Serve().Return(http.ErrServerClosed).Times(1)
	mockStaticServer.EXPECT().Shutdown(gomock.Any()).Return(nil).Times(1)
	ts.app.OfflineCacheService = mockOfflineService
	ts.app.OfflineCacheStaticServer = mockStaticServer

	testCtx, cancel := context.WithTimeout(ts.ctx, 50*time.Millisecond)
	defer cancel()

	err := ts.app.run(testCtx, ts.config)
	assert.NoError(t, err)
}

// TestApp_Run_SkipsServeAndShutdownWhenStaticServerBindFails is the
// regression test for the startup-ordering fix: if the static server's
// port cannot be bound (e.g. an unrelated process already holds it),
// main.go must never launch Serve in the background — the old
// ListenAndServe-only wiring would only discover a bind failure
// asynchronously, well after the rest of daemon startup had already
// proceeded assuming success — nor attempt Shutdown on a server that
// was never actually started.
func TestApp_Run_SkipsServeAndShutdownWhenStaticServerBindFails(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	ts.mockCDP.EXPECT().Start(gomock.Any(), gomock.Any())
	ts.mockCDP.EXPECT().Close()
	ts.mockWatchdog.EXPECT().Start(gomock.Any())
	ts.mockWatchdog.EXPECT().Stop()
	ts.mockDBus.EXPECT().Start().Return(nil)
	ts.mockDBus.EXPECT().Stop().Return(nil)
	ts.mockMediator.EXPECT().Start()
	ts.mockMediator.EXPECT().Stop()
	ts.mockStatusPoller.EXPECT().Start(gomock.Any())
	ts.mockStatusPoller.EXPECT().Stop()
	ts.mockRefresher.EXPECT().Start()
	ts.mockRefresher.EXPECT().Stop()
	ts.mockHub.EXPECT().Start()
	ts.mockHub.EXPECT().Stop().Return(nil)
	ts.mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("test-hostname"), nil)
	ts.mockOS.EXPECT().ReadFile(constants.DEVICE_NAME_FILE).Return(nil, stdos.ErrNotExist)
	ts.mockOS.EXPECT().IsNotExist(stdos.ErrNotExist).Return(true)
	ts.mockMediator.EXPECT().InitializeMDNS(gomock.Any(), gomock.Any(), gomock.Any())
	ts.mockDaemon.EXPECT().SdNotify(false, go_daemon.SdNotifyReady).Return(true, nil)
	ts.mockOOMRecoverer.EXPECT().Start(gomock.Any())
	ts.mockDBus.EXPECT().
		Call(gomock.Any(), dbus.MONITORD_NAME, dbus.MONITORD_PATH, dbus.MONITORD_INTERFACE, dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS, true).
		Return([]interface{}{false}, nil)
	ts.mockStateManager.EXPECT().
		Load(ts.logger).
		Return(&state.State{Relayer: &state.RelayerState{TopicID: ""}}, nil)
	ts.mockStateManager.EXPECT().
		ClaimSnapshot().
		Return(state.ClaimInfo{TopicID: "", TopicReady: false}).
		AnyTimes()
	// Relayer.Close is called unconditionally on shutdown regardless of the
	// connectivity-gate outcome above (see run()'s own doc on why).
	ts.mockRelayer.EXPECT().Close()

	ctrl := ts.ctrl
	mockOfflineService := mocks.NewMockOfflineCacheService(ctrl)
	mockStaticServer := mocks.NewMockOfflineCacheStaticServer(ctrl)
	mockOfflineService.EXPECT().Start(gomock.Any()).Return(nil).Times(1)
	mockOfflineService.EXPECT().Stop().Times(1)
	mockStaticServer.EXPECT().Listen().Return(errors.New("bind: address already in use")).Times(1)
	// Deliberately no Serve/Shutdown expectations: gomock's strict
	// controller fails this test if main.go calls either despite Listen
	// having failed.
	ts.app.OfflineCacheService = mockOfflineService
	ts.app.OfflineCacheStaticServer = mockStaticServer

	testCtx, cancel := context.WithTimeout(ts.ctx, 50*time.Millisecond)
	defer cancel()

	err := ts.app.run(testCtx, ts.config)
	assert.NoError(t, err, "a static-server bind failure must degrade gracefully, never fail the whole daemon run")
}

// TestApp_Run_OnConnectAttachesOfflineCacheReplay pins the INLINE half of
// the reconnect resync: AttachOnReconnect must run straight off the CDP
// connect callback, because it arms Fetch interception on offlinecache's own
// socket and needs no page JS.
//
// Its companion half — re-applying the item scope, which does need a hydrated
// page — is deliberately NOT here: it is the "replay-scope-resync"
// reconciler, covered by TestReplayScopeResyncReconciler. Asserting
// PlaylistRefresher.ForceRefresh on this callback is what the old version of
// this test did, and it pinned a resync that could not actually work at this
// point in the lifecycle (the status fetch it depends on evaluates
// window.handleCDPRequest against a document that has not hydrated yet).
func TestApp_Run_OnConnectAttachesOfflineCacheReplay(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	ts.mockWatchdog.EXPECT().Start(gomock.Any())
	ts.mockWatchdog.EXPECT().Stop()
	ts.mockDBus.EXPECT().Start().Return(nil)
	ts.mockDBus.EXPECT().Stop().Return(nil)
	ts.mockMediator.EXPECT().Start()
	ts.mockMediator.EXPECT().Stop()
	ts.mockStatusPoller.EXPECT().Start(gomock.Any())
	ts.mockStatusPoller.EXPECT().Stop()
	ts.mockRefresher.EXPECT().Start()
	ts.mockRefresher.EXPECT().Stop()
	ts.mockHub.EXPECT().Start()
	ts.mockHub.EXPECT().Stop().Return(nil)
	ts.mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("test-hostname"), nil)
	ts.mockOS.EXPECT().ReadFile(constants.DEVICE_NAME_FILE).Return(nil, stdos.ErrNotExist)
	ts.mockOS.EXPECT().IsNotExist(stdos.ErrNotExist).Return(true)
	ts.mockMediator.EXPECT().InitializeMDNS(gomock.Any(), gomock.Any(), gomock.Any())
	ts.mockDaemon.EXPECT().SdNotify(false, go_daemon.SdNotifyReady).Return(true, nil)
	ts.mockOOMRecoverer.EXPECT().Start(gomock.Any())
	ts.mockDBus.EXPECT().
		Call(gomock.Any(), dbus.MONITORD_NAME, dbus.MONITORD_PATH, dbus.MONITORD_INTERFACE, dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS, true).
		Return([]interface{}{false}, nil)
	ts.mockStateManager.EXPECT().
		Load(ts.logger).
		Return(&state.State{Relayer: &state.RelayerState{TopicID: ""}}, nil)
	ts.mockStateManager.EXPECT().
		ClaimSnapshot().
		Return(state.ClaimInfo{TopicID: "", TopicReady: false}).
		AnyTimes()
	// Relayer.Close is called unconditionally on shutdown regardless of the
	// connectivity-gate outcome above (see run()'s own doc on why).
	ts.mockRelayer.EXPECT().Close()

	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ts.ctrl)
	mockKioskReplay.EXPECT().AttachOnReconnect(gomock.Any()).Return(nil).Times(1)
	ts.app.KioskReplay = mockKioskReplay
	// Deliberately no ForceRefresh expectation: the scope resync moved to the
	// replay-scope-resync reconciler. ts.mockRefresher is a strict gomock mock,
	// so an unexpected ForceRefresh here fails the test — which is what pins
	// the split rather than merely not asserting it.

	var onConnect func()
	ts.mockCDP.EXPECT().Start(gomock.Any(), gomock.Any()).Do(func(_ context.Context, fn func()) {
		onConnect = fn
	})
	ts.mockCDP.EXPECT().Close()

	testCtx, cancel := context.WithTimeout(ts.ctx, 50*time.Millisecond)
	defer cancel()

	err := ts.app.run(testCtx, ts.config)
	assert.NoError(t, err)

	require.NotNil(t, onConnect, "CDP.Start must be given an onConnect callback")
	onConnect()
}

// TestReplayScopeResyncReconciler pins the half of the reconnect resync that
// moved off the CDP connect callback. Before this split the assertion lived in
// TestApp_Run_OnConnectAttachesOfflineCacheReplay; without a test here the
// resync would be entirely uncovered, and a future edit dropping the
// registration in initializeApp would silently reinstate the up-to-
// PLAYLIST_REFRESH_INTERVAL replay outage the reconciler exists to prevent.
func TestReplayScopeResyncReconciler(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRefresher := mocks.NewMockRefresher(ctrl)
	mockRefresher.EXPECT().ForceRefresh().Times(1)

	replayScopeResyncReconciler(mockRefresher)(context.Background())
}

// TestStatusForceRefreshReconciler covers the status poller half of what this
// branch's merge moved OFF the connect callback (it is develop's reconciler
// now). Deleting the inline call removed the repo's only assertion that a CDP
// connect force-refreshes status, so without this the regression would be
// uncovered on both sides of the move.
//
// There is deliberately no sibling TestSetupUIResyncReconciler: the other
// moved producer, setupUIResyncReconciler, takes a concrete *setupui.Service
// rather than a consumer-owned interface, so there is no seam to assert
// against without narrowing that parameter first.
func TestStatusForceRefreshReconciler(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockPoller := mocks.NewMockStatusPoller(ctrl)
	mockPoller.EXPECT().ForceRefresh().Times(1)

	statusForceRefreshReconciler(mockPoller)(context.Background())
}

func TestApp_Run_Errors(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup)
		wantErr   string
	}{
		// NOTE: "state load failure" is deliberately NOT an error case anymore:
		// run() quarantines the unreadable file and continues on an empty state
		// (see "state load failure quarantines and continues startup" in the
		// success table) — aborting crash-looped the sole recovery daemon.
		// NOTE: "DBus start failure" is deliberately NOT an error case anymore:
		// run() logs, retries Start in the background, and continues bringing up
		// the recovery surfaces (see "DBus start failure continues to READY with
		// recovery surfaces" in the success table) — aborting crash-looped the
		// sole SoftAP/LAN-recovery daemon before either surface existed.
		{
			// PR #218 review regression: a failed initial relayer connection must NOT
			// abort run(). Aborting here happens before SdNotifyReady, so a relayer
			// outage would crash-loop controld and take the in-process SoftAP
			// provisioning and LAN recovery down with it. The new contract: log, hand
			// the connection to a background RetryableConnect, and proceed all the way
			// to READY.
			name: "relayer initial connect failure continues to READY",
			setupFunc: func(ts *testSetup) {
				// Mock state load ok - relayer ready (topic present)
				ts.mockStateManager.EXPECT().
					Load(ts.logger).
					Return(&state.State{
						Relayer: &state.RelayerState{TopicID: "test-topic"},
					}, nil)
				ts.mockStateManager.EXPECT().
					ClaimSnapshot().
					Return(state.ClaimInfo{TopicID: "test-topic", TopicReady: true}).
					AnyTimes()

				// Mock Watchdog start and stop
				ts.mockWatchdog.EXPECT().Start(gomock.Any())
				ts.mockWatchdog.EXPECT().Stop()

				// Mock DBus start ok
				ts.mockDBus.EXPECT().Start().Return(nil)
				ts.mockDBus.EXPECT().Stop().Return(nil)

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

				// Mock OS ReadFile for mDNS device info. The device-name
				// record is read alongside the hostname: an unnamed unit is
				// the ordinary case, and a missing file must leave the
				// advertised name as the serial.
				ts.mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("test-hostname"), nil)
				ts.mockOS.EXPECT().ReadFile(constants.DEVICE_NAME_FILE).Return(nil, stdos.ErrNotExist)
				ts.mockOS.EXPECT().IsNotExist(stdos.ErrNotExist).Return(true)

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
				ts.mockStateManager.EXPECT().
					ClaimSnapshot().
					Return(state.ClaimInfo{TopicID: "", TopicReady: false}).
					AnyTimes()

				// CDP connects in the background; run() reaches Start + Close here.
				ts.mockCDP.EXPECT().Start(gomock.Any(), gomock.Any())
				ts.mockCDP.EXPECT().Close()

				// Mock Watchdog start and stop
				ts.mockWatchdog.EXPECT().Start(gomock.Any())
				ts.mockWatchdog.EXPECT().Stop()

				// Mock DBus start ok
				ts.mockDBus.EXPECT().Start().Return(nil)
				ts.mockDBus.EXPECT().Stop().Return(nil)

				// Mock Mediator start and stop
				ts.mockMediator.EXPECT().Start()
				ts.mockMediator.EXPECT().Stop()

				// Mock DBus call
				ts.mockDBus.EXPECT().
					Call(gomock.Any(), dbus.MONITORD_NAME, dbus.MONITORD_PATH, dbus.MONITORD_INTERFACE, dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS, true).
					Return([]interface{}{false}, nil)
				// Relayer Close is unconditional at shutdown (a later reconcile
				// may have connected); it is a no-op on a nil conn.
				ts.mockRelayer.EXPECT().Close()

				// Mock StatusPoller start and stop
				ts.mockStatusPoller.EXPECT().Start(gomock.Any())
				ts.mockStatusPoller.EXPECT().Stop()

				// Mock Refresher start and stop
				ts.mockRefresher.EXPECT().Start()
				ts.mockRefresher.EXPECT().Stop()

				// Mock Hub start and stop
				ts.mockHub.EXPECT().Start()
				ts.mockHub.EXPECT().Stop().Return(nil)

				// Mock OS ReadFile for mDNS device info. The device-name
				// record is read alongside the hostname: an unnamed unit is
				// the ordinary case, and a missing file must leave the
				// advertised name as the serial.
				ts.mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("test-hostname"), nil)
				ts.mockOS.EXPECT().ReadFile(constants.DEVICE_NAME_FILE).Return(nil, stdos.ErrNotExist)
				ts.mockOS.EXPECT().IsNotExist(stdos.ErrNotExist).Return(true)

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
				ts.mockStateManager.EXPECT().
					ClaimSnapshot().
					Return(state.ClaimInfo{TopicID: "", TopicReady: false}).
					AnyTimes()

				// CDP connects in the background; run() reaches Start + Close here.
				ts.mockCDP.EXPECT().Start(gomock.Any(), gomock.Any())
				ts.mockCDP.EXPECT().Close()

				// Mock Watchdog start and stop
				ts.mockWatchdog.EXPECT().Start(gomock.Any())
				ts.mockWatchdog.EXPECT().Stop()

				// Mock DBus start ok
				ts.mockDBus.EXPECT().Start().Return(nil)
				ts.mockDBus.EXPECT().Stop().Return(nil)

				// Mock Mediator start and stop
				ts.mockMediator.EXPECT().Start()
				ts.mockMediator.EXPECT().Stop()

				// Mock DBus call failure
				ts.mockDBus.EXPECT().
					Call(gomock.Any(), dbus.MONITORD_NAME, dbus.MONITORD_PATH, dbus.MONITORD_INTERFACE, dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS, true).
					Return(nil, errors.New("DBus call failed"))
				// Relayer Close is unconditional at shutdown (a later reconcile
				// may have connected); it is a no-op on a nil conn.
				ts.mockRelayer.EXPECT().Close()

				// Mock StatusPoller start and stop
				ts.mockStatusPoller.EXPECT().Start(gomock.Any())
				ts.mockStatusPoller.EXPECT().Stop()

				// Mock Refresher start and stop
				ts.mockRefresher.EXPECT().Start()
				ts.mockRefresher.EXPECT().Stop()

				// Mock Hub start and stop
				ts.mockHub.EXPECT().Start()
				ts.mockHub.EXPECT().Stop().Return(nil)

				// Mock OS ReadFile for mDNS device info. The device-name
				// record is read alongside the hostname: an unnamed unit is
				// the ordinary case, and a missing file must leave the
				// advertised name as the serial.
				ts.mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("test-hostname"), nil)
				ts.mockOS.EXPECT().ReadFile(constants.DEVICE_NAME_FILE).Return(nil, stdos.ErrNotExist)
				ts.mockOS.EXPECT().IsNotExist(stdos.ErrNotExist).Return(true)

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
		nil,
		// Gateway User-Agent config: absent, which is the fielded default
		// and resolves to ENABLED — so this also asserts the wiring builds
		// its interceptor with no config present. Constructing it performs
		// no I/O; it dials only from AttachOnReconnect, which this test
		// never reaches.
		nil,
		// Netlog config: disabled so the wiring test never touches the real
		// ring directory (buildNetlogRecorder would otherwise attempt
		// /home/feralfile and merely degrade to nil with a warn).
		&config.NetlogConfig{Disabled: true},
		provisioning.Tuning{},
		"com.feralfile.test",
		nil,
	)

	assert.NotNil(t, app)
	assert.Equal(t, logger, app.Logger)
	assert.NotNil(t, app.Ctx)

	// The User-Agent rewrite must be wired with NO gatewayUserAgent config
	// present: that is what every fielded device has today, and the bug it
	// fixes (#296) leaves those artworks permanently unrenderable. A nil
	// here would mean the fix ships switched off for exactly the fleet that
	// needs it.
	assert.NotNil(t, app.UARewrite, "kiosk User-Agent rewrite must default ON with absent config")

	// The app context is the daemon-lifetime context handed to long-lived
	// components (hub, claim flow); Cancel must cancel exactly it, or those
	// paths outlive shutdown on a never-canceled Background context.
	require.NotNil(t, app.Cancel)
	select {
	case <-app.Ctx.Done():
		t.Fatal("app context canceled prematurely")
	default:
	}
	app.Cancel()
	select {
	case <-app.Ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("app.Cancel did not cancel app.Ctx")
	}

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
	assert.NotNil(t, app.PlaylistScheduler)
	assert.NotNil(t, app.Hub)

	// offlineCacheConfig was nil, so the feature stays fully disabled.
	assert.Nil(t, app.KioskReplay)
	assert.Nil(t, app.OfflineCacheService)
	assert.Nil(t, app.OfflineCacheStaticServer)

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

func TestInitializeApp_OfflineCacheEnabled(t *testing.T) {
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))

	app := initializeApp(
		logger,
		"http://localhost:9222",
		"wss://test.relay.com",
		"test-api-key",
		nil,
		&config.OfflineCacheConfig{Enabled: true, RootDir: t.TempDir()},
		nil,
		&config.NetlogConfig{Disabled: true},
		provisioning.Tuning{},
		"com.feralfile.test",
		nil,
	)

	assert.NotNil(t, app)
	assert.NotNil(t, app.KioskReplay)
	assert.NotNil(t, app.OfflineCacheService)
	assert.NotNil(t, app.OfflineCacheStaticServer)
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
	ts.mockStateManager.EXPECT().ClaimSnapshot().Return(state.ClaimInfo{TopicID: "test-topic", TopicReady: true}).AnyTimes()

	ts.mockWatchdog.EXPECT().Start(gomock.Any())
	ts.mockWatchdog.EXPECT().Stop()
	ts.mockDBus.EXPECT().Start().Return(nil)
	ts.mockDBus.EXPECT().Stop().Return(nil)
	ts.mockMediator.EXPECT().Start()
	ts.mockMediator.EXPECT().Stop()

	ts.mockHub.EXPECT().Start().Do(func() { record("hub") })
	ts.mockHub.EXPECT().Stop().Return(nil)
	ts.mockOS.EXPECT().ReadFile(constants.HOSTNAME_FILE).Return([]byte("test-hostname"), nil)
	ts.mockOS.EXPECT().ReadFile(constants.DEVICE_NAME_FILE).Return(nil, stdos.ErrNotExist)
	ts.mockOS.EXPECT().IsNotExist(stdos.ErrNotExist).Return(true)
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

	assert.True(t, spy.wasStarted(), "provisioning must start")

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

// An explicit "enabled": false is the documented off switch, and it must
// actually leave the interceptor unbuilt — otherwise the daemon still opens
// a CDP session and arms Fetch on a device whose operator disabled it.
func TestInitializeAppGatewayUserAgentDisabled(t *testing.T) {
	logger := zaptest.NewLogger(t)
	disabled := false

	app := initializeApp(
		logger,
		"http://localhost:9222",
		"wss://test.relay.com",
		"test-api-key",
		nil,
		nil,
		&config.GatewayUserAgentConfig{Enabled: &disabled},
		&config.NetlogConfig{Disabled: true},
		provisioning.Tuning{},
		"com.feralfile.test",
		nil,
	)
	defer app.Cancel()

	assert.Nil(t, app.UARewrite)
}

// A host list in which NO entry is usable must degrade to the default scope,
// never abort startup: config.Load failure is fatal under Restart=always, and
// this daemon owns every recovery surface on the device, so a typo in an
// optional block must not crash-loop the box out of reach. Degrading to the
// defaults rather than to "no rewrite" is the same landing point an
// unreadable config block gets — see TestInitializeAppGatewayUserAgentKeeps-
// UsableHosts for the partial case, which is the likelier one.
func TestInitializeAppGatewayUserAgentInvalidHostsDegrades(t *testing.T) {
	logger := zaptest.NewLogger(t)

	app := initializeApp(
		logger,
		"http://localhost:9222",
		"wss://test.relay.com",
		"test-api-key",
		nil,
		nil,
		&config.GatewayUserAgentConfig{Hosts: []string{"https://"}},
		&config.NetlogConfig{Disabled: true},
		provisioning.Tuning{},
		"com.feralfile.test",
		nil,
	)
	defer app.Cancel()

	assert.NotNil(t, app, "invalid host list must not abort startup")
	// Every entry was unusable, so there is no operator scope left to
	// honor and the rewrite lands on the built-in defaults — the same
	// place an entirely unreadable config block lands.
	assert.NotNil(t, app.UARewrite, "an unusable host list must degrade to the default scope, not to no rewrite")
}

// The realistic operator edit is APPENDING a newly hostile gateway, and
// "*.newgateway.link" is the spelling validateLiteralHost's own doc calls
// likely. That typo must not take ipfs.io and dweb.link down with it: doing
// so re-opens #296 for artworks that were rendering fine, over a config edit
// about an unrelated host, with the cause visible only in a daemon log on a
// headless device.
func TestInitializeAppGatewayUserAgentKeepsUsableHosts(t *testing.T) {
	logger := zaptest.NewLogger(t)

	app := initializeApp(
		logger,
		"http://localhost:9222",
		"wss://test.relay.com",
		"test-api-key",
		nil,
		nil,
		&config.GatewayUserAgentConfig{
			Hosts: []string{"ipfs.io", "*.newgateway.link", "dweb.link"},
		},
		&config.NetlogConfig{Disabled: true},
		provisioning.Tuning{},
		"com.feralfile.test",
		nil,
	)
	defer app.Cancel()

	require.NotNil(t, app)
	assert.NotNil(t, app.UARewrite, "one bad entry must not disable the whole rewrite")
}
