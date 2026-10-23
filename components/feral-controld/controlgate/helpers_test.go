package controlgate_test

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/control"
)

const testDevice = "FF1-GATETEST"

var b64 = base64.RawURLEncoding

// newAuthority opens a fresh in-memory legacy authority.
func newAuthority(t *testing.T, enforced bool) (*control.Authority, *control.MemFS) {
	t.Helper()
	fsys := control.NewMemFS()
	a, err := control.New(control.Config{Dir: "/s/control", DeviceID: testDevice, FS: fsys, Enforced: enforced})
	if err != nil || a.LockErr() != nil {
		t.Fatal(err, a.LockErr())
	}
	return a, fsys
}

// client is a controller-side test implementation over the exported codec.
type client struct {
	t       *testing.T
	label   string
	sig     *ecdsa.PrivateKey
	enc     *ecdh.PrivateKey
	sigPub  control.PublicJWK
	encPub  control.PublicJWK
	ctl     string
	ctr     uint64
	profile control.Profile
	nk      []byte
}

func newClient(t *testing.T, label string, profile control.Profile) *client {
	t.Helper()
	sig, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	enc, _ := ecdh.P256().GenerateKey(rand.Reader)
	sp, _ := control.PublicJWKFromECDSA(&sig.PublicKey)
	ep, _ := control.PublicJWKFromECDH(enc.PublicKey())
	return &client{t: t, label: label, sig: sig, enc: enc, sigPub: sp, encPub: ep, ctl: sp.Thumbprint(), profile: profile}
}

type sent struct {
	claims control.RequestClaims
	rk     []byte
	body   json.RawMessage // the ff1Envelope request object
	cast   []byte          // the whole cast body
}

func (c *client) build(op control.Op, cmd string, req string, inv string) sent {
	c.t.Helper()
	c.ctr++
	rk := make([]byte, 32)
	_, _ = rand.Read(rk)
	now := time.Now().Unix()
	rc := control.RequestClaims{V: 1, Dev: c.profile.Dev, Gen: c.profile.Gen, Ctl: c.ctl, Rid: c.label + strconv.FormatUint(c.ctr, 10),
		Ctr: c.ctr, Iat: now, Exp: now + 60, Op: op, RK: b64.EncodeToString(rk)}
	if op == control.OpCmd {
		rc.Cmd, rc.Req = cmd, json.RawMessage(req)
	} else {
		rc.Enroll = &control.Enroll{Sig: c.sigPub, Enc: c.encPub, Label: c.label, Platform: "cli", Inv: inv}
	}
	payload, err := rc.Marshal()
	if err != nil {
		c.t.Fatal(err)
	}
	jws, err := control.SignRequest(c.sig, payload)
	if err != nil {
		c.t.Fatal(err)
	}
	epk, _ := ecdh.P256().GenerateKey(rand.Reader)
	iv := make([]byte, 12)
	_, _ = rand.Read(iv)
	compact, err := control.SealRequest(c.profile.Enc, epk, iv, jws)
	if err != nil {
		c.t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"v": 1, "jwe": compact})
	cast, _ := json.Marshal(map[string]any{"command": control.CmdEnvelope, "request": json.RawMessage(body)})
	return sent{claims: rc, rk: rk, body: body, cast: cast}
}

// open verifies a {"v":1,"jwe"} reply against the pinned profile and binding.
func (c *client) open(s sent, wire []byte) control.ReplyClaims {
	c.t.Helper()
	var w struct {
		V   int    `json:"v"`
		JWE string `json:"jwe"`
	}
	if err := json.Unmarshal(wire, &w); err != nil || w.V != 1 || w.JWE == "" {
		c.t.Fatalf("not a wire reply: %s", wire)
	}
	r, err := control.OpenReply(w.JWE, s.rk, c.profile.Sig)
	if err != nil {
		c.t.Fatalf("OpenReply: %v", err)
	}
	if !r.Matches(c.profile.Dev, c.profile.Gen, c.ctl, s.claims.Rid) {
		c.t.Fatalf("reply binding mismatch %+v", r)
	}
	return r
}

// recorder is a commandrouter.Handler that records what it ran and as whom.
type recorder struct {
	mu    sync.Mutex
	calls []call
	err   error
}

type call struct {
	cmd            commands.Command
	principal      control.Principal
	hasP           bool
	fromController bool
}

func (r *recorder) Process(ctx context.Context, cmd commands.Command) (interface{}, error) {
	p, ok := control.PrincipalFrom(ctx)
	r.mu.Lock()
	r.calls = append(r.calls, call{cmd, p, ok, commands.IsFromController(ctx)})
	r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	return map[string]any{"ok": true, "ran": string(cmd.Type)}, nil
}

func (r *recorder) last() call {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return call{}
	}
	return r.calls[len(r.calls)-1]
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}
