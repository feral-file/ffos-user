package logger_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"

	"github.com/getsentry/sentry-go"
	"go.uber.org/zap/zaptest"
)

func TestRelayerMessageTracer_StartTransaction(t *testing.T) {
	l := zaptest.NewLogger(t)
	tracer := logger.NewRelayerMessageTracer(l)

	ctx := context.Background()

	// Test system message
	systemPayload := relayer.Payload{
		MessageID: relayer.MESSAGE_ID_SYSTEM,
		Message: struct {
			Command *relayer.RelayerCmd    `json:"command,omitempty"`
			Args    map[string]interface{} `json:"request,omitempty"`
			TopicID *string                `json:"topicID,omitempty"`
		}{
			TopicID: stringPtr("test-topic-123"),
		},
	}

	transaction, _ := tracer.StartTransaction(ctx, systemPayload)

	// Verify transaction data
	if transaction.Data["message_id"] != relayer.MESSAGE_ID_SYSTEM {
		t.Errorf("Expected message_id to be %s, got %v", relayer.MESSAGE_ID_SYSTEM, transaction.Data["message_id"])
	}

	if transaction.Data["topic_id"] != "test-topic-123" {
		t.Errorf("Expected topic_id to be test-topic-123, got %v", transaction.Data["topic_id"])
	}

	transaction.Finish()
}

func TestRelayerMessageTracer_StartTransaction_WithCommand(t *testing.T) {
	l := zaptest.NewLogger(t)
	tracer := logger.NewRelayerMessageTracer(l)

	ctx := context.Background()

	// Test command message
	connectCmd := relayer.CMD_CONNECT
	commandPayload := relayer.Payload{
		MessageID: "msg-456",
		Message: struct {
			Command *relayer.RelayerCmd    `json:"command,omitempty"`
			Args    map[string]interface{} `json:"request,omitempty"`
			TopicID *string                `json:"topicID,omitempty"`
		}{
			Command: &connectCmd,
			Args: map[string]interface{}{
				"clientDevice": map[string]interface{}{
					"device_id": "test-device",
				},
			},
		},
	}

	transaction, _ := tracer.StartTransaction(ctx, commandPayload)

	// Verify command data
	if transaction.Data["command"] != string(relayer.CMD_CONNECT) {
		t.Errorf("Expected command to be %s, got %v", relayer.CMD_CONNECT, transaction.Data["command"])
	}

	if transaction.Data["has_args"] != true {
		t.Error("Expected has_args to be true")
	}

	if transaction.Data["arg_count"] != 1 {
		t.Errorf("Expected arg_count to be 1, got %v", transaction.Data["arg_count"])
	}

	transaction.Finish()
}

func TestRelayerMessageTracer_StartSpans(t *testing.T) {
	l := zaptest.NewLogger(t)
	tracer := logger.NewRelayerMessageTracer(l)

	// Initialize Sentry with a dummy DSN for testing
	_ = sentry.Init(sentry.ClientOptions{
		Dsn: "https://dummy@sentry.io/123", // Using dummy DSN for testing
	})
	defer sentry.Flush(0)

	ctx := context.Background()

	// Create a transaction first
	payload := relayer.Payload{
		MessageID: "test-msg",
		Message: struct {
			Command *relayer.RelayerCmd    `json:"command,omitempty"`
			Args    map[string]interface{} `json:"request,omitempty"`
			TopicID *string                `json:"topicID,omitempty"`
		}{},
	}

	transaction, tracedCtx := tracer.StartTransaction(ctx, payload)
	defer transaction.Finish()

	// Test parsing span
	parseSpan := tracer.StartParsingSpan(tracedCtx)
	if parseSpan.Op != "relayer.parse" {
		t.Errorf("Expected operation to be relayer.parse, got %s", parseSpan.Op)
	}
	if parseSpan.Data["stage"] != "parsing" {
		t.Errorf("Expected stage to be parsing, got %v", parseSpan.Data["stage"])
	}
	parseSpan.Finish()

	// Test command execution span
	connectCmd := relayer.CMD_CONNECT
	execSpan := tracer.StartCommandExecutionSpan(tracedCtx, connectCmd)
	if execSpan.Op != "relayer.execute" {
		t.Errorf("Expected operation to be relayer.execute, got %s", execSpan.Op)
	}
	if execSpan.Data["command"] != string(connectCmd) {
		t.Errorf("Expected command to be %s, got %v", connectCmd, execSpan.Data["command"])
	}
	execSpan.Finish()

	// Test CDP request span
	cdpSpan := tracer.StartCDPRequestSpan(tracedCtx)
	if cdpSpan.Op != "relayer.cdp" {
		t.Errorf("Expected operation to be relayer.cdp, got %s", cdpSpan.Op)
	}
	if cdpSpan.Data["stage"] != "cdp_forwarding" {
		t.Errorf("Expected stage to be cdp_forwarding, got %v", cdpSpan.Data["stage"])
	}
	cdpSpan.Finish()

	// Test response span
	responseSpan := tracer.StartResponseSpan(tracedCtx)
	if responseSpan.Op != "relayer.response" {
		t.Errorf("Expected operation to be relayer.response, got %s", responseSpan.Op)
	}
	if responseSpan.Data["stage"] != "response" {
		t.Errorf("Expected stage to be response, got %v", responseSpan.Data["stage"])
	}
	responseSpan.Finish()
}

func TestRelayerMessageTracer_FinishSpanWithError(t *testing.T) {
	l := zaptest.NewLogger(t)
	tracer := logger.NewRelayerMessageTracer(l)

	// Initialize Sentry with a dummy DSN for testing
	_ = sentry.Init(sentry.ClientOptions{
		Dsn: "https://dummy@sentry.io/123",
	})
	defer sentry.Flush(0)

	ctx := context.Background()
	span := sentry.StartSpan(ctx, "test")

	// Test finishing with error
	testErr := fmt.Errorf("test error")
	tracer.FinishSpanWithError(span, testErr)

	if span.Status != sentry.SpanStatusInternalError {
		t.Errorf("Expected status to be InternalError, got %v", span.Status)
	}

	if span.Data["error"] != "test error" {
		t.Errorf("Expected error to be 'test error', got %v", span.Data["error"])
	}
}

func TestRelayerMessageTracer_FinishSpanWithoutError(t *testing.T) {
	l := zaptest.NewLogger(t)
	tracer := logger.NewRelayerMessageTracer(l)

	// Initialize Sentry with a dummy DSN for testing
	_ = sentry.Init(sentry.ClientOptions{
		Dsn: "https://dummy@sentry.io/123",
	})
	defer sentry.Flush(0)

	ctx := context.Background()
	span := sentry.StartSpan(ctx, "test")

	// Test finishing without error
	tracer.FinishSpanWithError(span, nil)

	if span.Status != sentry.SpanStatusOK {
		t.Errorf("Expected status to be OK, got %v", span.Status)
	}

	if span.Data["error"] != nil {
		t.Errorf("Expected no error data, got %v", span.Data["error"])
	}
}

// Helper function to create string pointer
func stringPtr(s string) *string {
	return &s
}
