package offlinecache

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAutomaticFreshness(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name      string
		headers   http.Header
		remaining time.Duration
	}{
		{"immutable media", http.Header{"Cache-Control": {"public, max-age=3600, immutable"}}, time.Hour},
		{"age", http.Header{"Cache-Control": {"max-age=3600"}, "Age": {"600"}}, 50 * time.Minute},
		{"old date", http.Header{"Cache-Control": {"max-age=3600"}, "Date": {now.Add(-20 * time.Minute).Format(http.TimeFormat)}}, 40 * time.Minute},
		{"expires", http.Header{"Expires": {now.Add(time.Hour).Format(http.TimeFormat)}}, time.Hour},
		{"no policy", http.Header{}, 0},
		{"no store", http.Header{"Cache-Control": {"max-age=3600, no-store"}}, 0},
		{"revalidate", http.Header{"Cache-Control": {"max-age=3600, no-cache"}}, 0},
		{"private", http.Header{"Cache-Control": {"max-age=3600, private"}}, 0},
		{"vary cookie", http.Header{"Cache-Control": {"max-age=3600"}, "Vary": {"Cookie"}}, 0},
		{"vary encoding", http.Header{"Cache-Control": {"max-age=3600"}, "Vary": {"Accept-Encoding"}}, time.Hour},
		{"cookie", http.Header{"Cache-Control": {"max-age=3600"}, "Set-Cookie": {"session=x"}}, 0},
		{"expired", http.Header{"Cache-Control": {"max-age=30"}, "Age": {"60"}}, 0},
		{"conflicting lifetimes", http.Header{"Cache-Control": {"max-age=30, max-age=3600"}}, 0},
		{"invalid age", http.Header{"Cache-Control": {"max-age=3600"}, "Age": {"bad"}}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			until := automaticFreshUntil(tt.headers, now)
			if tt.remaining == 0 {
				require.True(t, until.IsZero())
				return
			}
			require.Equal(t, now.Add(tt.remaining), until)
		})
	}
}

func TestStorageBudget(t *testing.T) {
	const gib = int64(1 << 30)
	for _, tt := range []struct {
		name                                  string
		total, available, used, ceiling, want int64
	}{
		{"half disk", 128 * gib, 120 * gib, 0, 0, 64 * gib},
		{"configured cap", 128 * gib, 120 * gib, 0, 20 * gib, 20 * gib},
		{"reserve", 128 * gib, 15 * gib, 10 * gib, 0, 15*gib + 10*gib - 128*gib/10},
		{"small disk", 16 * gib, 6 * gib, 0, 0, gib},
		{"full disk", 128 * gib, gib, gib, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := storagePolicy{percent: 50, minFree: 5 * gib, maxBytes: tt.ceiling}
			require.Equal(t, tt.want, p.limit(tt.total, tt.available, tt.used))
		})
	}
}
