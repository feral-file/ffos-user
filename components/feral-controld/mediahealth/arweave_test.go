package mediahealth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const txA = "EwKI3znILCW86MZiIwji_kxrFG9Rdp-2arLMpxjyojs"

func TestArweaveConfirmURL(t *testing.T) {
	cases := []struct {
		source, gateway, want string
		ok                    bool
	}{
		{"https://arweave.net/" + txA, "", "https://ar-io.dev/" + txA, true},
		{"https://arweave.net/" + txA + "/index.html?x=1", "", "https://ar-io.dev/" + txA + "/index.html?x=1", true},
		{"https://ar-io.dev/" + txA, "", "https://arweave.net/" + txA, true},
		{"https://ar-io.net/" + txA, "", "https://ar-io.dev/" + txA, true},
		{"https://arweave.net/" + txA, "https://permagate.io/ignored-path", "https://permagate.io/" + txA, true},
		{"https://arweave.net/" + txA, "http://insecure.example", "https://ar-io.dev/" + txA, true},
		{"https://arweave.net/not-a-transaction", "", "", false},
		{"https://arweave.net/", "", "", false},
		{"https://animus_rtfkt.ar.io/?tokenId=1", "", "", false},
		{"https://ipfs.filebase.io/ipfs/" + txA, "", "", false},
	}
	for _, tc := range cases {
		got, ok := arweaveConfirmURL(tc.source, tc.gateway)
		assert.Equal(t, tc.ok, ok, tc.source)
		assert.Equal(t, tc.want, got, tc.source)
	}
}

func newArweaveProber(t *testing.T, f *tableFetcher) *Prober {
	t.Helper()
	p := newPassProber(t, f, &testClock{t: time.Now()})
	p.Online = func() bool { return true }
	return p
}

// The office FF1 fixture: arweave.net 404 while ar-io.dev serves the
// transaction. Not a verdict — a gateway failure.
func TestArweave_SecondGatewayServesIt(t *testing.T) {
	f := newTableFetcher()
	src := "https://arweave.net/" + txA
	f.answers[src] = ok(404, "text/html", []byte("404 - Page cannot be found"), -1)
	f.answers["https://ar-io.dev/"+txA] = ok(206, "video/mp4", []byte("\\x00\\x00\\x00\\x18ftypmp42"), -1, "Content-Range", "bytes 0-23/1000")
	p := newArweaveProber(t, f)

	res := p.ProbeAndRecord(context.Background(), src)
	assert.Equal(t, VerdictInconclusive, res.Verdict, "a gateway's 404 that a second gateway disproves is never broken")
	assert.Empty(t, res.Unreachable, "the transaction exists: an answer, not an unreachable attempt")
	assert.True(t, res.GatewayFailed)
	assert.Equal(t, 404, res.Status)
	rec, err := p.Store.Get(src)
	require.NoError(t, err)
	assert.NotEqual(t, StateBroken, rec.State)
	assert.Equal(t, IssueServerError, rec.Issue, "honest: the gateway failed to serve")
	assert.Zero(t, rec.UnreachableFailures, "and it does not feed the unreachable streak")
	hints, err := p.Store.Hints([]string{src}, time.Now())
	require.NoError(t, err)
	assert.Empty(t, hints, "nothing is hinted away")
}

func TestArweave_BothGatewaysSayMissing(t *testing.T) {
	f := newTableFetcher()
	src := "https://arweave.net/" + txA
	f.answers[src] = ok(404, "text/html", nil, -1)
	f.answers["https://ar-io.dev/"+txA] = ok(404, "text/html", nil, -1)
	p := newArweaveProber(t, f)
	res := p.ProbeAndRecord(context.Background(), src)
	assert.Equal(t, VerdictDead, res.Verdict, "confirmed by an independent gateway")
	assert.Contains(t, res.Detail, "ar-io.dev also answered HTTP 404")
	rec, err := p.Store.Get(src)
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State)
}

func TestArweave_UnconfirmedIsTransient(t *testing.T) {
	f := newTableFetcher()
	src := "https://arweave.net/" + txA
	f.answers[src] = ok(404, "text/html", nil, -1)
	f.answers["https://ar-io.dev/"+txA] = ok(502, "", nil, -1)
	p := newArweaveProber(t, f)
	res := p.ProbeAndRecord(context.Background(), src)
	assert.Equal(t, VerdictInconclusive, res.Verdict, "a 404 nobody confirmed is not a verdict")
	assert.Equal(t, UnreachableServer, res.Unreachable)
}

// At cast, a second gateway that runs out of the 5 s deadline leaves the
// item unrecorded, like any cast timeout: the lookahead's 30 s probe is
// the real attempt.
func TestArweave_CastDeadlineDuringConfirmationRecordsNothing(t *testing.T) {
	f := newTableFetcher()
	src := "https://arweave.net/" + txA
	f.answers[src] = ok(404, "text/html", nil, -1)
	f.answers["https://ar-io.dev/"+txA] = fail(FetchErrTimeout)
	p := newArweaveProber(t, f)
	res := p.Preflight(context.Background(), []string{src})
	assert.Equal(t, VerdictInconclusive, res[0].Verdict, "never dead at cast on an unconfirmed 404")
	rec, err := p.Store.Get(src)
	require.NoError(t, err)
	assert.Nil(t, rec)
}

func TestArweave_OtherHostsKeepTheirSingleRequest(t *testing.T) {
	f := newTableFetcher()
	src := "https://raw.example/missing.png"
	f.answers[src] = ok(404, "", nil, -1)
	p := newArweaveProber(t, f)
	res := p.ProbeAndRecord(context.Background(), src)
	assert.Equal(t, VerdictDead, res.Verdict)
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Len(t, f.calls, 1, "no confirmation request outside arweave transaction paths")
}

// F24 (PR #438 review): the first gateway answering 404 for days while the
// second serves the transaction must never conclude broken, and it ends a
// streak that earlier no-answer attempts had started.
func TestArweave_ServedTransactionNeverConcludesBroken(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	src := "https://arweave.net/" + txA
	f.answers[src] = ok(404, "text/html", nil, -1)
	f.answers["https://ar-io.dev/"+txA] = ok(206, "video/mp4", []byte("\x00\x00\x00\x18ftypmp42"), -1, "Content-Range", "bytes 0-23/1000")
	p := newPassProber(t, f, clock)
	p.Online = func() bool { return true }

	_, err := p.Store.ApplyProbe(src, ProbeResult{Verdict: VerdictInconclusive, Reason: ReasonTransport, Err: context.DeadlineExceeded, Unreachable: UnreachableNoAnswer, NetworkHealthy: true})
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		clock.Advance(13 * time.Hour)
		p.ProbeAndRecord(context.Background(), src)
	}
	rec, err := p.Store.Get(src)
	require.NoError(t, err)
	assert.NotEqual(t, StateBroken, rec.State, "three served confirmations over 26 h never conclude")
	assert.Nil(t, rec.UnreachableSince, "each served confirmation ends the streak")
	assert.Equal(t, IssueServerError, rec.Issue)
	hints, err := p.Store.Hints([]string{src}, clock.t)
	require.NoError(t, err)
	assert.Empty(t, hints)
}
