package control_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/control/internal/fixturegen"
)

const fixtureDir = "testdata/fixtures"

// TestFixturesAreCurrent fails when the committed vectors differ from what
// the code generates. Regenerate with `go run ./control/cmd/genfixtures`.
func TestFixturesAreCurrent(t *testing.T) {
	want, err := fixturegen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	var onDisk []string
	_ = filepath.WalkDir(fixtureDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(fixtureDir, p)
			onDisk = append(onDisk, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(onDisk)
	if len(onDisk) != len(want) {
		t.Fatalf("%d files on disk, generator produces %d; regenerate", len(onDisk), len(want))
	}
	for _, name := range onDisk {
		got, _ := os.ReadFile(filepath.Join(fixtureDir, name)) //nolint:gosec // G304: walked from the fixture dir.
		if !bytes.Equal(got, want[name]) {
			t.Errorf("%s is stale; run go run ./control/cmd/genfixtures", name)
		}
	}
}

func loadKeys(t *testing.T) fixturegen.Keys {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	var k fixturegen.Keys
	if err := json.Unmarshal(b, &k); err != nil {
		t.Fatal(err)
	}
	return k
}

func loadFixtures(t *testing.T, sub string) []fixturegen.Fixture {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(fixtureDir, sub, "*.json"))
	if len(paths) == 0 {
		t.Fatalf("no %s fixtures", sub)
	}
	var out []fixturegen.Fixture
	for _, p := range paths {
		b, err := os.ReadFile(p) //nolint:gosec // G304: globbed from the fixture dir.
		if err != nil {
			t.Fatal(err)
		}
		var f fixturegen.Fixture
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		out = append(out, f)
	}
	return out
}

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func pubJWK(t *testing.T, j fixturegen.PrivateJWK) control.PublicJWK {
	t.Helper()
	p, err := control.ParsePublicJWK([]byte(j.PublicJSON()))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}

func TestPositiveFixtures(t *testing.T) {
	k := loadKeys(t)
	devSig := pubJWK(t, k.Device.Sig)
	for _, f := range loadFixtures(t, "positive") {
		t.Run(f.Name, func(t *testing.T) {
			if f.Kind == "approvalCode" {
				b, _ := os.ReadFile(filepath.Join(fixtureDir, "positive", f.Name+".json")) //nolint:gosec // G304: fixture dir.
				var v fixturegen.ApprovalCodes
				if err := json.Unmarshal(b, &v); err != nil || len(v.Cases) < 2 {
					t.Fatal(err)
				}
				for _, c := range v.Cases {
					if got := control.ApprovalCode(c.Ctl, c.SigPin); got != c.Code || len(got) != 6 {
						t.Fatalf("ApprovalCode(%s, %s) = %s, want %s", c.Ctl, c.SigPin, got, c.Code)
					}
				}
				if v.Cases[len(v.Cases)-1].Code == v.Cases[len(v.Cases)-2].Code {
					t.Fatal("the device pin does not change the code")
				}
				return
			}
			if f.Kind == "notification" {
				for _, n := range f.Notifications {
					got, err := control.OpenNotification(n.Compact, k.Controllers[n.To].Ctl, b64(t, k.Controllers[n.To].NK))
					if err != nil {
						t.Fatal(err)
					}
					gb, _ := json.Marshal(got)
					if !jsonEqual(t, gb, n.Claims) {
						t.Fatalf("claims %s != %s", gb, n.Claims)
					}
				}
				return
			}
			// Independent decode with go-jose: the device key opens the
			// JWE and the controller key verifies the inner JWS.
			devEnc, _ := ecdsa.ParseRawPrivateKey(elliptic.P256(), b64(t, k.Device.Enc.D))
			obj, err := jose.ParseEncryptedCompact(f.Compact, []jose.KeyAlgorithm{jose.ECDH_ES}, []jose.ContentEncryption{jose.A256GCM})
			if err != nil {
				t.Fatal(err)
			}
			jwsBytes, err := obj.Decrypt(devEnc)
			if err != nil || string(jwsBytes) != f.JWS {
				t.Fatalf("go-jose decrypt: %v", err)
			}
			sig, err := jose.ParseSigned(f.JWS, []jose.SignatureAlgorithm{jose.ES256})
			if err != nil {
				t.Fatal(err)
			}
			payload, err := sig.Verify(&k.Controllers[f.From].Sig.ECDSA().PublicKey)
			if err != nil || string(payload) != f.Payload {
				t.Fatalf("go-jose verify: %v", err)
			}

			// The device's verdict.
			a, _, err := fixturegen.BuildDevice(k, *f.Setup)
			if err != nil {
				t.Fatal(err)
			}
			adm, err := a.Open(f.Compact)
			if err != nil {
				t.Fatal(err)
			}
			if adm.Execute() != f.Expect.Execute || adm.Command != f.Expect.Command {
				t.Fatalf("execute=%v command=%q, want %+v", adm.Execute(), adm.Command, f.Expect)
			}
			if f.Expect.Principal != nil && adm.Principal != *f.Expect.Principal {
				t.Fatalf("principal %+v", adm.Principal)
			}
			if !adm.Execute() {
				r, err := control.OpenReply(adm.Final, b64(t, f.RK), devSig)
				if err != nil || r.OK != f.Expect.ReplyOK || r.Code != f.Expect.ReplyCode {
					t.Fatalf("reply %+v %v", r, err)
				}
			}
			// The client's view of the recorded reply.
			var rc struct{ Ctl, Rid string }
			_ = json.Unmarshal([]byte(f.Payload), &rc)
			r, err := control.OpenReply(f.Reply.Compact, b64(t, f.RK), devSig)
			if err != nil {
				t.Fatal(err)
			}
			if !r.Matches(k.Device.Dev, k.Device.Gen, rc.Ctl, rc.Rid) || r.OK != f.Expect.ReplyOK {
				t.Fatalf("reply binding %+v", r)
			}
			rb, _ := json.Marshal(r)
			if !jsonEqual(t, rb, f.Reply.Claims) {
				t.Fatalf("reply claims %s != %s", rb, f.Reply.Claims)
			}
			for _, n := range f.Notifications {
				if _, err := control.OpenNotification(n.Compact, k.Controllers[n.To].Ctl, b64(t, k.Controllers[n.To].NK)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestNegativeFixtures(t *testing.T) {
	k := loadKeys(t)
	devSig := pubJWK(t, k.Device.Sig)
	seen := map[string]bool{}
	for _, f := range loadFixtures(t, "negative") {
		seen[f.Name] = true
		t.Run(f.Name, func(t *testing.T) {
			switch f.Kind {
			case "reply":
				r, err := control.OpenReply(f.Compact, b64(t, f.RK), devSig)
				if err == nil && r.Matches(f.Bind.Dev, f.Bind.Gen, f.Bind.Ctl, f.Bind.Rid) {
					t.Fatal("client accepted a reply it must reject")
				}
				return
			case "notification":
				if _, err := control.OpenNotification(f.Compact, k.Controllers[f.To].Ctl, b64(t, f.NK)); err == nil {
					t.Fatal("client accepted a notification it must reject")
				}
				return
			}
			a, _, err := fixturegen.BuildDevice(k, *f.Setup)
			if err != nil {
				t.Fatal(err)
			}
			compact := f.Compact
			if f.CompactRepeat != nil {
				compact = strings.Repeat(f.CompactRepeat.Char, f.CompactRepeat.Count)
			}
			adm, err := a.Open(compact)
			if f.Expect.Public != "" {
				var pe *control.PublicError
				if !errors.As(err, &pe) || pe.Code != f.Expect.Public {
					t.Fatalf("got %v, want public %s", err, f.Expect.Public)
				}
				return
			}
			if err != nil || adm.Execute() {
				t.Fatalf("got %v, want reply %s", err, f.Expect.ReplyCode)
			}
			r, err := control.OpenReply(adm.Final, b64(t, f.RK), devSig)
			if err != nil || r.OK || r.Code != f.Expect.ReplyCode {
				t.Fatalf("reply %+v %v, want %s", r, err, f.Expect.ReplyCode)
			}
		})
	}
	// The §12 list plus the M1 additions must all be present.
	for _, name := range []string{"tampered-tag", "wrong-jwe-kid", "wrong-dev", "wrong-gen", "expired", "replayed-ctr",
		"out-of-window-ctr", "der-signature", "jwe-extra-header-member", "jws-extra-header-member", "jws-alg-none",
		"jwe-alg-rsa", "jws-alg-rs256", "oversize", "reply-wrong-rk", "revoked-controller", "unknown-kid",
		"lifetime-over-120", "ctl-not-kid"} {
		if !seen[name] {
			t.Errorf("negative fixture %s missing", name)
		}
	}
}
