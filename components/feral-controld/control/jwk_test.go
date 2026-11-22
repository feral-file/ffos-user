package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPublicJWKParse(t *testing.T) {
	k := pubOf(t, ecdsaKey(t, "jwk"))
	good := string(k.canonical())
	got, err := ParsePublicJWK([]byte(good))
	if err != nil || !got.Equal(k) {
		t.Fatalf("round trip: %v", err)
	}
	// Member order does not matter.
	reordered := `{"y":"` + b64u(k.Y[:]) + `","x":"` + b64u(k.X[:]) + `","kty":"EC","crv":"P-256"}`
	if _, err := ParsePublicJWK([]byte(reordered)); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"extra":       strings.Replace(good, `{`, `{"kid":"x",`, 1),
		"private d":   strings.Replace(good, `{`, `{"d":"`+b64u(make([]byte, 32))+`",`, 1),
		"dup x":       strings.Replace(good, `"x":`, `"x":"AA","x":`, 1),
		"P-384":       strings.Replace(good, "P-256", "P-384", 1),
		"kty RSA":     strings.Replace(good, `"EC"`, `"RSA"`, 1),
		"short x":     strings.Replace(good, b64u(k.X[:]), b64u(k.X[:31]), 1),
		"long y":      strings.Replace(good, b64u(k.Y[:]), b64u(append(k.Y[:], 0)), 1),
		"off curve":   strings.Replace(good, b64u(k.Y[:]), b64u(k.X[:]), 1),
		"zero point":  `{"crv":"P-256","kty":"EC","x":"` + b64u(make([]byte, 32)) + `","y":"` + b64u(make([]byte, 32)) + `"}`,
		"padded":      strings.Replace(good, b64u(k.X[:]), b64u(k.X[:])+"=", 1),
		"missing y":   `{"crv":"P-256","kty":"EC","x":"` + b64u(k.X[:]) + `"}`,
		"x is number": `{"crv":"P-256","kty":"EC","x":1,"y":"` + b64u(k.Y[:]) + `"}`,
	}
	for name, in := range bad {
		if in == good {
			t.Fatalf("%s: edit did not apply", name)
		}
		if _, err := ParsePublicJWK([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPublicJWKJSON(t *testing.T) {
	k := pubOf(t, ecdsaKey(t, "jwk"))
	b, _ := json.Marshal(struct{ K PublicJWK }{k})
	var back struct{ K PublicJWK }
	if err := json.Unmarshal(b, &back); err != nil || !back.K.Equal(k) {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"K":{"kty":"EC"}}`), &back); err == nil {
		t.Fatal("accepted partial JWK")
	}
	// The thumbprint input is exactly the canonical form (§2).
	if !strings.HasPrefix(string(k.canonical()), `{"crv":"P-256","kty":"EC","x":"`) {
		t.Fatal(string(k.canonical()))
	}
}

func TestPrivateJWK(t *testing.T) {
	sig := ecdsaKey(t, "p")
	pj, err := privateJWKFromECDSA(sig)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(pj)
	var back privateJWK
	if err := json.Unmarshal(b, &back); err != nil || back != pj {
		t.Fatalf("round trip: %v", err)
	}
	other, _ := privateJWKFromECDSA(ecdsaKey(t, "q"))
	mismatched := strings.Replace(string(b), b64u(pj.D[:]), b64u(other.D[:]), 1)
	if err := json.Unmarshal([]byte(mismatched), &back); err == nil {
		t.Fatal("accepted d that does not match x,y")
	}
}
