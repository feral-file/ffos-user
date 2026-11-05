// Package sigverify computes the DP-1 signature verdict for a playlist
// document (core spec §7.1) and holds the small amount of state the rest of
// the daemon needs to report it (feral-file/ffos-user#307).
//
// The package is deliberately narrow: Verify is a pure function over bytes,
// and the mode/policy that decides what a verdict MEANS for a cast lives in
// the caller (commandrouter) so this package never grows into a second
// command router. It imports dp1-go's sign package and nothing of ours.
//
// What a verdict proves — and what it does not. dp1-go verifies each
// signatures[] entry with the public key embedded in its own kid (did:key
// carries the Ed25519 key, did:pkh the Ethereum address). A StatusValid
// verdict therefore proves the document is byte-for-byte (after JCS) what the
// key named in kid signed, i.e. it has not been tampered with in transit and
// no placeholder signature was attached. It does NOT prove the signer is one
// we trust: a feed re-signing a document with a key of its own choosing still
// verifies. Trust anchoring (a trusted-key list, per-URL key pinning) is a
// deliberately separate, later layer; Signers carries the kids so that layer
// has data to work from.
//
// Dependency note: dp1-go/sign brings go-ethereum (for eip191 / did:pkh) and
// go-multibase into this module — about 1.6 MB of binary, CGO-free. Accepted
// deliberately over an in-house Ed25519-only verifier so the device judges
// documents with the same code the spec ecosystem signs them with, eip191
// included (the production feed already carries one).
package sigverify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/display-protocol/dp1-go/sign"
)

// Status is the three-way outcome of verifying one document.
type Status string

const (
	// StatusValid: signatures[] is present and every entry verifies.
	StatusValid Status = "valid"
	// StatusInvalid: signatures[] is present and at least one entry does
	// not verify — tampered content (payload_hash mismatch), a placeholder or
	// wrong-key signature, an algorithm dp1-go does not implement, or a
	// malformed entry. The only status meaning "a claim was made and it is
	// false"; the mode policy treats it like unsigned, the reason string
	// tells them apart.
	StatusInvalid Status = "invalid"
	// StatusUnsigned: no signatures[] (or an empty one). A legacy v1.0
	// `signature` string alone is ALSO reported as unsigned: verifying it
	// needs an out-of-band public key this daemon has no source for, so it
	// carries no checkable claim. LegacyPresent records that it was there.
	StatusUnsigned Status = "unsigned"
)

// Signer is one signatures[] entry's identity plus its own verification
// outcome. Kid is a DID (did:key / did:pkh), safe to log and to return to
// casters: it names a public key, never a URL or a credential.
type Signer struct {
	Alg  string `json:"alg"`
	Kid  string `json:"kid"`
	Role string `json:"role"`
	OK   bool   `json:"ok"`
	// Reason is set only when OK is false; one of the reason vocabulary
	// strings below.
	Reason string `json:"reason,omitempty"`
}

// Verdict is the result of Verify for one document.
type Verdict struct {
	Status Status
	// Signers lists every signatures[] entry in document order; nil when the
	// document is unsigned.
	Signers []Signer
	// LegacyPresent reports a v1.0.x top-level `signature` string was found.
	// Informational only (see StatusUnsigned).
	LegacyPresent bool
	// Reason is a short, caster-safe explanation for a non-valid status:
	// "unsigned", "unsigned; legacy signature ignored", or the first failed
	// signer's "<role> signature invalid: <reason>". Empty when valid.
	// Never contains a URL or a kid.
	Reason string
}

// Reason vocabulary for a failed signer. Stable strings returned in diagnostic
// cast replies and asserted by tests. No signer reason interpolates document
// text; the unsupported algorithm's name lives in Signer.Alg instead.
const (
	ReasonPayloadHashMismatch = "payload_hash mismatch"
	ReasonSignatureInvalid    = "signature invalid"
	ReasonUnsupportedAlg      = "unsupported alg"
	ReasonMalformed           = "malformed signature"
	ReasonTooManySignatures   = "too many signatures"
	ReasonDocumentTooLarge    = "document too large"
	// ReasonUnverified marks an in-bounds entry that was never checked
	// because a sibling entry tripped a bound and the document was refused
	// before any cryptography ran.
	ReasonUnverified = "unverified"
	// Document-level reasons for the unsigned verdict.
	ReasonUnsigned       = "unsigned"
	ReasonUnsignedLegacy = "unsigned; legacy signature ignored"
)

// Signer field bounds. alg, kid, and role are copied from an untrusted
// document into the cast reply and the log line, so their length must not
// be the caster's choice: an entry past any of these is reported as
// malformed with the oversized field blanked, verified by nothing, and the
// document is invalid regardless of what dp1-go says about the other
// entries (dp1-go does not look at role at all, so a valid signature with a
// megabyte role would otherwise come back "valid"). A did:key is ~56 chars
// and a did:pkh ~60; the DP-1 role and alg vocabularies are single words.
const (
	MaxAlgLen  = 32
	MaxRoleLen = 32
	MaxKidLen  = 256
)

// MaxDocumentBytes bounds the document Verify will canonicalize at all.
// It MUST stay >= the largest document any ingress admits. There is no
// "unverifiable" status, so an over-cap document is reported invalid — the
// same bucket as a tampered one — and a cap below an ingress would brand
// honest documents as forgeries rather than merely decline to judge them.
//
// The ingresses no longer agree on one figure: the inline paths cap at
// 4 MiB (hub request body, relayer frame, control v1 compact form) while a
// URL fetch admits dp1.MaxPlaylistBodyBytes, 10 MiB, so that the Feral File
// series published above 4 MiB can be displayed (feral-file/feral-file#3597).
// This tracks the LARGEST of them, not the common one.
//
// It remains this package's own bound rather than a trusted inherited one,
// because commandrouter's dp1_call fallback can hand Verify more bytes than
// any ingress measured: encoding/json HTML-escapes &/</> at six bytes each.
// Over the cap: invalid, no cryptography.
const MaxDocumentBytes = 10 << 20

// MaxSignatures bounds how many signatures[] entries Verify will even hand
// to dp1-go. The cost it defends against is no longer document-sized:
// since the dp1-go version this module pins, one call canonicalizes the
// document once and each entry then costs a single signature check over the
// resulting 32 bytes — microseconds, independent of document size. Before
// that, every entry cost two whole-document canonicalizations (payload_hash
// check + signing digest), which made an unbounded array hours of CPU per
// hostile cast on an unauthenticated 4 MiB body. That per-entry cost is
// what sized this cap, and what sized the 4 MiB ingress figures.
//
// The cap stays because what remains is still attacker-chosen: N ECDSA
// recoveries for eip191, and N alg/kid/role strings copied into the cast
// reply and the log line. DP-1 defines six roles (curator, feed, agent,
// institution, licensor, publisher); 16 is comfortably above any honest
// chain. Past it the document is invalid with ReasonTooManySignatures and
// NO cryptography runs, so the cost of a hostile document is one envelope
// decode.
const MaxSignatures = 16

// signatureEnvelope is the minimal top-level projection Verify needs beyond
// what dp1-go decodes itself: the legacy string, and the entries in document
// order so Signers can be built.
type signatureEnvelope struct {
	// Signature is raw so PRESENCE can be told apart from an empty string:
	// the wire contract flags legacySignature whenever a v1.0 `signature`
	// string is present, "" included.
	Signature  json.RawMessage `json:"signature"`
	Signatures json.RawMessage `json:"signatures"`
}

// legacyPresent reports whether the top-level `signature` field carried a
// JSON string of any length. null or a non-string is not a legacy signature.
func legacyPresent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	// json.Unmarshal accepts null into a string without error, so the
	// literal is inspected directly: only a string token counts.
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return false
	}
	var s string
	return json.Unmarshal(trimmed, &s) == nil
}

// Verify computes the verdict for raw, a complete DP-1 playlist document as
// JSON. It is pure and safe for concurrent use.
//
// raw need not be the publisher's wire bytes: dp1-go verifies over the JCS
// canonical form (RFC 8785) of the document minus its signature fields, so
// whitespace, key order, and a round-trip through encoding/json or a Go
// struct that preserves every field all verify identically. What DOES break
// verification is dropping or adding a field — which is why callers must
// verify the bytes they received or re-marshaled from a generic map, never a
// typed struct that may have discarded unknown keys (see commandrouter's
// dp1_call branch), and must verify BEFORE dynamic hydration rewrites items.
func Verify(raw []byte) Verdict {
	if len(raw) > MaxDocumentBytes {
		return Verdict{Status: StatusInvalid, Reason: ReasonDocumentTooLarge}
	}
	var env signatureEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// Not a JSON object at all. Reported as invalid rather than unsigned
		// so a garbage document is never mistaken for an honest unsigned one.
		return Verdict{Status: StatusInvalid, Reason: ReasonMalformed}
	}
	legacy := legacyPresent(env.Signature)

	// Count entries BEFORE dp1-go touches the document (see MaxSignatures),
	// and without materializing them: the decoder stops at the first entry
	// past the cap, so a hostile array costs at most MaxSignatures+1 small
	// element copies, never one allocation per entry. A non-array is left
	// for dp1-go to classify as malformed below.
	if tooManySignatures(env.Signatures) {
		return Verdict{
			Status:        StatusInvalid,
			LegacyPresent: legacy,
			Reason:        fmt.Sprintf("%s (> %d)", ReasonTooManySignatures, MaxSignatures),
		}
	}

	// Decode the (count-bounded) entries and enforce the field bounds BEFORE
	// dp1-go sees the document: the bounds exist so an oversized entry buys
	// no canonicalization of the document at all. A decode failure or an
	// empty array is left for dp1-go below, which classifies both the same
	// way and does no cryptography on either.
	var entries []dp1playlist.Signature
	if uerr := json.Unmarshal(env.Signatures, &entries); uerr == nil && len(entries) > 0 {
		if v, refused := refuseOversized(entries, legacy); refused {
			return v
		}
	}

	// One call, one canonicalization of the document, and every entry's own
	// error. dp1-go's detailed verifier exists for exactly this caller: the
	// plain VerifyPlaylistSignatures returns the failed entries WITHOUT their
	// errors, so labeling which entry failed and why used to mean re-verifying
	// each one here — another whole-document canonicalization apiece, on the
	// failure path, which is the path a hostile document chooses
	// (display-protocol/dp1-go#16).
	results, err := sign.VerifyMultiSignaturesJSONDetailed(raw)
	switch {
	case errors.Is(err, sign.ErrNoSignatures):
		reason := ReasonUnsigned
		if legacy {
			reason = ReasonUnsignedLegacy
		}
		return Verdict{Status: StatusUnsigned, LegacyPresent: legacy, Reason: reason}
	case err != nil:
		// signatures is present but not an array of signature objects.
		return Verdict{Status: StatusInvalid, LegacyPresent: legacy, Reason: ReasonMalformed}
	}
	if len(results) != len(entries) {
		// Cannot happen: dp1-go decoded the same array from the same bytes
		// this function just counted and field-bounded. Guarded so a library
		// change surfaces as invalid, never as a silently valid verdict over
		// entries the bounds above never saw.
		return Verdict{Status: StatusInvalid, LegacyPresent: legacy, Reason: ReasonMalformed}
	}

	// The verdict is read off the per-entry results alone. There is no
	// document-level ok to agree with any more, so the two can no longer
	// contradict each other and there is nothing to arbitrate: valid means
	// every entry reported OK.
	signers := make([]Signer, 0, len(results))
	firstFailure := ""
	for _, r := range results {
		e := r.Signature
		s := Signer{Alg: e.Alg, Kid: e.Kid, Role: e.Role, OK: r.OK()}
		if !s.OK {
			s.Reason = classify(r.Err)
			if firstFailure == "" {
				firstFailure = fmt.Sprintf("%s signature invalid: %s", roleOrUnknown(e.Role), s.Reason)
			}
		}
		signers = append(signers, s)
	}
	if firstFailure == "" {
		return Verdict{Status: StatusValid, Signers: signers, LegacyPresent: legacy}
	}
	return Verdict{Status: StatusInvalid, Signers: signers, LegacyPresent: legacy, Reason: firstFailure}
}

// refuseOversized reports the invalid verdict for a document in which any
// entry exceeds a signer field bound, or (nil, false) when every entry is in
// bounds. The oversized fields are blanked so nothing caster-sized is echoed;
// in-bounds sibling entries are listed as unverified, because the document
// is refused before any of them is checked.
func refuseOversized(entries []dp1playlist.Signature, legacy bool) (Verdict, bool) {
	refused := false
	signers := make([]Signer, 0, len(entries))
	firstFailure := ""
	for _, e := range entries {
		s := Signer{Alg: e.Alg, Kid: e.Kid, Role: e.Role, OK: false, Reason: ReasonUnverified}
		over := s.Alg == "" || len(s.Alg) > MaxAlgLen || len(s.Kid) > MaxKidLen || len(s.Role) > MaxRoleLen
		if over {
			if len(s.Alg) > MaxAlgLen {
				s.Alg = ""
			}
			if len(s.Kid) > MaxKidLen {
				s.Kid = ""
			}
			if len(s.Role) > MaxRoleLen {
				s.Role = ""
			}
			s.Reason = ReasonMalformed
			refused = true
			if firstFailure == "" {
				firstFailure = fmt.Sprintf("%s signature invalid: %s", roleOrUnknown(s.Role), s.Reason)
			}
		}
		signers = append(signers, s)
	}
	if !refused {
		return Verdict{}, false
	}
	return Verdict{Status: StatusInvalid, Signers: signers, LegacyPresent: legacy, Reason: firstFailure}, true
}

// tooManySignatures streams the signatures array and reports true as soon
// as it sees more than MaxSignatures elements. Anything that is not an array
// reports false so the caller's normal decode path classifies it.
func tooManySignatures(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('[') {
		return false
	}
	count := 0
	for dec.More() {
		var elem json.RawMessage
		if err := dec.Decode(&elem); err != nil {
			return false
		}
		count++
		if count > MaxSignatures {
			return true
		}
	}
	return false
}

// classify maps a dp1-go per-entry verification error onto the reason
// vocabulary. Order matters: ErrSigInvalid wraps the base64 and length
// failures too, so it must be tested before the generic fallback. The
// payload-hash mismatch was an unwrapped error matched by its text until
// dp1-go exported ErrPayloadHashMismatch for this caller; all three arms are
// pinned by the tamper tests.
func classify(err error) string {
	switch {
	case errors.Is(err, sign.ErrUnsupportedAlg):
		return ReasonUnsupportedAlg
	case errors.Is(err, sign.ErrSigInvalid):
		return ReasonSignatureInvalid
	case errors.Is(err, sign.ErrPayloadHashMismatch):
		return ReasonPayloadHashMismatch
	default:
		return ReasonMalformed
	}
}

func roleOrUnknown(role string) string {
	if role == "" {
		return "unknown-role"
	}
	return role
}
