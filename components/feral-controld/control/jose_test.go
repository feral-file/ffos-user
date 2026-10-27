package control

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"fmt"
	"strings"
	"testing"

	jose "github.com/go-jose/go-jose/v4"
)

// go-jose is the independent implementation the hand-written codec is
// checked against, in both directions, for every profile.

func TestRequestJWEInteropWithGoJose(t *testing.T) {
	id := testIdentity(t)
	devEncECDSA, _ := ecdsa.ParseRawPrivateKey(elliptic.P256(), id.enc.Bytes())
	kid := id.encPub.Thumbprint()

	// Ours → go-jose.
	ours, err := SealRequest(id.encPub, ecdhKey(t, "epk1"), make([]byte, 12), "a.b.c") //nolint:gosec // G407: a fixed IV is the point of a test vector.
	if err != nil {
		t.Fatal(err)
	}
	obj, err := jose.ParseEncryptedCompact(ours, []jose.KeyAlgorithm{jose.ECDH_ES}, []jose.ContentEncryption{jose.A256GCM})
	if err != nil {
		t.Fatal(err)
	}
	pt, err := obj.Decrypt(devEncECDSA)
	if err != nil || string(pt) != "a.b.c" {
		t.Fatalf("go-jose decrypt: %v %q", err, pt)
	}
	if obj.Header.KeyID != kid {
		t.Fatalf("kid = %q", obj.Header.KeyID)
	}

	// go-jose → ours, with the profile's parameters.
	enc, err := jose.NewEncrypter(jose.A256GCM,
		jose.Recipient{Algorithm: jose.ECDH_ES, Key: &devEncECDSA.PublicKey, KeyID: kid},
		(&jose.EncrypterOptions{}).WithType(typReqJWE).WithContentType(typReqJWS))
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := enc.Encrypt([]byte("x.y.z"))
	if err != nil {
		t.Fatal(err)
	}
	compact, err := theirs.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	got, err := openRequest(compact, id.enc, kid)
	if err != nil || string(got) != "x.y.z" {
		t.Fatalf("openRequest(go-jose): %v %q", err, got)
	}

	// go-jose with an extra header member (its default without options
	// omits typ/cty) is refused by the exact-header rule.
	enc2, _ := jose.NewEncrypter(jose.A256GCM, jose.Recipient{Algorithm: jose.ECDH_ES, Key: &devEncECDSA.PublicKey, KeyID: kid}, nil)
	o2, _ := enc2.Encrypt([]byte("x"))
	c2, _ := o2.CompactSerialize()
	if _, err := openRequest(c2, id.enc, kid); err == nil {
		t.Fatal("accepted a header without typ/cty")
	}
}

func TestJWSInteropWithGoJose(t *testing.T) {
	key := ecdsaKey(t, "jws")
	pub := pubOf(t, key)
	payload := []byte(`{"a":1}`)

	ours, err := signJWS(key, jwsHeader(pub.Thumbprint(), typReqJWS), payload)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := jose.ParseSigned(ours, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := sig.Verify(&key.PublicKey); err != nil || string(got) != string(payload) {
		t.Fatalf("go-jose verify: %v", err)
	}
	// Deterministic (RFC 6979).
	again, _ := signJWS(key, jwsHeader(pub.Thumbprint(), typReqJWS), payload)
	if again != ours {
		t.Fatal("signJWS is not deterministic")
	}

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key},
		(&jose.SignerOptions{}).WithType(typReqJWS).WithHeader("kid", pub.Thumbprint()))
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	compact, _ := theirs.CompactSerialize()
	p, err := parseJWS(compact, typReqJWS)
	if err != nil {
		t.Fatal(err)
	}
	if !p.verify(&key.PublicKey) || string(p.payload) != string(payload) || p.kid != pub.Thumbprint() {
		t.Fatal("go-jose JWS did not verify")
	}
}

func TestDirJWEInteropWithGoJose(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	ours, err := sealJWE(replyJWEHeader, key, make([]byte, 12), []byte("p.q.r")) //nolint:gosec // G407: a fixed IV is the point of a test vector.
	if err != nil {
		t.Fatal(err)
	}
	obj, err := jose.ParseEncryptedCompact(ours, []jose.KeyAlgorithm{jose.DIRECT}, []jose.ContentEncryption{jose.A256GCM})
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := obj.Decrypt(key); err != nil || string(pt) != "p.q.r" {
		t.Fatalf("go-jose dir decrypt: %v", err)
	}

	enc, _ := jose.NewEncrypter(jose.A256GCM, jose.Recipient{Algorithm: jose.DIRECT, Key: key},
		(&jose.EncrypterOptions{}).WithType(typResJWE).WithContentType(typResJWS))
	theirs, _ := enc.Encrypt([]byte("s.t.u"))
	compact, _ := theirs.CompactSerialize()
	j, err := splitJWE(compact, map[string]string{"alg": algDir, "enc": encA256, "typ": typResJWE, "cty": typResJWS})
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := j.open(key); err != nil || string(pt) != "s.t.u" {
		t.Fatalf("open go-jose dir: %v", err)
	}

	// Notification header: kid instead of cty.
	nenc, _ := jose.NewEncrypter(jose.A256GCM, jose.Recipient{Algorithm: jose.DIRECT, Key: key, KeyID: "ctl"},
		(&jose.EncrypterOptions{}).WithType(typNtfJWE))
	nobj, _ := nenc.Encrypt([]byte(`{}`))
	nc, _ := nobj.CompactSerialize()
	if _, err := splitJWE(nc, map[string]string{"alg": algDir, "enc": encA256, "typ": typNtfJWE, "kid": "ctl"}); err != nil {
		t.Fatal(err)
	}
	ours, _ = sealJWE(notificationJWEHeader("ctl"), key, make([]byte, 12), []byte("n")) //nolint:gosec // G407: a fixed IV is the point of a test vector.
	nobj2, err := jose.ParseEncryptedCompact(ours, []jose.KeyAlgorithm{jose.DIRECT}, []jose.ContentEncryption{jose.A256GCM})
	if err != nil || nobj2.Header.KeyID != "ctl" {
		t.Fatalf("go-jose parse notification: %v", err)
	}
}

func TestThumbprintMatchesGoJose(t *testing.T) {
	for _, label := range []string{"a", "b", "c"} {
		k := ecdsaKey(t, label)
		want, err := (&jose.JSONWebKey{Key: &k.PublicKey}).Thumbprint(crypto.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		if got := pubOf(t, k).Thumbprint(); got != b64u(want) {
			t.Fatalf("thumbprint %s != go-jose %s", got, b64u(want))
		}
	}
}

func TestRequestHeaderExactness(t *testing.T) {
	id := testIdentity(t)
	kid := id.encPub.Thumbprint()
	epk := ecdhKey(t, "epk")
	epkPub := encPubOf(t, epk)
	z, _ := epk.ECDH(id.enc.PublicKey())
	cek := concatKDF(z)
	build := func(header string) string {
		out, err := sealJWE([]byte(header), cek, make([]byte, 12), []byte("a.b.c")) //nolint:gosec // G407: a fixed IV is the point of a test vector.
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	good := string(requestJWEHeader(kid, epkPub))
	if _, err := openRequest(build(good), id.enc, kid); err != nil {
		t.Fatalf("good header refused: %v", err)
	}
	epkJSON := string(epkPub.canonical())
	cases := map[string]string{
		"extra member":   strings.Replace(good, `{"alg"`, `{"zip":"DEF","alg"`, 1),
		"duplicate alg":  strings.Replace(good, `{"alg":"ECDH-ES"`, `{"alg":"ECDH-ES","alg":"ECDH-ES"`, 1),
		"alg RSA":        strings.Replace(good, `"ECDH-ES"`, `"RSA-OAEP-256"`, 1),
		"alg ECDH-ES+KW": strings.Replace(good, `"ECDH-ES"`, `"ECDH-ES+A256KW"`, 1),
		"enc A128GCM":    strings.Replace(good, `"A256GCM"`, `"A128GCM"`, 1),
		"wrong typ":      strings.Replace(good, typReqJWE, "JWE", 1),
		"no cty":         strings.Replace(good, `,"cty":"ff1-req+jws"`, ``, 1),
		"wrong kid":      strings.Replace(good, kid, testIdentity(t).sigPub.Thumbprint(), 1),
		"apu":            strings.Replace(good, `{"alg"`, `{"apu":"","alg"`, 1),
		"epk extra":      strings.Replace(good, epkJSON, strings.Replace(epkJSON, `{`, `{"use":"enc",`, 1), 1),
		"epk off curve":  strings.Replace(good, b64u(epkPub.Y[:]), b64u(make([]byte, 32)), 1),
		"epk with d":     strings.Replace(good, epkJSON, strings.Replace(epkJSON, `{`, `{"d":"`+b64u(epk.Bytes())+`",`, 1), 1),
		"kid number":     strings.Replace(good, `"kid":"`+kid+`"`, `"kid":1`, 1),
		"not object":     `[]`,
	}
	for name, h := range cases {
		if h == good {
			t.Fatalf("%s: edit did not apply", name)
		}
		if _, err := openRequest(build(h), id.enc, kid); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCompactStructure(t *testing.T) {
	id := testIdentity(t)
	kid := id.encPub.Thumbprint()
	good, _ := SealRequest(id.encPub, ecdhKey(t, "e"), make([]byte, 12), "a.b.c") //nolint:gosec // G407: a fixed IV is the point of a test vector.
	parts := strings.Split(good, ".")
	join := func(p []string) string { return strings.Join(p, ".") }
	mod := func(i int, v string) string { q := append([]string(nil), parts...); q[i] = v; return join(q) }
	cases := map[string]string{
		"four parts":     join(parts[:4]),
		"six parts":      good + ".",
		"encrypted key":  mod(1, "AAAA"),
		"short iv":       mod(2, b64u(make([]byte, 8))),
		"short tag":      mod(4, b64u(make([]byte, 8))),
		"padded iv":      mod(2, parts[2]+"="),
		"newline in ct":  mod(3, parts[3][:2]+"\n"+parts[3][2:]),
		"flipped ct":     mod(3, flip(parts[3])),
		"flipped header": mod(0, flip(parts[0])),
		"huge header":    mod(0, strings.Repeat("A", MaxHeaderBytes+1)),
	}
	for name, c := range cases {
		if _, err := openRequest(c, id.enc, kid); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// flip changes the first base64url character to another valid one.
func flip(s string) string {
	if s[0] == 'A' {
		return "B" + s[1:]
	}
	return "A" + s[1:]
}

func TestJWSStrictness(t *testing.T) {
	key := ecdsaKey(t, "k")
	kid := pubOf(t, key).Thumbprint()
	good, _ := signJWS(key, jwsHeader(kid, typReqJWS), []byte(`{}`))
	if p, err := parseJWS(good, typReqJWS); err != nil || !p.verify(&key.PublicKey) {
		t.Fatal("good JWS refused")
	}
	headers := map[string]string{
		"alg none":   `{"alg":"none","kid":"` + kid + `","typ":"ff1-req+jws"}`,
		"alg RS256":  `{"alg":"RS256","kid":"` + kid + `","typ":"ff1-req+jws"}`,
		"alg HS256":  `{"alg":"HS256","kid":"` + kid + `","typ":"ff1-req+jws"}`,
		"extra":      `{"alg":"ES256","kid":"` + kid + `","typ":"ff1-req+jws","jwk":{}}`,
		"crit":       `{"alg":"ES256","kid":"` + kid + `","typ":"ff1-req+jws","crit":["b64"]}`,
		"reply typ":  `{"alg":"ES256","kid":"` + kid + `","typ":"ff1-res+jws"}`,
		"short kid":  `{"alg":"ES256","kid":"abc","typ":"ff1-req+jws"}`,
		"dup typ":    `{"alg":"ES256","kid":"` + kid + `","typ":"ff1-req+jws","typ":"ff1-req+jws"}`,
		"escape dup": `{"alg":"ES256","kid":"` + kid + `","typ":"ff1-req+jws","\u0074yp":"ff1-req+jws"}`,
	}
	for name, h := range headers {
		c, _ := signJWS(key, []byte(h), []byte(`{}`))
		if _, err := parseJWS(c, typReqJWS); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Signature shape is judged by verify: DER, truncated, high bits.
	der := toDER(t, good)
	if p, err := parseJWS(der, typReqJWS); err != nil || p.verify(&key.PublicKey) {
		t.Fatal("DER signature verified")
	}
	parts := strings.Split(good, ".")
	if p, _ := parseJWS(parts[0]+"."+parts[1]+"."+b64u(make([]byte, 64)), typReqJWS); p.verify(&key.PublicKey) {
		t.Fatal("zero signature verified")
	}
	if _, err := parseJWS(good+".x", typReqJWS); err == nil {
		t.Fatal("four-part JWS accepted")
	}
}

func TestConcatKDFKnownAnswer(t *testing.T) {
	// RFC 7518 Appendix C uses A128GCM with apu/apv; the profile pins
	// A256GCM with empty apu/apv, so the cross-check is go-jose (above) plus
	// this pin, computed independently (Python hashlib over the RFC 7518
	// §4.6.2 byte layout) when the test was written.
	const concatKDFZeroPin = "ohtGU230iroWUJBOQYR7JT-iHsoYc-MLxzoRXrqr6Yw"
	z := make([]byte, 32)
	got := b64u(concatKDF(z))
	if got != concatKDFZeroPin {
		t.Fatalf("concatKDF(0^32) = %s", got)
	}
}

func TestOpenReplyRejectsUnboundClaims(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	rc := d.cmdClaims(owner, "getDeviceStatus", `{}`)
	adm := d.expectExecute(owner, rc)
	out, _ := adm.Reply(true, "", map[string]any{})
	rk, _ := b64uDecode(rc.RK)
	r, err := OpenReply(out, rk, d.id.sigPub)
	if err != nil {
		t.Fatal(err)
	}
	if r.Matches(testDeviceID, d.id.gen, rc.Ctl, "other-rid") || r.Matches("FF1-X", d.id.gen, rc.Ctl, rc.Rid) {
		t.Fatal("Matches ignored a binding field")
	}
	// Claims shape rules.
	for _, bad := range []string{
		`{"v":1,"dev":"d","gen":"g","ctl":"c","rid":"r","iat":1,"ok":true,"code":"x"}`,
		`{"v":1,"dev":"d","gen":"g","ctl":"c","rid":"r","iat":1,"ok":false}`,
		`{"v":1,"dev":"d","gen":"g","ctl":"c","rid":"r","iat":1,"ok":"yes"}`,
		`{"v":1,"dev":"d","gen":"g","ctl":"c","rid":"r","iat":1,"ok":true,"extra":1}`,
	} {
		if _, err := parseReplyClaims([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestNotificationEpStrict(t *testing.T) {
	base := `{"v":1,"dev":"d","gen":"g","ctl":"c","ep":"%s","seq":1,"iat":1,"type":"t","body":{}}`
	for ep, ok := range map[string]bool{
		"AAAAAAAAAAA": true, "AAAA": false, "AAAAAAAAAAA=": false, "": false, "AAAAAAAAAAAAAAA": false, "AAAAAAAAAA*": false,
	} {
		_, err := parseNotificationClaims([]byte(fmt.Sprintf(base, ep)))
		if (err == nil) != ok {
			t.Errorf("ep %q: err=%v want ok=%v", ep, err, ok)
		}
	}
}
