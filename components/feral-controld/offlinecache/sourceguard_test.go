package offlinecache_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
)

// fixedResolver answers every name with one caller-chosen address, so a
// hostname can be pointed at a reserved range without touching real DNS.
type fixedResolver struct {
	ip  string
	err error
}

func (r fixedResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	if r.err != nil {
		return nil, r.err
	}
	return []net.IPAddr{{IP: net.ParseIP(r.ip)}}, nil
}

// multiResolver answers with several addresses at once — the shape that
// matters for the "every answer must be safe" rule.
type multiResolver struct{ ips []string }

func (r multiResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	addrs := make([]net.IPAddr, 0, len(r.ips))
	for _, ip := range r.ips {
		addrs = append(addrs, net.IPAddr{IP: net.ParseIP(ip)})
	}
	return addrs, nil
}

// TestClassify_RejectsUnsafeSchemes pins that anything outside http(s)
// is refused BEFORE any network call. The HTTP mock carries zero
// expectations, so gomock's strict controller fails the test outright if
// a rejected scheme ever reaches a probe.
//
// file:// is the one that matters most: capture.go hands item.Source
// straight to Page.navigate in the headless browser, so a file:// source
// that survived classification would be local file disclosure.
func TestClassify_RejectsUnsafeSchemes(t *testing.T) {
	unsafe := []string{
		"file:///etc/shadow",
		"file://localhost/home/feralfile/.ssh/id_ed25519",
		"ftp://example.com/art.png",
		"gopher://example.com/",
		"ws://example.com/socket",
		"chrome://settings",
		"devtools://devtools/bundled/inspector.html",
		"about:blank",
		"blob:https://example.com/1234",
		"javascript:fetch('http://127.0.0.1:9222/json')",
		"jar:http://example.com/a.jar!/b.png",
	}

	for _, raw := range unsafe {
		t.Run(raw, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			classifier := offlinecache.NewClassifier(
				mocks.NewMockHTTPClient(ctrl), publicResolver{})

			got, err := classifier.Classify(context.Background(), raw)
			require.Error(t, err)
			assert.ErrorIs(t, err, offlinecache.ErrUnsafeSource)
			assert.Equal(t, offlinecache.ClassUnknown, got)
		})
	}
}

// TestClassify_RejectsReservedLiteralAddresses pins the SSRF cases that
// need no DNS at all. The loopback ports are the live services this
// device actually runs — Chromium's DevTools endpoints (9222 kiosk,
// 9223 capture), the unauthenticated hub, sys-monitord — and reaching
// any of them through a playlist would be a privilege escalation, not
// merely a bad download.
func TestClassify_RejectsReservedLiteralAddresses(t *testing.T) {
	unsafe := []string{
		"http://127.0.0.1:9222/json/new?http://evil.example",
		"http://127.0.0.1:9223/json/version",
		"http://127.0.0.1:1111/api/cast",
		"http://127.0.0.1:9001/metrics",
		"http://localhost:8080/",  // a NAME, so it takes the resolver path
		"http://[::1]:9222/json",  // IPv6 loopback
		"http://0.0.0.0:1111/",    // unspecified
		"http://10.0.0.5/art.png", // RFC 1918
		"http://172.16.4.9/art.png",
		"http://192.168.31.208:1111/api/cast",      // this device on its own LAN
		"http://169.254.169.254/latest/meta-data/", // link-local metadata service
		"http://[fe80::1]/",
		"http://[fd00::1]/",              // IPv6 ULA
		"http://[::ffff:127.0.0.1]/json", // IPv4-mapped loopback
		"http://100.64.0.1/",             // CGNAT
		"http://255.255.255.255/",        // broadcast
		"http://[64:ff9b::7f00:1]/",      // NAT64-wrapped 127.0.0.1
		"http://[2002:7f00:1::]/",        // 6to4-wrapped 127.0.0.1
	}

	for _, raw := range unsafe {
		t.Run(raw, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			// "localhost" is a NAME, not a literal, so it takes the
			// resolver path — point that at loopback the way a real
			// system resolver would.
			classifier := offlinecache.NewClassifier(
				mocks.NewMockHTTPClient(ctrl), fixedResolver{ip: "127.0.0.1"})

			got, err := classifier.Classify(context.Background(), raw)
			require.Error(t, err)
			assert.ErrorIs(t, err, offlinecache.ErrUnsafeSource)
			assert.Equal(t, offlinecache.ClassUnknown, got)
		})
	}
}

// TestClassify_RejectsHostnameResolvingToReserved pins the DNS half of
// the guard: a perfectly ordinary-looking public hostname whose A record
// points at loopback is the standard way to smuggle an SSRF past a
// string-matching filter.
func TestClassify_RejectsHostnameResolvingToReserved(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	classifier := offlinecache.NewClassifier(
		mocks.NewMockHTTPClient(ctrl), fixedResolver{ip: "127.0.0.1"})

	got, err := classifier.Classify(context.Background(), "https://art.example.com/piece")
	require.Error(t, err)
	assert.ErrorIs(t, err, offlinecache.ErrUnsafeSource)
	assert.Equal(t, offlinecache.ClassUnknown, got)
}

// TestClassify_RejectsWhenAnyResolvedAddressIsReserved pins that the
// check is over EVERY answer, not just the first. A name returning one
// public and one loopback address would otherwise be admitted here and
// then dialed round-robin at fetch time.
func TestClassify_RejectsWhenAnyResolvedAddressIsReserved(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	classifier := offlinecache.NewClassifier(
		mocks.NewMockHTTPClient(ctrl),
		multiResolver{ips: []string{"93.184.216.34", "127.0.0.1"}})

	got, err := classifier.Classify(context.Background(), "https://art.example.com/piece")
	require.Error(t, err)
	assert.ErrorIs(t, err, offlinecache.ErrUnsafeSource)
	assert.Equal(t, offlinecache.ClassUnknown, got)
}

// TestClassify_RejectsUserinfoDisguisedHost pins that the credential
// prefix trick is resolved to the real target host rather than read off
// the raw string — "http://cdn.example.com@127.0.0.1/" connects to
// 127.0.0.1, and a check that matched on the visible prefix would wave
// it straight through. replay.go carries the same hazard note.
func TestClassify_RejectsUserinfoDisguisedHost(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	classifier := offlinecache.NewClassifier(
		mocks.NewMockHTTPClient(ctrl), publicResolver{})

	got, err := classifier.Classify(context.Background(),
		"http://cdn.feralfileassets.com@127.0.0.1:9222/json/new")
	require.Error(t, err)
	assert.ErrorIs(t, err, offlinecache.ErrUnsafeSource)
	assert.Equal(t, offlinecache.ClassUnknown, got)
}

// TestClassify_ResolveFailureIsNotTaggedUnsafe pins that a transient DNS
// fault reports as a plain error, NOT as a security rejection: an
// offline device must not look like it is under attack in the logs.
func TestClassify_ResolveFailureIsNotTaggedUnsafe(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	classifier := offlinecache.NewClassifier(
		mocks.NewMockHTTPClient(ctrl),
		fixedResolver{err: errors.New("server misbehaving")})

	got, err := classifier.Classify(context.Background(), "https://art.example.com/piece")
	require.Error(t, err)
	assert.NotErrorIs(t, err, offlinecache.ErrUnsafeSource)
	assert.Equal(t, offlinecache.ClassUnknown, got)
}

// TestClassify_AllowsPublicSource pins that the guard does not break the
// normal path: a public CDN host still classifies via a real probe.
func TestClassify_AllowsPublicSource(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const src = "https://cdn.feralfileassets.com/previews/abc/preview.mp4"

	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	req, err := http.NewRequest(http.MethodHead, src, nil)
	require.NoError(t, err)
	mockHTTP.EXPECT().NewRequest(http.MethodHead, src, nil).Return(req, nil).Times(1)
	mockHTTP.EXPECT().Do(gomock.Any()).
		Return(newTestResponse(http.StatusOK, "video/mp4"), nil).Times(1)

	classifier := offlinecache.NewClassifier(mockHTTP, fixedResolver{ip: "93.184.216.34"})

	got, err := classifier.Classify(context.Background(), src)
	require.NoError(t, err)
	assert.Equal(t, offlinecache.ClassMedia, got)
}

// TestClassify_StreamingSuffixDoesNotBypassGuard pins the ordering
// hazard: the .m3u8/.mpd shortcut returns without a network call, so if
// it ran BEFORE the guard an unsafe URL could earn a benign-looking
// classification instead of being rejected.
func TestClassify_StreamingSuffixDoesNotBypassGuard(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	classifier := offlinecache.NewClassifier(
		mocks.NewMockHTTPClient(ctrl), publicResolver{})

	got, err := classifier.Classify(context.Background(), "file:///home/feralfile/master.m3u8")
	require.Error(t, err)
	assert.ErrorIs(t, err, offlinecache.ErrUnsafeSource)
	assert.Equal(t, offlinecache.ClassUnknown, got)
}
