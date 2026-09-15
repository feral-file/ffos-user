package sigverify_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/display-protocol/dp1-go/sign"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/sigverify"
)

// feedFixtureKid is the production feed's signing key at the time the fixture
// was captured (2026-09-11). If the feed rotates keys the fixture stays valid:
// the kid is embedded in the document itself.
const feedFixtureKid = "did:key:z6MkiCBAPqLbkzZmLG2nAyfzJqfiEr58NDscj2ar4FJ1pP3U"

func loadFeedFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "feed-dd875fbe.json"))
	require.NoError(t, err)
	return raw
}

func TestVerify_LiveFeedFixture_Valid(t *testing.T) {
	v := sigverify.Verify(loadFeedFixture(t))

	assert.Equal(t, sigverify.StatusValid, v.Status)
	assert.Empty(t, v.Reason)
	assert.False(t, v.LegacyPresent)
	require.Len(t, v.Signers, 1)
	assert.Equal(t, sigverify.Signer{Alg: "ed25519", Kid: feedFixtureKid, Role: "feed", OK: true}, v.Signers[0])
}

// TestVerify_MapRoundTrip_StillValid pins that JCS canonicalization makes a
// key-order/whitespace-different document verify identically — the property
// commandrouter's in-process fallback (a re-marshaled map, used only when a
// command has no wire form) relies on. The ingress paths themselves verify
// the caller's wire token, for size reasons (see
// TestVerify_BigIntegerToken_SurvivesMapRoundTrip for why not for numeric
// ones).
func TestVerify_MapRoundTrip_StillValid(t *testing.T) {
	raw := loadFeedFixture(t)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	remarshaled, err := json.Marshal(m)
	require.NoError(t, err)
	require.NotEqual(t, raw, remarshaled, "fixture must not already be in encoding/json's canonical order for this test to mean anything")

	assert.Equal(t, sigverify.StatusValid, sigverify.Verify(remarshaled).Status)
}

// TestVerify_TypedRoundTrip_StillValid documents that today's dp1.Playlist
// preserves every field of a feed playlist, so a struct round-trip verifies.
// It is a canary, not a contract callers may rely on: a future field the
// struct drops would flip it, and callers must keep verifying the received
// bytes (see Verify's doc).
func TestVerify_TypedRoundTrip_StillValid(t *testing.T) {
	raw := loadFeedFixture(t)
	var p dp1.Playlist
	require.NoError(t, json.Unmarshal(raw, &p))
	remarshaled, err := json.Marshal(p)
	require.NoError(t, err)

	assert.Equal(t, sigverify.StatusValid, sigverify.Verify(remarshaled).Status)
}

func TestVerify_TamperedContent_PayloadHashMismatch(t *testing.T) {
	raw := loadFeedFixture(t)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	items := m["items"].([]any)
	items[0].(map[string]any)["source"] = "https://evil.example/swap"
	tampered, err := json.Marshal(m)
	require.NoError(t, err)

	v := sigverify.Verify(tampered)

	assert.Equal(t, sigverify.StatusInvalid, v.Status)
	require.Len(t, v.Signers, 1)
	assert.False(t, v.Signers[0].OK)
	assert.Equal(t, sigverify.ReasonPayloadHashMismatch, v.Signers[0].Reason)
	assert.Equal(t, "feed signature invalid: payload_hash mismatch", v.Reason)
	// Sanitization: the reason must never carry document content.
	assert.NotContains(t, v.Reason, "evil.example")
}

func TestVerify_KidSwapped_SignatureInvalid(t *testing.T) {
	raw := loadFeedFixture(t)
	// Another well-formed did:key (a real agent key seen on the feed).
	other := "did:key:z6Mkp9zyd8ecbEozk3qoLaFHDbBKnyXMuby9sMdeA8ygGzzw"
	swapped := []byte(strings.Replace(string(raw), feedFixtureKid, other, 1))
	require.NotEqual(t, raw, swapped)

	v := sigverify.Verify(swapped)

	assert.Equal(t, sigverify.StatusInvalid, v.Status)
	require.Len(t, v.Signers, 1)
	assert.Equal(t, other, v.Signers[0].Kid)
	assert.Equal(t, sigverify.ReasonSignatureInvalid, v.Signers[0].Reason)
}

// unsignedDoc is a minimal playlist with no signature fields at all.
func unsignedDoc() map[string]any {
	return map[string]any{
		"dpVersion": "1.1.0",
		"id":        "0f4a5f1e-3f39-4a4e-9c7e-6a2d7b4a1c11",
		"title":     "unsigned",
		"items": []any{
			map[string]any{
				"id":       "6f1c1d2e-1b2a-4c3d-8e9f-0a1b2c3d4e5f",
				"source":   "https://example.com/a",
				"duration": 10,
				"license":  "open",
			},
		},
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func TestVerify_NoSignatures_Unsigned(t *testing.T) {
	v := sigverify.Verify(mustJSON(t, unsignedDoc()))

	assert.Equal(t, sigverify.StatusUnsigned, v.Status)
	assert.Nil(t, v.Signers)
	assert.False(t, v.LegacyPresent)
	assert.Equal(t, "unsigned", v.Reason)
}

func TestVerify_EmptySignaturesArray_Unsigned(t *testing.T) {
	doc := unsignedDoc()
	doc["signatures"] = []any{}

	v := sigverify.Verify(mustJSON(t, doc))

	assert.Equal(t, sigverify.StatusUnsigned, v.Status)
}

func TestVerify_LegacyOnly_UnsignedAndFlagged(t *testing.T) {
	doc := unsignedDoc()
	doc["signature"] = "ed25519:" + strings.Repeat("ab", 64)

	v := sigverify.Verify(mustJSON(t, doc))

	assert.Equal(t, sigverify.StatusUnsigned, v.Status)
	assert.True(t, v.LegacyPresent)
	assert.Equal(t, "unsigned; legacy signature ignored", v.Reason)
}

// TestVerify_EmptyLegacyString_StillFlagged: presence, not value, drives
// the legacy flag — a v1.0 producer that emits "signature": "" is still a
// legacy producer the controller should be told about.
func TestVerify_EmptyLegacyString_StillFlagged(t *testing.T) {
	doc := unsignedDoc()
	doc["signature"] = ""

	v := sigverify.Verify(mustJSON(t, doc))

	assert.Equal(t, sigverify.StatusUnsigned, v.Status)
	assert.True(t, v.LegacyPresent)
	assert.Equal(t, "unsigned; legacy signature ignored", v.Reason)
}

func TestVerify_NullLegacy_NotFlagged(t *testing.T) {
	doc := unsignedDoc()
	doc["signature"] = nil

	v := sigverify.Verify(mustJSON(t, doc))

	assert.Equal(t, sigverify.StatusUnsigned, v.Status)
	assert.False(t, v.LegacyPresent)
}

func TestVerify_NotJSON_Invalid(t *testing.T) {
	v := sigverify.Verify([]byte("not json"))

	assert.Equal(t, sigverify.StatusInvalid, v.Status)
	assert.Equal(t, sigverify.ReasonMalformed, v.Reason)
}

func TestVerify_SignaturesNotAnArray_Invalid(t *testing.T) {
	doc := unsignedDoc()
	doc["signatures"] = "yes"

	v := sigverify.Verify(mustJSON(t, doc))

	assert.Equal(t, sigverify.StatusInvalid, v.Status)
	assert.Equal(t, sigverify.ReasonMalformed, v.Reason)
}

// signWith appends a real Ed25519 signature entry for the document as it
// currently stands (dp1-go signs the JCS form minus signature fields, so
// signing after other entries were appended is the multi-signer flow).
func signWith(t *testing.T, doc map[string]any, priv ed25519.PrivateKey, role string) {
	t.Helper()
	entry, err := sign.SignMultiEd25519(mustJSON(t, doc), priv, role, "2026-09-11T00:00:00Z")
	require.NoError(t, err)
	var existing []any
	if cur, ok := doc["signatures"].([]any); ok {
		existing = cur
	}
	var asMap map[string]any
	require.NoError(t, json.Unmarshal(mustJSON(t, entry), &asMap))
	doc["signatures"] = append(existing, asMap)
}

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return priv
}

func TestVerify_TwoSignerChain_Valid(t *testing.T) {
	doc := unsignedDoc()
	signWith(t, doc, newKey(t), dp1playlist.RoleCurator)
	signWith(t, doc, newKey(t), dp1playlist.RoleFeed)

	v := sigverify.Verify(mustJSON(t, doc))

	assert.Equal(t, sigverify.StatusValid, v.Status)
	require.Len(t, v.Signers, 2)
	assert.Equal(t, dp1playlist.RoleCurator, v.Signers[0].Role)
	assert.Equal(t, dp1playlist.RoleFeed, v.Signers[1].Role)
	assert.True(t, v.Signers[0].OK)
	assert.True(t, v.Signers[1].OK)
	assert.True(t, strings.HasPrefix(v.Signers[0].Kid, "did:key:z6Mk"))
}

// TestVerify_PlaceholderSignature_Invalid models a controller that attaches a
// syntactically shaped but empty signature: the exact "fake signature" a
// device must never mistake for provenance.
func TestVerify_PlaceholderSignature_Invalid(t *testing.T) {
	doc := unsignedDoc()
	signWith(t, doc, newKey(t), dp1playlist.RoleFeed)
	entry := doc["signatures"].([]any)[0].(map[string]any)
	entry["sig"] = ""

	v := sigverify.Verify(mustJSON(t, doc))

	assert.Equal(t, sigverify.StatusInvalid, v.Status)
	require.Len(t, v.Signers, 1)
	assert.Equal(t, sigverify.ReasonSignatureInvalid, v.Signers[0].Reason)
	assert.Equal(t, "feed signature invalid: signature invalid", v.Reason)
}

// TestVerify_UnsupportedAlg_Invalid pins the contract divergence recorded in
// docs/api-design.md: the v2 profile lists ecdsa-p256, dp1-go v0.6.0 does not
// implement it, so such an entry reads as invalid rather than being skipped.
func TestVerify_UnsupportedAlg_Invalid(t *testing.T) {
	doc := unsignedDoc()
	signWith(t, doc, newKey(t), dp1playlist.RoleFeed)
	entry := doc["signatures"].([]any)[0].(map[string]any)
	entry["alg"] = "ecdsa-p256"

	v := sigverify.Verify(mustJSON(t, doc))

	assert.Equal(t, sigverify.StatusInvalid, v.Status)
	require.Len(t, v.Signers, 1)
	assert.Equal(t, sigverify.ReasonUnsupportedAlg, v.Signers[0].Reason)
	assert.Equal(t, "ecdsa-p256", v.Signers[0].Alg, "the algorithm's name is reported in alg, never interpolated into a reason")
}

// TestVerify_OneBadEntryFailsTheChain: DP-1 requires every listed signature
// to verify, so a valid feed signature does not rescue a broken agent one,
// and the per-signer report names exactly which entry failed.
func TestVerify_OneBadEntryFailsTheChain(t *testing.T) {
	doc := unsignedDoc()
	signWith(t, doc, newKey(t), dp1playlist.RoleFeed)
	signWith(t, doc, newKey(t), dp1playlist.RoleAgent)
	agent := doc["signatures"].([]any)[1].(map[string]any)
	agent["sig"] = strings.Repeat("A", 86) // well-formed base64url, wrong bytes

	v := sigverify.Verify(mustJSON(t, doc))

	assert.Equal(t, sigverify.StatusInvalid, v.Status)
	require.Len(t, v.Signers, 2)
	assert.True(t, v.Signers[0].OK)
	assert.False(t, v.Signers[1].OK)
	assert.Equal(t, "agent signature invalid: signature invalid", v.Reason)
}

// TestVerify_TooManySignatures_RefusedWithoutCrypto pins the CPU bound: a
// document past MaxSignatures is invalid on the count alone. The entries are
// deliberately garbage — if dp1-go ran on them the reason would be
// "malformed signature", so the reason string proves the cap short-circuited.
func TestVerify_TooManySignatures_RefusedWithoutCrypto(t *testing.T) {
	doc := unsignedDoc()
	entries := make([]any, 0, sigverify.MaxSignatures+1)
	for i := 0; i <= sigverify.MaxSignatures; i++ {
		entries = append(entries, map[string]any{"alg": "ed25519", "kid": "x", "sig": "y"})
	}
	doc["signatures"] = entries

	v := sigverify.Verify(mustJSON(t, doc))

	assert.Equal(t, sigverify.StatusInvalid, v.Status)
	assert.Nil(t, v.Signers)
	assert.Equal(t, "too many signatures (> 16)", v.Reason)
}

// TestVerify_AtCap_StillVerified: exactly MaxSignatures entries are judged
// normally, so the cap cannot reject an honest long chain by one.
func TestVerify_AtCap_StillVerified(t *testing.T) {
	doc := unsignedDoc()
	for i := 0; i < sigverify.MaxSignatures; i++ {
		signWith(t, doc, newKey(t), dp1playlist.RoleAgent)
	}

	v := sigverify.Verify(mustJSON(t, doc))

	assert.Equal(t, sigverify.StatusValid, v.Status)
	assert.Len(t, v.Signers, sigverify.MaxSignatures)
}

// TestVerify_OversizedKid_MalformedAndNotEchoed pins the field bounds: an
// entry whose kid exceeds MaxKidLen is malformed, its kid is blanked so
// nothing caster-sized reaches the reply or the log, and nothing is verified.
func TestVerify_OversizedKid_MalformedAndNotEchoed(t *testing.T) {
	doc := unsignedDoc()
	signWith(t, doc, newKey(t), dp1playlist.RoleFeed)
	entry := doc["signatures"].([]any)[0].(map[string]any)
	entry["kid"] = "did:key:" + strings.Repeat("z", sigverify.MaxKidLen)

	v := sigverify.Verify(mustJSON(t, doc))

	assert.Equal(t, sigverify.StatusInvalid, v.Status)
	require.Len(t, v.Signers, 1)
	assert.Empty(t, v.Signers[0].Kid)
	assert.Equal(t, "feed", v.Signers[0].Role, "in-bounds fields are kept")
	assert.Equal(t, sigverify.ReasonMalformed, v.Signers[0].Reason)
}

// TestVerify_OversizedRole_InvalidDespiteValidCrypto: dp1-go never looks at
// role, so a cryptographically valid entry with a megabyte role would come
// back "valid" from the library. The bound must override that.
func TestVerify_OversizedRole_InvalidDespiteValidCrypto(t *testing.T) {
	doc := unsignedDoc()
	signWith(t, doc, newKey(t), strings.Repeat("r", sigverify.MaxRoleLen+1))

	v := sigverify.Verify(mustJSON(t, doc))

	assert.Equal(t, sigverify.StatusInvalid, v.Status)
	require.Len(t, v.Signers, 1)
	assert.Empty(t, v.Signers[0].Role)
	assert.Equal(t, sigverify.ReasonMalformed, v.Signers[0].Reason)
	assert.Equal(t, "unknown-role signature invalid: malformed signature", v.Reason)
}

// TestVerify_OversizedEntry_RefusedBeforeCrypto proves the bound is enforced
// before dp1-go runs: the in-bounds sibling here carries a genuinely valid
// signature, and if any verification had happened it would be OK=true. It
// is reported as unverified instead.
func TestVerify_OversizedEntry_RefusedBeforeCrypto(t *testing.T) {
	doc := unsignedDoc()
	signWith(t, doc, newKey(t), dp1playlist.RoleFeed)
	signWith(t, doc, newKey(t), dp1playlist.RoleAgent)
	agent := doc["signatures"].([]any)[1].(map[string]any)
	agent["kid"] = "did:key:" + strings.Repeat("z", sigverify.MaxKidLen)

	v := sigverify.Verify(mustJSON(t, doc))

	assert.Equal(t, sigverify.StatusInvalid, v.Status)
	require.Len(t, v.Signers, 2)
	assert.False(t, v.Signers[0].OK)
	assert.Equal(t, sigverify.ReasonUnverified, v.Signers[0].Reason, "the valid sibling must not have been verified")
	assert.Equal(t, sigverify.ReasonMalformed, v.Signers[1].Reason)
	assert.Empty(t, v.Signers[1].Kid)
	assert.Equal(t, "agent signature invalid: malformed signature", v.Reason)
}

// TestVerify_DocumentOverCap_RefusedWithoutCrypto: the verifier's own size
// bound, independent of the ingress caps that should already have refused
// such a document.
func TestVerify_DocumentOverCap_RefusedWithoutCrypto(t *testing.T) {
	doc := unsignedDoc()
	signWith(t, doc, newKey(t), dp1playlist.RoleFeed)
	doc["summary"] = strings.Repeat("s", sigverify.MaxDocumentBytes)

	v := sigverify.Verify(mustJSON(t, doc))

	assert.Equal(t, sigverify.StatusInvalid, v.Status)
	assert.Equal(t, sigverify.ReasonDocumentTooLarge, v.Reason)
	assert.Nil(t, v.Signers)
}

// TestVerify_BigIntegerToken_SurvivesMapRoundTrip documents a subtlety of
// the ingress contract: a map round-trip rounds an integer past 2^53 to a
// float64, yet the document still verifies, because JCS canonicalizes
// numbers as ES6 doubles on BOTH sides — the signer's digest was already
// computed over the rounded form. Numeric fidelity is therefore not why the
// inline path verifies the wire token; size is (HTML escaping can inflate a
// re-marshal past MaxDocumentBytes).
func TestVerify_BigIntegerToken_SurvivesMapRoundTrip(t *testing.T) {
	doc := `{"dpVersion":"1.1.0","id":"0f4a5f1e-3f39-4a4e-9c7e-6a2d7b4a1c11","title":"big","items":[{"id":"6f1c1d2e-1b2a-4c3d-8e9f-0a1b2c3d4e5f","source":"https://example.com/a","duration":9007199254740993,"license":"open"}]}`
	entry, err := sign.SignMultiEd25519([]byte(doc), newKey(t), dp1playlist.RoleFeed, "2026-09-12T00:00:00Z")
	require.NoError(t, err)
	entryJSON := mustJSON(t, entry)
	signed := []byte(strings.TrimSuffix(doc, "}") + `,"signatures":[` + string(entryJSON) + `]}`)

	assert.Equal(t, sigverify.StatusValid, sigverify.Verify(signed).Status, "the wire token verifies")

	var m map[string]any
	require.NoError(t, json.Unmarshal(signed, &m))
	remarshaled := mustJSON(t, m)
	assert.NotContains(t, string(remarshaled), "9007199254740993", "the round-trip did round the integer")
	assert.Equal(t, sigverify.StatusValid, sigverify.Verify(remarshaled).Status, "and JCS makes that irrelevant to the digest")
}

func TestVerify_ReindentedDocument_StillValid(t *testing.T) {
	var m map[string]any
	require.NoError(t, json.Unmarshal(loadFeedFixture(t), &m))
	pretty, err := json.MarshalIndent(m, "", "    ")
	require.NoError(t, err)

	assert.Equal(t, sigverify.StatusValid, sigverify.Verify(pretty).Status)
}

// TestPublicReason_NeverEchoesDocumentStrings: role and alg are the
// document's own text within a length bound, so a hostile document can put
// a URL in either. Verdict.Reason names the role (for the log and the
// owner's reply); PublicReason, which leaves the device in a strict
// rejection, must carry only the closed vocabulary.
func TestPublicReason_NeverEchoesDocumentStrings(t *testing.T) {
	doc := unsignedDoc()
	signWith(t, doc, newKey(t), dp1playlist.RoleFeed)
	entry := doc["signatures"].([]any)[0].(map[string]any)
	entry["role"] = "https://evil.example/r"
	entry["alg"] = "https://evil.example/a"

	v := sigverify.Verify(mustJSON(t, doc))

	require.Equal(t, sigverify.StatusInvalid, v.Status)
	assert.Contains(t, v.Reason, "https://evil.example/r", "the log-side reason names the role as written")
	assert.Equal(t, "signature invalid: unsupported alg", v.PublicReason())
	assert.NotContains(t, v.PublicReason(), "evil")
}

func TestPublicReason_Vocabulary(t *testing.T) {
	// Tampered content: the first failed signer's classified reason.
	var doc map[string]any
	require.NoError(t, json.Unmarshal(loadFeedFixture(t), &doc))
	doc["title"] = "tampered"
	assert.Equal(t, "signature invalid: payload_hash mismatch", sigverify.Verify(mustJSON(t, doc)).PublicReason())

	// Unsigned, with and without a legacy string.
	assert.Equal(t, sigverify.ReasonUnsigned, sigverify.Verify(mustJSON(t, unsignedDoc())).PublicReason())
	legacy := unsignedDoc()
	legacy["signature"] = "ed25519:abc"
	assert.Equal(t, sigverify.ReasonUnsignedLegacy, sigverify.Verify(mustJSON(t, legacy)).PublicReason())

	// Document-level failures are fixed text already.
	assert.Equal(t, sigverify.ReasonMalformed, sigverify.Verify([]byte("not json")).PublicReason())
	assert.Equal(t, "", sigverify.Verify(loadFeedFixture(t)).PublicReason())
	assert.Equal(t, sigverify.ReasonMalformed, sigverify.Verdict{Status: sigverify.StatusInvalid}.PublicReason(), "an invalid verdict with no detail still yields fixed text")
}
