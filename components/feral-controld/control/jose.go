package control

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// This file is a deliberately minimal JOSE: exactly the four object profiles
// of docs/ff1-control-v1.md (request JWE §3.1, request/reply JWS §3.2/§4.1,
// reply JWE §4.1, sealed notification JWE §9) and nothing else. There is no
// algorithm negotiation: every header must be EXACTLY its profile's member set
// with its profile's values, so "alg":"none", an RSA alg, a "zip", a "jku" or
// a "crit" is not an unsupported option but a malformed object.
//
// go-jose v4 is used only in the tests, as an independent implementation the
// output of this file is checked against in both directions.

const (
	// MaxCompactBytes bounds a whole compact object before any decoding. It is
	// above the largest legitimate request (an inline DP-1 playlist) and is the
	// cap a transport adapter should apply before buffering a body.
	MaxCompactBytes = 4 << 20
	// MaxHeaderBytes bounds the base64url protected-header segment.
	MaxHeaderBytes = 4 << 10

	algECDHES = "ECDH-ES"
	algDir    = "dir"
	encA256   = "A256GCM"
	algES256  = "ES256"

	typReqJWE = "ff1-req+jwe"
	typReqJWS = "ff1-req+jws"
	typResJWE = "ff1-res+jwe"
	typResJWS = "ff1-res+jws"
	typNtfJWE = "ff1-ntf+jwe"

	gcmIVLen   = 12
	gcmTagLen  = 16
	cekLen     = 32
	es256SigLn = 64
	// thumbLen is len(b64u(32 bytes)): every kid in the protocol is a thumbprint.
	thumbLen = 43
)

// errMalformed wraps every structural/cryptographic failure in this file.
var errMalformed = errors.New("control: malformed JOSE object")

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errMalformed, fmt.Sprintf(format, args...))
}

// parseHeader decodes and checks a protected-header segment: size, strict
// JSON, exact member set (the keys of want plus extra), and the exact string
// value of each member in want. An empty want value means "any string" (the
// caller compares it). Members in extra are returned for the caller to parse.
func parseHeader(seg string, want map[string]string, extra ...string) (map[string]json.RawMessage, error) {
	if len(seg) == 0 || len(seg) > MaxHeaderBytes {
		return nil, malformed("protected header size %d", len(seg))
	}
	raw, err := b64uDecode(seg)
	if err != nil {
		return nil, malformed("protected header encoding: %v", err)
	}
	required := make([]string, 0, len(want)+len(extra))
	for k := range want {
		required = append(required, k)
	}
	required = append(required, extra...)
	m, err := strictObject(raw, required, nil)
	if err != nil {
		return nil, malformed("protected header: %v", err)
	}
	for k, v := range want {
		got, err := stringMember(m, k)
		if err != nil {
			return nil, malformed("protected header: %v", err)
		}
		if v != "" && got != v {
			return nil, malformed("protected header %q = %q, want %q", k, got, v)
		}
	}
	return m, nil
}

type compactJWE struct {
	headerSeg string
	header    map[string]json.RawMessage
	iv        []byte
	sealed    []byte // ciphertext || tag, the shape cipher.AEAD.Open takes
}

// splitJWE parses the five-part compact form. The encrypted-key part must be
// empty in both JWE profiles (ECDH-ES direct, dir).
func splitJWE(compact string, want map[string]string, extra ...string) (compactJWE, error) {
	if len(compact) > MaxCompactBytes {
		return compactJWE{}, malformed("compact JWE is %d bytes", len(compact))
	}
	parts := strings.Split(compact, ".")
	if len(parts) != 5 {
		return compactJWE{}, malformed("compact JWE has %d parts", len(parts))
	}
	if parts[1] != "" {
		return compactJWE{}, malformed("JWE encrypted key must be empty")
	}
	h, err := parseHeader(parts[0], want, extra...)
	if err != nil {
		return compactJWE{}, err
	}
	iv, err := b64uDecode(parts[2])
	if err != nil || len(iv) != gcmIVLen {
		return compactJWE{}, malformed("JWE IV")
	}
	ct, err := b64uDecode(parts[3])
	if err != nil {
		return compactJWE{}, malformed("JWE ciphertext encoding")
	}
	tag, err := b64uDecode(parts[4])
	if err != nil || len(tag) != gcmTagLen {
		return compactJWE{}, malformed("JWE tag")
	}
	return compactJWE{headerSeg: parts[0], header: h, iv: iv, sealed: append(ct, tag...)}, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != cekLen {
		return nil, malformed("content key is %d bytes", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealJWE encrypts pt under key with the given IV. AAD is the ASCII of the
// base64url protected header (RFC 7516 §5.1 step 14); compact form has no
// encrypted key.
func sealJWE(header []byte, key, iv, pt []byte) (string, error) {
	if len(iv) != gcmIVLen {
		return "", malformed("IV is %d bytes", len(iv))
	}
	aead, err := newGCM(key)
	if err != nil {
		return "", err
	}
	hseg := b64u(header)
	sealed := aead.Seal(nil, iv, pt, []byte(hseg))
	ct, tag := sealed[:len(sealed)-gcmTagLen], sealed[len(sealed)-gcmTagLen:]
	return hseg + ".." + b64u(iv) + "." + b64u(ct) + "." + b64u(tag), nil
}

func (j compactJWE) open(key []byte) ([]byte, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	pt, err := aead.Open(nil, j.iv, j.sealed, []byte(j.headerSeg))
	if err != nil {
		return nil, malformed("JWE authentication failed")
	}
	return pt, nil
}

// concatKDF derives the ECDH-ES direct CEK (RFC 7518 §4.6.2, NIST SP 800-56A
// Concat KDF): one SHA-256 round over
//
//	counter(1) || Z || len||"A256GCM" || len||apu("") || len||apv("") || keydatalen(256)
//
// keydatalen equals the hash length, so a single round is the whole output.
func concatKDF(z []byte) []byte {
	h := sha256.New()
	var u32 [4]byte
	binary.BigEndian.PutUint32(u32[:], 1)
	h.Write(u32[:])
	h.Write(z)
	binary.BigEndian.PutUint32(u32[:], uint32(len(encA256)))
	h.Write(u32[:])
	h.Write([]byte(encA256))
	binary.BigEndian.PutUint32(u32[:], 0) // PartyUInfo: empty
	h.Write(u32[:])
	h.Write(u32[:]) // PartyVInfo: empty
	binary.BigEndian.PutUint32(u32[:], cekLen*8)
	h.Write(u32[:]) // SuppPubInfo
	return h.Sum(nil)
}

// requestJWEHeader is the §3.1 header in its canonical member order.
func requestJWEHeader(kid string, epk PublicJWK) []byte {
	return []byte(`{"alg":"ECDH-ES","enc":"A256GCM","kid":` + jsonString(kid) +
		`,"typ":"ff1-req+jwe","cty":"ff1-req+jws","epk":` + string(epk.canonical()) + `}`)
}

// SealRequest builds a §3.1 request JWE around a compact JWS. epk is the
// sender's fresh ephemeral key and iv a fresh 96-bit IV; both are parameters
// (not drawn here) so test vectors can fix them. Controllers MUST use a new
// epk and IV for every request.
func SealRequest(devEnc PublicJWK, epk *ecdh.PrivateKey, iv []byte, jws string) (string, error) {
	pub, err := devEnc.ECDH()
	if err != nil {
		return "", err
	}
	z, err := epk.ECDH(pub)
	if err != nil {
		return "", err
	}
	epkPub, err := PublicJWKFromECDH(epk.PublicKey())
	if err != nil {
		return "", err
	}
	return sealJWE(requestJWEHeader(devEnc.Thumbprint(), epkPub), concatKDF(z), iv, []byte(jws))
}

// openRequest is §3.4 step 1: exact header, kid == thumb(devEnc), a valid
// ephemeral point, and an authentic ciphertext. It returns the inner JWS.
func openRequest(compact string, devEnc *ecdh.PrivateKey, devEncKid string) ([]byte, error) {
	j, err := splitJWE(compact, map[string]string{
		"alg": algECDHES, "enc": encA256, "kid": devEncKid, "typ": typReqJWE, "cty": typReqJWS,
	}, "epk")
	if err != nil {
		return nil, err
	}
	epk, err := ParsePublicJWK(j.header["epk"])
	if err != nil {
		return nil, malformed("epk: %v", err)
	}
	epkPub, err := epk.ECDH()
	if err != nil {
		return nil, malformed("epk: %v", err)
	}
	z, err := devEnc.ECDH(epkPub)
	if err != nil {
		return nil, malformed("ECDH: %v", err)
	}
	return j.open(concatKDF(z))
}

// replyJWEHeader / notificationJWEHeader are the §4.1 and §9 headers.
var replyJWEHeader = []byte(`{"alg":"dir","enc":"A256GCM","typ":"ff1-res+jwe","cty":"ff1-res+jws"}`)

func notificationJWEHeader(ctl string) []byte {
	return []byte(`{"alg":"dir","enc":"A256GCM","typ":"ff1-ntf+jwe","kid":` + jsonString(ctl) + `}`)
}

// OpenReply is the controller's side of §4.1-4.2: decrypt with the request's
// rk, verify the inner JWS against the PINNED devSig, and return the claims.
// It does not compare dev/gen/ctl/rid; the caller must (see ReplyClaims).
func OpenReply(compact string, rk []byte, devSig PublicJWK) (ReplyClaims, error) {
	j, err := splitJWE(compact, map[string]string{"alg": algDir, "enc": encA256, "typ": typResJWE, "cty": typResJWS})
	if err != nil {
		return ReplyClaims{}, err
	}
	jwsBytes, err := j.open(rk)
	if err != nil {
		return ReplyClaims{}, err
	}
	p, err := parseJWS(string(jwsBytes), typResJWS)
	if err != nil {
		return ReplyClaims{}, err
	}
	if p.kid != devSig.Thumbprint() {
		return ReplyClaims{}, malformed("reply kid is not the pinned devSig")
	}
	pub, err := devSig.ECDSA()
	if err != nil {
		return ReplyClaims{}, err
	}
	if !p.verify(pub) {
		return ReplyClaims{}, malformed("reply signature")
	}
	return parseReplyClaims(p.payload)
}

// OpenNotification is the controller's side of §9.
func OpenNotification(compact, ctl string, nk []byte) (NotificationClaims, error) {
	j, err := splitJWE(compact, map[string]string{"alg": algDir, "enc": encA256, "typ": typNtfJWE, "kid": ctl})
	if err != nil {
		return NotificationClaims{}, err
	}
	pt, err := j.open(nk)
	if err != nil {
		return NotificationClaims{}, err
	}
	n, err := parseNotificationClaims(pt)
	if err != nil {
		return n, err
	}
	if n.Ctl != ctl {
		return NotificationClaims{}, malformed("notification ctl does not match kid")
	}
	return n, nil
}

// jwsHeader is the §3.2/§4.1 header in canonical member order.
func jwsHeader(kid, typ string) []byte {
	return []byte(`{"alg":"ES256","kid":` + jsonString(kid) + `,"typ":` + jsonString(typ) + `}`)
}

// signJWS produces an ES256 compact JWS with a raw r||s signature (RFC 7518
// §3.4). Signing is deterministic (RFC 6979, the stdlib's nil-rand mode): it
// needs no randomness source and makes test vectors byte-reproducible.
// Verifiers MUST NOT depend on determinism; randomized ES256 is equally valid.
func signJWS(priv *ecdsa.PrivateKey, header, payload []byte) (string, error) {
	input := b64u(header) + "." + b64u(payload)
	digest := sha256.Sum256([]byte(input))
	der, err := priv.Sign(nil, digest[:], crypto.SHA256)
	if err != nil {
		return "", err
	}
	var rs struct{ R, S *big.Int }
	if rest, err := asn1.Unmarshal(der, &rs); err != nil || len(rest) != 0 {
		return "", fmt.Errorf("control: decoding stdlib signature: %w", err)
	}
	sig := make([]byte, es256SigLn)
	rs.R.FillBytes(sig[:32])
	rs.S.FillBytes(sig[32:])
	return input + "." + b64u(sig), nil
}

// SignRequest builds a §3.2 request JWS over already-encoded claims.
func SignRequest(ctlSig *ecdsa.PrivateKey, claims []byte) (string, error) {
	pub, err := PublicJWKFromECDSA(&ctlSig.PublicKey)
	if err != nil {
		return "", err
	}
	return signJWS(ctlSig, jwsHeader(pub.Thumbprint(), typReqJWS), claims)
}

type parsedJWS struct {
	kid     string
	payload []byte
	input   string // b64u(header) "." b64u(payload): the signed bytes
	sig     []byte
}

// parseJWS checks the three-part form, the exact header and a kid that is a
// thumbprint-shaped string.
func parseJWS(compact, typ string) (parsedJWS, error) {
	if len(compact) > MaxCompactBytes {
		return parsedJWS{}, malformed("compact JWS is %d bytes", len(compact))
	}
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		return parsedJWS{}, malformed("compact JWS has %d parts", len(parts))
	}
	h, err := parseHeader(parts[0], map[string]string{"alg": algES256, "kid": "", "typ": typ})
	if err != nil {
		return parsedJWS{}, err
	}
	kid, _ := stringMember(h, "kid")
	if !isThumbprint(kid) {
		return parsedJWS{}, malformed("JWS kid is not a thumbprint")
	}
	payload, err := b64uDecode(parts[1])
	if err != nil {
		return parsedJWS{}, malformed("JWS payload encoding")
	}
	// The signature's LENGTH is judged by verify, not here: a DER or
	// truncated signature on a well-formed JWS is a signature failure
	// (bad_signature), which §3.4 reports after the key lookup.
	sig, err := b64uDecode(parts[2])
	if err != nil {
		return parsedJWS{}, malformed("JWS signature encoding")
	}
	return parsedJWS{kid: kid, payload: payload, input: parts[0] + "." + parts[1], sig: sig}, nil
}

func (p parsedJWS) verify(pub *ecdsa.PublicKey) bool {
	if len(p.sig) != es256SigLn {
		return false // DER, truncated or padded: never r||s
	}
	digest := sha256.Sum256([]byte(p.input))
	r := new(big.Int).SetBytes(p.sig[:32])
	s := new(big.Int).SetBytes(p.sig[32:])
	// ecdsa.Verify rejects r or s outside [1, n-1]. High-S is accepted:
	// ES256 does not mandate low-S and WebCrypto does not normalize.
	return ecdsa.Verify(pub, digest[:], r, s)
}

func isThumbprint(s string) bool {
	if len(s) != thumbLen {
		return false
	}
	b, err := b64uDecode(s)
	return err == nil && len(b) == sha256.Size
}

// jsonString encodes s as a JSON string literal. encoding/json escapes <, >
// and & as < etc.; that is still valid JSON and only ever applied to
// values this package already validated as base64url, so it never triggers.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
