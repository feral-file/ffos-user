package controlgate

import (
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
)

// A carrier that does not parse is logged once per interval, with a count
// and none of the sender's text.
func TestCarrierRefusalIsLoggedBoundedAndWithoutSenderText(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	g := &Gate{logger: zap.New(core)}
	for range 50 {
		if _, perr := g.HandleEnvelope(t.Context(), []byte(`{"v":1,"jwe":"x","SECRET_IN_A_MEMBER_NAME":0}`), control.Origin{}); perr == nil {
			t.Fatal("carrier with an extra member was accepted")
		}
	}
	if got := logs.Len(); got != 1 {
		t.Fatalf("lines = %d, want 1", got)
	}
	entry := logs.All()[0]
	fields := entry.ContextMap()
	if fields["code"] != "bad_envelope" || fields["count"] != int64(1) {
		t.Fatalf("fields = %v", fields)
	}
	for _, value := range fields {
		if text, ok := value.(string); ok && strings.Contains(text, "SECRET") {
			t.Fatalf("sender text reached the log: %v", fields)
		}
	}
}
