package offlinecache

import (
	"crypto/tls"
	"crypto/x509"
	"io"
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
// command-storm configuration. Every probe holds its completed headers while
// the body stalls out the request timeout. HTTP/2 can retain BOTH the initial
// header list and a separately-limited trailer list, and Go adds per-field
// accounting headroom to each transport limit, so the resident worst case is
// slots x maxProbeHeaderBytesPerSlot — reachable by an unauthenticated LAN
// caller against an OOM-sensitive device.
func TestTransport_BoundsAggregateProbeHeaderMemory(t *testing.T) {
	limit := sourceGuard{}.transport().MaxResponseHeaderBytes
	worstCase := int64(maxConcurrentHeaderProbes) * maxProbeHeaderBytesPerSlot

	if limit <= 0 {
		t.Fatalf("MaxResponseHeaderBytes must be set; Go's 10 MiB default is far too high here")
	}
	if limit+http2HeaderListAccountingBytes != maxResponseHeaderListBytes {
		t.Fatalf("the configured limit plus Go's HTTP/2 allowance is %d, want the effective per-list ceiling %d",
			limit+http2HeaderListAccountingBytes, maxResponseHeaderListBytes)
	}
	if worstCase > maxAggregateProbeHeaderBytes {
		t.Fatalf("the shared probe budget can hold %d bytes of attacker-chosen HTTP/2 headers (%d slots x %d); keep it under %d",
			worstCase, maxConcurrentHeaderProbes, maxProbeHeaderBytesPerSlot, maxAggregateProbeHeaderBytes)
	}
}

// TestTransport_HTTP2InitialHeadersAndTrailersShareOneBudgetSlot exercises the
// response shape the arithmetic above must cover. Go retains the initial
// header map while reading a separately-limited trailer map at EOF, so one
// admitted probe can hold two near-limit attacker-chosen blocks at once.
func TestTransport_HTTP2InitialHeadersAndTrailersShareOneBudgetSlot(t *testing.T) {
	const nearLimitBlockBytes = int(maxResponseHeaderListBytes - (4 << 10))
	initial := strings.Repeat("i", nearLimitBlockBytes)
	trailer := strings.Repeat("t", nearLimitBlockBytes)

	origin := httptest.NewUnstartedServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		w.Header().Set("X-Probe-Initial", initial)
		w.Header().Add("Trailer", "X-Probe-Trailer")
		w.WriteHeader(go_http.StatusOK)
		_, _ = w.Write([]byte("x"))
		w.Header().Set("X-Probe-Trailer", trailer)
	}))
	origin.EnableHTTP2 = true
	origin.StartTLS()
	defer origin.Close()

	roots := x509.NewCertPool()
	roots.AddCert(origin.Certificate())
	guard := sourceGuard{isReserved: loopbackIsPublic}
	transport := guard.transport()
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
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)

	assert.Equal(t, 2, resp.ProtoMajor, "the regression requires HTTP/2's separate trailer limit")
	assert.Len(t, resp.Header.Get("X-Probe-Initial"), nearLimitBlockBytes)
	assert.Len(t, resp.Trailer.Get("X-Probe-Trailer"), nearLimitBlockBytes)
}
