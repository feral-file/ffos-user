package offlinecache

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// TestStaticServer_Serve_UnexpectedListenerFailureMarksNotListening is
// the regression test for the gap the "static server bind failures are
// only logged" review round left behind: a Listen() that succeeded but
// whose underlying listener later dies out from under Serve (e.g. a
// spurious FD close, not a graceful Shutdown) must still flip
// IsListening() to false once Serve returns — otherwise replay.go's
// large-asset redirect gate (fulfillFromBlob's IsListening check) would
// keep treating this server as available forever after, reintroducing
// exactly the dead-redirect risk that gate exists to prevent. This is
// a whitebox test because forcing an "already listening, then killed"
// state requires reaching into the private listener field directly;
// black-box tests can only observe Listen/Shutdown's own paths.
func TestStaticServer_Serve_UnexpectedListenerFailureMarksNotListening(t *testing.T) {
	server := NewStaticServer("127.0.0.1:0", nil, wrapper.NewOS(), zaptest.NewLogger(t)).(*staticServer)
	require.NoError(t, server.Listen())
	assert.True(t, server.IsListening())

	// Simulate the listener dying out from under Serve without going
	// through Shutdown: closing it directly makes the next Accept()
	// return a "use of closed network connection" error, which is NOT
	// http.ErrServerClosed, so Serve treats it as a genuine unexpected
	// failure rather than a graceful stop.
	require.NoError(t, server.listener.Close())

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()

	select {
	case err := <-serveErr:
		assert.Error(t, err, "Serve must surface the unexpected listener failure, not swallow it like a graceful Shutdown's ErrServerClosed")
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after its listener was closed out from under it")
	}

	assert.False(t, server.IsListening(),
		"IsListening must go false once Serve has genuinely stopped accepting connections, regardless of why it stopped")
}
