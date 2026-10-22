package control_test

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/control/internal/fixturegen"
)

func readVector(t *testing.T, sub, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, sub, name+".json")) //nolint:gosec // G304: fixture dir.
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// checkControllerQR runs controller-qr.json (§5.6.1).
func checkControllerQR(t *testing.T, sub, name string) {
	var v fixturegen.ControllerQRVectors
	readVector(t, sub, name, &v)
	if len(v.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range v.Cases {
		q, err := control.ParseControllerQR(c.QR)
		if sub == "negative" {
			if err == nil {
				t.Errorf("%s: accepted", c.Description)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.Description, err)
		}
		sig, _ := json.Marshal(q.Sig)
		enc, _ := json.Marshal(q.Enc)
		if q.ChannelID != c.ChannelID || q.Ctl() != c.Ctl || string(sig) != c.Sig || string(enc) != c.Enc ||
			q.Platform != c.Platform || q.Label != c.Label || q.String() != c.QR {
			t.Fatalf("%s: parsed %+v", c.Description, q)
		}
	}
}

// checkRegistration runs a registration vector (§5.6.2-3). A positive one is
// also decrypted from the spec text with stdlib primitives, independently of
// OpenRegistration, and its wire is reproduced byte for byte.
func checkRegistration(t *testing.T, sub, name string) {
	var v fixturegen.RegistrationVector
	readVector(t, sub, name, &v)
	q, err := control.ParseControllerQR(v.QR)
	if err != nil {
		t.Fatal(err)
	}
	minter := v.MinterKey.ECDH()
	res, err := control.OpenRegistration(q, minter, []byte(v.Wire), v.WantDev, v.Now)
	if sub == "negative" {
		if err == nil {
			t.Fatal("controller accepted a registration result it must reject")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if res.Ctl != q.Ctl() || res.ChannelID != v.ChannelID {
		t.Fatalf("result %+v", res)
	}
	wire, err := control.SealRegistrationBytes(q, []byte(v.Plaintext), v.OwnerEphemeralKey.ECDH(), b64(t, v.IV))
	if err != nil || string(wire) != v.Wire {
		t.Fatalf("wire not reproduced: %v", err)
	}
	var w struct{ EPK, IV, CT string }
	if err := json.Unmarshal([]byte(v.Wire), &w); err != nil {
		t.Fatal(err)
	}
	epk, err := ecdh.P256().NewPublicKey(b64(t, w.EPK))
	if err != nil {
		t.Fatal(err)
	}
	z, err := minter.ECDH(epk)
	if err != nil {
		t.Fatal(err)
	}
	minterB64 := base64.RawURLEncoding.EncodeToString(minter.PublicKey().Bytes())
	salt := sha256.Sum256([]byte("ff1-ctl:1|" + v.ChannelID))
	key, err := hkdf.Key(sha256.New, z, salt[:], "ff1-controller-registration/v1", 32)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	aad := []byte("ff1-controller-registration/v1|" + v.ChannelID + "|" + minterB64 + "|" + w.EPK)
	pt, err := gcm.Open(nil, b64(t, w.IV), b64(t, w.CT), aad)
	if err != nil || string(pt) != v.Plaintext {
		t.Fatalf("independent decrypt: %v", err)
	}
}
