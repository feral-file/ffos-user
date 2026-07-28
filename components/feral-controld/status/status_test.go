package status

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/ddc"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

type fakeRelayer struct {
	connectedResponses []bool
	connectedCalls     int
	sendResponses      []error
	sendCalls          int
}

func (f *fakeRelayer) IsConnected() bool {
	if len(f.connectedResponses) == 0 {
		return false
	}
	idx := f.connectedCalls
	if idx >= len(f.connectedResponses) {
		idx = len(f.connectedResponses) - 1
	}
	f.connectedCalls++
	return f.connectedResponses[idx]
}

func (f *fakeRelayer) Connect(context.Context) error { return nil }

func (f *fakeRelayer) RetryableConnect(context.Context) error { return nil }

func (f *fakeRelayer) Send(context.Context, interface{}) error {
	var err error
	if len(f.sendResponses) > 0 {
		idx := f.sendCalls
		if idx >= len(f.sendResponses) {
			idx = len(f.sendResponses) - 1
		}
		err = f.sendResponses[idx]
	}
	f.sendCalls++
	return err
}

func (f *fakeRelayer) OnRelayerMessage(relayer.Handler) {}

func (f *fakeRelayer) RemoveRelayerMessage(relayer.Handler) {}

func (f *fakeRelayer) Close() {}

type fakeWS struct {
	sendAllCalls int
	sendAllErr   error
	lastPayload  any
}

func (f *fakeWS) NewConnection(http.ResponseWriter, *http.Request) (string, error) { return "", nil }

func (f *fakeWS) Send(string, any) error { return nil }

func (f *fakeWS) SendAll(payload any) error {
	f.sendAllCalls++
	f.lastPayload = payload
	return f.sendAllErr
}

func (f *fakeWS) Close() {}

type fakeCDP struct {
	pageNavigationURL      string
	pageNavigationURLError error
	noLogSendCalls         int
	noLogSendResult        any
	noLogSendErr           error
	// notInitialized inverts Initialized() so the zero value reports a connected client
	// (the common case for these tests); set it to exercise the CDP-absent skip path.
	notInitialized bool
}

func (f *fakeCDP) Init(context.Context) error { return nil }

func (f *fakeCDP) Start(context.Context, func()) {}

func (f *fakeCDP) Send(string, map[string]interface{}) (interface{}, error) { return nil, nil }

func (f *fakeCDP) NoLogSend(string, map[string]interface{}) (interface{}, error) {
	f.noLogSendCalls++
	return f.noLogSendResult, f.noLogSendErr
}

func (f *fakeCDP) PageNavigationURL(context.Context) (string, error) {
	return f.pageNavigationURL, f.pageNavigationURLError
}

func (f *fakeCDP) Close() {}

func (f *fakeCDP) Initialized() bool { return !f.notInitialized }

type fakeDeviceStatus struct {
	status *DeviceStatusResponse
	err    error
	calls  int
}

func (f *fakeDeviceStatus) GetStatus(context.Context) (*DeviceStatusResponse, error) {
	f.calls++
	return f.status, f.err
}

func TestSendNotification_RelayerCatchesUpAfterReconnect(t *testing.T) {
	fRelayer := &fakeRelayer{
		connectedResponses: []bool{false, true},
		sendResponses:      []error{nil},
	}
	fWS := &fakeWS{}

	p := &poller{
		relayer:                 fRelayer,
		ws:                      fWS,
		logger:                  zap.NewNop(),
		lastRelayerStatusHashes: make(map[relayer.NotificationType]string),
		lastWSStatusHashes:      make(map[relayer.NotificationType]string),
	}

	message := map[string]interface{}{
		"ok": true,
	}

	ctx := context.Background()
	p.sendNotification(ctx, relayer.NOTIFICATION_TYPE_PLAYER_STATUS, message)
	p.sendNotification(ctx, relayer.NOTIFICATION_TYPE_PLAYER_STATUS, message)

	if fRelayer.sendCalls != 1 {
		t.Fatalf("expected relayer to receive one catch-up send, got %d", fRelayer.sendCalls)
	}
	if fWS.sendAllCalls != 1 {
		t.Fatalf("expected websocket to receive one send due to dedupe, got %d", fWS.sendAllCalls)
	}
}

func TestSendNotification_RetryRelayerWhenSendFails(t *testing.T) {
	fRelayer := &fakeRelayer{
		connectedResponses: []bool{true, true},
		sendResponses:      []error{errors.New("send failed"), nil},
	}
	fWS := &fakeWS{}

	p := &poller{
		relayer:                 fRelayer,
		ws:                      fWS,
		logger:                  zap.NewNop(),
		lastRelayerStatusHashes: make(map[relayer.NotificationType]string),
		lastWSStatusHashes:      make(map[relayer.NotificationType]string),
	}

	message := map[string]interface{}{
		"ok": true,
	}

	ctx := context.Background()
	p.sendNotification(ctx, relayer.NOTIFICATION_TYPE_PLAYER_STATUS, message)
	p.sendNotification(ctx, relayer.NOTIFICATION_TYPE_PLAYER_STATUS, message)

	if fRelayer.sendCalls != 2 {
		t.Fatalf("expected relayer to retry unchanged status after send failure, got %d sends", fRelayer.sendCalls)
	}
	if fWS.sendAllCalls != 1 {
		t.Fatalf("expected websocket dedupe to send once, got %d", fWS.sendAllCalls)
	}
}

func TestPollDeviceStatus_SendsDeviceStatus(t *testing.T) {
	fRelayer := &fakeRelayer{connectedResponses: []bool{true}}
	fWS := &fakeWS{}
	fDeviceStatus := &fakeDeviceStatus{
		status: &DeviceStatusResponse{ScreenRotation: "landscape"},
	}

	p := &poller{
		relayer:                 fRelayer,
		ws:                      fWS,
		deviceStatus:            fDeviceStatus,
		logger:                  zap.NewNop(),
		lastRelayerStatusHashes: make(map[relayer.NotificationType]string),
		lastWSStatusHashes:      make(map[relayer.NotificationType]string),
	}

	p.pollDeviceStatus(context.Background())

	if fWS.sendAllCalls != 1 {
		t.Fatalf("expected one device status send, got %d", fWS.sendAllCalls)
	}
	if fDeviceStatus.calls != 1 {
		t.Fatalf("expected one device status read, got %d", fDeviceStatus.calls)
	}

	payload, ok := fWS.lastPayload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected payload to be a map, got %T", fWS.lastPayload)
	}
	message, ok := payload["message"].(*DeviceStatusResponse)
	if !ok {
		t.Fatalf("expected message to be *DeviceStatusResponse, got %T", payload["message"])
	}
	if message.ScreenRotation != "landscape" {
		t.Fatalf("expected screenRotation to be preserved, got %+v", message.ScreenRotation)
	}
}

func TestPollPlayerStatus_SkipsWhenNotOnPlayerPage(t *testing.T) {
	mockCDP := &fakeCDP{
		pageNavigationURL: "file:///opt/feral/ui/launcher/index.html?step=qr",
	}
	mockRelayer := &fakeRelayer{connectedResponses: []bool{true}}
	mockWS := &fakeWS{}

	p := &poller{
		cdp:                     mockCDP,
		relayer:                 mockRelayer,
		ws:                      mockWS,
		json:                    wrapper.NewJSON(),
		logger:                  zap.NewNop(),
		lastRelayerStatusHashes: make(map[relayer.NotificationType]string),
		lastWSStatusHashes:      make(map[relayer.NotificationType]string),
	}

	p.pollPlayerStatus(context.Background())

	if mockWS.sendAllCalls != 0 {
		t.Fatalf("expected no websocket send while launcher is shown, got %d", mockWS.sendAllCalls)
	}
	if mockRelayer.sendCalls != 0 {
		t.Fatalf("expected no relayer send while launcher is shown, got %d", mockRelayer.sendCalls)
	}
}

func TestPollPlayerStatus_SkipsWhenNotOnPlayerPageAdvancesPlaybackMetrics(t *testing.T) {
	mockCDP := &fakeCDP{
		pageNavigationURL: "file:///opt/feral/ui/launcher/index.html?step=qr",
	}
	mockRelayer := &fakeRelayer{connectedResponses: []bool{true}}
	mockWS := &fakeWS{}

	p := &poller{
		cdp:                       mockCDP,
		relayer:                   mockRelayer,
		ws:                        mockWS,
		logger:                    zap.NewNop(),
		lastPlaybackSampleAt:      time.Now().Add(-2 * time.Second),
		lastIsPlaying:             true,
		playbackSampleInitialized: true,
		lastRelayerStatusHashes:   make(map[relayer.NotificationType]string),
		lastWSStatusHashes:        make(map[relayer.NotificationType]string),
	}

	before := counterValue(artPlaybackDurationSecondsTotal)
	p.pollPlayerStatus(context.Background())
	after := counterValue(artPlaybackDurationSecondsTotal)

	if after <= before {
		t.Fatalf("expected playback duration counter to advance when skipping launcher page, before=%v after=%v", before, after)
	}
}

func TestPollPlayerStatus_ContinuesWhenPageURLReadFails(t *testing.T) {
	mockCDP := &fakeCDP{
		pageNavigationURLError: errors.New("target unavailable"),
	}
	mockRelayer := &fakeRelayer{connectedResponses: []bool{true}}
	mockWS := &fakeWS{}

	p := &poller{
		cdp:                     mockCDP,
		relayer:                 mockRelayer,
		ws:                      mockWS,
		json:                    wrapper.NewJSON(),
		logger:                  zap.NewNop(),
		lastRelayerStatusHashes: make(map[relayer.NotificationType]string),
		lastWSStatusHashes:      make(map[relayer.NotificationType]string),
	}

	p.pollPlayerStatus(context.Background())

	if mockCDP.noLogSendCalls != 1 {
		t.Fatalf("expected checkStatus to run when page URL cannot be read, got %d calls", mockCDP.noLogSendCalls)
	}
}

func TestPollPlayerStatus_SkipsWhenCDPNotConnected(t *testing.T) {
	// Headless / mid-reconnect: CDP reports not connected. The poll must skip entirely
	// (no checkStatus send, no error notification) so logs and Sentry are not flooded.
	mockCDP := &fakeCDP{
		notInitialized:    true,
		pageNavigationURL: constants.WEBAPP_URL,
	}
	mockRelayer := &fakeRelayer{connectedResponses: []bool{true}}
	mockWS := &fakeWS{}

	p := &poller{
		cdp:                     mockCDP,
		relayer:                 mockRelayer,
		ws:                      mockWS,
		json:                    wrapper.NewJSON(),
		logger:                  zap.NewNop(),
		lastRelayerStatusHashes: make(map[relayer.NotificationType]string),
		lastWSStatusHashes:      make(map[relayer.NotificationType]string),
	}

	p.pollPlayerStatus(context.Background())

	if mockCDP.noLogSendCalls != 0 {
		t.Fatalf("expected no checkStatus send while CDP is disconnected, got %d", mockCDP.noLogSendCalls)
	}
	if mockWS.sendAllCalls != 0 {
		t.Fatalf("expected no websocket notification while CDP is disconnected, got %d", mockWS.sendAllCalls)
	}
	if mockRelayer.sendCalls != 0 {
		t.Fatalf("expected no relayer notification while CDP is disconnected, got %d", mockRelayer.sendCalls)
	}
}

func TestPollPlayerStatus_PollsWhenOnPlayerPage(t *testing.T) {
	mockCDP := &fakeCDP{
		pageNavigationURL: constants.WEBAPP_URL,
	}
	mockRelayer := &fakeRelayer{connectedResponses: []bool{true}}
	mockWS := &fakeWS{}

	p := &poller{
		cdp:                     mockCDP,
		relayer:                 mockRelayer,
		ws:                      mockWS,
		json:                    wrapper.NewJSON(),
		logger:                  zap.NewNop(),
		lastRelayerStatusHashes: make(map[relayer.NotificationType]string),
		lastWSStatusHashes:      make(map[relayer.NotificationType]string),
	}

	p.pollPlayerStatus(context.Background())

	if mockCDP.noLogSendCalls != 1 {
		t.Fatalf("expected one checkStatus call on the player page, got %d", mockCDP.noLogSendCalls)
	}
}

// TestPollPlayerStatus_ForwardsRenderStatus pins renderStatus across the
// checkStatus -> typed PlayerStatus -> notification hop. PlayerStatus is a
// typed struct, so a field it does not model is dropped in that marshal /
// unmarshal round trip and never reaches controllers.
func TestPollPlayerStatus_ForwardsRenderStatus(t *testing.T) {
	mockCDP := &fakeCDP{
		pageNavigationURL: constants.WEBAPP_URL,
		noLogSendResult: map[string]interface{}{
			"message": map[string]interface{}{
				"ok":           true,
				"castCommand":  "displayPlaylist",
				"index":        1,
				"renderStatus": 2,
				"isPaused":     false,
			},
		},
	}
	mockRelayer := &fakeRelayer{connectedResponses: []bool{true}}
	mockWS := &fakeWS{}

	p := &poller{
		cdp:                     mockCDP,
		relayer:                 mockRelayer,
		ws:                      mockWS,
		json:                    wrapper.NewJSON(),
		logger:                  zap.NewNop(),
		lastRelayerStatusHashes: make(map[relayer.NotificationType]string),
		lastWSStatusHashes:      make(map[relayer.NotificationType]string),
	}

	p.pollPlayerStatus(context.Background())

	if mockWS.sendAllCalls != 1 {
		t.Fatalf("expected one websocket send, got %d", mockWS.sendAllCalls)
	}

	payload, ok := mockWS.lastPayload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected websocket payload map, got %T", mockWS.lastPayload)
	}
	message, ok := payload["message"].(*PlayerStatus)
	if !ok {
		t.Fatalf("expected payload message to be *PlayerStatus, got %T", payload["message"])
	}
	if message.RenderStatus == nil || *message.RenderStatus != 2 {
		t.Fatalf("expected renderStatus to survive polling, got %+v", message.RenderStatus)
	}
	if message.Index == nil || *message.Index != 1 {
		t.Fatalf("expected index to survive polling, got %+v", message.Index)
	}
}

func TestPollRound_ClearsDedupHashesWhenCDPReconnects(t *testing.T) {
	fCDP := &fakeCDP{notInitialized: true}
	p := &poller{
		cdp:     fCDP,
		relayer: &fakeRelayer{}, // never connected: device/DDC polls skip themselves
		ws:      &fakeWS{},
		logger:  zap.NewNop(),
		lastRelayerStatusHashes: map[relayer.NotificationType]string{
			relayer.NOTIFICATION_TYPE_PLAYER_STATUS: "pre-restart",
		},
		lastWSStatusHashes: map[relayer.NotificationType]string{
			relayer.NOTIFICATION_TYPE_PLAYER_STATUS: "pre-restart",
		},
	}
	ctx := context.Background()

	// CDP still down: the caches describe the last state actually pushed and must
	// survive so the down-window itself does not force re-sends.
	p.pollRound(ctx)
	if len(p.lastRelayerStatusHashes) != 1 || len(p.lastWSStatusHashes) != 1 {
		t.Fatalf("expected dedup hashes to survive while CDP stays down, got %d/%d entries",
			len(p.lastRelayerStatusHashes), len(p.lastWSStatusHashes))
	}

	// CDP (re)connects: a restarted Chromium reloaded the web app from defaults, so
	// "unchanged since last push" no longer proves clients have the state — both
	// caches must be dropped for one fresh push (the old PartOf= design got this by
	// restarting controld).
	fCDP.notInitialized = false
	p.pollRound(ctx)
	if len(p.lastRelayerStatusHashes) != 0 || len(p.lastWSStatusHashes) != 0 {
		t.Fatalf("expected dedup hashes cleared on CDP reconnect, got %d/%d entries",
			len(p.lastRelayerStatusHashes), len(p.lastWSStatusHashes))
	}

	// Steady state after the reconnect tick: no further clearing churn.
	p.lastWSStatusHashes[relayer.NOTIFICATION_TYPE_PLAYER_STATUS] = "fresh"
	p.pollRound(ctx)
	if len(p.lastWSStatusHashes) != 1 {
		t.Fatalf("expected dedup hashes kept while CDP stays connected, got %d entries",
			len(p.lastWSStatusHashes))
	}
}

// fakePanelDDC captures the context for deadline inspection.
type fakePanelDDC struct {
	collectCtx chan context.Context
	status     *ddc.DdcPanelStatus
	err        error
	// noPoll simulates the availability tracker's "unsupported" verdict.
	noPoll bool
	// shouldPollCalls counts ShouldPoll invocations; the real implementation
	// refreshes the display fingerprint as a side effect, so the poller must
	// call it every round even when the no-display gate skips the poll.
	shouldPollCalls int
	// shouldPollSeq scripts individual ShouldPoll answers (consumed in order,
	// then falling back to !noPoll) so a single pollDDCStatus call can see the
	// gate open and the post-collect verdict still closed, the way a failed
	// reprobe of an unsupported panel does.
	shouldPollSeq []bool
}

func (f *fakePanelDDC) CollectStatus(ctx context.Context) (*ddc.DdcPanelStatus, error) {
	if f.collectCtx != nil {
		f.collectCtx <- ctx
	}
	return f.status, f.err
}

func (f *fakePanelDDC) ApplyControl(context.Context, ddc.DdcPanelAction, json.RawMessage) error {
	return nil
}

func (f *fakePanelDDC) ShouldPoll() bool {
	f.shouldPollCalls++
	if len(f.shouldPollSeq) > 0 {
		v := f.shouldPollSeq[0]
		f.shouldPollSeq = f.shouldPollSeq[1:]
		return v
	}
	return !f.noPoll
}
func (f *fakePanelDDC) Generation() uint64 { return 0 }

func TestPollDDCStatus_ContextCarriesTimeout(t *testing.T) {
	ctxCh := make(chan context.Context, 1)
	fakeDDC := &fakePanelDDC{
		collectCtx: ctxCh,
		status:     &ddc.DdcPanelStatus{},
	}
	fRelayer := &fakeRelayer{connectedResponses: []bool{true}}
	fWS := &fakeWS{}

	p := &poller{
		relayer:                 fRelayer,
		ws:                      fWS,
		panelDDC:                fakeDDC,
		logger:                  zap.NewNop(),
		lastRelayerStatusHashes: make(map[relayer.NotificationType]string),
		lastWSStatusHashes:      make(map[relayer.NotificationType]string),
	}

	p.pollDDCStatus(context.Background())

	select {
	case received := <-ctxCh:
		deadline, ok := received.Deadline()
		if !ok {
			t.Fatal("expected CollectStatus context to carry a deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > ddcPollTimeout {
			t.Fatalf("deadline should be within (0, %v], got %v remaining", ddcPollTimeout, remaining)
		}
	default:
		t.Fatal("CollectStatus was not called")
	}
}

func TestPollDDCStatus_TimeoutCancelsHangingCollect(t *testing.T) {
	t.Parallel()

	ctxCh := make(chan context.Context, 1)

	fRelayer := &fakeRelayer{connectedResponses: []bool{true}}
	fWS := &fakeWS{}

	p := &poller{
		relayer:                 fRelayer,
		ws:                      fWS,
		panelDDC:                &blockingPanelDDC{ctxCh: ctxCh},
		logger:                  zap.NewNop(),
		lastRelayerStatusHashes: make(map[relayer.NotificationType]string),
		lastWSStatusHashes:      make(map[relayer.NotificationType]string),
	}

	// Use a parent context with a very short timeout to make the test fast.
	// The DDC timeout derives from this parent, so it will be the shorter of the two.
	parentCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		p.pollDDCStatus(parentCtx)
		close(done)
	}()

	select {
	case <-done:
		// pollDDCStatus returned — the timeout worked.
	case <-time.After(2 * time.Second):
		t.Fatal("pollDDCStatus did not return within 2s; timeout is not bounding the DDC call")
	}
}

// blockingPanelDDC blocks CollectStatus until the context is canceled.
type blockingPanelDDC struct {
	ctxCh chan context.Context
}

func (b *blockingPanelDDC) CollectStatus(ctx context.Context) (*ddc.DdcPanelStatus, error) {
	if b.ctxCh != nil {
		b.ctxCh <- ctx
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *blockingPanelDDC) ApplyControl(context.Context, ddc.DdcPanelAction, json.RawMessage) error {
	return nil
}

func (b *blockingPanelDDC) ShouldPoll() bool   { return true }
func (b *blockingPanelDDC) Generation() uint64 { return 0 }

// TestPollDDCStatus_SkipsWhenNoDisplayConnected pins the headless gate: with no
// DRM connector connected, ddcutil can never find a display, so the 5s poll
// must not shell out to ddcutil at all (previously an info+warn+error log
// triplet every round, forever, on headless devices).
func TestPollDDCStatus_SkipsWhenNoDisplayConnected(t *testing.T) {
	ctxCh := make(chan context.Context, 1)
	fakeDDC := &fakePanelDDC{
		collectCtx: ctxCh,
		status:     &ddc.DdcPanelStatus{},
	}
	fRelayer := &fakeRelayer{connectedResponses: []bool{true}}

	p := &poller{
		relayer:                 fRelayer,
		ws:                      &fakeWS{},
		panelDDC:                fakeDDC,
		displayConnected:        func() bool { return false },
		logger:                  zap.NewNop(),
		lastRelayerStatusHashes: make(map[relayer.NotificationType]string),
		lastWSStatusHashes:      make(map[relayer.NotificationType]string),
	}

	p.pollDDCStatus(context.Background())

	select {
	case <-ctxCh:
		t.Fatal("CollectStatus must not run while no display is connected")
	default:
	}

	// The skip still pushes a one-shot "panel unreadable" status so consumers
	// drop any cached panel values; repeats are collapsed by the hash dedup.
	if fRelayer.sendCalls != 1 {
		t.Fatalf("expected exactly one unavailable-status notification, got %d", fRelayer.sendCalls)
	}
	p.pollDDCStatus(context.Background())
	if fRelayer.sendCalls != 1 {
		t.Fatalf("expected dedup to suppress the repeat notification, got %d sends", fRelayer.sendCalls)
	}
}

// TestPollDDCStatus_NoDisplaySkipStillObservesFingerprint pins the tracker's
// visibility into off periods: ShouldPoll (whose side effect is the per-tick
// display-fingerprint refresh) must run even on rounds the no-display gate
// skips. Otherwise a monitor powered off and back on with an unchanged end
// fingerprint is invisible to the tracker, and verdicts latched around
// power-off (unsupported, recovery-futile) survive the power cycle and kill
// ddc_status forever.
func TestPollDDCStatus_NoDisplaySkipStillObservesFingerprint(t *testing.T) {
	fakeDDC := &fakePanelDDC{status: &ddc.DdcPanelStatus{}}
	fRelayer := &fakeRelayer{connectedResponses: []bool{true}}

	p := &poller{
		relayer:                 fRelayer,
		ws:                      &fakeWS{},
		panelDDC:                fakeDDC,
		displayConnected:        func() bool { return false },
		logger:                  zap.NewNop(),
		lastRelayerStatusHashes: make(map[relayer.NotificationType]string),
		lastWSStatusHashes:      make(map[relayer.NotificationType]string),
	}

	p.pollDDCStatus(context.Background())

	if fakeDDC.shouldPollCalls != 1 {
		t.Fatalf("ShouldPoll must run on no-display rounds to refresh the fingerprint, got %d calls", fakeDDC.shouldPollCalls)
	}
}

// TestPollDDCStatus_PollsAgainOnceDisplayConnects proves the gate is evaluated
// per round: plugging a monitor in (connector flips to "connected") resumes DDC
// polling on the next tick with no restart required.
func TestPollDDCStatus_PollsAgainOnceDisplayConnects(t *testing.T) {
	ctxCh := make(chan context.Context, 1)
	fakeDDC := &fakePanelDDC{
		collectCtx: ctxCh,
		status:     &ddc.DdcPanelStatus{},
	}
	fRelayer := &fakeRelayer{connectedResponses: []bool{true, true}}

	connected := false
	p := &poller{
		relayer:                 fRelayer,
		ws:                      &fakeWS{},
		panelDDC:                fakeDDC,
		displayConnected:        func() bool { return connected },
		logger:                  zap.NewNop(),
		lastRelayerStatusHashes: make(map[relayer.NotificationType]string),
		lastWSStatusHashes:      make(map[relayer.NotificationType]string),
	}

	p.pollDDCStatus(context.Background())
	select {
	case <-ctxCh:
		t.Fatal("CollectStatus must not run while no display is connected")
	default:
	}

	connected = true
	p.pollDDCStatus(context.Background())
	select {
	case <-ctxCh:
	default:
		t.Fatal("CollectStatus should run once a display is connected")
	}
}

// TestPollDDCStatus_FailedReprobeSendsSteadyUnsupportedPayload pins the wire
// contract for the powered-off-monitor steady state: a reprobe round whose
// collect still fails against an unchanged tracker verdict (ShouldPoll stays
// false afterwards) must push the SAME "display does not support DDC/CI"
// payload the skip rounds push. Pre-fix it pushed the raw per-field ddcutil
// errors, so skip rounds and reprobe rounds alternated payloads under the
// per-type dedup hash and the relayer received BOTH payloads every reprobe
// lease, forever, while a monitor was merely powered off.
func TestPollDDCStatus_FailedReprobeSendsSteadyUnsupportedPayload(t *testing.T) {
	fakeDDC := &fakePanelDDC{
		status: &ddc.DdcPanelStatus{
			Errors: map[string]string{"power": "No displays implementing DDC/CI found: exit status 1"},
		},
		// Reprobe round: gate open, then still-unsupported after the failed
		// collect. Every later round takes the noPoll skip path.
		shouldPollSeq: []bool{true, false},
		noPoll:        true,
	}
	fRelayer := &fakeRelayer{connectedResponses: []bool{true}}
	fWS := &fakeWS{}
	p := &poller{
		relayer:                 fRelayer,
		ws:                      fWS,
		panelDDC:                fakeDDC,
		displayConnected:        func() bool { return true },
		logger:                  zap.NewNop(),
		lastRelayerStatusHashes: make(map[relayer.NotificationType]string),
		lastWSStatusHashes:      make(map[relayer.NotificationType]string),
	}

	// The failed reprobe round must send the steady unsupported payload, not
	// the raw error fields.
	p.pollDDCStatus(context.Background())
	if fRelayer.sendCalls != 1 {
		t.Fatalf("expected the failed reprobe to send one notification, got %d", fRelayer.sendCalls)
	}
	data, ok := fWS.lastPayload.(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected websocket payload type %T", fWS.lastPayload)
	}
	st, ok := data["message"].(*ddc.DdcPanelStatus)
	if !ok {
		t.Fatalf("unexpected notification message type %T", data["message"])
	}
	if st.Errors["panel"] != "display does not support DDC/CI" {
		t.Fatalf("failed reprobe must push the steady unsupported payload, got %+v", st)
	}

	// A following skip round pushes the identical payload — the dedup hash
	// must collapse it instead of alternating.
	p.pollDDCStatus(context.Background())
	if fRelayer.sendCalls != 1 {
		t.Fatalf("expected dedup to collapse the skip-round repeat, got %d sends", fRelayer.sendCalls)
	}
}

// TestPollDDCStatus_UnsupportedIsQuietSkipWithOneNotification pins the
// poller's contract with the ddc availability tracker: ShouldPoll()==false
// means "display has no DDC/CI" — no CollectStatus call, no Error-level log
// (the pre-tracker code error-logged every 5s round forever), and exactly one
// dedup-collapsed "panel unreadable" notification so consumers drop cached
// panel values instead of showing them stale forever.
func TestPollDDCStatus_UnsupportedIsQuietSkipWithOneNotification(t *testing.T) {
	core, observed := observer.New(zap.ErrorLevel)

	ctxCh := make(chan context.Context, 1)
	fRelayer := &fakeRelayer{connectedResponses: []bool{true}}
	p := &poller{
		relayer:                 fRelayer,
		ws:                      &fakeWS{},
		panelDDC:                &fakePanelDDC{collectCtx: ctxCh, noPoll: true},
		displayConnected:        func() bool { return true },
		logger:                  zap.New(core),
		lastRelayerStatusHashes: make(map[relayer.NotificationType]string),
		lastWSStatusHashes:      make(map[relayer.NotificationType]string),
	}

	p.pollDDCStatus(context.Background())
	p.pollDDCStatus(context.Background())

	select {
	case <-ctxCh:
		t.Fatal("CollectStatus must not run while the tracker says unsupported")
	default:
	}
	if fRelayer.sendCalls != 1 {
		t.Fatalf("expected exactly one unavailable-status notification (dedup collapses repeats), got %d", fRelayer.sendCalls)
	}
	if n := observed.Len(); n != 0 {
		t.Fatalf("expected no Error-level logs for an unsupported display, got %d: %v",
			n, observed.All())
	}
}

// TestPlayerStatus_DefaultDurationRoundTrip guards the checkStatus -> typed
// unmarshal -> player_status re-marshal bridge for deviceSettings.defaultDuration.
// PlayerStatus is a typed struct, so any field missing from it is silently
// dropped between the player and controllers; this is the regression the
// field addition exists to prevent.
func TestPlayerStatus_DefaultDurationRoundTrip(t *testing.T) {
	raw := []byte(`{
		"ok": true,
		"index": 0,
		"deviceSettings": {"scaling": "fit", "orientation": "landscape", "defaultDuration": 600}
	}`)

	var status PlayerStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatalf("unmarshal checkStatus reply: %v", err)
	}
	if status.DeviceSettings == nil || status.DeviceSettings.DefaultDuration == nil {
		t.Fatal("deviceSettings.defaultDuration was dropped on unmarshal")
	}
	if *status.DeviceSettings.DefaultDuration != 600 {
		t.Fatalf("defaultDuration = %v, want 600", *status.DeviceSettings.DefaultDuration)
	}

	remarshaled, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("re-marshal player status: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(remarshaled, &wire); err != nil {
		t.Fatalf("parse re-marshaled status: %v", err)
	}
	ds, ok := wire["deviceSettings"].(map[string]any)
	if !ok {
		t.Fatal("deviceSettings missing from re-marshaled status")
	}
	if got := ds["defaultDuration"]; got != float64(600) {
		t.Fatalf("re-marshaled defaultDuration = %v, want 600", got)
	}
}

// TestPlayerStatus_DefaultDurationOmittedWhenAbsent ensures current-firmware
// replies (no defaultDuration) re-marshal without inventing the field.
func TestPlayerStatus_DefaultDurationOmittedWhenAbsent(t *testing.T) {
	raw := []byte(`{
		"ok": true,
		"index": 0,
		"deviceSettings": {"scaling": "fit", "orientation": "landscape"}
	}`)

	var status PlayerStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatalf("unmarshal checkStatus reply: %v", err)
	}
	remarshaled, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("re-marshal player status: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(remarshaled, &wire); err != nil {
		t.Fatalf("parse re-marshaled status: %v", err)
	}
	ds, ok := wire["deviceSettings"].(map[string]any)
	if !ok {
		t.Fatal("deviceSettings missing from re-marshaled status")
	}
	if _, present := ds["defaultDuration"]; present {
		t.Fatal("defaultDuration should be omitted when the player did not report one")
	}
}

// TestPlayerStatus_TombstoneRoundTrip guards deviceSettings.tombstone across
// the same checkStatus -> typed unmarshal -> player_status re-marshal bridge.
// ff-player #255 reports the field; without it here the label renders on the
// wall but ff-app's On/Off/Timed control has no current value to show.
func TestPlayerStatus_TombstoneRoundTrip(t *testing.T) {
	raw := []byte(`{
		"ok": true,
		"index": 0,
		"deviceSettings": {"scaling": "fit", "orientation": "landscape", "tombstone": "on"}
	}`)

	var status PlayerStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatalf("unmarshal checkStatus reply: %v", err)
	}
	if status.DeviceSettings == nil || status.DeviceSettings.Tombstone == nil {
		t.Fatal("deviceSettings.tombstone was dropped on unmarshal")
	}
	if *status.DeviceSettings.Tombstone != "on" {
		t.Fatalf("tombstone = %q, want \"on\"", *status.DeviceSettings.Tombstone)
	}

	remarshaled, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("re-marshal player status: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(remarshaled, &wire); err != nil {
		t.Fatalf("parse re-marshaled status: %v", err)
	}
	ds, ok := wire["deviceSettings"].(map[string]any)
	if !ok {
		t.Fatal("deviceSettings missing from re-marshaled status")
	}
	if got := ds["tombstone"]; got != "on" {
		t.Fatalf("re-marshaled tombstone = %v, want \"on\"", got)
	}
}

// TestPlayerStatus_TombstoneOmittedWhenAbsent ensures a player that never had
// a tombstone mode set re-marshals without inventing one — absence is what
// tells ff-app to show the "timed" fallback rather than a stored choice.
func TestPlayerStatus_TombstoneOmittedWhenAbsent(t *testing.T) {
	raw := []byte(`{
		"ok": true,
		"index": 0,
		"deviceSettings": {"scaling": "fit", "orientation": "landscape"}
	}`)

	var status PlayerStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatalf("unmarshal checkStatus reply: %v", err)
	}
	remarshaled, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("re-marshal player status: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(remarshaled, &wire); err != nil {
		t.Fatalf("parse re-marshaled status: %v", err)
	}
	ds, ok := wire["deviceSettings"].(map[string]any)
	if !ok {
		t.Fatal("deviceSettings missing from re-marshaled status")
	}
	if _, present := ds["tombstone"]; present {
		t.Fatal("tombstone should be omitted when the player did not report one")
	}
}
