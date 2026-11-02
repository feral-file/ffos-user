package control

import (
	"encoding/json"
	"testing"
)

// registerReq is a registerController request for c's keys.
func registerReq(t *testing.T, c *testCtl, role Role, extra string) string {
	t.Helper()
	sig, _ := json.Marshal(c.sigPub)
	enc, _ := json.Marshal(c.encPub)
	return `{"sig":` + string(sig) + `,"enc":` + string(enc) + `,"label":"` + c.label + `","platform":"cli","role":"` +
		string(role) + `"` + extra + `}`
}

func (d *testDevice) register(owner, c *testCtl, role Role, extra string) ReplyClaims {
	d.t.Helper()
	return d.expectOK(owner, d.cmdClaims(owner, CmdRegisterController, registerReq(d.t.(*testing.T), c, role, extra)))
}

func (d *testDevice) registerFails(owner, c *testCtl, role Role, extra, failure string) {
	d.t.Helper()
	r := d.expectCode(owner, d.cmdClaims(owner, CmdRegisterController, registerReq(d.t.(*testing.T), c, role, extra)),
		CodeCommandFailed)
	if string(r.Res) != `{"error":"`+failure+`"}` {
		d.t.Fatalf("res = %s, want %s", r.Res, failure)
	}
}

func (d *testDevice) enrollNoInv(c *testCtl) (ReplyClaims, enrollResult) {
	d.t.Helper()
	rc := d.claims(c, OpEnroll)
	rc.Enroll = c.enroll("")
	r := d.final(d.send(c, rc), rc)
	var res enrollResult
	if r.OK {
		if err := json.Unmarshal(r.Res, &res); err != nil {
			d.t.Fatal(err)
		}
	}
	return r, res
}

// §5.6: the owner registers scanned keys; the controller then enrolls
// without inv and gets its role, nk and expiry.
func TestRegisterControllerThenEnroll(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	cli := newTestCtl(t, "laptop")
	exp := int64(testNow + 3600)
	r := d.register(owner, cli, RoleController, `,"expiresAt":`+itoa(exp))
	if string(r.Res) != `{"ctl":"`+cli.ctl+`","role":"controller","expiresAt":`+itoa(exp)+`}` {
		t.Fatalf("register res = %s", r.Res)
	}
	g, ok := d.a.Store().Grant(cli.ctl)
	if !ok || !g.Active(testNow) || g.ApprovedBy != owner.ctl || g.Label != "laptop" || g.CreatedAt != testNow {
		t.Fatalf("grant = %+v", g)
	}
	er, res := d.enrollNoInv(cli)
	if !er.OK || res.Role != RoleController || res.NK != b64u(g.NK[:]) || res.ExpiresAt != exp {
		t.Fatalf("enroll = %+v %+v", er, res)
	}
	// The registered controller can run a controller-tier command.
	d.expectExecute(cli, d.cmdClaims(cli, "rotate", `{}`))
}

func TestRegisterControllerIsIdempotent(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	cli := newTestCtl(t, "laptop")
	d.register(owner, cli, RoleController, "")
	first, _ := d.a.Store().Grant(cli.ctl)
	d.advance(5)
	d.register(owner, cli, RoleController, "")
	again, _ := d.a.Store().Grant(cli.ctl)
	if again.NK != first.NK || again.CreatedAt != first.CreatedAt || again.Window != first.Window {
		t.Fatalf("repeat changed the grant: %+v -> %+v", first, again)
	}
}

// An active grant takes the new role and expiry and keeps nk, window and
// createdAt.
func TestRegisterControllerUpdatesActiveGrant(t *testing.T) {
	d, owner, ctl := ownerDevice(t)
	d.expectExecute(ctl, d.cmdClaims(ctl, "rotate", `{}`))
	before, _ := d.a.Store().Grant(ctl.ctl)
	d.register(owner, ctl, RoleOwner, "")
	after, _ := d.a.Store().Grant(ctl.ctl)
	if after.Role != RoleOwner || after.NK != before.NK || after.CreatedAt != before.CreatedAt ||
		after.Window != before.Window || after.Platform != "cli" {
		t.Fatalf("update: %+v -> %+v", before, after)
	}
}

// A revoked record is reactivated with a fresh nk; its window and createdAt
// are kept, so its old envelopes stay replays.
func TestRegisterControllerReactivatesRevoked(t *testing.T) {
	d, owner, ctl := ownerDevice(t)
	old := d.cmdClaims(ctl, "rotate", `{}`)
	oldCompact := d.seal(ctl, old)
	if adm, err := d.a.Open(oldCompact); err != nil || !adm.Execute() {
		t.Fatalf("first use: %v", err)
	}
	d.expectOK(owner, d.cmdClaims(owner, CmdRevokeController, `{"ctl":"`+ctl.ctl+`"}`))
	revoked, _ := d.a.Store().Grant(ctl.ctl)
	d.advance(10)
	d.register(owner, ctl, RoleController, "")
	g, _ := d.a.Store().Grant(ctl.ctl)
	if !g.Active(d.now.Load()) || g.NK == revoked.NK || g.CreatedAt != revoked.CreatedAt || g.Window != revoked.Window {
		t.Fatalf("reactivation: %+v -> %+v", revoked, g)
	}
	adm, err := d.a.Open(oldCompact)
	if err != nil || d.final(adm, old).Code != CodeReplay {
		t.Fatal("a pre-revocation envelope was accepted after reactivation")
	}
}

func TestRegisterControllerRefusesEncConflict(t *testing.T) {
	d, owner, ctl := ownerDevice(t)
	other := newTestCtl(t, "other")
	ctl.encPub = other.encPub
	d.registerFails(owner, ctl, RoleController, "", "invalid_request")
}

func TestRegisterControllerValidation(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	cli := newTestCtl(t, "laptop")
	d.registerFails(owner, cli, RoleOwner, `,"expiresAt":`+itoa(testNow+60), "invalid_request")
	d.registerFails(owner, cli, RoleController, `,"expiresAt":`+itoa(testNow), "invalid_request")
	d.registerFails(owner, cli, "display", "", "invalid_request")
	d.registerFails(owner, cli, RoleController, `,"extra":1`, "invalid_request")
	bad := newTestCtl(t, "bad\u202elabel")
	d.registerFails(owner, bad, RoleController, "", "invalid_request")
	if _, ok := d.a.Store().Grant(cli.ctl); ok {
		t.Fatal("a refused registration stored a record")
	}
}

func TestRegisterControllerNeedsOwner(t *testing.T) {
	d, _, ctl := ownerDevice(t)
	cli := newTestCtl(t, "laptop")
	d.expectCode(ctl, d.cmdClaims(ctl, CmdRegisterController, registerReq(t, cli, RoleController, "")), CodePermissionDenied)
}

func TestRegisterControllerKeepsAnOwner(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	d.registerFails(owner, owner, RoleController, "", "last_owner")
}

func TestRegisterControllerActiveCap(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	for i := 2; i < maxActiveGrants; i++ {
		d.register(owner, newTestCtl(t, "c"+itoa(int64(i))), RoleController, "")
	}
	d.registerFails(owner, newTestCtl(t, "one-too-many"), RoleController, "", "too_many_controllers")
}

// A LAN requestAccess from the same keys is cleared by registration, and the
// ctr it spent stays spent.
func TestRegisterControllerClearsPending(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	phone := newTestCtl(t, "phone")
	rc := d.claims(phone, OpRequestAccess)
	rc.Enroll = phone.enroll("")
	if r := d.final(d.send(phone, rc), rc); !r.OK {
		t.Fatalf("requestAccess: %+v", r)
	}
	d.register(owner, phone, RoleController, "")
	if _, ok := d.a.pending[phone.ctl]; ok {
		t.Fatal("pending request survived registration")
	}
	g, _ := d.a.Store().Grant(phone.ctl)
	if g.Window.Hi != rc.Ctr {
		t.Fatalf("window hi = %d, want the pending request's ctr %d", g.Window.Hi, rc.Ctr)
	}
	if r, _ := d.enrollNoInv(phone); !r.OK {
		t.Fatalf("enroll: %+v", r)
	}
}

// An owner's earlier deny does not outlive a registration of the same keys.
func TestRegisterControllerClearsDeny(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	phone := newTestCtl(t, "phone")
	rc := d.claims(phone, OpRequestAccess)
	rc.Enroll = phone.enroll("")
	d.final(d.send(phone, rc), rc)
	d.expectOK(owner, d.cmdClaims(owner, CmdRevokeController, `{"ctl":"`+phone.ctl+`"}`))
	if _, ok := d.a.denied[phone.ctl]; !ok {
		t.Fatal("deny not recorded")
	}
	d.register(owner, phone, RoleController, "")
	if _, ok := d.a.denied[phone.ctl]; ok {
		t.Fatal("deny survived registration")
	}
}

// §5.5: a controller may revoke its own grant and nothing else.
func TestSelfRevoke(t *testing.T) {
	d, owner, ctl := ownerDevice(t)
	other := newTestCtl(t, "other")
	d.register(owner, other, RoleController, "")
	d.expectCode(ctl, d.cmdClaims(ctl, CmdRevokeController, `{"ctl":"`+other.ctl+`"}`), CodePermissionDenied)
	d.expectCode(ctl, d.cmdClaims(ctl, CmdRevokeController, `{"ctl":"`+ctl.ctl+`","x":1}`), CodePermissionDenied)
	self := d.cmdClaims(ctl, CmdRevokeController, `{"ctl":"`+ctl.ctl+`"}`)
	adm := d.send(ctl, self)
	if r := d.final(adm, self); !r.OK || len(adm.RevokedCtls) != 1 || adm.RevokedCtls[0] != ctl.ctl {
		t.Fatalf("self-revoke: %+v revoked=%v", r, adm.RevokedCtls)
	}
	if g, _ := d.a.Store().Grant(ctl.ctl); g.Active(d.now.Load()) {
		t.Fatal("self-revoke left the grant active")
	}
	d.expectCode(ctl, d.cmdClaims(ctl, "rotate", `{}`), CodeAccessRevoked)
	if g, _ := d.a.Store().Grant(other.ctl); !g.Active(d.now.Load()) {
		t.Fatal("refused revoke touched another grant")
	}
}

func TestSoleOwnerCannotSelfRevoke(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	rc := d.cmdClaims(owner, CmdRevokeController, `{"ctl":"`+owner.ctl+`"}`)
	adm := d.send(owner, rc)
	r := d.final(adm, rc)
	if r.Code != CodeCommandFailed || string(r.Res) != `{"error":"last_owner"}` || len(adm.RevokedCtls) != 0 {
		t.Fatalf("res = %s revoked=%v", r.Res, adm.RevokedCtls)
	}
}

// An owner revoking another controller reports it for unbinding; denying a
// pending request revokes no grant.
func TestRevokeReportsRevokedCtl(t *testing.T) {
	d, owner, ctl := ownerDevice(t)
	rc := d.cmdClaims(owner, CmdRevokeController, `{"ctl":"`+ctl.ctl+`"}`)
	adm := d.send(owner, rc)
	if r := d.final(adm, rc); !r.OK || len(adm.RevokedCtls) != 1 || adm.RevokedCtls[0] != ctl.ctl {
		t.Fatalf("revoke: %+v revoked=%v", r, adm.RevokedCtls)
	}
	phone := newTestCtl(t, "phone")
	ask := d.claims(phone, OpRequestAccess)
	ask.Enroll = phone.enroll("")
	d.final(d.send(phone, ask), ask)
	deny := d.cmdClaims(owner, CmdRevokeController, `{"ctl":"`+phone.ctl+`"}`)
	adm = d.send(owner, deny)
	if r := d.final(adm, deny); !r.OK || len(adm.RevokedCtls) != 0 {
		t.Fatalf("deny: %+v revoked=%v", r, adm.RevokedCtls)
	}
}

// §5.1: the first owner never enrolls over the device-ID relay; the LAN and
// the legacy relayer keep the screen-QR path, and the refusal spends nothing
// of the invitation.
func TestFirstOwnerEnrollRefusedOnRelayV2(t *testing.T) {
	d := newTestDevice(t)
	inv, err := d.a.IssueInvitation()
	if err != nil {
		t.Fatal(err)
	}
	phone := newTestCtl(t, "phone")
	try := func(route Route) ReplyClaims {
		rc := d.claims(phone, OpEnroll)
		rc.Enroll = phone.enroll(inv.Value)
		adm, err := d.a.OpenFrom(d.seal(phone, rc), Origin{Route: route, Known: true})
		if err != nil {
			t.Fatal(err)
		}
		return d.final(adm, rc)
	}
	if r := try(RouteRelayV2); r.Code != CodeInvitationInvalid {
		t.Fatalf("v2 enroll: %+v", r)
	}
	if _, live := d.a.CurrentInvitation(); !live || d.a.State() != StateLegacy {
		t.Fatal("v2 refusal consumed the invitation")
	}
	if r := try(RouteRelayer); !r.OK || d.a.State() != StateEnrolled {
		t.Fatalf("legacy relayer enroll: %+v", r)
	}
}

// §9: a reactivated or new grant is reported so a v2 binding left over from
// before the revocation is dropped; an in-place role change is not.
func TestRegisterReportsReactivatedCtl(t *testing.T) {
	d, owner, ctl := ownerDevice(t)
	upd := d.cmdClaims(owner, CmdRegisterController, registerReq(t, ctl, RoleController, ""))
	if adm := d.send(owner, upd); len(adm.ReactivatedCtls) != 0 {
		t.Fatalf("active update reported %v", adm.ReactivatedCtls)
	}
	d.expectOK(owner, d.cmdClaims(owner, CmdRevokeController, `{"ctl":"`+ctl.ctl+`"}`))
	re := d.cmdClaims(owner, CmdRegisterController, registerReq(t, ctl, RoleController, ""))
	adm := d.send(owner, re)
	if r := d.final(adm, re); !r.OK || len(adm.ReactivatedCtls) != 1 || adm.ReactivatedCtls[0] != ctl.ctl {
		t.Fatalf("reactivation: %+v %v", r, adm.ReactivatedCtls)
	}
	cli := newTestCtl(t, "laptop")
	nw := d.cmdClaims(owner, CmdRegisterController, registerReq(t, cli, RoleController, ""))
	if adm := d.send(owner, nw); len(adm.ReactivatedCtls) != 1 || adm.ReactivatedCtls[0] != cli.ctl {
		t.Fatalf("new grant: %v", adm.ReactivatedCtls)
	}
}
