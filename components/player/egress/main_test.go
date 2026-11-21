package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPrivateHTTPAndTunnelRequestsDenied(t *testing.T) {
	for _, tc := range []struct{ method, target string }{
		{"GET", "http://192.168.1.2/item"},
		{"CONNECT", "[::ffff:127.0.0.1]:443"},
		{"GET", "http://printer.local/item"},
	} {
		req := httptest.NewRequest(tc.method, tc.target, nil)
		response := httptest.NewRecorder()
		proxy(response, req)
		if response.Code != http.StatusBadGateway {
			t.Errorf("%s %s returned %d", tc.method, tc.target, response.Code)
		}
	}
}
