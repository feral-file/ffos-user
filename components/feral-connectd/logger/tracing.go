package logger

import (
	"context"
	"fmt"

	"github.com/Feral-File/feralfile-device/components/feral-connectd/relayer"
	"github.com/getsentry/sentry-go"
	"go.uber.org/zap"
)

// RelayerMessageTracer handles Sentry transaction tracing for relayer messages
type RelayerMessageTracer struct {
	logger *zap.Logger
}

// NewRelayerMessageTracer creates a new tracer for relayer messages
func NewRelayerMessageTracer(logger *zap.Logger) *RelayerMessageTracer {
	return &RelayerMessageTracer{
		logger: logger,
	}
}

// StartTransaction creates a new Sentry transaction for a relayer message
// Following the Sentry documentation pattern for custom instrumentation
func (t *RelayerMessageTracer) StartTransaction(ctx context.Context, payload relayer.Payload) (*sentry.Span, context.Context) {
	// Skip if Sentry is not available
	hub := sentry.GetHubFromContext(ctx)
	if hub == nil {
		hub = sentry.CurrentHub().Clone()
		ctx = sentry.SetHubOnContext(ctx, hub)
	}

	// Use message ID as transaction name for better tracking
	transactionName := fmt.Sprintf("relayer.message.%s", payload.MessageID)
	if payload.MessageID == relayer.MESSAGE_ID_SYSTEM {
		transactionName = "relayer.message.system"
	}

	// Create transaction with appropriate context
	options := []sentry.SpanOption{
		sentry.WithOpName("relayer.rpc"),
		sentry.WithTransactionSource(sentry.SourceComponent),
	}

	transaction := sentry.StartTransaction(ctx, transactionName, options...)

	// Set transaction data
	transaction.SetData("message_id", payload.MessageID)
	if payload.Message.Command != nil {
		transaction.SetData("command", string(*payload.Message.Command))
	}
	if payload.Message.TopicID != nil {
		transaction.SetData("topic_id", *payload.Message.TopicID)
	}
	transaction.SetData("has_args", len(payload.Message.Args) > 0)
	transaction.SetData("arg_count", len(payload.Message.Args))

	t.logger.Debug("Started Sentry transaction for relayer message",
		zap.String("transaction_name", transactionName),
		zap.String("message_id", payload.MessageID))

	return transaction, transaction.Context()
}

// StartSpan creates a child span for a specific operation within the transaction
func (t *RelayerMessageTracer) StartSpan(ctx context.Context, operation string) *sentry.Span {
	span := sentry.StartSpan(ctx, operation)
	span.Description = operation

	t.logger.Debug("Started Sentry span",
		zap.String("operation", operation))

	return span
}

// StartParsingSpan creates a span for message parsing
func (t *RelayerMessageTracer) StartParsingSpan(ctx context.Context) *sentry.Span {
	span := t.StartSpan(ctx, "relayer.parse")
	span.SetData("stage", "parsing")
	return span
}

// StartCommandExecutionSpan creates a span for command execution
func (t *RelayerMessageTracer) StartCommandExecutionSpan(ctx context.Context, command relayer.RelayerCmd) *sentry.Span {
	span := t.StartSpan(ctx, "relayer.execute")
	span.Description = fmt.Sprintf("execute_%s", string(command))
	span.SetData("stage", "execution")
	span.SetData("command", string(command))
	return span
}

// StartCDPRequestSpan creates a span for CDP requests
func (t *RelayerMessageTracer) StartCDPRequestSpan(ctx context.Context) *sentry.Span {
	span := t.StartSpan(ctx, "relayer.cdp")
	span.Description = "forward_to_cdp"
	span.SetData("stage", "cdp_forwarding")
	return span
}

// StartResponseSpan creates a span for sending response back to relayer
func (t *RelayerMessageTracer) StartResponseSpan(ctx context.Context) *sentry.Span {
	span := t.StartSpan(ctx, "relayer.response")
	span.Description = "send_response"
	span.SetData("stage", "response")
	return span
}

// FinishSpanWithError finishes a span and records any error
func (t *RelayerMessageTracer) FinishSpanWithError(span *sentry.Span, err error) {
	if err != nil {
		span.Status = sentry.SpanStatusInternalError
		span.SetData("error", err.Error())
		t.logger.Debug("Finished Sentry span with error",
			zap.String("operation", span.Op),
			zap.Error(err))
	} else {
		span.Status = sentry.SpanStatusOK
		t.logger.Debug("Finished Sentry span successfully",
			zap.String("operation", span.Op))
	}
	span.Finish()
}

// FinishTransactionWithError finishes a transaction and records any error
func (t *RelayerMessageTracer) FinishTransactionWithError(transaction *sentry.Span, err error) {
	if err != nil {
		transaction.Status = sentry.SpanStatusInternalError
		transaction.SetData("final_error", err.Error())
		t.logger.Debug("Finished Sentry transaction with error",
			zap.String("transaction_name", transaction.Description),
			zap.Error(err))
	} else {
		transaction.Status = sentry.SpanStatusOK
		t.logger.Debug("Finished Sentry transaction successfully",
			zap.String("transaction_name", transaction.Description))
	}
	transaction.Finish()
}

// SetTopicIDGlobally updates the global Sentry scope with a new topic ID
// This is called when we receive a system message with topic ID
func (t *RelayerMessageTracer) SetTopicIDGlobally(topicID string) {
	SetGlobalTopicID(topicID)
	t.logger.Info("Updated global Sentry topic ID",
		zap.String("topic_id", topicID))
}
