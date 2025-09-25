package status

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/stretchr/testify/assert"
)

// restore helpers after each test
func withHelperOverrides(t *testing.T, overrides func()) {
	t.Helper()
	origBuild := buildCheckStatusPayloadFn
	origSend := sendCDPRequestFn
	origExtract := extractMessageFn
	t.Cleanup(func() {
		buildCheckStatusPayloadFn = origBuild
		sendCDPRequestFn = origSend
		extractMessageFn = origExtract
	})
	overrides()
}

func TestFetchPlayerStatus_buildCheckStatusPayload_failure(t *testing.T) {
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()
	var dummyCDP cdp.CDP

	withHelperOverrides(t, func() {
		buildCheckStatusPayloadFn = func() (string, error) { return "", errors.New("marshal failed") }
	})

	result, err := FetchPlayerStatus(ctx, dummyCDP, logger)

	assert.Contains(t, err.Error(), "marshal failed")
	assert.Nil(t, result)
}

func TestFetchPlayerStatus_sendCDPRequest_failure(t *testing.T) {
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()
	var dummyCDP cdp.CDP

	withHelperOverrides(t, func() {
		buildCheckStatusPayloadFn = func() (string, error) { return "{}", nil }
		sendCDPRequestFn = func(_ cdp.CDP, _ string) (map[string]interface{}, error) {
			return nil, errors.New("cdp evaluate failed")
		}
	})

	result, err := FetchPlayerStatus(ctx, dummyCDP, logger)

	assert.Contains(t, err.Error(), "cdp evaluate failed")
	assert.Nil(t, result)
}

func TestFetchPlayerStatus_extractMessage_failure(t *testing.T) {
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()
	var dummyCDP cdp.CDP

	withHelperOverrides(t, func() {
		buildCheckStatusPayloadFn = func() (string, error) { return "{}", nil }
		sendCDPRequestFn = func(_ cdp.CDP, _ string) (map[string]interface{}, error) { return map[string]interface{}{}, nil }
		extractMessageFn = func(_ map[string]interface{}) (map[string]interface{}, error) {
			return nil, errors.New("invalid message")
		}
	})

	result, err := FetchPlayerStatus(ctx, dummyCDP, logger)

	assert.Contains(t, err.Error(), "invalid message")
	assert.Nil(t, result)
}
