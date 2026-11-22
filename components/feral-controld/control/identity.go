package control

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// DefaultDir is the control state directory (§8). It lives under .state so it
// rides the OTA carry-forward (ffos feral-system-update.sh) and is wiped by
// factory reset.
const DefaultDir = "/home/feralfile/.state/control"

const (
	identityFile = "identity.json"
	grantsFile   = "grants.json"
	genLen       = 16
)

// Identity is the device's control identity: devSig, devEnc and the gen they
// were created with (§2). It is immutable once loaded.
type Identity struct {
	deviceID  string
	gen       string
	createdAt int64
	sig       *ecdsa.PrivateKey
	enc       *ecdh.PrivateKey
	sigPub    PublicJWK
	encPub    PublicJWK
}

// Profile is the public device profile of §2.
type Profile struct {
	V   int       `json:"v"`
	Dev string    `json:"dev"`
	Gen string    `json:"gen"`
	Sig PublicJWK `json:"sig"`
	Enc PublicJWK `json:"enc"`
}

// Pins are the thumbprints a controller pins out of band (claim QR, §11).
type Pins struct {
	Sig string `json:"sig"`
	Enc string `json:"enc"`
}

// NewIdentityFromKeys builds an identity from existing keys. Production
// identities come from InitDir/New; this exists for test vectors and tools.
func NewIdentityFromKeys(deviceID, gen string, sig *ecdsa.PrivateKey, enc *ecdh.PrivateKey, createdAt int64) (*Identity, error) {
	if deviceID == "" {
		return nil, errors.New("control: empty deviceID")
	}
	if g, err := b64uDecode(gen); err != nil || len(g) != genLen {
		return nil, errors.New("control: gen must be b64u(16 bytes)")
	}
	sp, err := PublicJWKFromECDSA(&sig.PublicKey)
	if err != nil {
		return nil, err
	}
	ep, err := PublicJWKFromECDH(enc.PublicKey())
	if err != nil {
		return nil, err
	}
	return &Identity{deviceID: deviceID, gen: gen, createdAt: createdAt, sig: sig, enc: enc, sigPub: sp, encPub: ep}, nil
}

// newRandomIdentity creates fresh keys and a fresh gen. Key generation uses
// the stdlib's own CSPRNG (Go ignores a caller-supplied reader for ECDH key
// generation); r only supplies gen.
func newRandomIdentity(deviceID string, r io.Reader, now time.Time) (*Identity, error) {
	enc, err := ecdh.P256().GenerateKey(r)
	if err != nil {
		return nil, err
	}
	// Derive the ECDSA key from a separate fresh ECDH scalar: both are just a
	// uniformly random P-256 scalar, and this keeps one key-generation path.
	sigScalar, err := ecdh.P256().GenerateKey(r)
	if err != nil {
		return nil, err
	}
	sig, err := privateJWK{D: [32]byte(sigScalar.Bytes())}.ecdsa()
	if err != nil {
		return nil, err
	}
	g := make([]byte, genLen)
	if _, err := io.ReadFull(r, g); err != nil {
		return nil, err
	}
	return NewIdentityFromKeys(deviceID, b64u(g), sig, enc, now.Unix())
}

// DeviceID is the hostname the identity answers to.
func (id *Identity) DeviceID() string { return id.deviceID }

// Gen is the control generation.
func (id *Identity) Gen() string { return id.gen }

// Profile returns the public profile.
func (id *Identity) Profile() Profile {
	return Profile{V: 1, Dev: id.deviceID, Gen: id.gen, Sig: id.sigPub, Enc: id.encPub}
}

// Pins returns thumb(sig) and thumb(enc).
func (id *Identity) Pins() Pins {
	return Pins{Sig: id.sigPub.Thumbprint(), Enc: id.encPub.Thumbprint()}
}

// identityJSON is identity.json (§8). deviceId is not stored: it is the
// hostname, supplied at load, so renaming the host does not need a rewrite.
type identityJSON struct {
	V         int        `json:"v"`
	Gen       string     `json:"gen"`
	Sig       privateJWK `json:"sig"`
	Enc       privateJWK `json:"enc"`
	CreatedAt int64      `json:"createdAt"`
}

func (id *Identity) marshal() ([]byte, error) {
	sig, err := privateJWKFromECDSA(id.sig)
	if err != nil {
		return nil, err
	}
	enc, err := privateJWKFromECDH(id.enc)
	if err != nil {
		return nil, err
	}
	return json.Marshal(identityJSON{V: 1, Gen: id.gen, Sig: sig, Enc: enc, CreatedAt: id.createdAt})
}

func parseIdentity(raw []byte, deviceID string) (*Identity, error) {
	m, err := strictObject(raw, []string{"v", "gen", "sig", "enc", "createdAt"}, nil)
	if err != nil {
		return nil, fmt.Errorf("identity.json: %w", err)
	}
	if v, err := uintMember(m, "v"); err != nil || v != 1 {
		return nil, errors.New("identity.json: unsupported version")
	}
	if _, err := uintMember(m, "createdAt"); err != nil {
		return nil, fmt.Errorf("identity.json: %w", err)
	}
	var f identityJSON
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("identity.json: %w", err)
	}
	sig, err := f.Sig.ecdsa()
	if err != nil {
		return nil, fmt.Errorf("identity.json sig: %w", err)
	}
	enc, err := f.Enc.ecdh()
	if err != nil {
		return nil, fmt.Errorf("identity.json enc: %w", err)
	}
	return NewIdentityFromKeys(deviceID, f.Gen, sig, enc, f.CreatedAt)
}
