package relayer

import (
	"net/url"
	"testing"
)

func TestLegacyConnectionURL(t *testing.T) {
	for _, tc := range []struct{ endpoint, key, topic string }{
		{"ws://127.0.0.1:8787", "", ""},
		{"ws://127.0.0.1:8787/", "", "topic+/=&"},
		{"wss://relay.example", "key&?#", "topic"},
		{"wss://relay.example/api/connection", "", "topic"},
	} {
		t.Run(tc.endpoint+tc.key, func(t *testing.T) {
			raw, err := legacyConnectionURL(tc.endpoint, tc.key, tc.topic)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if u.Path != "/api/connection" || u.Query().Get("apiKey") != tc.key || u.Query().Get("topicID") != tc.topic {
				t.Fatal("connection path or encoded credentials do not match")
			}
			if tc.key == "" && u.Query().Has("apiKey") {
				t.Fatal("empty key must be omitted")
			}
		})
	}
	for _, endpoint := range []string{"https://relay.example", "://bad", "ws://relay.example/path", "ws://user:secret@relay.example"} {
		if _, err := legacyConnectionURL(endpoint, "", ""); err == nil {
			t.Fatalf("accepted invalid endpoint %q", endpoint)
		}
	}
}
