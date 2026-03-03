package status

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"go.uber.org/zap"

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
