package control

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/json"
	"errors"
	"math/big"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testDeviceID = "FF1-TESTUNIT"

// scalarFor derives a deterministic P-256 private scalar from a label.
func scalarFor(t testing.TB, label string) []byte {
	t.Helper()
	for i := 0; ; i++ {
		sum := sha256.Sum256([]byte(label + string(rune('a'+i))))
		if _, err := ecdh.P256().NewPrivateKey(sum[:]); err == nil {
			return sum[:]
		}
	}
}

func ecdhKey(t testing.TB, label string) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.P256().NewPrivateKey(scalarFor(t, label))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func ecdsaKey(t testing.TB, label string) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), scalarFor(t, label))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pubOf(t testing.TB, k *ecdsa.PrivateKey) PublicJWK {
	t.Helper()
	p, err := PublicJWKFromECDSA(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func encPubOf(t testing.TB, k *ecdh.PrivateKey) PublicJWK {
	t.Helper()
	p, err := PublicJWKFromECDH(k.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type testCtl struct {
	label  string
	sig    *ecdsa.PrivateKey
	enc    *ecdh.PrivateKey
	sigPub PublicJWK
	encPub PublicJWK
	ctl    string
	ctr    uint64
}

func newTestCtl(t testing.TB, label string) *testCtl {
	t.Helper()
	c := &testCtl{label: label, sig: ecdsaKey(t, "ctl-sig/"+label), enc: ecdhKey(t, "ctl-enc/"+label)}
	c.sigPub, c.encPub = pubOf(t, c.sig), encPubOf(t, c.enc)
	c.ctl = c.sigPub.Thumbprint()
	return c
}

func (c *testCtl) grant(role Role) Grant {
	var nk [32]byte
	copy(nk[:], []byte("nk-"+c.label+"-0123456789abcdefghijklmnop"))
	return Grant{Ctl: c.ctl, Sig: c.sigPub, Enc: c.encPub, Role: role, Label: c.label, Platform: "cli",
		CreatedAt: 1, ApprovedBy: approvedByInvitation, NK: nk}
}

func (c *testCtl) enroll(inv string) *Enroll {
	return &Enroll{Sig: c.sigPub, Enc: c.encPub, Label: c.label, Platform: "ios", Inv: inv}
}

type testDevice struct {
	t   testing.TB
	a   *Authority
	fs  *MemFS
	id  *Identity
	now atomic.Int64
	dir string
}

func testIdentity(t testing.TB) *Identity {
	t.Helper()
	gen := sha256.Sum256([]byte("gen"))
	id, err := NewIdentityFromKeys(testDeviceID, b64u(gen[:16]), ecdsaKey(t, "dev-sig"), ecdhKey(t, "dev-enc"), 1)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

const testNow = 1_767_225_600

// newTestDevice provisions a MemFS control dir holding grants and opens an
// Authority over it with a controllable clock and a seeded rand stream.
func newTestDevice(t testing.TB, grants ...Grant) *testDevice {
	t.Helper()
	d := &testDevice{t: t, fs: NewMemFS(), id: testIdentity(t), dir: "/state/control"}
	d.now.Store(testNow)
	if err := InitDir(d.fs, d.dir, d.id, grants); err != nil {
		t.Fatal(err)
	}
	d.reopen(false)
	return d
}

func (d *testDevice) reopen(enforced bool) {
	d.t.Helper()
	a, err := New(Config{Dir: d.dir, DeviceID: testDeviceID, Enforced: enforced, FS: d.fs,
		Now:  func() time.Time { return time.Unix(d.now.Load(), 0) },
		Rand: rand.NewChaCha8([32]byte{1})})
	if err != nil {
		d.t.Fatal(err)
	}
	if a.LockErr() != nil {
		d.t.Fatalf("authority locked: %v", a.LockErr())
	}
	d.a = a
}

func (d *testDevice) advance(sec int64) { d.now.Add(sec) }

// claims returns default claims from c to d for op.
func (d *testDevice) claims(c *testCtl, op Op) RequestClaims {
	c.ctr++
	now := d.now.Load()
	rk := sha256.Sum256([]byte("rk/" + c.label + strconv.FormatUint(c.ctr, 10)))
	return RequestClaims{V: 1, Dev: testDeviceID, Gen: d.id.gen, Ctl: c.ctl, Rid: "r" + c.label,
		Ctr: c.ctr, Iat: now, Exp: now + 60, Op: op, RK: b64u(rk[:])}
}

func (d *testDevice) cmdClaims(c *testCtl, cmd string, req string) RequestClaims {
	rc := d.claims(c, OpCmd)
	rc.Cmd, rc.Req = cmd, json.RawMessage(req)
	return rc
}

// seal signs claims with c and encrypts to the device.
func (d *testDevice) seal(c *testCtl, rc RequestClaims) string {
	d.t.Helper()
	payload, err := rc.Marshal()
	if err != nil {
		d.t.Fatal(err)
	}
	return sealPayload(d.t, d.id, c.sig, payload)
}

func sealPayload(t testing.TB, id *Identity, sig *ecdsa.PrivateKey, payload []byte) string {
	t.Helper()
	jws, err := SignRequest(sig, payload)
	if err != nil {
		t.Fatal(err)
	}
	return sealJWS(t, id, jws)
}

var epkCounter atomic.Int64

func sealJWS(t testing.TB, id *Identity, jws string) string {
	t.Helper()
	n := epkCounter.Add(1)
	epk := ecdhKey(t, "epk"+strconv.FormatInt(n, 10))
	iv := sha256.Sum256([]byte("iv" + strconv.FormatInt(n, 10)))
	out, err := SealRequest(id.encPub, epk, iv[:12], jws)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// send opens rc from c and returns the admission (failing on public errors).
func (d *testDevice) send(c *testCtl, rc RequestClaims) *Admission {
	d.t.Helper()
	adm, err := d.a.Open(d.seal(c, rc))
	if err != nil {
		d.t.Fatalf("Open: unexpected public error %v", err)
	}
	return adm
}

// final opens adm.Final with rc.rk and checks the §4.2 binding.
func (d *testDevice) final(adm *Admission, rc RequestClaims) ReplyClaims {
	d.t.Helper()
	if adm.Final == "" {
		d.t.Fatalf("expected a final reply, admission wants execution of %q", adm.Command)
	}
	return d.openReply(adm.Final, rc)
}

func (d *testDevice) openReply(compact string, rc RequestClaims) ReplyClaims {
	d.t.Helper()
	rk, _ := b64uDecode(rc.RK)
	r, err := OpenReply(compact, rk, d.id.sigPub)
	if err != nil {
		d.t.Fatalf("OpenReply: %v", err)
	}
	if !r.Matches(testDeviceID, d.id.gen, rc.Ctl, rc.Rid) {
		d.t.Fatalf("reply binding mismatch: %+v", r)
	}
	return r
}

// expectCode sends rc and asserts the encrypted reply code.
func (d *testDevice) expectCode(c *testCtl, rc RequestClaims, code string) ReplyClaims {
	d.t.Helper()
	adm := d.send(c, rc)
	r := d.final(adm, rc)
	if r.OK || r.Code != code {
		d.t.Fatalf("reply = ok:%v code:%q, want code %q (res %s)", r.OK, r.Code, code, r.Res)
	}
	return r
}

func (d *testDevice) expectOK(c *testCtl, rc RequestClaims) ReplyClaims {
	d.t.Helper()
	adm := d.send(c, rc)
	r := d.final(adm, rc)
	if !r.OK {
		d.t.Fatalf("reply = code %q res %s, want ok", r.Code, r.Res)
	}
	return r
}

func (d *testDevice) expectExecute(c *testCtl, rc RequestClaims) *Admission {
	d.t.Helper()
	adm := d.send(c, rc)
	if !adm.Execute() {
		r := d.final(adm, rc)
		d.t.Fatalf("expected execution, got code %q", r.Code)
	}
	return adm
}

func (d *testDevice) expectPublic(compact, code string) {
	d.t.Helper()
	_, err := d.a.Open(compact)
	if publicCode(err) != code {
		d.t.Fatalf("Open error = %v, want public %q", err, code)
	}
}

// toDER re-encodes a compact JWS's r||s signature as ASN.1 DER.
func toDER(t testing.TB, jws string) string {
	t.Helper()
	parts := strings.Split(jws, ".")
	sig, _ := b64uDecode(parts[2])
	der, err := asn1.Marshal(struct{ R, S *big.Int }{new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])})
	if err != nil {
		t.Fatal(err)
	}
	return parts[0] + "." + parts[1] + "." + b64u(der)
}

// publicCode returns the public error code carried by err, or "".
func publicCode(err error) string {
	var pe *PublicError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func utoa(u uint64) string { return strconv.FormatUint(u, 10) }
