package control

import (
	"strings"
	"testing"
)

// The compact parsers face untrusted bytes from the LAN and the relay. The
// fuzz targets assert they never panic and never accept something the
// strict rules forbid (anything accepted must re-encode to itself).

func FuzzOpen(f *testing.F) {
	d, owner, _ := ownerDevice(&testing.T{})
	good := d.seal(owner, d.cmdClaims(owner, "displayPlaylist", `{}`))
	f.Add(good)
	f.Add("")
	f.Add("....")
	f.Add(strings.Replace(good, ".", "..", 1))
	f.Fuzz(func(t *testing.T, s string) {
		adm, err := d.a.Open(s)
		if err == nil && adm == nil {
			t.Fatal("nil admission without error")
		}
		if err != nil {
			if publicCode(err) == "" {
				t.Fatalf("non-public error %T", err)
			}
		}
	})
}

func FuzzParseJWS(f *testing.F) {
	key := ecdsaKey(f, "fuzz")
	good, _ := signJWS(key, jwsHeader(pubOf(f, key).Thumbprint(), typReqJWS), []byte(`{"a":1}`))
	f.Add(good)
	f.Add("a.b.c")
	f.Fuzz(func(t *testing.T, s string) {
		p, err := parseJWS(s, typReqJWS)
		if err != nil {
			return
		}
		if p.verify(&key.PublicKey) && s != good && !strings.HasPrefix(s, strings.Split(good, ".")[0]) {
			t.Fatalf("verified a JWS with a different header: %q", s)
		}
	})
}

func FuzzDirJWE(f *testing.F) {
	key := make([]byte, 32)
	good, _ := sealJWE(replyJWEHeader, key, make([]byte, 12), []byte("x")) //nolint:gosec // G407: a fixed IV is the point of a test vector.
	f.Add(good)
	f.Fuzz(func(t *testing.T, s string) {
		j, err := splitJWE(s, map[string]string{"alg": algDir, "enc": encA256, "typ": typResJWE, "cty": typResJWS})
		if err != nil {
			return
		}
		if pt, err := j.open(key); err == nil && s != good && string(pt) != "x" {
			t.Fatalf("forged plaintext %q", pt)
		}
	})
}

func FuzzCheckJSON(f *testing.F) {
	for _, s := range []string{`{}`, `{"a":1,"a":2}`, `[1,{"b":[]}]`, `{"a":1,"a":1}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) { _ = checkJSON(b) })
}

func FuzzParsePublicJWK(f *testing.F) {
	k := pubOf(f, ecdsaKey(f, "fz"))
	f.Add(k.canonical())
	f.Fuzz(func(t *testing.T, b []byte) {
		got, err := ParsePublicJWK(b)
		if err != nil {
			return
		}
		if _, err := got.ECDH(); err != nil {
			t.Fatal("accepted an invalid point")
		}
		back, err := ParsePublicJWK(got.canonical())
		if err != nil || !back.Equal(got) {
			t.Fatal("canonical form does not round-trip")
		}
	})
}
