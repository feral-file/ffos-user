package mediahealth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedFetcher answers each request from a queue, recording what was
// asked, so the 206/416/short-prefix retry rules can be pinned without a
// network.
type scriptedFetcher struct {
	answers []scriptedAnswer
	calls   []FetchRequest
}

type scriptedAnswer struct {
	resp *FetchResponse
	err  error
}

func (f *scriptedFetcher) Fetch(_ context.Context, req FetchRequest) (*FetchResponse, error) {
	f.calls = append(f.calls, req)
	if len(f.answers) == 0 {
		return nil, &FetchError{Kind: FetchErrTransport, Err: errors.New("no scripted answer")}
	}
	a := f.answers[0]
	f.answers = f.answers[1:]
	return a.resp, a.err
}

func ok(status int, ct string, body []byte, length int64, hdr ...string) scriptedAnswer {
	h := http.Header{}
	if ct != "" {
		h.Set("Content-Type", ct)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		h.Set(hdr[i], hdr[i+1])
	}
	return scriptedAnswer{resp: &FetchResponse{Status: status, Header: h, Body: body, ContentLength: length}}
}

func fail(kind FetchErrorKind) scriptedAnswer {
	return scriptedAnswer{err: &FetchError{Kind: kind, Err: fmt.Errorf("scripted %s", kind)}}
}

func newProbe(f Fetcher) *Probe {
	return &Probe{Fetcher: f, Validator: NewContentValidator(DefaultProbeMaxBytes, []string{"gateway time-out"})}
}

func TestProbeOne_StatusTable(t *testing.T) {
	cases := []struct {
		status int
		want   Verdict
	}{
		{404, VerdictDead}, {410, VerdictDead}, {400, VerdictDead},
		{401, VerdictInconclusive}, {403, VerdictInconclusive}, {406, VerdictInconclusive},
		{407, VerdictInconclusive}, {408, VerdictInconclusive}, {429, VerdictInconclusive},
		{500, VerdictInconclusive}, {502, VerdictInconclusive}, {503, VerdictInconclusive},
		{301, VerdictInconclusive},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			f := &scriptedFetcher{answers: []scriptedAnswer{ok(tc.status, "", nil, -1)}}
			res := newProbe(f).ProbeOne(context.Background(), "https://origin.example/a")
			assert.Equal(t, tc.want, res.Verdict)
			assert.Equal(t, tc.status, res.Status)
			assert.Equal(t, ReasonHTTPStatus, res.Reason)
			if tc.want == VerdictDead {
				assert.Equal(t, CategorySourceMissing, CategoryOf(res.Reason, res.Status))
			}
		})
	}
}

func TestProbeOne_RangedRequestAndContentRules(t *testing.T) {
	body := minimalPNG(10, 10)
	f := &scriptedFetcher{answers: []scriptedAnswer{
		ok(206, "image/png", body, int64(len(body)), "Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(body))),
	}}
	res := newProbe(f).ProbeOne(context.Background(), "https://origin.example/a.png")
	require.Len(t, f.calls, 1)
	assert.Equal(t, DefaultProbeMaxBytes, f.calls[0].RangeBytes, "one ranged GET of the probe window")
	assert.Equal(t, DefaultProbeMaxBytes, f.calls[0].MaxBodyBytes)
	assert.Equal(t, VerdictAlive, res.Verdict)
	assert.Equal(t, "image/png", res.Declared)
	assert.Equal(t, "image/png", res.Sniffed)
	assert.Equal(t, "https://origin.example/a.png", res.WireURL)
}

func TestProbeOne_ContentRuleMakesDead(t *testing.T) {
	f := &scriptedFetcher{answers: []scriptedAnswer{
		ok(200, "image/png", []byte("<!DOCTYPE html><html><body>nope</body></html>"), -1),
	}}
	res := newProbe(f).ProbeOne(context.Background(), "https://origin.example/a.png")
	assert.Equal(t, VerdictDead, res.Verdict)
	assert.Equal(t, ReasonTypeMismatch, res.Reason)
	assert.Equal(t, CategoryNotMedia, CategoryOf(res.Reason, 200))
}

func TestProbeOne_206WithoutFromZeroRangeRetriesUnranged(t *testing.T) {
	body := minimalPNG(10, 10)
	f := &scriptedFetcher{answers: []scriptedAnswer{
		ok(206, "image/png", body, int64(len(body)), "Content-Range", "bytes 100-137/1000"),
		ok(200, "image/png", body, int64(len(body))),
	}}
	res := newProbe(f).ProbeOne(context.Background(), "https://origin.example/a.png")
	require.Len(t, f.calls, 2)
	assert.Equal(t, 0, f.calls[1].RangeBytes, "retry carries no Range")
	assert.Equal(t, VerdictAlive, res.Verdict)
}

func TestProbeOne_ShortPrefixOfLargerResourceRetriesUnranged(t *testing.T) {
	// 10 bytes promised of a 1 MiB resource: not enough to sniff; a few
	// leading bytes of binary media sniff as text/plain and the mismatch
	// rule would false-broken real media.
	short := []byte("\x00\x00\x00\x18ftypmp42")
	f := &scriptedFetcher{answers: []scriptedAnswer{
		ok(206, "video/mp4", short, int64(len(short)), "Content-Range", fmt.Sprintf("bytes 0-%d/1048576", len(short)-1)),
		ok(200, "video/mp4", append(short, make([]byte, DefaultProbeMaxBytes-len(short))...), 1048576),
	}}
	res := newProbe(f).ProbeOne(context.Background(), "https://origin.example/a.mp4")
	require.Len(t, f.calls, 2)
	assert.Equal(t, VerdictAlive, res.Verdict)
}

func TestProbeOne_ShortPrefixThatIsTheWholeResourceIsJudged(t *testing.T) {
	short := []byte("tiny")
	f := &scriptedFetcher{answers: []scriptedAnswer{
		ok(206, "image/png", short, int64(len(short)), "Content-Range", "bytes 0-3/4"),
	}}
	res := newProbe(f).ProbeOne(context.Background(), "https://origin.example/a.png")
	require.Len(t, f.calls, 1, "a prefix that IS the whole resource is complete evidence")
	assert.Equal(t, VerdictDead, res.Verdict)
	assert.Equal(t, ReasonTypeMismatch, res.Reason)
}

func TestProbeOne_416RetriesUnrangedOnce(t *testing.T) {
	f := &scriptedFetcher{answers: []scriptedAnswer{
		ok(416, "", nil, -1),
		ok(416, "", nil, -1),
	}}
	res := newProbe(f).ProbeOne(context.Background(), "https://origin.example/a")
	require.Len(t, f.calls, 2)
	assert.Equal(t, VerdictInconclusive, res.Verdict, "a second 416 is taken as the answer, not looped")
}

func TestProbeOne_206ToUnrangedRetryIsDead(t *testing.T) {
	f := &scriptedFetcher{answers: []scriptedAnswer{
		ok(416, "", nil, -1),
		ok(206, "image/png", minimalPNG(1, 1), 24, "Content-Range", "bytes 0-23/24"),
	}}
	res := newProbe(f).ProbeOne(context.Background(), "https://origin.example/a")
	assert.Equal(t, VerdictDead, res.Verdict)
	assert.Equal(t, ReasonHTTPStatus, res.Reason)
}

func TestProbeOne_FetchErrorsClassify(t *testing.T) {
	cases := []struct {
		kind   FetchErrorKind
		want   Verdict
		reason Reason
	}{
		{FetchErrDNS, VerdictDead, ReasonDNS},
		{FetchErrGuard, VerdictInconclusive, ""},
		{FetchErrTooLong, VerdictInconclusive, ""},
		{FetchErrTimeout, VerdictInconclusive, ReasonTransport},
		{FetchErrTransport, VerdictInconclusive, ReasonTransport},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			f := &scriptedFetcher{answers: []scriptedAnswer{fail(tc.kind)}}
			res := newProbe(f).ProbeOne(context.Background(), "https://origin.example/a")
			assert.Equal(t, tc.want, res.Verdict)
			assert.Equal(t, tc.reason, res.Reason)
			assert.Error(t, res.Err)
		})
	}
}

func TestProbeOne_EmptyBodyIsCorruptNotTransient(t *testing.T) {
	f := &scriptedFetcher{answers: []scriptedAnswer{ok(204, "image/png", nil, 0)}}
	res := newProbe(f).ProbeOne(context.Background(), "https://origin.example/a")
	assert.Equal(t, VerdictDead, res.Verdict)
	assert.Equal(t, ReasonZeroLength, res.Reason)
	assert.Equal(t, CategoryCorruptFile, CategoryOf(res.Reason, 204))
}

func TestProbeOne_MidBodyReadErrorWithoutTruncationEvidenceIsTransient(t *testing.T) {
	body := minimalPNG(10, 10)
	resp := ok(200, "image/png", body, -1)
	resp.resp.ReadErr = errors.New("connection reset")
	f := &scriptedFetcher{answers: []scriptedAnswer{resp}}
	res := newProbe(f).ProbeOne(context.Background(), "https://origin.example/a")
	assert.Equal(t, VerdictInconclusive, res.Verdict)
}

func TestProbeOne_DataURIs(t *testing.T) {
	p := newProbe(&scriptedFetcher{})
	res := p.ProbeOne(context.Background(), "data:image/png;base64,iVBORw0KGgo=")
	assert.Equal(t, VerdictInline, res.Verdict)

	res = p.ProbeOne(context.Background(), "data:image/png;base64")
	assert.Equal(t, VerdictDead, res.Verdict, "whole URI inside the scan bound and no comma: proven malformed")
	assert.Equal(t, ReasonDataURIInvalid, res.Reason)

	long := "data:image/png;" + strings.Repeat("p", dataURIMetadataBytes+10)
	res = p.ProbeOne(context.Background(), long)
	assert.Equal(t, VerdictInconclusive, res.Verdict, "a comma merely beyond the scan bound proves nothing")
}

// fakeRewriter is a Rewriter double: one host is hostile, one CID path
// redirects to a replacement gateway.
type fakeRewriter struct{}

func (fakeRewriter) RewriteURL(raw string) (string, bool) {
	if strings.HasPrefix(raw, "https://ipfs.io/ipfs/") {
		return "https://ipfs.filebase.io" + strings.TrimPrefix(raw, "https://ipfs.io"), true
	}
	return "", false
}
func (fakeRewriter) Matches(raw string) bool { return strings.HasPrefix(raw, "https://ipfs.io/") }
func (fakeRewriter) UserAgent() string       { return "feral-player/2.0" }

func TestProbeOne_ProbesTheRewrittenWire(t *testing.T) {
	f := &scriptedFetcher{answers: []scriptedAnswer{ok(200, "image/png", minimalPNG(1, 1), -1), ok(200, "text/html", []byte("<html><body>x</body></html>"), -1)}}
	p := newProbe(f)
	p.Rewriter = fakeRewriter{}

	res := p.ProbeOne(context.Background(), "https://ipfs.io/ipfs/bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi/a.png")
	require.Len(t, f.calls, 1)
	assert.Equal(t, "https://ipfs.filebase.io/ipfs/bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi/a.png", f.calls[0].URL)
	assert.Empty(t, f.calls[0].UserAgent, "a redirected request keeps the default agent, as the kiosk interceptor does")
	assert.Equal(t, f.calls[0].URL, res.WireURL)

	p.ProbeOne(context.Background(), "https://ipfs.io/ipns/some.name/index.html")
	require.Len(t, f.calls, 2)
	assert.Equal(t, "https://ipfs.io/ipns/some.name/index.html", f.calls[1].URL)
	assert.Equal(t, "feral-player/2.0", f.calls[1].UserAgent, "a non-CID path on a hostile host gets the policy agent")
}

func TestRedactForLog(t *testing.T) {
	assert.Equal(t, "https://cdn.example/a.png?…", redactForLog("https://cdn.example/a.png?X-Amz-Signature=secret"))
	assert.Equal(t, "https://host/a", redactForLog("https://user:pw@host/a"))
	assert.Equal(t, "https://host/a?…", redactForLog("https://host/a#frag"))
}

func TestParseContentRange(t *testing.T) {
	s, e, tot, ok := parseContentRange("bytes 0-99/1000")
	assert.True(t, ok)
	assert.Equal(t, [3]int64{0, 99, 1000}, [3]int64{s, e, tot})
	_, _, tot, ok = parseContentRange("bytes 0-99/*")
	assert.True(t, ok)
	assert.Equal(t, int64(-1), tot)
	_, _, _, ok = parseContentRange("bytes 0-not-a-range/500000")
	assert.False(t, ok)
	_, _, _, ok = parseContentRange("bytes 5-4/10")
	assert.False(t, ok)
	_, _, _, ok = parseContentRange("bytes 0-10/10")
	assert.False(t, ok, "total must exceed end")
}

// F14 (PR #438 review, round 4): the probe hands its wire policy to the
// fetcher for every redirect hop; without a policy there is no hook.
func TestProbeOne_RedirectHookIsTheWirePolicy(t *testing.T) {
	f := &scriptedFetcher{answers: []scriptedAnswer{ok(200, "image/png", minimalPNG(1, 1), -1)}}
	p := newProbe(f)
	p.Rewriter = fakeRewriter{}
	p.ProbeOne(context.Background(), "https://cdn.example/art")
	require.Len(t, f.calls, 1)
	require.NotNil(t, f.calls[0].Redirect)
	wire, agent := f.calls[0].Redirect("https://ipfs.io/ipfs/bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi/a.png")
	assert.Equal(t, "https://ipfs.filebase.io/ipfs/bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi/a.png", wire)
	assert.Empty(t, agent)
	wire, agent = f.calls[0].Redirect("https://ipfs.io/ipns/name/x")
	assert.Equal(t, "https://ipfs.io/ipns/name/x", wire)
	assert.Equal(t, "feral-player/2.0", agent)
	wire, agent = f.calls[0].Redirect("https://benign.example/x")
	assert.Equal(t, "https://benign.example/x", wire)
	assert.Empty(t, agent)

	f = &scriptedFetcher{answers: []scriptedAnswer{ok(200, "image/png", minimalPNG(1, 1), -1)}}
	newProbe(f).ProbeOne(context.Background(), "https://cdn.example/art")
	require.Len(t, f.calls, 1)
	assert.Nil(t, f.calls[0].Redirect, "no policy, no hook")
}
