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
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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

// Reason vocabulary for a failed signer. Stable strings: they are returned
// verbatim to casters (and, in strict mode, in the rejection message) and
// asserted by tests, so treat a change as a wire change.
const (
	ReasonPayloadHashMismatch = "payload_hash mismatch"
	ReasonSignatureInvalid    = "signature invalid"
	ReasonMalformed           = "malformed signature"
	ReasonTooManySignatures   = "too many signatures"
	reasonUnsupportedAlgFmt   = "unsupported alg %s"
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

// MaxSignatures bounds how many signatures[] entries Verify will even hand
// to dp1-go. Every entry costs two JCS canonicalizations of the WHOLE
// document (payload_hash check + signing digest), and the unauthenticated
// LAN hub accepts a 4 MiB body with no entry cap — so an unbounded loop is
// hours of CPU per hostile cast, holding a command-gate slot the whole
// time. DP-1 defines six roles (curator, feed, agent, institution, licensor,
// publisher); 16 is comfortably above any honest chain. Past it the
// document is invalid with ReasonTooManySignatures and NO cryptography
// runs, so the cost of a hostile document is one envelope decode.
const MaxSignatures = 16

// signatureEnvelope is the minimal top-level projection Verify needs beyond
// what dp1-go decodes itself: the legacy string, and the entries in document
// order so Signers can be built.
type signatureEnvelope struct {
	Signature  string          `json:"signature"`
	Signatures json.RawMessage `json:"signatures"`
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
	var env signatureEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// Not a JSON object at all. Reported as invalid rather than unsigned
		// so a garbage document is never mistaken for an honest unsigned one.
		return Verdict{Status: StatusInvalid, Reason: ReasonMalformed}
	}
	legacy := env.Signature != ""

	// Count entries BEFORE dp1-go touches the document (see MaxSignatures).
	// A non-array here is left for dp1-go to classify as malformed below.
	var rawEntries []json.RawMessage
	if len(env.Signatures) > 0 && json.Unmarshal(env.Signatures, &rawEntries) == nil && len(rawEntries) > MaxSignatures {
		return Verdict{
			Status:        StatusInvalid,
			LegacyPresent: legacy,
			Reason:        fmt.Sprintf("%s (%d > %d)", ReasonTooManySignatures, len(rawEntries), MaxSignatures),
		}
	}

	ok, _, err := sign.VerifyPlaylistSignatures(raw)
	switch {
	case errors.Is(err, sign.ErrNoSignatures):
		reason := "unsigned"
		if legacy {
			reason = "unsigned; legacy signature ignored"
		}
		return Verdict{Status: StatusUnsigned, LegacyPresent: legacy, Reason: reason}
	case err != nil:
		// signatures is present but not an array of signature objects.
		return Verdict{Status: StatusInvalid, LegacyPresent: legacy, Reason: ReasonMalformed}
	}

	// dp1-go decoded this array a moment ago, so the projection cannot fail;
	// guarded anyway so a library change surfaces as invalid, never as a
	// nil deref.
	var entries []dp1playlist.Signature
	if uerr := json.Unmarshal(env.Signatures, &entries); uerr != nil || len(entries) == 0 {
		return Verdict{Status: StatusInvalid, LegacyPresent: legacy, Reason: ReasonMalformed}
	}

	signers := make([]Signer, 0, len(entries))
	firstFailure := ""
	oversized := false
	for _, e := range entries {
		s := Signer{Alg: e.Alg, Kid: e.Kid, Role: e.Role, OK: true}
		if s.Alg == "" || len(s.Alg) > MaxAlgLen || len(s.Kid) > MaxKidLen || len(s.Role) > MaxRoleLen {
			// Blank exactly the fields that overflowed so nothing
			// caster-sized is echoed, keep the rest for diagnosis.
			if len(s.Alg) > MaxAlgLen {
				s.Alg = ""
			}
			if len(s.Kid) > MaxKidLen {
				s.Kid = ""
			}
			if len(s.Role) > MaxRoleLen {
				s.Role = ""
			}
			s.OK = false
			s.Reason = ReasonMalformed
			oversized = true
			if firstFailure == "" {
				firstFailure = fmt.Sprintf("%s signature invalid: %s", roleOrUnknown(s.Role), s.Reason)
			}
			signers = append(signers, s)
			continue
		}
		if !ok {
			// Re-run per entry only on the failure path to label WHICH
			// entries failed and why: VerifyPlaylistSignatures returns the
			// failed structs but not their errors, and matching those back
			// by value is more fragile than one extra verify per entry.
			if verr := sign.VerifyMultiSignature(raw, e); verr != nil {
				s.OK = false
				s.Reason = classify(verr, e.Alg)
				if firstFailure == "" {
					firstFailure = fmt.Sprintf("%s signature invalid: %s", roleOrUnknown(e.Role), s.Reason)
				}
			}
		}
		signers = append(signers, s)
	}
	if ok && !oversized {
		return Verdict{Status: StatusValid, Signers: signers, LegacyPresent: legacy}
	}
	if firstFailure == "" {
		// dp1-go said not-ok but every per-entry re-check passed: cannot
		// happen with a deterministic verifier, but never report valid on
		// the library's word being contradicted.
		firstFailure = "signature invalid: " + ReasonMalformed
	}
	return Verdict{Status: StatusInvalid, Signers: signers, LegacyPresent: legacy, Reason: firstFailure}
}

// classify maps a dp1-go per-entry verification error onto the reason
// vocabulary. Order matters: ErrSigInvalid wraps the base64 and length
// failures too, and the payload-hash mismatch is a plain (unwrapped) error
// in dp1-go v0.6.0, hence the string match — pinned by the tamper test.
func classify(err error, alg string) string {
	switch {
	case errors.Is(err, sign.ErrUnsupportedAlg):
		return fmt.Sprintf(reasonUnsupportedAlgFmt, alg)
	case errors.Is(err, sign.ErrSigInvalid):
		return ReasonSignatureInvalid
	case strings.Contains(err.Error(), "payload_hash"):
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
