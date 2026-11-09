package control

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
)

// P-256 is the only curve in the v1 profile (docs/ff1-control-v1.md §2). Every
// key the protocol carries — device, controller, ephemeral — is one of these.
const (
	p256CoordLen  = 32
	p256ScalarLen = 32
)

var errBadJWK = errors.New("control: invalid P-256 JWK")

// PublicJWK is a P-256 public key in the exact wire shape of §2:
// {"kty":"EC","crv":"P-256","x":b64u(32),"y":b64u(32)}. A value of this type
// has been checked to be a point on the curve (not the identity), so any code
// holding one can hand it to ECDH or ECDSA without re-validating.
type PublicJWK struct {
	X, Y [p256CoordLen]byte
}

// uncompressed is the SEC 1 encoding 0x04||X||Y the stdlib constructors take.
func (k PublicJWK) uncompressed() []byte {
	b := make([]byte, 0, 1+2*p256CoordLen)
	b = append(b, 4)
	b = append(b, k.X[:]...)
	return append(b, k.Y[:]...)
}

// ECDH returns the key as an ECDH public key.
func (k PublicJWK) ECDH() (*ecdh.PublicKey, error) {
	return ecdh.P256().NewPublicKey(k.uncompressed())
}

// ECDSA returns the key as an ECDSA public key.
func (k PublicJWK) ECDSA() (*ecdsa.PublicKey, error) {
	return ecdsa.ParseUncompressedPublicKey(elliptic.P256(), k.uncompressed())
}

// Thumbprint is the RFC 7638 thumbprint of §2: b64u(SHA-256) over the
// members crv, kty, x, y in that (lexicographic) order with no whitespace.
func (k PublicJWK) Thumbprint() string {
	sum := sha256.Sum256(k.canonical())
	return b64u(sum[:])
}

// canonical is the RFC 7638 required-member serialization. It is also the
// wire form this package emits, so a JWK we write is byte-identical to the
// input of its own thumbprint.
func (k PublicJWK) canonical() []byte {
	return []byte(`{"crv":"P-256","kty":"EC","x":"` + b64u(k.X[:]) + `","y":"` + b64u(k.Y[:]) + `"}`)
}

// MarshalJSON emits the canonical member set.
func (k PublicJWK) MarshalJSON() ([]byte, error) { return k.canonical(), nil }

// UnmarshalJSON parses strictly; see ParsePublicJWK.
func (k *PublicJWK) UnmarshalJSON(b []byte) error {
	parsed, err := ParsePublicJWK(b)
	if err != nil {
		return err
	}
	*k = parsed
	return nil
}

// Equal reports whether two keys are the same point.
func (k PublicJWK) Equal(o PublicJWK) bool { return k.X == o.X && k.Y == o.Y }

// ParsePublicJWK parses a public JWK. It accepts EXACTLY the members kty, crv,
// x and y (any order) and rejects everything else: an extra member (a private
// "d" above all — a client that ships its private key has a bug we must not
// paper over), a duplicate member, a coordinate that is not exactly 32 bytes
// of unpadded base64url, and a point that is not on P-256.
func ParsePublicJWK(raw []byte) (PublicJWK, error) {
	m, err := strictObject(raw, []string{"kty", "crv", "x", "y"}, nil)
	if err != nil {
		return PublicJWK{}, fmt.Errorf("%w: %w", errBadJWK, err)
	}
	return publicFromMembers(m)
}

func publicFromMembers(m map[string]json.RawMessage) (PublicJWK, error) {
	if err := expectString(m, "kty", "EC"); err != nil {
		return PublicJWK{}, fmt.Errorf("%w: %w", errBadJWK, err)
	}
	if err := expectString(m, "crv", "P-256"); err != nil {
		return PublicJWK{}, fmt.Errorf("%w: %w", errBadJWK, err)
	}
	var k PublicJWK
	if err := fixedB64Member(m, "x", k.X[:]); err != nil {
		return PublicJWK{}, fmt.Errorf("%w: %w", errBadJWK, err)
	}
	if err := fixedB64Member(m, "y", k.Y[:]); err != nil {
		return PublicJWK{}, fmt.Errorf("%w: %w", errBadJWK, err)
	}
	// NewPublicKey implements SEC 1 §2.3.4: on-curve check, identity rejected.
	if _, err := k.ECDH(); err != nil {
		return PublicJWK{}, fmt.Errorf("%w: point not on curve: %w", errBadJWK, err)
	}
	return k, nil
}

// PublicJWKFromECDH converts a stdlib ECDH key.
func PublicJWKFromECDH(pub *ecdh.PublicKey) (PublicJWK, error) {
	return publicJWKFromUncompressed(pub.Bytes())
}

// PublicJWKFromECDSA converts a stdlib ECDSA key.
func PublicJWKFromECDSA(pub *ecdsa.PublicKey) (PublicJWK, error) {
	b, err := pub.Bytes()
	if err != nil {
		return PublicJWK{}, err
	}
	return publicJWKFromUncompressed(b)
}

func publicJWKFromUncompressed(b []byte) (PublicJWK, error) {
	if len(b) != 1+2*p256CoordLen || b[0] != 4 {
		return PublicJWK{}, errBadJWK
	}
	var k PublicJWK
	copy(k.X[:], b[1:1+p256CoordLen])
	copy(k.Y[:], b[1+p256CoordLen:])
	return k, nil
}

// privateJWK is the on-disk form of a device private key (§8 identity.json):
// the public members plus "d". It never leaves the device.
type privateJWK struct {
	Pub PublicJWK
	D   [p256ScalarLen]byte
}

func (k privateJWK) MarshalJSON() ([]byte, error) {
	return []byte(`{"crv":"P-256","d":"` + b64u(k.D[:]) + `","kty":"EC","x":"` + b64u(k.Pub.X[:]) + `","y":"` + b64u(k.Pub.Y[:]) + `"}`), nil
}

func (k *privateJWK) UnmarshalJSON(raw []byte) error {
	m, err := strictObject(raw, []string{"kty", "crv", "x", "y", "d"}, nil)
	if err != nil {
		return fmt.Errorf("%w: %w", errBadJWK, err)
	}
	pub, err := publicFromMembers(m)
	if err != nil {
		return err
	}
	var d [p256ScalarLen]byte
	if err := fixedB64Member(m, "d", d[:]); err != nil {
		return fmt.Errorf("%w: %w", errBadJWK, err)
	}
	// The scalar must reproduce the stored public point; a mismatch means
	// the file was edited or corrupted, and silently trusting either half
	// would sign or decrypt with a key the profile does not advertise.
	priv, err := ecdh.P256().NewPrivateKey(d[:])
	if err != nil {
		return fmt.Errorf("%w: %w", errBadJWK, err)
	}
	if !bytes.Equal(priv.PublicKey().Bytes(), pub.uncompressed()) {
		return fmt.Errorf("%w: d does not match x,y", errBadJWK)
	}
	k.Pub, k.D = pub, d
	return nil
}

func (k privateJWK) ecdh() (*ecdh.PrivateKey, error) { return ecdh.P256().NewPrivateKey(k.D[:]) }

func (k privateJWK) ecdsa() (*ecdsa.PrivateKey, error) {
	return ecdsa.ParseRawPrivateKey(elliptic.P256(), k.D[:])
}

func privateJWKFromECDH(priv *ecdh.PrivateKey) (privateJWK, error) {
	pub, err := PublicJWKFromECDH(priv.PublicKey())
	if err != nil {
		return privateJWK{}, err
	}
	var out privateJWK
	out.Pub = pub
	copy(out.D[:], priv.Bytes())
	return out, nil
}

func privateJWKFromECDSA(priv *ecdsa.PrivateKey) (privateJWK, error) {
	pub, err := PublicJWKFromECDSA(&priv.PublicKey)
	if err != nil {
		return privateJWK{}, err
	}
	d, err := priv.Bytes()
	if err != nil {
		return privateJWK{}, err
	}
	var out privateJWK
	out.Pub = pub
	copy(out.D[:], d)
	return out, nil
}
