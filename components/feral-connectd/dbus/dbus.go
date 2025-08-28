package dbus

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/feral-file/godbus"

	"github.com/feral-file/ffos-user/components/feral-connectd/relayer"
	"github.com/feral-file/ffos-user/components/feral-connectd/state"

	"github.com/godbus/dbus/v5"
	"go.uber.org/zap"
)

const (
	INTERFACE godbus.Interface = "com.feralfile.connectd.general"
	PATH      godbus.Path      = "/com/feralfile/connectd"
	NAME      string           = "com.feralfile.connectd"

	MONITORD_INTERFACE                      godbus.Interface = "com.feralfile.sysmonitord"
	MONITORD_PATH                           godbus.Path      = "/com/feralfile/sysmonitord"
	MONITORD_NAME                           string           = "com.feralfile.sysmonitord"
	MONITORD_METHOD_GET_CONNECTIVITY_STATUS godbus.Member    = "GetConnectivityStatus"
	MONITORD_EVENT_SYSMETRICS               godbus.Member    = "sysmetrics"
	MONITORD_EVENT_CONNECTIVITY_CHANGE      godbus.Member    = "connectivity_change"

	SETUPD_EVENT_SHOW_PAIRING_QR_CODE godbus.Member = "show_pairing_qr_code"
)

//go:generate mockgen -source=dbus.go -destination=../mocks/dbus.go -package=mocks -mock_names=DBus=MockDBus
type DBus interface {
	Start() error
	Stop() error
	Export(obj interface{}, path godbus.Path, iface godbus.Interface) error
	Call(ctx context.Context, name string, path godbus.Path, iface godbus.Interface, method godbus.Member, args ...any) ([]any, error)
	RetryableSend(ctx context.Context, payload godbus.DBusPayload) error
	OnBusSignal(handler godbus.BusSignalHandler)
	RemoveBusSignal(handler godbus.BusSignalHandler)
}

//go:generate mockgen -source=dbus.go -destination=../mocks/dbus.go -package=mocks -mock_names=DBusHandler=MockDBusHandler
type DBusHandler interface {
	GetRelayerTopicID() (string, *dbus.Error)
}

type handler struct {
	ctx     context.Context
	relayer relayer.Relayer
	logger  *zap.Logger
}

func NewHandler(
	ctx context.Context,
	relayer relayer.Relayer,
	logger *zap.Logger) DBusHandler {
	return &handler{
		ctx:     ctx,
		relayer: relayer,
		logger:  logger,
	}
}

func (c *handler) GetRelayerTopicID() (string, *dbus.Error) {
	c.logger.Info("DBus RPC called: GetRelayerTopicID")

	topicID := state.GetState().Relayer.TopicID
	if topicID != "" {
		return topicID, nil
	}

	// Context for timeout
	deadlineCtx, deadlineCancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer deadlineCancel()

	// Create a context that will be canceled when either the deadline is reached or the global context is canceled
	retryCtx, retryCancel := context.WithCancel(c.ctx)
	_ = retryCancel // Explicitly ignore retryCancel for successful case

	// Channel to signal when the topicID is received
	doneChan := make(chan struct{})
	errChan := make(chan error)

	// Temporary handler to receive the topicID
	var closeOnce sync.Once
	var handler relayer.Handler
	handler = func(ctx context.Context, payload relayer.Payload) error {
		var err error
		defer func() {
			if err != nil {
				errChan <- err
			}
		}()

		if payload.MessageID == relayer.MESSAGE_ID_SYSTEM {
			topicID := payload.Message.TopicID
			if topicID == nil {
				err = fmt.Errorf("payload doesn't contain topicID")
				return err
			}

			// Save s
			s := state.GetState()
			s.Relayer.TopicID = *topicID
			err = s.Save()
			if err != nil {
				return err
			}

			// Remove handler and close doneChan
			closeOnce.Do(func() {
				close(doneChan)
			})
			c.relayer.RemoveRelayerMessage(handler)
		}
		return nil
	}

	// Add handler to relayer
	c.relayer.OnRelayerMessage(handler)
	defer c.relayer.RemoveRelayerMessage(handler)

	// Connect to the relayer
	err := c.relayer.RetryableConnect(retryCtx)
	if err != nil {
		retryCancel()
		return "", dbus.NewError(err.Error(), []interface{}{})
	}

	// Wait for the topicID to be received or an error to occur
	for {
		select {
		case <-doneChan:
			return state.GetState().Relayer.TopicID, nil
		case err := <-errChan:
			retryCancel()
			return "", dbus.NewError(err.Error(), []interface{}{})
		case <-deadlineCtx.Done():
			retryCancel()
			return "", dbus.NewError(deadlineCtx.Err().Error(), []interface{}{})
		}
	}
}
