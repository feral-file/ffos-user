package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
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

	// The same envelope again is a replay; a second invitation enroll is
	// refused because the device is no longer legacy.
	d.expectPublicOrCode(phone, rc, CodeReplay)
	rc2 := d.claims(phone, OpEnroll)
	rc2.Enroll = phone.enroll(inv.Value)
	d.expectCode(phone, rc2, CodeInvitationInvalid)
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

func TestInvitationAttemptLimitAndExpiry(t *testing.T) {
	d := newTestDevice(t)
	inv, _ := d.a.IssueInvitation()
	phone := newTestCtl(t, "phone")
	for i := 0; i < invitationMaxAttempts; i++ {
		rc := d.claims(phone, OpEnroll)
		rc.Enroll = phone.enroll("AAAAAAAAAAAAAAAAAAAAAA")
		d.expectCode(phone, rc, CodeInvitationInvalid)
	}
	rc := d.claims(phone, OpEnroll)
	rc.Enroll = phone.enroll(inv.Value)
	d.expectCode(phone, rc, CodeInvitationInvalid) // voided after 5 misses

	inv, _ = d.a.IssueInvitation()
	d.advance(invitationTTL)
	rc = d.claims(phone, OpEnroll)
	rc.Enroll = phone.enroll(inv.Value)
	d.expectCode(phone, rc, CodeInvitationInvalid) // expired
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
	d.expectOK(reqs[0], ask(reqs[0]))                    // a refresh is not new
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
	if pe.HTTPStatus() != 403 || string(pe.Body()) != `{"error":{"code":"envelope_required"}}` {
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
