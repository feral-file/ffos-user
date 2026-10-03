package mintpairing

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	minter "github.com/feral-file/ff-art-computer-handoff/clients/ephemeral-token-minter/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// joinBroker answers one minter join of ch_site; mintRequest, when set, is the
// request metadata the site announced at create.
func joinBroker(t *testing.T, mintRequest json.RawMessage) *httptest.Server {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	require.NoError(t, err)
	raw := key.PublicKey().Bytes() // 0x04 || X || Y
	browserJWK := map[string]string{
		"kty": "EC",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(raw[1:33]),
		"y":   base64.RawURLEncoding.EncodeToString(raw[33:65]),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/channels/ch_site/join", r.URL.Path)
		body := map[string]any{
			"channelId":           "ch_site",
			"role":                "minter",
			"minterToken":         "mt_joined",
			"algorithm":           minter.Algorithm,
			"browserPublicKeyJwk": browserJWK,
			"origin":              testSiteOrigin,
			"browserInfo":         map[string]string{"name": "Art Blocks"},
			"expiresAt":           time.Now().Add(time.Minute).UTC(),
			"nextSeq":             1,
		}
		if mintRequest != nil {
			body["mintRequest"] = mintRequest
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		require.NoError(t, json.NewEncoder(w).Encode(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestRealBrokerJoiner_CarriesTheAnnouncedRequest(t *testing.T) {
	server := joinBroker(t, json.RawMessage(`{"v":1,"requestMessageId":"msg_site","supportsPersistentSessions":true,"requestedExpiresInSeconds":600}`))
	joined, err := realBrokerJoiner{client: minter.NewClient(server.Client())}.JoinChannel(context.Background(), joinChannelRequest{
		BrokerBaseURL: server.URL,
		ChannelID:     "ch_site",
		PairingToken:  "pt_secret",
	})
	require.NoError(t, err)
	require.NotNil(t, joined.announcedRequest)
	assert.Equal(t, "msg_site", joined.announcedRequest.MessageID)
	assert.Equal(t, testSiteOrigin, joined.announcedRequest.Origin)
	assert.Equal(t, 600, joined.announcedRequest.RequestedExpiresInSeconds)
	assert.True(t, joined.announcedRequest.SupportsPersistentSessions)
}

func TestRealBrokerJoiner_NoAnnouncementWaitsForTheEncryptedRequest(t *testing.T) {
	server := joinBroker(t, nil)
	joined, err := realBrokerJoiner{client: minter.NewClient(server.Client())}.JoinChannel(context.Background(), joinChannelRequest{
		BrokerBaseURL: server.URL,
		ChannelID:     "ch_site",
		PairingToken:  "pt_secret",
	})
	require.NoError(t, err)
	assert.Nil(t, joined.announcedRequest)
	assert.Equal(t, testSiteOrigin, joined.origin)
}
