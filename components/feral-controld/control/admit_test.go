package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func ownerDevice(t *testing.T) (*testDevice, *testCtl, *testCtl) {
	owner, ctl := newTestCtl(t, "owner"), newTestCtl(t, "ctl")
	return newTestDevice(t, owner.grant(RoleOwner), ctl.grant(RoleController)), owner, ctl
}

func TestFirstOwnerInvitationFlow(t *testing.T) {
	d := newTestDevice(t)
	if d.a.State() != StateLegacy {
		t.Fatalf("state = %s", d.a.State())
	}
	inv, err := d.a.IssueInvitation()
	if err != nil {
		t.Fatal(err)
	}
	if inv.Pins != d.id.Pins() || inv.Gen != d.id.gen || inv.ExpiresAt != testNow+invitationTTL {
		t.Fatalf("invitation = %+v", inv)
	}
	phone := newTestCtl(t, "phone")

	rc := d.claims(phone, OpEnroll)
	rc.Enroll = phone.enroll(inv.Value)
	adm := d.send(phone, rc)
	r := d.final(adm, rc)
	if !r.OK || !adm.OwnerEnrolled {
		t.Fatalf("enroll failed: %+v", r)
	}
	var res struct {
		Role    Role    `json:"role"`
		NK      string  `json:"nk"`
		Profile Profile `json:"profile"`
	}
	if err := json.Unmarshal(r.Res, &res); err != nil {
		t.Fatal(err)
	}
	if res.Role != RoleOwner || res.Profile != d.id.Profile() {
		t.Fatalf("res = %+v", res)
	}
	if d.a.State() != StateEnrolled {
		t.Fatalf("state after enroll = %s", d.a.State())
	}
	if _, ok := d.a.CurrentInvitation(); ok {
		t.Fatal("invitation not consumed")
	}
	g, ok := d.a.Store().Grant(phone.ctl)
	if !ok || b64u(g.NK[:]) != res.NK || g.ApprovedBy != approvedByInvitation {
		t.Fatalf("grant = %+v", g)
	}

	// The same envelope again is a replay. A retry by the owner with a new
	// ctr gets its grant back (round 3 L-2); another key is refused because
	// the device is no longer legacy.
	d.expectPublicOrCode(phone, rc, CodeReplay)
	rc2 := d.claims(phone, OpEnroll)
	rc2.Enroll = phone.enroll(inv.Value)
	if r := d.expectOK(phone, rc2); !strings.Contains(string(r.Res), res.NK) {
		t.Fatalf("retry res = %s, want the same nk", r.Res)
	}
	other := newTestCtl(t, "other")
	rc3 := d.claims(other, OpEnroll)
	rc3.Enroll = other.enroll(inv.Value)
	d.expectCode(other, rc3, CodeInvitationInvalid)
	if _, err := d.a.IssueInvitation(); err == nil {
		t.Fatal("invitation issued while enrolled")
	}

	// The grant survives a restart.
	d.reopen(false)
	if d.a.State() != StateEnrolled {
		t.Fatal("grant not durable")
	}
	d.expectExecute(phone, d.cmdClaims(phone, "sshAccess", `{}`))
}

// expectPublicOrCode resends the exact same compact (a true replay).
func (d *testDevice) expectPublicOrCode(c *testCtl, rc RequestClaims, code string) {
	d.t.Helper()
	d.expectCode(c, rc, code)
}

// Review F2: wrong guesses never void the invitation (128 random bits need no
// attempt cap; voiding let any LAN peer block enrollment forever).
func TestInvitationSurvivesWrongGuessesAndExpires(t *testing.T) {
	d := newTestDevice(t)
	inv, _ := d.a.IssueInvitation()
	phone, attacker := newTestCtl(t, "phone"), newTestCtl(t, "attacker")
	for i := 0; i < 50; i++ {
		rc := d.claims(attacker, OpEnroll)
		rc.Enroll = attacker.enroll("AAAAAAAAAAAAAAAAAAAAAA")
		d.expectCode(attacker, rc, CodeInvitationInvalid)
	}
	if cur, ok := d.a.CurrentInvitation(); !ok || cur.Value != inv.Value {
		t.Fatal("wrong guesses changed or voided the invitation")
	}
	rc := d.claims(phone, OpEnroll)
	rc.Enroll = phone.enroll(inv.Value)
	d.expectOK(phone, rc)

	d2 := newTestDevice(t)
	inv, _ = d2.a.IssueInvitation()
	d2.advance(invitationTTL)
	rc = d2.claims(phone, OpEnroll)
	rc.Enroll = phone.enroll(inv.Value)
	d2.expectCode(phone, rc, CodeInvitationInvalid) // expired
}

func TestRequestAccessApproveEnroll(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	tablet := newTestCtl(t, "tablet")

	// Enroll before asking: not approved.
	rc := d.claims(tablet, OpEnroll)
	rc.Enroll = tablet.enroll("")
	d.expectCode(tablet, rc, CodeNotApproved)

	// requestAccess: pending reply + a sealed notification per owner.
	rc = d.claims(tablet, OpRequestAccess)
	rc.Enroll = tablet.enroll("")
	adm := d.send(tablet, rc)
	r := d.final(adm, rc)
	if !r.OK || !strings.Contains(string(r.Res), `"status":"pending"`) || !strings.Contains(string(r.Res), ApprovalCode(tablet.ctl, d.id.Pins().Sig)) {
		t.Fatalf("requestAccess reply = %+v %s", r, r.Res)
	}
	if len(adm.Notifications) != 1 || adm.Notifications[0].Ctl != owner.ctl {
		t.Fatalf("notifications = %+v", adm.Notifications)
	}
	og, _ := d.a.Store().Grant(owner.ctl)
	n, err := OpenNotification(adm.Notifications[0].JWE, owner.ctl, og.NK[:])
	if err != nil {
		t.Fatal(err)
	}
	if n.Type != NotificationApprovalRequest || n.Seq != 1 || !strings.Contains(string(n.Body), tablet.ctl) {
		t.Fatalf("notification = %+v", n)
	}

	// Enroll while pending.
	rc = d.claims(tablet, OpEnroll)
	rc.Enroll = tablet.enroll("")
	d.expectCode(tablet, rc, CodePending)

	// A controller cannot approve; the owner can.
	approve := d.cmdClaims(owner, CmdApproveController, `{"ctl":"`+tablet.ctl+`","role":"controller","expiresAt":`+itoa(testNow+3600)+`}`)
	d.expectOK(owner, approve)
	if len(d.a.pending) != 0 {
		t.Fatal("pending not cleared on approval")
	}

	rc = d.claims(tablet, OpEnroll)
	rc.Enroll = tablet.enroll("")
	r = d.expectOK(tablet, rc)
	if !strings.Contains(string(r.Res), `"role":"controller"`) || !strings.Contains(string(r.Res), `"expiresAt":`) {
		t.Fatalf("enroll res = %s", r.Res)
	}
	// Enroll again (lost reply) returns the same grant.
	rc = d.claims(tablet, OpEnroll)
	rc.Enroll = tablet.enroll("")
	r2 := d.expectOK(tablet, rc)
	if string(r2.Res) != string(r.Res) {
		t.Fatalf("re-enroll changed the grant: %s vs %s", r2.Res, r.Res)
	}
	// A different enc on re-enroll is refused.
	rc = d.claims(tablet, OpEnroll)
	rc.Enroll = tablet.enroll("")
	rc.Enroll.Enc = owner.encPub
	d.expectCode(tablet, rc, CodeInvalidRequest)

	adm = d.expectExecute(tablet, d.cmdClaims(tablet, "displayPlaylist", `{"dp1_call":{}}`))
	if adm.Principal != (Principal{ControllerID: tablet.ctl, Role: RoleController}) {
		t.Fatalf("principal = %+v", adm.Principal)
	}
	d.expectCode(tablet, d.cmdClaims(tablet, "factoryReset", `{}`), CodePermissionDenied)

	// Grant expiry.
	d.advance(3600)
	d.expectCode(tablet, d.cmdClaims(tablet, "displayPlaylist", `{}`), CodeAccessRevoked)
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestPendingLimits(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	var reqs []*testCtl
	for i := 0; i < maxPending+1; i++ {
		reqs = append(reqs, newTestCtl(t, "req"+itoa(int64(i))))
	}
	ask := func(c *testCtl) RequestClaims {
		rc := d.claims(c, OpRequestAccess)
		rc.Enroll = c.enroll("")
		return rc
	}
	d.expectOK(reqs[0], ask(reqs[0]))
	d.expectCode(reqs[1], ask(reqs[1]), CodeRateLimited) // < 10 s since the last new one
	d.advance(pendingRefreshInterval)
	d.expectOK(reqs[0], ask(reqs[0])) // a refresh is not new
	for i := 1; i < maxPending; i++ {
		d.advance(pendingNewInterval)
		d.expectOK(reqs[i], ask(reqs[i]))
	}
	d.advance(pendingNewInterval)
	d.expectCode(reqs[maxPending], ask(reqs[maxPending]), CodeRateLimited) // table full

	// TTL: everything lapses; approving a lapsed request fails.
	d.advance(pendingTTL)
	r := d.expectCode(owner, d.cmdClaims(owner, CmdApproveController, `{"ctl":"`+reqs[1].ctl+`","role":"controller"}`), CodeCommandFailed)
	if string(r.Res) != `{"error":"not_pending"}` {
		t.Fatalf("res = %s", r.Res)
	}
	d.expectOK(reqs[maxPending], ask(reqs[maxPending]))
}

func TestRevokeAndReapproveKeepsWindow(t *testing.T) {
	d, owner, ctl := ownerDevice(t)
	used := d.cmdClaims(ctl, "pause", `{}`) // unknown name: owner tier
	d.expectCode(ctl, used, CodePermissionDenied)
	ok := d.cmdClaims(ctl, "rotate", `{}`)
	compact := d.seal(ctl, ok)
	if adm, err := d.a.Open(compact); err != nil || !adm.Execute() {
		t.Fatalf("rotate not admitted: %v", err)
	}

	d.expectOK(owner, d.cmdClaims(owner, CmdRevokeController, `{"ctl":"`+ctl.ctl+`"}`))
	d.expectCode(ctl, d.cmdClaims(ctl, "rotate", `{}`), CodeAccessRevoked)

	// Re-request and re-approve.
	rc := d.claims(ctl, OpRequestAccess)
	rc.Enroll = ctl.enroll("")
	d.expectOK(ctl, rc)
	d.expectOK(owner, d.cmdClaims(owner, CmdApproveController, `{"ctl":"`+ctl.ctl+`","role":"controller"}`))
	d.expectExecute(ctl, d.cmdClaims(ctl, "rotate", `{}`))
	// The captured pre-revocation envelope stays dead.
	adm, err := d.a.Open(compact)
	if err != nil {
		t.Fatal(err)
	}
	if r := d.final(adm, ok); r.Code != CodeReplay {
		t.Fatalf("replayed pre-revocation request: %+v", r)
	}
}

func TestCannotRevokeLastOwner(t *testing.T) {
	d, owner, ctl := ownerDevice(t)
	r := d.expectCode(owner, d.cmdClaims(owner, CmdRevokeController, `{"ctl":"`+owner.ctl+`"}`), CodeCommandFailed)
	if string(r.Res) != `{"error":"last_owner"}` {
		t.Fatalf("res = %s", r.Res)
	}
	r = d.expectCode(owner, d.cmdClaims(owner, CmdRevokeController, `{"ctl":"nope"}`), CodeCommandFailed)
	if string(r.Res) != `{"error":"not_found"}` {
		t.Fatalf("res = %s", r.Res)
	}
	d.expectCode(ctl, d.cmdClaims(ctl, CmdRevokeController, `{"ctl":"`+owner.ctl+`"}`), CodePermissionDenied)
}

func TestListControllers(t *testing.T) {
	d, owner, ctl := ownerDevice(t)
	tablet := newTestCtl(t, "tablet")
	rc := d.claims(tablet, OpRequestAccess)
	rc.Enroll = tablet.enroll("")
	d.expectOK(tablet, rc)
	r := d.expectOK(owner, d.cmdClaims(owner, CmdListControllers, `{}`))
	var res struct {
		Controllers []controllerView `json:"controllers"`
		Pending     []pendingView    `json:"pending"`
	}
	if err := json.Unmarshal(r.Res, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Controllers) != 2 || !res.Controllers[0].Self || res.Controllers[1].Ctl != ctl.ctl || res.Controllers[1].Self {
		t.Fatalf("controllers = %+v", res.Controllers)
	}
	if len(res.Pending) != 1 || res.Pending[0].Code != ApprovalCode(tablet.ctl, d.id.Pins().Sig) {
		t.Fatalf("pending = %+v", res.Pending)
	}
}

func TestApproveValidation(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	tablet := newTestCtl(t, "tablet")
	rc := d.claims(tablet, OpRequestAccess)
	rc.Enroll = tablet.enroll("")
	d.expectOK(tablet, rc)
	for _, req := range []string{
		`{"ctl":"` + tablet.ctl + `","role":"admin"}`,
		`{"ctl":"` + tablet.ctl + `","role":"owner","expiresAt":` + itoa(testNow+60) + `}`,
		`{"ctl":"` + tablet.ctl + `","role":"controller","expiresAt":` + itoa(testNow) + `}`,
		`{"ctl":"` + tablet.ctl + `","role":"controller","extra":1}`,
		`{"ctl":"` + tablet.ctl + `"}`,
	} {
		r := d.expectCode(owner, d.cmdClaims(owner, CmdApproveController, req), CodeCommandFailed)
		if string(r.Res) != `{"error":"invalid_request"}` {
			t.Fatalf("%s: res = %s", req, r.Res)
		}
	}
	// Approving as owner works (phone migration).
	d.expectOK(owner, d.cmdClaims(owner, CmdApproveController, `{"ctl":"`+tablet.ctl+`","role":"owner"}`))
	g, _ := d.a.Store().Grant(tablet.ctl)
	if g.Role != RoleOwner || g.ApprovedBy != owner.ctl {
		t.Fatalf("grant = %+v", g)
	}
	// Now the first owner can be revoked.
	d.expectOK(tablet, d.cmdClaims(tablet, CmdRevokeController, `{"ctl":"`+owner.ctl+`"}`))
}

func TestClaimChecks(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	stranger := newTestCtl(t, "stranger")
	cases := []struct {
		name   string
		mutate func(*RequestClaims)
		code   string
	}{
		{"wrong dev", func(c *RequestClaims) { c.Dev = "FF1-OTHER" }, CodeInvalidRequest},
		{"wrong gen", func(c *RequestClaims) { c.Gen = "AAAAAAAAAAAAAAAAAAAAAA" }, CodeInvalidRequest},
		{"ctl != kid", func(c *RequestClaims) { c.Ctl = stranger.ctl }, CodeInvalidRequest},
		{"lifetime 0", func(c *RequestClaims) { c.Exp = c.Iat }, CodeInvalidRequest},
		{"lifetime 121", func(c *RequestClaims) { c.Exp = c.Iat + 121 }, CodeInvalidRequest},
		{"expired", func(c *RequestClaims) { c.Iat, c.Exp = testNow-100, testNow-31 }, CodeExpired},
		{"grace", func(c *RequestClaims) { c.Iat, c.Exp = testNow-100, testNow-30 }, ""},
		{"future", func(c *RequestClaims) { c.Iat, c.Exp = testNow+301, testNow+400 }, CodeInvalidRequest},
		{"skew ok", func(c *RequestClaims) { c.Iat, c.Exp = testNow+300, testNow+400 }, ""},
		{"v2", func(c *RequestClaims) { c.V = 2 }, CodeInvalidRequest},
		{"no req", func(c *RequestClaims) { c.Req = nil }, CodeInvalidRequest},
		{"req array", func(c *RequestClaims) { c.Req = json.RawMessage(`[]`) }, CodeInvalidRequest},
		{"cmd with enroll", func(c *RequestClaims) { c.Enroll = owner.enroll("") }, CodeInvalidRequest},
		{"bad op", func(c *RequestClaims) { c.Op = "admin" }, CodeInvalidRequest},
		{"envelope in envelope", func(c *RequestClaims) { c.Cmd = CmdEnvelope }, CodePermissionDenied},
		{"internal", func(c *RequestClaims) { c.Cmd = "resolveRecentlyPlayed" }, CodePermissionDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := d.t
			d.t = t
			defer func() { d.t = parent }()
			rc := d.cmdClaims(owner, "displayPlaylist", `{}`)
			tc.mutate(&rc)
			if rc.Ctl != owner.ctl {
				// The reply names the verified signer, not the claim.
				adm := d.send(owner, rc)
				rc.Ctl = owner.ctl
				if r := d.final(adm, rc); r.Code != tc.code {
					t.Fatalf("code = %q", r.Code)
				}
				return
			}
			if tc.code == "" {
				d.expectExecute(owner, rc)
				return
			}
			d.expectCode(owner, rc, tc.code)
		})
	}
}

func TestRawClaimStrictness(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	base := d.cmdClaims(owner, "displayPlaylist", `{}`)
	good, _ := base.Marshal()
	cases := map[string]string{
		"unknown claim":   strings.Replace(string(good), `"v":1`, `"v":1,"x":1`, 1),
		"duplicate claim": strings.Replace(string(good), `"v":1`, `"v":1,"v":1`, 1),
		"float ctr":       strings.Replace(string(good), `"ctr":`+utoa(base.Ctr), `"ctr":1.0`, 1),
		"string iat":      strings.Replace(string(good), `"iat":`+itoa(base.Iat), `"iat":"1"`, 1),
		"zero ctr":        strings.Replace(string(good), `"ctr":`+utoa(base.Ctr), `"ctr":0`, 1),
		"huge ctr":        strings.Replace(string(good), `"ctr":`+utoa(base.Ctr), `"ctr":9007199254740992`, 1),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if payload == string(good) {
				t.Fatal("replacement did not apply")
			}
			adm, err := d.a.Open(sealPayload(t, d.id, owner.sig, []byte(payload)))
			if name == "duplicate claim" {
				// A duplicate member makes the claims unreadable: no
				// reply key can be trusted.
				if publicCode(err) != CodeBadEnvelope {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r := d.final(adm, base); r.Code != CodeInvalidRequest {
				t.Fatalf("code = %q", r.Code)
			}
		})
	}
	// rk or rid unusable: public bad_envelope.
	for name, payload := range map[string]string{
		"short rk": strings.Replace(string(good), base.RK, "AAAA", 1),
		"bad rid":  strings.Replace(string(good), `"rid":"`+base.Rid+`"`, `"rid":"a b"`, 1),
		"not obj":  `[]`,
	} {
		_, err := d.a.Open(sealPayload(t, d.id, owner.sig, []byte(payload)))
		if publicCode(err) != CodeBadEnvelope {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestPublicErrors(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	stranger := newTestCtl(t, "stranger")
	rc := d.cmdClaims(owner, "displayPlaylist", `{}`)
	good := d.seal(owner, rc)

	d.expectPublic("", CodeBadEnvelope)
	d.expectPublic(good[:len(good)-2]+"AA", CodeBadEnvelope) // tag
	d.expectPublic(strings.Repeat("a", MaxCompactBytes+1), CodeBadEnvelope)
	// Unknown controller on op=cmd.
	d.expectPublic(d.seal(stranger, d.cmdClaims(stranger, "displayPlaylist", `{}`)), CodeUnknownController)
	// Signed by someone else under the owner's kid.
	payload, _ := d.cmdClaims(owner, "displayPlaylist", `{}`).Marshal()
	forged, _ := signJWS(stranger.sig, jwsHeader(owner.ctl, typReqJWS), payload)
	d.expectPublic(sealJWS(t, d.id, forged), CodeBadSignature)
	// DER signature.
	jws, _ := SignRequest(owner.sig, payload)
	d.expectPublic(sealJWS(t, d.id, toDER(t, jws)), CodeBadSignature)
	// Encrypted to a different device key.
	other := testIdentity(t)
	other.encPub = encPubOf(t, ecdhKey(t, "other-dev"))
	d.expectPublic(sealPayload(t, other, owner.sig, payload), CodeBadEnvelope)

	pe := &PublicError{Code: CodeEnvelopeRequired}
	if pe.HTTPStatus() != 403 || string(pe.Body()) != `{"error":{"code":"envelope_required","message":"this FF1 is secured: update the Feral File app or ff-cli to send signed commands"}}` {
		t.Fatalf("%d %s", pe.HTTPStatus(), pe.Body())
	}
	if PublicErrorStatus(CodeBadSignature) != 401 {
		t.Fatal("status")
	}
}

func TestReplyBuilder(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	rc := d.cmdClaims(owner, "getDeviceStatus", `{}`)
	adm := d.expectExecute(owner, rc)
	out, err := adm.Reply(true, "", map[string]any{"ok": true, "x": "<&>"})
	if err != nil {
		t.Fatal(err)
	}
	r := d.openReply(out, rc)
	if !r.OK || string(r.Res) != `{"ok":true,"x":"<&>"}` {
		t.Fatalf("reply = %+v %s", r, r.Res)
	}
	out, _ = adm.Reply(false, CodeCommandFailed, json.RawMessage(`{"error":"sourceUnreachable"}`))
	r = d.openReply(out, rc)
	if r.OK || r.Code != CodeCommandFailed || string(r.Res) != `{"error":"sourceUnreachable"}` {
		t.Fatalf("reply = %+v", r)
	}
	if _, err := adm.Reply(true, CodeCommandFailed, nil); err == nil {
		t.Fatal("ok with code accepted")
	}
	// A reply opened with another rk, or pinned to another key, fails.
	if _, err := OpenReply(out, make([]byte, 32), d.id.sigPub); err == nil {
		t.Fatal("opened with wrong rk")
	}
	rk, _ := b64uDecode(rc.RK)
	if _, err := OpenReply(out, rk, owner.sigPub); err == nil {
		t.Fatal("verified against the wrong pin")
	}
}

func TestPersistFailureDoesNotAdmit(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	d.fs.FailWrite = func(string) error { return errors.New("disk full") }
	rc := d.cmdClaims(owner, "displayPlaylist", `{}`)
	compact := d.seal(owner, rc)
	adm, err := d.a.Open(compact)
	if err != nil {
		t.Fatal(err)
	}
	if adm.Execute() || d.final(adm, rc).Code != CodeInternalError {
		t.Fatal("admitted a request whose window was not persisted")
	}
	// Not committed: the same ctr is still usable once the disk recovers.
	d.fs.FailWrite = nil
	if adm, _ := d.a.Open(compact); !adm.Execute() {
		t.Fatal("ctr consumed by an uncommitted write")
	}

	// Committed but directory fsync failed: not executed, ctr consumed.
	d.fs.FailSync = func(string) error { return errors.New("eio") }
	rc = d.cmdClaims(owner, "displayPlaylist", `{}`)
	compact = d.seal(owner, rc)
	if adm, _ := d.a.Open(compact); adm.Execute() {
		t.Fatal("admitted on an unconfirmed write")
	}
	d.fs.FailSync = nil
	if adm, _ := d.a.Open(compact); d.final(adm, rc).Code != CodeReplay {
		t.Fatal("ctr not consumed by a committed write")
	}
}

func TestLockedAuthority(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	d.fs.SetFile(d.dir+"/grants.json", []byte(`{"v":1,"gen":"x","controllers":[]}`))
	a, err := New(Config{Dir: d.dir, DeviceID: testDeviceID, FS: d.fs})
	if err != nil {
		t.Fatal(err)
	}
	if a.State() != StateLocked || !errors.Is(a.LockErr(), ErrLocked) {
		t.Fatalf("state = %s err = %v", a.State(), a.LockErr())
	}
	_, err = a.Open(d.seal(owner, d.cmdClaims(owner, "displayPlaylist", `{}`)))
	if publicCode(err) != CodeLocked {
		t.Fatalf("err = %v", err)
	}
	if st := a.Status(); st.State != StateLocked || st.Profile != nil {
		t.Fatalf("status = %+v", st)
	}
	if _, err := a.SealAll("x", map[string]any{}); err == nil {
		t.Fatal("sealed while locked")
	}
}

func TestStateAndStatus(t *testing.T) {
	d, _, _ := ownerDevice(t)
	if d.a.State() != StateEnrolled {
		t.Fatal(d.a.State())
	}
	d.reopen(true)
	st := d.a.Status()
	if st.State != StateEnforced || st.Profile == nil || *st.Profile != d.id.Profile() {
		t.Fatalf("status = %+v", st)
	}
	b, _ := json.Marshal(st)
	if !strings.HasPrefix(string(b), `{"v":1,"state":"enforced","profile":{"v":1,"dev":"FF1-TESTUNIT"`) {
		t.Fatalf("status json = %s", b)
	}
}

func TestLegacyRefusesCmdAndRequestAccess(t *testing.T) {
	d := newTestDevice(t)
	phone := newTestCtl(t, "phone")
	rc := d.claims(phone, OpRequestAccess)
	rc.Enroll = phone.enroll("")
	d.expectCode(phone, rc, CodeNotApproved)
	rc = d.claims(phone, OpEnroll)
	rc.Enroll = phone.enroll("")
	d.expectCode(phone, rc, CodeNotApproved)
	rc = d.claims(phone, OpEnroll)
	rc.Enroll = phone.enroll("AAAAAAAAAAAAAAAAAAAAAA")
	d.expectCode(phone, rc, CodeInvitationInvalid) // none issued
	// requestAccess must not carry inv.
	rc = d.claims(phone, OpRequestAccess)
	rc.Enroll = phone.enroll("AAAAAAAAAAAAAAAAAAAAAA")
	d.expectCode(phone, rc, CodeInvalidRequest)
}

func TestEnrollKeyMismatch(t *testing.T) {
	d, _, _ := ownerDevice(t)
	phone, other := newTestCtl(t, "phone"), newTestCtl(t, "other")
	rc := d.claims(phone, OpRequestAccess)
	rc.Enroll = other.enroll("") // enroll.sig is not the signer
	// kid (phone) is unknown and thumb(enroll.sig) != kid: no key.
	d.expectPublic(d.seal(phone, rc), CodeUnknownController)
}

func TestSealSequence(t *testing.T) {
	d, owner, ctl := ownerDevice(t)
	all, err := d.a.SealAll("device_status", map[string]any{"a": 1})
	if err != nil || len(all) != 2 {
		t.Fatalf("%v %d", err, len(all))
	}
	s, err := d.a.Seal(owner.ctl, "device_status", json.RawMessage(`{"b":2}`))
	if err != nil {
		t.Fatal(err)
	}
	g, _ := d.a.Store().Grant(owner.ctl)
	n, err := OpenNotification(s.JWE, owner.ctl, g.NK[:])
	if err != nil || n.Seq != 2 || string(n.Body) != `{"b":2}` || n.Dev != testDeviceID || len(n.Ep) != 11 || n.Ep != d.a.ep {
		t.Fatalf("%v %+v", err, n)
	}
	if _, err := OpenNotification(s.JWE, ctl.ctl, g.NK[:]); err == nil {
		t.Fatal("opened under another ctl")
	}
	wire, _ := s.Wire()
	if !strings.HasPrefix(string(wire), `{"type":"notification","notification_type":"sealed","persist_record_count":0,"message":{"v":1,"ctl":"`) {
		t.Fatalf("wire = %s", wire)
	}
	if _, err := d.a.Seal("nope", "x", nil); err == nil {
		t.Fatal("sealed for unknown ctl")
	}
}

func TestApprovalCode(t *testing.T) {
	// Pinned against an independent computation (Python hashlib).
	const ctl, pin = "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs", "zcvu9gS-ZCl5bV9iKdPaLfRAoXW6xwVt27y1Nnpmcmw"
	if got := ApprovalCode(ctl, pin); got != "610259" {
		t.Fatal(got)
	}
	// The device pin is bound: a different pin gives a different code.
	if ApprovalCode(ctl, pin) == ApprovalCode(ctl, "other") {
		t.Fatal("approval code ignores the device pin")
	}
}

func TestPrincipalContext(t *testing.T) {
	p := Principal{ControllerID: "x", Role: RoleOwner}
	got, ok := PrincipalFrom(WithPrincipal(context.Background(), p))
	if !ok || got != p {
		t.Fatal(got)
	}
	if _, ok := PrincipalFrom(context.Background()); ok {
		t.Fatal("principal on empty ctx")
	}
}

// Review F4 (a): a refresh is rate-limited per ctl and re-notifies owners
// only when the label or platform changed.
func TestPendingRefreshLimitsAndQuietRefresh(t *testing.T) {
	d, _, _ := ownerDevice(t)
	tab := newTestCtl(t, "tab")
	ask := func(label string) *Admission {
		rc := d.claims(tab, OpRequestAccess)
		rc.Enroll = tab.enroll("")
		rc.Enroll.Label = label
		return d.send(tab, rc)
	}
	if adm := ask("tab"); len(adm.Notifications) != 1 {
		t.Fatal("new request did not notify")
	}
	rc := d.claims(tab, OpRequestAccess)
	rc.Enroll = tab.enroll("")
	d.expectCode(tab, rc, CodeRateLimited) // refresh within 10 s
	d.advance(pendingRefreshInterval)
	if adm := ask("tab"); len(adm.Notifications) != 0 || adm.Final == "" {
		t.Fatal("unchanged refresh re-notified owners")
	}
	d.advance(pendingRefreshInterval)
	if adm := ask("renamed"); len(adm.Notifications) != 1 {
		t.Fatal("changed refresh did not re-notify")
	}
}

// Review F4 (b): an owner's deny holds until the request would have expired.
func TestDeniedRequestStaysDenied(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	tab := newTestCtl(t, "tab")
	ask := func() RequestClaims {
		rc := d.claims(tab, OpRequestAccess)
		rc.Enroll = tab.enroll("")
		return rc
	}
	d.expectOK(tab, ask())
	d.expectOK(owner, d.cmdClaims(owner, CmdRevokeController, `{"ctl":"`+tab.ctl+`"}`))
	d.advance(pendingRefreshInterval + pendingNewInterval)
	d.expectCode(tab, ask(), CodeNotApproved)
	d.advance(pendingTTL)
	d.expectOK(tab, ask()) // the deny lapsed with the request it denied
}

// Review F4 (c): per-route caps under the global cap.
func TestPendingPerRouteCaps(t *testing.T) {
	d, _, _ := ownerDevice(t)
	open := func(c *testCtl, o Origin) ReplyClaims {
		rc := d.claims(c, OpRequestAccess)
		rc.Enroll = c.enroll("")
		adm, err := d.a.OpenFrom(d.seal(c, rc), o)
		if err != nil {
			t.Fatal(err)
		}
		return d.final(adm, rc)
	}
	lan := Origin{Route: RouteLAN, Remote: "192.168.1.9", Known: true}
	if r := open(newTestCtl(t, "a"), lan); !r.OK {
		t.Fatal(r.Code)
	}
	d.advance(pendingNewInterval)
	if r := open(newTestCtl(t, "b"), lan); r.Code != CodeRateLimited {
		t.Fatalf("second pending from one LAN host: %+v", r)
	}
	if r := open(newTestCtl(t, "c"), Origin{Route: RouteLAN, Remote: "192.168.1.10", Known: true}); !r.OK {
		t.Fatalf("another LAN host: %+v", r)
	}
}

// requestAccess is a LAN-only fallback (§5.2): the legacy relayer and the v2
// relay refuse it with not_approved and spend the ctr of a known record.
func TestRequestAccessIsLANOnly(t *testing.T) {
	d, owner, ctl := ownerDevice(t)
	open := func(c *testCtl, o Origin) ReplyClaims {
		rc := d.claims(c, OpRequestAccess)
		rc.Enroll = c.enroll("")
		adm, err := d.a.OpenFrom(d.seal(c, rc), o)
		if err != nil {
			t.Fatal(err)
		}
		return d.final(adm, rc)
	}
	for _, route := range []Route{RouteRelayer, RouteRelayV2} {
		d.advance(pendingNewInterval)
		if r := open(newTestCtl(t, "relay"+itoa(int64(route))), Origin{Route: route, Known: true}); r.Code != CodeNotApproved {
			t.Fatalf("route %d: %+v", route, r)
		}
	}
	if n := len(d.a.pending); n != 0 {
		t.Fatalf("a relay request entered the pending table (%d)", n)
	}
	// A revoked record asking over a relay is refused too, and its ctr is
	// spent.
	d.expectOK(owner, d.cmdClaims(owner, CmdRevokeController, `{"ctl":"`+ctl.ctl+`"}`))
	if r := open(ctl, Origin{Route: RouteRelayer, Known: true}); r.Code != CodeNotApproved {
		t.Fatalf("known record on the relayer: %+v", r)
	}
	g, _ := d.a.Store().Grant(ctl.ctl)
	if g.Window.Hi != ctl.ctr {
		t.Fatalf("refused relay requestAccess did not spend ctr: hi=%d ctr=%d", g.Window.Hi, ctl.ctr)
	}
}

// Review F7: an owner changes an active grant's role in place; demoting the
// last owner is refused.
func TestApproveChangesRoleInPlace(t *testing.T) {
	d, owner, ctl := ownerDevice(t)
	before, _ := d.a.Store().Grant(ctl.ctl)
	d.expectOK(owner, d.cmdClaims(owner, CmdApproveController, `{"ctl":"`+ctl.ctl+`","role":"owner"}`))
	after, _ := d.a.Store().Grant(ctl.ctl)
	if after.Role != RoleOwner || after.NK != before.NK || after.CreatedAt != before.CreatedAt || after.ApprovedBy != owner.ctl {
		t.Fatalf("role change: %+v", after)
	}
	// Two owners: one may demote the other, but not the last one.
	d.expectOK(ctl, d.cmdClaims(ctl, CmdApproveController, `{"ctl":"`+owner.ctl+`","role":"controller"}`))
	r := d.expectCode(ctl, d.cmdClaims(ctl, CmdApproveController, `{"ctl":"`+ctl.ctl+`","role":"controller"}`), CodeCommandFailed)
	if string(r.Res) != `{"error":"last_owner"}` {
		t.Fatalf("res = %s", r.Res)
	}
	// A revoked record still needs a fresh request.
	d.expectOK(ctl, d.cmdClaims(ctl, CmdRevokeController, `{"ctl":"`+owner.ctl+`"}`))
	r = d.expectCode(ctl, d.cmdClaims(ctl, CmdApproveController, `{"ctl":"`+owner.ctl+`","role":"owner"}`), CodeCommandFailed)
	if string(r.Res) != `{"error":"not_pending"}` {
		t.Fatalf("res = %s", r.Res)
	}
}

// Review F8: seq continues across revoke and re-approval.
func TestSeqSurvivesRevoke(t *testing.T) {
	d, owner, ctl := ownerDevice(t)
	for range 3 {
		if _, err := d.a.Seal(ctl.ctl, "x", map[string]int{}); err != nil {
			t.Fatal(err)
		}
	}
	d.expectOK(owner, d.cmdClaims(owner, CmdRevokeController, `{"ctl":"`+ctl.ctl+`"}`))
	rc := d.claims(ctl, OpRequestAccess)
	rc.Enroll = ctl.enroll("")
	d.expectOK(ctl, rc)
	d.expectOK(owner, d.cmdClaims(owner, CmdApproveController, `{"ctl":"`+ctl.ctl+`","role":"controller"}`))
	s, _ := d.a.Seal(ctl.ctl, "x", map[string]int{})
	g, _ := d.a.Store().Grant(ctl.ctl)
	n, err := OpenNotification(s.JWE, ctl.ctl, g.NK[:])
	if err != nil || n.Seq != 4 {
		t.Fatalf("seq after re-approval = %d (%v), want 4", n.Seq, err)
	}
}

// Review N1 support: the generation moves on issue, clear and consumption,
// and not on a no-op clear.
func TestInvitationGeneration(t *testing.T) {
	d := newTestDevice(t)
	g0 := d.a.InvitationGeneration()
	d.a.ClearInvitation()
	if d.a.InvitationGeneration() != g0 {
		t.Fatal("clearing nothing moved the generation")
	}
	inv, _ := d.a.IssueInvitation()
	g1 := d.a.InvitationGeneration()
	if g1 == g0 {
		t.Fatal("issue did not move the generation")
	}
	phone := newTestCtl(t, "phone")
	rc := d.claims(phone, OpEnroll)
	rc.Enroll = phone.enroll(inv.Value)
	d.expectOK(phone, rc)
	if d.a.InvitationGeneration() == g1 {
		t.Fatal("consumption did not move the generation")
	}
}

// Review N3: a pending request's whole life is capped at 10 minutes,
// however often it refreshes.
func TestPendingAbsoluteLife(t *testing.T) {
	d, _, _ := ownerDevice(t)
	tab := newTestCtl(t, "tab")
	var last ReplyClaims
	for i := 0; i < 8; i++ {
		rc := d.claims(tab, OpRequestAccess)
		rc.Enroll = tab.enroll("")
		last = d.expectOK(tab, rc)
		d.advance(pendingRefreshInterval * 6) // 60 s
	}
	if !strings.Contains(string(last.Res), `"expiresAt":`+itoa(testNow+pendingMaxLife)) {
		t.Fatalf("refreshes extended past the absolute life: %s", last.Res)
	}
	d.advance(pendingMaxLife) // well past
	d.a.mu.Lock()
	d.a.prunePending(d.now.Load())
	_, alive := d.a.pending[tab.ctl]
	d.a.mu.Unlock()
	if alive {
		t.Fatal("pending entry outlived its absolute life")
	}
}

func TestLANSourceKey(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.9":                "192.168.1.9",
		"::ffff:192.168.1.9":         "192.168.1.9",
		"2001:db8:1:2:aaaa::1":       "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff:ffff::99": "2001:db8:1:2::/64",
		"fe80::1":                    "fe80::/10",
		"fe80::1%eth0":               "fe80::/10", // zone stripped
		"fe80:0:0:5::99%wlan0":       "fe80::/10", // all link-local is one source
		"2001:db8:1:2::1%eth0":       "2001:db8:1:2::/64",
		"not-an-ip":                  "not-an-ip",
	} {
		if got := LANSourceKey(in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}

// Review N3: one host rotating IPv6 addresses inside its /64 is one source.
func TestPendingCapAggregatesIPv6Prefix(t *testing.T) {
	d, _, _ := ownerDevice(t)
	open := func(c *testCtl, ip string) ReplyClaims {
		rc := d.claims(c, OpRequestAccess)
		rc.Enroll = c.enroll("")
		adm, err := d.a.OpenFrom(d.seal(c, rc), Origin{Route: RouteLAN, Remote: LANSourceKey(ip), Known: true})
		if err != nil {
			t.Fatal(err)
		}
		return d.final(adm, rc)
	}
	if r := open(newTestCtl(t, "a"), "2001:db8:1:2::10"); !r.OK {
		t.Fatal(r.Code)
	}
	d.advance(pendingNewInterval)
	if r := open(newTestCtl(t, "b"), "2001:db8:1:2::11"); r.Code != CodeRateLimited {
		t.Fatalf("same /64 got a second slot: %+v", r)
	}
}

// Review M1: a hostile envelope stuck in the unauthenticated phase holds no
// lock: State() and a valid owner request complete meanwhile.
func TestUnauthenticatedOpenDoesNotBlock(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	d.a.openHook = func() {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}
	go func() { _, _ = d.a.Open(strings.Repeat("A", 1<<20)) }()
	<-entered
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.a.State()
		rc := d.cmdClaims(owner, "rotate", `{}`)
		adm, err := d.a.Open(d.seal(owner, rc))
		if err != nil || !adm.Execute() {
			t.Errorf("owner request failed while a hostile open was in flight: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stuck unauthenticated open blocked State() / a valid request")
	}
	close(release)
}

// Review M1: concurrent unauthenticated opens are bounded; beyond the bound
// a request waits briefly and then gets the public busy error.
func TestOpenConcurrencyBound(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	prev := openWait
	openWait = 50 * time.Millisecond
	t.Cleanup(func() { openWait = prev })
	release := make(chan struct{})
	var held sync.WaitGroup
	held.Add(maxConcurrentOpens)
	d.a.openHook = func() { held.Done(); <-release }
	for range maxConcurrentOpens {
		go func() { _, _ = d.a.Open("x") }()
	}
	held.Wait()
	d.a.openHook = nil
	_, err := d.a.Open(d.seal(owner, d.cmdClaims(owner, "rotate", `{}`)))
	if publicCode(err) != CodeBusy {
		t.Fatalf("err = %v, want busy", err)
	}
	if (&PublicError{Code: CodeBusy}).HTTPStatus() != 503 {
		t.Fatal("busy is not 503")
	}
	close(release)
}

// Review M1: an unknown kid with a large payload is refused before any
// JSON walk of it.
func TestUnknownKidLargePayloadRefused(t *testing.T) {
	d, _, _ := ownerDevice(t)
	stranger := newTestCtl(t, "stranger")
	rc := d.claims(stranger, OpRequestAccess)
	rc.Enroll = stranger.enroll("")
	payload, _ := rc.Marshal()
	big := strings.Replace(string(payload), `"label":"stranger"`, `"label":"`+strings.Repeat("x", maxEnrollPayload)+`"`, 1)
	d.expectPublic(sealPayload(t, d.id, stranger.sig, []byte(big)), CodeUnknownController)
}

// Review L3: a control op whose write committed but was not confirmed
// durable keeps its memory effects; a retry does not rotate nk.
func TestApproveDurabilityUncertainIsApplied(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	tab := newTestCtl(t, "tab")
	rc := d.claims(tab, OpRequestAccess)
	rc.Enroll = tab.enroll("")
	d.expectOK(tab, rc)
	d.fs.FailSync = func(string) error { return errors.New("eio") }
	d.expectCode(owner, d.cmdClaims(owner, CmdApproveController, `{"ctl":"`+tab.ctl+`","role":"controller"}`), CodeInternalError)
	d.fs.FailSync = nil
	g1, ok := d.a.Store().Grant(tab.ctl)
	if !ok || !g1.Active(d.now.Load()) {
		t.Fatal("committed approval not in memory")
	}
	if _, pending := d.a.pending[tab.ctl]; pending {
		t.Fatal("pending entry survived a committed approval")
	}
	d.expectOK(owner, d.cmdClaims(owner, CmdApproveController, `{"ctl":"`+tab.ctl+`","role":"controller"}`))
	if g2, _ := d.a.Store().Grant(tab.ctl); g2.NK != g1.NK {
		t.Fatal("retry of an applied approval rotated nk")
	}
}

// Review L4: a requestAccess whose window write did not commit leaves no
// pending entry and spends no new-request slot.
func TestRequestAccessPersistFailureLeavesNoPending(t *testing.T) {
	d, owner, ctl := ownerDevice(t)
	d.expectOK(owner, d.cmdClaims(owner, CmdRevokeController, `{"ctl":"`+ctl.ctl+`"}`))
	d.fs.FailWrite = func(string) error { return errors.New("enospc") }
	rc := d.claims(ctl, OpRequestAccess)
	rc.Enroll = ctl.enroll("")
	d.expectCode(ctl, rc, CodeInternalError)
	d.fs.FailWrite = nil
	if len(d.a.pending) != 0 || d.a.lastNewPending != 0 {
		t.Fatal("uncommitted requestAccess left state behind")
	}
	other := newTestCtl(t, "other")
	rc = d.claims(other, OpRequestAccess)
	rc.Enroll = other.enroll("")
	d.expectOK(other, rc) // not rate-limited by a request that never landed
}

// Review L5: a pre-approval enroll from a ctl with a record persists its
// window, so the same envelope cannot be replayed later.
func TestPreApprovalEnrollPersistsWindow(t *testing.T) {
	d, owner, ctl := ownerDevice(t)
	d.expectOK(owner, d.cmdClaims(owner, CmdRevokeController, `{"ctl":"`+ctl.ctl+`"}`))
	rc := d.claims(ctl, OpEnroll)
	rc.Enroll = ctl.enroll("")
	compact := d.seal(ctl, rc)
	adm, err := d.a.Open(compact)
	if err != nil || d.final(adm, rc).Code != CodeNotApproved {
		t.Fatal("pre-approval enroll")
	}
	d.reopen(false)
	adm, _ = d.a.Open(compact)
	if d.final(adm, rc).Code != CodeReplay {
		t.Fatal("pre-approval enroll window not persisted")
	}
}

// Review L7: labels carrying control or bidi/format characters are refused.
func TestLabelUnsafeCharactersRefused(t *testing.T) {
	d, _, _ := ownerDevice(t)
	for i, label := range []string{"evil\u202eenohp", "tab\x07", "a\u2060b", "x\u0085"} {
		tab := newTestCtl(t, "tab"+itoa(int64(i)))
		rc := d.claims(tab, OpRequestAccess)
		rc.Enroll = tab.enroll("")
		rc.Enroll.Label = label
		d.expectCode(tab, rc, CodeInvalidRequest)
	}
}

// Review I2: the carrier is exactly {"v":1,"jwe"}.
func TestParseCarrierStrict(t *testing.T) {
	if j, err := ParseCarrier([]byte(`{"v":1,"jwe":"a.b"}`)); err != nil || j != "a.b" {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"V":1,"jwe":"a"}`, `{"v":1,"JWE":"a"}`, `{"v":1,"jwe":"a","jwe":"b"}`,
		`{"v":1,"jwe":"a","x":1}`, `{"v":1.0,"jwe":"a"}`, `{"v":2,"jwe":"a"}`, `{"v":1,"jwe":""}`, `[]`} {
		if _, err := ParseCarrier([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

// Review D1: an authenticated denial spends ctr, so a captured refused
// envelope never becomes live after a later authorization change.
func TestRefusedRequestSpendsCtr(t *testing.T) {
	replayed := func(t *testing.T, d *testDevice, compact string, rc RequestClaims) {
		t.Helper()
		adm, err := d.a.Open(compact)
		if err != nil {
			t.Fatal(err)
		}
		if adm.Execute() {
			t.Fatalf("refused envelope executed on replay")
		}
		if r := d.final(adm, rc); r.Code != CodeReplay {
			t.Fatalf("replay = %+v, want %q", r, CodeReplay)
		}
	}

	t.Run("permission_denied then promotion", func(t *testing.T) {
		d, owner, ctl := ownerDevice(t)
		rc := d.cmdClaims(ctl, "pause", `{}`) // unknown name: owner tier
		compact := d.seal(ctl, rc)
		adm, err := d.a.Open(compact)
		if err != nil || d.final(adm, rc).Code != CodePermissionDenied {
			t.Fatalf("not denied: %v", err)
		}
		d.expectOK(owner, d.cmdClaims(owner, CmdApproveController, `{"ctl":"`+ctl.ctl+`","role":"owner"}`))
		replayed(t, d, compact, rc)
	})

	t.Run("access_revoked then re-approval", func(t *testing.T) {
		d, owner, ctl := ownerDevice(t)
		d.expectOK(owner, d.cmdClaims(owner, CmdRevokeController, `{"ctl":"`+ctl.ctl+`"}`))
		rc := d.cmdClaims(ctl, "rotate", `{}`)
		compact := d.seal(ctl, rc)
		adm, err := d.a.Open(compact)
		if err != nil || d.final(adm, rc).Code != CodeAccessRevoked {
			t.Fatalf("not revoked: %v", err)
		}
		ask := d.claims(ctl, OpRequestAccess)
		ask.Enroll = ctl.enroll("")
		d.expectOK(ctl, ask)
		d.expectOK(owner, d.cmdClaims(owner, CmdApproveController, `{"ctl":"`+ctl.ctl+`","role":"controller"}`))
		replayed(t, d, compact, rc)
	})

	t.Run("rate_limited requestAccess", func(t *testing.T) {
		d, _, _ := ownerDevice(t)
		tab := newTestCtl(t, "tab")
		ask := func() RequestClaims {
			rc := d.claims(tab, OpRequestAccess)
			rc.Enroll = tab.enroll("")
			return rc
		}
		d.expectOK(tab, ask())
		rc := ask()
		compact := d.seal(tab, rc)
		adm, err := d.a.Open(compact)
		if err != nil || d.final(adm, rc).Code != CodeRateLimited {
			t.Fatalf("not rate limited: %v", err)
		}
		d.advance(pendingRefreshInterval)
		replayed(t, d, compact, rc)
	})

	t.Run("future iat", func(t *testing.T) {
		d, _, ctl := ownerDevice(t)
		rc := d.cmdClaims(ctl, "sshAccess", `{}`)
		rc.Iat += futureSkew + 60
		rc.Exp = rc.Iat + 60
		compact := d.seal(ctl, rc)
		adm, err := d.a.Open(compact)
		if err != nil || d.final(adm, rc).Code != CodeInvalidRequest {
			t.Fatalf("future iat not refused: %v", err)
		}
		d.advance(90) // iat is now within futureSkew
		replayed(t, d, compact, rc)
	})

	t.Run("enroll with a different enc", func(t *testing.T) {
		d, _, ctl := ownerDevice(t)
		rc := d.claims(ctl, OpEnroll)
		rc.Enroll = ctl.enroll("")
		rc.Enroll.Enc = newTestCtl(t, "other").enroll("").Enc
		compact := d.seal(ctl, rc)
		adm, err := d.a.Open(compact)
		if err != nil || d.final(adm, rc).Code != CodeInvalidRequest {
			t.Fatalf("not refused: %v", err)
		}
		replayed(t, d, compact, rc)
	})
}

// Round 3 L-2: a first-owner enroll whose write committed but was not
// confirmed spends the invitation; a retry with inv gets the grant back.
func TestFirstOwnerEnrollRetryAfterUnconfirmedWrite(t *testing.T) {
	d := newTestDevice(t)
	inv, err := d.a.IssueInvitation()
	if err != nil {
		t.Fatal(err)
	}
	phone := newTestCtl(t, "phone")
	rc := d.claims(phone, OpEnroll)
	rc.Enroll = phone.enroll(inv.Value)
	d.fs.FailSync = func(string) error { return errors.New("eio") }
	d.expectCode(phone, rc, CodeInternalError)
	d.fs.FailSync = nil
	g, ok := d.a.Store().Grant(phone.ctl)
	if !ok {
		t.Fatal("committed grant not in memory")
	}
	rc = d.claims(phone, OpEnroll)
	rc.Enroll = phone.enroll(inv.Value)
	if r := d.expectOK(phone, rc); !strings.Contains(string(r.Res), b64u(g.NK[:])) {
		t.Fatalf("retry res = %s, want the committed nk", r.Res)
	}
}

// §7.1: every ff-app cast starts with connect, so a controller's sealed
// connect is admitted (the executor keeps the claim itself owner-only).
func TestControllerConnectAdmitted(t *testing.T) {
	d, _, ctl := ownerDevice(t)
	adm := d.expectExecute(ctl, d.cmdClaims(ctl, "connect", `{"clientDevice":{"device_id":"p"}}`))
	if adm.Principal.Role != RoleController {
		t.Fatalf("principal role %q", adm.Principal.Role)
	}
}

// Review round 4 M1: revoked and expired records no longer hold the record
// cap forever. At the cap, the record inactive longest is evicted once its
// replay window can no longer matter; recent ones and active ones never are.
func TestRecordCapEvictsLongInactive(t *testing.T) {
	owner := newTestCtl(t, "owner")
	gs := []Grant{owner.grant(RoleOwner)}
	for i := 1; i < maxGrantRecords; i++ {
		g := newTestCtl(t, "old-"+utoa(uint64(i))).grant(RoleController)
		g.RevokedAt = testNow - 10 // recently revoked: not yet evictable
		gs = append(gs, g)
	}
	d := newTestDevice(t, gs...)
	newcomer := newTestCtl(t, "newcomer")
	request := func() {
		rc := d.claims(newcomer, OpRequestAccess)
		rc.Enroll = newcomer.enroll("")
		d.expectOK(newcomer, rc)
	}
	approve := `{"ctl":"` + newcomer.ctl + `","role":"controller"}`

	request()
	r := d.expectCode(owner, d.cmdClaims(owner, CmdApproveController, approve), CodeCommandFailed)
	if string(r.Res) != `{"error":"too_many_controllers"}` {
		t.Fatalf("res = %s", r.Res)
	}

	d.advance(recordRetention + 1)
	request()
	d.expectOK(owner, d.cmdClaims(owner, CmdApproveController, approve))
	got := d.a.Store().Grants()
	if len(got) != maxGrantRecords {
		t.Fatalf("records = %d, want %d", len(got), maxGrantRecords)
	}
	if indexOf(got, owner.ctl) < 0 || indexOf(got, newcomer.ctl) < 0 {
		t.Fatal("owner or newcomer missing after eviction")
	}
}

// Review round 4 L1: an expired grant can be revoked, and stays revoked
// when the wall clock steps back.
func TestRevokeExpiredGrantSticks(t *testing.T) {
	owner, guest := newTestCtl(t, "owner"), newTestCtl(t, "guest")
	g := guest.grant(RoleController)
	g.ExpiresAt = testNow + 60
	d := newTestDevice(t, owner.grant(RoleOwner), g)
	d.advance(120)
	d.expectOK(owner, d.cmdClaims(owner, CmdRevokeController, `{"ctl":"`+guest.ctl+`"}`))
	d.advance(-120)
	d.expectCode(guest, d.cmdClaims(guest, "rotate", `{}`), CodeAccessRevoked)
}
