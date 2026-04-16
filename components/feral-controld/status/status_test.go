package status

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/ddc"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
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
}

func (f *fakeWS) NewConnection(http.ResponseWriter, *http.Request) (string, error) { return "", nil }

func (f *fakeWS) Send(string, any) error { return nil }

func (f *fakeWS) SendAll(any) error {
	f.sendAllCalls++
	return f.sendAllErr
}

func (f *fakeWS) Close() {}

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

// fakePanelDDC captures the context for deadline inspection.
type fakePanelDDC struct {
	collectCtx chan context.Context
	status     *ddc.DdcPanelStatus
	err        error
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
