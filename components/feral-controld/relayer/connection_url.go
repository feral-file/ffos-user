package relayer

import (
	"errors"
	"net/url"
)

// legacyConnectionURL selects the compatibility route independently of its
// optional API key. A merged broker deployment may omit the key; that must not
// send the WebSocket handshake to the root or put the topic in the path.
func legacyConnectionURL(endpoint, apiKey, topicID string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "ws" && u.Scheme != "wss") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid legacy relayer WebSocket endpoint")
	}
	if u.Path != "" && u.Path != "/" && u.Path != "/api/connection" {
		return "", errors.New("invalid legacy relayer connection path")
	}
	u.Path = "/api/connection"
	q := u.Query()
	if apiKey != "" {
		q.Set("apiKey", apiKey)
	}
	if topicID != "" {
		q.Set("topicID", topicID)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
