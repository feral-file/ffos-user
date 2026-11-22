package fixturegen

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"math/big"
	"strconv"
)

// The generator carries its own tiny JOSE writer for the NEGATIVE vectors
// (non-profile headers, DER signatures, foreign signers). Positive vectors
// are produced by the production code in package control, so the golden
// test pins exactly what the device emits.

var b64 = base64.RawURLEncoding

func b64u(b []byte) string { return b64.EncodeToString(b) }

func mustB64(s string) []byte {
	b, err := b64.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// derive returns 32 bytes bound to a label.
func derive(label string) []byte {
	sum := sha256.Sum256([]byte("ff1-control-v1/fixture/" + label))
	return sum[:]
}

// scalar returns a valid P-256 private scalar bound to label.
func scalar(label string) []byte {
	for i := 0; ; i++ {
		s := derive(label + "#" + strconv.Itoa(i))
		if _, err := ecdh.P256().NewPrivateKey(s); err == nil {
			return s
		}
	}
}

// PrivateJWK is a P-256 private key as stored in keys.json.
type PrivateJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	D   string `json:"d"`
}

func jwkFromScalar(d []byte) PrivateJWK {
	k, err := ecdh.P256().NewPrivateKey(d)
	if err != nil {
		panic(err)
	}
	pub := k.PublicKey().Bytes()
	return PrivateJWK{"EC", "P-256", b64u(pub[1:33]), b64u(pub[33:]), b64u(d)}
}

// ECDSA / ECDH convert a stored key back.
func (j PrivateJWK) ECDSA() *ecdsa.PrivateKey {
	k, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), mustB64(j.D))
	if err != nil {
		panic(err)
	}
	return k
}

func (j PrivateJWK) ECDH() *ecdh.PrivateKey {
	k, err := ecdh.P256().NewPrivateKey(mustB64(j.D))
	if err != nil {
		panic(err)
	}
	return k
}

// PublicJSON is the canonical public JWK (RFC 7638 member order).
func (j PrivateJWK) PublicJSON() string {
	return `{"crv":"P-256","kty":"EC","x":"` + j.X + `","y":"` + j.Y + `"}`
}

// Thumb is the RFC 7638 thumbprint.
func (j PrivateJWK) Thumb() string {
	s := sha256.Sum256([]byte(j.PublicJSON()))
	return b64u(s[:])
}

// signRaw is an ES256 compact JWS over an arbitrary header, r||s encoded
// unless der is set. Deterministic (RFC 6979).
func signRaw(priv *ecdsa.PrivateKey, header string, payload []byte, der bool) string {
	input := b64u([]byte(header)) + "." + b64u(payload)
	digest := sha256.Sum256([]byte(input))
	sig, err := priv.Sign(nil, digest[:], crypto.SHA256)
	if err != nil {
		panic(err)
	}
	if der {
		return input + "." + b64u(sig)
	}
	var rs struct{ R, S *big.Int }
	if _, err := asn1.Unmarshal(sig, &rs); err != nil {
		panic(err)
	}
	raw := make([]byte, 64)
	rs.R.FillBytes(raw[:32])
	rs.S.FillBytes(raw[32:])
	return input + "." + b64u(raw)
}

// concatKDF is RFC 7518 §4.6.2 for A256GCM with empty apu/apv.
func concatKDF(z []byte) []byte {
	h := sha256.New()
	var u [4]byte
	put := func(v uint32) { binary.BigEndian.PutUint32(u[:], v); h.Write(u[:]) }
	put(1)
	h.Write(z)
	put(7)
	h.Write([]byte("A256GCM"))
	put(0)
	put(0)
	put(256)
	return h.Sum(nil)
}

func gcmSeal(header string, key, iv, pt []byte) string {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	h := b64u([]byte(header))
	out := aead.Seal(nil, iv, pt, []byte(h))
	n := len(out) - 16
	return h + ".." + b64u(iv) + "." + b64u(out[:n]) + "." + b64u(out[n:])
}

// sealECDHES encrypts pt to devEnc with a header built around the epk JWK.
func sealECDHES(devEnc *ecdh.PublicKey, epk *ecdh.PrivateKey, iv []byte, header func(epkJSON string) string, pt []byte) string {
	z, err := epk.ECDH(devEnc)
	if err != nil {
		panic(err)
	}
	p := epk.PublicKey().Bytes()
	epkJSON := `{"crv":"P-256","kty":"EC","x":"` + b64u(p[1:33]) + `","y":"` + b64u(p[33:]) + `"}`
	return gcmSeal(header(epkJSON), concatKDF(z), iv, pt)
}
