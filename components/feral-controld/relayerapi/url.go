package relayerapi

import (
	"net/url"
	"strings"
)

// HTTPBaseString normalizes the configured relayer endpoint to the HTTP API
// origin used by relayer REST endpoints. WebSocket endpoints intentionally
// drop their path because the REST API is rooted at the relayer origin.
func HTTPBaseString(endpoint string) string {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return ""
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
		u.Path = ""
		u.RawPath = ""
	case "ws":
		u.Scheme = "http"
		u.Path = ""
		u.RawPath = ""
	}
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/")
}
