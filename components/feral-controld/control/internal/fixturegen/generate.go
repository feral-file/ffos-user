package fixturegen

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
)

type gen struct {
	k     Keys
	files map[string][]byte
}

// Generate returns every fixture file keyed by its path under fixtures/.
func Generate() (map[string][]byte, error) {
	g := &gen{k: NewKeys(), files: map[string][]byte{}}
	g.files["keys.json"] = Encode(g.k)
	steps := []func() error{g.positives, g.approvalCodes, g.negativeRequests, g.negativeClient, g.registrationVectors}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}
	return g.files, nil
}

func (g *gen) setup(name string, grants ...string) *Setup {
	if grants == nil {
		grants = []string{"owner", "controller", "revoked"}
	}
	return &Setup{Now: Now, Grants: grants, RandSeed: b64u(derive("rand/" + name))}
}

const castReq = `{"intent":{"action":"now_display"},"dp1_call":{"dpVersion":"1.0.0","title":"fixture","items":[]}}`

// claims returns the default op=cmd claims from `from`.
func (g *gen) claims(name, from string) control.RequestClaims {
	c := g.k.Controllers[from]
	return control.RequestClaims{V: 1, Dev: g.k.Device.Dev, Gen: g.k.Device.Gen, Ctl: c.Ctl,
		Rid: "rid-" + strings.ReplaceAll(name, ".", "_"), Ctr: 1, Iat: Now, Exp: Now + 60, Op: control.OpCmd,
		RK: b64u(derive("rk/" + name)), Cmd: "displayPlaylist", Req: json.RawMessage(castReq)}
}

func (g *gen) enroll(from, inv string) *control.Enroll {
	c := g.k.Controllers[from]
	return &control.Enroll{Sig: pub(c.Sig), Enc: pub(c.Enc), Label: c.Label, Platform: c.Platform, Inv: inv}
}

// request seals claims with the production code path.
func (g *gen) request(f *Fixture, rc control.RequestClaims) error {
	payload, err := rc.Marshal()
	if err != nil {
		return err
	}
	jws, err := control.SignRequest(g.k.Controllers[f.From].Sig.ECDSA(), payload)
	if err != nil {
		return err
	}
	epk := jwkFromScalar(scalar("epk/" + f.Name))
	iv := derive("iv/" + f.Name)[:12]
	compact, err := control.SealRequest(pub(g.k.Device.Enc), epk.ECDH(), iv, jws)
	if err != nil {
		return err
	}
	f.Payload, f.Claims, f.JWS, f.Compact = string(payload), payload, jws, compact
	f.EphemeralKey, f.IV, f.RK = &epk, b64u(iv), rc.RK
	return nil
}

func (g *gen) emit(dir string, f *Fixture) {
	g.files[dir+"/"+f.Name+".json"] = Encode(f)
}

// positive builds a positive request vector: open it on a fresh device,
// answer it (executeRes for commands the caller runs), record the reply.
func (g *gen) positive(f *Fixture, rc control.RequestClaims, executeRes any) error {
	a, inv, err := BuildDevice(g.k, *f.Setup)
	if err != nil {
		return err
	}
	if f.Setup.Invitation {
		// The invitation comes from the device's seeded stream (after the
		// 8-byte ep draw), so a consumer that rebuilds the device from
		// Setup gets the same value.
		rc.Enroll.Inv = inv
	}
	if err := g.request(f, rc); err != nil {
		return err
	}
	adm, err := a.Open(f.Compact)
	if err != nil {
		return fmt.Errorf("%s: %w", f.Name, err)
	}
	reply := adm.Final
	f.Expect.Command = adm.Command
	if adm.Execute() {
		f.Expect.Execute = true
		ok, code := true, ""
		if m, isMap := executeRes.(map[string]any); isMap && m["ok"] == false {
			ok, code = false, control.CodeCommandFailed
		}
		if reply, err = adm.Reply(ok, code, executeRes); err != nil {
			return err
		}
	}
	if adm.Principal.ControllerID != "" {
		p := adm.Principal
		f.Expect.Principal = &p
	}
	r, err := control.OpenReply(reply, mustB64(rc.RK), pub(g.k.Device.Sig))
	if err != nil {
		return err
	}
	f.Expect.ReplyOK, f.Expect.ReplyCode = r.OK, r.Code
	claims, _ := json.Marshal(r)
	f.Reply = &ReplyVector{Compact: reply, Claims: claims}
	for _, s := range adm.Notifications {
		to := ""
		for name, c := range g.k.Controllers {
			if c.Ctl == s.Ctl {
				to = name
			}
		}
		n, err := control.OpenNotification(s.JWE, s.Ctl, mustB64(g.k.Controllers[to].NK))
		if err != nil {
			return err
		}
		nc, _ := json.Marshal(n)
		wire, _ := s.Wire()
		f.Notifications = append(f.Notifications, NotificationVector{To: to, Compact: s.JWE, Claims: nc, Wire: string(wire)})
	}
	g.emit("positive", f)
	return nil
}

func (g *gen) positives() error {
	type pos struct {
		name, desc, from string
		setup            *Setup
		mutate           func(*control.RequestClaims)
		res              any
		// The outcome the vector is FOR. The generator checks the device
		// really produces it, so a label can never drift from its outcome.
		execute  bool
		wantCode string // "" = ok:true
	}
	list := []pos{
		{"cmd-owner-displayPlaylist", "Owner casts a playlist; the caller executes it and replies ok.", "owner", nil,
			nil, map[string]any{"ok": true}, true, ""},
		{"cmd-controller-rotate", "Controller-tier command from a controller grant.", "controller", nil,
			func(c *control.RequestClaims) { c.Cmd, c.Req = "rotate", json.RawMessage(`{"clockwise":true}`) }, map[string]any{"ok": true}, true, ""},
		{"cmd-controller-command-failed", "Executed command that failed: ok:false, code command_failed, res = the handler's error body.", "controller", nil,
			nil, map[string]any{"ok": false, "error": "sourceUnreachable", "command": "displayPlaylist", "message": "sourceUnreachable: no playlist item source is loadable (item 0: HTTP 400)"}, true, control.CodeCommandFailed},
		{"cmd-owner-listControllers", "Owner control command answered by the authority itself.", "owner", nil,
			func(c *control.RequestClaims) { c.Cmd, c.Req = control.CmdListControllers, json.RawMessage(`{}`) }, nil, false, ""},
		{"enroll-invitation", "First owner enrolls with the invitation on a legacy device (§5.1).", "stranger",
			nil, func(c *control.RequestClaims) {
				c.Op, c.Cmd, c.Req = control.OpEnroll, "", nil
				c.Enroll = g.enroll("stranger", "")
			}, nil, false, ""},
		{"requestAccess-pending", "A later controller asks for access; the owner gets a sealed controller_approval_request (§5.2).", "stranger", nil,
			func(c *control.RequestClaims) {
				c.Op, c.Cmd, c.Req = control.OpRequestAccess, "", nil
				c.Enroll = g.enroll("stranger", "")
			}, nil, false, ""},
		{"enroll-approved", "An approved controller enrolls without inv and receives its nk (§5.3).", "controller", nil,
			func(c *control.RequestClaims) {
				c.Op, c.Cmd, c.Req = control.OpEnroll, "", nil
				c.Enroll = g.enroll("controller", "")
			}, nil, false, ""},
		{"cmd-owner-reordered-ctr", "ctr below hi but inside the window and unseen is accepted (§6).", "owner", nil,
			func(c *control.RequestClaims) { c.Ctr = 40 }, map[string]any{"ok": true}, true, ""},
		{"cmd-owner-registerController", "Owner registers the keys it scanned from a controller QR (§5.6); res carries the derived ctl. The controller then enrolls without inv as in enroll-approved.", "owner", nil,
			func(c *control.RequestClaims) {
				c.Cmd, c.Req = control.CmdRegisterController, g.registerReq("stranger", "controller", `,"expiresAt":`+fmt.Sprint(Now+86400))
			}, nil, false, ""},
		{"cmd-owner-registerController-reactivate", "Registering a revoked record's keys reactivates it with a fresh nk, keeping its window (§5.6).", "owner", nil,
			func(c *control.RequestClaims) {
				c.Cmd, c.Req = control.CmdRegisterController, g.registerReq("revoked", "controller", "")
			}, nil, false, ""},
		{"cmd-controller-self-revoke", "A controller revokes its own grant: the one revokeController form a controller may send (§5.5).", "controller", nil,
			func(c *control.RequestClaims) {
				c.Cmd, c.Req = control.CmdRevokeController, json.RawMessage(`{"ctl":"`+g.k.Controllers["controller"].Ctl+`"}`)
			}, nil, false, ""},
	}
	for _, p := range list {
		f := &Fixture{Name: p.name, Kind: "request", Description: p.desc, From: p.from, Setup: g.setup(p.name)}
		switch p.name {
		case "enroll-invitation":
			f.Setup.Grants = []string{}
			f.Setup.Invitation = true
		case "cmd-owner-reordered-ctr":
			// hi=50; 50 and 49 seen (bits 0, 1); ctr 40 (bit 10) unseen.
			f.Setup.Windows = map[string]WindowSpec{"owner": {Hi: 50, Bitmap: "0000000000000003"}}
		}
		rc := g.claims(p.name, p.from)
		if p.mutate != nil {
			p.mutate(&rc)
		}
		if err := g.positive(f, rc, p.res); err != nil {
			return fmt.Errorf("%s: %w", p.name, err)
		}
		wantOK := p.wantCode == ""
		if f.Expect.Execute != p.execute || f.Expect.ReplyOK != wantOK || f.Expect.ReplyCode != p.wantCode {
			return fmt.Errorf("%s: device outcome execute=%v ok=%v code=%q, vector is for execute=%v code=%q",
				p.name, f.Expect.Execute, f.Expect.ReplyOK, f.Expect.ReplyCode, p.execute, p.wantCode)
		}
	}
	// A standalone sealed notification (§9).
	a, _, err := BuildDevice(g.k, *g.setup("notification-device-status"))
	if err != nil {
		return err
	}
	s, err := a.Seal(g.k.Controllers["owner"].Ctl, "device_status", map[string]any{"screenRotation": "landscape", "volume": 40})
	if err != nil {
		return err
	}
	n, err := control.OpenNotification(s.JWE, s.Ctl, mustB64(g.k.Controllers["owner"].NK))
	if err != nil {
		return err
	}
	nc, _ := json.Marshal(n)
	wire, _ := s.Wire()
	g.emit("positive", &Fixture{Name: "notification-device-status", Kind: "notification",
		Description: "Sealed notification to the owner under its nk; open with keys.json controllers.owner.nk.",
		Setup:       g.setup("notification-device-status"), To: "owner", NK: g.k.Controllers["owner"].NK, Compact: s.JWE,
		Notifications: []NotificationVector{{To: "owner", Compact: s.JWE, Claims: nc, Wire: string(wire)}}})
	return nil
}

// negative builds a device-side negative vector and checks the device
// really answers it with the expected code before emitting it.
func (g *gen) negative(f *Fixture, expectPublic, expectReply string) error {
	f.Kind = "request"
	if f.Setup == nil {
		f.Setup = g.setup(f.Name)
	}
	f.Expect = Expect{Public: expectPublic, ReplyCode: expectReply}
	a, _, err := BuildDevice(g.k, *f.Setup)
	if err != nil {
		return err
	}
	compact := f.Compact
	if f.CompactRepeat != nil {
		compact = strings.Repeat(f.CompactRepeat.Char, f.CompactRepeat.Count)
	}
	adm, err := a.Open(compact)
	var pe *control.PublicError
	switch {
	case expectPublic != "":
		if !errors.As(err, &pe) || pe.Code != expectPublic {
			return fmt.Errorf("%s: got %w, want public %s", f.Name, err, expectPublic)
		}
	default:
		if err != nil || adm.Final == "" {
			return fmt.Errorf("%s: got %w, want reply %s", f.Name, err, expectReply)
		}
		r, err := control.OpenReply(adm.Final, mustB64(f.RK), pub(g.k.Device.Sig))
		if err != nil || r.Code != expectReply {
			return fmt.Errorf("%s: reply %+v %w, want %s", f.Name, r, err, expectReply)
		}
		claims, _ := json.Marshal(r)
		f.Reply = &ReplyVector{Compact: adm.Final, Claims: claims}
	}
	g.emit("negative", f)
	return nil
}

func (g *gen) negativeRequests() error {
	devEnc := g.k.Device.Enc.ECDH().PublicKey()
	encKid := g.k.Device.Enc.Thumb()
	std := func(epkJSON string) string {
		return `{"alg":"ECDH-ES","enc":"A256GCM","kid":"` + encKid + `","typ":"ff1-req+jwe","cty":"ff1-req+jws","epk":` + epkJSON + `}`
	}
	owner := g.k.Controllers["owner"]
	jwsHdr := func(kid string) string { return `{"alg":"ES256","kid":"` + kid + `","typ":"ff1-req+jws"}` }

	// custom builds a vector from claims with a chosen JWS header/signer
	// and JWE header.
	type custom struct {
		name, desc, from, public, reply string
		mutate                          func(*control.RequestClaims)
		jwsHeader                       string // default canonical
		signer                          *PrivateJWK
		der                             bool
		jweHeader                       func(string) string
		post                            func(string) string
	}
	list := []custom{
		{name: "tampered-tag", desc: "Last tag byte flipped.", public: control.CodeBadEnvelope,
			post: func(c string) string {
				return c[:len(c)-1] + map[bool]string{true: "B", false: "A"}[c[len(c)-1] == 'A']
			}},
		{name: "wrong-jwe-kid", desc: "JWE kid is not thumb(devEnc) (the devSig thumbprint instead).", public: control.CodeBadEnvelope,
			jweHeader: func(e string) string { return strings.Replace(std(e), encKid, g.k.Device.Sig.Thumb(), 1) }},
		{name: "jwe-extra-header-member", desc: `JWE header carries an extra "zip" member.`, public: control.CodeBadEnvelope,
			jweHeader: func(e string) string { return strings.Replace(std(e), `{"alg"`, `{"zip":"DEF","alg"`, 1) }},
		{name: "jwe-duplicate-header-member", desc: "JWE header repeats alg.", public: control.CodeBadEnvelope,
			jweHeader: func(e string) string {
				return strings.Replace(std(e), `{"alg":"ECDH-ES"`, `{"alg":"ECDH-ES","alg":"ECDH-ES"`, 1)
			}},
		{name: "jwe-alg-rsa", desc: "JWE alg RSA-OAEP-256 (content still ECDH-ES derived).", public: control.CodeBadEnvelope,
			jweHeader: func(e string) string { return strings.Replace(std(e), `"ECDH-ES"`, `"RSA-OAEP-256"`, 1) }},
		{name: "jwe-epk-off-curve", desc: "epk y replaced so the point is not on P-256.", public: control.CodeBadEnvelope,
			post: func(c string) string { return c }, // replaced below
		},
		{name: "jws-extra-header-member", desc: `JWS header carries an extra "jwk" member.`, public: control.CodeBadEnvelope,
			jwsHeader: `{"alg":"ES256","kid":"` + owner.Ctl + `","typ":"ff1-req+jws","jwk":{}}`},
		{name: "jws-alg-none", desc: `JWS alg "none" (signature still present).`, public: control.CodeBadEnvelope,
			jwsHeader: `{"alg":"none","kid":"` + owner.Ctl + `","typ":"ff1-req+jws"}`},
		{name: "jws-alg-rs256", desc: "JWS alg RS256.", public: control.CodeBadEnvelope,
			jwsHeader: `{"alg":"RS256","kid":"` + owner.Ctl + `","typ":"ff1-req+jws"}`},
		{name: "der-signature", desc: "ES256 signature in ASN.1 DER instead of r||s.", public: control.CodeBadSignature, der: true},
		{name: "bad-signature", desc: "Owner kid, signed by the stranger's key.", public: control.CodeBadSignature,
			signer: ptr(g.k.Controllers["stranger"].Sig)},
		{name: "unknown-kid", desc: "op=cmd from a controller with no grant.", from: "stranger", public: control.CodeUnknownController},
		{name: "wrong-dev", desc: "dev names another device.", reply: control.CodeInvalidRequest,
			mutate: func(c *control.RequestClaims) { c.Dev = "FF1-OTHER001" }},
		{name: "wrong-gen", desc: "gen is not the device's.", reply: control.CodeInvalidRequest,
			mutate: func(c *control.RequestClaims) { c.Gen = b64u(derive("other-gen")[:16]) }},
		{name: "ctl-not-kid", desc: "ctl claim differs from the JWS kid; the reply names the kid.", reply: control.CodeInvalidRequest,
			mutate: func(c *control.RequestClaims) { c.Ctl = g.k.Controllers["controller"].Ctl }},
		{name: "lifetime-over-120", desc: "exp - iat = 121.", reply: control.CodeInvalidRequest,
			mutate: func(c *control.RequestClaims) { c.Exp = c.Iat + 121 }},
		{name: "expired", desc: "now > exp + 30.", reply: control.CodeExpired,
			mutate: func(c *control.RequestClaims) { c.Iat, c.Exp = Now-120, Now-31 }},
		{name: "replayed-ctr", desc: "ctr already accepted (window hi=10, bit 0 set).", reply: control.CodeReplay,
			mutate: func(c *control.RequestClaims) { c.Ctr = 10 }},
		{name: "out-of-window-ctr", desc: "ctr <= hi - 64 (hi=100, ctr=36).", reply: control.CodeReplay,
			mutate: func(c *control.RequestClaims) { c.Ctr = 36 }},
		{name: "revoked-controller", desc: "Authentic request from a revoked grant.", from: "revoked", reply: control.CodeAccessRevoked},
		{name: "permission-denied", desc: "Controller grant sends an owner-tier command.", from: "controller", reply: control.CodePermissionDenied,
			mutate: func(c *control.RequestClaims) { c.Cmd, c.Req = "sshAccess", json.RawMessage(`{"enabled":true}`) }},
		{name: "registerController-from-controller", desc: "A controller grant cannot register another controller (§5.6, owner tier).", from: "controller", reply: control.CodePermissionDenied,
			mutate: func(c *control.RequestClaims) {
				c.Cmd, c.Req = control.CmdRegisterController, g.registerReq("stranger", "controller", "")
			}},
		{name: "revoke-other-from-controller", desc: "A controller may revoke only itself; naming another ctl is owner tier (§5.5).", from: "controller", reply: control.CodePermissionDenied,
			mutate: func(c *control.RequestClaims) {
				c.Cmd, c.Req = control.CmdRevokeController, json.RawMessage(`{"ctl":"`+g.k.Controllers["owner"].Ctl+`"}`)
			}},
	}
	for _, v := range list {
		from := v.from
		if from == "" {
			from = "owner"
		}
		f := &Fixture{Name: v.name, Description: v.desc, From: from, Setup: g.setup(v.name)}
		switch v.name {
		case "replayed-ctr":
			f.Setup.Windows = map[string]WindowSpec{"owner": {Hi: 10, Bitmap: "0000000000000001"}}
		case "out-of-window-ctr":
			f.Setup.Windows = map[string]WindowSpec{"owner": {Hi: 100, Bitmap: "0000000000000001"}}
		}
		rc := g.claims(v.name, from)
		if v.mutate != nil {
			v.mutate(&rc)
		}
		payload, err := rc.Marshal()
		if err != nil {
			return err
		}
		signer := g.k.Controllers[from].Sig
		if v.signer != nil {
			signer = *v.signer
		}
		hdr := v.jwsHeader
		if hdr == "" {
			hdr = jwsHdr(g.k.Controllers[from].Ctl)
		}
		jws := signRaw(signer.ECDSA(), hdr, payload, v.der)
		epk := jwkFromScalar(scalar("epk/" + v.name))
		iv := derive("iv/" + v.name)[:12]
		jh := v.jweHeader
		if jh == nil {
			jh = std
		}
		if v.name == "jwe-epk-off-curve" {
			jh = func(e string) string {
				var m map[string]string
				_ = json.Unmarshal([]byte(e), &m)
				return std(strings.Replace(e, m["y"], m["x"], 1))
			}
		}
		compact := sealECDHES(devEnc, epk.ECDH(), iv, jh, []byte(jws))
		if v.post != nil {
			compact = v.post(compact)
		}
		f.Payload, f.Claims, f.JWS, f.Compact, f.EphemeralKey, f.IV, f.RK = string(payload), payload, jws, compact, &epk, b64u(iv), rc.RK
		if err := g.negative(f, v.public, v.reply); err != nil {
			return err
		}
	}
	return g.negative(&Fixture{Name: "oversize", Description: "A compact one byte over the 4 MiB limit (MaxCompactBytes); build it from compactRepeat.",
		CompactRepeat: &Repeat{Char: "A", Count: control.MaxCompactBytes + 1}}, control.CodeBadEnvelope, "")
}

func ptr[T any](v T) *T { return &v }

// negativeClient builds replies and notifications a controller MUST reject.
func (g *gen) negativeClient() error {
	owner := g.k.Controllers["owner"]
	rk := derive("rk/client")
	bind := &Bind{Dev: g.k.Device.Dev, Gen: g.k.Device.Gen, Ctl: owner.Ctl, Rid: "rid-client"}
	claims := `{"v":1,"dev":"` + bind.Dev + `","gen":"` + bind.Gen + `","ctl":"` + bind.Ctl + `","rid":"rid-client","iat":` + fmt.Sprint(Now) + `,"ok":true,"res":{"ok":true}}`
	resJWS := func(signer PrivateJWK, kid, body string) string {
		return signRaw(signer.ECDSA(), `{"alg":"ES256","kid":"`+kid+`","typ":"ff1-res+jws"}`, []byte(body), false)
	}
	resHdr := `{"alg":"dir","enc":"A256GCM","typ":"ff1-res+jwe","cty":"ff1-res+jws"}`
	devKid := g.k.Device.Sig.Thumb()
	iv := derive("iv/client")[:12]
	good := gcmSeal(resHdr, rk, iv, []byte(resJWS(g.k.Device.Sig, devKid, claims)))
	// Self-check: the good reply opens.
	if r, err := control.OpenReply(good, rk, pub(g.k.Device.Sig)); err != nil || !r.Matches(bind.Dev, bind.Gen, bind.Ctl, bind.Rid) {
		return fmt.Errorf("client baseline reply does not open: %w", err)
	}
	type rv struct {
		name, desc, compact, rk string
		bind                    *Bind
	}
	otherBind := *bind
	otherBind.Rid = "rid-other"
	replies := []rv{
		{"reply-wrong-rk", "Valid reply opened with a different rk.", good, b64u(derive("rk/other")), bind},
		{"reply-tampered", "Reply ciphertext byte flipped.", strings.Replace(good, ".."+b64u(iv)+".", ".."+b64u(iv)+"."+"A", 1), b64u(rk), bind},
		{"reply-wrong-signer", "Reply signed by a key that is not the pinned devSig (kid claims devSig).", gcmSeal(resHdr, rk, iv, []byte(resJWS(owner.Sig, devKid, claims))), b64u(rk), bind},
		{"reply-wrong-rid", "Authentic reply whose rid is not the request's.", good, b64u(rk), &otherBind},
		{"reply-extra-header-member", "Reply JWE header with an extra member.", gcmSeal(strings.Replace(resHdr, `{"alg"`, `{"kid":"x","alg"`, 1), rk, iv, []byte(resJWS(g.k.Device.Sig, devKid, claims))), b64u(rk), bind},
		{"reply-der-signature", "Reply signature in DER.", gcmSeal(resHdr, rk, iv, []byte(signRaw(g.k.Device.Sig.ECDSA(), `{"alg":"ES256","kid":"`+devKid+`","typ":"ff1-res+jws"}`, []byte(claims), true))), b64u(rk), bind},
	}
	for _, r := range replies {
		// Self-check: the client API rejects it.
		out, err := control.OpenReply(r.compact, mustB64(r.rk), pub(g.k.Device.Sig))
		if err == nil && out.Matches(r.bind.Dev, r.bind.Gen, r.bind.Ctl, r.bind.Rid) {
			return fmt.Errorf("%s: client accepted", r.name)
		}
		g.emit("negative", &Fixture{Name: r.name, Kind: "reply", Description: r.desc, Compact: r.compact, RK: r.rk, Bind: r.bind, Expect: Expect{Reject: true}})
	}
	ntfHdr := `{"alg":"dir","enc":"A256GCM","typ":"ff1-ntf+jwe","kid":"` + owner.Ctl + `"}`
	body := `{"v":1,"dev":"` + bind.Dev + `","gen":"` + bind.Gen + `","ctl":"` + owner.Ctl + `","ep":"AAAAAAAAAAA","seq":1,"iat":` + fmt.Sprint(Now) + `,"type":"device_status","body":{}}`
	nk := mustB64(owner.NK)
	ntfs := []rv{
		{"notification-wrong-nk", "Sealed under another controller's nk.", gcmSeal(ntfHdr, mustB64(g.k.Controllers["controller"].NK), iv, []byte(body)), owner.NK, nil},
		{"notification-ctl-mismatch", "Header kid is the owner but the claims name another ctl.", gcmSeal(ntfHdr, nk, iv, []byte(strings.Replace(body, owner.Ctl, g.k.Controllers["controller"].Ctl, 1))), owner.NK, nil},
		{"notification-bad-ep", "ep is not b64u of exactly 8 bytes (here 3 bytes).", gcmSeal(ntfHdr, nk, iv, []byte(strings.Replace(body, `"ep":"AAAAAAAAAAA"`, `"ep":"AAAA"`, 1))), owner.NK, nil},
		{"notification-padded-ep", "ep carries base64 padding.", gcmSeal(ntfHdr, nk, iv, []byte(strings.Replace(body, `"ep":"AAAAAAAAAAA"`, `"ep":"AAAAAAAAAAA="`, 1))), owner.NK, nil},
	}
	// Baseline: the unmodified body opens, so each negative fails for its
	// own reason.
	if _, err := control.OpenNotification(gcmSeal(ntfHdr, nk, iv, []byte(body)), owner.Ctl, nk); err != nil {
		return fmt.Errorf("client baseline notification does not open: %w", err)
	}
	for _, n := range ntfs {
		if _, err := control.OpenNotification(n.compact, owner.Ctl, mustB64(n.rk)); err == nil {
			return fmt.Errorf("%s: client accepted", n.name)
		}
		g.emit("negative", &Fixture{Name: n.name, Kind: "notification", Description: n.desc, Compact: n.compact, To: "owner", NK: n.rk, Expect: Expect{Reject: true}})
	}
	return nil
}

// ApprovalCodeCase is one §5.4 input/output pair.
type ApprovalCodeCase struct {
	Ctl    string `json:"ctl"`
	SigPin string `json:"sigPin"`
	Code   string `json:"code"`
}

// ApprovalCodes is positive/approval-code.json.
type ApprovalCodes struct {
	Name        string             `json:"name"`
	Kind        string             `json:"kind"`
	Description string             `json:"description"`
	Cases       []ApprovalCodeCase `json:"cases"`
}

func (g *gen) approvalCodes() error {
	devPin := g.k.Device.Pins.Sig
	var cases []ApprovalCodeCase
	for _, name := range []string{"owner", "controller", "revoked", "stranger"} {
		ctl := g.k.Controllers[name].Ctl
		cases = append(cases, ApprovalCodeCase{ctl, devPin, control.ApprovalCode(ctl, devPin)})
	}
	// The same requester against a different (e.g. MITM) device pin.
	stranger := g.k.Controllers["stranger"].Ctl
	other := g.k.Controllers["owner"].Sig.Thumb()
	cases = append(cases, ApprovalCodeCase{stranger, other, control.ApprovalCode(stranger, other)})
	g.files["positive/approval-code.json"] = Encode(ApprovalCodes{Name: "approval-code", Kind: "approvalCode",
		Description: "approvalCode(ctl, sigPin) = first 4 bytes of SHA-256(\"ff1-approval-code/v1|\" + ctl + \"|\" + sigPin) as uint32 BE, mod 1e6, 6 digits (§5.4). The last case uses a non-device pin and must differ.",
		Cases:       cases})
	return nil
}
