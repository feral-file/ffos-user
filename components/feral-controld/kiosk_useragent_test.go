package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func TestKioskUserAgent_ReadsTheKiosksOwnAgent(t *testing.T) {
	const ua = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"
	up := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" || !up {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"Browser":"Chrome/150.0.7871.114","User-Agent":"` + ua + `"}`))
	}))
	defer srv.Close()

	k := newKioskUserAgent(srv.URL+"/", zap.NewNop())
	assert.Empty(t, k.Get(), "unknown before the first read")
	assert.True(t, k.read(context.Background()))
	assert.Equal(t, ua, k.Get())

	up = false // the kiosk restarts
	assert.False(t, k.read(context.Background()))
	assert.Equal(t, ua, k.Get(), "the last good agent is kept")
}
