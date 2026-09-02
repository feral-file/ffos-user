package offlinecache

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	go_http "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// TestTransport_BoundsAggregateProbeHeaderMemory pins the response-header
// ceiling against the prober's process-wide slot budget, not a particular
// command-storm configuration. Every probe can hold its completed initial
// headers while the body stalls. It closes without reading EOF, so trailers
// are excluded; the per-slot budget also leaves a full list-sized allowance
// for Go's materialized map and transport bookkeeping.
func TestTransport_BoundsAggregateProbeHeaderMemory(t *testing.T) {
	transport := sourceGuard{}.oneShotProbeTransport()
	limit := transport.MaxResponseHeaderBytes
	worstCaseHeaders := int64(maxConcurrentHeaderProbes) * maxProbeHeaderBytesPerSlot
	worstCaseBudget := int64(maxConcurrentHeaderProbes) * maxProbeHeaderBudgetBytesPerSlot

	if limit <= 0 {
		t.Fatalf("MaxResponseHeaderBytes must be set; Go's 10 MiB default is far too high here")
	}
	if limit+http2HeaderListAccountingBytes != maxResponseHeaderListBytes {
		t.Fatalf("the configured limit plus the conservative rounding allowance is %d, want the per-slot header ceiling %d",
			limit+http2HeaderListAccountingBytes, maxResponseHeaderListBytes)
	}
	if transport.Protocols == nil || !transport.Protocols.HTTP1() || transport.Protocols.HTTP2() {
		t.Fatalf("one-shot probes must permit only HTTP/1; got protocols %v", transport.Protocols)
	}
	if worstCaseHeaders > maxAggregateProbeHeaderBytes {
		t.Fatalf("the shared probe budget can hold %d bytes of attacker-chosen response headers (%d slots x %d); keep it under %d",
			worstCaseHeaders, maxConcurrentHeaderProbes, maxProbeHeaderBytesPerSlot, maxAggregateProbeHeaderBytes)
	}
	if worstCaseBudget > maxAggregateProbeHeaderBytes {
		t.Fatalf("the shared probe budget accounts for %d bytes including implementation headroom (%d slots x %d); keep it under %d",
			worstCaseBudget, maxConcurrentHeaderProbes, maxProbeHeaderBudgetBytesPerSlot, maxAggregateProbeHeaderBytes)
	}
}

// TestProbeTransport_RejectsHTTP2AndDoesNotParseImmediateTrailers proves the
// one-shot client negotiates HTTP/1 even when the origin offers HTTP/2. The
// origin returns a trailer immediately; closing without reading EOF must leave
// it unparsed while the bounded initial header remains available.
func TestProbeTransport_RejectsHTTP2AndDoesNotParseImmediateTrailers(t *testing.T) {
	const nearLimitBlockBytes = int(maxResponseHeaderListBytes - (4 << 10))
	initial := strings.Repeat("i", nearLimitBlockBytes)

	origin := httptest.NewUnstartedServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		w.Header().Set("X-Probe-Initial", initial)
		w.Header().Set("Trailer", "X-Probe-Trailer")
		w.WriteHeader(go_http.StatusOK)
		_, _ = w.Write([]byte("x"))
		w.Header().Set("X-Probe-Trailer", strings.Repeat("t", nearLimitBlockBytes))
	}))
	origin.EnableHTTP2 = true
	origin.StartTLS()
	defer origin.Close()

	roots := x509.NewCertPool()
	roots.AddCert(origin.Certificate())
	guard := sourceGuard{isReserved: loopbackIsPublic}
	transport := guard.oneShotProbeTransport()
	transport.TLSClientConfig = &tls.Config{ //nolint:gosec // Test trusts only httptest's ephemeral certificate.
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
	}
	client := wrapper.NewHTTPClientFrom(&go_http.Client{
		Timeout:   2 * time.Second,
		Transport: transport,
	})

	req, err := client.NewRequest(go_http.MethodGet, origin.URL, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, 1, resp.ProtoMajor, "one-shot probes must reject offered HTTP/2")
	assert.Len(t, resp.Header.Get("X-Probe-Initial"), nearLimitBlockBytes)
	assert.Empty(t, resp.Trailer.Get("X-Probe-Trailer"), "closing before EOF must not parse trailers")
}

// TestSourceProbe_HTTP1ChunkedTrailerIsNeverParsed proves the source probe
// returns on the status line and initial headers instead of reading to EOF.
// The server withholds a trailer larger than the initial-header ceiling; the
// result must arrive while that trailer is still impossible to parse.
func TestSourceProbe_HTTP1ChunkedTrailerIsNeverParsed(t *testing.T) {
	protocol := make(chan int, 1)
	headersSent := make(chan struct{}, 1)
	releaseTrailer := make(chan struct{})

	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		protocol <- r.ProtoMajor
		w.Header().Set("Trailer", "X-Probe-Trailer")
		w.WriteHeader(go_http.StatusOK)
		_, _ = w.Write([]byte("x"))
		flusher, ok := w.(go_http.Flusher)
		if !ok {
			return
		}
		flusher.Flush()
		headersSent <- struct{}{}

		<-releaseTrailer
		w.Header().Set("X-Probe-Trailer", strings.Repeat("t", int(maxResponseHeaderListBytes*2)))
	}))
	defer origin.Close()
	defer close(releaseTrailer)

	guard := sourceGuard{isReserved: loopbackIsPublic}
	prober := newSourceProberWith(guard, newGuardedNoRedirectHTTPClientFor(guard, 5*time.Second))
	resultC := make(chan SourceProbeResult, 1)
	go func() {
		resultC <- prober.ProbeSources(context.Background(), []string{origin.URL})[0]
	}()

	select {
	case proto := <-protocol:
		assert.Equal(t, 1, proto, "the regression requires HTTP/1 chunked trailers")
	case <-time.After(2 * time.Second):
		t.Fatal("source probe did not reach the HTTP/1 origin")
	}
	select {
	case <-headersSent:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP/1 origin did not flush its initial headers and short body")
	}
	select {
	case result := <-resultC:
		assert.Equal(t, ProbeAlive, result.Verdict)
		assert.Equal(t, go_http.StatusOK, result.Status)
		assert.NoError(t, result.Err)
	case <-time.After(2 * time.Second):
		t.Fatal("source probe waited for EOF, exposing the HTTP/1 trailer parser")
	}
}
