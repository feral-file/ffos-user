package relayerapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHTTPBaseString_NormalizesWebSocketEndpointToOrigin(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     string
	}{
		{
			name:     "wss endpoint with path",
			endpoint: "wss://relayer.example/ws",
			want:     "https://relayer.example",
		},
		{
			name:     "ws endpoint with path query and fragment",
			endpoint: "ws://127.0.0.1:8080/ws?topic=abc#debug",
			want:     "http://127.0.0.1:8080",
		},
		{
			name:     "http path is preserved",
			endpoint: "https://relayer.example/base/",
			want:     "https://relayer.example/base",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, HTTPBaseString(tt.endpoint))
		})
	}
}
