package offlinecache

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBrowserHeadersKeepEachSecurityPolicy(t *testing.T) {
	fields, ok := browserReplayHeaders(map[string]string{"Content-Security-Policy": "default-src 'self'\nscript-src 'none'", "X-Frame-Options": "DENY", "Content-Encoding": "gzip", "Content-Length": "12", "Vary": "Accept-Encoding"})
	require.True(t, ok)
	count := 0
	for _, f := range fields {
		if f.Name == "Content-Security-Policy" {
			count++
		}
		require.NotEqual(t, "Content-Encoding", f.Name)
		require.NotEqual(t, "Content-Length", f.Name)
	}
	require.Equal(t, 2, count)
	for _, raw := range []map[string]string{{"Set-Cookie": "secret=1"}, {"Vary": "Cookie"}, {"Content-Security-Policy": "self\r\nignored"}} {
		_, ok = browserReplayHeaders(raw)
		require.False(t, ok)
	}
}
