package offlinecache

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	go_http "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProbeParser_BoundsAggregateHeaderMemory pins the status-only parser's
// byte ceiling and conservative per-slot charge against the shared process
// budget. Response fields are discarded through a fixed reader rather than
// materialized into a map, so cardinality cannot multiply the slot charge.
func TestProbeParser_BoundsAggregateHeaderMemory(t *testing.T) {
	worstCaseHeaders := int64(maxConcurrentHeaderProbes) * maxProbeHeaderBytesPerSlot
	worstCaseHTTPBudget := int64(maxConcurrentHeaderProbes) * maxProbeHeaderBudgetBytesPerSlot
	maxConcurrentTLSProbes := int64(maxConcurrentHeaderProbes) / probeHTTPSHeaderBudgetSlots
	worstCaseHTTPSBudget := maxConcurrentTLSProbes * maxProbeHTTPSBudgetBytes

	if maxProbeResponseHeaderBytes <= 0 || maxProbeResponseHeaderFields <= 0 {
		t.Fatal("probe response byte and field ceilings must both be positive")
	}
	if maxProbeHeaderBudgetBytesPerSlot < maxProbeResponseHeaderBytes {
		t.Fatalf("per-slot charge %d is below accepted wire bytes %d",
			maxProbeHeaderBudgetBytesPerSlot, maxProbeResponseHeaderBytes)
	}
	if maxProbeHTTPSBudgetBytes < maxProbeTLSHandshakeBytes+maxProbeResponseHeaderBytes {
		t.Fatalf("HTTPS per-probe charge %d is below bounded peer input %d",
			maxProbeHTTPSBudgetBytes, maxProbeTLSHandshakeBytes+maxProbeResponseHeaderBytes)
	}
	if probeHeaderBudgetWeight("https://example.com/work") != probeHTTPSHeaderBudgetSlots {
		t.Fatal("HTTPS probes must spend their full TLS-aware budget weight")
	}
	if probeHeaderBudgetWeight("http://example.com/work") != 1 {
		t.Fatal("plain HTTP probes must spend one base budget unit")
	}
	if worstCaseHeaders > maxAggregateProbeHeaderBytes {
		t.Fatalf("the shared probe budget can hold %d bytes of attacker-chosen response headers (%d slots x %d); keep it under %d",
			worstCaseHeaders, maxConcurrentHeaderProbes, maxProbeHeaderBytesPerSlot, maxAggregateProbeHeaderBytes)
	}
	if worstCaseHTTPBudget > maxAggregateProbeHeaderBytes {
		t.Fatalf("the shared probe budget accounts for %d bytes including implementation headroom (%d slots x %d); keep it under %d",
			worstCaseHTTPBudget, maxConcurrentHeaderProbes, maxProbeHeaderBudgetBytesPerSlot, maxAggregateProbeHeaderBytes)
	}
	if worstCaseHTTPSBudget > maxAggregateProbeHeaderBytes {
		t.Fatalf("the shared probe budget accounts for %d bytes of TLS-aware parser state (%d probes x %d); keep it under %d",
			worstCaseHTTPSBudget, maxConcurrentTLSProbes, maxProbeHTTPSBudgetBytes, maxAggregateProbeHeaderBytes)
	}
}

// TestProbeTLSHandshake_RejectsOversizedCertificateBeforeParsing proves the
// stricter status-probe envelope sits below crypto/tls, not after certificate
// and x509 structures have already been materialized.
func TestProbeTLSHandshake_RejectsOversizedCertificateBeforeParsing(t *testing.T) {
	certificateSource := httptest.NewTLSServer(go_http.HandlerFunc(func(go_http.ResponseWriter, *go_http.Request) {}))
	certificate := certificateSource.TLS.Certificates[0]
	roots := x509.NewCertPool()
	roots.AddCert(certificateSource.Certificate())
	certificateSource.Close()

	leafDER := certificate.Certificate[0]
	certificate.Certificate = nil
	chainBytes := 0
	for chainBytes <= int(maxProbeTLSHandshakeBytes)+len(leafDER) {
		certificate.Certificate = append(certificate.Certificate, leafDER)
		chainBytes += len(leafDER)
	}

	clientRaw, serverRaw := net.Pipe()
	deadline := time.Now().Add(2 * time.Second)
	require.NoError(t, clientRaw.SetDeadline(deadline))
	require.NoError(t, serverRaw.SetDeadline(deadline))
	defer func() { _ = clientRaw.Close() }()
	defer func() { _ = serverRaw.Close() }()

	serverTLS := tls.Server(serverRaw, &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	})
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serverTLS.HandshakeContext(context.Background())
	}()

	handshakeConn := newProbeTLSHandshakeConn(clientRaw)
	clientTLS := tls.Client(handshakeConn, &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: "example.com",
	})
	err := clientTLS.HandshakeContext(context.Background())
	require.ErrorIs(t, err, errProbeTLSHandshakeTooLarge)
	assert.Empty(t, clientTLS.ConnectionState().PeerCertificates,
		"the certificate chain must be rejected before x509 materialization")

	_ = clientRaw.Close()
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("TLS test server did not stop after the bounded client closed")
	}
}

// TestProbeClient_RejectsHTTP2AndDiscardsImmediateTrailers proves the
// status-only client negotiates HTTP/1 even when the origin offers HTTP/2.
// Both initial fields and the immediate trailer are discarded rather than
// materialized into the returned response.
func TestProbeClient_RejectsHTTP2AndDiscardsImmediateTrailers(t *testing.T) {
	const nearLimitBlockBytes = int(maxProbeResponseHeaderBytes - (4 << 10))
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
	client := newGuardedNoRedirectHTTPClientForTLS(
		guard,
		2*time.Second,
		&tls.Config{ //nolint:gosec // Test trusts only httptest's ephemeral certificate.
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
		},
	)

	req, err := client.NewRequest(go_http.MethodGet, origin.URL, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, 1, resp.ProtoMajor, "status-only probes must reject offered HTTP/2")
	assert.Empty(t, resp.Header, "probe response headers must never be materialized")
	assert.Empty(t, resp.Trailer, "probe response trailers must never be parsed")
	assert.Nil(t, resp.TLS, "probe responses must not retain parsed certificate state")
}

// TestProbeClient_RejectsHeaderCardinalityBeforeResponseConstruction covers
// the expansion case a wire-byte limit alone misses: many short, distinct
// names. The parser refuses the field that crosses its ceiling and returns no
// response map at all.
func TestProbeClient_RejectsHeaderCardinalityBeforeResponseConstruction(t *testing.T) {
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		for i := 0; i < maxProbeResponseHeaderFields; i++ {
			w.Header().Set(fmt.Sprintf("X-Probe-%03d", i), "x")
		}
		w.WriteHeader(go_http.StatusOK)
	}))
	defer origin.Close()

	guard := sourceGuard{isReserved: loopbackIsPublic}
	client := newGuardedNoRedirectHTTPClientFor(guard, 2*time.Second)
	resp, err := client.Get(origin.URL)

	assert.Nil(t, resp)
	require.Error(t, err)
	assert.ErrorContains(t, err, "header fields")
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
