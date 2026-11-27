package dp1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A playlist needs no signature, so a malformed one must pass the gate and the
// typed decode alike, and must not travel on in the re-emitted document.
func TestMalformedSignaturesNeverBlockTheDecode(t *testing.T) {
	for _, member := range []string{
		`"signatures":{}`,
		`"signatures":"x"`,
		`"signatures":7`,
		`"signatures":[7]`,
		`"signatures":[{"role":7}]`,
		`"signatures":[{"role":"feed"},{"sig":[]}]`,
		`"signature":7`,
		`"signature":{}`,
		`"signatures":null,"signature":null`,
	} {
		raw := []byte(`{"dpVersion":"1.1.0","title":"t","items":[{"source":"https://example.com/a"}],` + member + `}`)
		require.NoError(t, CheckPlayable(nil, "test", raw), member)

		var p Playlist
		require.NoError(t, json.Unmarshal(raw, &p), member)
		assert.Empty(t, p.Signature, member)
		assert.Nil(t, p.Signatures, member)

		out, err := json.Marshal(p)
		require.NoError(t, err, member)
		assert.NotContains(t, string(out), `"signature`, member)
	}
}

func TestWellFormedSignaturesSurviveTheDecode(t *testing.T) {
	raw := `{"dpVersion":"1.1.0","title":"t","items":[],"signature":"ed25519:ab","signatures":[{"role":"feed","kid":"k"}]}`

	var p Playlist
	require.NoError(t, json.Unmarshal([]byte(raw), &p))

	assert.Equal(t, "ed25519:ab", p.Signature)
	require.Len(t, p.Signatures, 1)
	assert.Equal(t, "feed", p.Signatures[0].Role)
}
