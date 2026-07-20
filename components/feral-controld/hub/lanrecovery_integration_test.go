package hub

// Offline LAN-recovery integration test (plan item P3.3).
//
// This proves the BLE-replacement recovery contract at the hub composition
// level: with the internet blackholed but the LAN up, a client on the LAN can
// execute the ex-BLE recovery command set against the device through the REAL
// command pipeline — the hub's http.ServeMux, the shared withMiddleware storm
// cap, the real commandrouter storm gate, and the real devicectl executor —
// with only the leaf OS/exec/dbus/cdp seams mocked.
//
// "Internet down / no relayer" is simulated by construction: nothing in this
// wiring touches relayer/ or mediator/ at all. There is no relayer client, no
// relayer topic is required, and the status provider reports a disconnected
// link. Every recovery command below therefore completes with zero relayer
// involvement — that relayer-independence is the whole point of the ex-BLE
// recovery channel.
//
// A real loopback httptest.Server stands in for the LAN client so requests
// traverse the actual registered mux and middleware, not just an in-process
// handler call.
//
// Scope note (mDNS): the "internet loss with link up keeps mDNS advertising"
// property is a mediator/mdns concern and is already regression-tested in
// mediator_test.go against the link-keyed advertiser. This test deliberately
// does not re-build or re-assert the advertiser; it covers the hub-side command
// pipeline composition.
//
// Scope note (uploadLogs presign+PUT): the setupOwner=controld in-process log
// upload is fire-and-forget and its HTTP client + endpoint are only reachable
// through devicectl's unexported logUploaderFactory seam, which a hub-package
// test cannot set. The presign POST + PUT wire behavior on a mocked HTTP client
// is therefore asserted at the executor level in
// devicectl/lanrecovery_uploadlogs_integration_test.go. Here, uploadLogs is
// exercised through the LAN ingress on the default (setupd) owner path, proving
// the command reaches the executor offline and is handled via the local D-Bus
// forward — itself relayer-independent.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/feral-file/godbus"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/config"
	ffdbus "github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/ddc"
	"github.com/feral-file/ffos-user/components/feral-controld/devicectl"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func lanStrPtr(s string) *string { return &s }

// errLANNotExist stands in for an fs "not found": every executor OS read misses
// in this test. Its identity does not matter because IsNotExist is mocked true.
var errLANNotExist = errors.New("lan test: not exist")

// lanRig is a fully wired offline hub: real executor -> real storm gate ->
// real hub handler chain, fronted by a loopback httptest.Server. Only the leaf
// device seams (cdp/dbus/exec/os/...) are mocked.
type lanRig struct {
	ctrl       *gomock.Controller
	server     *httptest.Server
	mockDBus   *mocks.MockDBus
	mockExec   *mocks.MockExec
	mockOS     *mocks.MockOS
	mockDevSts *mocks.MockDeviceStatus
}

// newLANRig builds the offline pipeline. gateCfg selects the storm-gate policy
// so individual sub-tests can exercise either the production defaults or a tight
// burst for the storm assertion. statusInfo is the device half of GET
// /api/status.
func newLANRig(t *testing.T, gateCfg commandrouter.GateConfig, statusInfo StatusInfo) *lanRig {
	t.Helper()
	ctrl := gomock.NewController(t)
	// FatalLevel suppresses Info/Error emission, so any detached best-effort
	// worker (e.g. setup narration, fire-and-forget upload) cannot touch the
	// testing.T logger after the sub-test returns.
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	mockCDP := mocks.NewMockCDP(ctrl)
	// Setup-narration (factory reset) is a best-effort async CDP push; tolerate
	// any/none. On this environment the player contract file is absent so
	// narration disables itself and never sends, but AnyTimes keeps the test
	// robust where it is present.
	mockCDP.EXPECT().Send(gomock.Any(), gomock.Any()).Return(map[string]any{"ok": true}, nil).AnyTimes()
	mockCDP.EXPECT().NoLogSend(gomock.Any(), gomock.Any()).Return(map[string]any{"ok": true}, nil).AnyTimes()

	mockDBus := mocks.NewMockDBus(ctrl)
	mockExec := mocks.NewMockExec(ctrl)
	mockOS := mocks.NewMockOS(ctrl)
	mockMath := mocks.NewMockMath(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	mockDevSts := mocks.NewMockDeviceStatus(ctrl)
	mockPoller := mocks.NewMockStatusPoller(ctrl)
	mockWS := mocks.NewMockWS(ctrl)

	jsonw := wrapper.NewJSON()
	panelDDC := ddc.New(mockExec, mockClock, logger)

	executor := devicectl.New(
		mockCDP, mockDBus, mockDevSts, mockPoller, panelDDC,
		jsonw, mockOS, mockExec, mockMath, mockClock, logger,
	)

	// Real command router + real storm gate. dp1 and mintPairing are unused by
	// the device-control recovery commands and are left nil.
	inner := commandrouter.New(executor, mockCDP, nil, mockPoller, nil, jsonw, logger)
	gated := commandrouter.NewGate(inner, gateCfg, logger)

	// Grab the mux the hub registers its routed+middlewared handlers onto, then
	// serve it over loopback as the "LAN client" transport.
	mux := http.NewServeMux()
	mockServer := mocks.NewMockHTTPServer(ctrl)
	mockServer.EXPECT().Handler().Return(mux).AnyTimes()

	provider := stubStatusProvider{info: statusInfo}
	New(ctx, mockWS, gated, provider, mockServer, jsonw, logger)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &lanRig{
		ctrl:       ctrl,
		server:     srv,
		mockDBus:   mockDBus,
		mockExec:   mockExec,
		mockOS:     mockOS,
		mockDevSts: mockDevSts,
	}
}

// postCast POSTs a cast envelope to the LAN hub and returns the status code and
// body. It mirrors a phone-side LAN recovery client hitting the device.
func (r *lanRig) postCast(t *testing.T, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(r.server.URL+"/api/cast", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(b)
}

// offlineStatus is a status snapshot for a device with a live LAN link but no
// internet/relayer: connectivity is reported disconnected and no topic is held.
func offlineStatus() StatusInfo {
	return StatusInfo{
		DeviceID:     "ff1-offline",
		Version:      "9.9.9",
		Claimed:      true,
		SetupState:   "claimed",
		Connectivity: "disconnected",
		TopicID:      "",
	}
}

// permissiveOSReads makes every executor OS read miss. It keeps the fire-and-
// forget in-process log upload (when exercised) from performing real network IO:
// with no log files found, the background zip aborts before any HTTP call.
func permissiveOSReads(m *mocks.MockOS) {
	m.EXPECT().ReadFile(gomock.Any()).Return(nil, errLANNotExist).AnyTimes()
	m.EXPECT().ReadDir(gomock.Any()).Return(nil, errLANNotExist).AnyTimes()
	m.EXPECT().IsNotExist(gomock.Any()).Return(true).AnyTimes()
}

// TestLANRecovery_OfflineCommandPipeline drives the full ex-BLE recovery set
// through the real offline pipeline. Each sub-test is an isolated rig (fresh
// storm-gate token buckets, fresh mocks, fresh global config/state injection).
func TestLANRecovery_OfflineCommandPipeline(t *testing.T) {
	t.Run("factoryReset starts factory-boot unit and rotates relayer topic", func(t *testing.T) {
		// setupOwner=controld -> in-process reset (systemctl start), plus the
		// security-critical persisted-topic rotation that holds in both owner modes.
		cfg := mocks.NewMockConfigManager(gomock.NewController(t))
		cfg.EXPECT().Get().Return(&config.Config{SetupOwner: lanStrPtr(config.SetupOwnerControld)}).AnyTimes()
		config.InjectConfigManagerForTesting(cfg)
		defer config.ResetForTesting()

		seeded := &state.State{Relayer: &state.RelayerState{TopicID: "relayer-topic-to-rotate"}}
		var saveCount int
		var savedTopic string
		sm := mocks.NewMockStateManager(gomock.NewController(t))
		sm.EXPECT().GetState().Return(seeded).AnyTimes()
		sm.EXPECT().Save(gomock.Any()).DoAndReturn(func(s *state.State) error {
			saveCount++
			savedTopic = s.Relayer.TopicID
			return nil
		}).AnyTimes()
		state.InjectStateManagerForTesting(sm)
		defer state.ResetForTesting()

		rig := newLANRig(t, commandrouter.DefaultGateConfig(), offlineStatus())
		permissiveOSReads(rig.mockOS)

		cmd := mocks.NewMockExecCmd(rig.ctrl)
		rig.mockExec.EXPECT().
			CommandContext(gomock.Any(), "systemctl", "start", "set-factory-boot.service").
			Return(cmd).Times(1)
		cmd.EXPECT().CombinedOutput().Return([]byte(""), nil).Times(1)

		code, body := rig.postCast(t, `{"command":"factoryReset"}`)

		assert.Equal(t, http.StatusOK, code)
		assert.JSONEq(t, `{"ok":true}`, body)
		assert.GreaterOrEqual(t, saveCount, 1, "persisted relayer topic must be saved (rotated)")
		assert.Equal(t, "", savedTopic, "persisted relayer topic must be cleared")
		assert.Equal(t, "", seeded.Relayer.TopicID, "in-memory topic must be cleared")
	})

	t.Run("reboot invokes sudo reboot", func(t *testing.T) {
		rig := newLANRig(t, commandrouter.DefaultGateConfig(), offlineStatus())
		permissiveOSReads(rig.mockOS)

		cmd := mocks.NewMockExecCmd(rig.ctrl)
		rig.mockExec.EXPECT().
			CommandContext(gomock.Any(), "sudo", "reboot", "-h", "now").
			Return(cmd).Times(1)
		cmd.EXPECT().Run().Return(nil).Times(1)

		code, body := rig.postCast(t, `{"command":"reboot"}`)
		assert.Equal(t, http.StatusOK, code)
		assert.JSONEq(t, `{"ok":true}`, body)
	})

	t.Run("shutdown invokes sudo shutdown", func(t *testing.T) {
		rig := newLANRig(t, commandrouter.DefaultGateConfig(), offlineStatus())
		permissiveOSReads(rig.mockOS)

		cmd := mocks.NewMockExecCmd(rig.ctrl)
		rig.mockExec.EXPECT().
			CommandContext(gomock.Any(), "sudo", "shutdown", "-h", "now").
			Return(cmd).Times(1)
		cmd.EXPECT().Run().Return(nil).Times(1)

		code, body := rig.postCast(t, `{"command":"shutdown"}`)
		assert.Equal(t, http.StatusOK, code)
		assert.JSONEq(t, `{"ok":true}`, body)
	})

	t.Run("updateToLatestVersion reaches the OTA gate", func(t *testing.T) {
		// FINDING (reported): otaGateInstance() hardcodes LocalOwner:false, so even
		// with setupOwner=controld the user-triggered update forwards to setupd over
		// the LOCAL D-Bus rather than driving the local updater + live HTTP version
		// check. We therefore assert the forwarder seam (the version-check HTTP path
		// is not reachable through the executor's public wiring yet). The forward is
		// still relayer-independent: it rides the local system bus, not the relayer.
		var forwardedMember godbus.Member
		rig := newLANRig(t, commandrouter.DefaultGateConfig(), offlineStatus())
		permissiveOSReads(rig.mockOS)
		rig.mockDBus.EXPECT().
			RetryableSend(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, p godbus.DBusPayload) error {
				forwardedMember = p.Member
				return nil
			}).Times(1)

		code, body := rig.postCast(t, `{"command":"updateToLatestVersion"}`)
		assert.Equal(t, http.StatusOK, code)
		assert.JSONEq(t, `{"ok":true}`, body)
		assert.Equal(t, ffdbus.SETUPD_EVENT_SYSTEM_UPDATE, forwardedMember,
			"update must reach the OTA gate and forward the system_update signal")
	})

	t.Run("uploadLogs reaches the executor offline via local D-Bus forward", func(t *testing.T) {
		// Default (setupd) owner: uploadLogs forwards over the LOCAL D-Bus to
		// setupd. This proves the command traverses the LAN ingress + storm gate +
		// executor with no relayer, and is dispatched. (The controld in-process
		// presign+PUT path is asserted in the devicectl-level test.)
		cfg := mocks.NewMockConfigManager(gomock.NewController(t))
		cfg.EXPECT().Get().Return(&config.Config{SetupOwner: lanStrPtr(config.SetupOwnerSetupd)}).AnyTimes()
		config.InjectConfigManagerForTesting(cfg)
		defer config.ResetForTesting()

		var uploadMember godbus.Member
		rig := newLANRig(t, commandrouter.DefaultGateConfig(), offlineStatus())
		permissiveOSReads(rig.mockOS)
		rig.mockDBus.EXPECT().
			RetryableSend(gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, p godbus.DBusPayload) error {
				uploadMember = p.Member
				return nil
			}).Times(1)

		body := `{"command":"uploadLogs","request":{"userId":"u","apiKey":"k","title":"t"}}`
		code, respBody := rig.postCast(t, body)
		assert.Equal(t, http.StatusOK, code)
		assert.JSONEq(t, `{"ok":true}`, respBody)
		assert.Equal(t, ffdbus.SETUPD_EVENT_UPLOAD_LOGS, uploadMember,
			"uploadLogs must reach the executor and forward the upload_logs signal")
	})

	t.Run("status read returns contract 1 while offline", func(t *testing.T) {
		rig := newLANRig(t, commandrouter.DefaultGateConfig(), offlineStatus())

		resp, err := http.Get(rig.server.URL + "/api/status")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		var got map[string]any
		require.NoError(t, json.Unmarshal(b, &got))
		assert.Equal(t, StatusContract, got["contract"])
		assert.Equal(t, "1", got["contract"])
		assert.Equal(t, "ff1-offline", got["device_id"])
		assert.Equal(t, "disconnected", got["connectivity"], "device is offline (no internet/relayer)")
	})
}

// TestLANRecovery_StormLimiterProtectsOfflinePipeline proves the storm gate is
// live on the offline LAN command path: a burst beyond the configured budget is
// shed with 429 while the in-budget requests succeed through the real executor.
// It uses the same technique as TestHandleCast_StormProtection but drives a real
// device-control command end to end over the loopback LAN transport.
func TestLANRecovery_StormLimiterProtectsOfflinePipeline(t *testing.T) {
	gateCfg := commandrouter.GateConfig{
		Enabled:       true,
		MaxConcurrent: 16,
		Policies: map[commands.Type]commandrouter.Policy{
			// Tight, non-deduped budget so a burst is deterministically shed.
			commands.CMD_DEVICE_STATUS: {Rate: 1, Burst: 2, Weight: 1},
		},
		Default: commandrouter.Policy{Rate: 1, Burst: 2, Weight: 1},
	}
	rig := newLANRig(t, gateCfg, offlineStatus())
	permissiveOSReads(rig.mockOS)
	rig.mockDevSts.EXPECT().
		GetStatus(gomock.Any()).
		Return(&status.DeviceStatusResponse{}, nil).
		AnyTimes()

	const total = 5
	var accepted, limited int
	for i := 0; i < total; i++ {
		code, _ := rig.postCast(t, `{"command":"getDeviceStatus"}`)
		switch code {
		case http.StatusTooManyRequests:
			limited++
		case http.StatusOK:
			accepted++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}

	assert.Equal(t, 2, accepted, "only the in-budget burst of 2 is accepted")
	assert.Equal(t, total-2, limited, "the rest are shed with 429 by the storm gate")
}
